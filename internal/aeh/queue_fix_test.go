package aeh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finishQueuePlan(t *testing.T, r Repo, p PlanState) {
	t.Helper()
	p.Kind, p.Phase = PlanCharacterize, PlanLogged
	if err := savePlanState(r, &p); err != nil {
		t.Fatal(err)
	}
}

func requireActivationWaiting(t *testing.T, r Repo, reason string) MemoryStatus {
	t.Helper()
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 1 || !strings.Contains(st.Queues[0].ActivationWaiting, reason) {
		t.Fatalf("activation waiting for %q: %+v, %v", reason, st, err)
	}
	if err := admitWork(r, "checkout", nil, "NEW-1"); err == nil {
		t.Fatal("waiting activation released admission control")
	}
	return st
}

func TestQueueDirtyMemoryWaitsAndActivatesAtNextCommand(t *testing.T) {
	r := queueProject(t)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	approveQueuedCheckout(t, r, false)
	finishQueuePlan(t, r, p)
	dir := memDir(t, r)
	write(t, dir, "wiki/owner-draft.md", "Owner's unfinished draft.\n")
	st := requireActivationWaiting(t, r, "uncommitted changes")
	if st.Dirty == "" || st.Specs[0].Version != "0.1.0" {
		t.Fatalf("status lost dirty state or activated: %+v", st)
	}
	if err := RecoverLiveWork(r); err != nil {
		t.Fatal(err)
	}
	errs, warnings, err := LintMemory(r)
	if err != nil || len(errs) != 0 || !strings.Contains(strings.Join(warnings, "\n"), "activation waiting:") {
		t.Fatalf("lint: %v %v %v", errs, warnings, err)
	}
	// Clearing the unrelated draft permits activation on the very next command.
	if err := os.Remove(filepath.Join(dir, "wiki", "owner-draft.md")); err != nil {
		t.Fatal(err)
	}
	st, err = ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || st.Specs[0].Version != "0.1.1" {
		t.Fatalf("activation after clean: %+v %v", st, err)
	}
}

func TestQueueEditedVersionWaitsWithoutBlockingUnrelatedApproval(t *testing.T) {
	r := queueProject(t)
	if _, err := CreateSpec(r, "shipping", "Shipping", []string{"src/shipping/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "shipping", twoCriteria)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	approveQueuedCheckout(t, r, false)
	finishQueuePlan(t, r, p)
	dir := memDir(t, r)
	before := readFile(t, filepath.Join(dir, filepath.FromSlash(specRel("checkout"))))
	editSpec(t, r, "checkout", "then one order is created.", "then an owner's future draft is used.")
	st := requireActivationWaiting(t, r, "version in force was edited")
	for _, action := range []string{"save your draft", "restore wiki/features/checkout/spec.md", "commit the restoration", queueRel("checkout")} {
		if !strings.Contains(st.Queues[0].ActivationWaiting, action) {
			t.Fatalf("non-actionable wait: %s", st.Queues[0].ActivationWaiting)
		}
	}
	if _, changed, err := ApproveSpec(r, "shipping"); err != nil || !changed {
		t.Fatalf("unrelated approval blocked: %t %v", changed, err)
	}
	if got := readFile(t, filepath.Join(dir, filepath.FromSlash(specRel("checkout")))); !strings.Contains(got, "owner's future draft") {
		t.Fatal("owner edit was overwritten")
	}
	write(t, dir, specRel("checkout"), before)
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "restore version in force")
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 {
		t.Fatalf("restoration did not unblock activation: %+v %v", st, err)
	}
}

func TestQueueVersionOnlyOwnerEditWaits(t *testing.T) {
	r := queueProject(t)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	approveQueuedCheckout(t, r, false)
	finishQueuePlan(t, r, p)
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.9.0")
	st := requireActivationWaiting(t, r, "version in force was edited")
	if st.Specs[0].Version != "0.9.0" {
		t.Fatal("activation overwrote the owner's version-only edit")
	}
}

func TestMemoryReadOnlyReportsQueueAndJournalErrors(t *testing.T) {
	for _, kind := range []string{"queue", "invalid journal", "moved HEAD"} {
		t.Run(kind, func(t *testing.T) {
			r := queueProject(t)
			dir := memDir(t, r)
			want := "approval queue"
			switch kind {
			case "queue":
				write(t, dir, queueRel("checkout"), "{broken\n")
			case "invalid journal":
				write(t, r.Root, ".aeh/live-write.json", "{}\n")
				want = "invalid memory transaction"
			case "moved HEAD":
				head := mustGit(t, dir, "rev-parse", "HEAD")
				log := readFile(t, filepath.Join(dir, "wiki", "log.md"))
				tx := liveWrite{Head: head, Subject: "refresh queue checkout", Marker: "AEH-Queue-Refresh: interrupted", Before: map[string]*string{"wiki/log.md": &log}, After: map[string]string{"wiki/log.md": log + "\nIntended change.\n"}}
				if err := writePrivateJSON(liveWritePath(r), tx); err != nil {
					t.Fatal(err)
				}
				mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-qm", "owner moved memory HEAD")
				want = "HEAD changed"
			}
			st, err := ReadMemoryStatus(r)
			if err != nil || len(st.Specs) != 1 || !strings.Contains(strings.Join(st.Errors, "\n"), want) {
				t.Fatalf("read status: %+v %v", st, err)
			}
			if kind == "moved HEAD" {
				for _, action := range []string{".aeh/live-write.json", "committed or no longer wanted", "remove"} {
					if !strings.Contains(strings.Join(st.Errors, "\n"), action) {
						t.Fatalf("missing recovery action %q: %v", action, st.Errors)
					}
				}
			}
			if err := RecoverLiveWork(r); err != nil {
				t.Fatalf("read-only project status failed: %v", err)
			}
			errs, _, err := LintMemory(r)
			if err != nil || !strings.Contains(strings.Join(errs, "\n"), want) {
				t.Fatalf("lint diagnostics: %v %v", errs, err)
			}
			if _, _, err := ApproveSpec(r, "checkout"); err == nil {
				t.Fatal("mutation trusted broken queue/journal")
			}
			if kind == "moved HEAD" {
				if err := os.Remove(liveWritePath(r)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := ApproveSpec(r, "checkout"); err != nil {
					t.Fatalf("documented recovery did not work: %v", err)
				}
			}
		})
	}
}

func TestAbandonIntentCancelsAfterAcceptedPendingRecord(t *testing.T) {
	r := queueProject(t)
	s := State{Version: 2, ID: "ACCEPTED-1", Feature: "checkout", Status: StatusAccepted, MemoryError: "memory record is pending", Head: fakeSHA('a')}
	if err := saveState(r, &s); err != nil {
		t.Fatal(err)
	}
	if err := SavePending(r, PendingRecord{OutcomeID: s.ID, Feature: s.Feature, Change: "patch", Note: "accepted change", Commit: s.Head}); err != nil {
		t.Fatal(err)
	}
	// Use a higher target so this tests cancellation and activation, not collision.
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.2.0")
	approveQueuedCheckout(t, r, false)
	if err := writePrivateJSON(abandonPath(r, s.ID), abandonIntent{ID: s.ID, Feature: s.Feature, Date: "2026-11-18"}); err != nil {
		t.Fatal(err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || len(st.Errors) != 0 {
		t.Fatalf("recovery: %+v %v", st, err)
	}
	var intent abandonIntent
	if err := json.Unmarshal([]byte(readFile(t, abandonPath(r, s.ID))), &intent); err != nil || !intent.Cancelled || intent.Done || !strings.Contains(intent.Reason, "accepted work is kept") {
		t.Fatalf("cancelled intent: %+v %v", intent, err)
	}
	if _, pending, err := LoadPending(r, s.ID); err != nil || pending {
		t.Fatalf("pending record not completed: %t %v", pending, err)
	}
	s, _ = loadState(r, s.ID)
	if s.Status != StatusAccepted || s.MemoryError != "" || s.MemoryCommit == "" {
		t.Fatalf("acceptance lost: %+v", s)
	}
	if err := Abandon(r, s.ID); err == nil || !strings.Contains(err.Error(), "accepted work is kept") {
		t.Fatalf("abandon did not report acceptance: %v", err)
	}
	if _, err := ReadMemoryStatus(r); err != nil {
		t.Fatal(err)
	}
	if log := mustGit(t, memDir(t, r), "log", "--format=%B"); strings.Count(log, "AEH-Record: "+s.ID+" ") != 1 || strings.Contains(log, "AEH-Abandon: "+s.ID) {
		t.Fatalf("duplicate record or abandoned acceptance: %s", log)
	}
}

func TestQueueVersionCollisionWaitsAndAllowsOnlyHigherSameTarget(t *testing.T) {
	for _, change := range []string{"patch", "minor"} {
		t.Run(change, func(t *testing.T) {
			r := queueProject(t)
			p := registerQueuePlan(t, r, "OLD-1", "checkout")
			q := approveQueuedCheckout(t, r, false).Queued
			v, _, err := RecordChange(r, PendingRecord{OutcomeID: p.ID, Feature: "checkout", Change: change, Note: "old content accepted", Commit: fakeSHA('a')}, "")
			if err != nil {
				t.Fatal(err)
			}
			finishQueuePlan(t, r, p)
			st := requireActivationWaiting(t, r, "recorded version in force "+v+" is >= queued version")
			if !strings.Contains(st.Queues[0].ActivationWaiting, "aeh memory approve checkout") || strings.Contains(st.Specs[0].Body, "exactly one order") {
				t.Fatal("collision overwrote content or supplied no remedy")
			}
			dir := memDir(t, r)
			next, _ := bumpVersion(v, "patch")
			text, _ := setSpecVersion(q.Spec, next)
			// Changing target content is never part of this narrow exception.
			write(t, dir, specRel("checkout"), strings.Replace(text, "exactly one order", "a different order", 1))
			mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "different target is forbidden")
			if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "same queued content") {
				t.Fatalf("collision allowed different target: %v", err)
			}
			// Reusing the colliding version is refused even for the exact target.
			low, _ := setSpecVersion(q.Spec, v)
			write(t, dir, specRel("checkout"), low)
			mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "colliding version is forbidden")
			if _, _, err := ApproveSpec(r, "checkout"); err == nil {
				t.Fatal("collision allowed reused version")
			}
			write(t, dir, specRel("checkout"), text)
			mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "raise queued target only")
			if _, changed, err := ApproveSpec(r, "checkout"); err != nil || !changed {
				t.Fatalf("collision remedy failed: %t %v", changed, err)
			}
			st, err = ReadMemoryStatus(r)
			if err != nil || len(st.Queues) != 0 || st.Specs[0].Version != next || !strings.Contains(st.Specs[0].Body, "exactly one order") || !strings.Contains(st.Specs[0].Body, "- "+v+" (") {
				t.Fatalf("replacement lost target or acceptance: %+v %v", st, err)
			}
		})
	}
}

func TestGeneratedAcceptedRunWritesReportAndNotifiesBeforeQueueReconciliation(t *testing.T) {
	for _, breakQueue := range []bool{false, true} {
		t.Run(map[bool]string{false: "activation waits", true: "queue corrupt after acceptance"}[breakQueue], func(t *testing.T) {
			f, p := generatedFixture(t, implementPay)
			notified := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var e Event
				if err := json.NewDecoder(req.Body).Decode(&e); err != nil {
					t.Error(err)
				}
				if e.Event == "accepted" {
					b, err := os.ReadFile(f.repo.ReportPath(p.ID))
					notified <- err == nil && strings.Contains(string(b), "Status: **accepted**")
					if breakQueue {
						if err := os.WriteFile(filepath.Join(memDir(t, f.repo), filepath.FromSlash(queueRel("pay"))), []byte("{broken\n"), 0o644); err != nil {
							t.Error(err)
						}
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			write(t, f.dir, "aeh/config.yaml", readFile(t, f.repo.ConfigPath())+"notify:\n  webhook: "+yamlQuote(server.URL)+"\n")
			f.commitAll("configure acceptance notification")
			editSpec(t, f.repo, "pay", "Pays the cart.", "Pays in one step.")
			raiseVersion(t, f.repo, "pay")
			if s, _, err := ApproveSpec(f.repo, "pay"); err != nil || s.Queued == nil {
				t.Fatalf("queue: %+v %v", s, err)
			}
			var log strings.Builder
			s, err := RunOutcome(context.Background(), f.repo, p.ID, &log)
			if err != nil || s.Status != StatusAccepted || s.MemoryError != "" {
				t.Fatalf("accepted run failed: %+v %v\n%s", s, err, log.String())
			}
			select {
			case reportReady := <-notified:
				if !reportReady {
					t.Fatal("acceptance notification preceded accepted evidence report")
				}
			default:
				t.Fatal("accepted notification missing")
			}
			if breakQueue {
				if !strings.Contains(log.String(), "warning: reconcile queues after acceptance:") {
					t.Fatalf("post-acceptance queue error not warned: %s", log.String())
				}
			} else {
				st, err := ReadMemoryStatus(f.repo)
				if err != nil || len(st.Queues) != 1 || !strings.Contains(st.Queues[0].ActivationWaiting, "recorded version in force") {
					t.Fatalf("collision not retained after accepted run: %+v %v", st, err)
				}
			}
		})
	}
}

type queuePlanLog struct {
	strings.Builder
	onLogged func()
}

func (l *queuePlanLog) Write(b []byte) (int, error) {
	n, err := l.Builder.Write(b)
	if strings.Contains(string(b), " planned on ") && l.onLogged != nil {
		l.onLogged()
		l.onLogged = nil
	}
	return n, err
}

func TestPlanLoggedBeforeQueueReconciliationWarning(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	if _, err := CreateSpec(f.repo, "checkout", "Checkout", []string{"lib/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, f.repo, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(f.repo, "checkout"); err != nil {
		t.Fatal(err)
	}
	p := registerQueuePlan(t, f.repo, "OLD-1", "checkout")
	approveQueuedCheckout(t, f.repo, false)
	finishQueuePlan(t, f.repo, p)
	// An unrelated committed owner draft prevents activation before and after planning.
	editSpec(t, f.repo, "checkout", "then one order is created.", "then the owner draft applies.")
	dir := memDir(t, f.repo)
	log := &queuePlanLog{onLogged: func() {
		// A queue becomes untrustworthy after the plan's success log: it must be a
		// warning of a completed plan, rather than changing that plan's outcome.
		write(t, dir, queueRel("checkout"), "{broken\n")
	}}
	st, err := PlanFeature(context.Background(), f.repo, "pay", log)
	if err != nil || st.Phase != PlanLogged || !strings.Contains(log.String(), "planned on") || !strings.Contains(log.String(), "warning: reconcile queues after logged plan:") {
		t.Fatalf("logged plan failed: %+v %v\n%s", st, err, log.String())
	}
	stored, found, err := LoadPlanState(f.repo, st.ID)
	if err != nil || !found || stored.Phase != PlanLogged {
		t.Fatalf("logged state lost: %+v %t %v", stored, found, err)
	}
}

func TestQueueInterruptedActivationDoesNotUseStaleSnapshotForAnotherQueue(t *testing.T) {
	r := queueProject(t)
	if _, err := CreateSpec(r, "shipping", "Shipping", []string{"src/shipping/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "shipping", twoCriteria)
	if _, _, err := ApproveSpec(r, "shipping"); err != nil {
		t.Fatal(err)
	}
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	other := registerQueuePlan(t, r, "SHIP-1", "shipping")
	approveQueuedCheckout(t, r, false)
	raiseVersion(t, r, "shipping")
	if s, _, err := ApproveSpec(r, "shipping"); err != nil || s.Queued == nil {
		t.Fatalf("second queue: %+v %v", s, err)
	}
	finishQueuePlan(t, r, p)
	finishQueuePlan(t, r, other)
	original := memoryWriteFile
	t.Cleanup(func() { memoryWriteFile = original })
	failed := false
	memoryWriteFile = func(path string, b []byte) error {
		if err := original(path, b); err != nil {
			return err
		}
		if filepath.Base(path) == approvalFile && !failed {
			failed = true
			return os.ErrPermission
		}
		return nil
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 2 || !strings.Contains(st.Queues[1].ActivationWaiting, "pending memory transaction") {
		t.Fatalf("another queue used interrupted bytes: %+v %v", st, err)
	}
	memoryWriteFile = original
	st, err = ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 {
		t.Fatalf("queues did not recover: %+v %v", st, err)
	}
	for _, s := range st.Specs {
		if s.Version != "0.1.1" || !s.Approved() {
			t.Fatalf("activation lost: %+v", s)
		}
	}
	dir := memDir(t, r)
	log := readFile(t, filepath.Join(dir, "wiki", "log.md"))
	if strings.Count(log, "activate approval | checkout") != 1 || strings.Count(log, "activate approval | shipping") != 1 {
		t.Fatalf("activation log overwritten or duplicated: %s", log)
	}
	if index := readFile(t, filepath.Join(dir, "wiki", "index.md")); index != renderIndex(st.Specs) {
		t.Fatal("activation used a stale index")
	}
}

func TestQueueFirstApprovalCollisionCanRaiseTarget(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	s, _, err := ApproveSpec(r, "checkout")
	if err != nil || s.Queued == nil || !s.Queued.NoCurrentApproval {
		t.Fatalf("first queued approval: %+v %v", s, err)
	}
	v, _, err := RecordChange(r, PendingRecord{OutcomeID: p.ID, Feature: p.Feature, Change: "patch", Note: "manual work", Commit: fakeSHA('a')}, "")
	if err != nil {
		t.Fatal(err)
	}
	finishQueuePlan(t, r, p)
	requireActivationWaiting(t, r, "recorded version in force")
	next, _ := bumpVersion(v, "patch")
	text, _ := setSpecVersion(s.Queued.Spec, next)
	dir := memDir(t, r)
	write(t, dir, specRel("checkout"), text)
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "raise first queued approval")
	if _, changed, err := ApproveSpec(r, "checkout"); err != nil || !changed {
		t.Fatalf("first approval collision remedy: %t %v", changed, err)
	}
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || !st.Specs[0].Approved() || st.Specs[0].Version != next || !strings.Contains(st.Specs[0].Body, "OLD-1 accepted") {
		t.Fatalf("first approval record lost: %+v %v", st, err)
	}
}
