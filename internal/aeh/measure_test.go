package aeh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The final check compares checked-out sealed files with their Git objects byte for
// byte, tolerating only CRLF line endings for LF blobs.
func TestGateSealedCheckout(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "core.autocrlf", "false")
	write(t, dir, "tests/acc/a.test.js", "ok\nok\n")
	write(t, dir, "tests/acc/b.test.js", "raw\r\nkeep\r\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "seal")
	head := mustGit(t, dir, "rev-parse", "HEAD")
	paths := []string{"tests/acc"}
	check := func(name string, pass bool, retired []string, prepare func()) {
		t.Helper()
		write(t, dir, "tests/acc/a.test.js", "ok\nok\n")
		write(t, dir, "tests/acc/b.test.js", "raw\r\nkeep\r\n")
		_ = os.Remove(filepath.Join(dir, "tests", "acc", "extra.test.js"))
		prepare()
		if g := gateSealedCheckout(dir, dir, head, retired, paths); g.Passed != pass {
			t.Fatalf("%s: passed=%t: %s", name, g.Passed, g.Output)
		}
	}
	check("identical", true, nil, func() {})
	check("crlf conversion", true, nil, func() { write(t, dir, "tests/acc/a.test.js", "ok\r\nok\r\n") })
	check("substituted", false, nil, func() { write(t, dir, "tests/acc/a.test.js", "pass\n") })
	check("crlf blob changed", false, nil, func() { write(t, dir, "tests/acc/b.test.js", "raw\nkeep\n") })
	check("extra file", false, nil, func() { write(t, dir, "tests/acc/extra.test.js", "x\n") })
	check("retired present", false, []string{"tests/acc/a.test.js"}, func() {})
}

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
