package aeh

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func queueProject(t *testing.T) Repo {
	t.Helper()
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	return r
}

func registerQueuePlan(t *testing.T, r Repo, id, feature string) PlanState {
	t.Helper()
	p := PlanState{Version: planStateVersion, ID: id, Feature: feature, Phase: PlanReserved, Status: StatusBlocked, SpecVersion: "0.1.0"}
	if err := savePlanState(r, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func approveQueuedCheckout(t *testing.T, r Repo, changedPaths bool) Spec {
	t.Helper()
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustSpec(t, snap, "checkout").Body, "then one order is created.") {
		editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	}
	if changedPaths {
		editSpec(t, r, "checkout", "src/checkout/**", "src/new/**")
	}
	raiseVersion(t, r, "checkout")
	s, changed, err := ApproveSpec(r, "checkout")
	if err != nil || !changed || s.Queued == nil {
		t.Fatalf("queue: %+v %t %v", s, changed, err)
	}
	return s
}

func requireQueuedError(t *testing.T, err error, feature string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "queued approval "+feature) {
		t.Fatalf("expected queue refusal for %s, got %v", feature, err)
	}
}

func TestQueueOneAndSeveralBlockers(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			r := queueProject(t)
			ids := []string{"OLD-1", "OLD-2", "OLD-3"}[:count]
			for _, id := range ids {
				registerQueuePlan(t, r, id, "checkout")
			}
			s := approveQueuedCheckout(t, r, false)
			if len(s.Queued.Blockers) != count {
				t.Fatalf("blockers: %+v", s.Queued)
			}
			st, err := ReadMemoryStatus(r)
			if err != nil || len(st.Queues) != 1 || len(st.Work) != count || st.Specs[0].Version != "0.1.0" || !st.Specs[0].Approved() {
				t.Fatalf("status: %+v %v", st, err)
			}
			_, _, err = ApproveSpec(r, "checkout")
			requireQueuedError(t, err, "checkout")
			for i, id := range ids {
				if err := Abandon(r, id); err != nil {
					t.Fatal(err)
				}
				st, err = ReadMemoryStatus(r)
				if err != nil {
					t.Fatal(err)
				}
				if i < count-1 && len(st.Queues) != 1 {
					t.Fatal("activated before the last blocker")
				}
			}
			if len(st.Queues) != 0 || st.Specs[0].Version != "0.1.1" || !st.Specs[0].Approved() {
				t.Fatalf("activation: %+v", st)
			}
			if len(st.Specs[0].Implemented) != 0 {
				t.Fatal("abandon counted as implementation")
			}
			if err := Answer(r, ids[0], "retry"); err == nil || !strings.Contains(err.Error(), "abandoned") {
				t.Fatalf("answer: %v", err)
			}
			if id, err := unfinishedPlan(r, "checkout"); err != nil || id != "" {
				t.Fatalf("unfinished abandoned plan: %s %v", id, err)
			}
			if err := Abandon(r, ids[0]); err != nil {
				t.Fatal(err)
			}
			log := mustGit(t, memDir(t, r), "log", "--format=%B")
			if strings.Count(log, "AEH-Abandon: "+ids[0]) != 1 {
				t.Fatalf("duplicate abandon marker: %s", log)
			}
			// Queue approvals are part of immutable approval history, including before
			// activation. The same version can never acquire different content.
			editSpec(t, r, "checkout", "exactly one order", "precisely one order")
			if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "already approved") {
				t.Fatalf("mutable queued version: %v", err)
			}
		})
	}
}

func TestQueuePathsBlockOtherWorkAndRecompute(t *testing.T) {
	r := queueProject(t)
	if _, err := CreateSpec(r, "shipping", "Shipping", []string{"src/ship/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "shipping", twoCriteria)
	registerQueuePlan(t, r, "OLD-1", "checkout")
	s := State{Version: 2, ID: "SHIP-1", Feature: "shipping", Paths: []string{"src/new/"}, Status: StatusBlocked}
	if err := saveState(r, &s); err != nil {
		t.Fatal(err)
	}
	q := approveQueuedCheckout(t, r, true).Queued
	if len(q.Blockers) != 2 {
		t.Fatalf("cross-feature blockers: %+v", q.Blockers)
	}
	// Another feature claiming the reserved paths is refused by the queue.
	editSpec(t, r, "shipping", "src/ship/**", "src/new/labels/**")
	_, _, err := ApproveSpec(r, "shipping")
	requireQueuedError(t, err, "checkout")
	for _, tc := range []struct {
		feature string
		paths   []string
	}{{"checkout", nil}, {"", []string{"src/new/a.go"}}, {"", []string{"src/checkout/old.go"}}} {
		requireQueuedError(t, admitWork(r, tc.feature, tc.paths, "NEW-1"), "checkout")
	}
	if err := admitWork(r, "shipping", []string{"src/new/"}, "SHIP-1"); err != nil {
		t.Fatalf("blocker resume: %v", err)
	}
	if err := Abandon(r, "OLD-1"); err != nil {
		t.Fatal(err)
	}
	// A blocker that appears after approval must be discovered before activation.
	registerQueuePlan(t, r, "LATE-1", "checkout")
	if err := Abandon(r, "SHIP-1"); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 1 || len(st.Queues[0].Blockers) != 1 || st.Queues[0].Blockers[0].ID != "LATE-1" {
		t.Fatalf("recomputed queue: %+v %v", st, err)
	}
	if err := Abandon(r, "LATE-1"); err != nil {
		t.Fatal(err)
	}
	st, err = ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || st.Specs[0].Paths[0] != "src/new/**" {
		t.Fatalf("path activation: %+v %v", st, err)
	}
}

func TestQueuePendingRecordsAndAcceptedWorkKept(t *testing.T) {
	r := queueProject(t)
	registerQueuePlan(t, r, "OLD-1", "checkout")
	s := State{Version: 2, ID: "ACCEPTED-1", Feature: "checkout", Status: StatusAccepted, MemoryError: "memory record is pending", Head: fakeSHA('a')}
	if err := saveState(r, &s); err != nil {
		t.Fatal(err)
	}
	rec := PendingRecord{OutcomeID: s.ID, Feature: "checkout", Change: "patch", Note: "verified change", Commit: s.Head}
	if err := SavePending(r, rec); err != nil {
		t.Fatal(err)
	}
	// Leave room for the old patch record below the queued target.
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.2.0")
	q := approveQueuedCheckout(t, r, false).Queued
	if len(q.Blockers) != 2 {
		t.Fatalf("pending blocker: %+v", q.Blockers)
	}
	err := Abandon(r, s.ID)
	if err == nil || !strings.Contains(err.Error(), "accepted work is kept") {
		t.Fatalf("abandon acceptance: %v", err)
	}
	if _, ok, err := LoadPending(r, s.ID); err != nil || ok {
		t.Fatalf("acceptance not recorded: %t %v", ok, err)
	}
	s, _ = loadState(r, s.ID)
	if s.Status != StatusAccepted || s.MemoryError != "" || s.MemoryCommit == "" {
		t.Fatalf("accepted state: %+v", s)
	}
	if err := Abandon(r, "OLD-1"); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || !strings.Contains(st.Specs[0].Body, "ACCEPTED-1 accepted") {
		t.Fatalf("record lost at activation: %+v %v", st, err)
	}
	if err := Abandon(r, s.ID); err == nil {
		t.Fatal("already recorded work abandoned")
	}
}

func TestQueueActivationAndAbandonRecoverOnRead(t *testing.T) {
	r := queueProject(t)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	approveQueuedCheckout(t, r, false)
	i := abandonIntent{ID: p.ID, Feature: p.Feature, Date: "2026-11-17"}
	if err := writePrivateJSON(abandonPath(r, p.ID), i); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the plan changed but before the run state and marker.
	p.Status = StatusAbandoned
	if err := savePlanState(r, &p); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || st.Specs[0].Version != "0.1.1" {
		t.Fatalf("intent recovery: %+v %v", st, err)
	}
	s, _ := loadState(r, p.ID)
	if s.Status != StatusAbandoned {
		t.Fatalf("run state not recovered: %+v", s)
	}
	if _, err := RunOutcome(context.Background(), r, p.ID, nil); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("abandoned run resumed: %v", err)
	}

	// Crash in a real activation, after its approval file changed but before commit.
	dir := memDir(t, r)
	registerQueuePlan(t, r, "OLD-2", "checkout")
	queued := approveQueuedCheckout(t, r, false).Queued
	if queued.CurrentVersion != "0.1.1" {
		t.Fatalf("version-only queue changed the version in force: %+v", queued)
	}
	originalWrite := memoryWriteFile
	t.Cleanup(func() { memoryWriteFile = originalWrite })
	memoryWriteFile = func(path string, b []byte) error {
		if err := originalWrite(path, b); err != nil {
			return err
		}
		if filepath.Base(path) == approvalFile {
			return errors.New("simulated crash in activation")
		}
		return nil
	}
	if err := Abandon(r, "OLD-2"); err != nil {
		t.Fatalf("activation failure broke completed abandon: %v", err)
	}
	memoryWriteFile = originalWrite
	journal, err := os.ReadFile(liveWritePath(r))
	if err != nil {
		t.Fatal(err)
	}
	st, err = ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || st.Specs[0].Version != "0.1.2" {
		t.Fatalf("activation recovery: %+v %v", st, err)
	}
	// Crash after commit but before journal removal must not duplicate the activation.
	write(t, r.Root, ".aeh/live-write.json", string(journal))
	if _, err := ReadMemoryStatus(r); err != nil {
		t.Fatal(err)
	}
	if log := mustGit(t, dir, "log", "--format=%B"); strings.Count(log, "AEH-Queue-Activate: checkout 0.1.2") != 1 {
		t.Fatalf("duplicate activation: %s", log)
	}
	if _, err := os.Stat(liveWritePath(r)); !os.IsNotExist(err) {
		t.Fatalf("journal remains: %v", err)
	}
	// Recheck even on crash recovery: a newly registered blocker cancels the partial
	// activation and leaves the previous approval in force until that work is reconciled.
	registerQueuePlan(t, r, "OLD-3", "checkout")
	approveQueuedCheckout(t, r, false)
	memoryWriteFile = func(path string, b []byte) error {
		if err := originalWrite(path, b); err != nil {
			return err
		}
		if filepath.Base(path) == approvalFile {
			return errors.New("crash")
		}
		return nil
	}
	if err := Abandon(r, "OLD-3"); err != nil {
		t.Fatalf("activation failure broke completed abandon: %v", err)
	}
	memoryWriteFile = originalWrite
	registerQueuePlan(t, r, "LATE-3", "checkout")
	st, err = ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 1 || st.Specs[0].Version != "0.1.2" || st.Queues[0].Blockers[0].ID != "LATE-3" {
		t.Fatalf("activation failed to recheck: %+v %v", st, err)
	}
	if err := Abandon(r, "LATE-3"); err != nil {
		t.Fatal(err)
	}
}

func TestQueueLoggedCharacterizationsAndRevisionsAreDone(t *testing.T) {
	r := queueProject(t)
	for _, id := range []string{"CHAR-1", "REV-1"} {
		p := registerQueuePlan(t, r, id, "checkout")
		p.Phase = PlanLogged
		if id == "CHAR-1" {
			p.Kind = PlanCharacterize
		} else {
			p.NoImplementation = true
		}
		if err := savePlanState(r, &p); err != nil {
			t.Fatal(err)
		}
	}
	raiseVersion(t, r, "checkout")
	if s, _, err := ApproveSpec(r, "checkout"); err != nil || s.Queued != nil {
		t.Fatalf("finished plans blocked approval: %+v %v", s, err)
	}
}

func TestQueueFirstApprovalAlsoWaitsForStoppedManualWork(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	s := State{Version: 2, ID: "OLD-1", Feature: "checkout", Paths: []string{"src/checkout/"}, Status: StatusBlocked}
	if err := saveState(r, &s); err != nil {
		t.Fatal(err)
	}
	sp, _, err := ApproveSpec(r, "checkout")
	if err != nil || sp.Queued == nil || !sp.Approved() || sp.Queued.CurrentVersion != "none" {
		t.Fatalf("first approval: %+v %v", sp, err)
	}
	if err := Abandon(r, s.ID); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || !st.Specs[0].Approved() {
		t.Fatalf("first activation: %+v %v", st, err)
	}
}

func TestAbandonLoggedLegacyCharacterizationIsKept(t *testing.T) {
	r := queueProject(t)
	p := registerQueuePlan(t, r, "LEGACY-1", "checkout")
	p.Kind, p.Phase, p.Status = PlanCharacterize, PlanLogged, PlanLogged
	if err := savePlanState(r, &p); err != nil {
		t.Fatal(err)
	}
	if err := Abandon(r, p.ID); err == nil || !strings.Contains(err.Error(), "accepted work is kept") {
		t.Fatalf("legacy acceptance: %v", err)
	}
	p, _, _ = LoadPlanState(r, p.ID)
	if p.Status == StatusAbandoned {
		t.Fatal("accepted legacy plan abandoned")
	}
}

func TestAbandonRunKeepsBranchAndCreatesMemoryMarker(t *testing.T) {
	f := newFixture(t, map[string]string{"impl": agentNoop}, []string{"impl"}, "")
	s, _ := f.run()
	if s.Status != StatusBlocked {
		t.Fatalf("run: %+v", s)
	}
	branch := mustGit(t, f.dir, "rev-parse", IntegrationBranch(s.ID))
	if err := Abandon(f.repo, s.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, f.dir, "rev-parse", IntegrationBranch(s.ID)); got != branch {
		t.Fatal("abandon moved run branch")
	}
	if _, err := os.Stat(f.repo.ReportPath(s.ID)); err != nil {
		t.Fatal("run report removed")
	}
	if err := Answer(f.repo, s.ID, "retry"); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("answer abandoned run: %v", err)
	}
	if _, _, found, err := findRecord(memDir(t, f.repo), "AEH-Abandon: "+s.ID); err != nil || !found {
		t.Fatalf("marker: %t %v", found, err)
	}
}

func TestAbandonPublishedRevisionCompletesItsRecordAndRefuses(t *testing.T) {
	f, _ := retirementFixture(t, false)
	editSpec(t, f.repo, "pay", "## Acceptance criteria\n- AC-1: Given a cart, when the user pays, then src/pay.txt says paid.\n", "")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, f.repo)
	before := mustGit(t, dir, "rev-parse", "HEAD")
	p := f.plan()
	if !p.NoImplementation || p.Phase != PlanLogged {
		t.Fatalf("revision: %+v", p)
	}
	// Reconstruct the crash boundary: published branch, acceptance record still missing.
	mustGit(t, dir, "reset", "--hard", before)
	p.Phase, p.Status = PlanPublished, PlanPublished
	if err := savePlanState(f.repo, &p); err != nil {
		t.Fatal(err)
	}
	err := Abandon(f.repo, p.ID)
	if err == nil || !strings.Contains(err.Error(), "accepted work is kept") {
		t.Fatalf("published revision abandoned: %v", err)
	}
	im := lastRecordedImplementation(t, f)
	if im.Outcome != p.ID || len(im.Retired) != 1 {
		t.Fatalf("revision not recorded: %+v", im)
	}
	p, _, _ = LoadPlanState(f.repo, p.ID)
	if p.Phase != PlanLogged || p.Status == StatusAbandoned {
		t.Fatalf("accepted revision state: %+v", p)
	}
}

func TestQueueOtherFeaturePromotionAdmission(t *testing.T) {
	r, _ := newAreaProject(t)
	// A feature that reaches into the area's paths (claimed before stage 5 checked areas) gives
	// them up: the queue reserves the old paths until the new version activates.
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**", "src/billing/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	registerQueuePlan(t, r, "OLD-1", "checkout")
	editSpec(t, r, "checkout", "src/checkout/**, src/billing/**", "src/checkout/**")
	raiseVersion(t, r, "checkout")
	if s, _, err := ApproveSpec(r, "checkout"); err != nil || s.Queued == nil {
		t.Fatalf("queue: %+v %v", s, err)
	}
	requireQueuedError(t, AdoptPromote(r, "billing"), "checkout")
}

func TestQueueDiscoveryPromotionAdmission(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.start()
	if st := f.run(); st.Phase != DiscoveryDraft {
		t.Fatalf("discovery: %+v", st)
	}
	if _, err := CreateSpec(f.repo, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, f.repo, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(f.repo, "checkout"); err != nil {
		t.Fatal(err)
	}
	registerQueuePlan(t, f.repo, "OLD-1", "checkout")
	editSpec(t, f.repo, "checkout", "src/checkout/**", "src/cart/**")
	raiseVersion(t, f.repo, "checkout")
	if s, _, err := ApproveSpec(f.repo, "checkout"); err != nil || s.Queued == nil {
		t.Fatalf("queue: %+v %v", s, err)
	}
	_, err := DiscoverPromote(f.repo, "shop", "cart")
	requireQueuedError(t, err, "checkout")
}

func TestQueuePromotionRecoveryCannotBypassAdmission(t *testing.T) {
	r := queueProject(t)
	registerQueuePlan(t, r, "OLD-1", "checkout")
	q := approveQueuedCheckout(t, r, true).Queued
	text := strings.Replace(q.Spec, "feature: checkout", "feature: shipping", 1)
	dir := memDir(t, r)
	p, err := newPendingImport(dir, map[string][]byte{specRel("shipping"): []byte(text)}, "promote shipping", "promote shipping")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockProject(r)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = finishImport(r, dir, "discover/shop", p, func() error { return nil })
	requireQueuedError(t, err, "checkout")
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(specRel("shipping")))); !os.IsNotExist(err) {
		t.Fatalf("recovery claimed queued paths: %v", err)
	}
}

func TestQueueAdmissionAtEveryWorkEntry(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	editSpec(t, f.repo, "pay", "Pays the cart.", "Pays in one step.")
	raiseVersion(t, f.repo, "pay")
	// The old run records a minor bump to 0.2.0; activation needs a distinct version.
	editSpec(t, f.repo, "pay", "version: 0.1.1", "version: 0.3.0")
	s, _, err := ApproveSpec(f.repo, "pay")
	if err != nil || s.Queued == nil {
		t.Fatalf("queue: %+v %v", s, err)
	}
	_, err = PlanFeature(context.Background(), f.repo, "pay", nil)
	requireQueuedError(t, err, "pay")
	_, err = AdoptSeal(context.Background(), f.repo, "pay", nil)
	requireQueuedError(t, err, "pay")
	// A registered manual outcome has to pass admission before preflight or agents.
	write(t, f.dir, "aeh/outcomes/NEW-1.yaml", "id: NEW-1\ntitle: New work\nlevel: 1\nfeature: pay\nchange: patch\nchangelog: new work\ntasks:\n  - id: T1\n    goal: change\n    paths: [src/]\n")
	_, err = RunOutcome(context.Background(), f.repo, "NEW-1", nil)
	requireQueuedError(t, err, "pay")
	_, err = Do(context.Background(), f.repo, "new work", []string{"src/"}, "", MemoryLink{Feature: "pay", Change: "patch", Note: "new work"}, nil, "")
	requireQueuedError(t, err, "pay")
	write(t, f.dir, "aeh/outcomes/NEW-2.yaml", "id: NEW-2\ntitle: Unlinked work\nlevel: 1\ntasks:\n  - id: T1\n    goal: change\n    paths: [src/]\n")
	_, err = RunOutcome(context.Background(), f.repo, "NEW-2", nil)
	requireQueuedError(t, err, "pay")
	_, err = Do(context.Background(), f.repo, "unlinked work", []string{"src/"}, "", MemoryLink{}, nil, "")
	requireQueuedError(t, err, "pay")
	_, err = Do(context.Background(), f.repo, "unrestricted work", nil, "", MemoryLink{}, nil, "")
	requireQueuedError(t, err, "pay")
	// Remove only the test's new contract so the real pinned run can check a clean base.
	if err := os.Remove(f.repo.OutcomePath("NEW-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.repo.OutcomePath("NEW-2")); err != nil {
		t.Fatal(err)
	}
	st := generatedRunState(t, f, p.ID)
	if st.Status != StatusAccepted || st.MemoryError != "" {
		t.Fatalf("registered blocker did not finish: %+v", st)
	}
	status, err := ReadMemoryStatus(f.repo)
	if err != nil || len(status.Queues) != 0 {
		t.Fatalf("finish did not activate: %+v %v", status, err)
	}
	im := status.Specs[0].Implemented[0]
	if im.SpecVersion != "0.1.0" {
		t.Fatalf("recorded against new version: %+v", im)
	}
	if !status.Specs[0].staleApproval() {
		t.Fatal("new approval delta was silently rebound after old implementation")
	}
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatalf("reapprove actual delta: %v", err)
	}
}

func TestQueueBlockerPlanResumesAndAbandonedBranchSurvives(t *testing.T) {
	f := newPlanFixture(t, `echo "PLAN: TOO-LARGE"`, reviewOK)
	p := f.plan()
	editSpec(t, f.repo, "pay", "Pays the cart.", "Pays in one step.")
	raiseVersion(t, f.repo, "pay")
	if s, _, err := ApproveSpec(f.repo, "pay"); err != nil || s.Queued == nil {
		t.Fatalf("queue: %+v %v", s, err)
	}
	if err := Answer(f.repo, p.ID, "finish the old plan"); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", authorOK)
	p = f.plan()
	if p.Phase != PlanLogged || p.SpecVersion != "0.1.0" {
		t.Fatalf("plan resume: %+v", p)
	}
	branch := mustGit(t, f.dir, "rev-parse", IntegrationBranch(p.ID))
	if err := Abandon(f.repo, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, f.dir, "rev-parse", IntegrationBranch(p.ID)); got != branch {
		t.Fatal("abandon moved the branch")
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "runs")); err != nil {
		t.Fatal("attempt evidence removed")
	}
	if _, err := RunOutcome(context.Background(), f.repo, p.ID, nil); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("resume abandoned: %v", err)
	}
	var intent abandonIntent
	b, err := os.ReadFile(abandonPath(f.repo, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &intent); err != nil || !intent.Done {
		t.Fatalf("intent: %+v %v", intent, err)
	}
}
