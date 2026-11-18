package aeh

import (
	"context"
	"strings"
	"testing"
)

func TestApproveClarifications(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	spec, _, err := ApproveSpec(r, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	recordImplementation(t, r, spec, "checkout-v0_1_0", fakeSHA('a'))
	editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	if _, _, err := ApproveSpecWith(r, "checkout", ApproveOptions{Clarify: []int{2}, SetClarify: true}); err == nil || !strings.Contains(err.Error(), "AC-2 is not a changed criterion") {
		t.Fatalf("an unchanged criterion was clarified: %v", err)
	}
	if _, _, err := ApproveSpecWith(r, "checkout", ApproveOptions{Clarify: []int{1, 1}, SetClarify: true}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a repeated clarification was accepted: %v", err)
	}
	spec, changed, err := ApproveSpecWith(r, "checkout", ApproveOptions{Clarify: []int{1}, SetClarify: true})
	if err != nil || !changed || len(spec.Approval.Clarifications) != 1 || spec.Approval.Clarifications[0].ID != 1 {
		t.Fatalf("clarification: %+v %v %v", spec.Approval, changed, err)
	}
	crit, _, _ := spec.Criteria()
	if spec.Approval.Clarifications[0].New != criterionDigest(crit[0].Text) {
		t.Fatal("the clarification is not bound to the new text")
	}
	// The same marks, or no flag at all, keep a current approval as it is.
	if _, changed, err := ApproveSpecWith(r, "checkout", ApproveOptions{Clarify: []int{1}, SetClarify: true}); err != nil || changed {
		t.Fatalf("same clarifications rewrote the approval: %v %v", changed, err)
	}
	if spec, changed, err := ApproveSpec(r, "checkout"); err != nil || changed || len(spec.Approval.Clarifications) != 1 {
		t.Fatalf("approve without the flag dropped the clarification: %v %v", changed, err)
	}
	if spec, changed, err := ApproveSpecWith(r, "checkout", ApproveOptions{SetClarify: true}); err != nil || !changed || len(spec.Approval.Clarifications) != 0 {
		t.Fatalf("--clarify none: %+v %v %v", spec.Approval, changed, err)
	}
	// A clarification approved for one change does not apply to another change of the text.
	d := criteriaDelta([]SpecCriterion{{ID: 1, Text: "Something else."}}, &Implementation{Criteria: []ImplementedCriterion{{ID: 1, Text: "Old."}}})
	a := &SpecApproval{Clarifications: []Clarification{{ID: 1, Old: criterionDigest("Old."), New: criterionDigest("New.")}}}
	if err := applyClarifications(&d, a); err == nil || !strings.Contains(err.Error(), "approve again") {
		t.Fatalf("a stale clarification applied: %v", err)
	}
}

func TestParseEquivalence(t *testing.T) {
	for _, tc := range []struct {
		out, err string
		no       []string
	}{
		{out: "fine\nEQUIVALENT AC-1: yes\nEQUIVALENT AC-3: yes\nVERDICT: APPROVE"},
		{out: "EQUIVALENT AC-1: yes\nEQUIVALENT AC-3: no\nVERDICT: APPROVE", no: []string{"AC-3"}},
		{out: "EQUIVALENT AC-1: yes\nVERDICT: APPROVE", err: "AC-3 has no equivalence line"},
		{out: "EQUIVALENT AC-1: yes\nEQUIVALENT AC-1: yes\nEQUIVALENT AC-3: yes", err: "more than one"},
		{out: "EQUIVALENT AC-1: yes\nEQUIVALENT AC-2: yes\nEQUIVALENT AC-3: yes", err: "AC-2 is not a clarification"},
		{out: "EQUIVALENT AC-1: maybe\nEQUIVALENT AC-3: yes", err: "malformed"},
		{out: "Equivalent AC-1: yes\nEQUIVALENT AC-3: yes", err: "malformed"},
	} {
		no, err := parseEquivalence(tc.out, []int{1, 3})
		switch {
		case tc.err == "" && err != nil, tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Fatalf("%q: error %v, want %q", tc.out, err, tc.err)
		case tc.err == "" && strings.Join(no, ",") != strings.Join(tc.no, ","):
			t.Fatalf("%q: no %v, want %v", tc.out, no, tc.no)
		}
	}
}

// A clarified change has nothing to implement: its green replacement test, QA's equivalence
// and the retirement of the old test are planned, published and recorded without a run.
func TestPlanClarificationWithoutImplementation(t *testing.T) {
	f, old := retirementFixture(t, false)
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.", "then the file src/pay.txt says paid.")
	spec, _, err := ApproveSpecWith(f.repo, "pay", ApproveOptions{Clarify: []int{1}, SetClarify: true})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := generatedOutcomeID("pay", spec.Version)
	d := "tests/acceptance/" + id
	f.setScript("author", `d=`+d+`; mkdir -p $d
printf 'grep -q paid src/pay.txt\n' > $d/clarified.sh
printf 'trace:\n  AC-1: [clarified.sh]\n' > $d/plan.yaml
echo 'PLAN: READY'`)
	// Without the equivalence line QA's approval is invalid.
	f.setScript("qa", countCall+reviewOK)
	st, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err != nil || st.Status != StatusBlocked || !st.NoImplementation || !strings.Contains(st.Attempts[len(st.Attempts)-1].Feedback, "AC-1 has no equivalence line") {
		t.Fatalf("missing equivalence accepted: %+v %v", st, err)
	}
	if err := Answer(f.repo, id, "Retry."); err != nil {
		t.Fatal(err)
	}
	f.setScript("qa", countCall+`echo "EQUIVALENT AC-1: yes"; echo "VERDICT: APPROVE"`)
	p := f.plan()
	if p.Phase != PlanLogged || !p.NoImplementation || len(p.Retired) != 1 || p.Retired[0].Path != "tests/acceptance/"+old.ID+"/pay.sh" {
		t.Fatalf("clarification plan: %+v", p)
	}
	green := false
	for _, c := range p.Checks {
		green = green || c.Name == "green "+d+"/clarified.sh" && c.Passed
	}
	if !green {
		t.Fatalf("the clarified test was not checked green: %+v", p.Checks)
	}
	im := lastRecordedImplementation(t, f)
	if im.Outcome != id || im.Code != p.PlanCommit || len(im.Criteria) != 1 || im.Criteria[0].Tests[0] != d+"/clarified.sh" || len(im.Retired) != 1 {
		t.Fatalf("recorded revision: %+v", im)
	}
	if _, err := RunOutcome(context.Background(), f.repo, id, nil); err == nil || !strings.Contains(err.Error(), "nothing to implement") {
		t.Fatalf("a plan without implementation ran: %v", err)
	}
}

// Removing every criterion needs no test author: the runner retires the tests, and the
// feature keeps its retired numbers with no criteria left.
func TestPlanRemovingAllCriteriaNeedsNoAuthor(t *testing.T) {
	f, old := retirementFixture(t, false)
	editSpec(t, f.repo, "pay", "## Acceptance criteria\n- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.\n", "")
	spec, _, err := ApproveSpec(f.repo, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Approval.MaxCriterion != 1 || len(spec.Approval.Criteria) != 0 {
		t.Fatalf("approval without criteria: %+v", spec.Approval)
	}
	authored := f.count("author")
	p := f.plan()
	if p.Phase != PlanLogged || !p.NoImplementation || f.count("author") != authored || len(p.Retired) != 1 || p.Retired[0].Reason != "removed" {
		t.Fatalf("authorless removal: %+v", p)
	}
	if out := mustGit(t, f.dir, "ls-tree", "-r", "--name-only", p.PlanCommit, "tests/acceptance/"+old.ID+"/"); strings.Contains(out, "pay.sh") {
		t.Fatalf("the retired test is still in the plan commit: %s", out)
	}
	im := lastRecordedImplementation(t, f)
	if len(im.Criteria) != 0 || len(im.Retired) != 1 || im.MaxCriterion != 1 {
		t.Fatalf("recorded removal: %+v", im)
	}
	// AC-1 stays retired.
	editSpec(t, f.repo, "pay", "Pays the cart.\n", "Pays the cart.\n\n## Acceptance criteria\n- AC-1: Back again.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("a retired number came back: %v", err)
	}
}

// Removing a criterion from a shared file needs green recovery of the criterion it shared
// the file with, and still nothing to implement.
func TestPlanRemovalWithRecoveryHasNoTask(t *testing.T) {
	f, _ := retirementFixture(t, true)
	editSpec(t, f.repo, "pay", "- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.\n", "")
	spec, _, err := ApproveSpec(f.repo, "pay")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := generatedOutcomeID("pay", spec.Version)
	d := "tests/acceptance/" + id
	f.setScript("author", `d=`+d+`; mkdir -p $d
printf 'grep -q stable src/stable.txt\n' > $d/coverage.sh
printf 'task:\n  goal: nothing\n  paths: [src/]\ntrace:\n  AC-2: [coverage.sh]\n' > $d/plan.yaml
echo 'PLAN: READY'`)
	st, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err != nil || st.Status != StatusBlocked || !strings.Contains(st.Attempts[0].Feedback, "remove task") {
		t.Fatalf("a task was accepted for a delta with nothing to implement: %+v %v", st, err)
	}
	if err := Answer(f.repo, id, "Retry."); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", `d=`+d+`; mkdir -p $d
printf 'grep -q stable src/stable.txt\n' > $d/coverage.sh
printf 'trace:\n  AC-2: [coverage.sh]\n' > $d/plan.yaml
echo 'PLAN: READY'`)
	p := f.plan()
	if p.Phase != PlanLogged || !p.NoImplementation || len(p.Delta.Recovery) != 1 {
		t.Fatalf("removal with recovery: %+v", p)
	}
	im := lastRecordedImplementation(t, f)
	if len(im.Criteria) != 1 || im.Criteria[0].ID != 2 || im.Criteria[0].Tests[len(im.Criteria[0].Tests)-1] != d+"/coverage.sh" {
		t.Fatalf("recorded recovery: %+v", im)
	}
}

// The task and the trace are checked against the delta: a delta with something to implement
// needs a task and a trace of every new criterion; one with nothing to implement has no task.
func TestPlanCandidateTaskFollowsTheDelta(t *testing.T) {
	in := samplePlanInput()
	noTask, _ := parsePlanFile([]byte("trace:\n  AC-1: [pay.test.js]\n"))
	if err := validatePlanCandidate(in, sampleCandidate(in), noTask); err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Fatalf("a plan without task was accepted for new criteria: %v", err)
	}
	empty, _ := parsePlanFile([]byte("task:\n  goal: Create an order\n  paths: [src/checkout/]\ntrace: {}\n"))
	if err := validatePlanCandidate(in, sampleCandidate(in), empty); err == nil {
		t.Fatal("an empty trace was accepted for new criteria")
	}
	in.NoImplementation = true
	withTask, _ := parsePlanFile([]byte(samplePlan))
	if err := validatePlanCandidate(in, sampleCandidate(in), withTask); err == nil || !strings.Contains(err.Error(), "remove task") {
		t.Fatalf("a task was accepted for a delta with nothing to implement: %v", err)
	}
}
