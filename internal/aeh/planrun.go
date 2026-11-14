package aeh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	Version         int              `json:"version"`
	ID              string           `json:"id"`
	Feature         string           `json:"feature"`
	Phase           string           `json:"phase"`
	Status          string           `json:"status"` // running, blocked or the phase's end state
	Base            string           `json:"base"`
	SpecVersion     string           `json:"spec_version"`
	SpecDigest      string           `json:"spec_content_sha256"`
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
	return writePrivateJSON(p, s)
}

// planner carries one planning session.
type planner struct {
	repo    Repo
	cfg     Config
	pcfg    PlanConfig
	spec    Spec
	memDir  string
	state   *PlanState
	log     io.Writer
	prompts map[string]string // plan.md, review-plan.md and review-<role>.md
	sources map[string]string
}

func (p *planner) logf(format string, args ...any) {
	if p.log != nil {
		fmt.Fprintf(p.log, format+"\n", args...)
	}
}

// PlanFeature plans the approved spec of a feature: it pins the inputs, checks the base,
// lets a test author write acceptance tests and one task, checks and reviews the frozen
// result, and publishes it on aeh/<ID>. Calling it again resumes an unfinished plan.
func PlanFeature(ctx context.Context, r Repo, feature string, log io.Writer) (PlanState, error) {
	unlockProject, err := lockProject(r)
	if err != nil {
		return PlanState{}, err
	}
	defer unlockProject()
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
	if err := memoryClean(memory.Dir); err != nil {
		return PlanState{}, fmt.Errorf("plan %s: %w; commit your memory edits first", feature, err)
	}
	spec, ok := memory.Spec(feature)
	if !ok {
		return PlanState{}, fmt.Errorf("feature %q does not exist in memory", feature)
	}
	if !spec.Approved() {
		return PlanState{}, fmt.Errorf("feature %s is not approved as it reads now; approve it first: aeh memory approve %s", feature, feature)
	}
	id, err := generatedOutcomeID(feature, spec.Version)
	if err != nil {
		return PlanState{}, err
	}
	unlock, err := lockOutcome(r, id)
	if err != nil {
		return PlanState{}, err
	}
	defer unlock()

	p := &planner{repo: r, cfg: cfg, pcfg: cfg.plan(), spec: spec, memDir: memory.Dir, log: log}
	if err := p.loadPrompts(); err != nil {
		return PlanState{}, err
	}
	st, exists, err := LoadPlanState(r, id)
	if err != nil {
		return st, err
	}
	if !exists {
		st, err = p.reserve(id)
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
	if inputs != st.Inputs || st.SpecDigest != spec.Approval.Content {
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
	p.logf("%s planned on %s at %s. Build it with: aeh run %s", id, IntegrationBranch(id), short(st.PlanCommit), id)
	return st, nil
}

func (p *planner) loadPrompts() error {
	p.prompts, p.sources = map[string]string{}, map[string]string{}
	for _, name := range []string{"plan.md", "review-plan.md"} {
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
func (p *planner) reserve(id string) (PlanState, error) {
	st := PlanState{Version: planStateVersion, ID: id, Feature: p.spec.Feature, Phase: PlanReserved, Status: StatusRunning,
		SpecVersion: p.spec.Version, SpecDigest: p.spec.Approval.Content}
	if err := p.checkCollisions(id); err != nil {
		return st, err
	}
	crit, _, err := checkSpecCriteria(p.memDir, p.spec)
	if err != nil {
		return st, fmt.Errorf("plan %s: acceptance criteria: %w", p.spec.Feature, err)
	}
	if len(crit) == 0 {
		return st, fmt.Errorf("plan %s: the spec has no %q section; add criteria and approve it", p.spec.Feature, criteriaHeading)
	}
	base, err := headCommit(p.repo.Root)
	if err != nil {
		return st, err
	}
	st.Base = base
	var last *Implementation
	if im, ok := p.spec.LastImplementation(); ok {
		last = &im
		st.PriorCode = im.Code
		if _, err := git(p.repo.Root, "merge-base", "--is-ancestor", im.Code, base); err != nil {
			return st, fmt.Errorf("plan %s: the last implementation (%s on %s) is not in the base %s; merge it first, then run aeh plan again",
				p.spec.Feature, short(im.Code), IntegrationBranch(im.Outcome), short(base))
		}
	}
	st.Delta = criteriaDelta(crit, last)
	switch {
	case len(st.Delta.Changed) > 0 || len(st.Delta.Removed) > 0:
		return st, fmt.Errorf("plan %s: criteria changed %v or removed %v since %s; replacing sealed tests is not supported yet. Restore them, or keep them and add new criteria",
			p.spec.Feature, st.Delta.Changed, st.Delta.Removed, last.Outcome)
	case len(st.Delta.New) == 0:
		return st, fmt.Errorf("plan %s: the approved spec adds no criteria; nothing to plan", p.spec.Feature)
	}
	st.Route = RouteChange
	if last == nil {
		st.Route = RouteNew
	}
	st.Roles = requiredRoles(st.Route)
	if st.Inputs, err = p.inputsDigestFor(&st); err != nil {
		return st, err
	}
	p.state = &st
	p.logf("%s: planning %s %s (%d new criteria, route %s, base %s)", id, p.spec.Feature, p.spec.Version, len(st.Delta.New), st.Route, short(base))
	return st, savePlanState(p.repo, &st)
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
				name := strings.TrimSuffix(e.Name(), ext)
				if name != e.Name() && same(name) && !(ext == ".lock" && e.Name() == id+".lock") {
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
	tests, _ := git(p.repo.Root, "ls-tree", "-d", "--name-only", "HEAD", p.pcfg.AcceptanceDir+"/")
	for _, d := range strings.Fields(tests) {
		if same(filepath.Base(d)) {
			return fmt.Errorf("outcome ID %s is already used by the test directory %s", id, d)
		}
	}
	return nil
}

// inputsDigest fingerprints everything that decides the plan, so a resume never mixes
// inputs: base, spec, prior implementation, delta, configuration and prompts.
func (p *planner) inputsDigest() (string, error) { return p.inputsDigestFor(p.state) }

func (p *planner) inputsDigestFor(st *PlanState) (string, error) {
	v := struct {
		Base, Spec, Prior string
		Delta             CriteriaDelta
		Plan              PlanConfig
		Commands          any
		Protected         []string
		Rules             []TriageRule
		Reviewers         ReviewerPolicy
		Profiles          map[string]Profile
		Prompts           map[string]string
	}{st.Base, st.SpecDigest, st.PriorCode, st.Delta, p.pcfg, p.cfg.Commands, p.cfg.Protected, p.cfg.Triage.Rules, p.cfg.Reviewers, p.cfg.Profiles, p.prompts}
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
func (p *planner) acceptanceCommand(ctx context.Context, name, wt, runDir string, files []string) (GateResult, error) {
	manifest := filepath.Join(runDir, name+".manifest")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return GateResult{}, err
	}
	if err := os.WriteFile(manifest, []byte(strings.Join(files, "\n")+"\n"), 0o600); err != nil {
		return GateResult{}, err
	}
	command := strings.Replace(p.cfg.Commands.Acceptance, "{{manifest}}", filepath.ToSlash(manifest), 1)
	return gateCommand(ctx, name, wt, command, p.timeout(), p.cfg.PassEnv), nil
}

func (p *planner) timeout() time.Duration {
	return time.Duration(p.cfg.Commands.TimeoutMinutes) * time.Minute
}

func (p *planner) unchangedTests() []string {
	var files []string
	for _, c := range p.state.Delta.Unchanged {
		files = append(files, c.Tests...)
	}
	sort.Strings(files)
	return files
}

// baseline checks, before any agent runs, that the base builds, passes its tests and passes
// the sealed tests of unchanged criteria. A red base is the environment's problem, not the
// test author's, so it blocks without consuming attempts.
func (p *planner) baseline(ctx context.Context) (bool, string, error) {
	st := p.state
	runDir := p.repo.RunDir(st.ID, "plan-baseline")
	wt := p.repo.WorktreePath(st.ID + "-plan-baseline")
	if err := addWorktreeDetached(p.repo.Root, wt, st.Base); err != nil {
		return false, "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(p.repo.Root, wt)
		}
	}()
	if err := verifyCheckout(wt, st.Base); err != nil {
		return false, "", err
	}
	p.logf("  baseline on %s", short(st.Base))
	steps := []func() (GateResult, error){
		func() (GateResult, error) {
			return gateCommand(ctx, "build", wt, p.cfg.Commands.Build, p.timeout(), p.cfg.PassEnv), nil
		},
		func() (GateResult, error) {
			return gateCommand(ctx, "test", wt, p.cfg.Commands.Test, p.timeout(), p.cfg.PassEnv), nil
		},
	}
	if tests := p.unchangedTests(); len(tests) > 0 {
		steps = append(steps, func() (GateResult, error) { return p.acceptanceCommand(ctx, "sealed", wt, runDir, tests) })
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
	feedback := ""
	if n := len(st.Attempts); n > 0 {
		feedback = st.Attempts[n-1].Feedback
	}
	if len(st.Answers) > 0 {
		feedback = strings.TrimSpace(feedback + "\n\nThe owner answered: " + strings.Join(st.Answers, "\n"))
	}
	for _, profile := range p.pcfg.Ladder {
		for try := 0; try < 2; try++ {
			a, question, err := p.attempt(ctx, len(st.Attempts)+1, profile, feedback)
			st.Attempts = append(st.Attempts, a)
			if serr := savePlanState(p.repo, st); serr != nil && err == nil {
				err = serr
			}
			if err != nil {
				return false, "", err
			}
			if question != "" {
				return false, question, nil
			}
			if a.FailedStep == "" {
				st.Phase, st.PlanCommit, st.Author, st.Reviews = PlanPublishing, a.Frozen, profile, a.Reviews
				return true, "", savePlanState(p.repo, st)
			}
			feedback = fmt.Sprintf("Your previous attempt failed at %s:\n%s", a.FailedStep, a.Feedback)
		}
	}
	last := st.Attempts[len(st.Attempts)-1]
	return false, fmt.Sprintf("No plan author produced an acceptable plan. Last failure at %s:\n%s\nWhat should change? Answer to retry the plan ladder.", last.FailedStep, tail(last.Feedback, 2000)), nil
}

// attempt runs the test author once and checks its candidate. A non-empty question means
// the plan must block now (the author asked to split, or a command was uncertain).
func (p *planner) attempt(ctx context.Context, n int, profile, feedback string) (PlanAttempt, string, error) {
	st, r := p.state, p.repo
	name := fmt.Sprintf("plan-a%d", n)
	runDir := r.RunDir(st.ID, name)
	a := PlanAttempt{N: n, Profile: profile, RunDir: rel(r.Root, runDir)}
	start := time.Now()
	defer func() { a.Seconds = time.Since(start).Seconds() }()
	fail := func(step, msg string) (PlanAttempt, string, error) {
		a.FailedStep, a.Feedback = step, msg
		p.logf("    %-8s FAIL", step)
		a.Seconds = time.Since(start).Seconds()
		return a, "", nil
	}
	wt := r.WorktreePath(st.ID + "-" + name)
	if err := addWorktreeDetached(r.Root, wt, st.Base); err != nil {
		return a, "", err
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
	prompt, err := renderPrompt("plan.md", p.prompts["plan.md"], data)
	if err != nil {
		return a, "", err
	}
	p.logf("  plan attempt %d with profile %q", n, profile)
	outPath, err := runAgent(ctx, p.cfg.Profiles[profile], p.cfg.PassEnv, wt, prompt, runDir, "plan")
	if err != nil {
		if uncertainExit(err) {
			keep = true
			return a, fmt.Sprintf("The plan author %q was interrupted or timed out (worktree kept at %s). Confirm all processes stopped and review possible effects before answering to retry.", profile, wt), nil
		}
		return fail("author", err.Error())
	}
	out, _ := os.ReadFile(outPath)
	switch planVerdict(string(out)) {
	case "TOO-LARGE":
		a.FailedStep, a.Feedback = "author", "the author says the new criteria do not fit one task:\n"+tail(string(out), 2000)
		return a, fmt.Sprintf("The plan author says the new criteria of %s %s do not fit one task of %d changed lines:\n%s\nSplit the spec into smaller versions (approve fewer new criteria at a time), or answer to retry.",
			st.Feature, st.SpecVersion, p.pcfg.MaxDiffLines, tail(string(out), 1500)), nil
	case "READY":
	default:
		return fail("author", `the author's output must end with "PLAN: READY" or "PLAN: TOO-LARGE"`)
	}
	candidate, err := candidateCommit(wt, st.Base, fmt.Sprintf("aeh: plan %s (attempt %d, %s)", st.ID, n, profile))
	if err != nil {
		return fail("scope", err.Error())
	}
	a.Candidate = candidate
	files, err := planCandidateFiles(r.Root, st.Base, candidate)
	if err != nil {
		return a, "", err
	}
	dir := p.testDir()
	planYAML, ok := gitBlob(r.Root, candidate, dir+"plan.yaml")
	if !ok {
		return fail("scope", fmt.Sprintf("missing %splan.yaml", dir))
	}
	plan, err := parsePlanFile([]byte(planYAML))
	if err != nil {
		return fail("form", err.Error())
	}
	if err := validatePlanCandidate(in, files, plan); err != nil {
		return fail("scope", err.Error())
	}
	// Rules on the planned task's paths can raise the route and add reviewers before review.
	route, roles := applyRules(st.Route, st.Roles, rulesForTasks(p.cfg.Triage.Rules, []Task{{ID: "T1", Paths: plan.Task.Paths}}))
	frozen, err := p.freeze(wt, candidate, files, plan, route)
	if err != nil {
		return a, "", err
	}
	a.Frozen = frozen
	if err := writePrivateJSON(filepath.Join(runDir, "candidate.json"), map[string]string{"base": st.Base, "candidate": candidate, "frozen": frozen}); err != nil {
		return a, "", err
	}
	diff, err := candidateDiff(r.Root, st.Base, frozen)
	if err != nil {
		return a, "", err
	}
	if err := os.WriteFile(filepath.Join(runDir, "diff.patch"), []byte(diff), 0o600); err != nil {
		return a, "", err
	}

	vwt := wt + "-verify"
	if err := addWorktreeDetached(r.Root, vwt, frozen); err != nil {
		return a, "", err
	}
	keepVerify := false
	defer func() {
		if !keepVerify {
			removeWorktree(r.Root, vwt)
		}
	}()
	if err := verifyCheckout(vwt, frozen); err != nil {
		return a, "", err
	}
	checks, step, msg, uncertain, err := p.redGreen(ctx, vwt, frozen, runDir, plan)
	if err != nil {
		return a, "", err
	}
	_ = writePrivateJSON(filepath.Join(runDir, "checks.json"), checks)
	if uncertain {
		keepVerify = true
		a.FailedStep, a.Feedback = step, msg
		return a, fmt.Sprintf("Plan check %s was interrupted or timed out (worktree kept at %s). Confirm all processes stopped, then answer to retry.", step, vwt), nil
	}
	if step != "" {
		return fail(step, msg)
	}
	p.state.Checks = checks
	for _, role := range roleOrder {
		if !containsString(roles, role) {
			continue
		}
		v, g, err := p.review(ctx, role, profile, frozen, vwt, diff, planYAML, runDir)
		if err != nil {
			return a, "", err
		}
		a.Reviews = append(a.Reviews, v)
		if g.Uncertain {
			keepVerify = true
			a.FailedStep, a.Feedback = "review", g.Output
			return a, fmt.Sprintf("The %s review of the plan was interrupted or timed out (worktree kept at %s). Confirm all processes stopped, then answer to retry.", role, vwt), nil
		}
		if !g.Passed {
			return fail("review", g.Output)
		}
	}
	st.Route, st.Roles = route, roles
	return a, "", nil
}

func (p *planner) testDir() string { return p.pcfg.AcceptanceDir + "/" + p.state.ID + "/" }

func (p *planner) input() PlanInput {
	return PlanInput{OutcomeID: p.state.ID, Spec: p.spec, Delta: p.state.Delta, Protected: p.cfg.Protected, Config: p.cfg}
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
func (p *planner) freeze(wt, candidate string, files []PlanCandidateFile, plan PlanFile, route string) (string, error) {
	st := p.state
	contents := map[string][]byte{}
	for _, f := range files {
		b, ok := gitBlob(p.repo.Root, candidate, f.Path)
		if !ok {
			return "", fmt.Errorf("read %s from the candidate", f.Path)
		}
		contents[f.Path] = []byte(b)
	}
	var ids []string
	for _, c := range st.Delta.New {
		ids = append(ids, fmt.Sprintf("AC-%d", c.ID))
	}
	o := generatedOutcome(p.input(), plan, route, fmt.Sprintf("Adds %s.", strings.Join(ids, ", ")))
	outcome, err := marshalOutcome(o)
	if err != nil {
		return "", err
	}
	seal := sealFromContents(st.ID, contents, st.SpecDigest, time.Now().UTC())
	sealJSON, err := json.MarshalIndent(seal, "", "  ")
	if err != nil {
		return "", err
	}
	for rel, b := range map[string][]byte{
		"aeh/outcomes/" + st.ID + ".yaml": outcome,
		"aeh/seals/" + st.ID + ".json":    append(sealJSON, '\n'),
	} {
		full := filepath.Join(wt, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			return "", err
		}
	}
	frozen, err := candidateCommit(wt, st.Base, fmt.Sprintf("aeh: plan %s for %s %s\n\nAcceptance tests and one task for %s, sealed by aeh.",
		st.ID, st.Feature, st.SpecVersion, strings.Join(ids, ", ")))
	if err != nil {
		return "", err
	}
	// The frozen commit adds exactly the candidate's files and the two generated ones.
	got, err := planCandidateFiles(p.repo.Root, st.Base, frozen)
	if err != nil {
		return "", err
	}
	want := map[string]bool{"aeh/outcomes/" + st.ID + ".yaml": true, "aeh/seals/" + st.ID + ".json": true}
	for _, f := range files {
		want[f.Path] = true
	}
	if len(got) != len(want) {
		return "", fmt.Errorf("frozen plan commit changes %d paths, expected %d", len(got), len(want))
	}
	for _, f := range got {
		if !want[f.Path] || f.Status != 'A' {
			return "", fmt.Errorf("frozen plan commit has unexpected change %c %s", f.Status, f.Path)
		}
	}
	return frozen, nil
}

// redGreen checks the frozen commit: it builds, every new test file fails on its own (the
// feature does not exist yet), and the sealed tests of unchanged criteria still pass. Each
// command runs in the verify worktree, whose checkout must stay intact even when a command
// fails as expected. A failure is evidence, not proof: the reviewer judges why it fails.
func (p *planner) redGreen(ctx context.Context, wt, commit, runDir string, plan PlanFile) ([]PlanCheckEntry, string, string, bool, error) {
	var checks []PlanCheckEntry
	run := func(g GateResult) (GateResult, error) {
		if err := verifyCheckout(wt, commit); err != nil {
			return g, fmt.Errorf("plan check %s modified the checkout: %w", g.Name, err)
		}
		return g, nil
	}
	g, err := run(gateCommand(ctx, "build", wt, p.cfg.Commands.Build, p.timeout(), p.cfg.PassEnv))
	if err != nil {
		return checks, "build", err.Error(), false, nil
	}
	checks = append(checks, PlanCheckEntry{Name: "build", Passed: g.Passed})
	if g.Uncertain {
		return checks, "build", g.Output, true, nil
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
	for i, f := range files {
		g, err := p.acceptanceCommand(ctx, fmt.Sprintf("red-%d", i+1), wt, runDir, []string{dir + f})
		if err != nil {
			return checks, "", "", false, err
		}
		if g, err = run(g); err != nil {
			return checks, "red", err.Error(), false, nil
		}
		checks = append(checks, PlanCheckEntry{Name: "red " + dir + f, Passed: !g.Passed, Detail: tail(g.Output, 500)})
		if g.Uncertain {
			return checks, "red", g.Output, true, nil
		}
		if g.Passed {
			return checks, "red", fmt.Sprintf("%s%s passes before the feature exists; each test must fail until the criterion is implemented", dir, f), false, nil
		}
	}
	if tests := p.unchangedTests(); len(tests) > 0 {
		g, err := p.acceptanceCommand(ctx, "sealed", wt, runDir, tests)
		if err != nil {
			return checks, "", "", false, err
		}
		if g, err = run(g); err != nil {
			return checks, "sealed", err.Error(), false, nil
		}
		checks = append(checks, PlanCheckEntry{Name: "sealed tests still pass", Passed: g.Passed})
		if g.Uncertain {
			return checks, "sealed", g.Output, true, nil
		}
		if !g.Passed {
			return checks, "sealed", "the sealed tests of unchanged criteria fail with the new files:\n" + g.Output, false, nil
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
		name = "review-plan.md"
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
	if err := memoryClean(p.memDir); err != nil {
		return fmt.Errorf("log plan %s: %w; commit memory edits, then run aeh plan %s again to finish", st.ID, err, st.Feature)
	}
	today := memoryNow().UTC().Format("2006-01-02")
	w := memoryWrites{}
	err := appendLog(p.memDir, w, fmt.Sprintf("[%s] plan | %s %s -> %s @ %s", today, st.Feature, st.SpecVersion, st.ID, short(st.PlanCommit)))
	if err == nil {
		err = w.commit(p.memDir, fmt.Sprintf("plan %s %s (%s)", st.Feature, st.SpecVersion, st.ID), "AEH-Plan: "+st.ID+" "+st.PlanCommit)
	}
	if err != nil {
		return fmt.Errorf("log plan %s: %w", st.ID, w.restore(p.memDir, err))
	}
	st.Phase, st.Status = PlanLogged, PlanLogged
	return savePlanState(p.repo, st)
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
	return errors.New(question)
}

// answerPlan records the owner's answer to a blocked plan so aeh plan can retry it.
func answerPlan(r Repo, st PlanState, text string) error {
	if st.RestartRequired {
		return errors.New(st.Question)
	}
	if st.Status != StatusBlocked {
		return fmt.Errorf("plan %s is not blocked (phase %s)", st.ID, st.Phase)
	}
	st.Answers = append(st.Answers, strings.TrimSpace(text))
	st.Question, st.Status = "", StatusPending
	return savePlanState(r, &st)
}
