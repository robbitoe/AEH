package aeh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RetiredTest records a runner-authorized retirement, including the original sealed hash.
// Implementation entries record only their own retirements; seals carry cumulative absences.
type RetiredTest struct {
	Path   string `yaml:"path" json:"path"`
	SHA256 string `yaml:"sha256" json:"sha256"`
	Reason string `yaml:"reason" json:"reason"`
}

func validateRetiredTests(tests []RetiredTest) error {
	seen := map[string]bool{}
	for _, t := range tests {
		if err := checkPlanTestPath(t.Path); err != nil {
			return fmt.Errorf("retired test %q: %w", t.Path, err)
		}
		if matchAny(t.Path, []string{"aeh/", ".aeh/"}) || agentInstructionFile(t.Path) || gitControlFile(t.Path) {
			return fmt.Errorf("retired test %s: protected paths cannot be retired", t.Path)
		}
		for _, segment := range strings.Split(t.Path, "/") {
			if strings.EqualFold(segment, ".git") {
				return fmt.Errorf("retired test %s: Git control path cannot be retired", t.Path)
			}
		}
		if seen[t.Path] || !isSHA256Hex(t.SHA256) {
			return fmt.Errorf("retired test %s: duplicate path or invalid sha256", t.Path)
		}
		seen[t.Path] = true
		switch t.Reason {
		case "changed", "removed", "closure of a shared file", "relocated":
		default:
			return fmt.Errorf("retired test %s: invalid reason %q", t.Path, t.Reason)
		}
	}
	return nil
}

// redCriteria are what the implementation task builds: new criteria and changed ones that
// are not clarifications. Their tests must fail on the base.
func (d CriteriaDelta) redCriteria() []SpecCriterion {
	crit := append([]SpecCriterion(nil), d.New...)
	for _, c := range d.Changes {
		if !c.Clarified {
			crit = append(crit, SpecCriterion{ID: c.ID, Text: c.NewText})
		}
	}
	return crit
}

// greenIDs are the criteria whose new tests must pass on the base: clarifications and
// recovery of unchanged criteria in retired files.
func (d CriteriaDelta) greenIDs() map[string]bool {
	green := map[string]bool{}
	for _, c := range d.Changes {
		if c.Clarified {
			green[fmt.Sprintf("AC-%d", c.ID)] = true
		}
	}
	for _, c := range d.Recovery {
		green[fmt.Sprintf("AC-%d", c.ID)] = true
	}
	for _, c := range d.Relocated {
		green[fmt.Sprintf("AC-%d", c.ID)] = true
	}
	return green
}

// clarifiedIDs are the changed criteria approved as clarifications, sorted.
func (d CriteriaDelta) clarifiedIDs() []int {
	var ids []int
	for _, c := range d.Changes {
		if c.Clarified {
			ids = append(ids, c.ID)
		}
	}
	sort.Ints(ids)
	return ids
}

func (d CriteriaDelta) coverageCriteria() []SpecCriterion {
	crit := d.redCriteria()
	seen := map[int]bool{}
	for _, c := range crit {
		seen[c.ID] = true
	}
	for _, c := range d.Changes {
		if c.Clarified {
			crit = append(crit, SpecCriterion{ID: c.ID, Text: c.NewText})
			seen[c.ID] = true
		}
	}
	for _, c := range d.Relocated {
		if !seen[c.ID] {
			crit = append(crit, SpecCriterion{ID: c.ID, Text: c.Text})
			seen[c.ID] = true
		}
	}
	for _, c := range d.Recovery {
		if !seen[c.ID] {
			crit = append(crit, SpecCriterion{ID: c.ID, Text: c.Text})
			seen[c.ID] = true
		}
	}
	return crit
}

// noImplementation reports a delta with nothing to build: only removed criteria,
// clarifications or recovery. Its plan has no task and is recorded when published.
func (d CriteriaDelta) noImplementation() bool {
	return len(d.redCriteria()) == 0
}

// applyClarifications marks the changed criteria the approval clarified. Each clarification
// must match a change of this delta with the same old and new texts; otherwise the approval
// was given for another delta and must be repeated.
func applyClarifications(d *CriteriaDelta, a *SpecApproval) error {
	if a == nil {
		return nil
	}
	for _, cl := range a.Clarifications {
		found := false
		for i := range d.Changes {
			c := &d.Changes[i]
			if c.ID == cl.ID && criterionDigest(c.OldText) == cl.Old && criterionDigest(c.NewText) == cl.New {
				c.Clarified, found = true, true
			}
		}
		if !found {
			return fmt.Errorf("the clarification of AC-%d was approved for another change of its text; approve again", cl.ID)
		}
	}
	return nil
}

// fileKinds returns true for green recovery and false for red implementation tests.
func fileKinds(d CriteriaDelta, plan PlanFile) (map[string]bool, error) {
	green := d.greenIDs()
	kinds := map[string]bool{}
	for _, id := range sortedTraceIDs(plan.Trace) {
		for _, file := range plan.Trace[id] {
			if prior, ok := kinds[file]; ok && prior != green[id] {
				return nil, fmt.Errorf("plan: file %q traces both red and green criteria; use separate files for recovery", file)
			}
			kinds[file] = green[id]
		}
	}
	return kinds, nil
}

// implementedCriteria lists, for each criterion of the planned spec, the tests that cover it
// after the plan: its surviving sealed tests, plus the plan's new files traced to it.
func implementedCriteria(outcome string, crit []SpecCriterion, d CriteriaDelta, absences []string, trace PlanFile, dir string) ([]ImplementedCriterion, error) {
	crit = append([]SpecCriterion(nil), crit...)
	sort.Slice(crit, func(i, j int) bool { return crit[i].ID < crit[j].ID })
	unchanged := map[int]ImplementedCriterion{}
	for _, c := range d.Unchanged {
		unchanged[c.ID] = c
	}
	var out []ImplementedCriterion
	for _, c := range crit {
		var tests []string
		for _, file := range unchanged[c.ID].Tests {
			if !containsPath(absences, file) {
				tests = append(tests, file)
			}
		}
		for _, file := range trace.Trace[fmt.Sprintf("AC-%d", c.ID)] {
			tests = append(tests, dir+file)
		}
		if len(tests) == 0 {
			return nil, fmt.Errorf("plan %s has no tests for AC-%d", outcome, c.ID)
		}
		out = append(out, ImplementedCriterion{ID: c.ID, Text: c.Text, Tests: tests})
	}
	return out, nil
}

// equivalenceLine is QA's statement about one clarification.
var equivalenceLine = regexp.MustCompile(`^EQUIVALENT AC-([1-9][0-9]*): (yes|no)$`)

// parseEquivalence reads QA's "EQUIVALENT AC-<n>: yes|no" lines: exactly one per clarified
// criterion, none for others, nothing malformed. It returns the criteria QA answered no.
func parseEquivalence(out string, ids []int) ([]string, error) {
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	got := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "EQUIVALENT") {
			continue
		}
		m := equivalenceLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("malformed equivalence line %q; write EQUIVALENT AC-<n>: yes or no", tail(line, 80))
		}
		id, _ := strconv.Atoi(m[1])
		switch {
		case !want[id]:
			return nil, fmt.Errorf("AC-%d is not a clarification of this plan", id)
		case got[id] != "":
			return nil, fmt.Errorf("AC-%d has more than one equivalence line", id)
		}
		got[id] = m[2]
	}
	var no []string
	for _, id := range ids {
		switch got[id] {
		case "":
			return nil, fmt.Errorf("AC-%d has no equivalence line", id)
		case "no":
			no = append(no, fmt.Sprintf("AC-%d", id))
		}
	}
	return no, nil
}

// revisionSummary names what a plan without implementation does.
func revisionSummary(d CriteriaDelta) string {
	var parts []string
	list := func(ids []int) string {
		var s []string
		for _, id := range ids {
			s = append(s, fmt.Sprintf("AC-%d", id))
		}
		return strings.Join(s, ", ")
	}
	if len(d.Removed) > 0 {
		parts = append(parts, "retires the tests of "+list(d.Removed))
	}
	if ids := d.clarifiedIDs(); len(ids) > 0 {
		parts = append(parts, "clarifies "+list(ids))
	}
	var rec []int
	for _, c := range d.Recovery {
		rec = append(rec, c.ID)
	}
	if len(rec) > 0 {
		parts = append(parts, "re-covers "+list(rec))
	}
	if len(parts) == 0 {
		return "revises tests"
	}
	return strings.Join(parts, "; ")
}

func retiredPaths(tests []RetiredTest) []string {
	var paths []string
	for _, t := range tests {
		paths = append(paths, t.Path)
	}
	return paths
}

func containsPath(paths []string, file string) bool {
	for _, p := range paths {
		if p == file {
			return true
		}
	}
	return false
}

// retirement reads only the pinned implementation history and immutable code objects.
// A shared file is retired in full; its unchanged criteria require new green coverage.
func (p *planner) retirement(st *PlanState) ([]RetiredTest, []string, error) {
	reasons := map[string]string{}
	last, ok := p.spec.LastImplementation()
	if !ok {
		return nil, nil, nil
	}
	byID := map[int]ImplementedCriterion{}
	for _, c := range last.Criteria {
		byID[c.ID] = c
	}
	for _, id := range st.Delta.Removed {
		for _, f := range byID[id].Tests {
			reasons[f] = "removed"
		}
	}
	for _, id := range st.Delta.Changed {
		for _, f := range byID[id].Tests {
			reasons[f] = "changed"
		}
	}
	for _, c := range st.Delta.Relocated {
		for _, f := range c.Tests {
			b, exists := gitBlob(p.repo.Root, st.Base, f)
			if !exists {
				return nil, nil, fmt.Errorf("relocated test %s is missing on the base", f)
			}
			if containsRelocationFrom([]byte(b), st.Relocations) {
				reasons[f] = "relocated"
			}
		}
	}
	for _, c := range st.Delta.Recovery {
		for _, f := range c.Tests {
			if reasons[f] != "" && reasons[f] != "relocated" {
				reasons[f] = "closure of a shared file"
			}
		}
	}
	// Old plans sealed only their new files. Walk the pinned implementation history to
	// locate each original hash; never infer authorization from the base's current bytes.
	hashes := map[string]string{}
	absent := map[string]bool{}
	for i := len(p.spec.Implemented) - 1; i >= 0; i-- {
		im := p.spec.Implemented[i]
		b, exists := gitBlob(p.repo.Root, im.Code, "aeh/seals/"+im.Outcome+".json")
		if !exists {
			continue
		}
		var s Seal
		if err := json.Unmarshal([]byte(b), &s); err != nil {
			return nil, nil, fmt.Errorf("read prior seal %s: %w", im.Outcome, err)
		}
		if s.Outcome != im.Outcome || s.Hash != sealHash(s.Files, s.Retired) {
			return nil, nil, fmt.Errorf("prior seal %s has an invalid manifest", im.Outcome)
		}
		for _, f := range s.Files {
			if _, exists := hashes[f.Path]; !exists {
				hashes[f.Path] = f.SHA256
			}
		}
		for _, f := range s.Retired {
			absent[f] = true
		}
	}
	var retired []RetiredTest
	for file, reason := range reasons {
		if hashes[file] == "" {
			return nil, nil, fmt.Errorf("retire %s: no sealed hash in the pinned implementation history", file)
		}
		retired = append(retired, RetiredTest{Path: file, SHA256: hashes[file], Reason: reason})
		absent[file] = true
	}
	sort.Slice(retired, func(i, j int) bool { return retired[i].Path < retired[j].Path })
	if err := validateRetiredTests(retired); err != nil {
		return nil, nil, err
	}
	if err := verifyRetiredBase(p.repo.Root, st.Base, retired); err != nil {
		return nil, nil, err
	}
	var absences []string
	for file := range absent {
		absences = append(absences, file)
	}
	sort.Strings(absences)
	return retired, absences, nil
}

func verifyRetiredBase(root, base string, retired []RetiredTest) error {
	for _, f := range retired {
		contents, err := sealedGitContents(root, base, []string{f.Path})
		if err != nil {
			return fmt.Errorf("retire %s: inspect base: %w", f.Path, err)
		}
		b, ok := contents[f.Path]
		sum := sha256.Sum256(b)
		if !ok || len(contents) != 1 || hex.EncodeToString(sum[:]) != f.SHA256 {
			return fmt.Errorf("retire %s: base bytes differ from the sealed hash", f.Path)
		}
	}
	return nil
}
