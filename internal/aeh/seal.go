package aeh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SealFile is one sealed acceptance file.
type SealFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Seal records approved acceptance tests. It is committed under aeh/seals/.
type Seal struct {
	Outcome     string     `json:"outcome"`
	Hash        string     `json:"hash"`
	Files       []SealFile `json:"files"`
	ApprovedAt  time.Time  `json:"approved_at"`
	SealedBy    string     `json:"sealed_by,omitempty"`
	SpecContent string     `json:"spec_content_sha256,omitempty"`
}

// sealFromContents seals runner-verified Git blob bytes, keyed by repository-relative path.
// The caller supplies the complete manifest, including plan.yaml when sealing its directory.
func sealFromContents(id string, contents map[string][]byte, specDigest string, at time.Time) Seal {
	s := Seal{Outcome: id, SealedBy: "runner", SpecContent: specDigest, ApprovedAt: at}
	for p, b := range contents {
		sum := sha256.Sum256(b)
		s.Files = append(s.Files, SealFile{Path: p, SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	h := sha256.New()
	for _, f := range s.Files {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	s.Hash = hex.EncodeToString(h.Sum(nil))
	return s
}

// hashPaths hashes every file under the given paths (files or directories) relative to dir.
func hashPaths(dir string, paths []string) (string, []SealFile, error) {
	var files []SealFile
	for _, p := range paths {
		clean := filepath.Clean(filepath.FromSlash(p))
		if clean == "." || !filepath.IsLocal(clean) {
			return "", nil, fmt.Errorf("acceptance path must be inside the worktree: %q", p)
		}
		full := dir
		for _, part := range strings.Split(clean, string(filepath.Separator)) {
			full = filepath.Join(full, part)
			info, err := os.Lstat(full)
			if err != nil {
				return "", nil, fmt.Errorf("inspect acceptance path %q: %w", p, err)
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return "", nil, fmt.Errorf("acceptance path is not a regular file or directory: %q", p)
			}
		}
		err := filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("acceptance entry is not a regular file: %s", rel(dir, path))
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			files = append(files, SealFile{Path: filepath.ToSlash(rel(dir, path)), SHA256: hex.EncodeToString(sum[:])})
			return nil
		})
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", nil, fmt.Errorf("acceptance path %s does not exist", p)
			}
			return "", nil, err
		}
	}
	if len(files) == 0 {
		return "", nil, errors.New("acceptance paths contain no files")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s  %s\n", f.SHA256, f.Path)
	}
	return hex.EncodeToString(h.Sum(nil)), files, nil
}

// ApproveTests seals the outcome's acceptance tests as they are now in the working tree.
func ApproveTests(r Repo, id string) (Seal, error) {
	o, err := r.LoadOutcome(id)
	if err != nil {
		return Seal{}, err
	}
	if len(o.Acceptance) == 0 {
		return Seal{}, fmt.Errorf("outcome %s has no acceptance paths to approve", id)
	}
	hash, files, err := hashPaths(r.Root, o.Acceptance)
	if err != nil {
		return Seal{}, fmt.Errorf("seal %s: %w", id, err)
	}
	s := Seal{Outcome: id, Hash: hash, Files: files, ApprovedAt: time.Now().UTC()}
	return s, writeJSON(r.SealPath(id), s)
}

func loadSeal(r Repo, id string) (Seal, error) {
	var s Seal
	b, err := os.ReadFile(r.SealPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, fmt.Errorf("outcome %s has no approved tests: run `aeh tests approve %s`", id, id)
		}
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
