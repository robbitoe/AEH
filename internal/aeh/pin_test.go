package aeh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// raiseVersion raises the patch number of a spec's version in its front matter and commits it,
// as the owner does before approving a changed spec.
func raiseVersion(t *testing.T, r Repo, feature string) string {
	t.Helper()
	dir := memDir(t, r)
	p := filepath.Join(dir, filepath.FromSlash(specRel(feature)))
	text := readFile(t, p)
	spec, err := parseSpec(text)
	if err != nil {
		t.Fatal(err)
	}
	next, err := bumpVersion(spec.Version, "patch")
	if err != nil {
		t.Fatal(err)
	}
	if text, err = setSpecVersion(text, next); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "raise "+feature+" to "+next)
	return next
}

func TestApprovedVersionIsImmutable(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	spec, changed, err := ApproveSpec(r, "checkout")
	if err != nil || !changed || spec.Approval.DeltaBase != noImplementation {
		t.Fatalf("first approval: %+v %v %v", spec.Approval, changed, err)
	}
	editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	_, _, err = ApproveSpec(r, "checkout")
	if err == nil || !strings.Contains(err.Error(), "already approved") || !strings.Contains(err.Error(), "version: 0.1.1 or higher") {
		t.Fatalf("new content approved under an approved version: %v", err)
	}
	if v := raiseVersion(t, r, "checkout"); v != "0.1.1" {
		t.Fatal(v)
	}
	if spec, changed, err = ApproveSpec(r, "checkout"); err != nil || !changed || spec.Approval.Version != "0.1.1" {
		t.Fatalf("raised version: %+v %v %v", spec.Approval, changed, err)
	}
	// Lowering only the version keeps the spec reading as approved (the digest leaves the
	// version out), but it is not the newest approval: approve and plan refuse it.
	dir := memDir(t, r)
	editSpec(t, r, "checkout", "version: 0.1.1", "version: 0.1.0")
	if _, _, err = ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "highest approved: 0.1.1") {
		t.Fatalf("a lowered version passed as approved: %v", err)
	}
	// Restoring the old approved pair is not the newest approval either.
	sp := filepath.Join(dir, filepath.FromSlash(specRel("checkout")))
	ap := filepath.Join(dir, filepath.FromSlash(approvalRel("checkout")))
	first := mustGit(t, dir, "log", "--reverse", "--format=%H", "--", approvalRel("checkout"))
	first = strings.Fields(first)[0]
	for p, rel := range map[string]string{sp: specRel("checkout"), ap: approvalRel("checkout")} {
		b, _ := gitBlob(dir, first, rel)
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "restore the first approved pair")
	if snap, _ := LoadMemory(r); !mustSpec(t, snap, "checkout").Approved() {
		t.Fatal("setup: the restored pair should read as approved")
	}
	if _, _, err = ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "highest approved: 0.1.1") {
		t.Fatalf("a restored old pair passed as approved: %v", err)
	}
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.1.1")
	editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	if _, _, err = ApproveSpec(r, "checkout"); err != nil {
		t.Fatalf("the newest approved version and content again: %v", err)
	}
	// New content under an older version number is not raising it either. (The approval
	// digest covers title, paths and body, so the version alone never voids an approval.)
	editSpec(t, r, "checkout", "version: 0.1.1", "version: 0.1.0")
	editSpec(t, r, "checkout", "exactly one order", "precisely one order")
	if _, _, err = ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "highest approved: 0.1.1") {
		t.Fatalf("lower version approved: %v", err)
	}
}

func TestLoadMemoryAtReadsThePinnedSpec(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	pin := mustGit(t, dir, "rev-parse", "HEAD")
	// Remove AC-2 and approve a newer version: AC-2 is retired from now on, but the pinned
	// spec still has it and its criteria still check against history up to the pin.
	editSpec(t, r, "checkout", "- AC-2: Given a failed payment, when retried, then no second charge.\n", "")
	raiseVersion(t, r, "checkout")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	snap, err := loadMemoryAt(dir, pin)
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := snap.Spec("checkout")
	if !ok || snap.Commit != pin || spec.Version != "0.1.0" || !spec.Approved() || !strings.Contains(spec.Body, "AC-2") {
		t.Fatalf("pinned spec: %+v", spec)
	}
	if crit, _, err := checkSpecCriteria(dir, spec); err != nil || len(crit) != 2 {
		t.Fatalf("pinned criteria: %v %v", crit, err)
	}
	if now, _ := LoadMemory(r); now.Fingerprint == snap.Fingerprint {
		t.Fatal("the pinned snapshot reads the current memory")
	}
	if _, err := loadMemoryAt(dir, strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "no longer in memory's history") {
		t.Fatalf("unknown pin accepted: %v", err)
	}
	if _, err := loadMemoryAt(dir, "HEAD"); err == nil {
		t.Fatal("a symbolic pin was accepted")
	}
}

// A plan reads the memory it was reserved at: an owner edit after the reservation does not
// stop it, and the edit never reaches it.
func TestPlanResumesFromItsPinnedMemory(t *testing.T) {
	f := newPlanFixture(t, `echo "PLAN: TOO-LARGE"`, reviewOK)
	st := f.plan()
	if st.Status != StatusBlocked || !isFullSHA(st.MemoryCommit) {
		t.Fatalf("setup: %+v", st)
	}
	// The owner approves newer content, which needs a higher version: aeh plan still
	// continues the unfinished plan of 0.1.0 first, from its pin.
	editSpec(t, f.repo, "pay", "Pays the cart.", "Pays the cart in one step.")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	if err := Answer(f.repo, planID, "Retry."); err != nil {
		t.Fatal(err)
	}
	f.setScript("author", authorOK)
	st = f.plan()
	if st.ID != planID || st.Phase != PlanLogged || st.RestartRequired || st.SpecVersion != "0.1.0" {
		t.Fatalf("pinned plan did not finish: %+v", st)
	}
	// Tampering with the pin is refused: another commit in history holds another version.
	path, _ := planStatePath(f.repo, planID)
	raw := readFile(t, path)
	head := mustGit(t, memDir(t, f.repo), "rev-parse", "HEAD")
	write(t, filepath.Dir(path), filepath.Base(path), strings.Replace(raw, st.MemoryCommit, head, 1))
	if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "does not hold pay 0.1.0 as planned") {
		t.Fatalf("a run accepted a pin with another version: %v", err)
	}
}

// A merge that keeps the old files must not hide an approval made on its other parent.
func TestApprovalHistoryFollowsMergedBranches(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	trunk := mustGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	mustGit(t, dir, "checkout", "-qb", "side")
	editSpec(t, r, "checkout", "then one order is created.", "then exactly one order is created.")
	editSpec(t, r, "checkout", "version: 0.1.0", "version: 0.2.0")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "checkout", "-q", trunk)
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "-s", "ours", "--no-edit", "side")
	editSpec(t, r, "checkout", "then one order is created.", "then precisely one order is created.")
	raiseVersion(t, r, "checkout")
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "highest approved: 0.2.0") {
		t.Fatalf("a merge hid the approval of 0.2.0: %v", err)
	}
}

// A pinned snapshot rejects what loadMemoryDir rejects on disk instead of reading it as absent.
func TestLoadMemoryAtRejectsNonRegularFiles(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	write(t, dir, implementedRel("checkout")+"/x", "not a file\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "a directory named implemented.yaml")
	if _, err := loadMemoryAt(dir, mustGit(t, dir, "rev-parse", "HEAD")); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("a directory was read as no implementations: %v", err)
	}
}

func mustSpec(t *testing.T, snap MemorySnapshot, feature string) Spec {
	t.Helper()
	s, ok := snap.Spec(feature)
	if !ok {
		t.Fatalf("no feature %s", feature)
	}
	return s
}

func TestGeneratedRunFinishesItsPinnedVersion(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	if !isFullSHA(p.MemoryCommit) {
		t.Fatalf("plan not pinned: %+v", p)
	}
	// The owner approves a newer version with a second criterion before the run.
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.\n", "then src/pay.txt says paid.\n- AC-2: Given a paid cart, when paid again, then nothing changes.\n")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted || st.MemoryError != "" || st.MemoryVersion != "0.2.0" {
		t.Fatalf("pinned run: %+v", st)
	}
	dir := memDir(t, f.repo)
	im, err := readImplementations(filepath.Join(dir, filepath.FromSlash(implementedRel("pay"))))
	if err != nil || len(im) != 1 || im[0].SpecVersion != "0.1.0" || im[0].Approval != p.SpecDigest || len(im[0].Criteria) != 1 {
		t.Fatalf("implementation of the pinned version: %+v %v", im, err)
	}
	// The newer approval was computed before that implementation: it must be approved again
	// before planning, so its delta is the real one.
	snap, _ := LoadMemory(f.repo)
	spec, _ := snap.Spec("pay")
	if !spec.Approved() || !spec.staleApproval() {
		t.Fatalf("newer approval should be stale: %+v", spec.Approval)
	}
	mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(planID))
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil || !strings.Contains(err.Error(), "approve it again") {
		t.Fatalf("stale approval planned: %v", err)
	}
	spec, changed, err := ApproveSpec(f.repo, "pay")
	if err != nil || !changed || spec.Approval.DeltaBase != p.SpecDigest || spec.staleApproval() {
		t.Fatalf("re-approval: %+v %v %v", spec.Approval, changed, err)
	}
	if _, changed, err := ApproveSpec(f.repo, "pay"); err != nil || changed {
		t.Fatalf("a current approval was written again: %v %v", changed, err)
	}
	f.setScript("author", `exit 3`)
	next, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "pay-v0_2_0" || len(next.Delta.New) != 1 || next.Delta.New[0].ID != 2 || len(next.Delta.Unchanged) != 1 {
		t.Fatalf("next plan delta: %+v", next)
	}
}

// A plan reserved before stage 5 has no pin: its run reads memory as it is, as before.
func TestGeneratedRunWithoutPinReadsCurrentMemory(t *testing.T) {
	f, _ := generatedFixture(t, implementPay)
	path, err := planStatePath(f.repo, planID)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "memory_commit")
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	editSpec(t, f.repo, "pay", "Pays the cart.", "Updated payment behavior.")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	st := generatedRunState(t, f, planID)
	if st.Status != StatusBlocked || !st.RestartRequired || st.Question != "spec changed since plan; approve it and run aeh plan again" || f.count("impl") != 0 {
		t.Fatalf("unpinned run ignored the spec change: %+v", st)
	}
}
