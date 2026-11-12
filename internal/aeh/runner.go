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
	reviewPrompt    string
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

// Do runs a level-1 change described in one sentence: one task, no sealed tests, no review.
func Do(ctx context.Context, r Repo, text string, paths []string, profile string, log io.Writer) (State, error) {
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
		ID:    fmt.Sprintf("DO-%d", time.Now().UnixNano()),
		Title: firstLine(text),
		Level: 1,
		Tasks: []Task{{ID: "T1", Goal: text, Paths: paths, Start: profile}},
	}
	return run(ctx, r, cfg, o, log)
}

func run(ctx context.Context, r Repo, cfg Config, o Outcome, log io.Writer) (State, error) {
	unlock, err := lockOutcome(r, o.ID)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	st, err := loadState(r, o.ID)
	if err != nil {
		return st, err
	}
	switch st.Status {
	case StatusAccepted:
		return st, fmt.Errorf("%s is already accepted on branch %s", o.ID, st.Branch)
	case StatusBlocked:
		return st, fmt.Errorf("%s is blocked; answer it first: aeh answer %s \"...\"\n%s", o.ID, o.ID, st.Question)
	}
	if err := preflight(cfg, o); err != nil {
		return st, err
	}
	ru := &runner{repo: r, cfg: cfg, outcome: o, protected: protectedPaths(cfg, o), state: &st, log: log}
	if ru.implementPrompt, err = loadPrompt(r, "implement.md"); err != nil {
		return st, fmt.Errorf("load implementation prompt: %w", err)
	}
	if o.Level >= 2 {
		if ru.reviewPrompt, err = loadPrompt(r, "review.md"); err != nil {
			return st, fmt.Errorf("load review prompt: %w", err)
		}
	}
	if len(o.Acceptance) > 0 {
		if ru.seal, err = verifySeal(r, o); err != nil {
			return st, err
		}
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

	ok, question, err := ru.finalCheck(ctx)
	if err != nil {
		return st, err
	}
	if !ok {
		err := ru.block(question)
		return st, err
	}
	st.Status = StatusAccepted
	if err := saveState(r, &st); err != nil {
		return st, err
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
	data, err := json.Marshal(struct {
		Commands        any
		PassEnv         []string
		Protected       []string
		Limits          any
		Outcome         Outcome
		Seal            string
		ImplementPrompt string
		ReviewPrompt    string
	}{commands, ru.cfg.PassEnv, ru.cfg.Protected, ru.cfg.Limits, outcome, seal, ru.implementPrompt, ru.reviewPrompt})
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
	Feedback  string
	Uncertain bool
}

// attempt runs one agent in a fresh worktree and passes the result through the gates.
func (ru *runner) attempt(ctx context.Context, t Task, n int, profile string, maxLines int, prev attemptResult) (attemptResult, error) {
	o, r := ru.outcome, ru.repo
	name := fmt.Sprintf("%s-a%d", t.ID, n)
	runDir := r.RunDir(o.ID, name)
	res := attemptResult{Attempt: Attempt{N: n, Profile: profile, RunDir: rel(r.Root, runDir), StartedAt: time.Now().UTC()}}

	base := ru.state.Head
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
	})
	if err != nil {
		return res, fmt.Errorf("render prompt: %w", err)
	}
	ru.logf("  %s attempt %d with profile %q", t.ID, n, profile)
	var gates []GateResult
	check := func(g GateResult) bool {
		gates = append(gates, g)
		status := "ok"
		if !g.Passed {
			status = "FAIL: " + firstLine(g.Output)
		}
		ru.logf("    %-6s %s", g.Name, status)
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
	verified := wt + "-verify"
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
	if err := writePrivateJSON(filepath.Join(runDir, "candidate.json"), map[string]string{"base": base, "commit": commit, "contract": ru.state.Contract}); err != nil {
		return res, err
	}
	timeout := time.Duration(ru.cfg.Commands.TimeoutMinutes) * time.Minute
	passed := true
	if ru.seal != nil {
		passed = check(gateHash(verified, *ru.seal, o.Acceptance))
	}
	var diff string
	if passed {
		changes, err := candidateChanges(verified, base, commit)
		if err != nil {
			return res, err
		}
		diff, err = candidateDiff(verified, base, commit)
		if err != nil {
			return res, err
		}
		if err := os.WriteFile(filepath.Join(runDir, "diff.patch"), []byte(diff), 0o600); err != nil {
			return res, err
		}
		passed = check(gateScope(changes, ru.protected, t.Paths, maxLines))
	}
	if passed {
		passed = check(checkoutGate(gateCommand(ctx, "build", verified, ru.cfg.Commands.Build, timeout, ru.cfg.PassEnv), verified, commit))
	}
	if passed {
		passed = check(checkoutGate(gateCommand(ctx, "test", verified, ru.cfg.Commands.Test, timeout, ru.cfg.PassEnv), verified, commit))
	}
	if passed && o.Level >= 2 {
		g, err := ru.review(ctx, t, verified, diff, runDir)
		if err != nil {
			return res, err
		}
		passed = check(checkoutGate(g, verified, commit))
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

// review asks the reviewer profile for a verdict on the diff. It only runs at level 2.
func (ru *runner) review(ctx context.Context, t Task, wt, diff, runDir string) (GateResult, error) {
	g := GateResult{Name: "review"}
	start := time.Now()
	prompt, err := renderPrompt("review.md", ru.reviewPrompt, promptData{Outcome: ru.outcome, Task: t, Diff: diff})
	if err != nil {
		return g, err
	}
	outPath, err := runAgent(ctx, ru.cfg.Profiles[ru.cfg.Reviewer], ru.cfg.PassEnv, wt, prompt, runDir, "review")
	g.Seconds = time.Since(start).Seconds()
	if err != nil {
		g.Output, g.Uncertain = err.Error(), uncertainExit(err)
		return g, nil
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return g, fmt.Errorf("read reviewer output: %w", err)
	}
	switch parseVerdict(string(out)) {
	case "APPROVE":
		g.Passed = true
	case "REJECT":
		g.Output = "reviewer rejected the change:\n" + tail(string(out), maxGateOutput)
	default:
		g.Output = "reviewer gave no verdict"
	}
	return g, nil
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

// finalCheck runs the gates once more on the integrated branch: accepted means proven there.
func (ru *runner) finalCheck(ctx context.Context) (bool, string, error) {
	o, r := ru.outcome, ru.repo
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
		if !g.Passed {
			ru.logf("    %-6s FAIL: %s", g.Name, firstLine(g.Output))
			if g.Uncertain {
				return false, fmt.Sprintf("Final %s stopped. Confirm all processes stopped and review possible effects before answering. See the final check artifacts.", g.Name), nil
			}
			return false, fmt.Sprintf("The integrated branch fails the %q check:\n%s", g.Name, tail(strings.TrimSpace(g.Output), 1500)), nil
		}
		ru.logf("    %-6s ok", g.Name)
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
func preflight(cfg Config, o Outcome) error {
	names := append([]string{}, cfg.Ladder...)
	if o.Level >= 2 {
		if cfg.Reviewer == "" {
			return errors.New("level 2 requires a reviewer profile")
		}
		for _, task := range o.Tasks {
			first := task.Start
			if first == "" {
				first = cfg.Ladder[0]
			}
			if cfg.Reviewer == first || cfg.Reviewer == cfg.escalate(first) {
				return fmt.Errorf("task %s requires a reviewer profile separate from its implementation profiles", task.ID)
			}
		}
		names = append(names, cfg.Reviewer)
	}
	for _, t := range o.Tasks {
		if t.Start != "" {
			names = append(names, t.Start)
		}
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
	out := []string{"aeh/"}
	out = append(out, cfg.Protected...)
	return append(out, o.Acceptance...)
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
