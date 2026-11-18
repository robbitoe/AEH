package aeh

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func configureReplacementImplementer(t *testing.T, f *planFixture, script string) {
	t.Helper()
	f.setScript("impl", countCall+script)
	f.writeConfig("plan:\n  ladder: [author]\n")
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "ladder: [author]", "ladder: [impl]", 1)
	cfg += "  impl:\n    provider: implementer\n    command: [sh, " + yamlQuote(filepath.ToSlash(filepath.Join(f.tmp, "impl.sh"))) + ", '{{prompt}}']\n    env:\n      AEH_COUNTER: " + yamlQuote(filepath.ToSlash(filepath.Join(f.tmp, "impl.count"))) + "\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	f.commitAll("configure independent implementation")
}

func replacementAuthor(id string, shared bool) string {
	dir := "tests/acceptance/" + id
	trace := "  AC-1: [replacement.sh]\\n"
	extra := ""
	if shared {
		trace += "  AC-2: [coverage.sh]\\n"
		extra = "printf 'grep -q stable src/stable.txt\\n' > $d/coverage.sh\n"
	}
	return "d=" + dir + "; mkdir -p $d\nprintf 'grep -q PAID src/pay.txt\\n' > $d/replacement.sh\n" + extra +
		"printf 'task:\\n  goal: Change payment text\\n  paths: [src/]\\ntrace:\\n" + trace + "' > $d/plan.yaml\necho 'PLAN: READY'"
}

func retirementFixture(t *testing.T, shared bool) (*planFixture, PlanState) {
	t.Helper()
	if !shared {
		f, p := generatedFixture(t, implementPay)
		st := generatedRunState(t, f, p.ID)
		if st.Status != StatusAccepted || st.MemoryError != "" {
			t.Fatalf("initial implementation: %+v", st)
		}
		mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(p.ID))
		return f, p
	}
	f := newPlanFixture(t, authorOK, countCall+reviewOK)
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.\n", "then src/pay.txt says paid.\n- AC-2: The stable text remains stable.\n")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	id := "pay-v0_1_1"
	dir := "tests/acceptance/" + id
	f.setScript("author", `d=`+dir+`; mkdir -p $d
printf 'grep -q paid src/pay.txt\ngrep -q stable src/stable.txt\n' > $d/shared.sh
printf 'grep -q stable src/stable.txt\n' > $d/stable.sh
printf 'task:\n  goal: Implement payment and stable text\n  paths: [src/]\ntrace:\n  AC-1: [shared.sh]\n  AC-2: [shared.sh, stable.sh]\n' > $d/plan.yaml
echo 'PLAN: READY'`)
	configureReplacementImplementer(t, f, implementPay+`; echo stable > src/stable.txt`)
	p := f.plan()
	st := generatedRunState(t, f, p.ID)
	if st.Status != StatusAccepted || st.MemoryError != "" {
		t.Fatalf("initial shared implementation: %+v", st)
	}
	mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(p.ID))
	return f, p
}

func approveReplacement(t *testing.T, f *planFixture, shared bool) string {
	t.Helper()
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.", "then src/pay.txt says PAID.")
	spec, _, err := ApproveSpec(f.repo, "pay")
	if err != nil {
		t.Fatal(err)
	}
	id, err := generatedOutcomeID("pay", spec.Version)
	if err != nil {
		t.Fatal(err)
	}
	f.setScript("author", replacementAuthor(id, shared))
	f.setScript("impl", countCall+`echo PAID > src/pay.txt`)
	return id
}

func lastRecordedImplementation(t *testing.T, f *planFixture) Implementation {
	t.Helper()
	snap, err := LoadMemory(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	spec := mustSpec(t, snap, "pay")
	im, ok := spec.LastImplementation()
	if !ok {
		t.Fatal("no implementation")
	}
	return im
}

func TestPlanReplacesChangedCriterion(t *testing.T) {
	f, old := retirementFixture(t, false)
	id := approveReplacement(t, f, false)
	p := f.plan()
	oldPath := "tests/acceptance/" + old.ID + "/pay.sh"
	if p.Phase != PlanLogged || p.ID != id || len(p.Retired) != 1 || p.Retired[0].Path != oldPath || p.Retired[0].Reason != "changed" {
		t.Fatalf("replacement plan: %+v", p)
	}
	if _, ok := gitBlob(f.dir, p.PlanCommit, oldPath); ok {
		t.Fatal("runner did not retire the old test")
	}
	if len(p.Delta.Changes) != 1 || !strings.Contains(p.Delta.Changes[0].OldText, "paid") || !strings.Contains(p.Delta.Changes[0].NewText, "PAID") {
		t.Fatalf("texts: %+v", p.Delta)
	}
	if lastRecordedImplementation(t, f).Outcome != old.ID {
		t.Fatal("plan recorded implementation before acceptance")
	}
	st := generatedRunState(t, f, id)
	if st.Status != StatusAccepted || st.MemoryError != "" {
		t.Fatalf("replacement run: %+v", st)
	}
	im := lastRecordedImplementation(t, f)
	if len(im.Criteria) != 1 || !reflect.DeepEqual(im.Criteria[0].Tests, []string{"tests/acceptance/" + id + "/replacement.sh"}) || !reflect.DeepEqual(im.Retired, p.Retired) {
		t.Fatalf("replacement record: %+v", im)
	}
	for _, path := range []string{"aeh/seals/" + old.ID + ".json", "tests/acceptance/" + old.ID + "/plan.yaml"} {
		before, _ := gitBlob(f.dir, old.PlanCommit, path)
		after, _ := gitBlob(f.dir, p.PlanCommit, path)
		if before != after {
			t.Fatalf("historical file changed: %s", path)
		}
	}
}

func TestPlanRemovesCriterionAlongsideNew(t *testing.T) {
	f, old := retirementFixture(t, false)
	editSpec(t, f.repo, "pay", "- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.", "- AC-2: Given a cart, when the user pays, then src/pay.txt says PAID.")
	spec, _, err := ApproveSpec(f.repo, "pay")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := generatedOutcomeID("pay", spec.Version)
	f.setScript("author", strings.Replace(replacementAuthor(id, false), "AC-1:", "AC-2:", 1))
	f.setScript("impl", countCall+`echo PAID > src/pay.txt`)
	p := f.plan()
	if p.Phase != PlanLogged || len(p.Retired) != 1 || p.Retired[0].Reason != "removed" || !reflect.DeepEqual(p.Delta.Removed, []int{1}) {
		t.Fatalf("removed plan: %+v", p)
	}
	st := generatedRunState(t, f, id)
	im := lastRecordedImplementation(t, f)
	if st.Status != StatusAccepted || st.MemoryError != "" || len(im.Criteria) != 1 || im.Criteria[0].ID != 2 || im.MaxCriterion != 2 || im.Retired[0].Path != "tests/acceptance/"+old.ID+"/pay.sh" {
		t.Fatalf("removed record: %+v %+v", st, im)
	}
}

func TestPlanSharedFileClosurePreservesCoverage(t *testing.T) {
	f, old := retirementFixture(t, true)
	id := approveReplacement(t, f, true)
	p := f.plan()
	if p.Phase != PlanLogged || len(p.Delta.Recovery) != 1 || p.Delta.Recovery[0].ID != 2 || len(p.Retired) != 1 || p.Retired[0].Reason != "closure of a shared file" {
		t.Fatalf("closure: %+v", p)
	}
	green, red := false, false
	for _, c := range p.Checks {
		green = green || c.Name == "green tests/acceptance/"+id+"/coverage.sh" && c.Passed
		red = red || c.Name == "red tests/acceptance/"+id+"/replacement.sh" && c.Passed
	}
	if !green || !red {
		t.Fatalf("red/green evidence: %+v", p.Checks)
	}
	st := generatedRunState(t, f, id)
	im := lastRecordedImplementation(t, f)
	want := []string{"tests/acceptance/" + old.ID + "/stable.sh", "tests/acceptance/" + id + "/coverage.sh"}
	if st.Status != StatusAccepted || st.MemoryError != "" || len(im.Criteria) != 2 || !reflect.DeepEqual(im.Criteria[1].Tests, want) {
		t.Fatalf("unchanged coverage lost: %+v %+v", st, im)
	}
}

func TestPlanRejectsMixedRedGreenFile(t *testing.T) {
	in := samplePlanInput()
	in.Delta.Recovery = []ImplementedCriterion{{ID: 2, Text: "Existing behavior"}}
	plan, _ := parsePlanFile([]byte(samplePlan))
	plan.Trace["AC-2"] = plan.Trace["AC-1"]
	if err := validatePlanCandidate(in, sampleCandidate(in), plan); err == nil || !strings.Contains(err.Error(), "both red and green") {
		t.Fatalf("mixed file accepted: %v", err)
	}
}

func TestPlanCandidateRetirementScope(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status byte
		allow  bool
	}{
		{"tests/acceptance/old/old.sh", 'D', true},
		{"tests/acceptance/old/old.sh", 'M', false},
		{"tests/acceptance/old/old.sh", 'A', false},
		{"tests/acceptance/old/other.sh", 'D', false},
		{"tests/acceptance/old/other.sh", 'M', false},
		{"src/README", 'D', false},
		{"aeh/seals/old.json", 'D', false},
	} {
		t.Run(fmt.Sprintf("%c-%s", tc.status, tc.path), func(t *testing.T) {
			in := samplePlanInput()
			in.Retired = []RetiredTest{{Path: "tests/acceptance/old/old.sh"}}
			files := append(sampleCandidate(in), PlanCandidateFile{Path: tc.path, Mode: "100644", Status: tc.status})
			plan, _ := parsePlanFile([]byte(samplePlan))
			if err := validatePlanCandidate(in, files, plan); (err == nil) != tc.allow {
				t.Fatalf("allow=%v, got %v", tc.allow, err)
			}
		})
	}
}

func TestGeneratedRunRejectsRecreatedRetiredPath(t *testing.T) {
	f, old := retirementFixture(t, false)
	id := approveReplacement(t, f, false)
	p := f.plan()
	oldDir := "tests/acceptance/" + old.ID
	f.setScript("impl", `echo PAID > src/pay.txt; mkdir -p `+oldDir+`; echo 'exit 0' > `+oldDir+`/pay.sh`)
	st := generatedRunState(t, f, id)
	if st.Status != StatusBlocked || st.Head != p.PlanCommit {
		t.Fatalf("retired path recreation accepted: %+v", st)
	}
	for _, a := range st.Tasks[0].Attempts {
		if a.FailedGate != "hash" {
			t.Fatalf("wrong gate: %+v", a)
		}
		gates := readGateResults(f.dir, a.RunDir)
		if len(gates) == 0 || !strings.Contains(gates[0].Output, "must remain absent") {
			t.Fatalf("missing absence evidence: %+v", gates)
		}
	}
}

func TestPlanRejectsRetiredBaseHashMismatch(t *testing.T) {
	f, old := retirementFixture(t, false)
	write(t, f.dir, "tests/acceptance/"+old.ID+"/pay.sh", "exit 0\n")
	f.commitAll("tamper with sealed base test")
	approveReplacement(t, f, false)
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "base bytes differ from the sealed hash") {
		t.Fatalf("tampered base planned: %v", err)
	}
}

func TestPlanOnlyRemovedDeltaWaitsForL3(t *testing.T) {
	f, _ := retirementFixture(t, true)
	editSpec(t, f.repo, "pay", "- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.\n", "")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "L3") {
		t.Fatalf("only removed delta planned: %v", err)
	}
}

func TestPlanRetirementRecoveryKeepsPinnedInputs(t *testing.T) {
	f, _ := retirementFixture(t, true)
	id := approveReplacement(t, f, true)
	cfg, _ := f.repo.LoadConfig()
	snap, _ := LoadMemory(f.repo)
	spec := mustSpec(t, snap, "pay")
	head, _ := memoryHeadCommit(snap.Dir)
	p := &planner{repo: f.repo, cfg: cfg, pcfg: cfg.plan(), spec: spec, memDir: snap.Dir, owners: snap.ContractFingerprint("pay")}
	if err := p.loadPrompts(); err != nil {
		t.Fatal(err)
	}
	reserved, err := p.reserve(id, "", head)
	if err != nil {
		t.Fatal(err)
	}
	// Crash just after reservation. A newer approval must not change texts or closure.
	editSpec(t, f.repo, "pay", "says PAID.", "says refunded.")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	published := f.plan()
	if published.Phase != PlanLogged || published.ID != id || published.Inputs != reserved.Inputs || !reflect.DeepEqual(published.Retired, reserved.Retired) || !sameDelta(published.Delta, reserved.Delta) {
		t.Fatalf("reservation mixed versions: %+v %+v", reserved, published)
	}
	// Crash after publication, before the memory log. Branch publication and logging are
	// idempotent and continue from the same pin even though memory now has another version.
	mustGit(t, snap.Dir, "reset", "-q", "--hard", "HEAD~1")
	published.Phase, published.Status = PlanPublished, StatusRunning
	if err := savePlanState(f.repo, &published); err != nil {
		t.Fatal(err)
	}
	again := f.plan()
	if again.Phase != PlanLogged || again.PlanCommit != published.PlanCommit || again.Inputs != reserved.Inputs || !sameDelta(again.Delta, reserved.Delta) {
		t.Fatalf("publication mixed versions: %+v", again)
	}
}

func TestPlanReplacesAdoptedCharacterization(t *testing.T) {
	f, observed := newCharFixture(t, charAuthorOK)
	old, log, err := f.seal()
	if err != nil || old.Phase != PlanLogged {
		t.Fatalf("characterize: %v %+v %s", err, old, log)
	}
	mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(old.ID))
	configureReplacementImplementer(t, f, `echo 'PAID in EUR' > src/pay.txt`)
	id := approveReplacement(t, f, false)
	f.setScript("impl", `echo 'PAID in EUR' > src/pay.txt`)
	p := f.plan()
	if p.Phase != PlanLogged || len(p.Retired) != 1 || p.Retired[0].Path != planDir+"/pay.sh" {
		t.Fatalf("adopted replacement: %+v", p)
	}
	st := generatedRunState(t, f, id)
	im := lastRecordedImplementation(t, f)
	if st.Status != StatusAccepted || st.MemoryError != "" || im.ObservedBase != "" || im.MaxCriterion != 3 || len(im.Criteria) != 2 || im.Criteria[1].ID != 3 || im.Criteria[1].Tests[0] != planDir+"/currency.sh" {
		t.Fatalf("adopted provenance or coverage lost: %+v %+v", st, im)
	}
	snap, err := LoadMemory(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	history := mustSpec(t, snap, "pay").Implemented
	if len(history) != 2 || history[0].Outcome != old.ID || history[0].ObservedBase != observed {
		t.Fatalf("characterization provenance lost: %+v", history)
	}
}

func TestPlanDeltaJSONNames(t *testing.T) {
	st := PlanState{Delta: CriteriaDelta{
		Changes:  []CriterionChange{{ID: 1, OldText: "Old behavior", NewText: "New behavior"}},
		Recovery: []ImplementedCriterion{{ID: 2, Text: "Stable behavior"}},
	}}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(state["delta"], &delta); err != nil {
		t.Fatal(err)
	}
	if _, ok := delta["recovery"]; !ok || len(delta) != 6 {
		t.Fatalf("unexpected delta JSON fields: %s", state["delta"])
	}
	if got, want := string(delta["Changes"]), `[{"id":1,"old_text":"Old behavior","new_text":"New behavior"}]`; got != want {
		t.Fatalf("criterion change JSON: got %s, want %s", got, want)
	}
	var restored PlanState
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Delta, st.Delta) {
		t.Fatalf("delta changed after JSON round trip: %+v", restored.Delta)
	}
	st.Delta.Recovery = nil
	b, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"recovery"`) {
		t.Fatalf("empty recovery persisted: %s", b)
	}
}

func TestPlanPromptDeltaSections(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*PlanInput)
		want string
	}{
		{"first plan", func(*PlanInput) {}, ""},
		{"changed", func(in *PlanInput) {
			in.Delta.Changes = []CriterionChange{{ID: 2, OldText: "Old behavior", NewText: "New behavior"}}
		}, "\nChanged criteria (replacement tests must be red):\n- AC-2 old text: Old behavior\n  New text: New behavior\n"},
		{"retired", func(in *PlanInput) {
			in.Retired = []RetiredTest{{Path: "tests/old.sh", Reason: "changed"}}
		}, "\nRetired tests (the runner deletes these; never recreate their paths):\n- tests/old.sh (changed)\n"},
		{"removed", func(in *PlanInput) {
			in.Delta.Removed = []int{2}
		}, "\nRemoved criterion IDs:\n- AC-2\n"},
		{"recovery", func(in *PlanInput) {
			in.Delta.Unchanged = []ImplementedCriterion{{ID: 2, Text: "Stable behavior", Tests: []string{"tests/shared.sh"}}}
			in.Delta.Recovery = in.Delta.Unchanged
		}, "\nRecovery criteria (new tests must be green, in separate files from red tests):\n- AC-2: Stable behavior; previous tests: tests/shared.sh\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := samplePlanInput()
			tc.edit(&in)
			want := featureReferenceBlock(in.Spec, "\n\nNew criteria to cover:\n- AC-1: One order is created.\n"+tc.want+"\nUnchanged criteria (do not test again):\n")
			if got := newPlanPromptData(in).MemorySpec; got != want {
				t.Fatalf("prompt reference: got %q, want %q", got, want)
			}
		})
	}
}

func TestRetirementPromptsAndSealCompatibility(t *testing.T) {
	in := samplePlanInput()
	in.Delta = CriteriaDelta{Changes: []CriterionChange{{ID: 1, OldText: "Old behavior", NewText: "New behavior"}}, Recovery: []ImplementedCriterion{{ID: 2, Text: "Stable behavior"}}}
	in.Retired = []RetiredTest{{Path: "tests/old.sh", Reason: "changed"}}
	for _, name := range []string{"plan.md", "review-plan.md"} {
		b, _ := defaultPrompts.ReadFile("prompts/" + name)
		prompt, err := renderPrompt(name, string(b), newPlanPromptData(in))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Old behavior", "New behavior", "Stable behavior", "tests/old.sh", "recovery"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s missing %s", name, want)
			}
		}
	}
	var old Seal
	if err := json.Unmarshal([]byte(`{"outcome":"old","files":[]}`), &old); err != nil || len(old.Retired) != 0 {
		t.Fatalf("legacy seal: %+v %v", old, err)
	}
}

func TestSealedAbsenceWorktreeHashGate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "current.sh", "exit 0\n")
	seal := sealFromContents("test", map[string][]byte{"current.sh": []byte("exit 0\n")}, "", memoryNow(), []string{"retired.sh"})
	if g := gateHash(dir, seal, []string{"current.sh"}); !g.Passed {
		t.Fatalf("valid absence failed: %+v", g)
	}
	write(t, dir, "retired.sh", "exit 0\n")
	if g := gateHash(dir, seal, []string{"current.sh"}); g.Passed || !strings.Contains(g.Output, "must remain absent") {
		t.Fatalf("recreated path passed worktree hash: %+v", g)
	}
}

func TestRetirementCarriesHistoricalAbsences(t *testing.T) {
	root := t.TempDir()
	mustGit(t, root, "init", "-q")
	seal := sealFromContents("previous", map[string][]byte{"tests/current.sh": []byte("exit 0\n")}, "", memoryNow(), []string{"tests/retired.sh"})
	write(t, root, "tests/current.sh", "exit 0\n")
	if err := writeJSON(filepath.Join(root, "aeh", "seals", "previous.json"), seal); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "previous implementation")
	head := mustGit(t, root, "rev-parse", "HEAD")
	p := planner{repo: Repo{Root: root}, spec: Spec{Implemented: []Implementation{{Outcome: "previous", Code: head}}}}
	retired, absent, err := p.retirement(&PlanState{Base: head, Delta: CriteriaDelta{New: []SpecCriterion{{ID: 2, Text: "New behavior"}}}})
	if err != nil || len(retired) != 0 || !reflect.DeepEqual(absent, []string{"tests/retired.sh"}) {
		t.Fatalf("historical absence lost: %+v %+v %v", retired, absent, err)
	}
}

func TestPlanRetiredFilesApplyTriageRules(t *testing.T) {
	f := newPlanFixture(t, strings.ReplaceAll(authorOK, "pay.sh", "pay.sql"), countCall+reviewOK)
	configureReplacementImplementer(t, f, implementPay)
	old := f.plan()
	st := generatedRunState(t, f, old.ID)
	if st.Status != StatusAccepted || st.MemoryError != "" {
		t.Fatalf("initial implementation: %+v", st)
	}
	mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(old.ID))
	cfg := readFile(t, f.repo.ConfigPath()) + "triage:\n  rules:\n    - name: sql\n      paths: ['**/*.sql']\n      reviewers: [dba]\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	f.commitAll("review SQL deletions")
	approveReplacement(t, f, false)
	p := f.plan()
	if p.Phase != PlanLogged || reviewRolesOf(p.Reviews) != "qa,security,dba" {
		t.Fatalf("runner retirement skipped triage rules: %+v", p)
	}
}
