package aeh

import (
	"path/filepath"
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
