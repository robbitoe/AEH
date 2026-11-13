package aeh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the real runner against a throwaway git repository.
// The "agents" are shell scripts, so no model is called.

const acceptanceCheck = `#!/bin/sh
grep -q "fees ok" src/fees.txt
`

// Agent scripts. Each receives the prompt as its first argument.
const (
	agentGood   = `mkdir -p src && echo "fees ok" > src/fees.txt`
	agentNoop   = `true`
	agentTamper = `echo "exit 0" > tests/acceptance/check.sh; mkdir -p src && echo wrong > src/fees.txt`
	agentScope  = `mkdir -p lib && echo "fees ok" > lib/other.txt`
	// fails once (no output), then succeeds; the counter lives outside the repo
	agentFlaky   = `c="$AEH_COUNTER"; n=$(cat "$c" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$c"; if [ $n -ge 2 ]; then mkdir -p src && echo "fees ok" > src/fees.txt; fi`
	reviewOK     = `echo "All changes trace to the task."; echo "VERDICT: APPROVE"`
	reviewReject = `echo "Adds an unrequested endpoint."; echo "VERDICT: REJECT"`
)

type fixture struct {
	t    *testing.T
	repo Repo
	dir  string
}

func newFixture(t *testing.T, profiles map[string]string, ladder []string, reviewer string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("end-to-end tests require sh (Git for Windows provides it)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	tmp := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "core.autocrlf", "false")

	if reviewer == "" {
		reviewer = "qa-default"
		profiles[reviewer] = reviewOK
	}

	var cfg strings.Builder
	cfg.WriteString("project: test\ncommands:\n  build: \"git --version\"\n  test: \"sh tests/acceptance/check.sh\"\n  timeout_minutes: 1\n")
	cfg.WriteString("limits:\n  max_diff_lines: 50\n")
	fmt.Fprintf(&cfg, "ladder: [%s]\n", strings.Join(ladder, ", "))
	fmt.Fprintf(&cfg, "reviewer: %q\n", reviewer)
	cfg.WriteString("profiles:\n")
	for name, script := range profiles {
		path := filepath.Join(tmp, name+".sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&cfg, "  %s:\n    command: [\"sh\", %q, \"{{prompt}}\"]\n    env:\n      AEH_COUNTER: %q\n",
			name, filepath.ToSlash(path), filepath.ToSlash(filepath.Join(tmp, name+".count")))
	}
	write(t, dir, "aeh/config.yaml", cfg.String())
	write(t, dir, "aeh/outcomes/OUT-001.yaml", `id: OUT-001
title: Generate fees
level: 2
criteria:
  - given: no fees
    when: generating
    then: fees ok
acceptance: [tests/acceptance/]
tasks:
  - id: T1
    goal: Write src/fees.txt containing "fees ok"
    paths: [src/]
`)
	write(t, dir, "tests/acceptance/check.sh", acceptanceCheck)
	write(t, dir, ".gitignore", ".aeh/\n")
	repo := Repo{Root: dir}
	if _, err := ApproveTests(repo, "OUT-001"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	return &fixture{t: t, repo: repo, dir: dir}
}

func (f *fixture) run() (State, string) {
	f.t.Helper()
	var log strings.Builder
	st, err := RunOutcome(context.Background(), f.repo, "OUT-001", &log)
	if err != nil {
		f.t.Fatalf("run: %v\n%s", err, log.String())
	}
	return st, log.String()
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunAcceptsOnFirstAttempt(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
	st, log := f.run()
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
	if n := len(st.Tasks[0].Attempts); n != 1 {
		t.Fatalf("expected 1 attempt, got %d", n)
	}
	content := mustGit(t, f.dir, "show", IntegrationBranch("OUT-001")+":src/fees.txt")
	if content != "fees ok" {
		t.Fatalf("integration branch content %q", content)
	}
	if mustGit(t, f.dir, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatal("the user's branch must not change")
	}
	if _, err := os.Stat(f.repo.ReportPath("OUT-001")); err != nil {
		t.Fatal("report not written")
	}
	if out, _ := git(f.dir, "branch", "--list", "aeh-wip/*"); out != "" {
		t.Fatalf("attempt branches left behind: %s", out)
	}
}

func TestRetryReceivesFeedback(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentFlaky}, []string{"medio"}, "")
	st, log := f.run()
	if st.Status != StatusAccepted || len(st.Tasks[0].Attempts) != 2 {
		t.Fatalf("expected acceptance on attempt 2, got %s with %d attempts\n%s", st.Status, len(st.Tasks[0].Attempts), log)
	}
	if st.Tasks[0].Attempts[0].FailedGate != "scope" {
		t.Fatalf("first attempt should fail scope (no changes), got %q", st.Tasks[0].Attempts[0].FailedGate)
	}
	prompt, err := os.ReadFile(filepath.Join(f.dir, st.Tasks[0].Attempts[1].RunDir, "agent.prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "previous attempt was rejected") {
		t.Fatal("second prompt does not carry the failure feedback")
	}
}

func TestEscalatesOnThirdAttempt(t *testing.T) {
	f := newFixture(t, map[string]string{"local": agentNoop, "medio": agentGood}, []string{"local", "medio"}, "")
	st, log := f.run()
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
	var profiles []string
	for _, a := range st.Tasks[0].Attempts {
		profiles = append(profiles, a.Profile)
	}
	if got := strings.Join(profiles, ","); got != "local,local,medio" {
		t.Fatalf("ladder %s", got)
	}
}

func TestBlocksAndResumesAfterAnswer(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentScope}, []string{"medio"}, "")
	st, _ := f.run()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "outside the task's paths") {
		t.Fatalf("expected blocked on scope, got %s: %s", st.Status, st.Question)
	}
	if _, err := RunOutcome(context.Background(), f.repo, "OUT-001", nil); err == nil {
		t.Fatal("a blocked outcome must not run until answered")
	}
	if err := Answer(f.repo, "OUT-001", "write only under src/"); err != nil {
		t.Fatal(err)
	}
	// The owner also fixes the agent; the answer reaches the next prompt.
	if err := os.WriteFile(f.scriptPath("medio"), []byte("#!/bin/sh\n"+agentGood+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, log := f.run()
	if st.Status != StatusAccepted || len(st.Tasks[0].Attempts) != 4 {
		t.Fatalf("expected accepted on attempt 4, got %s (%d)\n%s", st.Status, len(st.Tasks[0].Attempts), log)
	}
	prompt, _ := os.ReadFile(filepath.Join(f.dir, st.Tasks[0].Attempts[3].RunDir, "agent.prompt.md"))
	if !strings.Contains(string(prompt), "write only under src/") {
		t.Fatal("answer missing from the prompt")
	}
}

func TestTamperingWithSealedTestsIsRejected(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentTamper}, []string{"medio"}, "")
	st, _ := f.run()
	if st.Status != StatusBlocked {
		t.Fatalf("expected blocked, got %s", st.Status)
	}
	for _, a := range st.Tasks[0].Attempts {
		if a.FailedGate != "hash" {
			t.Fatalf("expected every attempt to fail the hash gate, got %q", a.FailedGate)
		}
	}
	if _, ok := branchCommit(f.dir, IntegrationBranch("OUT-001")); !ok {
		t.Fatal("integration branch missing")
	}
	content, _ := git(f.dir, "show", IntegrationBranch("OUT-001")+":tests/acceptance/check.sh")
	if content != strings.TrimSpace(acceptanceCheck) {
		t.Fatal("sealed tests changed on the integration branch")
	}
}

func TestEditedTestsNeedReapproval(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
	write(t, f.dir, "tests/acceptance/check.sh", "exit 0\n")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-am", "weaken test")
	_, err := RunOutcome(context.Background(), f.repo, "OUT-001", nil)
	if err == nil || !strings.Contains(err.Error(), "changed since they were approved") {
		t.Fatalf("expected re-approval error, got %v", err)
	}
}

func TestReviewerCanReject(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood, "fuerte": reviewReject}, []string{"medio"}, "fuerte")
	st, _ := f.run()
	if st.Status != StatusBlocked || st.Tasks[0].Attempts[0].FailedGate != "review" {
		t.Fatalf("expected blocked by review, got %s / %q", st.Status, st.Tasks[0].Attempts[0].FailedGate)
	}

	g := newFixture(t, map[string]string{"medio": agentGood, "fuerte": reviewOK}, []string{"medio"}, "fuerte")
	st, log := g.run()
	if st.Status != StatusAccepted {
		t.Fatalf("expected accepted with approving reviewer, got %s\n%s", st.Status, log)
	}
}

func TestDoRunsLevelOne(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
	var log strings.Builder
	st, err := Do(context.Background(), f.repo, "write the fees file", []string{"src/"}, "", MemoryLink{}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log.String())
	}
}

func TestMissingCLIFailsFast(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
	cfg := strings.ReplaceAll(readFile(t, f.repo.ConfigPath()), `["sh",`, `["definitely-not-installed",`)
	write(t, f.dir, "aeh/config.yaml", cfg)
	_, err := RunOutcome(context.Background(), f.repo, "OUT-001", nil)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected preflight error, got %v", err)
	}
	if _, ok := branchCommit(f.dir, IntegrationBranch("OUT-001")); ok {
		t.Fatal("preflight must fail before creating branches")
	}
}

func TestLevelTwoRequiresSeparateReviewer(t *testing.T) {
	for _, reviewer := range []string{"", "medio"} {
		t.Run("reviewer="+reviewer, func(t *testing.T) {
			f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
			cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), `reviewer: "qa-default"`, fmt.Sprintf("reviewer: %q", reviewer), 1)
			write(t, f.dir, "aeh/config.yaml", cfg)
			_, err := RunOutcome(context.Background(), f.repo, "OUT-001", nil)
			if err == nil || !strings.Contains(err.Error(), "reviewer profile") {
				t.Fatalf("expected reviewer configuration error, got %v", err)
			}
			if _, ok := branchCommit(f.dir, IntegrationBranch("OUT-001")); ok {
				t.Fatal("invalid configuration created an integration branch")
			}
		})
	}
}

func TestRunnerRejectsUncheckedCandidateChanges(t *testing.T) {
	cases := []struct{ name, agent, reviewer, build, gate string }{
		{"agent commit", "git -c user.name=t -c user.email=t@t commit --allow-empty -qm unexpected; " + agentGood, reviewOK, "", "scope"},
		{"quoted protected path", agentGood + `; mkdir -p aeh; echo hidden > aeh/café`, reviewOK, "", "scope"},
		{"build restages", agentGood, reviewOK, `sh -c "echo tampered > src/fees.txt; git add -A"`, "build"},
		{"reviewer restages", agentGood, "echo tampered > src/fees.txt; git add -A; " + reviewOK, "", "review"},
		{"failed reviewer approves", agentGood, reviewOK + "; exit 7", "", "review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"medio": c.agent, "qa": c.reviewer}, []string{"medio"}, "qa")
			if c.build != "" {
				cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), `build: "git --version"`, fmt.Sprintf("build: %q", c.build), 1)
				write(t, f.dir, "aeh/config.yaml", cfg)
			}
			base := mustGit(t, f.dir, "rev-parse", "HEAD")
			st, _ := f.run()
			if st.Status != StatusBlocked || st.Tasks[0].Attempts[0].FailedGate != c.gate {
				t.Fatalf("expected rejection at %s, got %+v", c.gate, st)
			}
			if got := mustGit(t, f.dir, "rev-parse", IntegrationBranch("OUT-001")); got != base {
				t.Fatalf("integration branch advanced from %s to %s", base, got)
			}
		})
	}
}

func TestResumeRejectsContractAndBranchDrift(t *testing.T) {
	for _, change := range []string{"contract", "branch"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t, map[string]string{"medio": agentScope}, []string{"medio"}, "")
			st, _ := f.run()
			if st.Status != StatusBlocked {
				t.Fatalf("status %s", st.Status)
			}
			if err := Answer(f.repo, st.ID, "retry"); err != nil {
				t.Fatal(err)
			}
			if change == "contract" {
				cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "max_diff_lines: 50", "max_diff_lines: 51", 1)
				write(t, f.dir, "aeh/config.yaml", cfg)
			} else {
				mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-qm", "move")
				mustGit(t, f.dir, "branch", "-f", st.Branch, "HEAD")
			}
			_, err := RunOutcome(context.Background(), f.repo, st.ID, nil)
			if err == nil || !strings.Contains(err.Error(), "differs from recorded state") {
				t.Fatalf("expected resume to reject drift, got %v", err)
			}
		})
	}
}

func TestResumeAllowsProfileChanges(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentScope}, []string{"medio"}, "")
	st, _ := f.run()
	if st.Status != StatusBlocked {
		t.Fatalf("status %s", st.Status)
	}
	if err := Answer(f.repo, st.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "  timeout_minutes: 1\n", "  timeout_minutes: 2\n", 1)
	outcome := strings.Replace(readFile(t, f.repo.OutcomePath(st.ID)), "    paths: [src/]", "    paths: [src/]\n    start: medio", 1)
	write(t, f.dir, "aeh/outcomes/OUT-001.yaml", outcome)
	cfg += "    timeout_minutes: 5\nnotify:\n  webhook: \"\"\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	st, err := RunOutcome(context.Background(), f.repo, st.ID, nil)
	if err != nil {
		t.Fatalf("resume after a profile change: %v", err)
	}
	if st.Status != StatusBlocked {
		t.Fatalf("status %s", st.Status)
	}
}

func TestInterruptedAgentRetainsWorktreeWithoutRetry(t *testing.T) {
	// Signal readiness after the sleeping child has started, then cancel while
	// both the shell and its child still hold the agent's log handles.
	f := newFixture(t, map[string]string{"medio": agentGood + "; sleep 30 & echo ready > agent-ready; wait"}, []string{"medio"}, "")
	wt := f.repo.WorktreePath("OUT-001-T1-a1")
	t.Cleanup(func() {
		removeWorktree(f.dir, wt)
		deleteBranch(f.dir, AttemptBranch("OUT-001", "T1", 1))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := os.Stat(filepath.Join(wt, "agent-ready")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	st, err := RunOutcome(ctx, f.repo, "OUT-001", nil)
	if err != nil || st.Status != StatusBlocked || len(st.Tasks[0].Attempts) != 1 {
		t.Fatalf("expected one blocked attempt, got %+v, %v", st, err)
	}
	if got := st.Tasks[0].Attempts[0].Worktree; got != wt {
		t.Fatalf("retained worktree = %q, want %q", got, wt)
	}
	if _, err := os.Stat(filepath.Join(wt, "src", "fees.txt")); err != nil {
		t.Fatal(err)
	}
	if st.Head != st.Base {
		t.Fatal("interrupted attempt advanced the integration branch")
	}
	for _, name := range []string{"agent.out", "agent.err"} {
		if err := os.Remove(filepath.Join(f.repo.RunDir("OUT-001", "T1-a1"), name)); err != nil {
			t.Fatalf("interrupted agent still holds %s: %v", name, err)
		}
	}
}

func (f *fixture) scriptPath(profile string) string {
	cfg := readFile(f.t, f.repo.ConfigPath())
	for _, line := range strings.Split(cfg, "\n") {
		if strings.Contains(line, profile+".sh") {
			start := strings.Index(line, `"sh", "`) + len(`"sh", "`)
			end := strings.Index(line[start:], `"`)
			return line[start : start+end]
		}
	}
	f.t.Fatalf("no script for %s", profile)
	return ""
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
