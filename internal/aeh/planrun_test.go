package aeh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The plan fixture's acceptance command runs each test file listed in the manifest.
const (
	runManifest = `while IFS= read -r f; do [ -n "$f" ] && { sh "$f" || exit 1; }; done < "$1"
`
	planID   = "pay-v0_1_0"
	planDir  = "tests/acceptance/" + planID
	authorOK = `d=` + planDir + `; mkdir -p $d
printf 'grep -q "paid" src/pay.txt\n' > $d/pay.sh
printf 'task:\n  goal: Write src/pay.txt saying paid\n  paths: [src/]\ntrace:\n  AC-1: [pay.sh]\n' > $d/plan.yaml
echo "PLAN: READY"`
)

type planFixture struct {
	t    *testing.T
	repo Repo
	dir  string
	tmp  string
}

func newPlanFixture(t *testing.T, author, reviewer string) *planFixture {
	t.Helper()
	for _, tool := range []string{"sh", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("plan tests require " + tool)
		}
	}
	// A repository path with a space and a shell metacharacter: nothing may break or run.
	f := &planFixture{t: t, dir: filepath.Join(t.TempDir(), "AEH QA & co"), tmp: t.TempDir()}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.repo = Repo{Root: f.dir}
	mustGit(t, f.dir, "init", "-q", "-b", "main")
	mustGit(t, f.dir, "config", "core.autocrlf", "false")
	write(t, f.dir, ".gitignore", ".aeh/\n")
	write(t, f.dir, "tests/run-manifest.sh", runManifest)
	write(t, f.dir, "tests/suite.sh", "exit 0\n")
	write(t, f.dir, "src/README", "source\n")
	f.setScript("author", author)
	f.setScript("qa", reviewer)
	f.writeConfig("")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	if _, err := InitMemory(f.repo); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSpec(f.repo, "pay", "Pay", []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, f.repo, "pay", "Pays the cart.\n\n## Acceptance criteria\n- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *planFixture) writeConfig(extra string) {
	f.t.Helper()
	var cfg strings.Builder
	cfg.WriteString("project: test\ncommands:\n  build: \"git --version\"\n  test: \"sh tests/suite.sh\"\n")
	cfg.WriteString("  acceptance: \"sh tests/run-manifest.sh {{manifest}}\"\n  timeout_minutes: 1\n")
	cfg.WriteString("limits:\n  max_diff_lines: 50\nladder: [author]\n")
	cfg.WriteString("reviewers:\n  independence: profile\n  qa: [qa]\n  security: [qa]\n  dba: [qa]\n")
	cfg.WriteString(extra)
	cfg.WriteString("profiles:\n")
	for _, name := range []string{"author", "qa"} {
		fmt.Fprintf(&cfg, "  %s:\n    provider: %s\n    command: [\"sh\", %q, \"{{prompt}}\"]\n    env:\n      AEH_COUNTER: %q\n",
			name, name, filepath.ToSlash(filepath.Join(f.tmp, name+".sh")), filepath.ToSlash(filepath.Join(f.tmp, name+".count")))
	}
	write(f.t, f.dir, "aeh/config.yaml", cfg.String())
}

func (f *planFixture) commitAll(msg string) {
	f.t.Helper()
	mustGit(f.t, f.dir, "add", "-A")
	mustGit(f.t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", msg)
}

func (f *planFixture) setScript(name, script string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.tmp, name+".sh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *planFixture) plan() PlanState {
	f.t.Helper()
	var log strings.Builder
	st, err := PlanFeature(context.Background(), f.repo, "pay", &log)
	if err != nil {
		f.t.Fatalf("plan: %v\n%s", err, log.String())
	}
	return st
}

func (f *planFixture) count(name string) int {
	b, _ := os.ReadFile(filepath.Join(f.tmp, name+".count"))
	return strings.Count(string(b), "\n")
}

const countCall = `echo x >> "$AEH_COUNTER"; `

func TestPlanPublishesSealedOutcome(t *testing.T) {
	f := newPlanFixture(t, authorOK, countCall+reviewOK)
	st := f.plan()
	if st.Phase != PlanLogged || st.Status != PlanLogged || st.ID != planID || st.Route != RouteNew {
		t.Fatalf("plan not finished: %+v", st)
	}
	if head, ok := branchCommit(f.dir, IntegrationBranch(planID)); !ok || head != st.PlanCommit {
		t.Fatalf("branch not at the plan commit: %s %v", head, ok)
	}
	if got := mustGit(t, f.dir, "rev-parse", st.PlanCommit+"^"); got != st.Base {
		t.Fatal("the plan commit must sit directly on the base")
	}
	outcome, ok := gitBlob(f.dir, st.PlanCommit, "aeh/outcomes/"+planID+".yaml")
	if !ok || !strings.Contains(outcome, "id: "+planID) || !strings.Contains(outcome, "paths:\n        - src/") {
		t.Fatalf("generated outcome missing or wrong:\n%s", outcome)
	}
	sealText, ok := gitBlob(f.dir, st.PlanCommit, "aeh/seals/"+planID+".json")
	var seal Seal
	if !ok || json.Unmarshal([]byte(sealText), &seal) != nil || seal.SealedBy != "runner" || seal.SpecContent != st.SpecDigest || len(seal.Files) != 2 {
		t.Fatalf("seal: %s", sealText)
	}
	for _, sf := range seal.Files {
		blob, ok := gitBlob(f.dir, st.PlanCommit, sf.Path)
		sum := sha256.Sum256([]byte(blob))
		if !ok || hex.EncodeToString(sum[:]) != sf.SHA256 {
			t.Fatalf("sealed hash of %s does not match the published file", sf.Path)
		}
	}
	if len(st.Reviews) != 2 || st.Reviews[0].Role != RoleQA || st.Reviews[1].Role != RoleSecurity {
		t.Fatalf("reviews: %+v", st.Reviews)
	}
	for _, v := range st.Reviews {
		if v.SHA != st.PlanCommit || v.Verdict != "APPROVE" || v.Profile != "qa" {
			t.Fatalf("review not bound to the plan commit: %+v", v)
		}
	}
	if st.Checks[0].Name != "build" || !strings.HasPrefix(st.Checks[1].Name, "red ") || !st.Checks[1].Passed {
		t.Fatalf("checks: %+v", st.Checks)
	}
	if log := readFile(t, filepath.Join(memDir(t, f.repo), "wiki", "log.md")); !strings.Contains(log, "plan | pay 0.1.0 -> "+planID) {
		t.Fatalf("memory log: %s", log)
	}
	if err := memoryClean(memDir(t, f.repo)); err != nil {
		t.Fatal(err)
	}
	if out, _ := git(f.dir, "status", "--porcelain"); out != "" {
		t.Fatalf("the main checkout changed: %s", out)
	}
	// Planning again is a no-op.
	reviews := f.count("qa")
	if again := f.plan(); again.PlanCommit != st.PlanCommit || f.count("qa") != reviews {
		t.Fatal("a finished plan ran again")
	}
}

func TestPlanRetriesATestThatPassesOnTheBase(t *testing.T) {
	passing := `d=` + planDir + `; mkdir -p $d
case "$1" in *"passes before the feature exists"*) printf 'grep -q "paid" src/pay.txt\n' > $d/pay.sh ;; *) printf 'exit 0\n' > $d/pay.sh ;; esac
printf 'task:\n  goal: Write src/pay.txt saying paid\n  paths: [src/]\ntrace:\n  AC-1: [pay.sh]\n' > $d/plan.yaml
echo "PLAN: READY"`
	f := newPlanFixture(t, passing, reviewOK)
	st := f.plan()
	if st.Phase != PlanLogged || len(st.Attempts) != 2 || st.Attempts[0].FailedStep != "red" || st.Attempts[1].FailedStep != "" {
		t.Fatalf("expected a red failure, then success: %+v", st.Attempts)
	}
}

func TestPlanRetriesRejectedAndOutOfScopePlans(t *testing.T) {
	// The first attempt writes outside the test directory; the second is rejected by QA once.
	author := `case "$1" in *"previous attempt"*) ` + authorOK + ` ;; *) mkdir -p src && echo x > src/pay.txt; ` + authorOK + ` ;; esac`
	f := newPlanFixture(t, author, `n=$(cat "$AEH_COUNTER" 2>/dev/null | wc -l); `+countCall+`if [ "$n" = 0 ]; then echo "VERDICT: REJECT"; else echo "VERDICT: APPROVE"; fi`)
	f.writeConfig("plan:\n  ladder: [author]\n")
	f.commitAll("plan ladder")
	st := f.plan()
	if st.Status != StatusBlocked {
		// Two attempts with one profile: scope failure, then QA rejection, then the ladder ends.
		t.Fatalf("expected the single-profile ladder to block: %+v", st)
	}
	if len(st.Attempts) != 2 || st.Attempts[0].FailedStep != "scope" || st.Attempts[1].FailedStep != "review" {
		t.Fatalf("attempts: %+v", st.Attempts)
	}
	if !strings.Contains(st.Attempts[0].Feedback, "outside") {
		t.Fatalf("scope feedback: %s", st.Attempts[0].Feedback)
	}
	if err := Answer(f.repo, planID, "Try again."); err != nil {
		t.Fatal(err)
	}
	st = f.plan()
	if st.Phase != PlanLogged || len(st.Attempts) != 3 {
		t.Fatalf("answer did not resume the plan: %+v", st)
	}
}

func TestPlanBlocksOnARedBaseWithoutAttempts(t *testing.T) {
	f := newPlanFixture(t, countCall+authorOK, reviewOK)
	write(t, f.dir, "tests/suite.sh", "exit 1\n")
	f.commitAll("red base")
	st := f.plan()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "not green") || len(st.Attempts) != 0 || f.count("author") != 0 {
		t.Fatalf("a red base must block before any author runs: %+v", st)
	}
}

func TestPlanTooLargeBlocksAndAnswerRetries(t *testing.T) {
	f := newPlanFixture(t, `echo "Too much."; echo "PLAN: TOO-LARGE"`, reviewOK)
	st := f.plan()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "Split the spec") || len(st.Attempts) != 1 {
		t.Fatalf("too-large plan: %+v", st)
	}
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "answer it first") {
		t.Fatalf("a blocked plan must wait for an answer: %v", err)
	}
	if err := Answer(f.repo, planID, "Fits; keep it small."); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", `case "$1" in *"Fits; keep it small."*) `+authorOK+` ;; *) echo "PLAN: TOO-LARGE" ;; esac`)
	if st = f.plan(); st.Phase != PlanLogged {
		t.Fatalf("the answer must reach the author and the plan finish: %+v", st)
	}
}

func TestPlanRefusesChangedInputsOnResume(t *testing.T) {
	f := newPlanFixture(t, `echo "PLAN: TOO-LARGE"`, reviewOK)
	if st := f.plan(); st.Status != StatusBlocked {
		t.Fatalf("setup: %+v", st)
	}
	if err := Answer(f.repo, planID, "Retry."); err != nil {
		t.Fatal(err)
	}
	f.writeConfig("plan:\n  max_files: 5\n")
	_, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err == nil || !strings.Contains(err.Error(), "inputs changed") {
		t.Fatalf("changed inputs must not resume: %v", err)
	}
	if st, _, _ := LoadPlanState(f.repo, planID); !st.RestartRequired {
		t.Fatal("restart not recorded")
	}
	if err := Answer(f.repo, planID, "Retry."); err == nil {
		t.Fatal("a restart-required plan accepted an answer")
	}
}

func TestPlanPublishRecovery(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	st := f.plan()
	branch := IntegrationBranch(planID)
	// A crash after choosing the commit but before creating the branch: aeh plan creates it.
	mustGit(t, f.dir, "branch", "-D", branch)
	st.Phase, st.Status = PlanPublishing, StatusRunning
	if err := savePlanState(f.repo, &st); err != nil {
		t.Fatal(err)
	}
	if got := f.plan(); got.Phase != PlanLogged {
		t.Fatalf("recovery: %+v", got)
	}
	if head, _ := branchCommit(f.dir, branch); head != st.PlanCommit {
		t.Fatal("branch not recreated at the plan commit")
	}
	// The branch at another commit is a conflict aeh never resolves by moving it.
	mustGit(t, f.dir, "branch", "-f", branch, st.Base)
	st.Phase, st.Status = PlanPublishing, StatusRunning
	if err := savePlanState(f.repo, &st); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflict not reported: %v", err)
	}
	if head, _ := branchCommit(f.dir, branch); head != st.Base {
		t.Fatal("aeh moved a conflicting branch")
	}
}

func TestPlanRefusesCollidingIDs(t *testing.T) {
	for name, setup := range map[string]func(f *planFixture){
		"outcome": func(f *planFixture) { write(f.t, f.dir, "aeh/outcomes/PAY-V0_1_0.yaml", "id: x\n") },
		"branch":  func(f *planFixture) { mustGit(f.t, f.dir, "branch", "aeh/Pay-v0_1_0") },
		"tests": func(f *planFixture) {
			write(f.t, f.dir, "tests/acceptance/pay-V0_1_0/x.sh", "exit 0\n")
			f.commitAll("tests")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPlanFixture(t, countCall+authorOK, reviewOK)
			setup(f)
			if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "already used") {
				t.Fatalf("collision not refused: %v", err)
			}
			if f.count("author") != 0 {
				t.Fatal("an author ran for a colliding ID")
			}
		})
	}
}

func TestPlanNeedsAcceptanceCommandAndApprovedNewCriteria(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "  acceptance: \"sh tests/run-manifest.sh {{manifest}}\"\n", "", 1)
	write(t, f.dir, "aeh/config.yaml", cfg)
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "commands.acceptance") {
		t.Fatalf("missing acceptance command: %v", err)
	}
	f.writeConfig("")
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.", "then src/pay.txt says paid!")
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved spec planned: %v", err)
	}
}

func TestPlanRequiresTheLastImplementationInTheBase(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	snap, _ := LoadMemory(f.repo)
	spec, _ := snap.Spec("pay")
	recordImplementation(t, f.repo, spec, "pay-v0_1_0", fakeSHA('a'))
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.\n", "then src/pay.txt says paid.\n- AC-2: Given a paid cart, when paid again, then nothing changes.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	_, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err == nil || !strings.Contains(err.Error(), "not in the base") {
		t.Fatalf("an unmerged implementation must block planning: %v", err)
	}
}

// Changing an implemented criterion is out of scope until test revision exists (stage 5).
func TestPlanRefusesChangedCriteria(t *testing.T) {
	f := newPlanFixture(t, countCall+authorOK, reviewOK)
	snap, _ := LoadMemory(f.repo)
	spec, _ := snap.Spec("pay")
	recordImplementation(t, f.repo, spec, "pay-v0_1_0", mustGit(t, f.dir, "rev-parse", "HEAD"))
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.\n", "then src/pay.txt says PAID.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	_, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err == nil || !strings.Contains(err.Error(), "changed [1]") {
		t.Fatalf("changed criterion planned: %v", err)
	}
	// With the change undone and a new criterion added, the delta is only the new one.
	editSpec(t, f.repo, "pay", "then src/pay.txt says PAID.\n", "then src/pay.txt says paid.\n- AC-2: Given a paid cart, when paid again, then nothing changes.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", `exit 3`)
	var log strings.Builder
	st, err := PlanFeature(context.Background(), f.repo, "pay", &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if st.Route != RouteChange || len(st.Delta.New) != 1 || st.Delta.New[0].ID != 2 || len(st.Delta.Unchanged) != 1 {
		t.Fatalf("delta and route: %+v", st)
	}
}

func TestPlanVerdict(t *testing.T) {
	for out, want := range map[string]string{
		"done\nPLAN: READY\n":       "READY",
		"PLAN: TOO-LARGE  \r\n\r\n": "TOO-LARGE",
		"PLAN: READY\nmore text\n":  "",
		"plan: ready\n":             "",
		"":                          "",
		"PLAN: READY PLAN: READY\n": "",
	} {
		if got := planVerdict(out); got != want {
			t.Errorf("planVerdict(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestPlanMemoryLogIsIdempotent(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	st := f.plan()
	logPath := filepath.Join(memDir(t, f.repo), "wiki", "log.md")
	entry := "plan | pay 0.1.0 -> " + planID
	// A crash after the memory commit, before the state recorded it.
	st.Phase, st.Status = PlanPublished, StatusRunning
	if err := savePlanState(f.repo, &st); err != nil {
		t.Fatal(err)
	}
	if got := f.plan(); got.Phase != PlanLogged || strings.Count(readFile(t, logPath), entry) != 1 {
		t.Fatalf("log entry duplicated or plan unfinished: %+v", got)
	}
	// A crash between writing the log and committing it.
	mem := memDir(t, f.repo)
	mustGit(t, mem, "reset", "-q", "HEAD~1")
	if err := savePlanState(f.repo, &st); err != nil {
		t.Fatal(err)
	}
	if got := f.plan(); got.Phase != PlanLogged || strings.Count(readFile(t, logPath), entry) != 1 {
		t.Fatalf("pending log entry not committed once: %+v", got)
	}
	if err := memoryClean(mem); err != nil {
		t.Fatal(err)
	}
	if out := mustGit(t, mem, "log", "--format=%B", "-1"); !strings.Contains(out, "AEH-Plan: "+planID+" "+st.PlanCommit) {
		t.Fatalf("plan marker missing: %s", out)
	}
}

func TestPlanStatusAndLinkedWorktree(t *testing.T) {
	f := newPlanFixture(t, `echo "PLAN: TOO-LARGE"`, reviewOK)
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	linked := filepath.Join(t.TempDir(), "linked")
	mustGit(t, f.dir, "worktree", "add", "-q", "-b", "side", linked)
	write(t, linked, "side.txt", "x\n")
	mustGit(t, linked, "add", "-A")
	mustGit(t, linked, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "side")
	st, err := PlanFeature(context.Background(), Repo{Root: linked}, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Base != base {
		t.Fatalf("a plan from a linked worktree must pin the main checkout's HEAD, got %s", st.Base)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "state", planID+".plan.json")); err != nil {
		t.Fatalf("plan state not in the main checkout: %v", err)
	}
	if _, err := ListStates(f.repo); err != nil {
		t.Fatalf("status broke with a plan record: %v", err)
	}
	plans, err := ListPlanStates(Repo{Root: linked})
	if err != nil || len(plans) != 1 || plans[0].Status != StatusBlocked {
		t.Fatalf("plans: %+v %v", plans, err)
	}
	if err := Answer(Repo{Root: linked}, planID, "Retry."); err != nil {
		t.Fatalf("answer from a linked worktree: %v", err)
	}
	if st, _, _ := LoadPlanState(f.repo, planID); st.Status != StatusPending || len(st.Answers) != 1 {
		t.Fatalf("answer not recorded in the main checkout: %+v", st)
	}
}

func TestPlanRecoversStaleWorktreesAndInterruptedAttempts(t *testing.T) {
	f := newPlanFixture(t, `echo "PLAN: TOO-LARGE"`, reviewOK)
	// Something left at the baseline worktree's path, as after a crash.
	stale := f.repo.WorktreePath(planID + "-plan-baseline")
	write(t, stale, "leftover.txt", "x\n")
	st := f.plan()
	if st.Status != StatusBlocked || len(st.Attempts) != 1 {
		t.Fatalf("setup: %+v", st)
	}
	// An attempt that was running when the process stopped.
	st.Attempts = append(st.Attempts, PlanAttempt{N: 2, Profile: "author", FailedStep: "running"})
	if err := savePlanState(f.repo, &st); err != nil {
		t.Fatal(err)
	}
	write(t, f.repo.WorktreePath(planID+"-plan-a2"), "leftover.txt", "x\n")
	if err := Answer(f.repo, planID, "Go."); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", authorOK)
	st = f.plan()
	if st.Phase != PlanLogged || st.Attempts[1].FailedStep != "interrupted" || len(st.Attempts) != 3 {
		t.Fatalf("recovery: %+v", st.Attempts)
	}
	if _, err := os.Stat(f.repo.WorktreePath(planID + "-plan-a2")); !os.IsNotExist(err) {
		t.Fatal("the interrupted attempt's worktree was not removed")
	}
}

func TestPlanRefusesAnUntrackedTestDirectory(t *testing.T) {
	f := newPlanFixture(t, countCall+authorOK, reviewOK)
	write(t, f.dir, "tests/acceptance/PAY-v0_1_0/local.sh", "exit 0\n") // untracked
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("untracked test directory not detected: %v", err)
	}
}
