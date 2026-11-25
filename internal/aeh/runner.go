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
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// maxAttempts follows the ladder: same profile, same profile with feedback, next profile.
const maxAttempts = 3

// runner holds what one `aeh run` or `aeh do` needs. It is created per run.
type runner struct {
	repo            Repo
	cfg             Config
	outcome         Outcome
	seal            *Seal
	protected       []string
	state           *State
	log             io.Writer
	implementPrompt string
	reviewPrompts   map[string]string
	reviewSources   map[string]string
	triagePrompt    string
	memory          MemorySnapshot
	generated       *generatedRun
	sb              *sandbox // nil when gate commands run on the host
	// memoryMount is the memory commit sandboxed agents of this invocation see, fixed
	// at its first agent launch.
	memoryMount *[2]string
}

// agentMemory is the memory export of this run's sandboxed agents: the plan's pinned
// memory commit for a planned run, otherwise memory's HEAD when the first agent starts.
func (ru *runner) agentMemory() (dir, commit string, err error) {
	if ru.memoryMount == nil {
		dir, commit, err := stepMemory(ru.memory)
		if err != nil {
			return "", "", err
		}
		ru.memoryMount = &[2]string{dir, commit}
	}
	return ru.memoryMount[0], ru.memoryMount[1], nil
}

// generatedRun is an immutable snapshot of the plan provenance and its Git manifests.
type generatedRun struct {
	Plan     PlanState
	Seal     Seal
	Manifest Seal
	Paths    []string
	Trace    PlanFile
}

// RunOutcome executes an outcome from aeh/outcomes/<id>.yaml until it is accepted or blocked.
func RunOutcome(ctx context.Context, r Repo, id string, log io.Writer) (State, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return State{}, err
	}
	r = Repo{Root: main}
	unlock, err := lockLiveProject(r)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	plan, generated, err := LoadPlanState(r, id)
	if err != nil {
		return State{}, err
	}
	if plan.Status == StatusAbandoned {
		return State{}, fmt.Errorf("%s is abandoned and cannot resume", id)
	}
	if generated && plan.Kind == PlanCharacterize {
		return State{}, characterizeRunError(id)
	}
	if generated && plan.NoImplementation {
		return State{}, fmt.Errorf("%s has nothing to implement: it only retires or clarifies tests and was recorded when planned; merge %s", id, IntegrationBranch(id))
	}
	if generated {
		if plan.Phase != PlanLogged {
			return State{}, fmt.Errorf("plan %s is not finished; run aeh plan %s", id, plan.Feature)
		}
		main, err := MainCheckout(r)
		if err != nil {
			return State{}, err
		}
		r = Repo{Root: main}
		cfg, err := r.LoadConfig()
		if err != nil {
			return State{}, err
		}
		o, gen, err := loadGeneratedRun(r, cfg, plan)
		if err != nil {
			return State{}, err
		}
		return run(ctx, r, cfg, o, log, gen)
	}
	cfg, err := r.LoadConfig()
	if err != nil {
		return State{}, err
	}
	o, err := r.LoadOutcome(id)
	_, outcomeStatErr := os.Stat(r.OutcomePath(id))
	if err != nil && errors.Is(outcomeStatErr, os.ErrNotExist) && strings.HasPrefix(id, "DO-") {
		s, stateErr := loadState(r, id)
		if stateErr != nil {
			return s, stateErr
		}
		// Only a paused question resumes an aeh do: other blocks are refined with a new
		// aeh do. The stored contract is checked like an outcome file.
		if s.ResumeOutcome != nil && s.Cursor != nil && s.ResumeOutcome.ID == id {
			o = *s.ResumeOutcome
			err = o.normalize()
		}
	}
	if err != nil {
		return State{}, err
	}
	if o.Kind == PlanCharacterize {
		return State{}, characterizeRunError(id)
	}
	if o.Kind == PlanRevise {
		return State{}, fmt.Errorf("%s has nothing to implement; merge %s", id, IntegrationBranch(id))
	}
	return run(ctx, r, cfg, o, log, nil)
}

func characterizeRunError(id string) error {
	return fmt.Errorf("%s characterizes existing behavior: nothing to implement; merge %s", id, IntegrationBranch(id))
}

// Do runs a level-1 change described in one sentence, with route-based reviews.
// link declares the feature the change modifies, enforced by the drift gate.
// A nonempty route overrides model triage, just like route: in an outcome.
func Do(ctx context.Context, r Repo, text string, paths []string, profile string, link MemoryLink, log io.Writer, route string) (State, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return State{}, err
	}
	r = Repo{Root: main}
	unlock, err := lockLiveProject(r)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	if route != "" && !validRoute(route) {
		return State{}, errors.New("route must be trivial, change or new")
	}
	if err := link.Validate(); err != nil {
		return State{}, err
	}
	cfg, err := r.LoadConfig()
	if err != nil {
		return State{}, err
	}
	if profile != "" {
		if err := cfg.checkProfile(profile); err != nil {
			return State{}, err
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return State{}, errors.New("describe the change: aeh do \"...\"")
	}
	o := Outcome{
		ID:      fmt.Sprintf("DO-%d", time.Now().UnixNano()),
		Title:   firstLine(text),
		Level:   1,
		Route:   route,
		Feature: link.Feature, Change: link.Change, Changelog: link.Note,
		Tasks: []Task{{ID: "T1", Goal: text, Paths: paths, Start: profile}},
	}
	return run(ctx, r, cfg, o, log, nil)
}

// run executes an outcome; gen is the pinned plan provenance of a generated outcome, or nil.
func run(ctx context.Context, r Repo, cfg Config, o Outcome, log io.Writer, gen *generatedRun) (State, error) {
	// RunOutcome and Do hold the project lock before reading any work registry.
	var paths []string
	for _, t := range o.Tasks {
		paths = append(paths, t.Paths...)
	}
	if len(paths) == 0 {
		paths = []string{"**"}
	}
	if err := admitWork(r, o.Feature, paths, o.ID); err != nil {
		return State{}, err
	}
	unlock, err := lockOutcome(r, o.ID)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	memory, err := LoadMemory(r)
	if err != nil {
		return State{}, fmt.Errorf("load run memory: %w", err)
	}
	st, err := loadState(r, o.ID)
	if err != nil {
		return st, err
	}
	if st.Status == StatusAbandoned {
		return st, fmt.Errorf("%s is abandoned and cannot resume", o.ID)
	}
	if st.Version != 2 {
		return st, errors.New("pre-stage-2 state cannot resume; relaunch with another outcome ID")
	}
	if st.RestartRequired {
		return st, errors.New(st.Question)
	}
	if st.Title == "" {
		st.Title = o.Title
	}
	st.Feature, st.Paths, st.Change, st.Changelog = o.Feature, paths, o.Change, o.Changelog
	if strings.HasPrefix(o.ID, "DO-") {
		st.ResumeOutcome = &o
	}
	switch st.Status {
	case StatusAccepted:
		return st, fmt.Errorf("%s is already accepted on branch %s", o.ID, st.Branch)
	case StatusBlocked:
		return st, fmt.Errorf("%s is blocked; answer it first: aeh answer %s \"...\"\n%s", o.ID, o.ID, st.Question)
	}
	ru := &runner{repo: r, cfg: cfg, outcome: o, protected: protectedPaths(cfg, o), state: &st, log: log, memory: memory, generated: gen}
	if gen != nil {
		ru.seal = &gen.Seal
		if pin := gen.Plan.MemoryCommit; pin != "" {
			// The run reads memory as its plan pinned it: contract, prompt, drift gate and
			// the implementation record all see the planned version, whatever came after.
			pinned, err := loadMemoryAt(memory.Dir, pin)
			if err != nil {
				return st, fmt.Errorf("run %s: %w", o.ID, err)
			}
			spec, ok := pinned.Spec(gen.Plan.Feature)
			if !ok || spec.Version != gen.Plan.SpecVersion || (gen.Plan.Owners != "" && pinned.ContractFingerprint(gen.Plan.Feature) != gen.Plan.Owners) {
				return st, fmt.Errorf("run %s: pinned memory commit %s does not hold %s %s as planned", o.ID, short(pin), gen.Plan.Feature, gen.Plan.SpecVersion)
			}
			memory, ru.memory = pinned, pinned
		}
		spec, exists := memory.Spec(gen.Plan.Feature)
		if !exists || !spec.Approved() || spec.Approval.Content != gen.Plan.SpecDigest {
			st.RestartRequired = true
			err := ru.block("spec changed since plan; approve it and run aeh plan again")
			return st, err
		}
		if err := generatedPreflight(cfg, o, gen.Plan.Author); err != nil {
			return st, err
		}
		if st.Contract == "" {
			current, exists := branchCommit(r.Root, IntegrationBranch(o.ID))
			if !exists || current != gen.Plan.PlanCommit {
				return st, fmt.Errorf("cannot start %s: integration branch must point at plan commit %s", o.ID, gen.Plan.PlanCommit)
			}
		}
		if g := gateSealedManifest(r.Root, gen.Plan.PlanCommit, gen.Manifest, gen.Paths); !g.Passed {
			return st, errors.New(g.Output)
		}
	}
	if attrs := attributesFileInRepo(r.Root); attrs != "" {
		ru.protected = append(ru.protected, attrs)
	}
	if ru.implementPrompt, err = loadPrompt(r, "implement.md"); err != nil {
		return st, fmt.Errorf("load implementation prompt: %w", err)
	}
	ru.reviewPrompts, ru.reviewSources = map[string]string{}, map[string]string{}
	for _, role := range roleOrder {
		if ru.reviewPrompts[role], ru.reviewSources[role], err = loadReviewPrompt(r, role); err != nil {
			return st, fmt.Errorf("load review prompt: %w", err)
		}
	}
	if ru.triagePrompt, err = loadPrompt(r, "triage.md"); err != nil {
		return st, fmt.Errorf("load triage prompt: %w", err)
	}
	// The sandbox opens before triage, so a configured sandbox that cannot be used stops
	// the run before any agent is launched. A planned run uses only the isolation its
	// plan's checks ran under.
	if ru.sb, err = openFlowSandbox(r, cfg, "run-"+o.ID); err != nil {
		return st, err
	}
	if gen != nil {
		if err := checkPlanSandbox(gen.Plan.ID, gen.Plan.Sandbox, ru.sb); err != nil {
			return st, err
		}
	}
	// Pure triage can block a known new route without requiring any agent setup.
	// When a model is needed, validate its CLI and the reviewer policy first.
	if st.InitialRoute == "" && o.Route == "" && cfg.Triage.Profile != "" {
		if err := preflight(cfg, o, st.EffectiveRoute, st.Roles...); err != nil {
			return st, err
		}
	}
	if err := ru.triage(ctx); err != nil {
		return st, err
	}
	var sealErr error
	if gen == nil && o.Level >= 2 && len(o.Acceptance) > 0 {
		ru.seal, sealErr = verifySeal(r, o)
	}
	if question := ru.newRequirements(); question != "" {
		st.RestartRequired = true
		err := ru.block(question)
		return st, err
	}
	if sealErr != nil {
		return st, sealErr
	}
	if ru.sb.agents() && gen == nil && ru.seal != nil {
		head, err := headCommit(r.Root)
		if err != nil {
			return st, err
		}
		if g := sealTracked(r.Root, head, *ru.seal, o.Acceptance); !g.Passed {
			return st, errors.New(g.Output)
		}
	}
	if o.Feature != "" {
		if !memory.Present {
			return st, errors.New("declared feature requires product memory; run: aeh memory init")
		}
		if _, ok := memory.Spec(o.Feature); !ok {
			return st, fmt.Errorf("declared feature %q does not exist in product memory", o.Feature)
		}
	}
	if err := preflight(cfg, o, st.EffectiveRoute, st.Roles...); err != nil {
		return st, err
	}
	branch := IntegrationBranch(o.ID)
	contract, err := ru.contractHash()
	if err != nil {
		return st, err
	}
	if current, exists := branchCommit(r.Root, branch); gen != nil && st.Contract == "" && st.Branch == "" && st.Head == "" {
		if !exists || current != gen.Plan.PlanCommit {
			return st, fmt.Errorf("cannot start %s: integration branch must point at plan commit %s", o.ID, gen.Plan.PlanCommit)
		}
		st.Base, st.Head, st.Contract = gen.Plan.PlanCommit, gen.Plan.PlanCommit, contract
	} else if st.Contract != "" || exists || st.Branch != "" {
		if st.Contract != contract || st.Head == "" || current != st.Head {
			return st, fmt.Errorf("cannot resume %s: contract or integration branch differs from recorded state; inspect it or use a new outcome ID", o.ID)
		}
	} else {
		base, err := headCommit(r.Root)
		if err != nil {
			return st, err
		}
		if _, err := git(r.Root, "branch", branch, base); err != nil {
			return st, err
		}
		st.Base, st.Head, st.Contract = base, base, contract
	}
	if gen != nil {
		if g := gateSealedManifest(r.Root, st.Head, gen.Manifest, gen.Paths); !g.Passed {
			return st, errors.New(g.Output)
		}
	}
	st.Title, st.Branch, st.Status = o.Title, branch, StatusRunning
	for _, t := range o.Tasks {
		st.task(t.ID) // create every task entry first so pointers below stay valid
	}
	if err := saveState(r, &st); err != nil {
		return st, err
	}
	ru.logf("%s: %s (level %d, branch %s)", o.ID, o.Title, o.Level, branch)

	for _, t := range o.Tasks {
		ts := st.task(t.ID)
		if ts.Status == StatusDone {
			continue
		}
		ok, question, err := ru.runTask(ctx, t, ts)
		if err != nil {
			return st, err
		}
		if !ok {
			err := ru.block(question)
			return st, err
		}
	}
	ok, question, err := ru.completeReviews(ctx)
	if err != nil {
		return st, err
	}
	if !ok {
		err := ru.block(question)
		return st, err
	}
	ok, question, err = ru.finalCheck(ctx)
	if err != nil {
		return st, err
	}
	if !ok {
		err := ru.block(question)
		return st, err
	}
	var pending PendingRecord
	var pendingErr error
	if o.Feature != "" {
		current, err := LoadMemory(r)
		pin := ""
		if gen != nil {
			pin = gen.Plan.MemoryCommit
		}
		switch {
		case err != nil:
		case pin != "":
			// A pinned run ignores later memory changes; its pin must still be in history, and
			// no other implementation may have been recorded since its plan was reserved.
			if now, ok := current.Spec(o.Feature); ok && lastCode(now) != gen.Plan.PriorCode {
				st.RestartRequired = true
				err := ru.block(fmt.Sprintf("Another implementation of %s was recorded after plan %s was reserved; this plan is superseded. Plan the feature again: aeh plan %s", o.Feature, o.ID, o.Feature))
				return st, err
			}
			if _, perr := loadMemoryAt(current.Dir, pin); perr != nil {
				err = perr
			} else {
				current = memory
			}
		case gen != nil:
			spec, exists := current.Spec(o.Feature)
			if !exists || !spec.Approved() || spec.Approval.Content != gen.Plan.SpecDigest {
				st.RestartRequired = true
				err := ru.block("spec changed since plan; approve it and run aeh plan again")
				return st, err
			}
		}
		if err != nil || current.Fingerprint != memory.Fingerprint {
			reason := ErrMemoryChanged.Error()
			if err != nil {
				reason = err.Error()
			}
			err := ru.block(fmt.Sprintf("Cannot accept: %s. Inspect product memory and use a new outcome ID if its contract changed. What should change before retrying?", reason))
			return st, err
		}
		pending = PendingRecord{OutcomeID: o.ID, Feature: o.Feature, Change: o.Change, Note: o.Changelog, Commit: st.Head, MemoryBase: pin}
		if gen != nil {
			pending.Implementation, err = ru.implementation()
			if err != nil {
				return st, err
			}
		}
		pendingErr = SavePending(r, pending)
		// Persist a retry message with acceptance, even if the process stops before recording.
		st.MemoryError = "memory record is pending"
		if pendingErr != nil {
			st.MemoryError = fmt.Sprintf("save pending record: %v", pendingErr)
		}
	}
	st.Status = StatusAccepted
	if err := saveState(r, &st); err != nil {
		return st, err
	}
	if o.Feature != "" {
		ru.recordMemory(pending, pendingErr)
	}
	report := ru.writeReport()
	ru.logf("%s accepted. Branch %s is ready to review and merge. Report: %s", o.ID, branch, report)
	ru.notify("accepted", fmt.Sprintf("%s accepted on branch %s", o.ID, branch))
	if err := reconcileQueues(r); err != nil {
		ru.logf("warning: reconcile queues after acceptance: %v", err)
	}
	return st, nil
}

func (ru *runner) contractHash() (string, error) {
	var seal string
	if ru.seal != nil {
		seal = ru.seal.Hash
	}
	// Only what defines the work and its checks. Profiles, task start profiles, ladder,
	// reviewer, notify and timeouts can change, so a blocked outcome resumes after fixing them.
	commands := ru.cfg.Commands
	commands.TimeoutMinutes = 0
	outcome := ru.outcome
	outcome.Tasks = append([]Task(nil), ru.outcome.Tasks...)
	for i := range outcome.Tasks {
		outcome.Tasks[i].Start = ""
	}
	// Ownership of every feature plus the declared spec; not the versions of other features.
	type memoryContract struct {
		Fingerprint, Feature, Change, Changelog string
	}
	data, err := json.Marshal(struct {
		Commands        any
		PassEnv         []string
		Protected       []string
		Limits          any
		Outcome         Outcome
		Seal            string
		ImplementPrompt string
		ReviewPrompts   map[string]string
		TriagePrompt    string
		TriageOutput    string
		InitialRoute    string
		Rules           []TriageRule
		Memory          memoryContract
		Generated       any `json:",omitempty"`
		Sandbox         any `json:",omitempty"`
	}{commands, ru.cfg.PassEnv, ru.cfg.Protected, ru.cfg.Limits, outcome, seal, ru.implementPrompt, ru.reviewPrompts,
		ru.triagePrompt, ru.state.TriageOutput, ru.state.InitialRoute, ru.cfg.Triage.Rules,
		memoryContract{ru.memory.ContractFingerprint(outcome.Feature), outcome.Feature, outcome.Change, outcome.Changelog}, ru.generatedContract(),
		sandboxContract(ru.sb)})
	if err != nil {
		return "", fmt.Errorf("encode execution contract: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// runTask climbs the ladder for one task. It returns ok=false with a question when the task blocks.
func (ru *runner) runTask(ctx context.Context, t Task, ts *TaskState) (bool, string, error) {
	first := ru.cfg.Ladder[0]
	if t.Start != "" {
		if _, ok := ru.cfg.Profiles[t.Start]; !ok {
			return false, "", fmt.Errorf("task %s starts at unknown profile %q", t.ID, t.Start)
		}
		first = t.Start
	}
	maxLines := t.MaxDiffLines
	if maxLines <= 0 {
		maxLines = ru.cfg.Limits.MaxDiffLines
	}
	from := 0
	cursor := ru.state.Cursor
	if cursor != nil && cursor.Task == t.ID {
		first, from = cursor.First, cursor.LadderIndex
		if from < 0 || from >= maxAttempts || cursor.Answer == "" {
			return false, "", fmt.Errorf("task %s: invalid or unanswered spec cursor", t.ID)
		}
		for _, name := range []string{cursor.Author, cursor.Profile} {
			if err := ru.cfg.checkProfile(name); err != nil {
				return false, "", fmt.Errorf("resume spec cursor: %w", err)
			}
		}
	}
	ts.Status = StatusRunning
	var last attemptResult
	for i := from; i < maxAttempts; i++ {
		profile := first
		if i == maxAttempts-1 {
			profile = ru.cfg.escalate(first)
		}
		if cursor != nil && cursor.Task == t.ID {
			profile = cursor.Author
		}
		n := len(ts.Attempts) + 1
		res, err := ru.attempt(ctx, t, n, profile, maxLines, last)
		res.LadderIndex = i
		res.Seconds = time.Since(res.StartedAt).Seconds()
		if cursor != nil && cursor.Task == t.ID {
			res.ResumedFromCursor = true
		}
		if res.SpecQuestion != nil && err == nil {
			ru.pauseSpec(t, &res, i, first)
			ts.Status = StatusBlocked
		} else if err == nil {
			ru.state.Cursor = nil
		}
		ts.Attempts = append(ts.Attempts, res.Attempt)
		if res.Passed {
			ts.Status = StatusDone
			ru.state.Head = res.Commit
		}
		if saveErr := saveState(ru.repo, ru.state); saveErr != nil {
			return false, "", errors.Join(err, fmt.Errorf("record attempt %d: %w", n, saveErr))
		}
		if err != nil {
			return false, "", err
		}
		if res.SpecQuestion != nil {
			return false, res.SpecQuestion.Question, nil
		}
		cursor = nil
		if res.Passed {
			return true, "", nil
		}
		if res.Requirements != "" {
			ts.Status = StatusBlocked
			ru.state.RestartRequired = true
			return false, res.Requirements, nil
		}
		if res.Uncertain {
			ts.Status = StatusBlocked
			if ru.sb.agents() {
				// Every container of the attempt was removed; nothing is left running.
				return false, fmt.Sprintf("Task %s stopped at %s without a result:\n%s\nFix the cause (for example start Docker), then answer to retry. See the attempt artifacts.",
					t.ID, res.FailedGate, tail(strings.TrimSpace(res.Feedback), 1500)), nil
			}
			return false, fmt.Sprintf("Task %s stopped at %s. Confirm all processes stopped and review possible effects before answering to retry. See the attempt artifacts.", t.ID, res.FailedGate), nil
		}
		last = res
	}
	ts.Status = StatusBlocked
	q := fmt.Sprintf("Task %s failed %d attempts. Last failure at the %q check:\n%s\n\nWhat should change: the task, its paths, the criteria or the limits?",
		t.ID, maxAttempts, last.FailedGate, tail(strings.TrimSpace(last.Feedback), 1500))
	return false, q, nil
}

type attemptResult struct {
	Attempt
	Feedback     string
	Uncertain    bool
	Requirements string
}

// attempt runs one agent in a fresh worktree and passes the result through the gates.
func (ru *runner) attempt(ctx context.Context, t Task, n int, profile string, maxLines int, prev attemptResult) (res attemptResult, err error) {
	o, r := ru.outcome, ru.repo
	name := fmt.Sprintf("%s-a%d", t.ID, n)
	runDir := r.RunDir(o.ID, name)
	res = attemptResult{Attempt: Attempt{N: n, Profile: profile, RunDir: rel(r.Root, runDir), StartedAt: time.Now().UTC()}}

	base := ru.state.Head
	res.Base = base
	if current, _ := branchCommit(r.Root, IntegrationBranch(o.ID)); current != base {
		return res, fmt.Errorf("integration branch %s changed", IntegrationBranch(o.ID))
	}
	// Under sandbox policy 2 nothing of an attempt is checked out on the host: the agent
	// works on an export of the base and its result is captured from the container.
	sandboxed := ru.sb.agents()
	branch := AttemptBranch(o.ID, t.ID, n)
	wt := r.WorktreePath(o.ID + "-" + name)
	if !sandboxed {
		if err := addWorktree(r.Root, wt, branch, base); err != nil {
			return res, err
		}
		defer func() {
			if res.Worktree != wt {
				removeWorktree(r.Root, wt)
				deleteBranch(r.Root, branch)
			}
		}()
	}
	message := fmt.Sprintf("aeh: %s %s %s (attempt %d, %s)", o.ID, t.ID, firstLine(t.Goal), n, profile)

	prompt, err := renderPrompt("implement.md", ru.implementPrompt, promptData{
		Outcome: o, Task: t, Protected: ru.protected, MaxLines: maxLines,
		Build: ru.cfg.Commands.Build, Test: ru.cfg.Commands.Test, Answers: ru.state.Answers,
		Attempt: n, FailedGate: prev.FailedGate, Feedback: prev.Feedback,
		MemorySpec: memorySpecBlock(ru.memory, o.Feature),
	})
	if err != nil {
		return res, fmt.Errorf("render prompt: %w", err)
	}
	ru.logf("  %s attempt %d with profile %q", t.ID, n, profile)
	var gates []GateResult
	check := func(g GateResult) bool {
		gates = append(gates, g)
		ru.logf("    %-6s %s", g.Name, gateStatus(g))
		for _, warning := range g.Warnings {
			ru.logf("    %s warning: %s", g.Name, warning)
		}
		return g.Passed
	}
	record := func() error {
		return writePrivateJSON(filepath.Join(runDir, "gates.json"), gates)
	}
	var output, commit string
	var resume *Attempt
	if c := ru.state.Cursor; c != nil && c.Task == t.ID && c.Phase == "review" {
		for _, a := range ru.state.task(t.ID).Attempts {
			if a.N == c.Attempt && a.Commit == c.Candidate && a.Base == base {
				copy := a
				resume = &copy
			}
		}
		if resume == nil {
			return res, fmt.Errorf("task %s: cursor candidate does not match recorded attempt", t.ID)
		}
	} else {
		var outPath, captured string
		var runErr error
		if sandboxed {
			dir, mem, merr := ru.agentMemory()
			if merr != nil {
				return res, merr
			}
			step, serr := ru.sb.agentStep(ctx, r.Root, ru.cfg, agentStep{Kind: "implement", Profile: profile, Prompt: prompt,
				RunDir: runDir, LogName: "agent", Work: base, Capture: message, MemoryDir: dir, MemoryCommit: mem})
			if serr != nil {
				return res, serr
			}
			outPath, captured, runErr = step.OutPath, step.Candidate, step.AgentErr
		} else {
			if err := ru.cfg.hostAgents("implementation"); err != nil {
				return res, err
			}
			outPath, runErr = runAgent(ctx, ru.cfg.Profiles[profile], ru.cfg.PassEnv, wt, prompt, runDir, "agent")
		}
		if runErr != nil {
			err := runErr
			res.FailedGate, res.Feedback = "agent", err.Error()
			res.Uncertain = uncertainExit(err)
			if res.Uncertain && !sandboxed {
				res.Worktree = wt
			}
			check(GateResult{Name: res.FailedGate, Output: res.Feedback, Uncertain: res.Uncertain, Sandboxed: sandboxed})
			return res, record()
		}
		out, err := os.ReadFile(outPath)
		if err != nil {
			return res, fmt.Errorf("read implementer output: %w", err)
		}
		output = string(out)
		if sandboxed {
			if captured == "" {
				return res, errors.New("the sandbox returned no candidate for a clean agent exit")
			}
			// No host worktree holds the candidate: a private ref keeps it from Git's
			// garbage collection while the attempt can still resume.
			if _, err := git(r.Root, "update-ref", fmt.Sprintf("refs/aeh/candidates/%s/%s-a%d", o.ID, t.ID, n), captured); err != nil {
				return res, fmt.Errorf("retain candidate: %w", err)
			}
			commit = captured
		}
	}

	if resume != nil {
		commit = resume.Commit
		for _, v := range resume.Reviews {
			if v.SHA == commit && v.Verdict == "APPROVE" {
				res.Reviews = append(res.Reviews, v)
			}
		}
	} else if !sandboxed {
		commit, err = candidateCommit(wt, base, message)
	}
	if err != nil {
		res.FailedGate, res.Feedback = "scope", err.Error()
		check(GateResult{Name: res.FailedGate, Output: res.Feedback})
		return res, record()
	}
	if err := writePrivateJSON(filepath.Join(runDir, "candidate.json"), map[string]string{"base": base, "commit": commit, "contract": ru.state.Contract}); err != nil {
		return res, err
	}
	res.Commit = commit
	// --no-renames in candidateChanges includes both old and new rename paths.
	changes, err := candidateChanges(r.Root, base, commit)
	if err != nil {
		return res, err
	}
	ru.observeRules(rulesForFiles(ru.cfg.Triage.Rules, changedPaths(changes)))
	res.RolesNotRun = requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)
	if err := saveState(r, ru.state); err != nil {
		return res, err
	}
	diff, err := candidateDiff(r.Root, base, commit)
	if err != nil {
		return res, err
	}
	files, err := candidateFileStatus(r.Root, base, commit)
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(runDir, "diff.patch"), []byte(diff), 0o600); err != nil {
		return res, err
	}
	if question := ru.newRequirements(); question != "" {
		res.FailedGate, res.Feedback, res.Requirements = "requirements", question, question
		if err := writePrivateJSON(filepath.Join(runDir, "requirements.json"), map[string]any{"route": ru.state.EffectiveRoute, "roles": ru.state.Roles, "rules": ru.state.RulesHit, "missing": question}); err != nil {
			return res, err
		}
		return res, record()
	}
	question, invalid := agentTerminal(output)
	// A clean question with no candidate changes needs no verification run. It
	// approves nothing. When changes exist, failed gates take precedence below.
	if len(changes) == 0 && question != "" {
		if !sandboxed {
			if err := verifyCheckout(wt, base); err != nil {
				return res, fmt.Errorf("question checkout: %w", err)
			}
		}
		res.SpecQuestion = &SpecCursor{Phase: "implementation", Role: "implementer", Profile: profile, Provider: ru.cfg.Profiles[profile].Provider, Question: question}
		return res, record()
	}
	// Hash and scope read Git objects in the main repository, so nothing from the
	// candidate is checked out, and no checkout filter runs, until both have passed.
	passed := true
	if ru.seal != nil {
		passed = check(ru.sealedGate(base, commit))
	}
	if passed {
		passed = check(gateScope(changes, ru.protected, t.Paths, maxLines))
		if passed {
			passed = check(gateDrift(changedPaths(changes), ru.memory, o.Feature))
		}
	}
	verified := wt + "-verify"
	if sandboxed {
		verified = "" // gates and reviewers read exports of the candidate
	}
	if passed && !sandboxed {
		if err := addWorktreeDetached(r.Root, verified, commit); err != nil {
			return res, err
		}
		defer func() {
			if res.Worktree != verified {
				removeWorktree(r.Root, verified)
			}
		}()
		if err := verifyCheckout(verified, commit); err != nil {
			return res, err
		}
	}
	var v *verification
	if passed {
		var verr error
		if v, verr = newVerification(ctx, ru.sb, r.Root, verified, commit, ru.cfg); verr != nil {
			passed = check(sandboxFailure(verr))
		} else {
			defer func() {
				if cerr := v.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
		}
	}
	if passed {
		passed = check(v.checked(ctx, "build", ru.cfg.Commands.Build))
	}
	if passed {
		passed = check(v.checked(ctx, "test", ru.cfg.Commands.Test))
	}
	if passed && ru.generated != nil {
		passed = check(ru.acceptanceGate(ctx, v))
	}
	if passed && (question != "" || invalid) {
		if question != "" {
			res.SpecQuestion = &SpecCursor{Phase: "implementation", Role: "implementer", Profile: profile, Provider: ru.cfg.Profiles[profile].Provider, Question: question}
			return res, record()
		}
		passed = check(GateResult{Name: "agent", Output: "invalid SPEC-QUESTION terminal result"})
	}
	if passed {
		roles := requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)
		res.RolesNotRun = nil
		for i, role := range roles {
			if approvedOn(res.Reviews, role, commit) {
				continue
			}
			g, verdict, err := ru.review(ctx, t, role, profile, commit, verified, diff, files, runDir)
			if err != nil {
				return res, err
			}
			res.Reviews = append(res.Reviews, verdict)
			if verdict.Question != "" {
				res.SpecQuestion = &SpecCursor{Phase: "review", Role: role, Profile: verdict.Profile, Provider: verdict.Provider, Question: verdict.Question}
				res.RolesNotRun = append(res.RolesNotRun, roles[i+1:]...)
				// A state file names an object but does not make it reachable to
				// Git GC. Keep the paused candidate as durable evidence, including
				// after abandonment, without advancing the integration branch.
				ref := fmt.Sprintf("refs/aeh/questions/%s/%s-a%d", o.ID, t.ID, n)
				if _, err := git(r.Root, "update-ref", ref, commit); err != nil {
					return res, fmt.Errorf("retain question candidate: %w", err)
				}
				return res, record()
			}
			passed = check(g)
			if !passed {
				res.RolesNotRun = append(res.RolesNotRun, roles[i+1:]...)
				break
			}
		}
	} else {
		res.RolesNotRun = requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)
	}
	if !passed {
		failed := gates[len(gates)-1]
		res.FailedGate, res.Feedback = failed.Name, failed.Output
		res.Uncertain = failed.Uncertain
		if res.Uncertain {
			res.Worktree = verified
		}
	}
	if err := record(); err != nil {
		return res, err
	}
	if !passed {
		return res, nil
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err := moveBranch(r.Root, IntegrationBranch(o.ID), base, commit); err != nil {
		return res, fmt.Errorf("advance %s (is it checked out somewhere?): %w", IntegrationBranch(o.ID), err)
	}
	res.Passed, res.Commit = true, commit
	return res, nil
}

func checkoutGate(g GateResult, wt, commit string) GateResult {
	if g.Passed {
		if err := verifyCheckout(wt, commit); err != nil {
			g.Passed, g.Output = false, err.Error()
		}
	}
	return g
}

// review binds a role's independent verdict to a candidate and verifies its checkout.
func (ru *runner) review(ctx context.Context, t Task, role, author, commit, wt, diff, files, runDir string) (GateResult, ReviewVerdict, error) {
	g := GateResult{Name: "review"}
	v := ReviewVerdict{Role: role, SHA: commit, Verdict: "INVALID", PromptSource: ru.reviewSources[role]}
	profile, err := ru.cfg.reviewerFor(role, author)
	if err != nil {
		return g, v, err
	}
	v.Profile, v.Provider = profile, ru.cfg.Profiles[profile].Provider
	if c := ru.state.Cursor; c != nil && c.Task == t.ID && c.Phase == "review" && c.Role == role {
		profile = c.Profile
		if err := ru.cfg.independent(author, profile); err != nil {
			return g, v, fmt.Errorf("resume reviewer: %w", err)
		}
		v.Profile, v.Provider = profile, ru.cfg.Profiles[profile].Provider
	}
	if err := ru.cfg.checkProfile(profile); err != nil {
		return g, v, fmt.Errorf("review profile: %w", err)
	}
	start := time.Now()
	prompt, err := renderPrompt("review-"+role+".md", ru.reviewPrompts[role], promptData{Outcome: ru.outcome, Task: t, Diff: diff, Files: files, Answers: ru.state.Answers})
	if err != nil {
		return g, v, err
	}
	ru.logf("    review %s with %q (%s) on %s", role, profile, v.Provider, commit)
	var outPath string
	var checkoutErr error
	if ru.sb.agents() {
		// The reviewer reads a read-only export of the candidate: it cannot change what
		// it reviews, so there is no checkout to verify afterwards.
		g.Sandboxed = true
		dir, mem, merr := ru.agentMemory()
		if merr != nil {
			return g, v, merr
		}
		step, serr := ru.sb.agentStep(ctx, ru.repo.Root, ru.cfg, agentStep{Kind: "review", Profile: profile, Prompt: prompt,
			RunDir: runDir, LogName: "review-" + role, Work: commit, MemoryDir: dir, MemoryCommit: mem})
		if serr != nil {
			return g, v, serr
		}
		outPath, err = step.OutPath, step.AgentErr
	} else {
		if err := ru.cfg.hostAgents("review"); err != nil {
			return g, v, err
		}
		outPath, err = runAgent(ctx, ru.cfg.Profiles[profile], ru.cfg.PassEnv, wt, prompt, runDir, "review-"+role)
		// Check even a rejected or failed reviewer: all reviewers are read-only.
		checkoutErr = verifyCheckout(wt, commit)
	}
	g.Seconds = time.Since(start).Seconds()
	if err != nil {
		g.Output, g.Uncertain = err.Error(), uncertainExit(err)
		if checkoutErr != nil {
			g.Output += "\n" + checkoutErr.Error()
		}
		return g, v, nil
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return g, v, fmt.Errorf("read reviewer output: %w", err)
	}
	if checkoutErr != nil {
		g.Output = checkoutErr.Error()
		return g, v, nil
	}
	question, invalid := agentTerminal(string(out))
	if invalid {
		g.Output = "invalid SPEC-QUESTION terminal result"
		return g, v, nil
	}
	if question != "" {
		v.Verdict, v.Question, g.Output = "SPEC-QUESTION", question, question
		return g, v, nil
	}
	switch parseVerdict(string(out)) {
	case "APPROVE":
		g.Passed = true
		v.Verdict = "APPROVE"
	case "REJECT":
		v.Verdict = "REJECT"
		g.Output = "reviewer rejected the change:\n" + tail(string(out), maxGateOutput)
	default:
		g.Output = "reviewer gave no verdict"
	}
	return g, v, nil
}

// parseVerdict accepts one exact verdict on the final nonempty line of plain stdout.
func parseVerdict(out string) string {
	if question, invalid := agentTerminal(out); question != "" || invalid {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case "VERDICT: APPROVE", "VERDICT: REJECT":
			if i != len(lines)-1 {
				return ""
			}
			return strings.TrimPrefix(strings.TrimSpace(line), "VERDICT: ")
		}
	}
	return ""
}

func acceptedAttempt(task *TaskState) *Attempt {
	for i := len(task.Attempts) - 1; i >= 0; i-- {
		if task.Attempts[i].Passed {
			return &task.Attempts[i]
		}
	}
	return nil
}

func missingReviewRoles(a *Attempt, roles []string) []string {
	var missing []string
	for _, role := range roles {
		approved := false
		if a != nil {
			for _, v := range a.Reviews {
				if v.Role == role && v.SHA == a.Commit {
					approved = v.Verdict == "APPROVE"
				}
			}
		}
		if !approved {
			missing = append(missing, role)
		}
	}
	return missing
}

// completeReviews applies the final policy to earlier accepted candidates, without
// rerunning their agents, build/test gates, or already approved roles.
func (ru *runner) completeReviews(ctx context.Context) (bool, string, error) {
	roles := requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)
	for _, t := range ru.outcome.Tasks {
		ts := ru.state.task(t.ID)
		if ts.Status != StatusDone {
			return false, fmt.Sprintf("Task %s has no accepted candidate", t.ID), nil
		}
		a := acceptedAttempt(ts)
		if a == nil {
			return false, fmt.Sprintf("Task %s has no accepted candidate", t.ID), nil
		}
		missing := missingReviewRoles(a, roles)
		if len(missing) == 0 {
			continue
		}
		if ok, question, err := ru.completeTaskReviews(ctx, t, a, missing); err != nil || !ok {
			return ok, question, err
		}
	}
	return true, "", nil
}

func (ru *runner) completeTaskReviews(ctx context.Context, t Task, a *Attempt, missing []string) (bool, string, error) {
	r := ru.repo
	runDir := filepath.Join(r.Root, filepath.FromSlash(a.RunDir))
	// Existing stage-2 attempts recorded the base only in candidate.json.
	if a.Base == "" {
		data, err := os.ReadFile(filepath.Join(runDir, "candidate.json"))
		if err != nil {
			return false, "", err
		}
		var candidate struct{ Base, Commit string }
		if err := json.Unmarshal(data, &candidate); err != nil {
			return false, "", err
		}
		if candidate.Base == "" || candidate.Commit != a.Commit {
			return false, "", fmt.Errorf("task %s candidate evidence does not match its accepted attempt", t.ID)
		}
		a.Base = candidate.Base
	}
	diff, err := candidateDiff(r.Root, a.Base, a.Commit)
	if err != nil {
		return false, "", err
	}
	files, err := candidateFileStatus(r.Root, a.Base, a.Commit)
	if err != nil {
		return false, "", err
	}
	// Sandboxed reviewers read an export of the accepted commit; nothing is checked out.
	sandboxed := ru.sb.agents()
	wt, keep := "", false
	if !sandboxed {
		wtRoot := filepath.Dir(r.WorktreePath(ru.outcome.ID + "-reviews"))
		if err := os.MkdirAll(wtRoot, 0o700); err != nil {
			return false, "", err
		}
		if wt, err = os.MkdirTemp(wtRoot, ru.outcome.ID+"-"+t.ID+"-reviews-"); err != nil {
			return false, "", err
		}
		if err := addWorktreeDetached(r.Root, wt, a.Commit); err != nil {
			_ = os.Remove(wt)
			return false, "", err
		}
		defer func() {
			if !keep {
				removeWorktree(r.Root, wt)
			}
		}()
		if err := verifyCheckout(wt, a.Commit); err != nil {
			return false, "", err
		}
	}
	gates := readGateResults(r.Root, a.RunDir)
	for i, role := range missing {
		g, verdict, err := ru.review(ctx, t, role, a.Profile, a.Commit, wt, diff, files, runDir)
		if err != nil {
			g.Output = err.Error()
		}
		a.Reviews = append(a.Reviews, verdict)
		a.RolesNotRun = append([]string(nil), missing[i+1:]...)
		gates = append(gates, g)
		ru.logf("    added %s review for %s: %s", role, t.ID, gateStatus(g))
		question := ""
		if !g.Passed {
			keep = g.Uncertain && !sandboxed
			if keep {
				a.Worktree = wt
			}
			question = fmt.Sprintf("Task %s required %s review failed on its accepted SHA %s:\n%s\n", t.ID, role, a.Commit, tail(strings.TrimSpace(g.Output), 1500))
			switch {
			case verdict.Question != "":
				question = verdict.Question
				c := &SpecCursor{Task: t.ID, Phase: "review", Role: role, Profile: verdict.Profile, Provider: verdict.Provider,
					Author: a.Profile, Candidate: a.Commit, Attempt: a.N, LadderIndex: a.LadderIndex,
					RetriesUsed: a.LadderIndex, First: ru.cfg.Ladder[0], AddedReview: true, Question: question}
				if c.LadderIndex == maxAttempts-1 {
					c.RetriesUsed = 0
				}
				if t.Start != "" {
					c.First = t.Start
				}
				a.SpecQuestion, ru.state.Cursor = c, c
			case g.Uncertain:
				question += "Confirm all processes stopped and review possible effects before answering to retry. See the attempt artifacts."
			case verdict.Verdict == "REJECT":
				// Rejection requires a new candidate, but there is no transactional
				// branch/state rewind and replay mechanism for integrated tasks.
				ru.state.RestartRequired = true
				question += "The integration branch already contains accepted tasks and cannot be safely rewound and replayed by this runner. Fix the feedback and create an outcome with another ID; aeh answer cannot resume this run."
			default:
				question += "Resolve the review failure; aeh answer followed by aeh run retries the missing review."
			}
			ru.state.Status, ru.state.Question = StatusBlocked, question
		} else if c := ru.state.Cursor; c != nil && c.Task == t.ID && c.AddedReview {
			a.ResumedFromCursor = true
			ru.state.Cursor = nil
		}
		recordErr := writePrivateJSON(filepath.Join(runDir, "gates.json"), gates)
		if err := errors.Join(recordErr, saveState(r, ru.state)); err != nil {
			return false, "", err
		}
		if !g.Passed {
			return false, question, nil
		}
	}
	return true, "", nil
}

// finalCheck runs the gates once more on the integrated branch: accepted means proven there.
func (ru *runner) finalCheck(ctx context.Context) (_ bool, _ string, err error) {
	o, r := ru.outcome, ru.repo
	for i := range ru.state.Tasks {
		task := &ru.state.Tasks[i]
		if task.Status != StatusDone {
			return false, fmt.Sprintf("Task %s has no accepted candidate", task.ID), nil
		}
		for _, role := range missingReviewRoles(acceptedAttempt(task), requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)) {
			return false, fmt.Sprintf("Task %s is missing required %s approval on its accepted SHA", task.ID, role), nil
		}
	}
	head, ok := branchCommit(r.Root, IntegrationBranch(o.ID))
	if !ok || head != ru.state.Head {
		return false, "", fmt.Errorf("integration branch %s changed", IntegrationBranch(o.ID))
	}
	runRoot := filepath.Dir(r.RunDir(o.ID, "final"))
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		return false, "", err
	}
	runDir, err := os.MkdirTemp(runRoot, "final-")
	if err != nil {
		return false, "", err
	}
	// Under sandbox policy 2 the integrated branch holds agent-written content, so it is
	// never checked out on the host: the gates run on an export and the sealed tests are
	// compared as Git objects.
	sandboxed := ru.sb.agents()
	wt, keep := "", false
	if !sandboxed {
		wtRoot := filepath.Dir(r.WorktreePath(o.ID + "-final"))
		if err := os.MkdirAll(wtRoot, 0o700); err != nil {
			return false, "", err
		}
		if wt, err = os.MkdirTemp(wtRoot, o.ID+"-final-"); err != nil {
			return false, "", err
		}
		if err := addWorktreeDetached(r.Root, wt, head); err != nil {
			_ = os.Remove(wt)
			return false, "", err
		}
		defer func() {
			if !keep {
				removeWorktree(r.Root, wt)
			}
		}()
		if err := verifyCheckout(wt, head); err != nil {
			return false, "", err
		}
	}
	ru.logf("  final check on %s", IntegrationBranch(o.ID))
	gates := []GateResult{}
	if ru.seal != nil {
		if sandboxed {
			gates = append(gates, ru.sealedGate(ru.state.Base, head))
			if ru.generated == nil && gates[len(gates)-1].Passed {
				gates = append(gates, sealTracked(r.Root, head, *ru.seal, o.Acceptance))
			}
		} else if ru.generated != nil {
			gates = append(gates, ru.sealedGate(ru.state.Base, head))
			if gates[len(gates)-1].Passed {
				gates = append(gates, gateSealedCheckout(r.Root, wt, head, ru.generated.Manifest.Retired, ru.generated.Paths))
			}
		} else {
			gates = append(gates, gateHash(wt, *ru.seal, o.Acceptance))
		}
	}
	if len(gates) == 0 || gates[len(gates)-1].Passed {
		changes, err := candidateChanges(r.Root, ru.state.Base, ru.state.Head)
		if err != nil {
			return false, "", fmt.Errorf("list final changes: %w", err)
		}
		gates = append(gates, gateDrift(changedPaths(changes), ru.memory, o.Feature))
	}
	var v *verification
	if gates[len(gates)-1].Passed {
		var verr error
		if v, verr = newVerification(ctx, ru.sb, r.Root, wt, head, ru.cfg); verr != nil {
			gates = append(gates, sandboxFailure(verr))
		} else {
			defer func() {
				if cerr := v.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
		}
	}
	if gates[len(gates)-1].Passed {
		gates = append(gates, v.checked(ctx, "build", ru.cfg.Commands.Build))
	}
	if gates[len(gates)-1].Passed {
		gates = append(gates, v.checked(ctx, "test", ru.cfg.Commands.Test))
	}
	if gates[len(gates)-1].Passed && ru.generated != nil {
		gates = append(gates, ru.acceptanceGate(ctx, v))
	}
	keep = gates[len(gates)-1].Uncertain && !sandboxed
	evidence := map[string]string{"commit": head, "contract": ru.state.Contract}
	if keep {
		evidence["retained_worktree"] = wt
	}
	if err := writePrivateJSON(filepath.Join(runDir, "candidate.json"), evidence); err != nil {
		return false, "", err
	}
	if err := writePrivateJSON(filepath.Join(runDir, "gates.json"), gates); err != nil {
		return false, "", err
	}
	ru.state.FinalRun = rel(r.Root, runDir)
	for _, g := range gates {
		for _, warning := range g.Warnings {
			ru.logf("    %s warning: %s", g.Name, warning)
		}
		ru.logf("    %-6s %s", g.Name, gateStatus(g))
		if !g.Passed {
			if g.Uncertain {
				return false, fmt.Sprintf("Final %s stopped. Confirm all processes stopped and review possible effects before answering. See the final check artifacts.", g.Name), nil
			}
			return false, fmt.Sprintf("The integrated branch fails the %q check:\n%s", g.Name, tail(strings.TrimSpace(g.Output), 1500)), nil
		}
	}
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if current, _ := branchCommit(r.Root, IntegrationBranch(o.ID)); current != head {
		return false, "", fmt.Errorf("integration branch changed during the final check")
	}
	return true, "", nil
}

func (ru *runner) block(question string) error {
	ru.state.Status = StatusBlocked
	ru.state.Question = question
	if err := saveState(ru.repo, ru.state); err != nil {
		return err
	}
	ru.writeReport()
	hint := fmt.Sprintf("Reply with: aeh answer %s \"...\" and run it again", ru.outcome.ID)
	if strings.HasPrefix(ru.outcome.ID, "DO-") && ru.state.Cursor == nil {
		hint = "Refine the request and run `aeh do` again, or turn it into an outcome."
	}
	if ru.state.RestartRequired {
		hint = "Prepare the requirements and create an outcome with another ID."
	}
	ru.logf("%s blocked:\n%s\n%s", ru.outcome.ID, question, hint)
	ru.notify("blocked", "Outcome blocked. See the local report for details.")
	return nil
}

func (ru *runner) notify(event, msg string) {
	err := notify(ru.cfg.Notify.Webhook, Event{Event: event, Outcome: ru.outcome.ID, Title: ru.outcome.Title, Message: msg})
	if err != nil {
		ru.logf("warning: notification failed: %v", err)
	}
}

func (ru *runner) logf(format string, args ...any) {
	if ru.log != nil {
		fmt.Fprintf(ru.log, format+"\n", args...)
	}
}

// preflight fails fast when a profile's CLI is not installed, before anything is created.
func preflight(cfg Config, o Outcome, route string, added ...string) error {
	needsTriage := route == "" && o.Route == "" && cfg.Triage.Profile != ""
	if route == "" {
		route = o.Route
		if route == "" {
			route = RouteChange
		}
	}
	roles := requiredRoles(route, o.Reviewers, added)
	// A rejected candidate can touch paths outside the declared scope. Account for
	// every rule, even ones not predicted by task paths, before starting an agent.
	for _, rule := range cfg.Triage.Rules {
		route = maxRoute(route, rule.Route)
		roles = requiredRoles(route, roles, rule.Reviewers)
	}
	var authors []string
	for _, task := range o.Tasks {
		authors = append(authors, cfg.reachableAuthors(task)...)
	}
	if err := cfg.checkIndependence(authors, roles); err != nil {
		return fmt.Errorf("reviewer profile configuration: %w", err)
	}
	names := append([]string{}, authors...)
	for _, author := range authors {
		for _, role := range roles {
			n, err := cfg.reviewerFor(role, author)
			if err != nil {
				return err
			}
			names = append(names, n)
		}
	}
	if needsTriage {
		names = append(names, cfg.Triage.Profile)
	}
	for _, n := range names {
		if err := cfg.checkProfile(n); err != nil {
			return err
		}
		p := cfg.Profiles[n]
		if cfg.agentsSandboxed() {
			continue // the program lives in the agent image; aeh sandbox check looks for it
		}
		if _, err := exec.LookPath(p.Command[0]); err != nil {
			return fmt.Errorf("profile %q: %s is not installed or not on PATH", n, p.Command[0])
		}
	}
	return nil
}

const changeDefaultWarning = "warning: no triage profile configured; defaulting to change"

// triageRetry reports a sandboxed triage launch whose model gave no usable answer for a
// reason outside the model: the runtime failed, the container state was lost, or the
// launch was stopped. Such a triage runs again next time instead of falling back to new.
func triageRetry(res LaunchResult) bool {
	return res.Class == ClassInfrastructure || res.Class == ClassTerminated
}

// observeRules persists the maximum policy seen across all candidates and tasks.
func (ru *runner) observeRules(hit []TriageRule) {
	s := ru.state
	s.EffectiveRoute, s.Roles = applyRules(s.EffectiveRoute, s.Roles, hit)
	s.Roles = requiredRoles(s.EffectiveRoute, s.Roles, ru.outcome.Reviewers)
	for _, name := range ruleNames(hit) {
		if !containsString(s.RulesHit, name) {
			s.RulesHit = append(s.RulesHit, name)
		}
	}
}

// observeTaskRules applies the rules the declared task paths predict and records the ones
// left to the candidate's files.
func (ru *runner) observeTaskRules() {
	rules, tasks := ru.cfg.Triage.Rules, ru.outcome.Tasks
	ru.observeRules(rulesForTasks(rules, tasks))
	for _, name := range ruleNames(rulesDeferred(rules, tasks)) {
		if !containsString(ru.state.RulesDeferred, name) {
			ru.state.RulesDeferred = append(ru.state.RulesDeferred, name)
		}
	}
}

// triage writes a conservative result before launching the model. A crash anywhere
// thereafter resumes with that result (new), never a second model invocation.
func (ru *runner) triage(ctx context.Context) error {
	s, o := ru.state, ru.outcome
	if s.InitialRoute != "" {
		if s.TriageSource == "fallback-none" {
			ru.logf("%s", changeDefaultWarning)
		}
		return nil
	}
	// A sandboxed triage that could not run leaves the state as it was, so the next
	// invocation triages again: its view is read-only, so a retry has no effect to undo.
	before, err := json.Marshal(s)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		var prev State
		if err := json.Unmarshal(before, &prev); err != nil {
			return err
		}
		*s = prev
		if err := saveState(ru.repo, s); err != nil {
			return errors.Join(cause, err)
		}
		return fmt.Errorf("triage could not run in the sandbox; fix the cause (for example start Docker) and run the command again: %w", cause)
	}
	s.InitialRoute, s.TriageSource = o.Route, "declared"
	if o.Route == "" {
		if ru.cfg.Triage.Profile == "" {
			s.InitialRoute, s.TriageSource = RouteChange, "fallback-none"
			ru.logf("%s", changeDefaultWarning)
		} else {
			s.InitialRoute, s.TriageSource = RouteNew, "fallback-failed"
		}
	}
	s.EffectiveRoute = maxRoute(s.EffectiveRoute, s.InitialRoute)
	ru.observeTaskRules()
	if err := saveState(ru.repo, s); err != nil {
		return err
	}
	if o.Route != "" || ru.cfg.Triage.Profile == "" {
		return nil
	}
	prompt, err := renderPrompt("triage.md", ru.triagePrompt, promptData{Outcome: o, Features: featureIndexBlock(ru.memory)})
	if err != nil {
		return err
	}
	base, err := headCommit(ru.repo.Root)
	if err != nil {
		return err
	}
	runDir := ru.repo.RunDir(o.ID, "triage")
	var outPath string
	var runErr, checkoutErr error
	keep, sandboxed := false, ru.sb.agents()
	wt := ru.repo.WorktreePath(o.ID + "-triage")
	if sandboxed {
		dir, mem, err := ru.agentMemory()
		if err != nil {
			return rollback(err)
		}
		step, err := ru.sb.agentStep(ctx, ru.repo.Root, ru.cfg, agentStep{Kind: "triage", Profile: ru.cfg.Triage.Profile, Prompt: prompt,
			RunDir: runDir, LogName: "triage", Work: base, MemoryDir: dir, MemoryCommit: mem})
		if err != nil {
			return rollback(err)
		}
		if triageRetry(step.Launch) {
			return rollback(step.AgentErr)
		}
		outPath, runErr = step.OutPath, step.AgentErr
	} else {
		if err := addWorktreeDetached(ru.repo.Root, wt, base); err != nil {
			return err
		}
		defer func() {
			if !keep {
				removeWorktree(ru.repo.Root, wt)
			}
		}()
		if err := verifyCheckout(wt, base); err != nil {
			return err
		}
		if err := ru.cfg.hostAgents("triage"); err != nil {
			return err
		}
		outPath, runErr = runAgent(ctx, ru.cfg.Profiles[ru.cfg.Triage.Profile], ru.cfg.PassEnv, wt, prompt, runDir, "triage")
	}
	out, readErr := os.ReadFile(outPath)
	s.TriageOutput = string(out)
	if !sandboxed {
		checkoutErr = verifyCheckout(wt, base)
	}
	parsed := parseRoute(string(out))
	if runErr == nil && readErr == nil && checkoutErr == nil && parsed != "" {
		s.InitialRoute, s.TriageSource = parsed, "model"
		// The provisional new route was a crash fallback, not an observation.
		s.EffectiveRoute, s.Roles = parsed, nil
		ru.observeTaskRules()
	}
	if uncertainExit(runErr) && !sandboxed {
		keep = true
		s.TriageWorktree = wt
	}
	if err := writePrivateJSON(filepath.Join(runDir, "result.json"), map[string]any{
		"parsed_route": parsed, "initial_route": s.InitialRoute, "source": s.TriageSource,
		"error": fmt.Sprint(errors.Join(runErr, readErr, checkoutErr)),
	}); err != nil {
		return err
	}
	if err := saveState(ru.repo, s); err != nil {
		return err
	}
	if s.TriageSource == "fallback-failed" {
		ru.logf("warning: triage failed or gave no valid ROUTE; using new")
	}
	return ctx.Err()
}

func (ru *runner) newRequirements() string {
	if ru.state.EffectiveRoute != RouteNew {
		return ""
	}
	var missing []string
	spec, exists := ru.memory.Spec(ru.outcome.Feature)
	if ru.outcome.Feature == "" || !exists {
		missing = append(missing, "a declared feature with a spec in product memory")
	} else if !spec.Approved() {
		missing = append(missing, "owner approval of the current spec (aeh memory approve "+ru.outcome.Feature+")")
	}
	if ru.outcome.Level < 2 {
		missing = append(missing, "level 2")
	}
	if ru.seal == nil {
		missing = append(missing, "approved, unchanged sealed acceptance tests (aeh tests approve <ID>)")
	}
	if len(missing) == 0 {
		return ""
	}
	question := "Route new is blocked. Missing: " + strings.Join(missing, "; ") + ". The owner must prepare these requirements and create an outcome with another ID. aeh answer cannot resume this run because the contract changes."
	if strings.HasPrefix(ru.outcome.ID, "DO-") {
		question += " Turn this change into aeh/outcomes/<another-ID>.yaml with feature, change, changelog, level: 2, criteria, acceptance paths and tasks; approve the spec and seal the tests, then run aeh run <another-ID>."
	}
	return question
}

// verifySeal checks that the committed acceptance tests are exactly the ones approved.
func verifySeal(r Repo, o Outcome) (*Seal, error) {
	s, err := loadSeal(r, o.ID)
	if err != nil {
		return nil, err
	}
	dirty, err := dirtyPaths(r.Root, o.Acceptance)
	if err != nil {
		return nil, err
	}
	if dirty != "" {
		return nil, fmt.Errorf("commit the acceptance tests before running; uncommitted:\n%s", dirty)
	}
	hash, _, err := hashPaths(r.Root, o.Acceptance)
	if err != nil {
		return nil, err
	}
	if hash != s.Hash {
		return nil, fmt.Errorf("acceptance tests changed since they were approved; review them and run `aeh tests approve %s`", o.ID)
	}
	return &s, nil
}

// protectedPaths are the config's protected paths plus the harness files and the sealed tests.
func protectedPaths(cfg Config, o Outcome) []string {
	out := []string{"aeh/", ".aeh/"}
	out = append(out, cfg.Protected...)
	return append(out, o.Acceptance...)
}

// gateStatus is the console word for a gate: ok, skipped, or FAIL with the first output line.
// It matches the report, which also tells a skipped gate apart from one that passed.
func gateStatus(g GateResult) string {
	switch {
	case !g.Passed:
		return "FAIL: " + firstLine(g.Output)
	case g.Skipped:
		return "skipped"
	case g.Sandboxed:
		return "ok (sandbox)"
	}
	return "ok"
}

func changedPaths(changes []fileChange) []string {
	paths := make([]string, len(changes))
	for i, change := range changes {
		paths[i] = change.Path
	}
	return paths
}

// recordMemory runs only after acceptance has been saved. Recording errors never undo it.
func (ru *runner) recordMemory(p PendingRecord, pendingErr error) {
	err := pendingErr
	if err != nil {
		err = fmt.Errorf("save pending record: %w", err)
	} else {
		expected := ru.memory.Fingerprint
		if p.MemoryBase != "" {
			expected = "" // pinned: the record checks the implementation against the pin instead
		}
		ru.state.MemoryVersion, ru.state.MemoryCommit, err = RecordChange(ru.repo, p, expected)
	}
	ru.state.MemoryError = ""
	if err != nil {
		ru.state.MemoryError = err.Error()
	}
	ru.logf("%s", ru.memoryRecordSummary())
	if err := saveState(ru.repo, ru.state); err != nil {
		ru.logf("warning: save memory record result: %v", err)
		return // keep the pending record until the acceptance result is durable
	}
	if err == nil {
		if clearErr := ClearPending(ru.repo, p.OutcomeID); clearErr != nil {
			ru.state.MemoryError = fmt.Sprintf("clear pending record: %v", clearErr)
			_ = saveState(ru.repo, ru.state)
		}
	}
}

func (ru *runner) memoryRecordSummary() string {
	if ru.outcome.Feature == "" {
		return ""
	}
	if ru.state.MemoryError != "" {
		return fmt.Sprintf("memory not recorded: %s; retry with: aeh memory record %s", ru.state.MemoryError, ru.outcome.ID)
	}
	if ru.state.MemoryVersion != "" {
		return fmt.Sprintf("memory recorded: %s %s (memory commit %s)", ru.outcome.Feature, ru.state.MemoryVersion, short(ru.state.MemoryCommit))
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100] + "..."
	}
	return s
}

// loadGeneratedRun never discovers provenance in a checkout file. The state chooses the
// commit first; local copies are only checked for conflicts with those pinned bytes.
func loadGeneratedRun(r Repo, cfg Config, st PlanState) (Outcome, *generatedRun, error) {
	var o Outcome
	if st.Phase != PlanLogged {
		return o, nil, fmt.Errorf("plan %s is not finished; run aeh plan %s", st.ID, st.Feature)
	}
	if !isFullSHA(st.PlanCommit) || st.Author == "" || !validRoute(st.Route) {
		return o, nil, fmt.Errorf("plan %s has invalid provenance", st.ID)
	}
	if strings.TrimSpace(cfg.Commands.Acceptance) == "" {
		return o, nil, fmt.Errorf("generated outcome %s requires commands.acceptance with {{manifest}}", st.ID)
	}
	read := func(file string) ([]byte, error) {
		contents, err := sealedGitContents(r.Root, st.PlanCommit, []string{file})
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", st.ID, err)
		}
		b, ok := contents[file]
		if !ok || len(contents) != 1 {
			return nil, fmt.Errorf("plan %s: missing blob %s", st.ID, file)
		}
		local := filepath.Join(r.Root, filepath.FromSlash(file))
		info, err := os.Lstat(local)
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("shadowing file conflicts with plan %s: %s", st.ID, file)
			}
			shadow, err := os.ReadFile(local)
			if err != nil {
				return nil, err
			}
			if string(shadow) != string(b) {
				return nil, fmt.Errorf("shadowing file conflicts with plan %s: %s", st.ID, file)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return b, nil
	}
	b, err := read("aeh/outcomes/" + st.ID + ".yaml")
	if err != nil {
		return o, nil, err
	}
	// Keep LoadOutcome's YAML parsing, ID default and normalization semantics.
	if err := yaml.Unmarshal(b, &o); err != nil {
		return o, nil, fmt.Errorf("parse outcome %s: %w", st.ID, err)
	}
	if o.ID == "" {
		o.ID = st.ID
	}
	if err := o.normalize(); err != nil {
		return o, nil, err
	}
	dir := cfg.plan().AcceptanceDir + "/" + st.ID + "/"
	if o.ID != st.ID || o.Feature != st.Feature || o.Level != 2 || len(o.Tasks) != 1 ||
		!reflect.DeepEqual(o.Acceptance, []string{dir}) || o.Route != st.Route {
		return o, nil, fmt.Errorf("outcome %s differs from its plan state", st.ID)
	}
	gen := &generatedRun{Plan: st, Paths: []string{dir}}
	b, err = read("aeh/seals/" + st.ID + ".json")
	if err != nil {
		return o, nil, err
	}
	if err := json.Unmarshal(b, &gen.Seal); err != nil {
		return o, nil, fmt.Errorf("plan %s seal: %w", st.ID, err)
	}
	contents, err := sealedGitContents(r.Root, st.PlanCommit, gen.Paths)
	if err != nil {
		return o, nil, err
	}
	want := sealFromContents(st.ID, contents, st.SpecDigest, gen.Seal.ApprovedAt, st.Absences)
	legacyManifest := len(st.Absences) == 0 && gen.Seal.Hash == want.Hash && reflect.DeepEqual(gen.Seal.Files, want.Files)
	if gen.Seal.Outcome != st.ID || gen.Seal.SealedBy != "runner" || gen.Seal.SpecContent != st.SpecDigest ||
		!reflect.DeepEqual(gen.Seal.Retired, want.Retired) {
		return o, nil, fmt.Errorf("plan %s seal does not match its pinned acceptance files or spec", st.ID)
	}
	gen.Trace, err = parsePlanFile(contents[dir+"plan.yaml"])
	if err != nil {
		return o, nil, err
	}
	if o.Tasks[0].Goal != gen.Trace.Task.Goal || !reflect.DeepEqual(o.Tasks[0].Paths, gen.Trace.Task.Paths) {
		return o, nil, fmt.Errorf("outcome %s task differs from plan.yaml", st.ID)
	}
	// A prior implementation identifies both the test paths and the immutable code
	// commit that owns their bytes. Never trust their current checkout or branch base.
	for _, c := range st.Delta.Unchanged {
		if !isFullSHA(st.PriorCode) {
			return o, nil, fmt.Errorf("plan %s has no prior implementation commit", st.ID)
		}
		for _, file := range c.Tests {
			if containsPath(st.Absences, file) {
				continue
			}
			if err := checkPlanTestPath(file); err != nil {
				return o, nil, err
			}
			old, err := sealedGitContents(r.Root, st.PriorCode, []string{file})
			if err != nil {
				return o, nil, err
			}
			b, ok := old[file]
			if !ok || len(old) != 1 {
				return o, nil, fmt.Errorf("historical sealed test is not a file: %s", file)
			}
			if previous, exists := contents[file]; exists {
				if string(previous) != string(b) {
					return o, nil, fmt.Errorf("conflicting sealed test: %s", file)
				}
				continue
			}
			contents[file] = b
			gen.Paths = append(gen.Paths, file)
		}
	}
	gen.Manifest = sealFromContents(st.ID, contents, st.SpecDigest, gen.Seal.ApprovedAt, st.Absences)
	if !legacyManifest && (gen.Seal.Hash != gen.Manifest.Hash || !reflect.DeepEqual(gen.Seal.Files, gen.Manifest.Files)) {
		return o, nil, fmt.Errorf("plan %s seal does not match its pinned acceptance files or spec", st.ID)
	}
	// Plan roles include rules observed while authoring tests, even if implementation
	// paths alone would not predict those roles.
	o.Reviewers = requiredRoles(st.Route, o.Reviewers, st.Roles)
	return o, gen, nil
}

func generatedPreflight(cfg Config, o Outcome, author string) error {
	if _, ok := cfg.Profiles[author]; !ok {
		return fmt.Errorf("test author profile %q is missing; cannot verify implementer independence", author)
	}
	for _, task := range o.Tasks {
		for _, profile := range cfg.reachableAuthors(task) {
			if err := cfg.independent(author, profile); err != nil {
				return fmt.Errorf("implementer must be independent from test author: %w", err)
			}
		}
	}
	return nil
}

func (ru *runner) generatedContract() any {
	if ru.generated == nil {
		return nil
	}
	p := ru.generated.Plan
	return struct {
		Provenance, PlanCommit, Author, SpecDigest, PriorCode string
		Delta                                                 CriteriaDelta
		Route                                                 string
		Roles                                                 []string
		Reviews                                               []ReviewVerdict
		Checks                                                []PlanCheckEntry
		Manifest                                              Seal
		Retired                                               []RetiredTest `json:",omitempty"`
	}{"plan", p.PlanCommit, p.Author, p.SpecDigest, p.PriorCode, p.Delta, p.Route, p.Roles, p.Reviews, p.Checks, ru.generated.Manifest, p.Retired}
}

func (ru *runner) sealedGate(base, commit string) GateResult {
	if g := ru.generated; g != nil {
		return gateSealedManifest(ru.repo.Root, commit, g.Manifest, g.Paths)
	}
	return gateSealedTree(ru.repo.Root, base, commit, ru.outcome.Acceptance)
}

// sealTracked passes when the files of a manual seal are exactly the tracked files under
// the acceptance paths at commit, with the approved bytes. Sandboxed gates see only the
// committed blobs, so an approved test that Git ignores, never tracked, or stores with
// other bytes (a clean filter) would otherwise not be the test that runs.
func sealTracked(root, commit string, s Seal, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	tracked, err := sealedGitContents(root, commit, paths)
	if err != nil {
		g.Output = "acceptance tests unreadable: " + err.Error()
		return g
	}
	sealed := map[string]bool{}
	var problems []string
	for _, f := range s.Files {
		sealed[f.Path] = true
		blob, ok := tracked[f.Path]
		switch {
		case !ok:
			problems = append(problems, "sealed but not tracked: "+f.Path)
		case !sealedBytes(blob, f.SHA256):
			problems = append(problems, "committed bytes differ from the approved ones: "+f.Path)
		}
	}
	for p := range tracked {
		if !sealed[p] {
			problems = append(problems, "tracked but not sealed: "+p)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		g.Output = "the sandbox runs only tracked acceptance files, and they differ from the approved ones (commit them, or approve the tests again):\n" +
			strings.Join(problems, "\n")
		return g
	}
	g.Passed = true
	return g
}

// sealedBytes reports whether a committed blob is the approved file. The only accepted
// difference is the one core.autocrlf makes on checkout: a blob without carriage returns
// whose LF line endings became CRLF in the approved file. The text is the same.
func sealedBytes(blob []byte, approved string) bool {
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) == approved {
		return true
	}
	if bytes.IndexByte(blob, '\r') >= 0 {
		return false
	}
	sum = sha256.Sum256(bytes.ReplaceAll(blob, []byte("\n"), []byte("\r\n")))
	return hex.EncodeToString(sum[:]) == approved
}

// acceptanceGate runs the new and unchanged criteria through the configured manifest
// command, in addition to the normal project suite. plan.yaml is sealed, but is not a test.
func (ru *runner) acceptanceGate(ctx context.Context, v *verification) GateResult {
	var files []string
	for _, f := range ru.generated.Manifest.Files {
		if f.Path != ru.outcome.Acceptance[0]+"plan.yaml" {
			files = append(files, f.Path)
		}
	}
	g, err := v.acceptance(ctx, "acceptance", ru.cfg.Commands.Acceptance, files)
	if err != nil {
		return GateResult{Name: "acceptance", Output: err.Error()}
	}
	if err := v.damaged(); err != nil {
		g.Passed, g.Output = false, g.Output+"\n"+err.Error()
	}
	return g
}

func (ru *runner) implementation() (*Implementation, error) {
	g := ru.generated
	spec, _ := ru.memory.Spec(ru.outcome.Feature)
	crit, _, err := spec.Criteria()
	if err != nil {
		return nil, err
	}
	im := &Implementation{Outcome: ru.outcome.ID, Approval: g.Plan.SpecDigest, SpecVersion: g.Plan.SpecVersion, Code: ru.state.Head, Retired: append([]RetiredTest(nil), g.Plan.Retired...)}
	if im.Criteria, err = implementedCriteria(im.Outcome, crit, g.Plan.Delta, g.Plan.Absences, g.Trace, ru.outcome.Acceptance[0]); err != nil {
		return nil, err
	}
	return im, nil
}
