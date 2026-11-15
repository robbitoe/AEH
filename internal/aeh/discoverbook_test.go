package aeh

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validDraft = "---\ntitle: Cart\npaths: [src/cart/**, web/cart/**]\n---\n# Cart\n\n## Acceptance criteria\n- AC-1: Given a cart, when an item is added, then it is listed.\n"

func TestParseDraftFeature(t *testing.T) {
	for name, text := range map[string]string{
		"no front":       "# Cart\n",
		"unknown":        strings.Replace(validDraft, "title: Cart", "title: Cart\nversion: 0.1.0", 1),
		"anchor":         strings.Replace(validDraft, "title: Cart", "title: &a Cart", 1),
		"tag":            strings.Replace(validDraft, "title: Cart", "title: !!str Cart", 1),
		"document":       strings.Replace(validDraft, "paths:", "...\n---\npaths:", 1),
		"no title":       strings.Replace(validDraft, "title: Cart\n", "", 1),
		"no paths":       strings.Replace(validDraft, "paths: [src/cart/**, web/cart/**]\n", "", 1),
		"numeric title":  strings.Replace(validDraft, "title: Cart", "title: 123", 1),
		"numeric path":   strings.Replace(validDraft, "[src/cart/**, web/cart/**]", "[123]", 1),
		"bad paths":      strings.Replace(validDraft, "src/cart/**", "../private/**", 1),
		"no criteria":    strings.Replace(validDraft, "## Acceptance criteria", "## Behavior", 1),
		"empty criteria": strings.Split(validDraft, "- AC-1:")[0],
		"observed":       strings.Replace(validDraft, "- AC-1:", "- AC-1 (observed):", 1),
		"gap":            strings.Replace(validDraft, "AC-1", "AC-2", 1),
		"html":           strings.Replace(validDraft, "title: Cart", `title: "\x3cscript>"`, 1),
		"escaped secret": strings.Replace(validDraft, "title: Cart", `title: "\x67hp_`+strings.Repeat("A", 36)+`"`, 1),
		"long title":     strings.Replace(validDraft, "title: Cart", "title: "+strings.Repeat("a", 201), 1),
		"many paths":     strings.Replace(validDraft, "[src/cart/**, web/cart/**]", "["+strings.Repeat("src/cart/**, ", 20)+"src/cart/**]", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDraftFeature([]byte(text)); err == nil {
				t.Fatal("accepted invalid draft")
			}
		})
	}
	for _, text := range []string{validDraft, strings.ReplaceAll(validDraft, "\n", "\r\n"), strings.Replace(validDraft, "title: Cart", "title: "+strings.Repeat("a", 200), 1)} {
		if _, err := parseDraftFeature([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *discoveryFixture) setReviewer(script, provider string) {
	f.t.Helper()
	write(f.t, f.tmp, "review.sh", "#!/bin/sh\n"+script+"\n")
	b, err := os.ReadFile(filepath.Join(f.dir, "aeh", "config.yaml"))
	if err != nil {
		f.t.Fatal(err)
	}
	text := string(b) + fmt.Sprintf("  qa:\n    provider: %s\n    timeout_minutes: 1\n    command: [\"sh\", %q, \"{{prompt}}\"]\nreviewers:\n  qa: [agent, qa]\n", provider, filepath.ToSlash(filepath.Join(f.tmp, "review.sh")))
	write(f.t, f.dir, "aeh/config.yaml", text)
}

func (f *discoveryFixture) start() {
	f.t.Helper()
	if _, err := DiscoverNew(f.repo, "shop", "A small shop."); err != nil {
		f.t.Fatal(err)
	}
}

func (f *discoveryFixture) report() string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "reports", "discover-shop.md"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *discoveryFixture) commitDraft(name, text string) {
	f.t.Helper()
	mem := filepath.Join(f.dir, ".aeh", "memory")
	write(f.t, mem, "wiki/discovery/shop/drafts/"+name, text)
	mustGit(f.t, mem, "add", "-A")
	mustGit(f.t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "owner edit")
}

func TestDiscoveryBookValidationFeedback(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.start()
	for _, test := range []struct{ name, script, want string }{
		{"front", `sed -i 's/title: Cart/title: Cart\nextra: no/' features/cart.md`, "front matter"},
		{"overlap", `cp features/cart.md features/pay.md`, "paths overlap"},
		{"empty", `sed -i '/^- AC-/d' features/cart.md`, "no criteria"},
		{"observed", `sed -i 's/AC-1:/AC-1 (observed):/' features/cart.md`, "acceptance criteria"},
		{"html document", `printf '<img src=x>\n' >> vision.md`, "raw HTML"},
	} {
		f.setAgent(strings.Replace(writeBook, `echo "DISCOVERY: READY"`, test.script+"\necho \"DISCOVERY: READY\"", 1))
		st := f.run()
		if st.Status != StatusBlocked || len(st.Rounds) != 0 || !strings.Contains(st.Attempts[len(st.Attempts)-1].Feedback, test.want) {
			t.Fatalf("%s: %+v", test.name, st)
		}
		if strings.Contains(f.report(), "<img") {
			t.Fatal("raw agent HTML reached report")
		}
	}
	if _, err := CreateSpec(f.repo, "cart", "Existing cart", []string{"old/cart/**"}); err != nil {
		t.Fatal(err)
	}
	f.setAgent(writeBook)
	st := f.run()
	if st.Status != StatusBlocked || !strings.Contains(st.Attempts[len(st.Attempts)-1].Feedback, "existing feature") {
		t.Fatalf("existing slug: %+v", st)
	}
}

const approveBook = `cat > findings.yaml <<'EOF'
findings:
  - text: "Can an empty cart be paid?"
    why: "The empty case needs a rule."
  - text: "Are coupons needed?"
    why: "Coupons affect totals."
EOF
echo "BOOK-REVIEW: APPROVE"`

func TestDiscoveryBookReviewPromoteAccept(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.setReviewer(approveBook, "b")
	f.start()
	st := f.run()
	if st.Phase != DiscoveryDraft || len(st.Rounds) != 1 {
		t.Fatalf("book state: %+v", st)
	}
	r := st.Rounds[0]
	if r.Review == nil || r.Review.Status != "approve" || r.Review.Profile != "qa" || r.Review.Provider != "b" || len(r.Questions) != 2 || r.Questions[0].Blocking {
		t.Fatalf("review: %+v", r)
	}
	template, _ := loadPrompt(f.repo, "review-book.md")
	if r.Review.Prompt != fmt.Sprintf("%x", sha256.Sum256([]byte(template))) {
		t.Fatal("prompt template digest missing")
	}
	shown, err := DiscoverShow(f.repo, "shop")
	if err != nil || shown.Rounds[0].Review.Stale {
		t.Fatalf("fresh review: %+v %v", shown, err)
	}
	for _, text := range []string{f.memFile("wiki/discovery/shop/rounds/01.md"), f.report()} {
		for _, want := range []string{"Reviewer qa (b)", "R1-Q1 (optional)", "Can an empty cart be paid?", "approve", r.Review.Book} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q:\n%s", want, text)
			}
		}
	}
	if _, err := DiscoverSkip(f.repo, "shop", "R1-Q2"); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", "No, checkout requires an item."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.report(), "No, checkout requires an item.") || !strings.Contains(f.report(), "Skipped by the owner") {
		t.Fatal("review question answers missing")
	}

	// Edits of final draft bytes stale the review, including rebuilt mockups in the digest.
	draft := f.memFile("wiki/discovery/shop/drafts/features/cart.md")
	f.commitDraft("features/cart.md", draft+"\n## Notes\nOwner clarification.\n")
	shown, err = DiscoverShow(f.repo, "shop")
	if err != nil || !shown.Rounds[0].Review.Stale || !strings.Contains(f.report(), "stale") {
		t.Fatalf("edited review: %+v %v", shown, err)
	}
	if _, err := DiscoverPromote(f.repo, "shop", "cart"); err != nil {
		t.Fatal(err)
	}
	specText := f.memFile(specRel("cart"))
	spec, err := parseSpec(specText)
	if err != nil || spec.Version != "0.1.0" || !strings.Contains(specText, "promoted from discovery shop; not implemented yet.") {
		t.Fatalf("promoted spec: %s %v", specText, err)
	}
	if !strings.Contains(f.memFile("wiki/index.md"), "cart") {
		t.Fatal("index was not regenerated")
	}
	approved, _, err := ApproveSpec(f.repo, "cart")
	if err != nil || !approved.Approved() {
		t.Fatalf("approve promoted spec: %v", err)
	}
	crit, _, err := checkSpecCriteria(filepath.Join(f.dir, ".aeh", "memory"), approved)
	if err != nil || len(criteriaDelta(crit, nil).New) != 1 {
		t.Fatalf("stage-3 criteria: %v %v", crit, err)
	}
	memory, err := LoadMemory(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	contract := memory.ContractFingerprint("cart")
	approval := approved.Approval.Content
	mem := memory.Dir
	head := mustGit(t, mem, "rev-parse", "HEAD")
	if _, err := DiscoverPromote(f.repo, "shop", "cart"); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, mem, "rev-parse", "HEAD"); got != head {
		t.Fatal("duplicate promotion committed")
	}
	if _, err := DiscoverAccept(f.repo, "shop", "vision"); err != nil {
		t.Fatal(err)
	}
	if f.memFile("wiki/vision.md") != f.memFile("wiki/discovery/shop/drafts/vision.md") {
		t.Fatal("vision bytes differ")
	}
	head = mustGit(t, mem, "rev-parse", "HEAD")
	if _, err := DiscoverAccept(f.repo, "shop", "vision"); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, mem, "rev-parse", "HEAD"); got != head {
		t.Fatal("duplicate acceptance committed")
	}
	f.commitDraft("vision.md", "# Vision\n\nA revised shop.\n")
	if _, err := DiscoverAccept(f.repo, "shop", "vision"); err != nil {
		t.Fatal(err)
	}
	if f.memFile("wiki/vision.next.md") != "# Vision\n\nA revised shop.\n" {
		t.Fatal("existing vision was overwritten")
	}
	if _, err := DiscoverAccept(f.repo, "shop", "data"); err != nil {
		t.Fatal(err)
	}
	memory, err = LoadMemory(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := memory.Spec("cart")
	if memory.ContractFingerprint("cart") != contract || current.Approval.Content != approval || !current.Approved() {
		t.Fatal("advisory document changed a contract or approval")
	}
	for _, want := range []string{"## Attempts", "profile agent; accepted;", "## Promotions", "wiki/features/cart/spec.md", "## Acceptances", "wiki/vision.next.md"} {
		if !strings.Contains(f.report(), want) {
			t.Fatalf("report missing %q", want)
		}
	}
	if !strings.Contains(f.memFile("wiki/discovery/shop/record.md"), "Accepted vision to wiki/vision.next.md") {
		t.Fatal("session owner action record missing")
	}
}

func TestDiscoveryReviewAdvisoryStatuses(t *testing.T) {
	endToEnd(t)
	secret := "ghp_" + strings.Repeat("A", 36)
	for _, test := range []struct{ name, script, provider, want string }{
		{"reject", `echo "BOOK-REVIEW: REJECT"`, "b", "reject"},
		{"legacy reviewer", `echo "BOOK-REVIEW: APPROVE"`, "b", "approve"},
		{"invalid verdict", `echo "BOOK-REVIEW: APPROVE"; echo "extra"`, "b", "invalid"},
		{"unavailable", `exit 99`, "a", "unavailable"},
		{"secret finding", `printf 'findings:\n  - {text: "\x67hp_` + strings.Repeat("A", 36) + `", why: "x"}\n' > findings.yaml; echo "BOOK-REVIEW: APPROVE"`, "b", "invalid"},
		{"html finding", `echo 'findings: [{text: "<b>x</b>", why: x}]' > findings.yaml; echo "BOOK-REVIEW: APPROVE"`, "b", "invalid"},
		{"unexpected file", `echo x > extra.md; echo "BOOK-REVIEW: APPROVE"`, "b", "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDiscoveryFixture(t, writeBook)
			f.setReviewer(test.script, test.provider)
			if test.name == "legacy reviewer" {
				b, err := os.ReadFile(filepath.Join(f.dir, "aeh", "config.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				write(t, f.dir, "aeh/config.yaml", strings.Replace(string(b), "reviewers:\n  qa: [agent, qa]\n", "reviewer: qa\n", 1))
			}
			f.start()
			st := f.run()
			if st.Phase != DiscoveryDraft || st.Rounds[0].Review.Status != test.want || len(st.Rounds[0].Questions) != 0 {
				t.Fatalf("advisory status: %+v", st)
			}
			if test.want == "unavailable" {
				if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "runs", "discover-shop", "review1")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unavailable reviewer ran")
				}
			}
			shown, err := DiscoverShow(f.repo, "shop")
			if err != nil || shown.Rounds[0].Review.Status != test.want {
				t.Fatalf("show advisory review: %+v %v", shown, err)
			}
			b, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "discovery", "shop", "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{string(b), f.report()} {
				if strings.Contains(text, secret) || strings.Contains(text, "<b>") {
					t.Fatal("unsafe agent output reached state/report")
				}
			}
		})
	}
}

func TestDiscoveryInterruptedBookResumesSnapshot(t *testing.T) {
	endToEnd(t)
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprintf("completed-%t", complete), func(t *testing.T) {
			f := newDiscoveryFixture(t, writeBook)
			marker := filepath.Join(f.tmp, "review-started")
			f.setReviewer(`echo started > "`+filepath.ToSlash(marker)+`"; sleep 60; echo "BOOK-REVIEW: APPROVE"`, "b")
			f.start()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				timer := time.NewTimer(45 * time.Second)
				defer timer.Stop()
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-timer.C:
						cancel()
						return
					case <-ticker.C:
						if _, err := os.Stat(marker); err == nil {
							cancel()
							return
						}
					}
				}
			}()
			_, err := DiscoverRun(ctx, f.repo, "shop", nil)
			<-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interruption: %v", err)
			}
			st, _, err := LoadDiscovery(f.repo, "shop")
			if err != nil || st.Book == nil || st.Book.Review.Status != "interrupted" || st.Phase != DiscoveryReview {
				t.Fatalf("pending book: %+v %v", st, err)
			}
			digest := st.Attempts[0].Snapshot
			f.setAgent("exit 99") // Any new author invocation would exhaust the ladder.
			write(t, f.tmp, "review.sh", approveBook)
			want := "approve"
			if complete {
				p := filepath.Join(f.dir, ".aeh", "discovery", "shop", "snapshots", "a1.json")
				original, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				frozen, err := loadFrozen(p, bookManifest(DiscoveryConfig{MaxFeatures: 12, MaxMockupBytes: 204800}))
				if err != nil {
					t.Fatal(err)
				}
				frozen.Files["vision.md"] = []byte("# A substituted book\n")
				frozen.Digest = outputDigest(frozen.Manifest, frozen.Files)
				if err := saveFrozen(p, frozen); err != nil {
					t.Fatal(err)
				}
				if _, err := DiscoverRun(context.Background(), f.repo, "shop", nil); err == nil || !strings.Contains(err.Error(), "recorded author snapshot") {
					t.Fatalf("substituted snapshot was accepted: %v", err)
				}
				if err := os.WriteFile(p, original, 0o600); err != nil {
					t.Fatal(err)
				}
				st = f.run()
			} else {
				want = "interrupted"
				st, err = DiscoverRun(ctx, f.repo, "shop", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if st.Phase != DiscoveryDraft || st.Book != nil || len(st.Attempts) != 1 || st.Attempts[0].Snapshot != digest || st.Rounds[0].Review.Status != want {
				t.Fatalf("resume: %+v", st)
			}
			if complete && st.Rounds[0].Review.Feedback != "" {
				t.Fatal("successful retry retained interrupted feedback")
			}
		})
	}
}

func TestDiscoveryPromotionConflicts(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.start()
	f.run()
	if _, err := CreateSpec(f.repo, "cart", "Existing", []string{"existing/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverPromote(f.repo, "shop", "cart"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing feature: %v", err)
	}
	f.commitDraft("features/pay.md", strings.Replace(validDraft, "title: Cart", "title: Pay", 1))
	if _, err := CreateSpec(f.repo, "orders", "Orders", []string{"src/cart/internal/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverPromote(f.repo, "shop", "pay"); err == nil || !strings.Contains(err.Error(), "paths overlap") {
		t.Fatalf("overlap: %v", err)
	}
	f.commitDraft("features/pay.md", strings.Replace(validDraft, "[src/cart/**, web/cart/**]", "[src/pay/**]", 1))
	if _, err := DiscoverPromote(f.repo, "shop", "pay"); err != nil {
		t.Fatal(err)
	}
	f.commitDraft("features/billing.md", strings.Replace(validDraft, "[src/cart/**, web/cart/**]", "[src/pay/internal/**]", 1))
	if _, err := DiscoverPromote(f.repo, "shop", "billing"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("promoted overlap: %v", err)
	}
}

func TestParseBookFindingsStrict(t *testing.T) {
	for _, src := range []string{
		"findings: [{text: x, why: y, blocking: true}]",
		"findings: &a []", "findings: !!seq []", "findings: []\n---\nfindings: []",
		"findings: [{text: '```', why: x}]", "findings: [{text: x, why: ''}]",
		"findings: [{text: x, why: y}, {text: x, why: y}]", "{}",
	} {
		if _, err := parseBookFindings([]byte(src), 1, 1, "Reviewer qa (b)"); err == nil {
			t.Fatalf("accepted %s", src)
		}
	}
}
