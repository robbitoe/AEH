package aeh

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Repo locates every file AEH reads or writes for a target project.
//
//	aeh/          committed contracts: config, outcomes, seals, prompt overrides
//	.aeh/         local and ignored: state, runs, reports
//	worktrees     temporary, in the user cache directory, outside the repository
type Repo struct {
	Root string
}

// OpenRepo finds the git top level that contains dir.
func OpenRepo(dir string) (Repo, error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repo{}, fmt.Errorf("not inside a git repository: %w", err)
	}
	return Repo{Root: filepath.Clean(root)}, nil
}

func (r Repo) ConfigPath() string { return filepath.Join(r.Root, "aeh", "config.yaml") }
func (r Repo) OutcomePath(id string) string {
	return filepath.Join(r.Root, "aeh", "outcomes", id+".yaml")
}
func (r Repo) SealPath(id string) string      { return filepath.Join(r.Root, "aeh", "seals", id+".json") }
func (r Repo) PromptOverride(n string) string { return filepath.Join(r.Root, "aeh", "prompts", n) }
func (r Repo) StatePath(id string) string     { return filepath.Join(r.Root, ".aeh", "state", id+".json") }
func (r Repo) StateDir() string               { return filepath.Join(r.Root, ".aeh", "state") }
func (r Repo) RunDir(id, attempt string) string {
	return filepath.Join(r.Root, ".aeh", "runs", id, attempt)
}

// WorktreePath lives outside the repository on purpose: agent CLIs read instruction files
// from parent directories, and build tools glob subdirectories.
func (r Repo) WorktreePath(name string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(r.Root))
	project := filepath.Base(r.Root) + "-" + hex.EncodeToString(sum[:4])
	return filepath.Join(base, "aeh", "worktrees", project, name)
}
func (r Repo) ReportPath(id string) string { return filepath.Join(r.Root, ".aeh", "reports", id+".md") }

// IntegrationBranch is where accepted tasks of an outcome accumulate. AEH never touches other branches.
func IntegrationBranch(id string) string { return "aeh/" + id }

// AttemptBranch is the throwaway branch of one attempt. It must not live under aeh/<id>/
// because git cannot hold both refs aeh/<id> and aeh/<id>/x.
func AttemptBranch(id, task string, n int) string {
	return fmt.Sprintf("aeh-wip/%s-%s-a%d", id, task, n)
}
