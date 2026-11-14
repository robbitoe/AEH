package aeh

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
}

// RunOutcome executes an outcome from aeh/outcomes/<id>.yaml until it is accepted or blocked.
func RunOutcome(ctx context.Context, r Repo, id string, log io.Writer) (State, error) {
	cfg, err := r.LoadConfig()
	if err != nil {
		return State{}, err
	}
	o, err := r.LoadOutcome(id)
	if err != nil {
		return State{}, err
	}
	return run(ctx, r, cfg, o, log)
}

// Do runs a level-1 change described in one sentence, with route-based reviews.
// link declares the feature the change modifies, enforced by the drift gate.
// A nonempty route overrides model triage, just like route: in an outcome.
func Do(ctx context.Context, r Repo, text string, paths []string, profile string, link MemoryLink, log io.Writer, route string) (State, error) {
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
	return run(ctx, r, cfg, o, log)
}

func run(ctx context.Context, r Repo, cfg Config, o Outcome, log io.Writer) (State, error) {
	// One run or memory write at a time per project, across linked worktrees.
	unlockProject, err := lockProject(r)
	if err != nil {
		return State{}, err
	}
	defer unlockProject()
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
	if st.Version != 2 {
		return st, errors.New("pre-stage-2 state cannot resume; relaunch with another outcome ID")
	}
	if st.RestartRequired {
		return st, errors.New(st.Question)
	}
	if st.Title == "" {
		st.Title = o.Title
	}
	switch st.Status {
	case StatusAccepted:
		return st, fmt.Errorf("%s is already accepted on branch %s", o.ID, st.Branch)
	case StatusBlocked:
		return st, fmt.Errorf("%s is blocked; answer it first: aeh answer %s \"...\"\n%s", o.ID, o.ID, st.Question)
	}
	ru := &runner{repo: r, cfg: cfg, outcome: o, protected: protectedPaths(cfg, o), state: &st, log: log, memory: memory}
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
	if o.Level >= 2 && len(o.Acceptance) > 0 {
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
	if current, exists := branchCommit(r.Root, branch); st.Contract != "" || exists || st.Branch != "" {
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
		if err != nil || current.Fingerprint != memory.Fingerprint {
			reason := ErrMemoryChanged.Error()
			if err != nil {
				reason = err.Error()
			}
			err := ru.block(fmt.Sprintf("Cannot accept: %s. Inspect product memory and use a new outcome ID if its contract changed. What should change before retrying?", reason))
			return st, err
		}
		pending = PendingRecord{OutcomeID: o.ID, Feature: o.Feature, Change: o.Change, Note: o.Changelog, Commit: st.Head}
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
	}{commands, ru.cfg.PassEnv, ru.cfg.Protected, ru.cfg.Limits, outcome, seal, ru.implementPrompt, ru.reviewPrompts,
		ru.triagePrompt, ru.state.TriageOutput, ru.state.InitialRoute, ru.cfg.Triage.Rules,
		memoryContract{ru.memory.ContractFingerprint(outcome.Feature), outcome.Feature, outcome.Change, outcome.Changelog}})
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
	ts.Status = StatusRunning
	var last attemptResult
	for i := 0; i < maxAttempts; i++ {
		profile := first
		if i == maxAttempts-1 {
			profile = ru.cfg.escalate(first)
		}
		n := len(ts.Attempts) + 1
		res, err := ru.attempt(ctx, t, n, profile, maxLines, last)
		res.Seconds = time.Since(res.StartedAt).Seconds()
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
func (ru *runner) attempt(ctx context.Context, t Task, n int, profile string, maxLines int, prev attemptResult) (attemptResult, error) {
	o, r := ru.outcome, ru.repo
	name := fmt.Sprintf("%s-a%d", t.ID, n)
	runDir := r.RunDir(o.ID, name)
	res := attemptResult{Attempt: Attempt{N: n, Profile: profile, RunDir: rel(r.Root, runDir), StartedAt: time.Now().UTC()}}

	base := ru.state.Head
	res.Base = base
	if current, _ := branchCommit(r.Root, IntegrationBranch(o.ID)); current != base {
		return res, fmt.Errorf("integration branch %s changed", IntegrationBranch(o.ID))
	}
	branch := AttemptBranch(o.ID, t.ID, n)
	wt := r.WorktreePath(o.ID + "-" + name)
	if err := addWorktree(r.Root, wt, branch, base); err != nil {
		return res, err
	}
	defer func() {
		if res.Worktree != wt {
			removeWorktree(r.Root, wt)
			deleteBranch(r.Root, branch)
		}
	}()

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
	if _, err := runAgent(ctx, ru.cfg.Profiles[profile], ru.cfg.PassEnv, wt, prompt, runDir, "agent"); err != nil {
		res.FailedGate, res.Feedback = "agent", err.Error()
		res.Uncertain = uncertainExit(err)
		if res.Uncertain {
			res.Worktree = wt
		}
		check(GateResult{Name: res.FailedGate, Output: res.Feedback, Uncertain: res.Uncertain})
		return res, record()
	}

	commit, err := candidateCommit(wt, base, fmt.Sprintf("aeh: %s %s %s (attempt %d, %s)", o.ID, t.ID, firstLine(t.Goal), n, profile))
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
	// Hash and scope read Git objects in the main repository, so nothing from the
	// candidate is checked out, and no checkout filter runs, until both have passed.
	passed := true
	if ru.seal != nil {
		passed = check(gateSealedTree(r.Root, base, commit, o.Acceptance))
	}
	if passed {
		passed = check(gateScope(changes, ru.protected, t.Paths, maxLines))
		if passed {
			passed = check(gateDrift(changedPaths(changes), ru.memory, o.Feature))
		}
	}
	verified := wt + "-verify"
	if passed {
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
	timeout := time.Duration(ru.cfg.Commands.TimeoutMinutes) * time.Minute
	if passed {
		passed = check(checkoutGate(gateCommand(ctx, "build", verified, ru.cfg.Commands.Build, timeout, ru.cfg.PassEnv), verified, commit))
	}
	if passed {
		passed = check(checkoutGate(gateCommand(ctx, "test", verified, ru.cfg.Commands.Test, timeout, ru.cfg.PassEnv), verified, commit))
	}
	if passed {
		roles := requiredRoles(ru.state.EffectiveRoute, ru.state.Roles)
		res.RolesNotRun = nil
		for i, role := range roles {
			g, verdict, err := ru.review(ctx, t, role, profile, commit, verified, diff, runDir)
			if err != nil {
				return res, err
			}
			res.Reviews = append(res.Reviews, verdict)
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
func (ru *runner) review(ctx context.Context, t Task, role, author, commit, wt, diff, runDir string) (GateResult, ReviewVerdict, error) {
	g := GateResult{Name: "review"}
	v := ReviewVerdict{Role: role, SHA: commit, Verdict: "INVALID", PromptSource: ru.reviewSources[role]}
	profile, err := ru.cfg.reviewerFor(role, author)
	if err != nil {
		return g, v, err
	}
	v.Profile, v.Provider = profile, ru.cfg.Profiles[profile].Provider
	start := time.Now()
	prompt, err := renderPrompt("review-"+role+".md", ru.reviewPrompts[role], promptData{Outcome: ru.outcome, Task: t, Diff: diff})
	if err != nil {
		return g, v, err
	}
	ru.logf("    review %s with %q (%s) on %s", role, profile, v.Provider, commit)
	outPath, err := runAgent(ctx, ru.cfg.Profiles[profile], ru.cfg.PassEnv, wt, prompt, runDir, "review-"+role)
	g.Seconds = time.Since(start).Seconds()
	// Check even a rejected or failed reviewer: all reviewers are read-only.
	checkoutErr := verifyCheckout(wt, commit)
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
	if checkoutErr != nil {
		g.Passed, g.Output, v.Verdict = false, checkoutErr.Error(), "INVALID"
	}
	return g, v, nil
}

// parseVerdict accepts one exact verdict on the final nonempty line of plain stdout.
func parseVerdict(out string) string {
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
	wtRoot := filepath.Dir(r.WorktreePath(ru.outcome.ID + "-reviews"))
	if err := os.MkdirAll(wtRoot, 0o700); err != nil {
		return false, "", err
	}
	wt, err := os.MkdirTemp(wtRoot, ru.outcome.ID+"-"+t.ID+"-reviews-")
	if err != nil {
		return false, "", err
	}
	if err := addWorktreeDetached(r.Root, wt, a.Commit); err != nil {
		_ = os.Remove(wt)
		return false, "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(r.Root, wt)
		}
	}()
	if err := verifyCheckout(wt, a.Commit); err != nil {
		return false, "", err
	}
	gates := readGateResults(r.Root, a.RunDir)
	for i, role := range missing {
		g, verdict, err := ru.review(ctx, t, role, a.Profile, a.Commit, wt, diff, runDir)
		if err != nil {
			g.Output = err.Error()
		}
		a.Reviews = append(a.Reviews, verdict)
		a.RolesNotRun = append([]string(nil), missing[i+1:]...)
		gates = append(gates, g)
		ru.logf("    added %s review for %s: %s", role, t.ID, gateStatus(g))
		question := ""
		if !g.Passed {
			keep = g.Uncertain
			if keep {
				a.Worktree = wt
			}
			question = fmt.Sprintf("Task %s required %s review failed on its accepted SHA %s:\n%s\n", t.ID, role, a.Commit, tail(strings.TrimSpace(g.Output), 1500))
			switch {
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
func (ru *runner) finalCheck(ctx context.Context) (bool, string, error) {
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
	wtRoot := filepath.Dir(r.WorktreePath(o.ID + "-final"))
	if err := os.MkdirAll(wtRoot, 0o700); err != nil {
		return false, "", err
	}
	wt, err := os.MkdirTemp(wtRoot, o.ID+"-final-")
	if err != nil {
		return false, "", err
	}
	if err := addWorktreeDetached(r.Root, wt, head); err != nil {
		_ = os.Remove(wt)
		return false, "", err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(r.Root, wt)
		}
	}()
	if err := verifyCheckout(wt, head); err != nil {
		return false, "", err
	}
	ru.logf("  final check on %s", IntegrationBranch(o.ID))
	timeout := time.Duration(ru.cfg.Commands.TimeoutMinutes) * time.Minute
	gates := []GateResult{}
	if ru.seal != nil {
		gates = append(gates, gateHash(wt, *ru.seal, o.Acceptance))
	}
	if len(gates) == 0 || gates[len(gates)-1].Passed {
		changes, err := candidateChanges(r.Root, ru.state.Base, ru.state.Head)
		if err != nil {
			return false, "", fmt.Errorf("list final changes: %w", err)
		}
		gates = append(gates, gateDrift(changedPaths(changes), ru.memory, o.Feature))
	}
	if gates[len(gates)-1].Passed {
		gates = append(gates, checkoutGate(gateCommand(ctx, "build", wt, ru.cfg.Commands.Build, timeout, ru.cfg.PassEnv), wt, head))
	}
	if gates[len(gates)-1].Passed {
		gates = append(gates, checkoutGate(gateCommand(ctx, "test", wt, ru.cfg.Commands.Test, timeout, ru.cfg.PassEnv), wt, head))
	}
	keep = gates[len(gates)-1].Uncertain
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
	if strings.HasPrefix(ru.outcome.ID, "DO-") {
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
		if _, err := exec.LookPath(p.Command[0]); err != nil {
			return fmt.Errorf("profile %q: %s is not installed or not on PATH", n, p.Command[0])
		}
	}
	return nil
}

const changeDefaultWarning = "warning: no triage profile configured; defaulting to change"

// observeRules persists the maximum policy seen across all candidates and tasks.
func (ru *runner) observeRules(hit []TriageRule) {
	s := ru.state
	s.EffectiveRoute, s.Roles = applyRules(s.EffectiveRoute, s.Roles, hit)
	s.Roles = requiredRoles(s.EffectiveRoute, s.Roles, ru.outcome.Reviewers)
	for _, name := range ruleNames(hit) {
		found := false
		for _, have := range s.RulesHit {
			if have == name {
				found = true
			}
		}
		if !found {
			s.RulesHit = append(s.RulesHit, name)
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
	ru.observeRules(rulesForTasks(ru.cfg.Triage.Rules, o.Tasks))
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
	wt := ru.repo.WorktreePath(o.ID + "-triage")
	if err := addWorktreeDetached(ru.repo.Root, wt, base); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			removeWorktree(ru.repo.Root, wt)
		}
	}()
	if err := verifyCheckout(wt, base); err != nil {
		return err
	}
	runDir := ru.repo.RunDir(o.ID, "triage")
	outPath, runErr := runAgent(ctx, ru.cfg.Profiles[ru.cfg.Triage.Profile], ru.cfg.PassEnv, wt, prompt, runDir, "triage")
	out, readErr := os.ReadFile(outPath)
	s.TriageOutput = string(out)
	checkoutErr := verifyCheckout(wt, base)
	parsed := parseRoute(string(out))
	if runErr == nil && readErr == nil && checkoutErr == nil && parsed != "" {
		s.InitialRoute, s.TriageSource = parsed, "model"
		// The provisional new route was a crash fallback, not an observation.
		s.EffectiveRoute, s.Roles = parsed, nil
		ru.observeRules(rulesForTasks(ru.cfg.Triage.Rules, o.Tasks))
	}
	if uncertainExit(runErr) {
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
		ru.state.MemoryVersion, ru.state.MemoryCommit, err = RecordChange(ru.repo, p, ru.memory.Fingerprint)
		if err == nil {
			if clearErr := ClearPending(ru.repo, p.OutcomeID); clearErr != nil {
				err = fmt.Errorf("clear pending record: %w", clearErr)
			}
		}
	}
	ru.state.MemoryError = ""
	if err != nil {
		ru.state.MemoryError = err.Error()
	}
	ru.logf("%s", ru.memoryRecordSummary())
	if err := saveState(ru.repo, ru.state); err != nil {
		ru.logf("warning: save memory record result: %v", err)
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
