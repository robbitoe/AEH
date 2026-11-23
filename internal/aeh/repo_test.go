package aeh

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The same directory spelled differently (as GOTMPDIR with forward slashes spells test
// directories on Windows) must give the same worktree location as the cleaned main checkout.
func TestWorktreePathIgnoresRootSpelling(t *testing.T) {
	dir := t.TempDir()
	clean := Repo{Root: filepath.Join(dir, "project")}
	spelled := Repo{Root: dir + "/./project/"}
	if a, b := clean.WorktreePath("OUT-001-T1-a1"), spelled.WorktreePath("OUT-001-T1-a1"); a != b {
		t.Fatalf("worktree path depends on spelling: %q vs %q", a, b)
	}
}

func TestWorktreePathUsesTheRealProjectPath(t *testing.T) {
	parent := t.TempDir()
	long := filepath.Join(parent, "longprojectdirectory")
	if err := os.Mkdir(long, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, "LONGPR~1") // a Windows 8.3 short name, when the volume has them
	if runtime.GOOS != "windows" {
		if err := os.Symlink(long, filepath.Join(parent, "link")); err != nil {
			t.Skip("symbolic links unavailable")
		}
		other = filepath.Join(parent, "link")
	} else if _, err := os.Stat(other); err != nil {
		t.Skip("8.3 short names are disabled on this volume")
	}
	if a, b := (Repo{Root: long}).WorktreePath("x"), (Repo{Root: other}).WorktreePath("x"); a != b {
		t.Fatalf("one project, two worktree keys:\n%s\n%s", a, b)
	}
}
