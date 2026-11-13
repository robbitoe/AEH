package aeh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newMemoryProject is a project that ignores .aeh/, with initialized memory.
func newMemoryProject(t *testing.T) Repo {
	t.Helper()
	dir, _ := newRepo(t)
	write(t, dir, ".gitignore", ".aeh/\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "ignore")
	r, err := OpenRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := InitMemory(r)
	if err != nil || !created {
		t.Fatalf("init memory: created=%v err=%v", created, err)
	}
	memoryNow = func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { memoryNow = time.Now })
	return r
}

func memDir(t *testing.T, r Repo) string {
	t.Helper()
	d, err := MemoryDir(r)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func fakeSHA(c byte) string { return strings.Repeat(string(c), 40) }

func TestInitMemoryIsPrivateAndLocal(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	if top := mustGit(t, dir, "rev-parse", "--show-toplevel"); !samePath(top, dir) {
		t.Fatalf("memory is not its own repository: %s", top)
	}
	if out := mustGit(t, r.Root, "status", "--porcelain", "--untracked-files=all"); out != "" {
		t.Fatalf("project sees memory files:\n%s", out)
	}
	if out := mustGit(t, dir, "remote"); out != "" {
		t.Fatalf("memory has remotes: %s", out)
	}
	if created, err := InitMemory(r); err != nil || created {
		t.Fatalf("second init: created=%v err=%v", created, err)
	}
	snap, err := LoadMemory(r)
	if err != nil || !snap.Present || len(snap.Specs) != 0 {
		t.Fatalf("snapshot %+v err %v", snap, err)
	}
}

func TestInitMemoryRequiresIgnoredAeh(t *testing.T) {
	dir, _ := newRepo(t)
	r, _ := OpenRepo(dir)
	if _, err := InitMemory(r); err == nil || !strings.Contains(err.Error(), "not ignored") {
		t.Fatalf("expected a privacy error, got %v", err)
	}
}

func TestMemoryWithRemoteIsRejected(t *testing.T) {
	r := newMemoryProject(t)
	mustGit(t, memDir(t, r), "remote", "add", "origin", "https://example.invalid/memory.git")
	if _, err := LoadMemory(r); err == nil || !strings.Contains(err.Error(), "remotes") {
		t.Fatalf("expected a remote error, got %v", err)
	}
}

func TestCreateAndRecordBumpsVersion(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	p := PendingRecord{OutcomeID: "OUT-007", Feature: "checkout", Change: "patch", Note: "Pay button reads \"Confirm payment\".", Commit: fakeSHA('a')}
	v, c, err := RecordChange(r, p, snap.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.1.1" || c == "" {
		t.Fatalf("version %s commit %s", v, c)
	}
	dir := memDir(t, r)
	spec := readFile(t, filepath.Join(dir, "wiki", "features", "checkout", "spec.md"))
	want := "- 0.1.1 (2026-11-13) OUT-007 accepted on aeh/OUT-007 @ aaaaaaa: Pay button reads \"Confirm payment\"."
	if !strings.Contains(spec, "version: 0.1.1") || !strings.Contains(spec, want) {
		t.Fatalf("spec not updated:\n%s", spec)
	}
	if idx := readFile(t, filepath.Join(dir, "wiki", "index.md")); !strings.Contains(idx, "| 0.1.1 | OUT-007 @ aaaaaaa (2026-11-13) |") {
		t.Fatalf("index not updated:\n%s", idx)
	}
	if log := readFile(t, filepath.Join(dir, "wiki", "log.md")); !strings.Contains(log, "## [2026-11-13] record | checkout 0.1.1 | OUT-007 @ aaaaaaa") {
		t.Fatalf("log not updated:\n%s", log)
	}
	if err := memoryClean(dir); err != nil {
		t.Fatal(err)
	}
	if n := mustGit(t, dir, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatalf("expected 3 memory commits (init, create, record), got %s", n)
	}
	if author := mustGit(t, dir, "log", "-1", "--format=%an <%ae>"); author != "aeh <aeh@localhost>" {
		t.Fatalf("memory commit author %q", author)
	}
}

// A crash after the memory commit and before the state is saved must not bump twice.
func TestRecordIsIdempotent(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	snap, _ := LoadMemory(r)
	p := PendingRecord{OutcomeID: "OUT-007", Feature: "checkout", Change: "minor", Note: "n", Commit: fakeSHA('b')}
	v1, c1, err := RecordChange(r, p, snap.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	v2, c2, err := RecordChange(r, p, snap.Fingerprint) // stale fingerprint: the marker wins
	if err != nil || v1 != v2 || c1 != c2 || v1 != "0.2.0" {
		t.Fatalf("first %s %s, second %s %s, err %v", v1, c1, v2, c2, err)
	}
	p.Commit = fakeSHA('c')
	if v3, _, err := RecordChange(r, p, ""); err != nil || v3 != "0.3.0" {
		t.Fatalf("a different commit is a new record: %s %v", v3, err)
	}
}

func TestRecordRejectsChangedMemory(t *testing.T) {
	r := newMemoryProject(t)
	for _, f := range []string{"checkout", "cart"} {
		if _, err := CreateSpec(r, f, strings.ToUpper(f[:1])+f[1:], []string{"src/" + f + "/**"}); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := LoadMemory(r)
	// The owner changes another feature's paths between the start of the run and the record.
	dir := memDir(t, r)
	cart := filepath.Join(dir, "wiki", "features", "cart", "spec.md")
	if err := os.WriteFile(cart, []byte(strings.Replace(readFile(t, cart), "src/cart/**", "src/basket/**", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "owner edit")
	p := PendingRecord{OutcomeID: "OUT-008", Feature: "checkout", Change: "patch", Note: "n", Commit: fakeSHA('d')}
	if _, _, err := RecordChange(r, p, snap.Fingerprint); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("expected ErrMemoryChanged, got %v", err)
	}
}

func TestRecordRefusesDirtyMemory(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	write(t, dir, "wiki/notes.md", "draft\n")
	before := readFile(t, filepath.Join(dir, "wiki", "features", "checkout", "spec.md"))
	p := PendingRecord{OutcomeID: "OUT-009", Feature: "checkout", Change: "patch", Note: "n", Commit: fakeSHA('e')}
	if _, _, err := RecordChange(r, p, ""); !errors.Is(err, ErrMemoryDirty) {
		t.Fatalf("expected ErrMemoryDirty, got %v", err)
	}
	if after := readFile(t, filepath.Join(dir, "wiki", "features", "checkout", "spec.md")); after != before {
		t.Fatal("a refused record changed the spec")
	}
}

func TestPendingRecordRetry(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	p := PendingRecord{OutcomeID: "OUT-010", Feature: "checkout", Change: "major", Note: "n", Commit: fakeSHA('f')}
	if err := SavePending(r, p); err != nil {
		t.Fatal(err)
	}
	if list, err := ListPending(r); err != nil || len(list) != 1 {
		t.Fatalf("pending %v %v", list, err)
	}
	if v, _, err := RecordPending(r, "OUT-010"); err != nil || v != "1.0.0" {
		t.Fatalf("record pending: %s %v", v, err)
	}
	if _, ok, _ := LoadPending(r, "OUT-010"); ok {
		t.Fatal("pending record was not cleared")
	}
	if _, _, err := RecordPending(r, "OUT-010"); err == nil {
		t.Fatal("expected no pending record")
	}
}

func TestParseSpecRejectsInvalidSpecs(t *testing.T) {
	good := "---\nfeature: checkout\ntitle: Checkout\nversion: 1.2.3\npaths: [src/checkout/**]\n---\n# Checkout\n\n## Changelog\n"
	if s, err := parseSpec(strings.ReplaceAll(good, "\n", "\r\n")); err != nil || s.Version != "1.2.3" {
		t.Fatalf("CRLF spec: %+v %v", s, err)
	}
	cases := map[string]string{
		"no frontmatter":     "# Checkout\n",
		"unknown field":      strings.Replace(good, "title:", "owner: x\ntitle:", 1),
		"bad semver":         strings.Replace(good, "1.2.3", "1.2", 1),
		"leading zero":       strings.Replace(good, "1.2.3", "1.02.3", 1),
		"bad slug":           strings.Replace(good, "feature: checkout", "feature: Check_out", 1),
		"reserved slug":      strings.Replace(good, "feature: checkout", "feature: con", 1),
		"no paths":           strings.Replace(good, "paths: [src/checkout/**]", "paths: []", 1),
		"whole repo":         strings.Replace(good, "src/checkout/**", "**", 1),
		"escapes repo":       strings.Replace(good, "src/checkout/**", "../other/**", 1),
		"owns aeh":           strings.Replace(good, "src/checkout/**", ".aeh/memory/**", 1),
		"no changelog":       strings.Replace(good, "## Changelog\n", "", 1),
		"changelog not last": good + "## Notes\n",
	}
	for name, text := range cases {
		if _, err := parseSpec(text); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSlugsAndVersions(t *testing.T) {
	for _, s := range []string{"con", "nul", "com1", "lpt9", "a--b", "-a", "A", "a_b", ""} {
		if checkSlug(s) == nil {
			t.Errorf("slug %q accepted", s)
		}
	}
	for _, s := range []string{"checkout", "com", "com10", "pay-2"} {
		if err := checkSlug(s); err != nil {
			t.Errorf("slug %q rejected: %v", s, err)
		}
	}
	for in, want := range map[string]string{"patch": "1.2.4", "minor": "1.3.0", "major": "2.0.0"} {
		if got, err := bumpVersion("1.2.3", in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := bumpVersion("1.2.3", "trivial"); err == nil {
		t.Error("unknown change accepted")
	}
}

func TestMemoryLinkValidate(t *testing.T) {
	ok := []MemoryLink{{}, {Feature: "checkout", Change: "patch", Note: "x"}}
	bad := []MemoryLink{{Change: "patch"}, {Feature: "checkout", Note: "x"}, {Feature: "checkout", Change: "patch"}, {Feature: "Bad", Change: "patch", Note: "x"}}
	for _, l := range ok {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	for _, l := range bad {
		if l.Validate() == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}

// Running from a linked worktree must use, and lock, the main checkout's memory.
func TestLinkedWorktreeSharesMemoryAndLock(t *testing.T) {
	r := newMemoryProject(t)
	wt := filepath.Join(t.TempDir(), "linked")
	mustGit(t, r.Root, "worktree", "add", "-q", "--detach", wt)
	lr, err := OpenRepo(wt)
	if err != nil {
		t.Fatal(err)
	}
	if got := memDir(t, lr); !samePath(got, memDir(t, r)) {
		t.Fatalf("linked worktree memory %s, main %s", got, memDir(t, r))
	}
	unlock, err := lockProject(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockProject(lr); err == nil || !strings.Contains(err.Error(), "project lock") {
		t.Fatalf("expected the lock to be held, got %v", err)
	}
	if _, err := CreateSpec(lr, "checkout", "Checkout", []string{"src/checkout/**"}); err == nil {
		t.Fatal("memory write ran while another command held the project lock")
	}
	unlock()
	again, err := lockProject(lr)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestMemoryRejectsLinks(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	target := t.TempDir()
	link := filepath.Join(dir, "wiki", "features", "evil")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlink: %v", err)
		}
		// Junctions need no privilege on Windows.
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("junction: %v %s", err, out)
		}
	}
	if _, err := LoadMemory(r); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("expected a link error, got %v", err)
	}
}

func TestLintFindsOverlapAndStaleIndex(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/shop/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSpec(r, "cart", "Cart", []string{"src/shop/cart/**"}); err != nil {
		t.Fatal(err)
	}
	errs, warnings, err := LintMemory(r)
	if err != nil || len(errs) != 0 {
		t.Fatalf("errors %v %v", errs, err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "overlapping") {
		t.Fatalf("expected an overlap warning, got %v", warnings)
	}
}

// A .git junction would redirect memory writes into another repository.
func TestMemoryRejectsGitDirLink(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	other, _ := newRepo(t)
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	link, target := filepath.Join(dir, ".git"), filepath.Join(other, ".git")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlink: %v", err)
		}
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("junction: %v %s", err, out)
		}
	}
	if _, err := LoadMemory(r); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("expected a link error, got %v", err)
	}
	if _, err := InitMemory(r); err == nil {
		t.Fatal("init accepted a linked .git")
	}
}

func TestRunWaitsForProjectLock(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood}, []string{"medio"}, "")
	unlock, err := lockProject(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var log strings.Builder
	if _, err := RunOutcome(context.Background(), f.repo, "OUT-001", &log); err == nil || !strings.Contains(err.Error(), "project lock") {
		t.Fatalf("run ignored the project lock: %v", err)
	}
}

func TestPendingRecordsDoNotBreakStatus(t *testing.T) {
	r := newMemoryProject(t)
	if err := SavePending(r, PendingRecord{OutcomeID: "OUT-011", Feature: "checkout", Change: "patch", Note: "n", Commit: fakeSHA('a')}); err != nil {
		t.Fatal(err)
	}
	if _, err := ListStates(r); err != nil {
		t.Fatalf("status broke with a pending record: %v", err)
	}
}

func TestPrivacyRequiresIgnoredDirectory(t *testing.T) {
	dir, _ := newRepo(t)
	write(t, dir, ".gitignore", ".aeh/memory/wiki/index.md\n")
	r, _ := OpenRepo(dir)
	if _, err := InitMemory(r); err == nil || !strings.Contains(err.Error(), "not ignored") {
		t.Fatalf("expected a privacy error, got %v", err)
	}
}

// A failed write restores only what aeh wrote and keeps what someone else changed meanwhile.
func TestRollbackKeepsOtherEdits(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	w := memoryWrites{}
	if err := w.write(dir, "wiki/index.md", []byte("aeh index\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.write(dir, "wiki/log.md", []byte("aeh log\n")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "wiki/log.md", "owner edit\n")
	err := w.restore(dir, errors.New("commit failed"))
	if err == nil || !strings.Contains(err.Error(), "wiki/log.md") || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("restore error %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "wiki", "log.md")); got != "owner edit\n" {
		t.Fatalf("owner edit lost: %q", got)
	}
	if got := mustGit(t, dir, "status", "--porcelain", "--", "wiki/index.md"); got != "" {
		t.Fatalf("aeh's own write was not restored: %s", got)
	}
}

func TestRecordRefusesHiddenIndexFlags(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		mustGit(t, dir, "update-index", flag, "wiki/features/checkout/spec.md")
		p := PendingRecord{OutcomeID: "OUT-012", Feature: "checkout", Change: "patch", Note: "n", Commit: fakeSHA('a')}
		if _, _, err := RecordChange(r, p, ""); !errors.Is(err, ErrMemoryDirty) {
			t.Fatalf("%s: expected ErrMemoryDirty, got %v", flag, err)
		}
		mustGit(t, dir, "update-index", "--no"+flag[1:], "wiki/features/checkout/spec.md")
	}
}

// The owner may save and commit a spec with CRLF line endings on Windows.
func TestRecordAfterCRLFEdit(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	spec := filepath.Join(dir, "wiki", "features", "checkout", "spec.md")
	if err := os.WriteFile(spec, []byte(strings.ReplaceAll(readFile(t, spec), "\n", "\r\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "crlf")
	p := PendingRecord{OutcomeID: "OUT-013", Feature: "checkout", Change: "patch", Note: "n", Commit: fakeSHA('a')}
	if v, _, err := RecordChange(r, p, ""); err != nil || v != "0.1.1" {
		t.Fatalf("record after CRLF edit: %s %v", v, err)
	}
	if err := memoryClean(dir); err != nil {
		t.Fatal(err)
	}
}
