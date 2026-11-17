package aeh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Plan phases, persisted so an interrupted plan resumes where it stopped.
const (
	PlanReserved   = "reserved"   // inputs pinned; the base has not been checked yet
	PlanAuthoring  = "authoring"  // the base is green; attempts of the test author run
	PlanPublishing = "publishing" // a verified commit is chosen; aeh/<ID> is being created
	PlanPublished  = "published"  // aeh/<ID> points at the plan commit; the memory log is pending
	PlanLogged     = "logged"     // done: aeh run <ID> builds it
)

// PlanState is the local record of planning one generated outcome. It lives in the main
// checkout's .aeh/state/<ID>.plan.json, next to the run state of the same outcome.
type PlanState struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	Kind         string `json:"kind,omitempty"` // "" or PlanCharacterize
	Feature      string `json:"feature"`
	ObservedBase string `json:"observed_base_sha,omitempty"` // characterization: the inspected commit
	Phase        string `json:"phase"`
	Status       string `json:"status"` // running, blocked or the phase's end state
	Base         string `json:"base"`
	SpecVersion  string `json:"spec_version"`
	SpecDigest   string `json:"spec_content_sha256"`
	// MemoryCommit is the memory commit the plan was reserved at. The plan and its run read
	// memory as it was there. Empty in plans reserved before stage 5, which read it as it is.
	MemoryCommit string `json:"memory_commit,omitempty"`
	// Owners is the contract fingerprint of the pinned memory (which code every feature owns
	// and the feature's spec); the run checks its pinned snapshot against it.
	Owners          string           `json:"owners_sha256,omitempty"`
	PriorCode       string           `json:"prior_code_sha,omitempty"`
	Delta           CriteriaDelta    `json:"delta"`
	Route           string           `json:"route"`
	Roles           []string         `json:"roles"`
	Inputs          string           `json:"inputs_sha256"`
	Attempts        []PlanAttempt    `json:"attempts,omitempty"`
	PlanCommit      string           `json:"plan_commit,omitempty"`
	Author          string           `json:"author,omitempty"` // profile of the accepted plan's test author
	Reviews         []ReviewVerdict  `json:"reviews,omitempty"`
	Question        string           `json:"question,omitempty"`
	RestartRequired bool             `json:"restart_required,omitempty"`
	Answers         []string         `json:"answers,omitempty"`
	LadderFrom      int              `json:"ladder_from"` // attempts before this index belong to earlier ladder rounds
	UpdatedAt       time.Time        `json:"updated_at"`
	Checks          []PlanCheckEntry `json:"checks,omitempty"` // of the published attempt
}

// PlanAttempt is one run of the test author and the checks on its candidate.
type PlanAttempt struct {
	N          int             `json:"n"`
	Profile    string          `json:"profile"`
	RunDir     string          `json:"run_dir"`
	Candidate  string          `json:"candidate,omitempty"`
	Frozen     string          `json:"frozen,omitempty"`
	FailedStep string          `json:"failed_step,omitempty"`
	Feedback   string          `json:"feedback,omitempty"`
	Reviews    []ReviewVerdict `json:"reviews,omitempty"`
	Seconds    float64         `json:"seconds"`
}

// PlanCheckEntry is one deterministic check's result, for the report.
type PlanCheckEntry struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

const planStateVersion = 1

// planStatePath resolves the plan state in the main checkout, also from linked worktrees.
func planStatePath(r Repo, id string) (string, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return "", err
	}
	return filepath.Join(main, ".aeh", "state", id+".plan.json"), nil
}

// LoadPlanState reads an outcome's plan state; ok is false when the outcome was not planned.
func LoadPlanState(r Repo, id string) (PlanState, bool, error) {
	var s PlanState
	if !validID(id) {
		return s, false, fmt.Errorf("invalid outcome ID %q", id)
	}
	p, err := planStatePath(r, id)
	if err != nil {
		return s, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, fmt.Errorf("plan state %s: %w", id, err)
	}
	if s.ID != id || s.Version != planStateVersion {
		return s, false, fmt.Errorf("plan state %s: unexpected ID %q or version %d", id, s.ID, s.Version)
	}
	return s, true, nil
}

func savePlanState(r Repo, s *PlanState) error {
	p, err := planStatePath(r, s.ID)
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC()
	redactPlanState(s)
	return writePrivateJSON(p, s)
}

// redactPlanState redacts every diagnostic the state carries before it is saved: command,
// agent and Git output reach the next author's feedback, the owner's question and the
// terminal only from here, so no path can skip it. The in-memory state is redacted too.
func redactPlanState(s *PlanState) {
	s.Question = redactDiagnostic(s.Question)
	for i := range s.Answers {
		s.Answers[i] = redactDiagnostic(s.Answers[i]) // the owner's replies reach the next author
	}
	for i := range s.Attempts {
		s.Attempts[i].Feedback = redactDiagnostic(s.Attempts[i].Feedback)
	}
	for i := range s.Checks {
		s.Checks[i].Name = redactDiagnostic(s.Checks[i].Name) // names carry test paths
		s.Checks[i].Detail = redactDiagnostic(s.Checks[i].Detail)
	}
}

// redactPlanErr is the error boundary of aeh plan and aeh adopt seal: Git, command and memory
// diagnostics may carry secrets. Unlike redactErr it keeps the text otherwise as it is.
func redactPlanErr(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: redactDiagnostic(err.Error()), err: err}
}

// planner carries one planning session.
type planner struct {
	repo    Repo
	cfg     Config
	pcfg    PlanConfig
	spec    Spec
	memDir  string
	owners  string // every feature's paths and the declared spec, as the run contract sees them
	state   *PlanState
	log     io.Writer
	prompts map[string]string // the author's and QA's plan prompts and review-<role>.md
	sources map[string]string
	kind    string // "" or PlanCharacterize
}

// authorPrompt and qaPrompt name the prompts of this kind of plan.
func (p *planner) authorPrompt() string {
	if p.kind == PlanCharacterize {
		return "characterize.md"
	}
	return "plan.md"
}

func (p *planner) qaPrompt() string {
	if p.kind == PlanCharacterize {
		return "review-characterize.md"
	}
	return "review-plan.md"
}

// logf writes one redacted line to the terminal: arguments may carry command or agent output.
func (p *planner) logf(format string, args ...any) {
	if p.log != nil {
		fmt.Fprintln(p.log, redactDiagnostic(fmt.Sprintf(format, args...)))
	}
}

// PlanFeature plans the approved spec of a feature: it pins the inputs, checks the base,
// lets a test author write acceptance tests and one task, checks and reviews the frozen
// result, and publishes it on aeh/<ID>. Calling it again resumes an unfinished plan.
func PlanFeature(ctx context.Context, r Repo, feature string, log io.Writer) (_ PlanState, err error) {
	defer func() { err = redactPlanErr(err) }()
	return planFeature(ctx, r, feature, "", log)
}

// AdoptSeal plans characterization tests for a promoted, approved adopted feature: the tests
// must pass on the existing code, and the published commit is recorded as its first
// implementation. Calling it again resumes an unfinished characterization.
func AdoptSeal(ctx context.Context, r Repo, feature string, log io.Writer) (_ PlanState, err error) {
	defer func() { err = redactErr(err) }()
	return planFeature(ctx, r, feature, PlanCharacterize, log)
}

func planFeature(ctx context.Context, r Repo, feature, kind string, log io.Writer) (_ PlanState, err error) {
	// Plan state, locks, configuration and the base all come from the main checkout, also
	// when aeh plan runs in a linked worktree, so every caller sees one plan.
	main, err := MainCheckout(r)
	if err != nil {
		return PlanState{}, err
	}
	r = Repo{Root: main}
	unlockProject, err := lockProject(r)
	if err != nil {
		return PlanState{}, err
	}
	defer unlockProject()
	if kind == PlanCharacterize {
		// Like every adoption command: finish interrupted adoption imports first, and
		// regenerate the adoption report on the way out, under the same lock.
		if err := resumeAdoptImports(r); err != nil {
			return PlanState{}, err
		}
		defer func() {
			if a, lerr := loadAdoptState(r, ""); lerr == nil {
				a.reportCommand(&err)
			} else if err == nil {
				err = lerr
			}
		}()
	}
	cfg, err := r.LoadConfig()
	if err != nil {
		return PlanState{}, err
	}
	if strings.TrimSpace(cfg.Commands.Acceptance) == "" {
		return PlanState{}, errors.New("aeh plan needs commands.acceptance in aeh/config.yaml: a command that runs the test files listed in {{manifest}}")
	}
	memory, err := LoadMemory(r)
	if err != nil {
		return PlanState{}, err
	}
	if !memory.Present {
		return PlanState{}, errors.New("memory is not initialized; run: aeh memory init")
	}
	spec, ok := memory.Spec(feature)
	if !ok {
		return PlanState{}, fmt.Errorf("feature %q does not exist in memory", feature)
	}
	if kind == PlanCharacterize {
		// Recording a characterization bumps the spec's version, which names the next plan: a
		// published one is found by feature, never by the version it moved past.
		if st, ok, err := finishPublishedCharacterization(r, cfg, memory.Dir, spec, log); err != nil || ok {
			return st, err
		}
	}
	id, err := generatedOutcomeID(feature, spec.Version)
	if err != nil {
		return PlanState{}, err
	}
	// An unfinished plan reads its pinned memory, so it resumes even after the owner raised
	// the spec's version to approve newer content: aeh plan continues it first.
	if open, err := unfinishedPlan(r, feature); err != nil {
		return PlanState{}, err
	} else if open != "" {
		id = open
	}
	unlock, err := lockOutcome(r, id)
	if err != nil {
		return PlanState{}, err
	}
	defer unlock()
	st, exists, err := LoadPlanState(r, id)
	if err != nil {
		return st, err
	}
	var observed string
	if !exists {
		// Only a new reservation needs the spec approved as it reads now. A reserved plan
		// reads its pinned memory, so later edits never reach it.
		if !spec.Approved() {
			return PlanState{}, fmt.Errorf("feature %s is not approved as it reads now; approve it first: aeh memory approve %s", feature, feature)
		}
		if err := checkApprovalVersion(memory.Dir, spec, spec.Approval.Content); err != nil {
			return PlanState{}, fmt.Errorf("plan %s: %w", feature, err)
		}
		if spec.staleApproval() {
			return PlanState{}, fmt.Errorf("the approval of %s %s was computed against an implementation that is no longer the last one; approve it again so its delta is the real one: aeh memory approve %s",
				feature, spec.Version, feature)
		}
		if spec.Approval.Version != spec.Version {
			return PlanState{}, fmt.Errorf("feature %s is at version %s but its approval is of %s; approve this version first: aeh memory approve %s",
				feature, spec.Version, spec.Approval.Version, feature)
		}
		if observed, err = characterizationBase(memory.Dir, spec, kind); err != nil {
			return PlanState{}, err
		}
	}
	if exists && st.Kind != kind {
		if st.Kind == PlanCharacterize {
			return st, fmt.Errorf("%s is a characterization plan; continue it with: aeh adopt seal %s", id, feature)
		}
		return st, fmt.Errorf("%s is an implementation plan; continue it with: aeh plan %s", id, feature)
	}
	// Memory must be clean to reserve or to work before publication. After publication only
	// the log entry is missing, and logMemory reconciles an interrupted write of it.
	if !exists || st.Phase == PlanReserved || st.Phase == PlanAuthoring {
		if err := memoryClean(memory.Dir); err != nil {
			return st, fmt.Errorf("plan %s: %w; commit your memory edits first", feature, err)
		}
	}
	// The plan's view of memory: pinned at reservation (memory is clean, so its HEAD is what
	// was just read), read back from the pin on every resume.
	view := memory
	switch {
	case !exists:
		head, err := memoryHeadCommit(memory.Dir)
		if err != nil {
			return st, err
		}
		if view, err = loadMemoryAt(memory.Dir, head); err != nil {
			return st, err
		}
	case st.MemoryCommit != "":
		if view, err = loadMemoryAt(memory.Dir, st.MemoryCommit); err != nil {
			return st, fmt.Errorf("plan %s: %w", id, err)
		}
	}
	if spec, ok = view.Spec(feature); !ok {
		return st, fmt.Errorf("feature %q does not exist in memory", feature)
	}
	if exists && st.MemoryCommit != "" && spec.Version != st.SpecVersion {
		return st, fmt.Errorf("plan %s: pinned memory commit %s does not hold %s %s as planned", id, short(st.MemoryCommit), feature, st.SpecVersion)
	}
	if !spec.Approved() {
		return st, fmt.Errorf("feature %s is not approved as it reads now; approve it first: aeh memory approve %s", feature, feature)
	}
	p := &planner{repo: r, cfg: cfg, pcfg: cfg.plan(), spec: spec, memDir: memory.Dir, log: log, owners: view.ContractFingerprint(feature), kind: kind}
	if err := p.loadPrompts(); err != nil {
		return st, err
	}
	if !exists {
		st, err = p.reserve(id, observed, view.Commit)
		if err != nil {
			return st, err
		}
	}
	p.state = &st
	switch {
	case st.Phase == PlanLogged:
		p.logf("%s is already planned on %s; build it with: aeh run %s", id, IntegrationBranch(id), id)
		return st, nil
	case st.RestartRequired:
		return st, errors.New(st.Question)
	case st.Status == StatusBlocked:
		return st, fmt.Errorf("%s is blocked; answer it first: aeh answer %s \"...\"\n%s", id, id, st.Question)
	}
	inputs, err := p.inputsDigest()
	if err != nil {
		return st, err
	}
	prior, delta, err := p.currentDelta()
	if err != nil {
		return st, err
	}
	if inputs != st.Inputs || st.SpecDigest != spec.Approval.Content || prior != st.PriorCode || !sameDelta(delta, st.Delta) {
		return st, p.restart("The plan's inputs changed since it was reserved (spec, configuration or prompts). " +
			"Approve the spec as a new version, which gives the plan a new outcome ID, and run aeh plan again.")
	}
	st.Status = StatusRunning
	if err := savePlanState(r, &st); err != nil {
		return st, err
	}
	if st.Phase == PlanReserved {
		if err := p.preflight(); err != nil {
			return st, err
		}
		if ok, question, err := p.baseline(ctx); err != nil || !ok {
			if err != nil {
				return st, err
			}
			return st, p.block(question)
		}
		st.Phase = PlanAuthoring
		if err := savePlanState(r, &st); err != nil {
			return st, err
		}
	}
	if st.Phase == PlanAuthoring {
		if err := p.preflight(); err != nil {
			return st, err
		}
		ok, question, err := p.author(ctx)
		if err != nil {
			return st, err
		}
		if !ok {
			return st, p.block(question)
		}
	}
	if st.Phase == PlanPublishing {
		if err := p.publish(); err != nil {
			return st, err
		}
	}
	if st.Phase == PlanPublished {
		if err := p.logMemory(); err != nil {
			return st, err
		}
	}
	if kind == PlanCharacterize {
		p.logf("%s characterized on %s at %s and recorded in memory. Nothing to implement: merge %s into your base.", id, IntegrationBranch(id), short(st.PlanCommit), IntegrationBranch(id))
		return st, nil
	}
	p.logf("%s planned on %s at %s. Build it with: aeh run %s", id, IntegrationBranch(id), short(st.PlanCommit), id)
	return st, nil
}

// unfinishedPlan returns the ID of the feature's oldest plan that is neither logged nor
// waiting for a restart, or "". Its pin keeps it on the version it was reserved for.
func unfinishedPlan(r Repo, feature string) (string, error) {
	plans, err := ListPlanStates(r)
	if err != nil {
		return "", err
	}
	var open *PlanState
	for i := range plans {
		p := &plans[i]
		if p.Feature != feature || p.Phase == PlanLogged || p.RestartRequired {
			continue
		}
		if open == nil || p.UpdatedAt.Before(open.UpdatedAt) {
			open = p
		}
	}
	if open == nil {
		return "", nil
	}
	return open.ID, nil
}

// rulesForArea returns every rule that may touch a file of the area: a rule pattern compatible
// with the literal prefix of any area path counts. Uncertainty raises the route, never lowers it.
func rulesForArea(rules []TriageRule, paths []string) []TriageRule {
	var hit []TriageRule
	for _, rule := range rules {
	next:
		for _, rp := range rule.Paths {
			for _, ap := range paths {
				if ruleMayTouch(rp, ap) || ruleMayTouch(ap, rp) {
					hit = append(hit, rule)
					break next
				}
			}
		}
	}
	return hit
}

// finishPublishedCharacterization completes or reports a characterization of the feature that
// is already published on its branch: it records it in memory if a crash interrupted that
// (RecordChange is idempotent by outcome and commit), and is otherwise a no-op. The caller
// holds the project lock.
func finishPublishedCharacterization(r Repo, cfg Config, memDir string, spec Spec, log io.Writer) (PlanState, bool, error) {
	plans, err := ListPlanStates(r)
	if err != nil {
		return PlanState{}, false, err
	}
	for _, listed := range plans {
		if listed.Feature != spec.Feature || listed.Kind != PlanCharacterize || (listed.Phase != PlanPublished && listed.Phase != PlanLogged) {
			continue
		}
		unlock, err := lockOutcome(r, listed.ID)
		if err != nil {
			return listed, true, err
		}
		defer unlock()
		st, _, err := LoadPlanState(r, listed.ID)
		if err != nil {
			return st, true, err
		}
		p := &planner{repo: r, cfg: cfg, pcfg: cfg.plan(), spec: spec, memDir: memDir, log: log, kind: PlanCharacterize, state: &st}
		if st.Phase == PlanPublished {
			if err := p.recordCharacterization(); err != nil {
				return st, true, err
			}
		}
		p.logf("%s characterized on %s at %s and recorded in memory. Nothing to implement: merge %s into your base.", st.ID, IntegrationBranch(st.ID), short(st.PlanCommit), IntegrationBranch(st.ID))
		return st, true, nil
	}
	return PlanState{}, false, nil
}

// characterizationBase checks which kind of plan a feature admits. A feature promoted from an
// adopted area is characterized first: its existing behavior gets sealed tests before any
// change is planned. It returns the commit the area's agent last inspected.
func characterizationBase(memDir string, spec Spec, kind string) (string, error) {
	area, adopted, err := loadAreaIdentity(memDir, spec.Feature)
	if err != nil {
		return "", fmt.Errorf("adopted area %s: %w", spec.Feature, err)
	}
	_, implemented := spec.LastImplementation()
	switch {
	case kind == "" && adopted && !implemented:
		return "", fmt.Errorf("feature %s was adopted from existing code; characterize it first: aeh adopt seal %s", spec.Feature, spec.Feature)
	case kind == "":
		return "", nil
	case !adopted:
		return "", fmt.Errorf("feature %s has no adopted area; characterization applies only to adopted features", spec.Feature)
	case implemented:
		return "", fmt.Errorf("feature %s already has an implementation; plan new criteria with: aeh plan %s", spec.Feature, spec.Feature)
	}
	if len(area.Events) == 0 {
		return "", fmt.Errorf("adopted area %s has no observations", spec.Feature)
	}
	base := area.Events[len(area.Events)-1].CodeSHA
	if !isFullSHA(base) {
		return "", fmt.Errorf("adopted area %s: the last observation has no full code SHA", spec.Feature)
	}
	return base, nil
}

func (p *planner) loadPrompts() error {
	p.prompts, p.sources = map[string]string{}, map[string]string{}
	for _, name := range []string{p.authorPrompt(), p.qaPrompt()} {
		text, err := loadPrompt(p.repo, name)
		if err != nil {
			return fmt.Errorf("load %s: %w", name, err)
		}
		p.prompts[name], p.sources[name] = text, "default "+name
		if _, err := os.Stat(p.repo.PromptOverride(name)); err == nil {
			p.sources[name] = "aeh/prompts/" + name
		}
	}
	for _, role := range roleOrder {
		text, source, err := loadReviewPrompt(p.repo, role)
		if err != nil {
			return fmt.Errorf("load review prompt: %w", err)
		}
		p.prompts["review-"+role+".md"], p.sources["review-"+role+".md"] = text, source
	}
	return nil
}

// reserve pins the plan's inputs under a new outcome ID after checking that nothing else
// already uses that ID and that the last implementation is integrated in the base.
func (p *planner) reserve(id, observed, memoryCommit string) (PlanState, error) {
	st := PlanState{Version: planStateVersion, ID: id, Kind: p.kind, Feature: p.spec.Feature, Phase: PlanReserved, Status: StatusRunning,
		SpecVersion: p.spec.Version, SpecDigest: p.spec.Approval.Content, MemoryCommit: memoryCommit, Owners: p.owners, ObservedBase: observed}
	if err := p.checkCollisions(id); err != nil {
		return st, err
	}
	base, err := headCommit(p.repo.Root)
	if err != nil {
		return st, err
	}
	st.Base = base
	if st.PriorCode, st.Delta, err = p.currentDelta(); err != nil {
		return st, err
	}
	last, hasLast := p.spec.LastImplementation()
	if hasLast {
		if _, err := git(p.repo.Root, "merge-base", "--is-ancestor", last.Code, base); err != nil {
			return st, fmt.Errorf("plan %s: the last implementation (%s on %s) is not in the base %s; merge it first, then run aeh plan again",
				p.spec.Feature, short(last.Code), IntegrationBranch(last.Outcome), short(base))
		}
	}
	switch {
	case len(st.Delta.Changed) > 0 || len(st.Delta.Removed) > 0:
		return st, fmt.Errorf("plan %s: criteria changed %v or removed %v since %s; replacing sealed tests is not supported yet. Restore them, or keep them and add new criteria",
			p.spec.Feature, st.Delta.Changed, st.Delta.Removed, last.Outcome)
	case len(st.Delta.New) == 0:
		return st, fmt.Errorf("plan %s: the approved spec adds no criteria; nothing to plan", p.spec.Feature)
	}
	st.Route = RouteChange
	if !hasLast && p.kind != PlanCharacterize {
		st.Route = RouteNew
	}
	st.Roles = requiredRoles(st.Route)
	if p.kind == PlanCharacterize {
		// The route and reviewers come from the adopted area: every rule that may match
		// anything below the feature's paths raises them (conservatively, with triage's glob
		// semantics). The candidate's real files may raise them further.
		st.Route, st.Roles = applyRules(st.Route, st.Roles, rulesForArea(p.cfg.Triage.Rules, p.spec.Paths))
	}
	if st.Inputs, err = p.inputsDigestFor(&st); err != nil {
		return st, err
	}
	p.state = &st
	verb := "planning"
	if p.kind == PlanCharacterize {
		verb = "characterizing"
	}
	p.logf("%s: %s %s %s (%d criteria, route %s, base %s)", id, verb, p.spec.Feature, p.spec.Version, len(st.Delta.New), st.Route, short(base))
	return st, savePlanState(p.repo, &st)
}

// currentDelta reads the approved criteria and the last implementation as memory has them now.
func (p *planner) currentDelta() (string, CriteriaDelta, error) {
	crit, _, err := checkSpecCriteria(p.memDir, p.spec)
	if err != nil {
		return "", CriteriaDelta{}, fmt.Errorf("plan %s: acceptance criteria: %w", p.spec.Feature, err)
	}
	if len(crit) == 0 {
		return "", CriteriaDelta{}, fmt.Errorf("plan %s: the spec has no %q section; add criteria and approve it", p.spec.Feature, criteriaHeading)
	}
	im, ok := p.spec.LastImplementation()
	if !ok {
		return "", criteriaDelta(crit, nil), nil
	}
	return im.Code, criteriaDelta(crit, &im), nil
}

func sameDelta(a, b CriteriaDelta) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// checkCollisions refuses an ID that any outcome, state, seal, branch or test directory
// already uses, ignoring case so the same ID never means two things on Windows or macOS.
func (p *planner) checkCollisions(id string) error {
	lower := strings.ToLower(id)
	same := func(name string) bool { return strings.ToLower(name) == lower }
	dirs := map[string][]string{
		filepath.Join(p.repo.Root, "aeh", "outcomes"): {".yaml", ".yml"},
		filepath.Join(p.repo.Root, "aeh", "seals"):    {".json"},
		p.repo.StateDir(): {".json", ".plan.json", ".lock"},
	}
	for dir, exts := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, e := range entries {
			for _, ext := range exts {
				folded := strings.ToLower(e.Name())
				name := strings.TrimSuffix(folded, ext)
				if name != folded && same(name) && !(ext == ".lock" && e.Name() == id+".lock") {
					return fmt.Errorf("outcome ID %s is already used by %s", id, filepath.Join(dir, e.Name()))
				}
			}
		}
	}
	refs, err := git(p.repo.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads/aeh/")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if same(strings.TrimPrefix(ref, "aeh/")) {
			return fmt.Errorf("outcome ID %s is already used by branch %s", id, ref)
		}
	}
	tests, _ := git(p.repo.Root, "ls-tree", "-z", "-d", "--name-only", "HEAD", p.pcfg.AcceptanceDir+"/")
	names := strings.Split(tests, "\x00")
	if entries, err := os.ReadDir(filepath.Join(p.repo.Root, filepath.FromSlash(p.pcfg.AcceptanceDir))); err == nil {
		for _, e := range entries {
			names = append(names, e.Name()) // untracked or ignored directories count too
		}
	}
	for _, d := range names {
		if d != "" && same(path.Base(d)) {
			return fmt.Errorf("outcome ID %s is already used by the test directory %s", id, d)
		}
	}
	return nil
}

// inputsDigest fingerprints everything that decides the plan, so a resume never mixes
// inputs: base, spec, prior implementation, delta, configuration and prompts.
func (p *planner) inputsDigest() (string, error) { return p.inputsDigestFor(p.state) }

func (p *planner) inputsDigestFor(st *PlanState) (string, error) {
	// The effective reviewer of every role for every author, whatever configuration produced
	// it (declared policy or legacy reviewer), plus the configuration itself.
	effective := map[string]string{}
	for _, author := range p.pcfg.Ladder {
		for _, role := range roleOrder {
			name, err := p.cfg.reviewerFor(role, author)
			if err != nil {
				name = "error: " + err.Error()
			}
			effective[author+"/"+role] = name
		}
	}
	var prior *Implementation
	if im, ok := p.spec.LastImplementation(); ok {
		prior = &im
	}
	v := struct {
		Base, Spec        string
		Prior             *Implementation
		Delta             CriteriaDelta
		Plan              PlanConfig
		Commands          any
		PassEnv           []string
		Owners            string
		Protected         []string
		Rules             []TriageRule
		Reviewers         ReviewerPolicy
		ReviewersDeclared bool
		LegacyReviewer    string
		Effective         map[string]string
		Profiles          map[string]Profile
		Prompts           map[string]string
		Kind              string `json:",omitempty"`
		ObservedBase      string `json:",omitempty"`
	}{st.Base, st.SpecDigest, prior, st.Delta, p.pcfg, p.cfg.Commands, p.cfg.PassEnv, p.owners, p.cfg.Protected, p.cfg.Triage.Rules,
		p.cfg.Reviewers, p.cfg.Reviewers.declared, p.cfg.Reviewer, effective, p.cfg.Profiles, p.prompts, st.Kind, st.ObservedBase}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// allPlanRoles is every role a plan review may need: the route's and any rule's.
func (p *planner) allPlanRoles() []string {
	route, roles := p.state.Route, p.state.Roles
	for _, rule := range p.cfg.Triage.Rules {
		route = maxRoute(route, rule.Route)
		roles = requiredRoles(route, roles, rule.Reviewers)
	}
	return roles
}

// preflight fails before any agent runs when a profile is missing or not independent.
func (p *planner) preflight() error {
	if len(p.pcfg.Ladder) == 0 {
		return errors.New("plan: no profiles in plan.ladder or ladder")
	}
	roles := p.allPlanRoles()
	if err := p.cfg.checkIndependence(p.pcfg.Ladder, roles); err != nil {
		return fmt.Errorf("plan reviewer configuration: %w", err)
	}
	names := append([]string{}, p.pcfg.Ladder...)
	for _, author := range p.pcfg.Ladder {
		for _, role := range roles {
			n, err := p.cfg.reviewerFor(role, author)
			if err != nil {
				return err
			}
			names = append(names, n)
		}
	}
	for _, n := range names {
		if err := p.cfg.checkProfile(n); err != nil {
			return err
		}
		if _, err := exec.LookPath(p.cfg.Profiles[n].Command[0]); err != nil {
			return fmt.Errorf("profile %q: %s is not installed or not on PATH", n, p.cfg.Profiles[n].Command[0])
		}
	}
	return nil
}

// acceptanceCommand runs the configured acceptance command on a runner-written manifest.
func (p *planner) acceptanceCommand(ctx context.Context, name, wt string, files []string) (GateResult, error) {
	// The manifest is passed as a relative path made only of safe characters, so neither the
	// repository's location nor a file name ever reaches the shell. It lives in the worktree's
	// .aeh/, which memory's privacy rules require to be ignored, and is removed afterwards.
	rel := ".aeh/manifest-" + name + ".txt"
	if !safeManifestName.MatchString(rel) {
		return GateResult{}, fmt.Errorf("unsafe manifest name %q", rel)
	}
	full := filepath.Join(wt, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return GateResult{}, err
	}
	if err := os.WriteFile(full, []byte(strings.Join(files, "\n")+"\n"), 0o600); err != nil {
		return GateResult{}, err
	}
	defer os.Remove(full)
	command := strings.Replace(p.cfg.Commands.Acceptance, "{{manifest}}", rel, 1)
	return gateCommand(ctx, name, wt, command, p.timeout(), p.cfg.PassEnv), nil
}

var safeManifestName = regexp.MustCompile(`^\.aeh/manifest-[a-z0-9-]+\.txt$`)

func (p *planner) timeout() time.Duration {
	return time.Duration(p.cfg.Commands.TimeoutMinutes) * time.Minute
}

// previousSuite lists the acceptance tests the base already has: every file under the
// acceptance directory except plan files. The new tests are not in the base, so running this
// manifest on the plan commit checks that the additions leave the previous suite green.
func (p *planner) previousSuite() (files, missing []string, err error) {
	out, err := git(p.repo.Root, "ls-tree", "-r", "-z", "--name-only", p.state.Base, "--", p.pcfg.AcceptanceDir+"/")
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && path.Base(f) != "plan.yaml" {
			seen[f] = true
		}
	}
	// Every sealed test recorded for an unchanged criterion runs too, wherever it lives; one
	// that the base no longer has is reported, never silently dropped.
	for _, c := range p.state.Delta.Unchanged {
		for _, t := range c.Tests {
			if seen[t] {
				continue
			}
			if _, err := git(p.repo.Root, "cat-file", "-e", p.state.Base+":"+t); err != nil {
				missing = append(missing, fmt.Sprintf("%s (AC-%d)", t, c.ID))
				continue
			}
			seen[t] = true
		}
	}
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	sort.Strings(missing)
	return files, missing, nil
}

// freshWorktree checks out commit at path, first removing whatever an interrupted or answered
// earlier run left there. The caller holds the project and outcome locks.
func (p *planner) freshWorktree(wt, commit string) error {
	if _, err := os.Stat(wt); err == nil {
		removeWorktree(p.repo.Root, wt)
		if err := os.RemoveAll(wt); err != nil {
			return fmt.Errorf("remove stale worktree %s: %w", wt, err)
		}
	}
	if err := addWorktreeDetached(p.repo.Root, wt, commit); err != nil {
		return err
	}
	if err := verifyCheckout(wt, commit); err != nil {
		removeWorktree(p.repo.Root, wt)
		return err
	}
	return nil
}

// baseline checks, before any agent runs, that the base builds, passes its tests and passes
// the sealed tests of unchanged criteria. A red base is the environment's problem, not the
// test author's, so it blocks without consuming attempts.
func (p *planner) baseline(ctx context.Context) (bool, string, error) {
	st := p.state
	runDir := p.repo.RunDir(st.ID, "plan-baseline")
	wt := p.repo.WorktreePath(st.ID + "-plan-baseline")
	if err := p.freshWorktree(wt, st.Base); err != nil {
		return false, "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(p.repo.Root, wt)
		}
	}()
	p.logf("  baseline on %s", short(st.Base))
	steps := []func() (GateResult, error){
		func() (GateResult, error) {
			return gateCommand(ctx, "build", wt, p.cfg.Commands.Build, p.timeout(), p.cfg.PassEnv), nil
		},
		func() (GateResult, error) {
			return gateCommand(ctx, "test", wt, p.cfg.Commands.Test, p.timeout(), p.cfg.PassEnv), nil
		},
	}
	tests, missing, err := p.previousSuite()
	if err != nil {
		return false, "", err
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("The base %s lacks sealed tests of unchanged criteria: %s. Restore them in the base, then answer to retry.",
			short(st.Base), strings.Join(missing, ", ")), nil
	}
	if len(tests) > 0 {
		steps = append(steps, func() (GateResult, error) { return p.acceptanceCommand(ctx, "sealed", wt, tests) })
	}
	var results []GateResult
	for _, step := range steps {
		g, err := step()
		if err != nil {
			return false, "", err
		}
		g = checkoutGate(g, wt, st.Base)
		if cerr := verifyCheckout(wt, st.Base); cerr != nil && g.Passed {
			g.Passed, g.Output = false, cerr.Error()
		}
		// Command output may carry secrets: redact it before it is saved, logged or asked.
		g.Output = redactDiagnostic(g.Output)
		results = append(results, g)
		p.logf("    %-6s %s", g.Name, gateStatus(g))
		if g.Uncertain {
			keep = true
			_ = writePrivateJSON(filepath.Join(runDir, "gates.json"), results)
			return false, fmt.Sprintf("The baseline %s of %s was interrupted or timed out (worktree kept at %s). Confirm all processes stopped, then answer to retry.", g.Name, short(st.Base), wt), nil
		}
		if !g.Passed {
			_ = writePrivateJSON(filepath.Join(runDir, "gates.json"), results)
			return false, fmt.Sprintf("The base %s is not green: %s failed before any test was written:\n%s\nFix the base (or the commands), then answer to retry.",
				short(st.Base), g.Name, tail(g.Output, 2000)), nil
		}
	}
	return true, "", writePrivateJSON(filepath.Join(runDir, "gates.json"), results)
}

// author runs the plan ladder: each profile gets a first attempt and one retry with the
// failure as feedback, then the next profile; then the plan blocks with a question.
func (p *planner) author(ctx context.Context) (bool, string, error) {
	st := p.state
	for i := range st.Attempts {
		a := &st.Attempts[i]
		if a.FailedStep == "running" {
			a.FailedStep, a.Feedback = "interrupted", "the previous aeh plan stopped during this attempt"
		}
		// Worktrees kept for an uncertain step are released once the owner answered.
		wt := p.planWorktree(a.N)
		for _, dir := range []string{wt, wt + "-verify"} {
			if _, err := os.Stat(dir); err == nil {
				removeWorktree(p.repo.Root, dir)
				_ = os.RemoveAll(dir)
			}
		}
	}
	finish := func(a *PlanAttempt) (bool, string, error) {
		st.Phase, st.PlanCommit, st.Author, st.Reviews = PlanPublishing, a.Frozen, a.Profile, a.Reviews
		return true, "", p.save()
	}
	// A frozen candidate whose checks or reviews were interrupted resumes on the same commit;
	// one that passed everything before the process stopped goes straight to publication.
	if n := len(st.Attempts); n > 0 {
		last := &st.Attempts[n-1]
		if last.Frozen != "" && (last.FailedStep == "checking" || last.FailedStep == "reviewing") {
			p.logf("  resuming the checks of plan attempt %d on %s", last.N, short(last.Frozen))
			question, err := p.verifyFrozen(ctx, last)
			if serr := p.save(); serr != nil && err == nil {
				err = serr
			}
			if err != nil || question != "" {
				return false, question, err
			}
		}
		if last.Frozen != "" && last.FailedStep == "" {
			return finish(last)
		}
	}
	feedback := ""
	if n := len(st.Attempts); n > 0 {
		feedback = st.Attempts[n-1].Feedback
	}
	if len(st.Answers) > 0 {
		feedback = strings.TrimSpace(feedback + "\n\nThe owner answered: " + strings.Join(st.Answers, "\n"))
	}
	// Each profile gets two tries per ladder round. A crash resumes the round where it was; an
	// owner's answer starts a new round.
	for _, profile := range p.pcfg.Ladder {
		done := 0
		for _, prev := range st.Attempts[min(st.LadderFrom, len(st.Attempts)):] {
			if prev.Profile == profile {
				done++
			}
		}
		for try := done; try < 2; try++ {
			n := len(st.Attempts) + 1
			st.Attempts = append(st.Attempts, PlanAttempt{N: n, Profile: profile, FailedStep: "running",
				RunDir: rel(p.repo.Root, p.repo.RunDir(st.ID, fmt.Sprintf("plan-a%d", n)))})
			if err := p.save(); err != nil {
				return false, "", err
			}
			a := &st.Attempts[n-1]
			start := time.Now()
			question, err := p.attempt(ctx, a, feedback)
			a.Seconds = time.Since(start).Seconds()
			if serr := p.save(); serr != nil && err == nil {
				err = serr
			}
			if err != nil {
				return false, "", err
			}
			if question != "" {
				return false, question, nil
			}
			if a.FailedStep == "" {
				return finish(a)
			}
			p.logf("    %-8s FAIL", a.FailedStep)
			feedback = fmt.Sprintf("Your previous attempt failed at %s:\n%s", a.FailedStep, a.Feedback)
		}
	}
	last := st.Attempts[len(st.Attempts)-1]
	return false, fmt.Sprintf("No plan author produced an acceptable plan. Last failure at %s:\n%s\nWhat should change? Answer to retry the plan ladder.", last.FailedStep, tail(last.Feedback, 2000)), nil
}

func (p *planner) save() error { return savePlanState(p.repo, p.state) }

// blockNow marks the plan blocked in memory so the next save records the question together
// with the attempt's result; the caller returns the question.
func (p *planner) blockNow(question string) string {
	p.state.Status, p.state.Question = StatusBlocked, question
	return question
}

func (p *planner) planWorktree(n int) string {
	return p.repo.WorktreePath(fmt.Sprintf("%s-plan-a%d", p.state.ID, n))
}

// attempt runs the test author once and checks its candidate. A non-empty question means
// the plan must block now (the author asked to split, or a command was uncertain).
func (p *planner) attempt(ctx context.Context, a *PlanAttempt, feedback string) (string, error) {
	st, r := p.state, p.repo
	runDir := filepath.Join(r.Root, filepath.FromSlash(a.RunDir))
	fail := func(step, msg string) (string, error) {
		a.FailedStep, a.Feedback = step, msg
		return "", nil
	}
	wt := p.planWorktree(a.N)
	if err := p.freshWorktree(wt, st.Base); err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(r.Root, wt)
		}
	}()
	in := p.input()
	data := newPlanPromptData(in)
	data.Feedback = feedback
	prompt, err := renderPrompt(p.authorPrompt(), p.prompts[p.authorPrompt()], data)
	if err != nil {
		return "", err
	}
	p.logf("  plan attempt %d with profile %q", a.N, a.Profile)
	outPath, err := runAgent(ctx, p.cfg.Profiles[a.Profile], p.cfg.PassEnv, wt, prompt, runDir, "plan")
	if err != nil {
		if uncertainExit(err) {
			keep = true
			a.FailedStep, a.Feedback = "author", err.Error()
			return p.blockNow(fmt.Sprintf("The plan author %q was interrupted or timed out (worktree kept at %s). Confirm all processes stopped and review possible effects before answering to retry.", a.Profile, wt)), nil
		}
		return fail("author", err.Error())
	}
	out, _ := os.ReadFile(outPath)
	switch planVerdict(string(out)) {
	case "TOO-LARGE":
		a.FailedStep, a.Feedback = "author", "the author says the new criteria do not fit one task:\n"+tail(string(out), 2000)
		// Blocked in the same save as the attempt: the owner's pause cannot be lost to a crash.
		return p.blockNow(fmt.Sprintf("The plan author says the new criteria of %s %s do not fit one task of %d changed lines:\n%s\nSplit the spec into smaller versions (approve fewer new criteria at a time), or answer to retry.",
			st.Feature, st.SpecVersion, p.pcfg.MaxDiffLines, tail(string(out), 1500))), nil
	case "READY":
	default:
		return fail("author", `the author's output must end with "PLAN: READY" or "PLAN: TOO-LARGE"`)
	}
	candidate, err := candidateCommit(wt, st.Base, fmt.Sprintf("aeh: plan %s (attempt %d, %s)", st.ID, a.N, a.Profile))
	if err != nil {
		return fail("scope", err.Error())
	}
	a.Candidate = candidate
	files, err := planCandidateFiles(r.Root, st.Base, candidate)
	if err != nil {
		return "", err
	}
	// Rules on the files the candidate really changes, then on the planned task's paths, raise
	// the route and add reviewers. The policy persists across attempts and only increases.
	var changed []string
	for _, f := range files {
		changed = append(changed, f.Path)
	}
	st.Route, st.Roles = applyRules(st.Route, st.Roles, rulesForFiles(p.cfg.Triage.Rules, changed))
	dir := p.testDir()
	planYAML, ok := gitBlob(r.Root, candidate, dir+"plan.yaml")
	if !ok {
		return fail("scope", fmt.Sprintf("missing %splan.yaml", dir))
	}
	plan, err := parsePlanFile([]byte(planYAML))
	if err != nil {
		return fail("form", err.Error())
	}
	if len(plan.Task.Paths) > 0 {
		st.Route, st.Roles = applyRules(st.Route, st.Roles, rulesForTasks(p.cfg.Triage.Rules, []Task{{ID: "T1", Paths: plan.Task.Paths}}))
	}
	if err := p.save(); err != nil {
		return "", err
	}
	if err := validatePlanCandidate(in, files, plan); err != nil {
		return fail("scope", err.Error())
	}
	frozen, err := p.freeze(candidate, files, plan, st.Route)
	if err != nil {
		return "", err
	}
	a.Frozen, a.FailedStep = frozen, "checking"
	if err := writePrivateJSON(filepath.Join(runDir, "candidate.json"), map[string]string{"base": st.Base, "candidate": candidate, "frozen": frozen}); err != nil {
		return "", err
	}
	if err := p.save(); err != nil {
		return "", err
	}
	return p.verifyFrozen(ctx, a)
}

// verifyFrozen runs the deterministic checks and the role reviews on a frozen plan commit. It
// persists progress after the checks and after every verdict, so an interrupted run resumes on
// the same commit and asks only the reviewers that have not approved it yet.
func (p *planner) verifyFrozen(ctx context.Context, a *PlanAttempt) (string, error) {
	st, r := p.state, p.repo
	runDir := filepath.Join(r.Root, filepath.FromSlash(a.RunDir))
	fail := func(step, msg string) (string, error) {
		a.FailedStep, a.Feedback = step, msg
		return "", nil
	}
	planYAML, ok := gitBlob(r.Root, a.Frozen, p.testDir()+"plan.yaml")
	if !ok {
		return "", fmt.Errorf("plan commit %s has no plan.yaml", short(a.Frozen))
	}
	plan, err := parsePlanFile([]byte(planYAML))
	if err != nil {
		return "", fmt.Errorf("plan commit %s: %w", short(a.Frozen), err)
	}
	diff, err := candidateDiff(r.Root, st.Base, a.Frozen)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(runDir, "diff.patch"), []byte(diff), 0o600); err != nil {
		return "", err
	}
	vwt := p.planWorktree(a.N) + "-verify"
	if err := p.freshWorktree(vwt, a.Frozen); err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(r.Root, vwt)
		}
	}()
	if a.FailedStep == "checking" {
		checks, step, msg, uncertain, err := p.redGreen(ctx, vwt, a.Frozen, plan)
		if err != nil {
			return "", err
		}
		_ = writePrivateJSON(filepath.Join(runDir, "checks.json"), checks)
		if uncertain {
			keep = true
			a.FailedStep, a.Feedback = step, msg
			return p.blockNow(fmt.Sprintf("Plan check %s was interrupted or timed out (worktree kept at %s). Confirm all processes stopped, then answer to retry.", step, vwt)), nil
		}
		if step != "" {
			return fail(step, msg)
		}
		st.Checks, a.FailedStep = checks, "reviewing"
		if err := p.save(); err != nil {
			return "", err
		}
	}
	for _, role := range roleOrder {
		if !containsString(st.Roles, role) || approvedOn(a.Reviews, role, a.Frozen) {
			continue
		}
		v, g, err := p.review(ctx, role, a.Profile, a.Frozen, vwt, diff, planYAML, runDir)
		if err != nil {
			return "", err
		}
		g.Output = redactDiagnostic(g.Output)
		// The verdict and what it means for the attempt are saved together: a rejected or
		// uncertain review never resumes as a review still to ask.
		a.Reviews = append(a.Reviews, v)
		question := ""
		switch {
		case g.Uncertain:
			keep = true
			a.FailedStep, a.Feedback = "review", g.Output
			question = p.blockNow(fmt.Sprintf("The %s review of the plan was interrupted or timed out (worktree kept at %s). Confirm all processes stopped, then answer to retry.", role, vwt))
		case !g.Passed:
			a.FailedStep, a.Feedback = "review", g.Output
		}
		if err := p.save(); err != nil {
			return "", err
		}
		if question != "" || !g.Passed {
			return question, nil
		}
	}
	a.FailedStep, a.Feedback = "", ""
	return "", nil
}

func approvedOn(reviews []ReviewVerdict, role, sha string) bool {
	for _, v := range reviews {
		if v.Role == role && v.SHA == sha && v.Verdict == "APPROVE" {
			return true
		}
	}
	return false
}

func (p *planner) testDir() string { return p.pcfg.AcceptanceDir + "/" + p.state.ID + "/" }

func (p *planner) input() PlanInput {
	return PlanInput{OutcomeID: p.state.ID, Kind: p.state.Kind, Spec: p.spec, Delta: p.state.Delta, Protected: p.cfg.Protected, Config: p.cfg}
}

// planVerdict reads the author's last non-empty line: "PLAN: READY" or "PLAN: TOO-LARGE".
func planVerdict(out string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(out, "\r\n", "\n"), "\n \t"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	switch last {
	case "PLAN: READY":
		return "READY"
	case "PLAN: TOO-LARGE":
		return "TOO-LARGE"
	}
	return ""
}

// planCandidateFiles lists a candidate's changes from Git objects: path, mode, size, status.
func planCandidateFiles(root, base, commit string) ([]PlanCandidateFile, error) {
	out, err := git(root, "diff", "--raw", "-z", "--no-renames", "--ignore-submodules=none", "--no-abbrev", base, commit)
	if err != nil {
		return nil, err
	}
	var files []PlanCandidateFile
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta, path := strings.TrimPrefix(fields[i], ":"), fields[i+1]
		parts := strings.Fields(meta)
		if len(parts) != 5 || len(parts[4]) == 0 {
			return nil, fmt.Errorf("invalid Git raw diff record %q", fields[i])
		}
		f := PlanCandidateFile{Path: path, Mode: parts[1], Status: parts[4][0]}
		if f.Status == 'D' {
			f.Mode = parts[0]
		} else if f.Mode != "160000" {
			size, err := git(root, "cat-file", "-s", parts[3])
			if err != nil {
				return nil, err
			}
			if f.Size, err = strconv.ParseInt(size, 10, 64); err != nil {
				return nil, fmt.Errorf("size of %s: %w", path, err)
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// freeze writes the generated outcome and the runner's seal next to the tests and commits
// them as one commit on the base. That commit is what is checked, reviewed and published.
func (p *planner) freeze(candidate string, files []PlanCandidateFile, plan PlanFile, route string) (string, error) {
	st, root := p.state, p.repo.Root
	contents := map[string][]byte{}
	for _, f := range files {
		b, ok := gitBlob(root, candidate, f.Path)
		if !ok {
			return "", fmt.Errorf("read %s from the candidate", f.Path)
		}
		contents[f.Path] = []byte(b)
	}
	var ids []string
	for _, c := range st.Delta.New {
		ids = append(ids, fmt.Sprintf("AC-%d", c.ID))
	}
	changelog := fmt.Sprintf("Adds %s.", strings.Join(ids, ", "))
	if st.Kind == PlanCharacterize {
		changelog = fmt.Sprintf("Characterizes %s.", strings.Join(ids, ", "))
	}
	o := generatedOutcome(p.input(), plan, route, changelog)
	outcome, err := marshalOutcome(o)
	if err != nil {
		return "", err
	}
	seal := sealFromContents(st.ID, contents, st.SpecDigest, time.Now().UTC())
	sealJSON, err := json.MarshalIndent(seal, "", "  ")
	if err != nil {
		return "", err
	}
	generated := map[string][]byte{
		"aeh/outcomes/" + st.ID + ".yaml": outcome,
		"aeh/seals/" + st.ID + ".json":    append(sealJSON, '\n'),
	}
	// Build the tree from the candidate's objects in a private index: nothing the author's
	// worktree does after the candidate was captured can reach the plan commit.
	tmp, err := os.MkdirTemp("", "aeh-plan-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := gitEnv(root, env, nil, "read-tree", candidate); err != nil {
		return "", err
	}
	for rel, b := range generated {
		sha, err := gitEnv(root, nil, b, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return "", err
		}
		if _, err := gitEnv(root, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+sha+","+rel); err != nil {
			return "", err
		}
	}
	tree, err := gitEnv(root, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("aeh: plan %s for %s %s\n\nAcceptance tests and one task for %s, sealed by aeh.", st.ID, st.Feature, st.SpecVersion, strings.Join(ids, ", "))
	if st.Kind == PlanCharacterize {
		msg = fmt.Sprintf("aeh: characterize %s %s (%s)\n\nCharacterization tests of the existing behavior for %s, sealed by aeh.", st.Feature, st.SpecVersion, st.ID, strings.Join(ids, ", "))
	}
	frozen, err := git(root, "-c", "user.name=aeh", "-c", "user.email=aeh@localhost", "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", st.Base, "-m", msg)
	if err != nil {
		return "", err
	}
	// Verify the frozen commit: exactly the candidate's files with the same objects and modes,
	// plus the two generated files with exactly the generated bytes.
	got, err := planCandidateFiles(root, st.Base, frozen)
	if err != nil {
		return "", err
	}
	if len(got) != len(files)+len(generated) {
		return "", fmt.Errorf("frozen plan commit changes %d paths, expected %d", len(got), len(files)+len(generated))
	}
	want := map[string]PlanCandidateFile{}
	for _, f := range files {
		want[f.Path] = f
	}
	for _, f := range got {
		if f.Status != 'A' {
			return "", fmt.Errorf("frozen plan commit has unexpected change %c %s", f.Status, f.Path)
		}
		if b, ok := generated[f.Path]; ok {
			if blob, ok := gitBlob(root, frozen, f.Path); !ok || blob != string(b) || f.Mode != "100644" {
				return "", fmt.Errorf("frozen plan commit has unexpected content in %s", f.Path)
			}
			continue
		}
		c, ok := want[f.Path]
		if !ok || c.Mode != f.Mode {
			return "", fmt.Errorf("frozen plan commit has unexpected file %s", f.Path)
		}
		a, err1 := git(root, "rev-parse", candidate+":"+f.Path)
		b, err2 := git(root, "rev-parse", frozen+":"+f.Path)
		if err1 != nil || err2 != nil || a != b {
			return "", fmt.Errorf("frozen plan commit changed %s from the checked candidate", f.Path)
		}
	}
	return frozen, nil
}

// gitEnv runs git with extra environment variables and optional stdin.
func gitEnv(dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(commandEnv(nil), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// redGreen checks the frozen commit: it builds, every new test file fails on its own (the
// feature does not exist yet), and the sealed tests of unchanged criteria still pass. Each
// command runs in the verify worktree, whose checkout must stay intact even when a command
// fails as expected. A failure is evidence, not proof: the reviewer judges why it fails.
func (p *planner) redGreen(ctx context.Context, wt, commit string, plan PlanFile) ([]PlanCheckEntry, string, string, bool, error) {
	var checks []PlanCheckEntry
	// settle reports, in this order: an uncertain end (timeout or cancellation: block and keep
	// the worktree, whatever else happened), then damage to the checkout (a failed attempt).
	settle := func(g GateResult) (uncertain bool, damaged string) {
		if g.Uncertain {
			return true, ""
		}
		if err := verifyCheckout(wt, commit); err != nil {
			return false, fmt.Sprintf("%s modified the checkout: %v", g.Name, err)
		}
		return false, ""
	}
	g := gateCommand(ctx, "build", wt, p.cfg.Commands.Build, p.timeout(), p.cfg.PassEnv)
	g.Output = redactDiagnostic(g.Output) // before it becomes feedback, state or a question
	checks = append(checks, PlanCheckEntry{Name: "build", Passed: g.Passed && !g.Uncertain})
	if uncertain, damaged := settle(g); uncertain {
		return checks, "build", g.Output, true, nil
	} else if damaged != "" {
		return checks, "build", damaged, false, nil
	}
	if !g.Passed {
		return checks, "build", "the build fails with the new tests:\n" + g.Output, false, nil
	}
	dir := p.testDir()
	seen := map[string]bool{}
	var files []string
	for _, id := range sortedTraceIDs(plan.Trace) {
		for _, f := range plan.Trace[id] {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	sort.Strings(files)
	if p.state.Kind == PlanCharacterize {
		return p.green(ctx, wt, files, checks, settle)
	}
	for i, f := range files {
		g, err := p.acceptanceCommand(ctx, fmt.Sprintf("red-%d", i+1), wt, []string{dir + f})
		if err != nil {
			return checks, "", "", false, err
		}
		g.Output = redactDiagnostic(g.Output)
		checks = append(checks, PlanCheckEntry{Name: "red " + dir + f, Passed: !g.Passed && !g.Uncertain, Detail: tail(g.Output, 500)})
		if uncertain, damaged := settle(g); uncertain {
			return checks, "red", g.Output, true, nil
		} else if damaged != "" {
			return checks, "red", damaged, false, nil
		}
		if g.Passed {
			return checks, "red", fmt.Sprintf("%s%s passes before the feature exists; each test must fail until the criterion is implemented", dir, f), false, nil
		}
	}
	tests, missing, err := p.previousSuite()
	if err != nil {
		return checks, "", "", false, err
	}
	if len(missing) > 0 {
		return checks, "previous", "sealed tests of unchanged criteria are missing: " + strings.Join(missing, ", "), false, nil
	}
	if len(tests) > 0 {
		g, err := p.acceptanceCommand(ctx, "previous", wt, tests)
		if err != nil {
			return checks, "", "", false, err
		}
		g.Output = redactDiagnostic(g.Output)
		checks = append(checks, PlanCheckEntry{Name: "previous acceptance suite still passes", Passed: g.Passed && !g.Uncertain})
		if uncertain, damaged := settle(g); uncertain {
			return checks, "previous", g.Output, true, nil
		} else if damaged != "" {
			return checks, "previous", damaged, false, nil
		}
		if !g.Passed {
			return checks, "previous", "the previous acceptance suite fails with the new files:\n" + g.Output, false, nil
		}
	}
	return checks, "", "", false, nil
}

// green is the characterization check: every new test file passes on its own on the existing
// code, all of them pass together, and the previous acceptance suite still passes with them
// added. A pass is evidence, not proof: the QA reviewer judges that each test really runs and
// pins the confirmed behavior.
func (p *planner) green(ctx context.Context, wt string, files []string, checks []PlanCheckEntry,
	settle func(GateResult) (bool, string)) ([]PlanCheckEntry, string, string, bool, error) {
	dir := p.testDir()
	run := func(step, name string, manifest []string, what string) (string, string, bool, error) {
		g, err := p.acceptanceCommand(ctx, name, wt, manifest)
		if err != nil {
			return "", "", false, err
		}
		// Output of the owner's commands on adopted code may carry secrets: it is redacted
		// before it becomes feedback for the next author, state or a question on the terminal.
		output := redactDiagnostic(g.Output)
		checks = append(checks, PlanCheckEntry{Name: what, Passed: g.Passed && !g.Uncertain, Detail: tail(output, 500)})
		if uncertain, damaged := settle(g); uncertain {
			return step, output, true, nil
		} else if damaged != "" {
			return step, redactDiagnostic(damaged), false, nil
		}
		if !g.Passed {
			return step, what + " fails on the existing code; a characterization test must pass without any change:\n" + output, false, nil
		}
		return "", "", false, nil
	}
	var all []string
	for i, f := range files {
		all = append(all, dir+f)
		if step, msg, uncertain, err := run("green", fmt.Sprintf("green-%d", i+1), []string{dir + f}, "green "+dir+f); err != nil || step != "" {
			return checks, step, msg, uncertain, err
		}
	}
	if step, msg, uncertain, err := run("green", "green-all", all, "all new tests pass together"); err != nil || step != "" {
		return checks, step, msg, uncertain, err
	}
	tests, missing, err := p.previousSuite()
	if err != nil {
		return checks, "", "", false, err
	}
	if len(missing) > 0 {
		return checks, "previous", "sealed tests of unchanged criteria are missing: " + strings.Join(missing, ", "), false, nil
	}
	if len(tests) > 0 {
		with := append(append([]string{}, tests...), all...)
		if step, msg, uncertain, err := run("previous", "previous", with, "previous acceptance suite passes with the new tests"); err != nil || step != "" {
			return checks, step, msg, uncertain, err
		}
	}
	return checks, "", "", false, nil
}

// review asks one role's independent reviewer about the frozen plan commit. QA uses the plan
// review prompt; security and DBA use their usual prompts on the same diff.
func (p *planner) review(ctx context.Context, role, author, commit, wt, diff, planYAML, runDir string) (ReviewVerdict, GateResult, error) {
	g := GateResult{Name: "review"}
	name := "review-" + role + ".md"
	if role == RoleQA {
		name = p.qaPrompt()
	}
	v := ReviewVerdict{Role: role, SHA: commit, Verdict: "INVALID", PromptSource: p.sources[name]}
	profile, err := p.cfg.reviewerFor(role, author)
	if err != nil {
		return v, g, err
	}
	v.Profile, v.Provider = profile, p.cfg.Profiles[profile].Provider
	data := newPlanPromptData(p.input())
	data.PlanYAML, data.Diff = planYAML, diff
	data.Task = Task{ID: "T1", Goal: "acceptance tests and task of " + p.state.ID}
	prompt, err := renderPrompt(name, p.prompts[name], data)
	if err != nil {
		return v, g, err
	}
	p.logf("    review %s with %q (%s) on %s", role, profile, v.Provider, short(commit))
	start := time.Now()
	outPath, err := runAgent(ctx, p.cfg.Profiles[profile], p.cfg.PassEnv, wt, prompt, runDir, "review-"+role)
	g.Seconds = time.Since(start).Seconds()
	checkoutErr := verifyCheckout(wt, commit)
	if err != nil {
		g.Output, g.Uncertain = err.Error(), uncertainExit(err)
		if checkoutErr != nil {
			g.Output += "\n" + checkoutErr.Error()
		}
		return v, g, nil
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return v, g, fmt.Errorf("read reviewer output: %w", err)
	}
	switch parseVerdict(string(out)) {
	case "APPROVE":
		g.Passed, v.Verdict = true, "APPROVE"
	case "REJECT":
		v.Verdict = "REJECT"
		g.Output = role + " reviewer rejected the plan:\n" + tail(string(out), maxGateOutput)
	default:
		g.Output = role + " reviewer gave no verdict"
	}
	if checkoutErr != nil {
		g.Passed, g.Output, v.Verdict = false, checkoutErr.Error(), "INVALID"
	}
	return v, g, nil
}

// publish creates aeh/<ID> at the verified plan commit. The update only creates the branch,
// so a crash between choosing the commit and recording it is recovered by looking at the
// branch: at the chosen commit means done; at another commit is a conflict.
func (p *planner) publish() error {
	st := p.state
	branch := IntegrationBranch(st.ID)
	current, exists := branchCommit(p.repo.Root, branch)
	switch {
	case exists && current == st.PlanCommit:
	case exists:
		return p.restart(fmt.Sprintf("Branch %s already exists at %s, not at the verified plan commit %s. Inspect it; aeh will not move it.", branch, short(current), short(st.PlanCommit)))
	default:
		if _, err := git(p.repo.Root, "update-ref", "refs/heads/"+branch, st.PlanCommit, strings.Repeat("0", len(st.PlanCommit))); err != nil {
			return fmt.Errorf("publish %s: %w", branch, err)
		}
	}
	st.Phase = PlanPublished
	return savePlanState(p.repo, st)
}

// logMemory appends the plan to memory's log. The spec itself does not change: the
// implementation is recorded when aeh run accepts the outcome.
func (p *planner) logMemory() error {
	st := p.state
	if st.Kind == PlanCharacterize {
		return p.recordCharacterization()
	}
	marker := "AEH-Plan: " + st.ID + " " + st.PlanCommit
	done := func() error {
		st.Phase, st.Status = PlanLogged, PlanLogged
		return savePlanState(p.repo, st)
	}
	// A crash after the memory commit: the marker is there, nothing to append.
	if found, err := git(p.memDir, "log", "--format=%H", "--fixed-strings", "--grep="+marker); err != nil {
		return err
	} else if found != "" {
		return done()
	}
	today := memoryNow().UTC().Format("2006-01-02")
	subject := fmt.Sprintf("plan %s %s (%s)", st.Feature, st.SpecVersion, st.ID)
	if err := memoryClean(p.memDir); err != nil {
		// A crash between writing the log and committing it: commit that entry if it is the
		// only change, otherwise leave memory to the owner.
		if p.pendingLogEntry() {
			if err := commitMemory(p.memDir, []string{"wiki/log.md"}, subject, marker); err != nil {
				return fmt.Errorf("log plan %s: %w", st.ID, err)
			}
			return done()
		}
		return fmt.Errorf("log plan %s: %w; commit memory edits, then run aeh plan %s again to finish", st.ID, err, st.Feature)
	}
	w := memoryWrites{}
	err := appendLog(p.memDir, w, fmt.Sprintf("[%s] plan | %s %s -> %s @ %s", today, st.Feature, st.SpecVersion, st.ID, short(st.PlanCommit)))
	if err == nil {
		err = w.commit(p.memDir, subject, marker)
	}
	if err != nil {
		return fmt.Errorf("log plan %s: %w", st.ID, w.restore(p.memDir, err))
	}
	return done()
}

// recordCharacterization records the published commit as the feature's first implementation:
// code_sha is the commit that holds the sealed tests, observed_base_sha the inspected commit.
// A later plan requires code_sha in its base, that is, merged by the owner. RecordChange is
// idempotent by outcome and commit, so a crash after the memory commit only repeats it.
func (p *planner) recordCharacterization() error {
	st := p.state
	planYAML, ok := gitBlob(p.repo.Root, st.PlanCommit, p.testDir()+"plan.yaml")
	if !ok {
		return fmt.Errorf("characterization %s: plan commit %s has no plan.yaml", st.ID, short(st.PlanCommit))
	}
	plan, err := parsePlanFile([]byte(planYAML))
	if err != nil {
		return fmt.Errorf("characterization %s: %w", st.ID, err)
	}
	im := &Implementation{Outcome: st.ID, Approval: st.SpecDigest, SpecVersion: st.SpecVersion, Code: st.PlanCommit, ObservedBase: st.ObservedBase}
	for _, c := range st.Delta.New {
		var tests []string
		for _, f := range plan.Trace[fmt.Sprintf("AC-%d", c.ID)] {
			tests = append(tests, p.testDir()+f)
		}
		if len(tests) == 0 {
			return fmt.Errorf("characterization %s has no tests for AC-%d", st.ID, c.ID)
		}
		im.Criteria = append(im.Criteria, ImplementedCriterion{ID: c.ID, Text: c.Text, Tests: tests})
	}
	sort.Slice(im.Criteria, func(i, j int) bool { return im.Criteria[i].ID < im.Criteria[j].ID })
	note := fmt.Sprintf("characterization tests of the existing behavior observed at %s; merge %s", short(st.ObservedBase), IntegrationBranch(st.ID))
	rec := PendingRecord{OutcomeID: st.ID, Feature: st.Feature, Change: "minor", Note: note, Commit: st.PlanCommit, Implementation: im, MemoryBase: st.MemoryCommit}
	if _, _, err := RecordChange(p.repo, rec, ""); err != nil {
		return fmt.Errorf("characterization %s: %w; run aeh adopt seal %s again to finish", st.ID, err, st.Feature)
	}
	st.Phase, st.Status = PlanLogged, PlanLogged
	return savePlanState(p.repo, st)
}

// pendingLogEntry reports that memory's only change is this plan's uncommitted log entry.
func (p *planner) pendingLogEntry() bool {
	st := p.state
	// Written, staged or both: wiki/log.md must be the only path that differs from HEAD, and
	// nothing may be untracked.
	changed, err := git(p.memDir, "diff", "HEAD", "--name-only", "--no-renames")
	if err != nil || changed != "wiki/log.md" {
		return false
	}
	if untracked, err := git(p.memDir, "ls-files", "--others", "--exclude-standard"); err != nil || untracked != "" {
		return false
	}
	diff, err := git(p.memDir, "diff", "HEAD", "--unified=0", "--no-color", "--", "wiki/log.md")
	if err != nil {
		return false
	}
	entry := "plan | " + st.Feature + " " + st.SpecVersion + " -> " + st.ID + " @ " + short(st.PlanCommit)
	added := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), !strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "-"):
		case strings.HasPrefix(l, "-"):
			return false
		case strings.TrimSpace(strings.TrimPrefix(l, "+")) == "":
		case strings.Contains(l, entry):
			added++
		default:
			return false
		}
	}
	return added == 1
}

func (p *planner) block(question string) error {
	st := p.state
	st.Status, st.Question = StatusBlocked, question
	if err := savePlanState(p.repo, st); err != nil {
		return err
	}
	p.logf("%s blocked:\n%s", st.ID, question)
	return nil
}

func (p *planner) restart(question string) error {
	p.state.RestartRequired = true
	if err := p.block(question); err != nil {
		return err
	}
	return errors.New(p.state.Question) // redacted by the save
}

// answerPlan records the owner's answer to a blocked plan so aeh plan can retry it.
func answerPlan(r Repo, id, text string) error {
	main, err := MainCheckout(r)
	if err != nil {
		return err
	}
	if !samePath(main, r.Root) {
		// Plans live in the main checkout; take its outcome lock too, then reload.
		unlock, err := lockOutcome(Repo{Root: main}, id)
		if err != nil {
			return err
		}
		defer unlock()
	}
	st, ok, err := LoadPlanState(r, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("plan %s does not exist", id)
	}
	if st.RestartRequired {
		return errors.New(st.Question)
	}
	if st.Status != StatusBlocked {
		return fmt.Errorf("plan %s is not blocked (phase %s)", st.ID, st.Phase)
	}
	st.Answers = append(st.Answers, strings.TrimSpace(text))
	st.Question, st.Status, st.LadderFrom = "", StatusPending, len(st.Attempts)
	return savePlanState(r, &st)
}

// ListPlanStates returns every plan of the main checkout, most recently updated first.
func ListPlanStates(r Repo) ([]PlanState, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(main, ".aeh", "state"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PlanState
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".plan.json")
		if !ok || e.IsDir() {
			continue
		}
		st, found, err := LoadPlanState(r, id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
