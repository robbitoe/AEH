package aeh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCriteria(t *testing.T) {
	ok := []struct {
		name, body string
		want       []SpecCriterion
		present    bool
	}{
		{"absent", "# X\nText.\n\n## Changelog\n", nil, false},
		{"basic", "## Acceptance criteria\n- AC-1: Pays   once.\n- AC-2: Retries\tsafely.\n\n## Changelog\n",
			[]SpecCriterion{{1, "Pays once."}, {2, "Retries safely."}}, true},
		{"prose and fences before the heading", "The buyer:\n```\n- AC-9: an example, not a criterion\n## not a heading\n```\n## Acceptance criteria\n- AC-3: Gets a receipt.\n## Changelog\n",
			[]SpecCriterion{{3, "Gets a receipt."}}, true},
		{"fenced heading lookalike", "```\n## Acceptance criteria\n- AC-1: x\n```\n## Changelog\n", nil, false},
		{"crlf and trailing spaces", "## Acceptance criteria  \r\n- AC-1: Pays.  \r\n## Changelog\r\n", []SpecCriterion{{1, "Pays."}}, true},
		{"ends at first-level heading", "## Acceptance criteria\n- AC-1: Pays.\n# Appendix\n- anything\n", []SpecCriterion{{1, "Pays."}}, true},
		{"closing hashes", "## Acceptance criteria ##\n- AC-1: Pays.\n\n## Changelog\n", []SpecCriterion{{1, "Pays."}}, true},
		{"large id", "## Acceptance criteria\n- AC-1000001: Pays.\n", []SpecCriterion{{1000001, "Pays."}}, true},
		{"html without section", "<!-- note -->\nText.\n", nil, false},
		{"code span and comparison", "## Acceptance criteria\n- AC-1: Render `<!-- note -->` literally when amount > 0.\n",
			[]SpecCriterion{{1, "Render `<!-- note -->` literally when amount > 0."}}, true},
		{"autolink in prose", "See <https://example.com/pay> and <ops@example.com>.\n\n## Acceptance criteria\n- AC-1: Pays.\n", []SpecCriterion{{1, "Pays."}}, true},
	}
	for _, c := range ok {
		got, present, err := parseCriteria(c.body)
		if err != nil || present != c.present || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v %v %v, want %v %v", c.name, got, present, err, c.want, c.present)
		}
	}
	bad := map[string]string{
		"leading zero":        "## Acceptance criteria\n- AC-01: x\n",
		"zero":                "## Acceptance criteria\n- AC-0: x\n",
		"duplicate":           "## Acceptance criteria\n- AC-1: x\n- AC-1: y\n",
		"no text":             "## Acceptance criteria\n- AC-1:   \n",
		"other bullet":        "## Acceptance criteria\n- AC-1: x\n* AC-2: y\n",
		"plain bullet":        "## Acceptance criteria\n- AC-1: x\n- also this\n",
		"numbered":            "## Acceptance criteria\n1. AC-1: x\n",
		"continuation":        "## Acceptance criteria\n- AC-1: x\n  and more\n",
		"tab continuation":    "## Acceptance criteria\n- AC-1: x\n\tand more\n",
		"observed reserved":   "## Acceptance criteria\n- AC-1 (observed): x\n",
		"empty section":       "## Acceptance criteria\nNothing yet.\n## Changelog\n",
		"two sections":        "## Acceptance criteria\n- AC-1: x\n## Acceptance criteria\n- AC-2: y\n",
		"sub heading":         "## Acceptance criteria\n- AC-1: x\n### More\n- AC-2: y\n",
		"no space":            "## Acceptance criteria\n- AC-1:x\n",
		"html comment hides":  "## Acceptance criteria\n- AC-1: x\n<!--\n- AC-2: hidden\n-->\n",
		"comment hides head":  "## Acceptance criteria\n- AC-1: x\n<!-- ## Notes -->\n- AC-2: y\n",
		"html block":          "<div>\n\n## Acceptance criteria\n- AC-1: x\n</div>\n",
		"inline details":      "## Acceptance criteria\n- AC-1: <details><summary>Pay once.</summary>Pay twice.</details>\n",
		"setext heading":      "## Acceptance criteria\n- AC-1: x\n\nNotes\n---\n- AC-2: y\n",
		"thematic break":      "## Acceptance criteria\n- AC-1: x\n\n***\n",
		"lazy continuation":   "## Acceptance criteria\n- AC-1: Reject unauthenticated requests\nexcept requests to /public.\n",
		"prose after blank":   "## Acceptance criteria\n- AC-1: Pays.\n\nThis explains the rules.\n",
		"nbsp blank line":     "## Acceptance criteria\n- AC-1: Reject unauthenticated requests\n \nexcept requests to /public.\n",
		"lone CR":             "## Acceptance criteria\n- AC-1: First.\r- AC-2: Second.\n",
		"quote":               "## Acceptance criteria\n- AC-1: x\n> - AC-2: y\n",
		"nbsp bullet":         "## Acceptance criteria\n- AC-1: x\n- AC-2: y\n",
		"table mention":       "## Acceptance criteria\n- AC-1: x\n\n| AC-2 | y |\n",
		"indented heading":    "## Acceptance criteria\n- AC-1: x\n   ## Notes\n- AC-2: y\n",
		"nested after head":   "## Acceptance criteria\n- AC-1: x\n\n## Notes\n  - AC-2: y\n",
		"stray criterion":     "- AC-2: y\n\n## Acceptance criteria\n- AC-1: x\n",
		"quoted stray":        "> - AC-2: y\n\n## Acceptance criteria\n- AC-1: x\n",
		"list-item fences":    "## Acceptance criteria\n- AC-1: Require authentication.\n  ```\n- AC-2: Require authorization.\n  ```\n- AC-3: Audit requests.\n",
		"indented fence away": "- item\n  ```\n  code\n  ```\n\n## Acceptance criteria\n- AC-1: x\n",
		"huge id":             "## Acceptance criteria\n- AC-99999999999999999999: x\n",
		"sub heading 4":       "## Acceptance criteria\n- AC-1: x\n#### More\n",
	}
	for name, body := range bad {
		if _, _, err := parseCriteria(body); err == nil {
			t.Errorf("%s: accepted %q", name, body)
		}
	}
}

func TestCriteriaDelta(t *testing.T) {
	crit := []SpecCriterion{{1, "Pays once."}, {2, "Retries safely now."}, {4, "Sends a receipt."}}
	d := criteriaDelta(crit, nil)
	if len(d.New) != 3 || d.Unchanged != nil || d.Changed != nil || d.Removed != nil {
		t.Fatalf("first version: %+v", d)
	}
	last := &Implementation{Criteria: []ImplementedCriterion{
		{ID: 1, Text: "Pays once.", Tests: []string{"t/a.js"}},
		{ID: 2, Text: "Retries safely.", Tests: []string{"t/b.js"}},
		{ID: 3, Text: "Logs out.", Tests: []string{"t/c.js"}},
	}}
	d = criteriaDelta(crit, last)
	if !reflect.DeepEqual(d.New, []SpecCriterion{{4, "Sends a receipt."}}) || len(d.Unchanged) != 1 || d.Unchanged[0].ID != 1 ||
		!reflect.DeepEqual(d.Changed, []int{2}) || !reflect.DeepEqual(d.Removed, []int{3}) {
		t.Fatalf("delta: %+v", d)
	}
}

func TestCriteriaIdentityCheck(t *testing.T) {
	id := criteriaIdentity{Latest: map[int]bool{1: true, 2: true}, Retired: map[int]bool{3: true}, High: 3}
	for _, c := range []struct {
		ids []int
		ok  bool
	}{
		{[]int{1, 2}, true},
		{[]int{1, 2, 4}, true}, // next number above the retired 3
		{[]int{1, 9}, true},    // gaps above the mark are fine
		{[]int{1, 3}, false},   // 3 was retired
		{[]int{1, 2, 3}, false},
	} {
		var crit []SpecCriterion
		for _, n := range c.ids {
			crit = append(crit, SpecCriterion{ID: n, Text: "x"})
		}
		if err := id.check(crit); (err == nil) != c.ok {
			t.Errorf("ids %v: err %v, want ok=%v", c.ids, err, c.ok)
		}
	}
	// A number below the mark that no approval had (a skipped one) is not free either.
	id.High = 6
	if err := id.check([]SpecCriterion{{5, "x"}}); err == nil {
		t.Error("a skipped number below the mark was accepted")
	}
}

const twoCriteria = "Pays the cart.\n\n## Acceptance criteria\n- AC-1: Given a cart, when the user pays, then one order is created.\n- AC-2: Given a failed payment, when retried, then no second charge.\n"

func editSpec(t *testing.T, r Repo, feature, old, new string) {
	t.Helper()
	dir := memDir(t, r)
	p := filepath.Join(dir, "wiki", "features", feature, "spec.md")
	text := readFile(t, p)
	if !strings.Contains(text, old) {
		t.Fatalf("spec has no %q", old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(text, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "edit "+feature)
}

func TestApproveRecordsCriteriaAndRetiresNumbers(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	spec, changed, err := ApproveSpec(r, "checkout")
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if !reflect.DeepEqual(spec.Approval.Criteria, []int{1, 2}) || spec.Approval.MaxCriterion != 2 {
		t.Fatalf("approval criteria: %+v", spec.Approval)
	}
	approval := readFile(t, filepath.Join(memDir(t, r), "wiki", "features", "checkout", approvalFile))
	if !strings.Contains(approval, "criteria: [1, 2]") || !strings.Contains(approval, "max_criterion: 2") {
		t.Fatalf("approval file: %s", approval)
	}
	// Remove AC-2 and approve: 2 is retired.
	editSpec(t, r, "checkout", "- AC-2: Given a failed payment, when retried, then no second charge.\n", "")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	// Bringing AC-2 back is a reuse; AC-3 is fine.
	editSpec(t, r, "checkout", "then one order is created.\n", "then one order is created.\n- AC-2: Reused.\n")
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "AC-3") {
		t.Fatalf("reused number approved: %v", err)
	}
	errs, _, err := LintMemory(r)
	if err != nil || !strings.Contains(strings.Join(errs, "\n"), "retired") {
		t.Fatalf("lint must report the reuse: %v %v", errs, err)
	}
	// Lowering max_criterion and dropping criteria in a committed hand edit frees nothing:
	// identity comes from memory's history.
	dir := memDir(t, r)
	ap := filepath.Join(dir, "wiki", "features", "checkout", approvalFile)
	original := readFile(t, ap)
	for _, forged := range []string{
		strings.Replace(original, "max_criterion: 2", "max_criterion: 1", 1),                              // lower the mark
		strings.Replace(original, "criteria: [1]", "criteria: [1, 2]", 1),                                 // vouch for 2
		strings.Replace(strings.Replace(original, "criteria: [1]\n", "", 1), "max_criterion: 2\n", "", 1), // drop both
	} {
		if forged == original {
			t.Fatalf("forgery did not apply: %s", original)
		}
		if err := os.WriteFile(ap, []byte(forged), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "forge approval")
		if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "AC-3") {
			t.Fatalf("a forged approval file freed a retired number: %v\n%s", err, forged)
		}
	}
	if err := os.WriteFile(ap, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "restore approval")
	editSpec(t, r, "checkout", "- AC-2: Reused.", "- AC-3: New.")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	snap, _ := LoadMemory(r)
	spec, _ = snap.Spec("checkout")
	if !reflect.DeepEqual(spec.Approval.Criteria, []int{1, 3}) || spec.Approval.MaxCriterion != 3 {
		t.Fatalf("approval after retire: %+v", spec.Approval)
	}
}

// Invalid criteria fail approval even when the spec is already approved (the shortcut runs later).
func TestApproveChecksCriteriaBeforeShortcut(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", "Pays.\n\n## Acceptance criteria\n- AC-1: Pays once.\n")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	// Forge an approval for a spec whose criteria are invalid.
	editSpec(t, r, "checkout", "- AC-1: Pays once.", "- AC-1: Pays once.\n- AC-1: Twice.")
	snap, _ := LoadMemory(r)
	spec, _ := snap.Spec("checkout")
	a := *spec.Approval
	a.Content = approvalDigest(spec.Title, spec.Paths, spec.Body)
	dir := memDir(t, r)
	writeApprovalFile(t, dir, "checkout", a)
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("invalid criteria passed the shortcut: %v", err)
	}
}

func writeApprovalFile(t *testing.T, dir, feature string, a SpecApproval) {
	t.Helper()
	text := "version: " + a.Version + "\ncontent_sha256: " + a.Content + "\ndate: \"" + a.Date + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "wiki", "features", feature, approvalFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "approval")
}

func TestRecordWritesImplementation(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	spec, _, err := ApproveSpec(r, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	crit, _, _ := spec.Criteria()
	good := Implementation{Outcome: "checkout-v0_1_0", Approval: spec.Approval.Content, Code: fakeSHA('a')}
	for _, c := range crit {
		good.Criteria = append(good.Criteria, ImplementedCriterion{ID: c.ID, Text: c.Text, Tests: []string{"tests/acceptance/checkout-v0_1_0/ac.test.js"}})
	}
	dir := memDir(t, r)
	head := mustGit(t, dir, "rev-parse", "HEAD")
	p := PendingRecord{OutcomeID: "checkout-v0_1_0", Feature: "checkout", Change: "minor", Note: "Checkout.", Commit: fakeSHA('a')}

	// Mismatches are refused and leave memory untouched.
	for name, mutate := range map[string]func(*Implementation){
		"outcome":      func(im *Implementation) { im.Outcome = "other" },
		"commit":       func(im *Implementation) { im.Code = fakeSHA('b') },
		"approval":     func(im *Implementation) { im.Approval = strings.Repeat("0", 64) },
		"text":         func(im *Implementation) { im.Criteria[0].Text = "Something else." },
		"missing":      func(im *Implementation) { im.Criteria = im.Criteria[:1] },
		"no tests":     func(im *Implementation) { im.Criteria[0].Tests = nil },
		"escape tests": func(im *Implementation) { im.Criteria[0].Tests = []string{"../x.js"} },
	} {
		im := good
		im.Criteria = append([]ImplementedCriterion{}, good.Criteria...)
		mutate(&im)
		bad := p
		bad.Implementation = &im
		if _, _, err := RecordChange(r, bad, ""); err == nil {
			t.Errorf("%s: mismatched implementation recorded", name)
		}
		if got := mustGit(t, dir, "rev-parse", "HEAD"); got != head {
			t.Fatalf("%s: memory changed", name)
		}
	}

	p.Implementation = &good
	if _, _, err := RecordChange(r, p, ""); err != nil {
		t.Fatal(err)
	}
	if files := mustGit(t, dir, "show", "--name-only", "--format=", "HEAD"); !strings.Contains(files, implementedFile) || !strings.Contains(files, "spec.md") {
		t.Fatalf("implementation not in the record commit: %s", files)
	}
	snap, err := LoadMemory(r)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ = snap.Spec("checkout")
	im, ok := spec.LastImplementation()
	if !ok || im.Code != fakeSHA('a') || im.MaxCriterion != 2 || len(im.Criteria) != 2 || !spec.Approved() {
		t.Fatalf("implementation not read back: %+v %v", im, spec.Approved())
	}
	// A repeated record is idempotent and does not append twice.
	if _, _, err := RecordChange(r, p, ""); err != nil {
		t.Fatal(err)
	}
	snap, _ = LoadMemory(r)
	if spec, _ = snap.Spec("checkout"); len(spec.Implemented) != 1 {
		t.Fatalf("duplicate implementation: %d", len(spec.Implemented))
	}
	if err := memoryClean(dir); err != nil {
		t.Fatal(err)
	}
}

// An ID an implementation had stays retired once a later approval removes it.
func TestImplementationDoesNotReviveRetiredIDs(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	spec, _, err := ApproveSpec(r, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	recordImplementation(t, r, spec, "checkout-v0_1_0", fakeSHA('a'))
	editSpec(t, r, "checkout", "- AC-2: Given a failed payment, when retried, then no second charge.\n", "")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	editSpec(t, r, "checkout", "then one order is created.\n", "then one order is created.\n- AC-2: Different text.\n")
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "AC-3") {
		t.Fatalf("the implementation revived retired AC-2: %v", err)
	}
	// Truncating implemented.yaml in a commit does not lower the mark either.
	editSpec(t, r, "checkout", "- AC-2: Different text.\n", "")
	dir := memDir(t, r)
	if err := os.Remove(filepath.Join(dir, "wiki", "features", "checkout", implementedFile)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "drop implementations")
	editSpec(t, r, "checkout", "then one order is created.\n", "then one order is created.\n- AC-2: Again.\n")
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "AC-3") {
		t.Fatalf("dropping implemented.yaml freed AC-2: %v", err)
	}
}

func recordImplementation(t *testing.T, r Repo, spec Spec, outcome, sha string) {
	t.Helper()
	crit, _, err := spec.Criteria()
	if err != nil {
		t.Fatal(err)
	}
	im := Implementation{Outcome: outcome, Approval: spec.Approval.Content, Code: sha}
	for _, c := range crit {
		im.Criteria = append(im.Criteria, ImplementedCriterion{ID: c.ID, Text: c.Text, Tests: []string{"tests/acceptance/" + outcome + "/ac.test.js"}})
	}
	p := PendingRecord{OutcomeID: outcome, Feature: spec.Feature, Change: "minor", Note: "Done.", Commit: sha, Implementation: &im}
	if _, _, err := RecordChange(r, p, ""); err != nil {
		t.Fatal(err)
	}
}

// An approved spec without criteria cannot record an implementation, and does not panic.
// Restoring an old spec and its approval in a new commit does not bring a retired ID back,
// and history is read byte for byte (trailing spaces included).
func TestRestoredApprovalKeepsNumbersRetired(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	// The file ends in blank space that git() would trim; history must read it byte for byte.
	dir := memDir(t, r)
	sp := filepath.Join(dir, "wiki", "features", "checkout", "spec.md")
	if err := os.WriteFile(sp, []byte(readFile(t, sp)+"\n   "), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "trailing space")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	ap := filepath.Join(dir, "wiki", "features", "checkout", approvalFile)
	oldSpec, oldApproval := readFile(t, sp), readFile(t, ap)
	editSpec(t, r, "checkout", "- AC-2: Given a failed payment, when retried, then no second charge.\n", "")
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	for p, text := range map[string]string{sp: oldSpec, ap: oldApproval} {
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "restore the old pair")
	if _, _, err := ApproveSpec(r, "checkout"); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("a restored approval revived AC-2: %v", err)
	}
	if errs, _, _ := LintMemory(r); !strings.Contains(strings.Join(errs, "\n"), "AC-2 was removed") {
		t.Fatalf("lint must report the revived AC-2: %v", errs)
	}
}

// aeh's own changelog line must not turn valid criteria invalid.
func TestRecordRefusesNoteThatBreaksCriteria(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", twoCriteria)
	if _, _, err := ApproveSpec(r, "checkout"); err != nil {
		t.Fatal(err)
	}
	dir := memDir(t, r)
	head := mustGit(t, dir, "rev-parse", "HEAD")
	p := PendingRecord{OutcomeID: "OUT-1", Feature: "checkout", Change: "patch", Note: "Hide <!-- this.", Commit: fakeSHA('c')}
	if _, _, err := RecordChange(r, p, ""); err == nil {
		t.Fatal("a note with an HTML comment was recorded")
	}
	if got := mustGit(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("memory changed")
	}
}

func TestImplementationNeedsCriteria(t *testing.T) {
	r := newMemoryProject(t)
	if _, err := CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
		t.Fatal(err)
	}
	describeSpec(t, r, "checkout", "Pays once.")
	spec, _, err := ApproveSpec(r, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	im := Implementation{Outcome: "checkout-v0_1_0", Approval: spec.Approval.Content, Code: fakeSHA('a')}
	p := PendingRecord{OutcomeID: im.Outcome, Feature: "checkout", Change: "minor", Note: "Done.", Commit: im.Code, Implementation: &im}
	if _, _, err := RecordChange(r, p, ""); err == nil || !strings.Contains(err.Error(), "no acceptance criteria") {
		t.Fatalf("recorded an implementation without criteria: %v", err)
	}
}

func TestInvalidImplementationFileIsRejected(t *testing.T) {
	sha := fakeSHA('a')
	digest := strings.Repeat("a", 64)
	entry := func(extra string) string {
		return "- outcome: checkout-v0_1_0\n  approval_content_sha256: " + digest + "\n  code_sha: " + sha +
			"\n  criteria:\n    - id: 1\n      text: Pays once.\n      tests: [t/a.js]\n  max_criterion: 1\n" + extra
	}
	cases := map[string]string{
		"unknown field":  entry("  extra: 1\n"),
		"two documents":  entry("---\n[]\n"),
		"anchor":         "- &a\n  outcome: x\n",
		"bad sha":        strings.Replace(entry(""), sha, "abc", 1),
		"above max":      strings.Replace(entry(""), "max_criterion: 1", "max_criterion: 0", 1),
		"unsorted":       strings.Replace(entry(""), "    - id: 1\n      text: Pays once.\n      tests: [t/a.js]\n", "    - id: 2\n      text: A.\n      tests: [t/a.js]\n    - id: 1\n      text: B.\n      tests: [t/a.js]\n", 1),
		"mark goes down": strings.Replace(entry(""), "max_criterion: 1", "max_criterion: 2", 1) + entry(""),
		"text spacing":   strings.Replace(entry(""), "Pays once.", "Pays  once.", 1),
		"absolute test":  strings.Replace(entry(""), "t/a.js", "/etc/x", 1),
	}
	dir := t.TempDir()
	p := filepath.Join(dir, implementedFile)
	if err := os.WriteFile(p, []byte(entry("")), 0o644); err != nil {
		t.Fatal(err)
	}
	if list, err := readImplementations(p); err != nil || len(list) != 1 {
		t.Fatalf("valid file rejected: %v", err)
	}
	for name, text := range cases {
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readImplementations(p); err == nil {
			t.Errorf("%s: accepted\n%s", name, text)
		}
	}
}
