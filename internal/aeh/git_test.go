package aeh

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a repository with one committed file and returns its path and HEAD.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "src/a.txt", "one\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "one")
	head, err := headCommit(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, head
}

func TestGitIgnoresReplaceRefs(t *testing.T) {
	dir, first := newRepo(t)
	tree, _ := git(dir, "rev-parse", first+"^{tree}")
	write(t, dir, "src/a.txt", "two\n")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "two")
	second, _ := headCommit(dir)
	mustGit(t, dir, "replace", first, second)
	if got, _ := git(dir, "rev-parse", first+"^{tree}"); got != tree {
		t.Fatalf("a replace ref changed the tree behind %s", first)
	}
}

func TestWorktreeCheckoutRunsNoHooks(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("hooks require sh (Git for Windows provides it)")
	}
	dir, head := newRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+filepath.ToSlash(marker)+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "verify")
	if err := addWorktreeDetached(dir, wt, head); err != nil {
		t.Fatal(err)
	}
	defer removeWorktree(dir, wt)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post-checkout hook ran during a harness checkout")
	}
}

func TestVerifyCheckoutRejectsHiddenChanges(t *testing.T) {
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		t.Run(flag, func(t *testing.T) {
			dir, head := newRepo(t)
			if err := verifyCheckout(dir, head); err != nil {
				t.Fatalf("clean checkout rejected: %v", err)
			}
			mustGit(t, dir, "update-index", flag, "src/a.txt")
			write(t, dir, "src/a.txt", "changed\n")
			err := verifyCheckout(dir, head)
			if err == nil || !strings.Contains(err.Error(), "hides changes") {
				t.Fatalf("expected hidden change to be rejected, got %v", err)
			}
		})
	}
}

func TestSealedTreeComparesObjects(t *testing.T) {
	dir, base := newRepo(t)
	write(t, dir, "tests/acceptance/check.sh", "exit 0\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "tests")
	withTests, _ := headCommit(dir)
	if g := gateSealedTree(dir, withTests, withTests, []string{"tests/acceptance/"}); !g.Passed {
		t.Fatalf("unchanged tests failed: %s", g.Output)
	}
	if g := gateSealedTree(dir, base, withTests, []string{"tests/acceptance/"}); g.Passed {
		t.Fatal("changed acceptance path passed")
	}
}

func TestScopeRejectsSubmodulePointer(t *testing.T) {
	dir, base := newRepo(t)
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+base+",src/mod")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "add gitlink")
	commit, _ := headCommit(dir)
	changes, err := candidateChanges(dir, base, commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || !changes[0].Gitlink || changes[0].Path != "src/mod" {
		t.Fatalf("gitlink not detected: %+v", changes)
	}
	if g := gateScope(changes, nil, []string{"src/"}, 100); g.Passed || !strings.Contains(g.Output, "submodule") {
		t.Fatalf("scope accepted a submodule pointer: %s", g.Output)
	}
}

func TestScopeSeesPointerChangeHiddenByIgnoreAll(t *testing.T) {
	dir, first := newRepo(t)
	write(t, dir, "src/a.txt", "two\n")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "two")
	second, _ := headCommit(dir)
	write(t, dir, ".gitmodules", "[submodule \"mod\"]\n\tpath = src/mod\n\turl = ./mod\n\tignore = all\n")
	mustGit(t, dir, "add", ".gitmodules")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+first+",src/mod")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "submodule")
	base, _ := headCommit(dir)
	mustGit(t, dir, "config", "diff.ignoreSubmodules", "all")
	mustGit(t, dir, "update-index", "--cacheinfo", "160000,"+second+",src/mod")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "move pointer")
	commit, _ := headCommit(dir)
	changes, err := candidateChanges(dir, base, commit)
	if err != nil {
		t.Fatal(err)
	}
	if g := gateScope(changes, nil, []string{"src/"}, 100); g.Passed || !strings.Contains(g.Output, "submodule") {
		t.Fatalf("scope missed a pointer change hidden by ignore=all: %+v %s", changes, g.Output)
	}
}
