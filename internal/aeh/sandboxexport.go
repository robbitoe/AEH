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
// in a volume. Every .gitignore is translated into one excludes file with patterns
// anchored at the file's directory, and git check-ignore --no-index runs against an
// empty work tree with a separate Git directory. No rule file and no queried path
// exists on the host, so the host file system (case folding, NTFS short names, trailing
// dots) can never make Git read one directory's rules through another name, and no user
// excludes, templates or attributes take part.
type ignoreChecker struct {
	dir string // runner-private: git/ (Git directory), wt/ (empty work tree), excludes
}

func newIgnoreChecker(root, commit string, files []exportFile) (*ignoreChecker, error) {
	dir, err := os.MkdirTemp("", "aeh-ignore-")
	if err != nil {
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	c := &ignoreChecker{dir: dir}
	if _, err := git(dir, "init", "-q", "--bare", "--template=", "git"); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "wt"), 0o700); err != nil {
		c.Close()
		return nil, fmt.Errorf("ignore rules: %w", err)
	}
	var rules []exportFile
	for _, f := range files {
		if path.Base(f.Path) == ".gitignore" {
			rules = append(rules, f)
		}
	}
	// Shallower files first: a deeper .gitignore overrides its ancestors, and in one
	// excludes file the last matching pattern decides.
	sort.Slice(rules, func(i, j int) bool {
		di, dj := strings.Count(rules[i].Path, "/"), strings.Count(rules[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return rules[i].Path < rules[j].Path
	})
	if err := c.writeExcludes(root, commit, rules); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// maxIgnoreRules caps the translated rules of one commit, so a large .gitignore deep in
// the tree cannot make the runner write, or Git read, without bound.
const maxIgnoreRules = int64(64) << 20

// maxIgnoreFile caps one .gitignore read into memory.
const maxIgnoreFile = int64(8) << 20

// utf8BOM is the byte order mark Git skips at the start of a .gitignore.
const utf8BOM = "\xef\xbb\xbf"

// writeExcludes translates the .gitignore files, streaming, into the excludes file.
func (c *ignoreChecker) writeExcludes(root, commit string, rules []exportFile) error {
	f, err := os.OpenFile(filepath.Join(c.dir, "excludes"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("ignore rules: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	var size int64
	for _, rule := range rules {
		if rule.Size > maxIgnoreFile {
			return fmt.Errorf("ignore rules: %s has %d bytes; the sandbox reads .gitignore files up to %d MiB", rule.Path, rule.Size, maxIgnoreFile>>20)
		}
		// By object ID: a path argument would be re-encoded on Windows (arguments are
		// UTF-16 there), so a name that is not valid UTF-8 could not be read by path.
		b, err := gitRaw(root, "cat-file", "blob", rule.Blob)
		if err != nil {
			return fmt.Errorf("ignore rules: read %s at %s: %w", rule.Path, short(commit), err)
		}
		// As Git reads a .gitignore: a UTF-8 byte order mark at the start is skipped, each
		// line loses one trailing carriage return, and a pattern ends at its first NUL (Git
		// handles it as a C string). Lines are walked in place, with no slice of all of them.
		b = strings.TrimPrefix(b, utf8BOM)
		dir := path.Dir(rule.Path)
		for len(b) > 0 {
			line := b
			if i := strings.IndexByte(b, '\n'); i >= 0 {
				line, b = b[:i], b[i+1:]
			} else {
				b = ""
			}
			line = strings.TrimSuffix(line, "\r")
			if i := strings.IndexByte(line, 0); i >= 0 {
				line = line[:i]
			}
			out := anchorIgnoreLine(dir, line)
			if size += int64(len(out)) + 1; size > maxIgnoreRules {
				return fmt.Errorf("ignore rules: the .gitignore files of %s exceed %d MiB once anchored at their directories", short(commit), maxIgnoreRules>>20)
			}
			w.WriteString(out)
			w.WriteByte('\n')
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("ignore rules: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ignore rules: %w", err)
	}
	return nil
}

// anchorIgnoreLine rewrites one line of the .gitignore in directory dir as a pattern of a
// root-level excludes file with the same meaning (gitignore(5)): a pattern with a slash
// other than a trailing one is relative to dir; any other pattern matches at any depth
// below dir. Comments, blank lines and the root's own lines stay as they are.
func anchorIgnoreLine(dir, line string) string {
	if line == "" || strings.HasPrefix(line, "#") {
		return line
	}
	neg, pat := "", line
	if strings.HasPrefix(pat, "!") {
		neg, pat = "!", pat[1:]
	}
	body := strings.TrimRight(strings.TrimRight(pat, " "), "/")
	if body == "" {
		// A line of only spaces, "!", "/" or "!/" leaves an empty pattern. It has no
		// effect on git add -A, but git check-ignore matches it against a directory asked
		// as "name/" (an empty base name), at any level. It is dropped everywhere.
		return ""
	}
	if dir == "." {
		return line
	}
	prefix := escapeIgnoreDir(dir)
	if strings.Contains(body, "/") {
		return neg + prefix + "/" + strings.TrimPrefix(pat, "/")
	}
	return neg + prefix + "/**/" + pat
}

// escapeIgnoreDir escapes glob characters of a directory name, and a leading # or !, so
// the directory is matched literally.
func escapeIgnoreDir(dir string) string {
	// Byte by byte: a name need not be valid UTF-8, and Git matches bytes.
	var b strings.Builder
	for i := 0; i < len(dir); i++ {
		c := dir[i]
		if strings.IndexByte(`*?[\`, c) >= 0 || (i == 0 && (c == '#' || c == '!')) {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (c *ignoreChecker) Close() { _ = os.RemoveAll(c.dir) }

// checkIgnoreCmd runs git check-ignore against the commit's rules only. Matching is
// case-sensitive, as in the Linux volume.
func (c *ignoreChecker) checkIgnoreCmd(ctx context.Context, extra ...string) *exec.Cmd {
	args := append(append([]string{}, gitSafety...), "--git-dir="+filepath.Join(c.dir, "git"), "--work-tree="+filepath.Join(c.dir, "wt"),
		"-c", "core.excludesFile="+filepath.Join(c.dir, "excludes"), "-c", "core.ignorecase=false",
		"check-ignore", "--no-index", "--stdin", "-z")
	cmd := exec.CommandContext(ctx, "git", append(args, extra...)...)
	cmd.Dir = filepath.Join(c.dir, "wt")
	cmd.Env = append(commandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_FLUSH=1")
	return cmd
}

// ignored returns the subset of paths the commit's ignore rules ignore.
func (c *ignoreChecker) ignored(paths []string) (map[string]bool, error) {
	out := map[string]bool{}
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
// tree with many new entries costs one process, not one per entry.
type ignoreSession struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errb bytes.Buffer
}

func (c *ignoreChecker) session(ctx context.Context) (*ignoreSession, error) {
	s := &ignoreSession{cmd: c.checkIgnoreCmd(ctx, "--verbose", "--non-matching")}
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

// Close ends the process; exit status 1 only means nothing matched.
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
