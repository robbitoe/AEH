package aeh

import (
	"bufio"
	"bytes"
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
// in a volume. It runs git check-ignore --no-index in a runner-private repository that
// holds nothing but those files: no work tree of the product is read, and no user
// excludes, templates or attributes take part.
type ignoreChecker struct {
	dir string
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
	return c, nil
}

func (c *ignoreChecker) Close() { _ = os.RemoveAll(c.dir) }

// ignored returns the subset of paths the commit's ignore rules ignore.
func (c *ignoreChecker) ignored(paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out, nil
	}
	args := append(append([]string{}, gitSafety...), "-c", "core.excludesFile="+filepath.Join(c.dir, ".git", "empty-excludes"),
		"check-ignore", "--no-index", "--stdin", "-z")
	cmd := exec.Command("git", args...)
	cmd.Dir = c.dir
	cmd.Env = append(commandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
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

// checkExport compares a volume read back after a command with the exported tree.
// Every tracked file must keep its bytes and mode; new files are allowed only when the
// commit's ignore rules ignore them. readTree already refused links, devices and FIFOs.
func checkExport(e *commitExport, back map[string]treeEntry, rules *ignoreChecker) error {
	var problems []string
	tracked := map[string]bool{}
	for _, f := range e.Files {
		tracked[f.Path] = true
		got, ok := back[f.Path]
		switch {
		case !ok:
			problems = append(problems, "removed or replaced: "+f.Path)
		case got.SHA256 != e.sums[f.Path] || got.Size != f.Size:
			problems = append(problems, "modified: "+f.Path)
		case got.Executable != f.Executable:
			problems = append(problems, "mode changed: "+f.Path)
		}
	}
	var extra []string
	for p := range back {
		if !tracked[p] {
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
