package aeh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A checkout that converts line endings (core.autocrlf, eol attributes) changes the
// sealed files on disk but not their Git objects. The final check of a generated run
// must accept it: found by the stage 5 measurement with Git for Windows defaults.
func TestGeneratedRunFinalCheckWithLineEndingConversion(t *testing.T) {
	f, _ := generatedFixture(t, implementPay)
	info := filepath.Join(f.dir, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "attributes"), []byte("*.yaml text eol=crlf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted {
		t.Fatalf("status %s: %s", st.Status, st.Question)
	}
}

// An owner who queues the next minor version while work is pending must not need the
// collision recovery: the pending record takes a patch version below the target.
// Found by the stage 5 measurement.
func TestQueuedNextMinorActivatesAfterRecord(t *testing.T) {
	r := queueProject(t)
	p := registerQueuePlan(t, r, "OLD-1", "checkout")
	editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.2.0")
	s, changed, err := ApproveSpec(r, "checkout")
	if err != nil || !changed || s.Queued == nil {
		t.Fatalf("queue: %+v %t %v", s, changed, err)
	}
	v, _, err := RecordChange(r, PendingRecord{OutcomeID: p.ID, Feature: "checkout", Change: "minor", Note: "old content accepted", Commit: fakeSHA('a')}, "")
	if err != nil || v != "0.1.1" {
		t.Fatalf("record version %q below queued 0.2.0: %v", v, err)
	}
	finishQueuePlan(t, r, p)
	st, err := ReadMemoryStatus(r)
	if err != nil || len(st.Queues) != 0 || st.Specs[0].Version != "0.2.0" || !st.Specs[0].Approved() ||
		!strings.Contains(st.Specs[0].Body, "exactly one order") || !strings.Contains(st.Specs[0].Body, "- 0.1.1 (") {
		t.Fatalf("activation: %+v %v", st, err)
	}
}

// Only a paused question resumes an aeh do; a do blocked by failures is refined with a
// new aeh do. Found in the L6 review.
func TestDoWithoutQuestionDoesNotResume(t *testing.T) {
	f := newFixture(t, map[string]string{"impl": "exit 0"}, []string{"impl"}, "")
	st, err := Do(context.Background(), f.repo, "write fees", []string{"src/"}, "impl", MemoryLink{}, nil, RouteTrivial)
	if err != nil || st.Status != StatusBlocked || st.Cursor != nil {
		t.Fatalf("do block: %+v %v", st, err)
	}
	if err := Answer(f.repo, st.ID, "try again"); err != nil {
		t.Fatal(err)
	}
	if _, err := RunOutcome(context.Background(), f.repo, st.ID, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("do without a question resumed: %v", err)
	}
}
