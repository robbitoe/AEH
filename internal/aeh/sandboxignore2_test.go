package aeh

import (
	"strings"
	"testing"
)

// A newline or carriage return in a path would split a line of Git's line-based inputs.
func TestTreePathsRefuseLineBreaks(t *testing.T) {
	for _, bad := range []string{"safe\nsrc\nrest/.gitignore", "a\rb", "x\\y"} {
		if checkTreePath(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	blob := mustGitInput(t, dir, "x\n", "hash-object", "-w", "--stdin")
	// Git for Windows refuses the name by default; on Linux it is a valid path.
	mustGit(t, dir, "-c", "core.protectNTFS=false", "update-index", "--add", "--cacheinfo", "100644,"+blob+",safe\nsrc\nrest/.gitignore")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "newline directory")
	if _, err := newCommitExport(dir, mustGit(t, dir, "rev-parse", "HEAD")); err == nil {
		t.Fatal("a directory name with a newline was exported")
	}
}

// A large .gitignore under a long directory path would expand without bound once every
// line repeats the directory; the translation stops at its cap.
func TestIgnoreRulesTranslationIsCapped(t *testing.T) {
	deep := strings.Repeat("d123456789/", 300) // about 3,300 bytes
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	blob := mustGitInput(t, dir, strings.Repeat("x\n", 1<<20), "hash-object", "-w", "--stdin")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+deep+".gitignore")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "large rules")
	commit := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newIgnoreChecker(dir, commit, e.Files); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatalf("got %v", err)
	}
}
