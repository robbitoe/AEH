package aeh

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// git runs a git command in dir and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func headCommit(dir string) (string, error) { return git(dir, "rev-parse", "HEAD") }

func branchCommit(dir, branch string) (string, bool) {
	sha, err := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil || sha == "" {
		return "", false
	}
	return sha, true
}

// addWorktree creates a fresh worktree on a new branch starting at base.
func addWorktree(root, path, branch, base string) error {
	_, err := git(root, "worktree", "add", "-b", branch, path, base)
	return err
}

// addWorktreeDetached checks out a commit without creating a branch.
func addWorktreeDetached(root, path, commit string) error {
	_, err := git(root, "worktree", "add", "--detach", path, commit)
	return err
}

func removeWorktree(root, path string) {
	_, _ = git(root, "worktree", "remove", "--force", path)
	_, _ = git(root, "worktree", "prune")
	_ = os.Remove(filepath.Dir(path)) // drop the per-project folder once it is empty
}

func deleteBranch(root, branch string) {
	_, _ = git(root, "branch", "-D", branch)
}

// dirtyPaths lists uncommitted changes under the given paths.
func dirtyPaths(root string, paths []string) (string, error) {
	args := append([]string{"status", "--porcelain", "--"}, paths...)
	return git(root, args...)
}

// fileChange is one entry of `git diff --cached --numstat`.
type fileChange struct {
	Path  string
	Lines int
}

// stageAll stages everything in the worktree and returns what changed against HEAD.
func stageAll(wt string) ([]fileChange, error) {
	if _, err := git(wt, "add", "-A"); err != nil {
		return nil, err
	}
	out, err := git(wt, "diff", "--cached", "--numstat", "--no-renames")
	if err != nil {
		return nil, err
	}
	var changes []fileChange
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0]) // "-" for binary files counts as 0
		removed, _ := strconv.Atoi(parts[1])
		changes = append(changes, fileChange{Path: parts[2], Lines: added + removed})
	}
	return changes, nil
}

func stagedDiff(wt string) (string, error) {
	return git(wt, "diff", "--cached", "--no-renames")
}

// commitStaged commits the staged changes as the harness, without touching the user's git identity.
func commitStaged(wt, msg string) (string, error) {
	if _, err := git(wt, "-c", "user.name=aeh", "-c", "user.email=aeh@localhost", "commit", "--no-verify", "-q", "-m", msg); err != nil {
		return "", err
	}
	return headCommit(wt)
}

// moveBranch points branch at commit. Valid only when the branch is not checked out anywhere.
func moveBranch(root, branch, commit string) error {
	_, err := git(root, "branch", "-f", branch, commit)
	return err
}
