package aeh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// describeSpec replaces the placeholder text of a new spec and commits it, as the owner would.
func describeSpec(t *testing.T, r Repo, feature, text string) {
	t.Helper()
	dir := memDir(t, r)
	p := filepath.Join(dir, "wiki", "features", feature, "spec.md")
	spec := strings.Replace(readFile(t, p), specPlaceholder, text, 1)
	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "describe "+feature)
}

func TestApproveSpec(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("an undescribed spec must not be approved: %v", err)
	}
	if _, _, err := ApproveSpec(r, "nope"); err == nil {
		t.Fatal("unknown feature must fail")
	}
	describeSpec(t, r, "checkout", "The buyer pays the cart total once.")
	s, changed, err := ApproveSpec(r, "checkout")
	if err != nil || !changed || !s.Approved() || s.Approval.Version != "0.1.0" || s.Approval.Date != "2026-11-13" {
		t.Fatalf("approve: %+v changed=%v err=%v", s.Approval, changed, err)
	}
	dir := memDir(t, r)
	if err := memoryClean(dir); err != nil {
		t.Fatalf("approval must be committed: %v", err)
	}
	if subj := mustGit(t, dir, "log", "-1", "--format=%s"); subj != "approve checkout 0.1.0" {
		t.Errorf("commit subject %q", subj)
	}
	if log := readFile(t, filepath.Join(dir, "wiki", "log.md")); !strings.Contains(log, "## [2026-11-13] approve | checkout 0.1.0") {
		t.Errorf("log:\n%s", log)
	}
	if _, changed, err := ApproveSpec(r, "checkout"); err != nil || changed {
		t.Errorf("second approve must be a no-op: changed=%v err=%v", changed, err)
	}
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	if spec, _ := snap.Spec("checkout"); !spec.Approved() {
		t.Fatal("approval not loaded")
	}

	// Recording an accepted change bumps the version and adds a changelog line: still approved.
	p := PendingRecord{OutcomeID: "OUT-1", Feature: "checkout", Change: "minor", Note: "Pay once.", Commit: fakeSHA('b')}
	if _, _, err := RecordChange(r, p, snap.Fingerprint); err != nil {
		t.Fatal(err)
	}
	snap, _ = LoadMemory(r)
	if spec, _ := snap.Spec("checkout"); spec.Version != "0.2.0" || !spec.Approved() {
		t.Fatalf("record must keep the approval: %s approved=%v", spec.Version, spec.Approved())
	}

	// Editing the text, the title or the paths voids it.
	for name, edit := range map[string][2]string{
		"text":  {"The buyer pays the cart total once.", "The buyer pays twice."},
		"title": {"title: Checkout", "title: Checkout v2"},
		"paths": {"paths: [src/checkout/**]", "paths: [src/checkout/**, src/pay/**]"},
	} {
		sp := filepath.Join(dir, "wiki", "features", "checkout", "spec.md")
		orig := readFile(t, sp)
		if !strings.Contains(orig, edit[0]) {
			t.Fatalf("%s: %q not in spec:\n%s", name, edit[0], orig)
		}
		if err := os.WriteFile(sp, []byte(strings.Replace(orig, edit[0], edit[1], 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		snap, err := LoadMemory(r)
		if err != nil {
			t.Fatal(err)
		}
		if spec, _ := snap.Spec("checkout"); spec.Approved() || spec.Approval == nil {
			t.Errorf("%s edit must void the approval but keep the record of it", name)
		}
		if err := os.WriteFile(sp, []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Approving needs committed edits.
	sp := filepath.Join(dir, "wiki", "features", "checkout", "spec.md")
	if err := os.WriteFile(sp, []byte(strings.Replace(readFile(t, sp), "pays the cart total once", "pays the total", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "commit your spec edits") {
		t.Errorf("dirty memory: %v", err)
	}
}

func TestApprovalChangesTheContract(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", "Pay once.")
	before, _ := LoadMemory(r)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	after, _ := LoadMemory(r)
	if before.ContractFingerprint("checkout") == after.ContractFingerprint("checkout") {
		t.Error("approving the declared feature must change the contract")
	}
	if before.ContractFingerprint("") != after.ContractFingerprint("") {
		t.Error("another feature's approval must not change an undeclared contract")
	}
	if before.Fingerprint != after.Fingerprint {
		t.Error("approval must not change the drift fingerprint")
	}
}

func TestInvalidApprovalFileIsRejected(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(memDir(t, r), "wiki", "features", "checkout", approvalFile)
	for _, bad := range []string{
		"version: 0.1.0\ncontent_sha256: abc\ndate: x\n",
		"version: one\ncontent_sha256: " + strings.Repeat("a", 64) + "\n",
		"version: 0.1.0\ncontent_sha256: " + strings.Repeat("a", 64) + "\napproved_by: me\n",
	} {
		if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMemory(r); err == nil || !strings.Contains(err.Error(), approvalFile) {
			t.Errorf("approval %q must be rejected: %v", bad, err)
		}
	}
}

func TestSpecTextStopsAtChangelog(t *testing.T) {
	body := "# X\n\nText.\n\n```\n## Changelog\n```\n\n## Changelog\n- 0.1.0 created\n"
	if got := specText(body); got != "# X\n\nText.\n\n```\n## Changelog\n```\n\n" {
		t.Errorf("specText = %q", got)
	}
}
