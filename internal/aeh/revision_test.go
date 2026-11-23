package aeh

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// reviseCart writes a book whose cart draft revises the existing cart feature: AC-1 reworded,
// AC-2 new.
const reviseCart = `mkdir -p features
printf '# Vision\n\nA shop with limits.\n' > vision.md
printf '# Data\n\n- Cart: items.\n' > data.md
printf '%s\n' '---' 'title: Cart' 'paths: [src/cart/**, web/cart/**]' '---' '# Cart' '' 'The cart holds at most ten items.' '' '## Acceptance criteria' '- AC-1: Given an empty cart, when the user adds an item, then the cart lists it.' '- AC-2: Given ten items, when the user adds one more, then it is refused.' > features/cart.md
echo "DISCOVERY: READY"`

// revisedCartFixture promotes and approves cart from session shop, then imports a revision of
// it from session shop2.
func revisedCartFixture(t *testing.T) *discoveryFixture {
	t.Helper()
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.start()
	f.run()
	if _, err := DiscoverPromote(f.repo, "shop", "cart"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ApproveSpec(f.repo, "cart"); err != nil {
		t.Fatal(err)
	}
	f.setAgent(reviseCart)
	if _, err := DiscoverNew(f.repo, "shop2", "Limit the cart."); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	st, err := DiscoverRun(context.Background(), f.repo, "shop2", &log)
	if err != nil || st.Phase != DiscoveryDraft {
		t.Fatalf("revision round: %+v %v\n%s", st, err, log.String())
	}
	return f
}

func commitMemoryFile(t *testing.T, r Repo, rel, text string) {
	t.Helper()
	dir := memDir(t, r)
	write(t, dir, rel, text)
	mustGit(t, dir, "add", "--", rel)
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "owner edit")
}

// A draft for an existing feature is imported as its pending revision, the only source from
// then on; another session cannot propose a second one; promotion writes the next, unapproved
// version with the history kept and removes the revision in the same commit.
func TestDiscoveryRevisionLifecycle(t *testing.T) {
	f := revisedCartFixture(t)
	rv, err := parseRevision([]byte(f.memFile(revisionRel("cart"))))
	if err != nil {
		t.Fatal(err)
	}
	m := rv.Front.Revision
	if m.Session != "shop2" || !reflect.DeepEqual(m.Changed, []int{1}) || !reflect.DeepEqual(m.New, []int{2}) || m.Removed != nil || m.Paths || !isFullSHA(m.Base) {
		t.Fatalf("revision: %+v", rv.Front)
	}
	if spec := f.memFile(specRel("cart")); strings.Contains(spec, "ten items") {
		t.Fatal("importing a revision changed the spec")
	}

	// A second session's revision of the same feature is refused.
	if _, err := DiscoverNew(f.repo, "shop3", "Limit the cart again."); err != nil {
		t.Fatal(err)
	}
	_, _ = DiscoverRun(context.Background(), f.repo, "shop3", &strings.Builder{})
	st3, _, err := LoadDiscovery(f.repo, "shop3")
	if err != nil || st3.Phase == DiscoveryDraft || len(st3.Attempts) == 0 || !strings.Contains(st3.Attempts[len(st3.Attempts)-1].Feedback, "pending revision from discovery shop2") {
		t.Fatalf("second revision: %+v %v", st3, err)
	}

	// The owner edits spec.next.md: promotion takes it from there.
	text := f.memFile(revisionRel("cart"))
	commitMemoryFile(t, f.repo, revisionRel("cart"), strings.Replace(text, "at most ten items.", "at most ten items. Owner note.", 1))
	if _, err := DiscoverPromote(f.repo, "shop2", "cart"); err != nil {
		t.Fatal(err)
	}
	specText := f.memFile(specRel("cart"))
	spec, err := parseSpec(specText)
	if err != nil || spec.Version != "0.2.0" {
		t.Fatalf("promoted revision: %v\n%s", err, specText)
	}
	for _, want := range []string{"Owner note.", "- AC-2: Given ten items", "promoted from discovery shop;", "- 0.2.0 (", "revised from discovery shop2 (from_revision "} {
		if !strings.Contains(specText, want) {
			t.Fatalf("missing %q:\n%s", want, specText)
		}
	}
	if _, err := os.Stat(filepath.Join(memDir(t, f.repo), filepath.FromSlash(revisionRel("cart")))); !os.IsNotExist(err) {
		t.Fatalf("spec.next.md survived the promotion: %v", err)
	}
	if out := mustGit(t, memDir(t, f.repo), "show", "--name-status", "--format=", "HEAD"); !strings.Contains(out, "D\twiki/features/cart/spec.next.md") || !strings.Contains(out, "M\twiki/features/cart/spec.md") {
		t.Fatalf("one commit writes the spec and removes the revision:\n%s", out)
	}
	snap, _ := LoadMemory(f.repo)
	if mustSpec(t, snap, "cart").Approved() {
		t.Fatal("a promoted revision must wait for the owner's approval")
	}
	// A retry finds the revision promoted.
	if _, err := DiscoverPromote(f.repo, "shop2", "cart"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s, _, err := ApproveSpec(f.repo, "cart"); err != nil || s.Version != "0.2.0" {
		t.Fatalf("approve the revision: %+v %v", s, err)
	}
	st, err := DiscoverShow(f.repo, "shop2")
	if err != nil || len(st.Promotions) != 1 || !st.Promotions[0].Revision {
		t.Fatalf("show: %+v %v", st, err)
	}
	if !strings.Contains(f.memFile("wiki/discovery/shop2/record.md"), "Promoted the revision of cart") {
		t.Fatal("record.md does not name the revision")
	}
}

// A revision whose spec changed after the import is stale: promotion is refused, and only the
// session that proposed it can discard it.
func TestDiscoveryRevisionStaleAndDiscard(t *testing.T) {
	f := revisedCartFixture(t)
	editSpec(t, f.repo, "cart", "The cart holds items.", "The cart holds items for one session.")
	if _, err := DiscoverPromote(f.repo, "shop2", "cart"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := DiscoverDiscard(f.repo, "shop", "cart"); err == nil || !strings.Contains(err.Error(), "belongs to discovery shop2") {
		t.Fatalf("discard by another session: %v", err)
	}
	if _, err := DiscoverDiscard(f.repo, "shop2", "cart"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(memDir(t, f.repo), filepath.FromSlash(revisionRel("cart")))); !os.IsNotExist(err) {
		t.Fatalf("discard kept spec.next.md: %v", err)
	}
	if !strings.Contains(f.memFile("wiki/discovery/shop2/record.md"), "Discarded the revision of cart") {
		t.Fatal("record.md does not name the discard")
	}
	if _, err := DiscoverDiscard(f.repo, "shop2", "cart"); err != nil {
		t.Fatalf("a repeated discard: %v", err)
	}
	if _, err := DiscoverDiscard(f.repo, "shop", "cart"); err == nil || !strings.Contains(err.Error(), "no pending revision") {
		t.Fatalf("nothing to discard: %v", err)
	}
	if !strings.Contains(f.memFile(specRel("cart")), "for one session") {
		t.Fatal("discard touched the spec")
	}
}

// A revision keeps history's criterion numbers and may not claim another feature's paths or
// carry a changelog.
func TestRevisionCandidateRules(t *testing.T) {
	r := newMemoryProject(t)
	for _, f := range []struct{ name, path string }{{"pay", "src/pay/**"}, {"ship", "src/ship/**"}} {
		if _, err := CreateSpec(r, f.name, f.name, []string{f.path}); err != nil {
			t.Fatal(err)
		}
	}
	describeSpec(t, r, "pay", "Pays.\n\n## Acceptance criteria\n- AC-1: Given a cart, when paying, then it is paid.\n- AC-2: Given a cart, when paying twice, then nothing happens.\n")
	if _, _, err := ApproveSpec(r, "pay"); err != nil {
		t.Fatal(err)
	}
	editSpec(t, r, "pay", "- AC-2: Given a cart, when paying twice, then nothing happens.\n", "")
	raiseVersion(t, r, "pay")
	if _, _, err := ApproveSpec(r, "pay"); err != nil {
		t.Fatal(err)
	}
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	body := func(criteria string) string { return "Pays.\n\n## Acceptance criteria\n" + criteria }
	for name, c := range map[string]struct {
		paths []string
		body  string
		want  string
	}{
		"retired number": {[]string{"src/pay/**"}, body("- AC-1: Given a cart, when paying, then it is paid.\n- AC-2: Given a cart, when paying twice, then it is refused.\n"), "retired"},
		"changelog":      {[]string{"src/pay/**"}, body("- AC-1: Given a cart, when paying, then it is paid.\n") + "\n## Changelog\n- x\n", "no changelog"},
		"taken paths":    {[]string{"src/pay/**", "src/ship/labels/**"}, body("- AC-1: Given a cart, when paying, then it is paid.\n"), "overlap feature ship"},
		"no criteria":    {[]string{"src/pay/**"}, "Pays.\n", "non-empty"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := revisionCandidate(dir, snap, "pay", "Pay", c.paths, c.body); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	meta, err := revisionCandidate(dir, snap, "pay", "Pay", []string{"src/payments/**"}, body("- AC-1: Given a cart, when paying, then it is paid at once.\n- AC-3: Given a refund, when it is asked, then it is paid back.\n"))
	if err != nil || !reflect.DeepEqual(meta.Changed, []int{1}) || !reflect.DeepEqual(meta.New, []int{3}) || !meta.Paths {
		t.Fatalf("valid revision: %+v %v", meta, err)
	}
}

// While a version that changed the paths is not implemented, the feature owns the union of the
// old and new paths: the drift gate counts both and no other feature may claim the old ones. A
// version that only changes paths is a plan without implementation; its record releases them.
func TestPathMigrationOwnsTheUnionUntilRecorded(t *testing.T) {
	f, _ := retirementFixture(t, false)
	if im := lastRecordedImplementation(t, f); !reflect.DeepEqual(im.Paths, []string{"src/**"}) {
		t.Fatalf("an implementation records the paths it implemented: %+v", im.Paths)
	}
	if _, err := CreateSpec(f.repo, "docs", "Docs", []string{"docs/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, f.repo, "docs", "Docs.\n\n## Acceptance criteria\n- AC-1: Given docs, when read, then they exist.\n")
	if _, _, err := ApproveSpec(f.repo, "docs"); err != nil {
		t.Fatal(err)
	}
	editSpec(t, f.repo, "pay", "paths: [src/**]", "paths: [lib/**]")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	snap, _ := LoadMemory(f.repo)
	pay := mustSpec(t, snap, "pay")
	if !pay.PathsMigrating() || !reflect.DeepEqual(pay.OwnedPaths(), []string{"lib/**", "src/**"}) {
		t.Fatalf("owned while migrating: %v", pay.OwnedPaths())
	}
	if g := gateDrift([]string{"src/pay.txt", "lib/pay.txt"}, snap, "pay"); !g.Passed || len(g.Warnings) != 0 {
		t.Fatalf("the drift gate must count both paths: %+v", g)
	}
	// Another feature cannot claim the old paths yet.
	editSpec(t, f.repo, "docs", "paths: [docs/**]", "paths: [docs/**, src/legacy/**]")
	raiseVersion(t, f.repo, "docs")
	if _, _, err := ApproveSpec(f.repo, "docs"); err == nil || !strings.Contains(err.Error(), "overlap feature pay") {
		t.Fatalf("claiming paths of a migrating feature: %v", err)
	}
	p := f.plan()
	if p.Phase != PlanLogged || !p.NoImplementation {
		t.Fatalf("a paths-only version is a plan without implementation: %+v", p)
	}
	im := lastRecordedImplementation(t, f)
	if im.Outcome != p.ID || !reflect.DeepEqual(im.Paths, []string{"lib/**"}) {
		t.Fatalf("the record releases the old paths: %+v", im)
	}
	snap, _ = LoadMemory(f.repo)
	if pay = mustSpec(t, snap, "pay"); pay.PathsMigrating() || !reflect.DeepEqual(pay.OwnedPaths(), []string{"lib/**"}) {
		t.Fatalf("owned after the record: %v", pay.OwnedPaths())
	}
	if !strings.Contains(readFile(t, filepath.Join(memDir(t, f.repo), filepath.FromSlash(specRel("pay")))), "reassigns its paths to lib/**") {
		t.Fatal("the changelog does not say the paths moved")
	}
	if _, _, err := ApproveSpec(f.repo, "docs"); err != nil {
		t.Fatalf("released paths can be claimed: %v", err)
	}
}

// An adopted feature keeps its paths; once characterized, a changed spec retires its
// characterization tests like any sealed test, keeping observed_base_sha and its numbers.
func TestAdoptedFeatureRevisionAfterCharacterization(t *testing.T) {
	f, observed := newCharFixture(t, charAuthorOK)
	st, log, err := f.seal()
	if err != nil || st.Phase != PlanLogged {
		t.Fatalf("seal: %v %+v\n%s", err, st, log)
	}
	mustGit(t, f.dir, "merge", "-q", "--ff-only", IntegrationBranch(st.ID))
	editSpec(t, f.repo, "pay", "paths: [src/**]", "paths: [lib/**]")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err == nil || !strings.Contains(err.Error(), "adopted from existing code") {
		t.Fatalf("an adopted feature changed its paths: %v", err)
	}
	editSpec(t, f.repo, "pay", "paths: [lib/**]", "paths: [src/**]")
	// AC-2 was dropped during adoption: its number stays retired.
	snap, _ := LoadMemory(f.repo)
	spec := mustSpec(t, snap, "pay")
	reuse := spec
	reuse.Body = strings.Replace(spec.Body, "## Acceptance criteria\n", "## Acceptance criteria\n- AC-2: Given a cart, when the user pays twice, then it is refused.\n", 1)
	if _, _, err := checkSpecCriteria(snap.Dir, reuse); err == nil {
		t.Fatal("a dropped observation's number was reused")
	}
	// Remove AC-3: its characterization test is retired by a plan without implementation.
	var line string
	for _, l := range strings.Split(spec.Body, "\n") {
		if strings.HasPrefix(l, "- AC-3") {
			line = l + "\n"
		}
	}
	if line == "" {
		t.Fatalf("no AC-3 in:\n%s", spec.Body)
	}
	editSpec(t, f.repo, "pay", line, "")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	p := f.plan()
	if p.Phase != PlanLogged || !p.NoImplementation || len(p.Retired) != 1 || !strings.HasSuffix(p.Retired[0].Path, "currency.sh") {
		t.Fatalf("retire the characterization test: %+v", p)
	}
	snap, _ = LoadMemory(f.repo)
	ims := mustSpec(t, snap, "pay").Implemented
	if len(ims) != 2 || ims[0].ObservedBase != observed || ims[1].Outcome != p.ID || len(ims[1].Retired) != 1 {
		t.Fatalf("implementations: %+v", ims)
	}
}

// An interrupted import that removed a pending revision puts it back before retrying.
func TestDiscardPartialImportRestoresRemovedRevision(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "pay", "Pay", []string{"src/pay/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	rel := revisionRel("pay")
	commitMemoryFile(t, r, rel, "pending revision\n")
	mustGit(t, dir, "rm", "-q", "--", rel)
	p := &pendingImport{Files: map[string][]byte{}, Remove: []string{rel}}
	if err := discardPartialImport(dir, p, "discover/shop"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, filepath.FromSlash(rel))); got != "pending revision\n" {
		t.Fatalf("restored: %q", got)
	}
	if err := memoryClean(dir); err != nil {
		t.Fatal(err)
	}
}

// The supersede check of a plan without implementation reads committed memory: an owner's
// uncommitted edit of implemented.yaml does not mark the plan for restart.
func TestSupersedeCheckReadsCommittedMemory(t *testing.T) {
	f, _ := retirementFixture(t, false)
	dir := memDir(t, f.repo)
	editSpec(t, f.repo, "pay", "paths: [src/**]", "paths: [lib/**]")
	raiseVersion(t, f.repo, "pay")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	// The reviewer leaves an uncommitted implementation record behind: the record waits.
	impl := filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(implementedRel("pay"))))
	f.setScript("qa", countCall+reviewOK+`; sed -i.bak 's/code_sha: [0-9a-f]*/code_sha: `+fakeSHA('c')+`/' `+yamlQuote(impl)+`; rm -f `+yamlQuote(impl+".bak"))
	if _, err := PlanFeature(context.Background(), f.repo, "pay", nil); err == nil {
		t.Fatal("the record should wait for clean memory")
	}
	plans, _ := ListPlanStates(f.repo)
	for _, p := range plans {
		if p.Phase == PlanPublished && p.RestartRequired {
			t.Fatalf("an uncommitted edit superseded the plan: %+v", p)
		}
	}
	mustGit(t, dir, "checkout", "-q", "--", ".")
	f.setScript("qa", countCall+reviewOK)
	if p := f.plan(); p.Phase != PlanLogged {
		t.Fatalf("after restoring memory: %+v", p)
	}
}
