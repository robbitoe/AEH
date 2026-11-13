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

// gitSafety keeps agent-writable Git state from changing what the harness sees or runs:
// replace refs could swap the object behind a verified SHA, and hooks or fsmonitor would
// execute programs the agent wrote.
var gitSafety = []string{"--no-replace-objects", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}

// git runs a git command in dir with gitSafety and a filtered environment, and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), args...)...)
	cmd.Dir = dir
	cmd.Env = commandEnv(nil)
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
	Lines int // -1 means Git could not measure the change in lines.
}

// candidateCommit freezes the proposed tree as a single commit on the pinned base.
func candidateCommit(wt, base, msg string) (string, error) {
	head, err := headCommit(wt)
	if err != nil {
		return "", err
	}
	if head != base {
		return "", fmt.Errorf("agent moved the attempt HEAD from %s to %s", base, head)
	}
	if _, err := git(wt, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := git(wt, "write-tree")
	if err != nil {
		return "", err
	}
	return git(wt, "-c", "user.name=aeh", "-c", "user.email=aeh@localhost", "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", base, "-m", msg)
}

func candidateChanges(wt, base, commit string) ([]fileChange, error) {
	out, err := git(wt, "diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", base, commit)
	if err != nil {
		return nil, err
	}
	var changes []fileChange
	for _, line := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid Git numstat record %q", line)
		}
		added, aerr := strconv.Atoi(parts[0])
		removed, rerr := strconv.Atoi(parts[1])
		lines := added + removed
		if aerr != nil || rerr != nil {
			lines = -1
		}
		changes = append(changes, fileChange{Path: parts[2], Lines: lines})
	}
	return changes, nil
}

func candidateDiff(wt, base, commit string) (string, error) {
	return git(wt, "diff", "--no-renames", "--no-ext-diff", "--no-textconv", base, commit)
}

func verifyCheckout(wt, commit string) error {
	head, err := headCommit(wt)
	if err != nil {
		return err
	}
	if head != commit {
		return fmt.Errorf("verification HEAD changed from %s to %s", commit, head)
	}
	// assume-unchanged (lowercase tag) and skip-worktree (S) make status ignore a tracked file.
	flags, err := git(wt, "ls-files", "-v")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(flags, "\n") {
		if line != "" && (line[0] == 'S' || line[0] >= 'a' && line[0] <= 'z') {
			return fmt.Errorf("verification set an index flag that hides changes: %s", line)
		}
	}
	if _, err := git(wt, "update-index", "-q", "--really-refresh"); err != nil {
		return err
	}
	status, err := git(wt, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("verification changed the checkout:\n%s", status)
	}
	return nil
}

// moveBranch advances an unchecked-out integration branch only from the verified base.
func moveBranch(root, branch, base, commit string) error {
	ref := "refs/heads/" + branch
	worktrees, err := git(root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(worktrees, "\n") {
		if line == "branch "+ref {
			return fmt.Errorf("branch %s is checked out", branch)
		}
	}
	_, err = git(root, "update-ref", ref, commit, base)
	return err
}
