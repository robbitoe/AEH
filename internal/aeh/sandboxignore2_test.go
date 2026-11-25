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

// Lines of only spaces, "!", "/" or "!/" leave an empty pattern: git add -A keeps every
// path they would seem to touch (checked against real Git), so new directories stay in.
func TestIgnoreDegenerateLinesHaveNoEffect(t *testing.T) {
	c, _ := caseRulesRepo(t, map[string]string{".gitignore": "   \n!\nneg/private/\n", "neg/.gitignore": "!\n/\n!/\n   \n"})
	expectIgnored(t, c, map[string]bool{
		"newdir/": false, "newdir/f": false, "neg/sub/": false, "neg/sub/f": false,
		"neg/private/": true, "neg/private/s.txt": true,
	})
}

// Directory names are matched as bytes, including bytes that are not valid UTF-8.
func TestIgnoreRulesKeepDirectoryBytes(t *testing.T) {
	// The index entry goes through stdin: Windows passes arguments as UTF-16, which
	// would turn \xff into U+FFFD before Git sees it.
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	blob := mustGitInput(t, dir, "*.tmp\n", "hash-object", "-w", "--stdin")
	mustGitInput(t, dir, "100644 "+blob+"\tbad\xff/.gitignore\x00", "-c", "core.protectNTFS=false", "update-index", "-z", "--index-info")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "byte name")
	commit := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Files) != 1 || e.Files[0].Path != "bad\xff/.gitignore" {
		t.Fatalf("fixture %+v", e.Files)
	}
	c := mustIgnore(t, dir, commit, e)
	expectIgnored(t, c, map[string]bool{
		"bad\xff/secret.tmp": true, "bad�/important.tmp": false, "bad/x.tmp": false,
	})
	if got := escapeIgnoreDir("a\xff*"); got != "a\xff\\*" {
		t.Fatalf("escaped %q", got)
	}
}

// A .gitignore over the per-file cap is refused before it is read.
func TestIgnoreRulesFileIsCapped(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	blob := mustGitInput(t, dir, strings.Repeat("\n", int(maxIgnoreFile)+1), "hash-object", "-w", "--stdin")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+",.gitignore")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "large rules file")
	commit := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newIgnoreChecker(dir, commit, e.Files); err == nil || !strings.Contains(err.Error(), "up to") {
		t.Fatalf("got %v", err)
	}
}
