package aeh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A saved batch that removes a revision is not moved onto a HEAD that changed that revision.
func TestRebasedBatchChecksRemovedFiles(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "pay", "Pay", []string{"src/pay/**"}); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	commitMemoryFile(t, r, revisionRel("pay"), "revision as imported\n")
	head := mustGit(t, dir, "rev-parse", "HEAD")
	commitMemoryFile(t, r, revisionRel("pay"), "revision edited by the owner\n")
	p := &pendingImport{Files: map[string][]byte{"wiki/discovery/shop/record.md": []byte("# Discovery shop\n")}, Remove: []string{revisionRel("pay")},
		ExpectedHead: head, Subject: "discover shop: discard revision pay", Log: "discard revision pay"}
	err := finishImport(r, dir, "discover/shop", p, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), revisionRel("pay")) {
		t.Fatalf("a removal over an edited revision: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, filepath.FromSlash(revisionRel("pay")))); got != "revision edited by the owner\n" {
		t.Fatalf("the edited revision was touched: %q", got)
	}
}

// The provenance of a pending revision cannot be edited: not its base commit (to hide that it
// is stale) and not its session (to hand it to another session).
func TestRevisionMetadataCannotBeEdited(t *testing.T) {
	f := revisedCartFixture(t)
	dir := memDir(t, f.repo)
	original := f.memFile(revisionRel("cart"))
	rv, err := parseRevision([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	editSpec(t, f.repo, "cart", "The cart holds items.", "The cart holds items for one session.")
	head := mustGit(t, dir, "rev-parse", "HEAD")
	commitMemoryFile(t, f.repo, revisionRel("cart"), strings.Replace(original, rv.Front.Revision.Base, head, 1))
	if _, err := DiscoverPromote(f.repo, "shop2", "cart"); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("a rewritten base commit: %v", err)
	}
	commitMemoryFile(t, f.repo, revisionRel("cart"), strings.Replace(original, "session: shop2", "session: shop", 1))
	if _, err := DiscoverDiscard(f.repo, "shop", "cart"); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatalf("a rewritten session: %v", err)
	}
	commitMemoryFile(t, f.repo, revisionRel("cart"), strings.Replace(original, "The cart holds at most ten items.", "The cart holds at most ten items, says the owner.", 1))
	if _, err := DiscoverPromote(f.repo, "shop2", "cart"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("an owner's text edit keeps the provenance: %v", err)
	}
}

// An implementation recorded before records carried paths takes them from the spec at its
// record commit, so a later path change still owns the union.
func TestLegacyImplementationPathsFromHistory(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "pay", "Pay", []string{"src/pay/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "pay", "Pays.\n\n## Acceptance criteria\n- AC-1: Given a cart, when paying, then it is paid.\n")
	spec, _, err := ApproveSpec(r, "pay")
	if err != nil {
		t.Fatal(err)
	}
	recordImplementation(t, r, spec, "PAY-1", fakeSHA('a'))
	// The owner's next draft (new paths) is committed before the legacy record lands, so the
	// spec at the record commit is the draft, not what was implemented.
	editSpec(t, r, "pay", "paths: [src/pay/**]", "paths: [lib/pay/**]")
	dir := memDir(t, r)
	p := filepath.Join(dir, filepath.FromSlash(implementedRel("pay")))
	var kept []string
	for _, line := range strings.Split(readFile(t, p), "\n") {
		if !strings.Contains(line, "paths:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(p, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "record pay (PAY-1)\n\nAEH-Record: PAY-1 "+fakeSHA('a'))
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	pay := mustSpec(t, snap, "pay")
	if !pay.PathsMigrating() || !reflect.DeepEqual(pay.OwnedPaths(), []string{"lib/pay/**", "src/pay/**"}) {
		t.Fatalf("legacy record: %v", pay.OwnedPaths())
	}
}

// The first approval checks the path claim too: an adopted area's paths are taken.
func TestFirstApprovalChecksPathClaims(t *testing.T) {
	r, _ := newAreaProject(t)
	if _, err := CreateSpec(r, "fees", "Fees", []string{"src/billing/fees/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "fees", "Fees.\n\n## Acceptance criteria\n- AC-1: Given an order, when billed, then a fee is added.\n")
	if _, _, err := ApproveSpec(r, "fees"); err == nil || !strings.Contains(err.Error(), "overlap adopted area billing") {
		t.Fatalf("a first approval claimed an adopted area: %v", err)
	}
}

// A revision's version is above every version the spec ever had, approved or not.
func TestNextRevisionVersionAboveHistory(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "pay", "Pay", []string{"src/pay/**"}); err != nil {
		t.Fatal(err)
	}
	editSpec(t, r, "pay", "version: 0.1.0", "version: 0.5.0")
	editSpec(t, r, "pay", "version: 0.5.0", "version: 0.1.0")
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := nextRevisionVersion(memDir(t, r), mustSpec(t, snap, "pay")); err != nil || v != "0.6.0" {
		t.Fatalf("next version: %s %v", v, err)
	}
}
