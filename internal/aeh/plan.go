package aeh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// PlanFile is the author's task and declared traceability, not proof of test coverage.
// A characterization plan (kind: characterize) has no task: it only seals tests of
// behavior the code already has.
type PlanFile struct {
	Kind  string              `yaml:"kind,omitempty" json:",omitempty"`
	Task  PlanTask            `yaml:"task,omitempty"`
	Trace map[string][]string `yaml:"trace"`
}

// PlanCharacterize is the plan and outcome kind of characterization tests: green on the
// existing code, with nothing to implement.
const PlanCharacterize = "characterize"

// PlanTask covers the new and changed criteria in one implementation task.
type PlanTask struct {
	Goal  string   `yaml:"goal"`
	Paths []string `yaml:"paths"`
}

const maxPlanFileBytes = 64 * 1024

var planCriterionID = regexp.MustCompile(`^AC-[1-9][0-9]*$`)

func parsePlanFile(b []byte) (PlanFile, error) {
	var p PlanFile
	if len(b) > maxPlanFileBytes {
		return p, fmt.Errorf("plan.yaml: exceeds %d bytes; shorten the plan", maxPlanFileBytes)
	}
	if _, err := checkConfigYAML(b); err != nil {
		return p, fmt.Errorf("plan.yaml: %w", err)
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&p); err != nil {
		return p, fmt.Errorf("parse plan.yaml: %w", err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return p, errors.New("plan.yaml: use exactly one YAML document")
	}
	p.Task.Goal = strings.TrimSpace(p.Task.Goal)
	return p, validatePlanForm(p)
}

func validatePlanForm(p PlanFile) error {
	switch p.Kind {
	case PlanCharacterize:
		if p.Task.Goal != "" || len(p.Task.Paths) > 0 {
			return errors.New("plan.yaml: a characterization plan has no task; remove task")
		}
	case "":
		goal := strings.TrimSpace(p.Task.Goal)
		if goal == "" || len(goal) > 500 || strings.ContainsAny(goal, "\r\n\u0085\u2028\u2029") {
			return errors.New("plan.yaml: task.goal must be one non-empty line of at most 500 bytes")
		}
		if len(p.Task.Paths) == 0 {
			return errors.New("plan.yaml: task.paths must name at least one literal path")
		}
		for _, t := range p.Task.Paths {
			if err := checkTaskPath(t); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("plan.yaml: kind %q is unknown; omit it, or use characterize for a characterization plan", p.Kind)
	}
	if len(p.Trace) == 0 {
		return errors.New("plan.yaml: trace must map each new AC-<n> to test files")
	}
	for _, id := range sortedTraceIDs(p.Trace) {
		if !planCriterionID.MatchString(id) {
			return fmt.Errorf("plan.yaml: criterion %q must be AC-<positive integer> without leading zeros", id)
		}
		if len(p.Trace[id]) == 0 {
			return fmt.Errorf("plan.yaml: %s needs at least one test file", id)
		}
		for _, file := range p.Trace[id] {
			if err := checkPlanTestPath(file); err != nil {
				return fmt.Errorf("plan.yaml: %s test %q: %w", id, file, err)
			}
			if strings.EqualFold(file, "plan.yaml") {
				return fmt.Errorf("plan.yaml: %s must trace a test file, not plan.yaml", id)
			}
		}
	}
	return nil
}

func sortedTraceIDs(trace map[string][]string) []string {
	ids := make([]string, 0, len(trace))
	for id := range trace {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// checkPlanTestPath also preserves the runner manifest's one-path-per-line framing.
func checkPlanTestPath(p string) error {
	if err := checkRelPath(p); err != nil {
		return err
	}
	if strings.ContainsAny(p, "\r\n") {
		return errors.New("must not contain CR or LF; acceptance manifests use one path per line")
	}
	for _, segment := range strings.Split(p, "/") {
		if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") ||
			strings.ContainsAny(segment, "<>:\"|?*") || strings.ContainsFunc(segment, unicode.IsControl) {
			return fmt.Errorf("segment %q cannot be checked out on Windows: invalid character or trailing dot/space", segment)
		}
		device, _, _ := strings.Cut(strings.ToUpper(segment), ".")
		if device == "CON" || device == "PRN" || device == "AUX" || device == "NUL" ||
			(len(device) == 4 && (strings.HasPrefix(device, "COM") || strings.HasPrefix(device, "LPT")) && device[3] >= '1' && device[3] <= '9') {
			return fmt.Errorf("segment %q cannot be checked out on Windows: reserved device name", segment)
		}
	}
	return nil
}

func checkTaskPath(t string) error {
	if strings.ContainsAny(t, "*?[]") {
		return fmt.Errorf("plan.yaml: task path %q must be literal; remove wildcards", t)
	}
	if err := checkRelPath(strings.TrimSuffix(t, "/")); err != nil {
		return fmt.Errorf("plan.yaml: task path %q: %w", t, err)
	}
	return nil
}

func generatedOutcomeID(feature, version string) (string, error) {
	v, err := parseSemver(version)
	if err != nil {
		return "", fmt.Errorf("generate outcome ID for %q: %w", feature, err)
	}
	id := fmt.Sprintf("%s-v%d_%d_%d", feature, v[0], v[1], v[2])
	if !validID(feature) || !validID(id) {
		return "", fmt.Errorf("generate outcome ID: feature %q must produce a valid ID", feature)
	}
	return id, nil
}

// PlanCandidateFile describes a changed Git object, without reading a checkout.
// Renames must be supplied as a deletion and an addition, both of which are checked.
type PlanCandidateFile struct {
	Path   string
	Mode   string
	Size   int64
	Status byte
}

// PlanInput is the runner's immutable input to pure candidate validation.
type PlanInput struct {
	OutcomeID string
	Kind      string // "" or PlanCharacterize
	Spec      Spec
	Delta     CriteriaDelta
	Protected []string
	Config    Config
	Retired   []RetiredTest
	Absences  []string
}

// validatePlanCandidate checks scope, form, containment and declared traceability only.
// The runner parses plan.yaml first and separately verifies red/green behavior and reviews.
func validatePlanCandidate(in PlanInput, files []PlanCandidateFile, plan PlanFile) error {
	if !validID(in.OutcomeID) {
		return fmt.Errorf("plan: invalid outcome ID %q", in.OutcomeID)
	}
	if err := in.Config.validatePlanConfig(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	cfg := in.Config.plan()
	dir := cfg.AcceptanceDir + "/" + in.OutcomeID + "/"
	if len(files) > cfg.MaxFiles {
		return fmt.Errorf("plan: %d files exceed plan.max_files %d; reduce the test files", len(files), cfg.MaxFiles)
	}
	ordered := append([]PlanCandidateFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	present := map[string]bool{}
	folded := map[string]bool{}
	retired := retiredPaths(in.Retired)
	var total int64
	for _, f := range ordered {
		// A test path is published, recorded in memory's implementation history and shown in
		// later plans: it must not carry a credential. The message never repeats the path.
		if len(findSecrets([]byte(f.Path))) > 0 {
			return errors.New("plan: a changed path looks like it contains a credential; rename the file")
		}
		if err := checkPlanTestPath(f.Path); err != nil {
			return fmt.Errorf("plan: changed path %q: %w", f.Path, err)
		}
		if containsPath(retired, f.Path) {
			if strings.HasPrefix(strings.ToLower(f.Path), "aeh/") || f.Status != 'D' {
				return fmt.Errorf("plan: retired path %q permits only deletion (D)", f.Path)
			}
			continue
		}
		if containsPath(in.Absences, f.Path) {
			return fmt.Errorf("plan: retired path %q must remain absent", f.Path)
		}
		if !strings.HasPrefix(f.Path, dir) {
			return fmt.Errorf("plan: path %q is outside %s; write only inside the test directory", f.Path, dir)
		}
		if f.Status != 'A' {
			return fmt.Errorf("plan: path %q has status %q; only new files (A) are allowed", f.Path, f.Status)
		}
		if f.Mode != "100644" && f.Mode != "100755" {
			return fmt.Errorf("plan: path %q has mode %s; use regular files, not symlinks or submodules", f.Path, f.Mode)
		}
		for _, segment := range strings.Split(f.Path, "/") {
			// Reuse gate helpers per segment: control names cannot hide as directories either.
			if strings.EqualFold(segment, ".git") || agentInstructionFile(segment) || gitControlFile(segment) {
				return fmt.Errorf("plan: path %q contains control name %q; remove it", f.Path, segment)
			}
		}
		if folded[strings.ToLower(f.Path)] {
			return fmt.Errorf("plan: duplicate or case-colliding path %q; use distinct file paths", f.Path)
		}
		folded[strings.ToLower(f.Path)] = true
		if f.Size < 0 || f.Size > int64(cfg.MaxBytes)-total {
			return fmt.Errorf("plan: path %q exceeds plan.max_bytes %d or has an invalid size; reduce the test bytes", f.Path, cfg.MaxBytes)
		}
		total += f.Size
		present[strings.TrimPrefix(f.Path, dir)] = true
	}
	if !present["plan.yaml"] {
		return fmt.Errorf("plan: missing %splan.yaml; add the task and trace map", dir)
	}
	if err := validatePlanForm(plan); err != nil {
		return err
	}
	if plan.Kind != in.Kind {
		if in.Kind == PlanCharacterize {
			return errors.New("plan: plan.yaml must declare kind: characterize and no task")
		}
		return errors.New("plan: plan.yaml must not declare a kind; it plans an implementation task")
	}
	protected := append([]string{"aeh/", ".aeh/", cfg.AcceptanceDir}, in.Protected...)
	protected = append(protected, in.Config.Protected...)
	for _, t := range plan.Task.Paths {
		contained := false
		for _, pattern := range in.Spec.Paths {
			if planPathContained(pattern, t) {
				contained = true
				break
			}
		}
		if !contained {
			return fmt.Errorf("plan: task path %q is not provably contained in feature %q paths; narrow the task path", t, in.Spec.Feature)
		}
		for _, p := range protected {
			// Both directions matter: a task may neither enter nor enclose protected paths.
			if namesArea(ruleSegments(t), ruleSegments(p)) || namesArea(ruleSegments(p), ruleSegments(t)) {
				return fmt.Errorf("plan: task path %q overlaps protected path %q; narrow the task path", t, p)
			}
		}
	}
	want := map[string]bool{}
	for _, c := range in.Delta.coverageCriteria() {
		id := fmt.Sprintf("AC-%d", c.ID)
		want[id] = true
		if len(plan.Trace[id]) == 0 {
			return fmt.Errorf("plan: missing criterion %s; trace it to at least one test file", id)
		}
	}
	traced := map[string]bool{}
	for _, id := range sortedTraceIDs(plan.Trace) {
		if !want[id] {
			return fmt.Errorf("plan: criterion %s is outside the planned delta; unchanged criteria keep their sealed tests unless recovery is required", id)
		}
		for _, file := range plan.Trace[id] {
			if !present[file] {
				return fmt.Errorf("plan: %s traces missing file %q; add it inside %s or correct the trace", id, file, dir)
			}
			traced[file] = true
		}
	}
	for _, f := range ordered {
		if f.Status == 'D' {
			continue
		}
		file := strings.TrimPrefix(f.Path, dir)
		if file != "plan.yaml" && !traced[file] {
			return fmt.Errorf("plan: test file %q is untraced; map it to a new criterion or remove it", f.Path)
		}
	}
	if _, err := fileKinds(in.Delta, plan); err != nil {
		return err
	}
	return nil
}

// planPathContained proves that every file below task matches matchAny's literal base.
// As in matchAny, a literal pattern, with or without a trailing / or **, covers descendants.
// Wildcards remaining in the base cannot prove containment: a single path.Match cannot
// cover files at arbitrary depth below a task directory.
func planPathContained(pattern, task string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		pattern, task = strings.ToLower(pattern), strings.ToLower(task)
	}
	pattern = strings.TrimPrefix(strings.ReplaceAll(pattern, "\\", "/"), "./")
	base := strings.TrimSuffix(strings.TrimSuffix(pattern, "**"), "/")
	if strings.ContainsAny(base, "*?[]") {
		return false
	}
	task = strings.TrimSuffix(task, "/")
	return base == "" || task == base || strings.HasPrefix(task, base+"/")
}

// generatedOutcome uses a minor change for first and subsequent implementations.
// Criterion text goes in Then; Outcome needs no synthetic
// Given/When placeholders and its existing normalization accepts this representation.
func generatedOutcome(in PlanInput, plan PlanFile, route string, changelog string) Outcome {
	cfg := in.Config.plan()
	o := Outcome{
		ID: in.OutcomeID, Kind: in.Kind, Title: in.Spec.Title, Level: 2, Feature: in.Spec.Feature,
		Change: "minor", Changelog: changelog, Route: route,
		Acceptance: []string{cfg.AcceptanceDir + "/" + in.OutcomeID + "/"},
	}
	if in.Kind != PlanCharacterize {
		o.Tasks = []Task{{ID: "T1", Goal: plan.Task.Goal, Paths: append([]string(nil), plan.Task.Paths...), MaxDiffLines: cfg.MaxDiffLines}}
	}
	var lines []string
	for _, c := range in.Delta.redCriteria() {
		text := normalizeCriterion(c.Text)
		lines = append(lines, fmt.Sprintf("AC-%d: %s", c.ID, text))
		o.Criteria = append(o.Criteria, Criterion{Then: text})
	}
	o.Description = strings.Join(lines, "\n")
	return o
}

func marshalOutcome(o Outcome) ([]byte, error) {
	b, err := yaml.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("marshal outcome %s: %w", o.ID, err)
	}
	return b, nil
}
