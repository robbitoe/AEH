package aeh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plan and its planned run with every agent in a container: the test author's files
// are captured from its container, reviewers read exports, and nothing an agent wrote is
// checked out on the host. Agents see the plan's pinned memory at /memory.
func TestDockerSandboxedPlanAndPlannedRun(t *testing.T) {
	image := testImageID(t)
	f := newPlanFixture(t, authorOK, reviewOK)
	memoryCheck := `test -r /memory/wiki/features/pay/spec.md || { echo "no memory"; exit 7; }; test ! -e .git || exit 8; `
	reviewer := memoryCheck + `if (echo x >> /work/src/README) 2>/dev/null; then echo WRITABLE; exit 9; fi; echo "VERDICT: APPROVE"`
	cfg := "project: test\ncommands:\n  build: \"test ! -e .git\"\n  test: \"sh tests/suite.sh\"\n" +
		"  acceptance: \"sh tests/run-manifest.sh {{manifest}}\"\n  timeout_minutes: 1\n" +
		"limits:\n  max_diff_lines: 50\nladder: [impl]\nplan:\n  ladder: [author]\n" +
		"reviewers:\n  independence: profile\n  qa: [qa]\n  security: [qa]\n  dba: [qa]\n" +
		"profiles:\n" +
		"  author:\n    provider: author\n    command: [sh, -c, " + strconvQuote(memoryCheck+authorOK) + "]\n" +
		"  qa:\n    provider: qa\n    command: [sh, -c, " + strconvQuote(reviewer) + "]\n" +
		"  impl:\n    provider: implementer\n    command: [sh, -c, " + strconvQuote(memoryCheck+implementPay) + "]\n" +
		"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	f.commitAll("sandboxed agents")
	p := f.plan()
	if p.Phase != PlanLogged {
		t.Fatalf("plan not finished: %+v", p)
	}
	if blob, ok := gitBlob(f.dir, p.PlanCommit, planDir+"/pay.sh"); !ok || !strings.Contains(blob, "paid") {
		t.Fatalf("captured test %q", blob)
	}
	runDir := filepath.Join(f.dir, filepath.FromSlash(p.Attempts[0].RunDir))
	for _, name := range []string{"plan.launch.json", "review-qa.launch.json"} {
		b, err := os.ReadFile(filepath.Join(runDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var rec launchRecord
		if err := json.Unmarshal(b, &rec); err != nil || rec.Class != ClassProcess || rec.MemoryCommit != p.MemoryCommit {
			t.Fatalf("%s: %s", name, b)
		}
	}
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted {
		t.Fatalf("planned run: %+v", st)
	}
	if list := mustGit(t, f.dir, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("host worktrees left:\n%s", list)
	}
}
