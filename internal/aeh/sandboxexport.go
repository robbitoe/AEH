package aeh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// exportFile is one tracked regular file of an exported commit.
type exportFile struct {
	Path       string
	Blob       string
	Executable bool
	Size       int64
}

// commitExport streams a commit's tree into a volume with raw blob bytes: no filters,
// no export-subst, no end-of-line conversion and no .git. The modes are the commit's:
// 100755 becomes 0755 and 100644 becomes 0644 on every host.
type commitExport struct {
	root   string
	Commit string
	Files  []exportFile
	sums   map[string]string // sha256 of each streamed file, filled while streaming
}

// newCommitExport lists the commit's tree and refuses what a volume cannot represent
// faithfully: gitlinks (their content is not in the repository) and symbolic links.
func newCommitExport(root, commit string) (*commitExport, error) {
	out, err := git(root, "ls-tree", "-r", "-z", "--long", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", commit, err)
	}
	e := &commitExport{root: root, Commit: commit}
	var refused []string
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("export %s: unexpected ls-tree entry %q", commit, entry)
		}
		mode, kind, blob := fields[0], fields[1], fields[2]
		switch {
		case kind == "commit":
			refused = append(refused, "submodule "+name)
			continue
		case mode == "120000":
			refused = append(refused, "symbolic link "+name)
			continue
		case kind != "blob" || (mode != "100644" && mode != "100755"):
			refused = append(refused, "unsupported entry "+name)
			continue
		}
		if err := checkTreePath(name); err != nil {
			refused = append(refused, err.Error())
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("export %s: size of %s: %w", commit, name, err)
		}
		e.Files = append(e.Files, exportFile{Path: name, Blob: blob, Executable: mode == "100755", Size: size})
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, fmt.Errorf("the sandbox cannot export %s: %s", short(commit), strings.Join(refused, "; "))
	}
	sort.Slice(e.Files, func(i, j int) bool { return e.Files[i].Path < e.Files[j].Path })
	return e, nil
}

// source streams every file through one git cat-file --batch process.
func (e *commitExport) source() tarSource {
	return func(add func(string, bool, int64, io.Reader) error) error {
		e.sums = map[string]string{}
		cmd := exec.Command("git", append(append([]string{}, gitSafety...), "cat-file", "--batch")...)
		cmd.Dir, cmd.Env = e.root, commandEnv(nil)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("export: %w", err)
		}
		go func() {
			w := bufio.NewWriter(stdin)
			for _, f := range e.Files {
				fmt.Fprintln(w, f.Blob)
			}
			w.Flush()
			stdin.Close()
		}()
		r := bufio.NewReaderSize(stdout, 64<<10)
		streamErr := func() error {
			for _, f := range e.Files {
				header, err := r.ReadString('\n')
				if err != nil {
					return fmt.Errorf("export %s: %w", f.Path, err)
				}
				fields := strings.Fields(header)
				if len(fields) != 3 || fields[0] != f.Blob || fields[1] != "blob" || fields[2] != strconv.FormatInt(f.Size, 10) {
					return fmt.Errorf("export %s: unexpected object header %q", f.Path, strings.TrimSpace(header))
				}
				sum := sha256.New()
				if err := add(f.Path, f.Executable, f.Size, io.TeeReader(io.LimitReader(r, f.Size), sum)); err != nil {
					return err
				}
				if b, err := r.ReadByte(); err != nil || b != '\n' {
					return fmt.Errorf("export %s: object not terminated", f.Path)
				}
				e.sums[f.Path] = hex.EncodeToString(sum.Sum(nil))
			}
			return nil
		}()
		if streamErr != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		if streamErr != nil {
			return streamErr
		}
		if waitErr != nil {
			return fmt.Errorf("export: git cat-file: %v: %s", waitErr, strings.TrimSpace(errb.String()))
		}
		return nil
	}
}

// ignoreChecker evaluates the commit's own .gitignore files for paths that exist only
// in a volume. It runs git check-ignore --no-index in a runner-private repository that
// holds nothing but those files: no work tree of the product is read, and no user
// excludes, templates or attributes take part.
type ignoreChecker struct {
	dir string
	// created holds the exact paths aeh wrote: each .gitignore and its directories. Any
	// other path the host resolves inside the private repository is an alias (another
	// case, an NTFS short name such as LONGDI~1, a trailing dot): Git would read rules
	// through it that Linux would not.
	created map[string]bool
}

func newIgnoreChecker(root, commit string, files []exportFile) (*ignoreChecker, error) {
	dir, err := os.MkdirTemp("", "aeh-ignore-")
	if err != nil {
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	c := &ignoreChecker{dir: dir}
	if _, err := git(dir, "init", "-q", "--template=", "."); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	var rules []string
	c.created = map[string]bool{}
	for _, f := range files {
		if path.Base(f.Path) == ".gitignore" {
			rules = append(rules, f.Path)
			c.created[f.Path] = true
			for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
				c.created[d] = true
			}
		}
	}
	// The ignore files and their directories are written to the host, so every path must
	// stay distinct there: on Windows or macOS A/ and a/ would be one directory, and the
	// rules of one would be read through the other.
	written := make([]string, 0, len(c.created))
	for p := range c.created {
		written = append(written, p)
	}
	sort.Strings(written)
	if err := checkHostPaths(written); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	for _, f := range files {
		if path.Base(f.Path) != ".gitignore" {
			continue
		}
		b, ok := gitBlob(root, commit, f.Path)
		if !ok {
			c.Close()
			return nil, fmt.Errorf("ignore rules: read %s at %s", f.Path, short(commit))
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			c.Close()
			return nil, fmt.Errorf("ignore rules: %w", err)
		}
		if err := os.WriteFile(dst, []byte(b), 0o600); err != nil {
			c.Close()
			return nil, fmt.Errorf("ignore rules: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "empty-excludes"), nil, 0o600); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	if err := c.checkExactNames(); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	return c, nil
}

// checkExactNames confirms that every path aeh wrote exists under exactly that name. A
// host alias (another case, an NTFS short name such as LONGDI~1, a trailing dot) makes
// two repository paths one host entry; listing the parent shows the real name only.
func (c *ignoreChecker) checkExactNames() error {
	for p := range c.created {
		parent, base := path.Split(p)
		entries, err := os.ReadDir(filepath.Join(c.dir, filepath.FromSlash(parent)))
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			if e.Name() == base {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is the same file on this host as another path of the commit", p)
		}
	}
	return nil
}

func (c *ignoreChecker) Close() { _ = os.RemoveAll(c.dir) }

// faithful reports whether the private repository can evaluate p as Linux would. A path
// under a host alias (another case, an NTFS short name, a trailing dot) cannot: it counts
// as not ignored, the safe side.
func (c *ignoreChecker) faithful(p string) bool {
	return !c.hostAlias(strings.TrimSuffix(p, "/"))
}

// replacedDir reports a file path that is a directory in the private repository (a
// tracked directory holding a .gitignore that the agent replaced with a file). Git there
// would see a directory and apply directory-only rules; replacedDirIgnored evaluates it
// as the file it is.
func (c *ignoreChecker) replacedDir(p string) bool {
	return !strings.HasSuffix(p, "/") && c.created[p]
}

// replacedDirIgnored evaluates file p in a second private repository holding only the
// .gitignore files of p's ancestors, where nothing exists at p: the rules that apply to a
// file named p are exactly those.
func (c *ignoreChecker) replacedDirIgnored(p string) (bool, error) {
	var files []exportFile
	for q := range c.created {
		if path.Base(q) != ".gitignore" {
			continue
		}
		if info, err := os.Stat(filepath.Join(c.dir, filepath.FromSlash(q))); err != nil || info.IsDir() {
			continue
		}
		if d := path.Dir(q); d == "." || strings.HasPrefix(p, d+"/") {
			files = append(files, exportFile{Path: q})
		}
	}
	dir, err := os.MkdirTemp("", "aeh-ignore-")
	if err != nil {
		return false, fmt.Errorf("ignore rules: %w", err)
	}
	sub := &ignoreChecker{dir: dir, created: map[string]bool{}}
	defer sub.Close()
	if _, err := git(dir, "init", "-q", "--template=", "."); err != nil {
		return false, fmt.Errorf("ignore rules: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "empty-excludes"), nil, 0o600); err != nil {
		return false, fmt.Errorf("ignore rules: %w", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return false, fmt.Errorf("ignore rules: %w", err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return false, fmt.Errorf("ignore rules: %w", err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return false, fmt.Errorf("ignore rules: %w", err)
		}
		sub.created[f.Path] = true
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			sub.created[d] = true
		}
	}
	got, err := sub.ignored([]string{p})
	return got[p], err
}

// checkIgnoreArgs runs git check-ignore against the commit's rules only. Matching is
// case-sensitive, as in the Linux volume; git init would otherwise turn core.ignorecase
// on for a case-insensitive host file system.
func (c *ignoreChecker) checkIgnoreCmd(ctx context.Context, extra ...string) *exec.Cmd {
	args := append(append([]string{}, gitSafety...), "-c", "core.excludesFile="+filepath.Join(c.dir, ".git", "empty-excludes"),
		"-c", "core.ignorecase=false", "check-ignore", "--no-index", "--stdin", "-z")
	cmd := exec.CommandContext(ctx, "git", append(args, extra...)...)
	cmd.Dir = c.dir
	cmd.Env = append(commandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_FLUSH=1")
	return cmd
}

// ignored returns the subset of paths the commit's ignore rules ignore.
func (c *ignoreChecker) ignored(paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	var query []string
	for _, p := range paths {
		switch {
		case !c.faithful(p):
		case c.replacedDir(p):
			ign, err := c.replacedDirIgnored(p)
			if err != nil {
				return nil, err
			}
			if ign {
				out[p] = true
			}
		default:
			query = append(query, p)
		}
	}
	paths = query
	if len(paths) == 0 {
		return out, nil
	}
	cmd := c.checkIgnoreCmd(context.Background())
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stdout, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: none ignored
		return nil, fmt.Errorf("ignore rules: git check-ignore: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out, nil
}

// ignoreSession answers one path at a time from a single git check-ignore process, so a
// tree with many new directories costs one process, not one per directory.
type ignoreSession struct {
	c    *ignoreChecker
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errb bytes.Buffer
}

func (c *ignoreChecker) session(ctx context.Context) (*ignoreSession, error) {
	s := &ignoreSession{c: c, cmd: c.checkIgnoreCmd(ctx, "--verbose", "--non-matching")}
	s.cmd.Stderr = &s.errb
	var err error
	if s.in, err = s.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s.out = bufio.NewReader(stdout)
	if err := s.cmd.Start(); err != nil {
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	return s, nil
}

// ignored asks about one path. With --verbose --non-matching every path gets one record
// of four NUL-terminated fields: source, line, pattern and path. A path is ignored when a
// rule matched and the rule is not a negation.
func (s *ignoreSession) ignored(p string) (bool, error) {
	if !s.c.faithful(p) {
		return false, nil
	}
	if s.c.replacedDir(p) {
		return s.c.replacedDirIgnored(p)
	}
	if _, err := io.WriteString(s.in, p+"\x00"); err != nil {
		return false, fmt.Errorf("ignore rules: %w: %s", err, strings.TrimSpace(s.errb.String()))
	}
	var fields [4]string
	for i := range fields {
		f, err := s.out.ReadString(0)
		if err != nil {
			return false, fmt.Errorf("ignore rules: git check-ignore: %w: %s", err, strings.TrimSpace(s.errb.String()))
		}
		fields[i] = strings.TrimSuffix(f, "\x00")
	}
	if fields[3] != p {
		return false, fmt.Errorf("ignore rules: git check-ignore answered for %q, asked %q", fields[3], p)
	}
	return fields[0] != "" && !strings.HasPrefix(fields[2], "!"), nil
}

// Close ends the process; exit status 1 only means the last batch matched nothing.
func (s *ignoreSession) Close() {
	_ = s.in.Close()
	_ = s.cmd.Wait()
}

// checkExport compares a volume read back after a command with the exported tree.
// Every tracked file must keep its bytes and mode; new files are allowed only when the
// commit's ignore rules ignore them. readTree already refused links, devices and FIFOs.
func checkExport(e *commitExport, back map[string]treeEntry, rules *ignoreChecker) error {
	var problems []string
	tracked := map[string]bool{}
	dirs := map[string]bool{".": true} // the work root too
	for _, f := range e.Files {
		tracked[f.Path] = true
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
		want := int64(0o644)
		if f.Executable {
			want = 0o755
		}
		got, ok := back[f.Path]
		switch {
		case !ok || got.Dir:
			problems = append(problems, "removed or replaced: "+f.Path)
		case got.SHA256 != e.sums[f.Path] || got.Size != f.Size:
			problems = append(problems, "modified: "+f.Path)
		case got.Mode != want:
			problems = append(problems, fmt.Sprintf("mode changed: %s (%04o, exported %04o)", f.Path, got.Mode, want))
		}
	}
	// Directories holding tracked files keep the exported mode, so no later command
	// loses access to tracked content.
	for d := range dirs {
		if got, ok := back[d]; !ok || !got.Dir {
			problems = append(problems, "removed or replaced: "+d+"/")
		} else if got.Mode != 0o755 {
			problems = append(problems, fmt.Sprintf("mode changed: %s/ (%04o, exported 0755)", d, got.Mode))
		}
	}
	var extra []string
	for p, entry := range back {
		if !tracked[p] && !entry.Dir {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	ignored, err := rules.ignored(extra)
	if err != nil {
		return err
	}
	for _, p := range extra {
		if !ignored[p] {
			problems = append(problems, "new file not ignored by the commit's .gitignore: "+p)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		if len(problems) > 50 {
			problems = append(problems[:50], fmt.Sprintf("... and %d more", len(problems)-50))
		}
		return fmt.Errorf("verification changed the checkout:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// windowsReserved are device names Windows maps regardless of extension.
var windowsReserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true}

// hostPathKey is how the host file system identifies a repository path, or an error when
// the host cannot represent it faithfully.
func hostPathKey(p string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		for _, part := range strings.Split(p, "/") {
			base := strings.ToLower(part)
			if i := strings.IndexByte(base, '.'); i >= 0 {
				base = base[:i]
			}
			if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || windowsReserved[base] ||
				strings.ContainsFunc(part, func(r rune) bool { return r < 32 || strings.ContainsRune(`<>:"|?*`, r) }) {
				return "", fmt.Errorf("%s cannot be represented on Windows", p)
			}
		}
		return strings.ToLower(p), nil
	case "darwin":
		return strings.ToLower(p), nil
	}
	return p, nil
}

// checkHostPaths refuses paths the host would merge or alter.
func checkHostPaths(paths []string) error {
	seen := map[string]string{}
	for _, p := range paths {
		key, err := hostPathKey(p)
		if err != nil {
			return err
		}
		if other, ok := seen[key]; ok {
			return fmt.Errorf("%s and %s are the same file on this host", other, p)
		}
		seen[key] = p
	}
	return nil
}

// hostAlias reports a path that the host resolves, through a prefix aeh did not create
// with that exact name, to something in the private repository. Such a path cannot be
// evaluated faithfully here and counts as not ignored (the safe side).
func (c *ignoreChecker) hostAlias(p string) bool {
	parts := strings.Split(p, "/")
	for i := range parts {
		q := strings.Join(parts[:i+1], "/")
		if c.created[q] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(c.dir, filepath.FromSlash(q))); err == nil {
			return true
		}
		return false // nothing there: no rule can be read below it
	}
	return false
}
