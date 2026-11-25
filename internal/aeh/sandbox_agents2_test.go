package aeh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// A profile without a sandbox section serializes as before, so digests of plans that
// include whole profiles do not change.
func TestProfileJSONWithoutSandboxIsUnchanged(t *testing.T) {
	b, err := json.Marshal(Profile{Provider: "x", Command: []string{"a"}})
	if err != nil || strings.Contains(string(b), "Sandbox") {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = json.Marshal(Profile{Provider: "x", Sandbox: &ProfileSandbox{PassEnv: []string{"K"}}})
	if !strings.Contains(string(b), `"Sandbox":{"PassEnv":["K"]}`) {
		t.Fatalf("%s", b)
	}
}

// Agent programs live in the agent image, so the run's preflight does not look for them
// on the host.
func TestPreflightLeavesAgentProgramsToTheImage(t *testing.T) {
	body := "project: t\ncommands:\n  test: \"true\"\nladder: [a]\nreviewer: b\nprofiles:\n  a:\n    provider: x\n    command: [no-such-agent-cli]\n  b:\n    provider: y\n    command: [no-such-review-cli]\n"
	o := Outcome{ID: "OUT-1", Route: RouteChange, Tasks: []Task{{ID: "T1", Goal: "g", Paths: []string{"src/"}}}}
	cfg, err := loadTestConfig(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflight(cfg, o, ""); err == nil {
		t.Fatal("host preflight accepted a missing program")
	}
	cfg, err = loadTestConfig(t, body+"sandbox:\n  image: \""+testDigest+"\"\n  agent_image: \""+testDigest+"\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := preflight(cfg, o, ""); err != nil {
		t.Fatalf("sandboxed preflight: %v", err)
	}
}

// Under policy 2 the approved tests must be exactly the tracked ones: an ignored sealed
// test would never reach the container.
func TestSealTrackedNeedsEverySealedFileTracked(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	write(t, dir, "tests/acceptance/a.sh", "a\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "tests")
	head := mustGit(t, dir, "rev-parse", "HEAD")
	paths := []string{"tests/acceptance/"}
	one := Seal{Files: []SealFile{{Path: "tests/acceptance/a.sh", SHA256: sha256Hex("a\n")}}}
	if g := sealTracked(dir, head, one, paths); !g.Passed {
		t.Fatalf("exact seal refused: %s", g.Output)
	}
	// The approved checkout had CRLF (core.autocrlf): the same text.
	crlf := Seal{Files: []SealFile{{Path: "tests/acceptance/a.sh", SHA256: sha256Hex("a\r\n")}}}
	if g := sealTracked(dir, head, crlf, paths); !g.Passed {
		t.Fatalf("autocrlf seal refused: %s", g.Output)
	}
	// A clean filter stored other bytes than the approved checkout shows.
	other := Seal{Files: []SealFile{{Path: "tests/acceptance/a.sh", SHA256: sha256Hex("assert\n")}}}
	if g := sealTracked(dir, head, other, paths); g.Passed || !strings.Contains(g.Output, "committed bytes differ from the approved ones: tests/acceptance/a.sh") {
		t.Fatalf("got %+v", g)
	}
	two := Seal{Files: []SealFile{{Path: "tests/acceptance/a.sh", SHA256: sha256Hex("a\n")}, {Path: "tests/acceptance/ignored.sh"}}}
	if g := sealTracked(dir, head, two, paths); g.Passed || !strings.Contains(g.Output, "sealed but not tracked: tests/acceptance/ignored.sh") {
		t.Fatalf("got %+v", g)
	}
	if g := sealTracked(dir, head, Seal{}, paths); g.Passed || !strings.Contains(g.Output, "tracked but not sealed") {
		t.Fatalf("got %+v", g)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A triage that ended outside the model (runtime failure, lost container state, stop or
// timeout) runs again; an answer the model gave, or an out-of-memory kill, does not.
func TestTriageRetryClasses(t *testing.T) {
	for class, want := range map[string]bool{ClassInfrastructure: true, ClassTerminated: true, ClassProcess: false, ClassResource: false} {
		if got := triageRetry(LaunchResult{Class: class}); got != want {
			t.Errorf("%s: %v", class, got)
		}
	}
}

// A triage launch the sandbox cannot carry out leaves no route behind: the next run
// triages again instead of keeping the provisional new route.
func TestDockerSandboxedTriageInfrastructureFailureIsRetried(t *testing.T) {
	// The profile's env refers to a variable it does not forward: the launch is refused
	// before any container starts, which is an infrastructure failure.
	tri := "  tri:\n    provider: tri\n    command: [sh, -c, \"echo ROUTE: change\"]\n    env:\n      X: \"$NOT_FORWARDED\"\n"
	f := agentsFixture(t, `true`, `echo "VERDICT: APPROVE"`, tri, "triage:\n  profile: tri\n")
	var log strings.Builder
	_, err := RunOutcome(context.Background(), f.repo, "OUT-001", &log)
	if err == nil || !strings.Contains(err.Error(), "triage could not run in the sandbox") {
		t.Fatalf("got %v\n%s", err, log.String())
	}
	st, err := loadState(f.repo, "OUT-001")
	if err != nil {
		t.Fatal(err)
	}
	if st.InitialRoute != "" || st.TriageSource != "" {
		t.Fatalf("provisional route kept: %q %q", st.InitialRoute, st.TriageSource)
	}
}
