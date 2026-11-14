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
	criterionLine = regexp.MustCompile(`^- AC-([1-9][0-9]{0,5}): (.*)$`)
	observedLine  = regexp.MustCompile(`^- AC-[0-9]+ \(observed\):`)
	listItem      = regexp.MustCompile(`^(?:[-*+]|[0-9]+[.)])(?:\s|$)`)
)

// parseCriteria reads the "## Acceptance criteria" section of a spec body. present is false
// when the section is absent: such a spec can be approved, not planned. Inside the section,
// outside fenced code, each criterion is one line "- AC-<n>: <text>" with n a positive integer
// without leading zeros. Any other list item, an indented line (a hidden continuation), a
// duplicate ID or an empty section is an error, so what aeh plans is exactly what the owner
// reads. Prose lines and fenced examples are allowed.
func parseCriteria(body string) (crit []SpecCriterion, present bool, err error) {
	var f codeFence
	in, sections := false, 0
	seen := map[int]bool{}
	for i, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if f.step(raw) {
			continue
		}
		line := strings.TrimRight(raw, " \t")
		if strings.HasPrefix(line, "## ") || line == "##" {
			in = line == criteriaHeading
			if in {
				sections++
			}
			continue
		}
		if strings.HasPrefix(line, "# ") || line == "#" {
			in = false // a first-level heading ends the section too
		}
		if !in || strings.TrimSpace(line) == "" {
			continue
		}
		at := fmt.Sprintf("acceptance criteria line %d", i+1)
		if line != strings.TrimLeft(line, " \t") {
			return nil, true, fmt.Errorf("%s: indented lines are not allowed; write each criterion on one line", at)
		}
		if observedLine.MatchString(line) {
			return nil, true, fmt.Errorf("%s: observed criteria are reserved for adopting existing systems", at)
		}
		m := criterionLine.FindStringSubmatch(line)
		if m == nil {
			if listItem.MatchString(line) {
				return nil, true, fmt.Errorf(`%s: a list item must read "- AC-<n>: <text>" (n from 1, no leading zeros)`, at)
			}
			if strings.HasPrefix(line, "#") {
				return nil, true, fmt.Errorf("%s: headings inside the section are not allowed", at)
			}
			continue // prose
		}
		id, _ := strconv.Atoi(m[1])
		text := strings.Join(strings.Fields(m[2]), " ")
		if text == "" {
			return nil, true, fmt.Errorf("%s: AC-%d has no text", at, id)
		}
		if seen[id] {
			return nil, true, fmt.Errorf("%s: AC-%d appears twice", at, id)
		}
		seen[id] = true
		crit = append(crit, SpecCriterion{ID: id, Text: text})
	}
	switch {
	case sections > 1:
		return nil, true, fmt.Errorf("the spec has %d %q sections; keep one", sections, criteriaHeading)
	case sections == 1 && len(crit) == 0:
		return nil, true, fmt.Errorf("the %q section has no criteria", criteriaHeading)
	}
	return crit, sections == 1, nil
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

// criteriaHighWater is the highest criterion ID ever approved or implemented for the feature.
// A criterion that was removed keeps its number retired: a new criterion must go above it.
func criteriaHighWater(s Spec) int {
	high := 0
	if s.Approval != nil {
		high = s.Approval.MaxCriterion
	}
	if im, ok := s.LastImplementation(); ok && im.MaxCriterion > high {
		high = im.MaxCriterion
	}
	return high
}

// checkCriteriaIdentity rejects criterion IDs that reuse a retired number or skip back below
// the high-water mark. An ID the last approval or implementation already had keeps its place,
// even with new text; any other ID must be above every ID used before.
func checkCriteriaIdentity(s Spec, crit []SpecCriterion) error {
	known := map[int]bool{}
	if s.Approval != nil {
		for _, id := range s.Approval.Criteria {
			known[id] = true
		}
	}
	if im, ok := s.LastImplementation(); ok {
		for _, c := range im.Criteria {
			known[c.ID] = true
		}
	}
	high := criteriaHighWater(s)
	for _, c := range crit {
		if !known[c.ID] && c.ID <= high {
			return fmt.Errorf("AC-%d reuses a retired or skipped number; new criteria start at AC-%d", c.ID, high+1)
		}
	}
	return nil
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
