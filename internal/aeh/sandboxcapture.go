package aeh

import (
	"archive/tar"
	"bytes"
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

// captureCandidate builds the candidate commit from an agent's work volume, read back as
// a tar stream, without running Git on anything the container wrote. It replaces
// git add -A in an agent's worktree (stage 6, S2): no filter driver, attribute, ident,
// encoding or end-of-line conversion runs, and no .git file the agent could rewrite is
// read. Tracked files keep the base's entries when bytes and executable bit are equal;
// new files the base's ignore rules ignore are left out; any link, device or .git path
// fails with the path.
func captureCandidate(root, base string, e *commitExport, rules *ignoreChecker, r io.Reader, spoolDir, msg string) (string, error) {
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

	tracked := map[string]exportFile{}
	baseDirs := map[string]bool{}
	for _, f := range e.Files {
		tracked[f.Path] = f
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			baseDirs[d] = true
		}
	}
	seen := map[string]bool{}
	var changed, added []captured
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
			return "", fmt.Errorf("capture: more than %d entries", maxTreeEntries)
		}
		if len(h.Name) > maxTreePath {
			return "", errors.New("capture: a path is longer than the limit")
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
			continue
		}
		for _, part := range strings.Split(rel, "/") {
			if strings.EqualFold(part, ".git") {
				return "", fmt.Errorf("the agent created %s: .git paths are not allowed in the sandbox work directory", rel)
			}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			// A new directory the base's rules ignore is skipped whole, as Git would.
			if !baseDirs[rel] {
				ign, err := rules.ignored([]string{rel + "/"})
				if err != nil {
					return "", fmt.Errorf("capture: %w", err)
				}
				if ign[rel+"/"] {
					ignoredDirs[rel] = true
				}
			}
			continue
		case tar.TypeReg:
		default:
			return "", fmt.Errorf("the agent left %s, which is not a regular file or directory (type %q); links are not allowed", rel, h.Typeflag)
		}
		if h.Size > captureFileLimit {
			return "", fmt.Errorf("the agent left %s with %d bytes; the limit is %d MiB per file", rel, h.Size, captureFileLimit>>20)
		}
		seen[rel] = true
		executable := h.Mode&0o111 != 0
		spoolPath := filepath.Join(spool, strconv.Itoa(entries))
		blob, err := spoolBlob(tr, h.Size, format, spoolPath)
		if err != nil {
			return "", fmt.Errorf("capture %s: %w", rel, err)
		}
		c := captured{Path: rel, Executable: executable, Blob: blob, spool: spoolPath}
		if f, ok := tracked[rel]; ok {
			if f.Blob == blob && f.Executable == executable {
				os.Remove(spoolPath)
				continue
			}
			changed = append(changed, c)
		} else {
			added = append(added, c)
		}
		if total += h.Size; total > captureTotalLimit {
			return "", fmt.Errorf("the agent's changes exceed %d GiB", captureTotalLimit>>30)
		}
	}
	// New files the base's ignore rules ignore stay out, as git add -A would leave them.
	if len(added) > 0 {
		paths := make([]string, len(added))
		for i, c := range added {
			paths[i] = c.Path
		}
		ign, err := rules.ignored(paths)
		if err != nil {
			return "", fmt.Errorf("capture: %w", err)
		}
		kept := added[:0]
		for _, c := range added {
			if !ign[c.Path] {
				kept = append(kept, c)
			}
		}
		added = kept
	}
	var removed []string
	for p := range tracked {
		if !seen[p] {
			removed = append(removed, p)
		}
	}
	sort.Strings(removed)
	write := append(changed, added...)
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
func storeBlobs(root string, files []captured) error {
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
