package aeh

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const obsSHA = "1111111111111111111111111111111111111111"

func newAreaProject(t *testing.T) (Repo, string) {
	t.Helper()
	r := newMemoryProject(t)
	draft := &areaDraft{Front: areaFront{Title: "Billing", Paths: []string{"src/billing/**"}}, Prose: "# Billing\n\nComputes fees.\n"}
	_, err := recordAreaObservations(r, "billing", draft, []observationInput{
		{Text: "Given an order of 100, when billed, then the fee is 2.", Locations: []string{"src/billing/fee.go:42"}},
		{Text: "Given a refund, when billed, then the fee is kept.", Locations: []string{"src/billing/refund.go:10-20"}},
		{Text: "Given no currency, when billed, then USD is assumed.", Locations: []string{"src/billing/fee.go:7"}},
	}, obsSHA)
	if err != nil {
		t.Fatal(err)
	}
	return r, memDir(t, r)
}

func observation(t *testing.T, r Repo, id int) Observation {
	t.Helper()
	area, err := LoadAdoptArea(r, "billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range area.Observations {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("AC-%d not found", id)
	return Observation{}
}

func TestAdoptAreaRecordsObservations(t *testing.T) {
	r, mem := newAreaProject(t)
	area := memRead(t, mem, "wiki/adopt/billing/area.md")
	for _, want := range []string{"title: Billing", "# Billing\n\nComputes fees.\n", "## Acceptance criteria\n- AC-1 (observed): Given an order of 100",
		"- AC-3 (observed): Given no currency"} {
		if !strings.Contains(area, want) {
			t.Fatalf("area.md lacks %q:\n%s", want, area)
		}
	}
	log := memRead(t, mem, "wiki/adopt/billing/observations.log.yaml")
	if !strings.Contains(log, "src/billing/refund.go:10-20") || strings.Count(log, "event: observe") != 3 {
		t.Fatalf("log:\n%s", log)
	}
	if msg := mustGit(t, mem, "log", "-1", "--format=%B"); !strings.Contains(msg, "AEH-Import: adopt/billing ") {
		t.Fatalf("commit message:\n%s", msg)
	}
	// Drafts of areas are not features.
	if snap, err := LoadMemory(r); err != nil || len(snap.Specs) != 0 {
		t.Fatalf("an area became a feature: %v %v", snap.Specs, err)
	}
}

func TestAdoptConfirmBindsToWhatTheOwnerSaw(t *testing.T) {
	r, mem := newAreaProject(t)
	o := observation(t, r, 1)
	if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, "000000000000"); err == nil || !strings.Contains(err.Error(), o.Text) || !strings.Contains(err.Error(), o.Token()) {
		t.Fatalf("a stale token must show the current text and token: %v", err)
	}
	got, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, o.Token())
	if err != nil || got.Status != ObservationConfirmed {
		t.Fatalf("keep: %+v %v", got, err)
	}
	head := mustGit(t, mem, "rev-parse", "HEAD")
	if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, "anything"); err != nil {
		t.Fatalf("the same decision again must be a no-op: %v", err)
	}
	if mustGit(t, mem, "rev-parse", "HEAD") != head {
		t.Fatal("a repeated decision committed")
	}
	if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionDrop, o.Token()); err == nil || !strings.Contains(err.Error(), "already decided") {
		t.Fatalf("a different decision must fail: %v", err)
	}

	bug := observation(t, r, 2)
	if _, err := AdoptConfirm(r, "billing", "AC-2", DecisionBug, bug.Token()); err != nil {
		t.Fatal(err)
	}
	drop := observation(t, r, 3)
	if _, err := AdoptConfirm(r, "billing", "AC-3", DecisionDrop, drop.Token()); err != nil {
		t.Fatal(err)
	}
	area := memRead(t, mem, "wiki/adopt/billing/area.md")
	if !strings.Contains(area, "- AC-1: Given an order of 100") || strings.Contains(area, "AC-2") || strings.Contains(area, "AC-3") {
		t.Fatalf("area.md after decisions:\n%s", area)
	}
	defects := memRead(t, mem, "wiki/adopt/billing/defects.md")
	if !strings.Contains(defects, "- AC-2: Given a refund") || strings.Contains(defects, "AC-3") {
		t.Fatalf("defects.md:\n%s", defects)
	}
	log := memRead(t, mem, "wiki/adopt/billing/observations.log.yaml")
	if strings.Count(log, "event: confirm") != 3 || !strings.Contains(log, "decision: drop") {
		t.Fatalf("every decision is logged, drop included:\n%s", log)
	}

	// Retired numbers are never reused, and decided observations cannot be observed again.
	lockAndObserve := func(in observationInput) (areaIdentity, error) {
		return recordAreaObservations(r, "billing", nil, []observationInput{in}, obsSHA)
	}
	next, err := lockAndObserve(observationInput{Text: "Given a coupon, when billed, then the fee drops.", Locations: []string{"src/billing/coupon.go:3"}})
	if err != nil || next.High != 4 || next.Obs[4] == nil {
		t.Fatalf("new observation: %+v %v", next.Obs, err)
	}
	for _, id := range []int{1, 2, 3, 9} {
		if _, err := lockAndObserve(observationInput{ID: id, Text: "Changed.", Locations: []string{"a.go:1"}}); err == nil {
			t.Errorf("AC-%d was observed again", id)
		}
	}
}

func TestAdoptReobservationChangesTheToken(t *testing.T) {
	r, _ := newAreaProject(t)
	before := observation(t, r, 1)
	if _, err := recordAreaObservations(r, "billing", nil, []observationInput{{ID: 1, Text: "Given an order of 100, when billed, then the fee is 3.",
		Locations: []string{"src/billing/fee.go:44"}}}, "2222222222222222222222222222222222222222"); err != nil {
		t.Fatal(err)
	}
	after := observation(t, r, 1)
	if after.Token() == before.Token() || after.CodeSHA[0] != '2' {
		t.Fatalf("re-observation: %+v", after)
	}
	if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, before.Token()); err == nil || !strings.Contains(err.Error(), "fee is 3") {
		t.Fatalf("a decision on the old text must fail and show the new one: %v", err)
	}
}

func TestAdoptRejectsHandEditsAndRewrites(t *testing.T) {
	r, mem := newAreaProject(t)
	commit := func(msg string) {
		t.Helper()
		mustGit(t, mem, "add", "-A")
		mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", msg)
	}
	areaPath := "wiki/adopt/billing/area.md"
	original := memRead(t, mem, areaPath)

	// Prose is the owner's to edit.
	write(t, mem, areaPath, strings.Replace(original, "Computes fees.", "Computes fees for orders.", 1))
	commit("owner prose")
	o := observation(t, r, 1)

	// A criterion added or changed by hand is reported, not accepted.
	edited := memRead(t, mem, areaPath)
	for name, bad := range map[string]string{
		"changed": strings.Replace(edited, "fee is 2.", "fee is 5.", 1),
		"added":   edited + "- AC-4: Given anything, then something.\n",
		"removed": strings.Replace(edited, "- AC-3 (observed): Given no currency, when billed, then USD is assumed.\n", "", 1),
	} {
		write(t, mem, areaPath, bad)
		commit(name)
		if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, o.Token()); err == nil || !strings.Contains(err.Error(), "does not match the observation log") {
			t.Fatalf("%s criterion: %v", name, err)
		}
		write(t, mem, areaPath, edited)
		commit("restore")
	}

	// The log only grows: removing an event, or restoring an older log, is refused.
	logPath := "wiki/adopt/billing/observations.log.yaml"
	if _, err := AdoptConfirm(r, "billing", "AC-3", DecisionDrop, observation(t, r, 3).Token()); err != nil {
		t.Fatal(err)
	}
	full := memRead(t, mem, logPath)
	older := mustGit(t, mem, "show", "HEAD~1:"+logPath) + "\n"
	write(t, mem, logPath, older)
	write(t, mem, areaPath, edited)
	commit("revive AC-3")
	if _, err := LoadAdoptArea(r, "billing"); err == nil || !strings.Contains(err.Error(), "drops or rewrites") {
		t.Fatalf("a restored older log must be refused: %v", err)
	}
	write(t, mem, logPath, full)
	commit("restore log")
	mustGit(t, mem, "rm", "-q", logPath)
	commit("delete log")
	if _, err := LoadAdoptArea(r, "billing"); err == nil || !strings.Contains(err.Error(), "missing at memory HEAD") {
		t.Fatalf("a deleted log must be refused: %v", err)
	}
}

// A merge that keeps an older log cannot drop the events of the merged branch.
func TestAdoptRejectsMergeThatDropsEvents(t *testing.T) {
	r, mem := newAreaProject(t)
	main := mustGit(t, mem, "rev-parse", "--abbrev-ref", "HEAD")
	mustGit(t, mem, "checkout", "-q", "-b", "side")
	if _, err := AdoptConfirm(r, "billing", "AC-3", DecisionDrop, observation(t, r, 3).Token()); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mem, "checkout", "-q", main)
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "merge", "-q", "-s", "ours", "--no-edit", "side")
	if _, err := LoadAdoptArea(r, "billing"); err == nil || !strings.Contains(err.Error(), "drops or rewrites") {
		t.Fatalf("a merge that keeps the older log must be refused: %v", err)
	}
}

func TestReplayObservationRules(t *testing.T) {
	text := "Given x, when y, then z."
	obs := func(id int) observationEvent {
		return observationEvent{Event: "observe", ID: id, Text: text, TextSHA: observationDigest(text), CodeSHA: obsSHA, Locations: []string{"a.go:1"}, Date: "2026-11-15"}
	}
	confirm := func(id int, d string) observationEvent {
		return observationEvent{Event: "confirm", ID: id, Text: text, TextSHA: observationDigest(text), CodeSHA: obsSHA, Decision: d, Date: "2026-11-15"}
	}
	if _, err := replayObservations([]observationEvent{obs(1), obs(2), confirm(1, DecisionKeep), confirm(2, DecisionBug)}); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]observationEvent{
		"skipped number":  {obs(2)},
		"confirm unknown": {confirm(1, DecisionKeep)},
		"confirm twice":   {obs(1), confirm(1, DecisionKeep), confirm(1, DecisionKeep)},
		"observe retired": {obs(1), confirm(1, DecisionDrop), obs(1)},
		"bad decision":    {obs(1), confirm(1, "maybe")},
		"no locations":    {func() observationEvent { e := obs(1); e.Locations = nil; return e }()},
		"digest mismatch": {func() observationEvent { e := obs(1); e.TextSHA = observationDigest("other"); return e }()},
		"stale binding":   {obs(1), func() observationEvent { e := confirm(1, DecisionKeep); e.CodeSHA = strings.Repeat("2", 40); return e }()},
		"html text": {func() observationEvent {
			e := obs(1)
			e.Text = "<b>x</b>"
			e.TextSHA = observationDigest(e.Text)
			return e
		}()},
		"criterion inside": {func() observationEvent {
			e := obs(1)
			e.Text = "see AC-2: x"
			e.TextSHA = observationDigest(e.Text)
			return e
		}()},
		"bad location":   {func() observationEvent { e := obs(1); e.Locations = []string{"../x.go:1"}; return e }()},
		"reversed range": {func() observationEvent { e := obs(1); e.Locations = []string{"x.go:9-3"}; return e }()},
		"unknown event":  {func() observationEvent { e := obs(1); e.Event = "edit"; return e }()},
	}
	for name, events := range bad {
		if _, err := replayObservations(events); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAdoptRejectsUnsafeInput(t *testing.T) {
	r := newMemoryProject(t)
	secret := "ghp_" + strings.Repeat("C", 36)
	draft := &areaDraft{Front: areaFront{Title: "Billing", Paths: []string{"src/billing/**"}}, Prose: "# Billing\n"}
	for name, in := range map[string]observationInput{
		"secret":   {Text: "Uses token " + secret, Locations: []string{"a.go:1"}},
		"html":     {Text: "<script>x</script>", Locations: []string{"a.go:1"}},
		"location": {Text: "Fine.", Locations: []string{"/etc/passwd:1"}},
		"bidi":     {Text: "Fine.", Locations: []string{"src/\u202efee.go:1"}},
		"leak":     {Text: "Fine.", Locations: []string{"src/" + secret + ".:1"}},
	} {
		_, err := recordAreaObservations(r, "billing", draft, []observationInput{in}, obsSHA)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, d := range map[string]*areaDraft{
		"root path":      {Front: areaFront{Title: "B", Paths: []string{"**"}}, Prose: "x\n"},
		"criteria prose": {Front: areaFront{Title: "B", Paths: []string{"src/**"}}, Prose: "## Acceptance criteria\n- AC-1: x\n"},
		"html prose":     {Front: areaFront{Title: "B", Paths: []string{"src/**"}}, Prose: "<b>x</b>\n"},
	} {
		if _, err := recordAreaObservations(r, "billing", d, []observationInput{{Text: "Fine.", Locations: []string{"a.go:1"}}}, obsSHA); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := AdoptConfirm(r, "billing", "AC-1", DecisionKeep, "x"); err == nil {
		t.Fatal("confirmed an observation of an area that does not exist")
	}
	if _, err := AdoptConfirm(r, "billing", "AC-01", DecisionKeep, "x"); err == nil {
		t.Fatal("accepted a malformed reference")
	}
	if snap, _ := LoadMemory(r); !errors.Is(memoryClean(snap.Dir), nil) {
		t.Fatal("a rejected input left memory dirty")
	}
}

func memRead(t *testing.T, dir, rel string) string {
	t.Helper()
	return readFile(t, filepath.Join(dir, filepath.FromSlash(rel)))
}
