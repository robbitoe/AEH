package aeh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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
		case "changed", "removed", "closure of a shared file":
		default:
			return fmt.Errorf("retired test %s: invalid reason %q", t.Path, t.Reason)
		}
	}
	return nil
}

func (d CriteriaDelta) redCriteria() []SpecCriterion {
	crit := append([]SpecCriterion(nil), d.New...)
	for _, c := range d.Changes {
		crit = append(crit, SpecCriterion{ID: c.ID, Text: c.NewText})
	}
	return crit
}

func (d CriteriaDelta) coverageCriteria() []SpecCriterion {
	crit := d.redCriteria()
	for _, c := range d.Recovery {
		crit = append(crit, SpecCriterion{ID: c.ID, Text: c.Text})
	}
	return crit
}

// fileKinds returns true for green recovery and false for red implementation tests.
func fileKinds(d CriteriaDelta, plan PlanFile) (map[string]bool, error) {
	green := map[string]bool{}
	for _, c := range d.Recovery {
		green[fmt.Sprintf("AC-%d", c.ID)] = true
	}
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
	for _, c := range st.Delta.Recovery {
		for _, f := range c.Tests {
			if reasons[f] != "" {
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
