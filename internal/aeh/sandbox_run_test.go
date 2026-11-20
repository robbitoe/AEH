package aeh

import (
	"context"
	"io"
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

func TestDockerPlannedRunNeedsThePlansSandbox(t *testing.T) {
	image := testImageID(t)
	// Planned on the host, then the sandbox is turned on: the run must not reuse the
	// host-produced red/green evidence under the sandbox policy.
	f, st := generatedFixture(t, `mkdir -p src && echo paid > src/pay.txt`)
	if st.Sandbox != "" {
		t.Fatalf("host plan pinned a sandbox: %+v", st)
	}
	path := filepath.Join(f.dir, "aeh", "config.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, f.dir, "aeh/config.yaml", strings.Replace(string(b), `build: "git --version"`, `build: "true"`, 1)+"sandbox:\n  image: \""+image+"\"\n")
	f.commitAll("sandbox on")
	_, err = RunOutcome(context.Background(), f.repo, st.ID, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "verified under another sandbox") {
		t.Fatalf("run reused host plan evidence: %v", err)
	}
}

func TestCheckPlanSandbox(t *testing.T) {
	if err := checkPlanSandbox("p", "", nil); err != nil {
		t.Fatal(err)
	}
	sb := &sandbox{cfg: SandboxConfig{Memory: "4g", CPUs: "2"}, engine: sandboxEngine{ID: "e"}, image: sandboxImage{ID: "sha256:i"}}
	pinned, err := sandboxSnapshot(sb)
	if err != nil || pinned == "" {
		t.Fatalf("%q %v", pinned, err)
	}
	if err := checkPlanSandbox("p", pinned, sb); err != nil {
		t.Fatal(err)
	}
	for name, now := range map[string]*sandbox{
		"host":   nil,
		"image":  {cfg: sb.cfg, engine: sb.engine, image: sandboxImage{ID: "sha256:other"}},
		"engine": {cfg: sb.cfg, engine: sandboxEngine{ID: "other"}, image: sb.image},
		"limits": {cfg: SandboxConfig{Memory: "8g", CPUs: "2"}, engine: sb.engine, image: sb.image},
	} {
		if err := checkPlanSandbox("p", pinned, now); err == nil {
			t.Errorf("%s change accepted", name)
		}
	}
	if err := checkPlanSandbox("p", "", sb); err == nil {
		t.Error("a host plan accepted under a sandbox")
	}
}

func TestDockerSandboxedAdoptVerify(t *testing.T) {
	image := testImageID(t)
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	f.adoptConfig("sandbox:\n  image: \"" + image + "\"\n")
	f.commitCode("sandbox on")
	f.inventory("")
	v, err := AdoptVerify(context.Background(), f.repo, "", nil)
	if err != nil || !v.Passed || v.Sandbox != image || len(v.Commands) != 2 {
		t.Fatalf("sandboxed verify: %+v %v", v, err)
	}
	// A build that changes tracked code fails integrity, and test never runs.
	write(t, f.dir, "tests/build.sh", "set -eu\necho forged > src/billing/fee.txt\n")
	f.commitCode("tampering build")
	v, err = AdoptVerify(context.Background(), f.repo, "", nil)
	if err != nil || v.Passed || len(v.Commands) != 1 || !strings.Contains(v.Commands[0].Integrity, "modified: src/billing/fee.txt") {
		t.Fatalf("tampering verify: %+v %v", v, err)
	}
}
