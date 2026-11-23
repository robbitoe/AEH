package aeh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MainCheckout returns the working tree that owns the repository's common Git directory.
// Every linked worktree of a project resolves to the same main checkout, so the project
// lock and the memory are shared instead of being created once per worktree.
func MainCheckout(r Repo) (string, error) {
	common, err := git(r.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("main checkout: %w", err)
	}
	common = filepath.Clean(common)
	if !strings.EqualFold(filepath.Base(common), ".git") {
		return "", fmt.Errorf("main checkout: git directory %s is not a standard .git directory (bare or separate git dir is not supported)", common)
	}
	main := filepath.Dir(common)
	top, err := git(main, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("main checkout: %w", err)
	}
	if !samePath(top, main) {
		return "", fmt.Errorf("main checkout: %s is not the working tree of %s", main, common)
	}
	return main, nil
}

// lockProject allows one run or memory write at a time per project, across worktrees.
// The lock is a directory in the main checkout's .aeh/; a crashed run leaves it behind
// and the error says how to clear it.
func lockProject(r Repo) (func(), error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(main, ".aeh")
	if err := rejectLink(dir); err != nil {
		return nil, fmt.Errorf("project lock: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("project lock: %w", err)
	}
	lock := filepath.Join(dir, "project.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			owner, _ := os.ReadFile(filepath.Join(lock, "owner"))
			return nil, fmt.Errorf("another aeh run or memory write holds the project lock (%s); if it crashed, verify it stopped before removing %s",
				strings.TrimSpace(string(owner)), lock)
		}
		return nil, fmt.Errorf("project lock: %w", err)
	}
	owner := fmt.Sprintf("pid %d since %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = os.WriteFile(filepath.Join(lock, "owner"), []byte(owner+"\n"), 0o600)
	return func() { _ = os.RemoveAll(lock) }, nil
}

// rejectLink fails when path exists and is a symbolic link, a junction or another reparse point.
func rejectLink(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("%s is a link or reparse point; refusing to follow it", path)
	}
	return nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	if fa, err := os.Stat(a); err == nil {
		if fb, err := os.Stat(b); err == nil {
			return os.SameFile(fa, fb)
		}
	}
	return false
}
