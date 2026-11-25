package aeh

import (
	"context"
	"errors"
	"testing"
)

// Git refuses .git and its host aliases in an index, sometimes only with a warning; the
// capture refuses them first, as the agent's failure.
func TestCaptureRefusesGitAliases(t *testing.T) {
	dir, base, e, rules := captureFixture(t)
	for _, name := range []string{".git./config", ".GIT/config", "sub/.git /x", "GIT~1/config", "a/git~2"} {
		files, exec := exportedTree(t, dir, e)
		files[name] = []byte("x\n")
		_, err := captureCandidate(context.Background(), dir, base, e, rules, captureTar(t, files, exec), t.TempDir(), "capture")
		var content *agentContentError
		if !errors.As(err, &content) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// A new file named like an ignore file is evaluated against the rules, not confused with
// one: the base's root .gitignore ignores .GITIGNORE.
func TestCaptureLeavesOutAFileNamedLikeAnIgnoredRuleFile(t *testing.T) {
	c, dir := caseRulesRepo(t, map[string]string{".gitignore": ".GITIGNORE\n", "keep.txt": "k\n"})
	_ = c
	base := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	rules := mustIgnore(t, dir, base, e)
	files, exec := exportedTree(t, dir, e)
	files[".GITIGNORE"] = []byte("ignored by the base\n")
	files["new.txt"] = []byte("kept\n")
	commit, err := captureCandidate(context.Background(), dir, base, e, rules, captureTar(t, files, exec), t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	if status := mustGit(t, dir, "diff", "--name-status", base, commit); status != "A\tnew.txt" {
		t.Fatalf("status:\n%s", status)
	}
}

// The tree check catches any path Git did not record as asked.
func TestCheckCapturedTreeFindsMissingPaths(t *testing.T) {
	dir, base, e, _ := captureFixture(t)
	tree := mustGit(t, dir, "rev-parse", base+"^{tree}")
	if err := checkCapturedTree(dir, tree, e, nil, nil); err != nil {
		t.Fatalf("base tree: %v", err)
	}
	err := checkCapturedTree(dir, tree, e, nil, []captured{{Path: "lost.txt", Blob: e.Files[0].Blob}})
	var content *agentContentError
	if !errors.As(err, &content) {
		t.Fatalf("got %v", err)
	}
	if err := checkCapturedTree(dir, tree, e, []string{e.Files[0].Path}, nil); err == nil {
		t.Fatal("a removal that did not happen passed")
	}
}
