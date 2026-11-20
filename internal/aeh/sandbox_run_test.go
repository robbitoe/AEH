package aeh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxFixture is the standard run fixture with gate commands in the sandbox.
func sandboxFixture(t *testing.T, agent, build string) *fixture {
	t.Helper()
	image := testImageID(t)
	f := newFixture(t, map[string]string{"medio": agent}, []string{"medio"}, "")
	path := filepath.Join(f.dir, "aeh", "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), `build: "git --version"`, "build: '"+build+"'", 1)
	cfg += "sandbox:\n  image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "sandbox")
	return f
}

func TestDockerSandboxedRunAccepts(t *testing.T) {
	f := sandboxFixture(t, agentGood, `test ! -e .git && test "$(id -u)" = 10001`)
	st, log := f.run()
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
	if !strings.Contains(log, "build  ok (sandbox)") || !strings.Contains(log, "test   ok (sandbox)") {
		t.Fatalf("gates did not run in the sandbox:\n%s", log)
	}
	final := readGateResults(f.dir, st.FinalRun)
	sandboxed := 0
	for _, g := range final {
		if g.Sandboxed {
			sandboxed++
		}
	}
	if sandboxed != 2 {
		t.Fatalf("final check: %d sandboxed gates, want build and test: %+v", sandboxed, final)
	}
	recorded, err := loadState(f.repo, "OUT-001")
	if err != nil || recorded.Contract == "" {
		t.Fatalf("state %v", err)
	}
}

func TestDockerSandboxedBuildCannotTouchSealedTests(t *testing.T) {
	// The agent's code rewrites the sealed acceptance test while the build runs. The
	// export integrity check fails the build, and the forged test never runs.
	agent := `mkdir -p src && echo "fees ok" > src/fees.txt && printf 'echo "exit 0" > tests/acceptance/check.sh\n' > src/build.sh`
	f := sandboxFixture(t, agent, `if [ -f src/build.sh ]; then sh src/build.sh; fi`)
	st, log := f.run()
	if st.Status != StatusBlocked {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
	if !strings.Contains(log, "modified: tests/acceptance/check.sh") && !strings.Contains(log, "verification changed the checkout") {
		t.Fatalf("integrity failure not reported:\n%s", log)
	}
}

func TestDockerSandboxedPlanPublishes(t *testing.T) {
	image := testImageID(t)
	f := newPlanFixture(t, authorOK, reviewOK)
	f.writeConfig("sandbox:\n  image: \"" + image + "\"\n  memory: 256m\n")
	path := filepath.Join(f.dir, "aeh", "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, f.dir, "aeh/config.yaml", strings.Replace(string(b), `build: "git --version"`, `build: "test ! -e .git"`, 1))
	f.commitAll("sandbox")
	st := f.plan()
	if st.Phase != PlanLogged || st.Status != PlanLogged {
		t.Fatalf("plan not finished: %+v", st)
	}
	if st.Checks[0].Name != "build" || !st.Checks[0].Passed || !strings.HasPrefix(st.Checks[1].Name, "red ") || !st.Checks[1].Passed {
		t.Fatalf("checks: %+v", st.Checks)
	}
	gates := readGateResults(f.dir, rel(f.dir, f.repo.RunDir(st.ID, "plan-baseline")))
	if len(gates) < 2 || !gates[0].Sandboxed || !gates[1].Sandboxed {
		t.Fatalf("baseline did not run in the sandbox: %+v", gates)
	}
}
