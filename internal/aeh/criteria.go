package aeh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// criteriaHeading opens the only section aeh plans from.
const criteriaHeading = "## Acceptance criteria"

// SpecCriterion is one acceptance criterion of a spec: "- AC-<ID>: <Text>". Text is normalized
// (runs of white space become one space) so versions compare by meaning, not layout.
type SpecCriterion struct {
	ID   int    `yaml:"id"`
	Text string `yaml:"text"`
}

var (
	criterionLine = regexp.MustCompile(`^- AC-([1-9][0-9]*): (.*)$`)
	observedLine  = regexp.MustCompile(`^- AC-[0-9]+ \(observed\):`)
	listItem      = regexp.MustCompile(`^(?:[-*+]|[0-9]+[.)])(?:\s|$)`)
	mentionsAC    = regexp.MustCompile(`(?i)\bAC-[0-9]`)
	htmlLine      = regexp.MustCompile(`^ {0,3}<[A-Za-z/!?]`)
)

// maxCriterionID bounds criterion numbers so they stay plain integers everywhere.
const maxCriterionID = 1_000_000

// headingOf returns an ATX heading line as "## Text" (up to three leading spaces and closing
// hashes removed), or "" when the line is not a heading.
func headingOf(line string) string {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || !strings.HasPrefix(t, "#") {
		return ""
	}
	n := len(t) - len(strings.TrimLeft(t, "#"))
	if n > 6 || (len(t) > n && t[n] != ' ' && t[n] != '\t') {
		return ""
	}
	text := strings.TrimSpace(t[n:])
	if strings.Trim(text, "#") == "" {
		text = ""
	} else if i := strings.LastIndexAny(text, " \t"); i >= 0 && strings.Trim(text[i+1:], "#") == "" {
		text = strings.TrimSpace(text[:i])
	}
	return strings.Repeat("#", n) + " " + text
}

// isBreakLine reports a setext underline or thematic break: only one of - = * _ and blanks.
func isBreakLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || strings.Trim(t, "-=*_ \t") != "" {
		return false
	}
	c := strings.TrimLeft(t, " \t")[0]
	return strings.Trim(t, string(c)+" \t") == ""
}

// parseCriteria reads the "## Acceptance criteria" section of a spec body. present is false
// when the section is absent: such a spec can be approved, not planned. Inside the section,
// outside fenced code, each criterion is one line "- AC-<n>: <text>" with n a positive integer
// without leading zeros. What aeh plans must be exactly what the owner reads, so anything that
// Markdown could render differently is an error: other list items, indented or lazy
// continuation lines, quotes, setext headings and thematic breaks, sub-headings, other lines
// that mention a criterion, and, anywhere in a spec that has the section, criterion lines
// outside it and raw HTML or HTML comments (they can hide text or headings). Prose separated by blank lines and fenced
// examples are allowed.
func parseCriteria(body string) (crit []SpecCriterion, present bool, err error) {
	var f codeFence
	in, sections, htmlAt, strayAt := false, 0, 0, 0
	afterCriterion := false
	seen := map[int]bool{}
	fail := func(n int, format string, a ...any) ([]SpecCriterion, bool, error) {
		return nil, true, fmt.Errorf("acceptance criteria line %d: "+format, append([]any{n}, a...)...)
	}
	for i, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		n := i + 1
		if f.step(raw) {
			afterCriterion = false
			continue
		}
		line := strings.TrimRight(raw, " \t")
		if htmlAt == 0 && (htmlLine.MatchString(line) || strings.Contains(line, "<!--") || strings.Contains(line, "-->")) {
			htmlAt = n
		}
		if h := headingOf(line); h != "" {
			wasIn := in
			if strings.HasPrefix(h, "## ") || strings.HasPrefix(h, "# ") {
				in = h == criteriaHeading
				if in {
					sections++
				}
			}
			if wasIn && !strings.HasPrefix(h, "# ") && !strings.HasPrefix(h, "## ") {
				return fail(n, "sub-headings inside the section are not allowed")
			}
			afterCriterion = false
			continue
		}
		if !in {
			if strayAt == 0 && (criterionLine.MatchString(line) || observedLine.MatchString(line)) {
				strayAt = n
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			afterCriterion = false
			continue
		}
		if line != strings.TrimLeft(line, " \t") {
			return fail(n, "indented lines are not allowed; write each criterion on one line")
		}
		if isBreakLine(line) {
			return fail(n, "setext headings and thematic breaks are not allowed in the section")
		}
		if strings.HasPrefix(line, ">") {
			return fail(n, "quotes are not allowed in the section")
		}
		if observedLine.MatchString(line) {
			return fail(n, "observed criteria are reserved for adopting existing systems")
		}
		m := criterionLine.FindStringSubmatch(line)
		if m == nil {
			if listItem.MatchString(line) {
				return fail(n, `a list item must read "- AC-<n>: <text>" (n from 1, no leading zeros)`)
			}
			if mentionsAC.MatchString(line) {
				return fail(n, `a line that mentions a criterion must read "- AC-<n>: <text>"`)
			}
			if afterCriterion {
				return fail(n, "this line would continue the criterion above; write each criterion on one line and separate prose with a blank line")
			}
			continue // prose
		}
		id, cerr := strconv.Atoi(m[1])
		if cerr != nil || id > maxCriterionID {
			return fail(n, "AC-%s is above the limit of %d", m[1], maxCriterionID)
		}
		text := strings.Join(strings.Fields(m[2]), " ")
		if text == "" {
			return fail(n, "AC-%d has no text", id)
		}
		if seen[id] {
			return fail(n, "AC-%d appears twice", id)
		}
		seen[id] = true
		crit = append(crit, SpecCriterion{ID: id, Text: text})
		afterCriterion = true
	}
	switch {
	case sections == 0:
		return nil, false, nil
	case sections > 1:
		return nil, true, fmt.Errorf("the spec has %d %q sections; keep one", sections, criteriaHeading)
	case strayAt > 0:
		return fail(strayAt, "a criterion outside the %q section would not be planned; move it into the section", criteriaHeading)
	case htmlAt > 0:
		return fail(htmlAt, "raw HTML and HTML comments are not allowed in a spec with acceptance criteria; they can hide text")
	case len(crit) == 0:
		return nil, true, fmt.Errorf("the %q section has no criteria", criteriaHeading)
	}
	return crit, true, nil
}

// Implementation records that one approved content of a spec was implemented and accepted:
// which outcome, on which exact commit, and the criteria it covered with their sealed tests.
// Only the runner writes it, in the same memory commit as the accepted change's record.
type Implementation struct {
	Outcome      string                 `yaml:"outcome"`
	Approval     string                 `yaml:"approval_content_sha256"` // digest of the approved spec that was planned
	Code         string                 `yaml:"code_sha"`                // full SHA accepted on aeh/<outcome>
	Criteria     []ImplementedCriterion `yaml:"criteria"`                // every criterion of that approval, sorted by ID
	MaxCriterion int                    `yaml:"max_criterion"`           // highest criterion ID ever approved, retired ones included
}

// ImplementedCriterion is one criterion as implemented, with the sealed tests that check it.
type ImplementedCriterion struct {
	ID    int      `yaml:"id"`
	Text  string   `yaml:"text"`
	Tests []string `yaml:"tests"`
}

const implementedFile = "implemented.yaml"

func implementedRel(feature string) string {
	return "wiki/features/" + feature + "/" + implementedFile
}

// readImplementations reads a feature's implementation records, oldest first. A missing file
// means none.
func readImplementations(p string) ([]Implementation, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := checkConfigYAML(b); err != nil {
		return nil, fmt.Errorf("%s: %w", implementedFile, err)
	}
	var list []Implementation
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: %w", implementedFile, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: must hold exactly one YAML document", implementedFile)
	}
	high := 0
	for i, im := range list {
		if err := im.validate(); err != nil {
			return nil, fmt.Errorf("%s: entry %d: %w", implementedFile, i+1, err)
		}
		if im.MaxCriterion < high {
			return nil, fmt.Errorf("%s: entry %d: max_criterion went down from %d to %d", implementedFile, i+1, high, im.MaxCriterion)
		}
		high = im.MaxCriterion
	}
	return list, nil
}

func (im Implementation) validate() error {
	if !validID(im.Outcome) {
		return fmt.Errorf("invalid outcome %q", im.Outcome)
	}
	if !isSHA256Hex(im.Approval) {
		return errors.New("approval_content_sha256 must be a sha256 in hex")
	}
	if !isFullSHA(im.Code) {
		return fmt.Errorf("code_sha %q is not a full commit SHA", im.Code)
	}
	if len(im.Criteria) == 0 {
		return errors.New("no criteria")
	}
	for i, c := range im.Criteria {
		if c.ID < 1 || (i > 0 && c.ID <= im.Criteria[i-1].ID) {
			return errors.New("criteria IDs must be positive, unique and sorted")
		}
		if c.ID > im.MaxCriterion {
			return fmt.Errorf("AC-%d is above max_criterion %d", c.ID, im.MaxCriterion)
		}
		if c.Text == "" || c.Text != strings.Join(strings.Fields(c.Text), " ") {
			return fmt.Errorf("AC-%d: text must be non-empty and normalized", c.ID)
		}
		if len(c.Tests) == 0 {
			return fmt.Errorf("AC-%d has no tests", c.ID)
		}
		for _, t := range c.Tests {
			if err := checkRelPath(t); err != nil {
				return fmt.Errorf("AC-%d: test %q: %w", c.ID, t, err)
			}
		}
	}
	return nil
}

// checkRelPath accepts a clean, relative, forward-slash file path inside the repository.
func checkRelPath(p string) error {
	switch {
	case p == "" || strings.ContainsAny(p, "\\*?[\x00") || strings.HasPrefix(p, "/") || strings.Contains(p, ":"):
		return errors.New("must be a plain relative path with forward slashes")
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return errors.New("must be clean: no empty, . or .. segments")
		}
	}
	return nil
}

func isSHA256Hex(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// LastImplementation returns the feature's most recent implementation record, if any.
func (s Spec) LastImplementation() (Implementation, bool) {
	if len(s.Implemented) == 0 {
		return Implementation{}, false
	}
	return s.Implemented[len(s.Implemented)-1], true
}

// criteriaIdentity is what memory's history says about a feature's criterion numbers.
type criteriaIdentity struct {
	Known map[int]bool // IDs of the latest genuine approval: they keep their place
	High  int          // highest ID ever approved or implemented, retired ones included
}

// loadCriteriaIdentity reads the feature's whole memory history, not only the current files:
// lowering max_criterion or truncating implemented.yaml in a later commit does not free a
// number. Known comes from the newest commit whose approval.yaml matches the spec next to it,
// so a hand-edited approval file cannot vouch for an ID either. Implementations only raise
// High: an ID they had and a later approval removed stays retired.
func loadCriteriaIdentity(dir string, s Spec) (criteriaIdentity, error) {
	id := criteriaIdentity{Known: map[int]bool{}}
	raise := func(n int) {
		if n > id.High {
			id.High = n
		}
	}
	addCurrent := func(a *SpecApproval, impls []Implementation) {
		if a != nil {
			raise(a.MaxCriterion)
			for _, n := range a.Criteria {
				raise(n)
			}
		}
		for _, im := range impls {
			raise(im.MaxCriterion)
			for _, c := range im.Criteria {
				raise(c.ID)
			}
		}
	}
	addCurrent(s.Approval, s.Implemented)
	aRel, iRel, sRel := approvalRel(s.Feature), implementedRel(s.Feature), specRel(s.Feature)
	log, err := git(dir, "log", "--format=%H", "--", aRel, iRel)
	if err != nil {
		return id, fmt.Errorf("criteria history of %s: %w", s.Feature, err)
	}
	knownSet := false
	for _, sha := range strings.Fields(log) {
		var a *SpecApproval
		if b, err := git(dir, "show", sha+":"+aRel); err == nil {
			var v SpecApproval
			if yaml.Unmarshal([]byte(b), &v) == nil {
				a = &v
			}
		}
		var impls []Implementation
		if b, err := git(dir, "show", sha+":"+iRel); err == nil {
			_ = yaml.Unmarshal([]byte(b), &impls)
		}
		addCurrent(a, impls)
		if a == nil {
			continue
		}
		text, err := git(dir, "show", sha+":"+sRel)
		if err != nil {
			continue
		}
		sp, err := parseSpec(text + "\n")
		if err != nil {
			continue
		}
		crit, _, err := sp.Criteria()
		if err != nil {
			continue
		}
		genuine := approvalDigest(sp.Title, sp.Paths, sp.Body) == a.Content
		for _, c := range crit {
			if genuine {
				raise(c.ID) // a genuinely approved content: its IDs were approved
			}
			if genuine && !knownSet {
				id.Known[c.ID] = true
			}
		}
		if genuine {
			knownSet = true
		}
	}
	return id, nil
}

// check rejects criterion IDs that reuse a retired number or skip back below the mark. An ID
// the latest approval had keeps its place, even with new text; any other ID must be above
// every ID used before.
func (id criteriaIdentity) check(crit []SpecCriterion) error {
	for _, c := range crit {
		if !id.Known[c.ID] && c.ID <= id.High {
			return fmt.Errorf("AC-%d reuses a retired or skipped number; new criteria start at AC-%d", c.ID, id.High+1)
		}
	}
	return nil
}

// checkSpecCriteria parses a spec's criteria and checks their numbers against history.
func checkSpecCriteria(dir string, s Spec) ([]SpecCriterion, criteriaIdentity, error) {
	crit, _, err := s.Criteria()
	if err != nil {
		return nil, criteriaIdentity{}, err
	}
	id, err := loadCriteriaIdentity(dir, s)
	if err != nil {
		return nil, id, err
	}
	return crit, id, id.check(crit)
}

// CriteriaDelta compares the approved criteria with the last implementation. New criteria are
// what a plan builds; unchanged ones keep their sealed tests; changed and removed ones need a
// test revision, which stage 3 does not do.
type CriteriaDelta struct {
	New       []SpecCriterion
	Unchanged []ImplementedCriterion
	Changed   []int
	Removed   []int
}

func criteriaDelta(crit []SpecCriterion, last *Implementation) CriteriaDelta {
	var d CriteriaDelta
	prev := map[int]ImplementedCriterion{}
	if last != nil {
		for _, c := range last.Criteria {
			prev[c.ID] = c
		}
	}
	now := map[int]bool{}
	for _, c := range crit {
		now[c.ID] = true
		p, ok := prev[c.ID]
		switch {
		case !ok:
			d.New = append(d.New, c)
		case p.Text == c.Text:
			d.Unchanged = append(d.Unchanged, p)
		default:
			d.Changed = append(d.Changed, c.ID)
		}
	}
	for id := range prev {
		if !now[id] {
			d.Removed = append(d.Removed, id)
		}
	}
	sort.Ints(d.Removed)
	return d
}
