package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	// captureFileLimit and captureTotalLimit cap what an agent can add to a candidate.
	captureFileLimit  = int64(100) << 20
	captureTotalLimit = int64(2) << 30
)

// captured is one path of the candidate that differs from the base.
type captured struct {
	Path       string
	Executable bool
	Blob       string // object ID computed while reading
	spool      string // host file holding the bytes until Git stores them
}

// agentContentError is a capture refusal caused by what the agent left in its work
// directory (a link, a .git path, a file over the limit). It is the agent's failure, not
// the runtime's: it spends a ladder step like a failed gate.
type agentContentError struct{ msg string }

func (e *agentContentError) Error() string { return e.msg }

func agentContent(format string, args ...any) error {
	return &agentContentError{msg: fmt.Sprintf(format, args...)}
}

// captureCandidate builds the candidate commit from an agent's work volume, read back as
// a tar stream, without running Git on anything the container wrote. It replaces
// git add -A in an agent's worktree (stage 6, S2): no filter driver, attribute, ident,
// encoding or end-of-line conversion runs, and no .git file the agent could rewrite is
// read. Tracked files keep the base's entries when bytes and executable bit are equal;
// new files and directories the base's ignore rules ignore are left out unread, as Git
// would leave them (so npm's links under an ignored node_modules/ are harmless); any
// other link, device or .git path fails with the path.
func captureCandidate(ctx context.Context, root, base string, e *commitExport, rules *ignoreChecker, r io.Reader, spoolDir, msg string) (string, error) {
	format, err := git(root, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	if format != "sha1" && format != "sha256" {
		return "", fmt.Errorf("capture: unsupported object format %q", format)
	}
	spool, err := os.MkdirTemp(spoolDir, "capture-")
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	defer os.RemoveAll(spool)
	ign, err := rules.session(ctx)
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	defer ign.Close()

	tracked := map[string]exportFile{}
	baseDirs := map[string]bool{}
	for _, f := range e.Files {
		tracked[f.Path] = f
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			baseDirs[d] = true
		}
	}
	seen := map[string]bool{}
	var write []captured
	ignoredDirs := map[string]bool{}
	underIgnored := func(p string) bool {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if ignoredDirs[d] {
				return true
			}
		}
		return false
	}
	var total int64
	tr := tar.NewReader(&cappedReader{r: r, left: readBackLimit})
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("capture: read back: %w", err)
		}
		if entries++; entries > maxTreeEntries {
			return "", agentContent("the agent left more than %d entries in its work directory", maxTreeEntries)
		}
		if len(h.Name) > maxTreePath {
			return "", agentContent("the agent left a path longer than %d bytes", maxTreePath)
		}
		name := strings.TrimSuffix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		if name == "work" {
			continue
		}
		rel, ok := strings.CutPrefix(name, "work/")
		if !ok || checkTreePath(rel) != nil {
			return "", fmt.Errorf("capture: unexpected entry %q", h.Name)
		}
		if underIgnored(rel) {
			continue // never read, never committed
		}
		f, isTracked := tracked[rel]
		if !isTracked && !baseDirs[rel] {
			query := rel
			if h.Typeflag == tar.TypeDir {
				query += "/"
			}
			skip, err := ign.ignored(query)
			if err != nil {
				return "", fmt.Errorf("capture: %w", err)
			}
			if skip {
				if h.Typeflag == tar.TypeDir {
					ignoredDirs[rel] = true
				}
				continue
			}
		}
		for _, part := range strings.Split(rel, "/") {
			if strings.EqualFold(part, ".git") {
				return "", agentContent("the agent created %s: .git paths are not allowed in the sandbox work directory", rel)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return "", agentContent("the agent left %s, which is not a regular file or directory (type %q); links are allowed only where .gitignore ignores them", rel, h.Typeflag)
		}
		seen[rel] = true
		// An unchanged tracked file of any size is accepted; the limit applies to content
		// the agent adds or changes.
		sameSize := isTracked && h.Size == f.Size
		if !sameSize && h.Size > captureFileLimit {
			return "", agentContent("the agent left %s with %d bytes; the limit is %d MiB per changed file", rel, h.Size, captureFileLimit>>20)
		}
		executable := h.Mode&0o111 != 0
		spoolPath := filepath.Join(spool, strconv.Itoa(entries))
		blob, err := spoolBlob(tr, h.Size, format, spoolPath)
		if err != nil {
			return "", fmt.Errorf("capture %s: %w", rel, err)
		}
		if isTracked && f.Blob == blob {
			os.Remove(spoolPath)
			if f.Executable == executable {
				continue
			}
			spoolPath = "" // only the mode changed: the base's blob is reused
		} else if h.Size > captureFileLimit {
			return "", agentContent("the agent changed %s to %d bytes; the limit is %d MiB per changed file", rel, h.Size, captureFileLimit>>20)
		} else if total += h.Size; total > captureTotalLimit {
			return "", agentContent("the agent's changes exceed %d GiB", captureTotalLimit>>30)
		}
		write = append(write, captured{Path: rel, Executable: executable, Blob: blob, spool: spoolPath})
	}
	var removed []string
	for p := range tracked {
		if !seen[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(removed)
	sort.Slice(write, func(i, j int) bool { return write[i].Path < write[j].Path })
	if err := storeBlobs(root, write); err != nil {
		return "", err
	}
	return commitCaptured(root, base, spool, removed, write, format, msg)
}

// spoolBlob copies size bytes to a host file and returns their Git blob object ID.
func spoolBlob(r io.Reader, size int64, format, dst string) (string, error) {
	var h hash.Hash
	if format == "sha256" {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	fmt.Fprintf(h, "blob %d\x00", size)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("read %d bytes, expected %d", n, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// storeBlobs writes the spooled files as blobs with no filters, and checks that Git
// stored exactly the bytes that were hashed while reading.
func storeBlobs(root string, all []captured) error {
	var files []captured // a mode-only change reuses the base's blob: nothing to store
	for _, c := range all {
		if c.spool != "" {
			files = append(files, c)
		}
	}
	if len(files) == 0 {
		return nil
	}
	var in strings.Builder
	for _, c := range files {
		in.WriteString(c.spool + "\n")
	}
	out, err := gitInput(root, nil, in.String(), "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) != len(files) {
		return fmt.Errorf("capture: git stored %d objects for %d files", len(ids), len(files))
	}
	for i, c := range files {
		if ids[i] != c.Blob {
			return fmt.Errorf("capture: %s changed while it was stored", c.Path)
		}
	}
	return nil
}

// commitCaptured builds the candidate's tree in a runner-private index, starting from the
// base: removals first, so a file replaced by a directory of the same name can be added.
func commitCaptured(root, base, spool string, removed []string, write []captured, format, msg string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(spool, "index")}
	if _, err := gitInput(root, env, "", "read-tree", base); err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	zero := strings.Repeat("0", 40)
	if format == "sha256" {
		zero = strings.Repeat("0", 64)
	}
	var info bytes.Buffer
	for _, p := range removed {
		fmt.Fprintf(&info, "0 %s\t%s\x00", zero, p)
	}
	for _, c := range write {
		mode := "100644"
		if c.Executable {
			mode = "100755"
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", mode, c.Blob, c.Path)
	}
	if info.Len() > 0 {
		if _, err := gitInput(root, env, info.String(), "update-index", "-z", "--index-info"); err != nil {
			return "", fmt.Errorf("capture: %w", err)
		}
	}
	tree, err := gitInput(root, env, "", "write-tree")
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	commit, err := gitInput(root, nil, "", "-c", "user.name=aeh", "-c", "user.email=aeh@localhost", "-c", "commit.gpgsign=false",
		"commit-tree", tree, "-p", base, "-m", msg)
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	return commit, nil
}

// gitInput runs git in the main repository with gitSafety, extra environment and stdin,
// and returns trimmed stdout. It never touches a directory a container wrote.
func gitInput(dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(commandEnv(nil), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
