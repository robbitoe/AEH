package aeh

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// tarSource writes a tree through add, one regular file at a time, with repository
// paths (slash-separated, relative). Executable files have mode 0755, others 0644.
type tarSource func(add func(name string, executable bool, size int64, r io.Reader) error) error

// treeFiles is a tarSource over an in-memory tree.
func treeFiles(files map[string][]byte, executable map[string]bool) tarSource {
	return func(add func(string, bool, int64, io.Reader) error) error {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if err := add(n, executable[n], int64(len(files[n])), strings.NewReader(string(files[n]))); err != nil {
				return err
			}
		}
		return nil
	}
}

const sandboxUID = 10001

// checkTreePath refuses names that would escape or be ambiguous inside the volume.
func checkTreePath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return fmt.Errorf("invalid path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path %q", name)
		}
	}
	return nil
}

// writeVolumeTar writes the tree under base/ (the mount point's own name), so extracting
// it into the mount point's parent also gives the mount point the sandbox owner. Every
// entry is owned by 10001:10001; directories are 0755, files 0644 or 0755; no links.
func writeVolumeTar(w io.Writer, base string, src tarSource) error {
	tw := tar.NewWriter(w)
	dirs := map[string]bool{}
	epoch := time.Unix(0, 0)
	header := func(name string, typ byte, mode, size int64) *tar.Header {
		return &tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: size, Uid: sandboxUID, Gid: sandboxUID,
			ModTime: epoch, Format: tar.FormatPAX}
	}
	addDir := func(dir string) error {
		if dirs[dir] {
			return nil
		}
		dirs[dir] = true
		return tw.WriteHeader(header(dir+"/", tar.TypeDir, 0o755, 0))
	}
	if err := addDir(base); err != nil {
		return err
	}
	err := src(func(name string, executable bool, size int64, r io.Reader) error {
		if err := checkTreePath(name); err != nil {
			return err
		}
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			if err := addDir(base + "/" + strings.Join(parts[:i], "/")); err != nil {
				return err
			}
		}
		full := base + "/" + name
		if dirs[full] {
			return fmt.Errorf("path %q is both a file and a directory", name)
		}
		mode := int64(0o644)
		if executable {
			mode = 0o755
		}
		if err := tw.WriteHeader(header(full, tar.TypeReg, mode, size)); err != nil {
			return err
		}
		n, err := io.Copy(tw, r)
		if err == nil && n != size {
			err = fmt.Errorf("%s: wrote %d bytes, expected %d", name, n, size)
		}
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

const (
	maxTreeEntries = 200000
	maxTreePath    = 4096
)

// cappedReader fails once more than left bytes were read.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("more than the read-back limit")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// treeEntry is one regular file read back from a volume.
type treeEntry struct {
	Executable bool
	Size       int64
	SHA256     string
}

// readTree parses a tar of a volume read back with docker cp. Names lose the leading
// base/ component. Directories are allowed; links, devices, FIFOs and any other entry
// type fail with the path. limit caps the total bytes read.
func readTree(r io.Reader, base string, limit int64) (map[string]treeEntry, error) {
	if limit > 0 {
		// The limit bounds the raw stream (headers, metadata and padding included), so a
		// container cannot make the runner read or allocate without bound.
		r = &cappedReader{r: r, left: limit}
	}
	tr := tar.NewReader(r)
	out := map[string]treeEntry{}
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read back: %w", err)
		}
		if entries++; entries > maxTreeEntries {
			return nil, fmt.Errorf("read back: more than %d entries", maxTreeEntries)
		}
		if len(h.Name) > maxTreePath {
			return nil, fmt.Errorf("read back: a path is longer than %d bytes", maxTreePath)
		}
		name := strings.TrimSuffix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		rel, ok := strings.CutPrefix(name, base+"/")
		if name == base {
			if h.Typeflag != tar.TypeDir {
				return nil, fmt.Errorf("read back: %s is not a directory", base)
			}
			continue
		}
		if !ok || checkTreePath(rel) != nil {
			return nil, fmt.Errorf("read back: unexpected entry %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("read back: %s is not a regular file or directory (type %q)", rel, h.Typeflag)
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, tr); err != nil {
			return nil, fmt.Errorf("read back %s: %w", rel, err)
		}
		out[rel] = treeEntry{Executable: h.Mode&0o111 != 0, Size: h.Size, SHA256: hex.EncodeToString(sum.Sum(nil))}
	}
}
