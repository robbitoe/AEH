package aeh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The characterization author pins behavior the base already has: src/pay.txt says paid.
const (
	charAuthorOK = `d=` + planDir + `; mkdir -p $d
printf 'grep -q "paid" src/pay.txt\n' > $d/pay.sh
printf 'grep -q "EUR" src/pay.txt\n' > $d/currency.sh
printf 'kind: characterize\ntrace:\n  AC-1: [pay.sh]\n  AC-3: [currency.sh]\n' > $d/plan.yaml
echo "PLAN: READY"`
	// A test of behavior the code does not have: it fails on the existing code, printing a
	// credential the way a misconfigured system might.
	charSecret    = "ghp_CHARACTERIZATIONSECRETAAAAAAAAAAAAAA" // ghp_ and 36 characters
	charAuthorRed = `d=` + planDir + `; mkdir -p $d
printf 'echo token ` + charSecret + `; grep -q "refunded" src/pay.txt\n' > $d/pay.sh
printf 'grep -q "EUR" src/pay.txt\n' > $d/currency.sh
printf 'kind: characterize\ntrace:\n  AC-1: [pay.sh]\n  AC-3: [currency.sh]\n' > $d/plan.yaml
echo "PLAN: READY"`
)

// newCharFixture is an adopted feature "pay": three observations at the base, AC-1 and AC-3
// kept, AC-2 dropped, promoted and approved by the owner.
func newCharFixture(t *testing.T, author string) (*planFixture, string) {
	t.Helper()
	for _, tool := range []string{"sh", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("characterization tests require " + tool)
		}
	}
	endToEnd(t)
	f := &planFixture{t: t, dir: filepath.Join(t.TempDir(), "char repo"), tmp: t.TempDir()}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.repo = Repo{Root: f.dir}
	mustGit(t, f.dir, "init", "-q", "-b", "main")
	mustGit(t, f.dir, "config", "core.autocrlf", "false")
	write(t, f.dir, ".gitignore", ".aeh/\n")
	write(t, f.dir, "tests/run-manifest.sh", runManifest)
	write(t, f.dir, "tests/suite.sh", "exit 0\n")
	write(t, f.dir, "src/pay.txt", "paid in EUR\n")
	f.setScript("author", author)
	f.setScript("qa", countCall+reviewOK)
	f.writeConfig("")
	f.commitAll("existing system")
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	if _, err := InitMemory(f.repo); err != nil {
		t.Fatal(err)
	}
	draft := &areaDraft{Front: areaFront{Title: "Pay", Paths: []string{"src/**"}}, Prose: "Pays the cart.\n"}
	if _, err := recordAreaObservations(f.repo, "pay", draft, []observationInput{
		{Text: "Given a cart, when the user pays, then src/pay.txt says paid.", Locations: []string{"src/pay.txt:1"}},
		{Text: "Given a cart, when the user pays twice, then nothing happens.", Locations: []string{"src/pay.txt:1"}},
		{Text: "Given a cart, when the user pays, then the currency is EUR.", Locations: []string{"src/pay.txt:1"}},
	}, base); err != nil {
		t.Fatal(err)
	}
	for id, decision := range map[int]string{1: DecisionKeep, 2: DecisionDrop, 3: DecisionKeep} {
		area, err := LoadAdoptArea(f.repo, "pay")
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range area.Observations {
			if o.ID == id {
				if _, err := AdoptConfirm(f.repo, "pay", fmt.Sprintf("AC-%d", o.ID), decision, o.Token()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := AdoptPromote(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	return f, base
}

func (f *planFixture) seal() (PlanState, string, error) {
	f.t.Helper()
	var log strings.Builder
	st, err := AdoptSeal(context.Background(), f.repo, "pay", &log)
	return st, log.String(), err
}

func areaState(t *testing.T, r Repo, feature string) string {
	t.Helper()
	st, err := AdoptStatus(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range st.Areas {
		if a.Feature == feature {
			return a.State
		}
	}
	t.Fatalf("area %s not listed: %+v", feature, st.Areas)
	return ""
}

func TestAdoptSealPublishesAndRecordsCharacterization(t *testing.T) {
	f, observed := newCharFixture(t, charAuthorOK)
	if got := areaState(t, f.repo, "pay"); got != "promoted" {
		t.Fatalf("before sealing: %s", got)
	}
	// An adopted feature is characterized before any change is planned.
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "aeh adopt seal pay") {
		t.Fatalf("aeh plan must send an adopted feature to characterization first: %v", err)
	}
	st, log, err := f.seal()
	if err != nil || st.Phase != PlanLogged || st.Kind != PlanCharacterize {
		t.Fatalf("seal: %v %+v\n%s", err, st, log)
	}
	if st.ObservedBase != observed || st.Route != RouteChange || reviewRolesOf(st.Reviews) != "qa,security" {
		t.Fatalf("provenance, route or reviews: %+v", st)
	}
	var green int
	for _, c := range st.Checks {
		if strings.HasPrefix(c.Name, "green ") && c.Passed {
			green++
		}
	}
	if green != 2 {
		t.Fatalf("every new file must pass on its own: %+v", st.Checks)
	}
	outcome, ok := gitBlob(f.dir, st.PlanCommit, "aeh/outcomes/"+planID+".yaml")
	if !ok || !strings.Contains(outcome, "kind: characterize") || !strings.Contains(outcome, "tasks: []") {
		t.Fatalf("generated outcome:\n%s", outcome)
	}
	if !strings.Contains(log, "Nothing to implement") {
		t.Fatalf("log: %s", log)
	}
	// A crash after publishing and before the memory record: the next seal records it, although
	// the record is what moves the spec's version.
	mustGit(t, memDir(t, f.repo), "reset", "-q", "--hard", "HEAD~1")
	crashed := st
	crashed.Phase, crashed.Status = PlanPublished, StatusRunning
	if err := savePlanState(f.repo, &crashed); err != nil {
		t.Fatal(err)
	}
	if got := areaState(t, f.repo, "pay"); got != "characterized" {
		t.Fatalf("published but not recorded: %s", got)
	}
	if again, log, err := f.seal(); err != nil || again.Phase != PlanLogged || again.PlanCommit != st.PlanCommit {
		t.Fatalf("resume the record: %v %+v\n%s", err, again, log)
	}
	// Memory records the published commit, with the inspected commit apart.
	snap, err := LoadMemory(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := snap.Spec("pay")
	im, ok := spec.LastImplementation()
	if !ok || im.Code != st.PlanCommit || im.ObservedBase != observed || im.Outcome != planID || im.MaxCriterion != 3 {
		t.Fatalf("implementation: %+v", im)
	}
	if len(im.Criteria) != 2 || im.Criteria[0].ID != 1 || im.Criteria[1].ID != 3 || im.Criteria[1].Tests[0] != planDir+"/currency.sh" {
		t.Fatalf("criteria: %+v", im.Criteria)
	}
	if spec.Version != "0.2.0" || !spec.Approved() {
		t.Fatalf("spec after the record: %s approved=%v", spec.Version, spec.Approved())
	}
	if out, _ := git(f.dir, "status", "--porcelain"); out != "" {
		t.Fatalf("the main checkout changed: %s", out)
	}
	// aeh run refuses it: there is nothing to implement.
	if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "nothing to implement; merge aeh/"+planID) {
		t.Fatalf("run must refuse a characterization: %v", err)
	}
	// Sealing again is a no-op.
	reviews := f.count("qa")
	if again, _, err := f.seal(); err != nil || again.PlanCommit != st.PlanCommit || f.count("qa") != reviews {
		t.Fatalf("a finished characterization ran again: %v", err)
	}
	if got := areaState(t, f.repo, "pay"); got != "published" {
		t.Fatalf("after publishing: %s", got)
	}
	// A later version adds a fix as a new criterion; it plans only after the owner merged.
	mem := memDir(t, f.repo)
	kept := "- AC-3: Given a cart, when the user pays, then the currency is EUR.\n"
	text := readFile(t, filepath.Join(mem, "wiki", "features", "pay", "spec.md"))
	if !strings.Contains(text, kept) {
		t.Fatalf("promoted spec:\n%s", text)
	}
	write(t, mem, "wiki/features/pay/spec.md", strings.Replace(text, kept, kept+"- AC-4: Given a paid cart, when the user pays twice, then nothing happens.\n", 1))
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "add AC-4")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", `echo "PLAN: TOO-LARGE"`)
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "not in the base") {
		t.Fatalf("a plan before the merge must be refused: %v", err)
	}
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "--no-edit", IntegrationBranch(planID))
	if got := areaState(t, f.repo, "pay"); got != "merged" {
		t.Fatalf("after the merge: %s", got)
	}
	next, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err != nil || next.Status != StatusBlocked || len(next.Delta.New) != 1 || next.Delta.New[0].ID != 4 || len(next.Delta.Unchanged) != 2 {
		t.Fatalf("the next plan must start from the characterization: %v %+v", err, next)
	}
}

func TestAdoptSealRejectsTestsThatFailOnTheExistingCode(t *testing.T) {
	f, _ := newCharFixture(t, charAuthorRed)
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked || len(st.Attempts) != 2 {
		t.Fatalf("a red characterization must block after the ladder: %v %+v\n%s", err, st, log)
	}
	a := st.Attempts[0]
	if a.FailedStep != "green" || !strings.Contains(a.Feedback, "fails on the existing code") || f.count("qa") != 0 {
		t.Fatalf("attempt: %+v", a)
	}
	// The command's output is redacted wherever it goes: feedback, state, terminal.
	statePath, err := planStatePath(f.repo, planID)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"state": readFile(t, statePath), "log": log, "question": st.Question} {
		if strings.Contains(text, charSecret) {
			t.Fatalf("the secret reached the %s", where)
		}
	}
	if !strings.Contains(a.Feedback, "[redacted") {
		t.Fatalf("feedback should show the redaction: %s", a.Feedback)
	}
	if _, ok := branchCommit(f.dir, IntegrationBranch(planID)); ok {
		t.Fatal("nothing may be published")
	}
	if got := areaState(t, f.repo, "pay"); got != "characterizing (blocked)" {
		t.Fatalf("state: %s", got)
	}
	// An answer retries; a fixed author finishes the same plan.
	if err := Answer(f.repo, planID, "pin only what the code does"); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", charAuthorOK)
	if st, log, err := f.seal(); err != nil || st.Phase != PlanLogged {
		t.Fatalf("retry: %v %+v\n%s", err, st, log)
	}
}

func TestAdoptSealRequiresACharacterizePlan(t *testing.T) {
	// A plan with a task is an implementation plan, never a characterization.
	f, _ := newCharFixture(t, authorOK)
	st, _, err := f.seal()
	if err != nil || st.Status != StatusBlocked || st.Attempts[0].FailedStep != "scope" ||
		!strings.Contains(st.Attempts[0].Feedback, "kind: characterize") {
		t.Fatalf("a task plan must be rejected: %v %+v", err, st)
	}
	// The plan state keeps its kind: aeh plan does not take it over.
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "aeh adopt seal pay") {
		t.Fatalf("aeh plan must not take over a characterization: %v", err)
	}
}

func TestAdoptSealRefusesFeaturesThatAreNotAdopted(t *testing.T) {
	f := newPlanFixture(t, charAuthorOK, reviewOK)
	if _, _, err := f.seal(); err == nil || !strings.Contains(err.Error(), "no adopted area") {
		t.Fatalf("characterization applies only to adopted features: %v", err)
	}
	// A characterize plan.yaml is refused in a normal plan.
	st := f.plan()
	if st.Status != StatusBlocked || st.Attempts[0].FailedStep != "scope" || !strings.Contains(st.Attempts[0].Feedback, "must not declare a kind") {
		t.Fatalf("a normal plan must refuse kind: characterize: %+v", st)
	}
}

// Empty new fields keep the JSON that execution contracts and plan input digests hashed
// before characterization existed, so unfinished normal runs and plans still resume.
func TestCharacterizeFieldsKeepOldJSON(t *testing.T) {
	for name, v := range map[string]any{
		"outcome":        Outcome{ID: "X", Title: "x"},
		"implementation": Implementation{Outcome: "X", Code: obsSHA},
		"plan":           PlanFile{Task: PlanTask{Goal: "g"}},
		"state":          PlanState{ID: "X"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "Kind") || strings.Contains(s, "kind") || strings.Contains(s, "bserved") {
			t.Errorf("%s: %s", name, s)
		}
	}
}

func TestCharacterizationRulesAreConservative(t *testing.T) {
	rules := []TriageRule{
		{Name: "migrations", Paths: []string{"src/db/*.sql"}, Route: RouteNew, Reviewers: []string{RoleDBA}},
		{Name: "auth", Paths: []string{"src/auth/**"}, Route: RouteNew},
		{Name: "docs", Paths: []string{"docs/**"}, Route: RouteNew},
	}
	got := ruleNames(rulesForArea(rules, []string{"src/db/migration*.sql"}))
	if strings.Join(got, ",") != "migrations" {
		t.Fatalf("a glob area must raise overlapping glob rules: %v", got)
	}
	if got := ruleNames(rulesForArea(rules, []string{"src/**"})); strings.Join(got, ",") != "migrations,auth" {
		t.Fatalf("an enclosing area raises every rule inside it: %v", got)
	}
}

func TestCharacterizeSchema(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml, want string
	}{
		"ok":        {"kind: characterize\ntrace:\n  AC-1: [a.sh]\n", ""},
		"with task": {"kind: characterize\ntask:\n  goal: x\n  paths: [src/]\ntrace:\n  AC-1: [a.sh]\n", "has no task"},
		"unknown":   {"kind: implement\ntask:\n  goal: x\n  paths: [src/]\ntrace:\n  AC-1: [a.sh]\n", "unknown"},
		"no task":   {"trace:\n  AC-1: [a.sh]\n", "task.goal"},
	} {
		_, err := parsePlanFile([]byte(tc.yaml))
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		o    Outcome
		want string
	}{
		"ok":         {Outcome{ID: "X", Title: "x", Kind: PlanCharacterize, Acceptance: []string{"t/"}}, ""},
		"tasks":      {Outcome{ID: "X", Title: "x", Kind: PlanCharacterize, Acceptance: []string{"t/"}, Tasks: []Task{{Goal: "g"}}}, "has no tasks"},
		"no tests":   {Outcome{ID: "X", Title: "x", Kind: PlanCharacterize}, "acceptance"},
		"other kind": {Outcome{ID: "X", Title: "x", Kind: "spike"}, "must be empty or characterize"},
	} {
		err := tc.o.normalize()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && len(tc.o.Tasks) != 0 {
			t.Errorf("%s: normalize invented a task", name)
		}
	}
	// A characterize outcome written by hand is refused by aeh run as well.
	f := newPlanFixture(t, authorOK, reviewOK)
	write(t, f.dir, "aeh/outcomes/CH-1.yaml", "id: CH-1\ntitle: x\nkind: characterize\nacceptance: [tests/x/]\n")
	if _, err := RunOutcome(context.Background(), f.repo, "CH-1", nil); err == nil || !strings.Contains(err.Error(), "nothing to implement") {
		t.Fatalf("run: %v", err)
	}
	if err := (Implementation{Outcome: "X", Approval: strings.Repeat("a", 64), Code: obsSHA, ObservedBase: "abc", MaxCriterion: 1,
		Criteria: []ImplementedCriterion{{ID: 1, Text: "x", Tests: []string{"t/a"}}}}).validate(); err == nil || !strings.Contains(err.Error(), "observed_base_sha") {
		t.Fatalf("observed base must be a full SHA: %v", err)
	}
}
