package aeh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An adoption agent learns where to write through {{output_dir}} in its profile's command.
func TestRunAgentReplacesOutputDir(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("requires sh")
	}
	work, out, runs := t.TempDir(), t.TempDir(), t.TempDir()
	p := Profile{Command: []string{"sh", "-c", `printf ok > "$1/seen"`, "sh", "{{output_dir}}"}, TimeoutMinutes: 1}
	if _, err := runAgentWithOutput(context.Background(), p, nil, work, out, "prompt {{output_dir}}", runs, "adopt"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "seen")); err != nil || string(b) != "ok" {
		t.Fatalf("the agent did not receive the output directory: %v %q", err, b)
	}
	// A step without an output directory refuses such a profile before running it.
	if _, err := runAgent(context.Background(), p, nil, work, "prompt", runs, "review"); err == nil || !strings.Contains(err.Error(), "adopt.ladder") {
		t.Fatalf("a profile asking for an output directory must be refused elsewhere: %v", err)
	}
	// The prompt's own text is never expanded.
	q := Profile{Command: []string{"sh", "-c", `printf '%s' "$1" > "$2/prompt"`, "sh", "{{prompt}}", "{{output_dir}}"}, TimeoutMinutes: 1}
	if _, err := runAgentWithOutput(context.Background(), q, nil, work, out, "keep {{output_dir}} literal", runs, "adopt"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "prompt")); string(b) != "keep {{output_dir}} literal" {
		t.Fatalf("prompt expanded: %q", b)
	}
}

func TestConfigKeepsOutputDirToAdoption(t *testing.T) {
	inspect := Profile{Command: []string{"codex", "exec", "--add-dir", "{{output_dir}}", "-"}}
	plain := Profile{Command: []string{"codex", "exec", "-"}}
	ok := Config{Ladder: []string{"plain"}, Adopt: AdoptConfig{Ladder: []string{"inspect"}},
		Profiles: map[string]Profile{"inspect": inspect, "plain": plain}}
	if err := ok.checkOutputDirProfiles(); err != nil {
		t.Fatalf("adopt.ladder may use {{output_dir}}: %v", err)
	}
	for where, c := range map[string]Config{
		"ladder":           {Ladder: []string{"inspect"}},
		"plan.ladder":      {Ladder: []string{"plain"}, Plan: PlanConfig{Ladder: []string{"inspect"}}},
		"reviewers.qa":     {Ladder: []string{"plain"}, Reviewers: ReviewerPolicy{QA: []string{"inspect"}}},
		"triage.profile":   {Ladder: []string{"plain"}, Triage: ok.Triage},
		"discovery.ladder": {Ladder: []string{"plain"}, Discovery: DiscoveryConfig{Ladder: []string{"inspect"}}},
	} {
		if where == "triage.profile" {
			c.Triage.Profile = "inspect"
		}
		c.Profiles = ok.Profiles
		if err := c.checkOutputDirProfiles(); err == nil || !strings.Contains(err.Error(), where) {
			t.Errorf("%s: %v", where, err)
		}
	}
}
