package aeh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// reportCommand refreshes the local report while the discovery command still holds its lock.
func (d *discoverySession) reportCommand(commandErr *error) {
	if d.state == nil {
		return
	}
	err := d.refreshReview()
	if err == nil {
		err = d.writeDiscoveryReport()
	}
	if *commandErr == nil && err != nil {
		*commandErr = err
	}
}

func renderBookReview(b *strings.Builder, v *DiscoveryBookReview) {
	validity := "current"
	if v.Stale {
		validity = "stale"
	}
	fmt.Fprintf(b, "\n### Advisory book review\n\nReviewer: %s (%s)\n\nStatus: %s; %s\n\nPrompt template SHA-256: %s\n\nBook SHA-256: %s\n",
		discoveryDiagnostic(v.Profile), discoveryDiagnostic(v.Provider), v.Status, validity, v.Prompt, v.Book)
	if v.Feedback != "" {
		fmt.Fprintf(b, "\nFeedback: %s\n", discoveryDiagnostic(v.Feedback))
	}
}

func (d *discoverySession) writeDiscoveryReport() error {
	st := d.state
	var b strings.Builder
	fmt.Fprintf(&b, "# Discovery %s\n\nPhase: %s\n\n", st.Slug, st.Phase)
	b.WriteString("Secret detection is best effort and cannot replace minimizing agent input. Reviews are advisory; promotion and acceptance are owner actions, not feature approvals.\n\n")
	fmt.Fprintf(&b, "## Idea\n\n%s\n\n", st.Idea)
	if st.Question != "" {
		fmt.Fprintf(&b, "## Runner feedback\n\n%s\n\n", discoveryDiagnostic(st.Question))
	}
	for _, round := range st.Rounds {
		b.WriteString(d.renderRound(round))
		b.WriteString("\n")
	}
	if st.Book != nil {
		fmt.Fprintf(&b, "## Pending book\n\nRound %d; author attempt %d; same saved snapshot will be resumed.\n", st.Book.Round, st.Book.Attempt)
		if st.Book.Review != nil {
			renderBookReview(&b, st.Book.Review)
		}
	}
	b.WriteString("\n## Attempts\n\n")
	for _, a := range st.Attempts {
		fmt.Fprintf(&b, "- Round %d, attempt %d: profile %s; %s; %.3f seconds. %s\n", a.Round, a.N, discoveryDiagnostic(a.Profile), a.Result, a.Seconds, discoveryDiagnostic(firstLine(a.Feedback)))
	}
	for _, group := range []struct {
		title   string
		actions []DiscoveryAction
	}{{"Promotions", st.Promotions}, {"Acceptances", st.Acceptances}} {
		fmt.Fprintf(&b, "\n## %s\n\n", group.title)
		for _, a := range group.actions {
			fmt.Fprintf(&b, "- %s to %s (%s), content SHA-256 %s.\n", a.Name, a.Target, a.Date, a.Content)
		}
	}
	text := b.String()
	if err := checkNoSecrets("discovery report", []byte(text)); err != nil {
		return err
	}
	if err := checkDisplayMarkdown(text, 8<<20); err != nil {
		return fmt.Errorf("discovery report: %w", err)
	}
	return writeFileAtomic(filepath.Join(d.repo.Root, ".aeh", "reports", "discover-"+st.Slug+".md"), []byte(text))
}

// writeReport writes .aeh/reports/<id>.md: criteria, sealed tests, every attempt and its gates.
// It returns the report path relative to the repository.
func (ru *runner) writeReport() string {
	o, st, r := ru.outcome, ru.state, ru.repo
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", o.ID, o.Title)
	fmt.Fprintf(&b, "Status: **%s** · Branch: `%s` · Base: `%s`\n\n", st.Status, st.Branch, short(st.Base))
	fmt.Fprintf(&b, "Recorded branch head: `%s` · Contract SHA-256: `%s`\n\n", st.Head, st.Contract)
	fmt.Fprintf(&b, "Route: initial **%s**, effective **%s** · Source: `%s`\n\nRules hit: %s\n\nRequired roles: %s\n\n", st.InitialRoute, st.EffectiveRoute, st.TriageSource, strings.Join(st.RulesHit, ", "), strings.Join(st.Roles, ", "))
	if len(st.RulesDeferred) > 0 {
		fmt.Fprintf(&b, "Rules deferred to the changed files (they may match below a broad task path): %s\n\n", strings.Join(st.RulesDeferred, ", "))
	}
	if st.TriageSource == "fallback-none" {
		fmt.Fprintf(&b, "%s\n\n", changeDefaultWarning)
	}
	if st.TriageWorktree != "" {
		fmt.Fprintf(&b, "Retained triage worktree: `%s`\n\n", st.TriageWorktree)
	}
	if ru.generated != nil {
		p := ru.generated.Plan
		fmt.Fprintf(&b, "## Plan\n\nPlan commit: `%s` · Test author: `%s` · sealed by runner\n\n", p.PlanCommit, p.Author)
		b.WriteString("Plan checks:\n\n")
		for _, c := range p.Checks {
			status := "FAIL"
			if c.Passed {
				status = "ok"
			}
			fmt.Fprintf(&b, "- %s: %s %s\n", c.Name, status, c.Detail)
		}
		b.WriteString("\nPlan reviews:\n\n")
		for _, v := range p.Reviews {
			fmt.Fprintf(&b, "- Role: **%s** · Profile: `%s` · Provider: `%s` · SHA: `%s` · Verdict: **%s**\n", v.Role, v.Profile, v.Provider, v.SHA, v.Verdict)
		}
		b.WriteString("\nThese checks and declared traceability are evidence, not proof of semantic coverage; independent reviewers judge why each test fails.\n\n")
	}
	if o.Feature != "" {
		fmt.Fprintf(&b, "Declared feature: `%s` · Change: `%s`\n\n", o.Feature, o.Change)
		if result := ru.memoryRecordSummary(); result != "" {
			fmt.Fprintf(&b, "%s\n\n", result)
		}
	}
	if st.Question != "" {
		fmt.Fprintf(&b, "## Open question\n\n%s\n\n", st.Question)
	}
	if len(o.Criteria) > 0 {
		b.WriteString("## Criteria\n\n")
		for _, c := range o.Criteria {
			fmt.Fprintf(&b, "- Given %s, when %s, then %s\n", c.Given, c.When, c.Then)
		}
		b.WriteString("\n")
	}
	if ru.seal != nil {
		fmt.Fprintf(&b, "## Sealed acceptance tests\n\nApproved %s · hash `%s`\n\n", ru.seal.ApprovedAt.Format("2006-01-02 15:04 UTC"), short(ru.seal.Hash))
		for _, f := range ru.seal.Files {
			fmt.Fprintf(&b, "- `%s`\n", f.Path)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Attempts\n\n| Task | # | Profile | Result | Gates | Seconds |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, t := range st.Tasks {
		for _, a := range t.Attempts {
			result := "accepted " + short(a.Commit)
			if !a.Passed {
				result = "failed at " + a.FailedGate
			}
			if a.SpecQuestion != nil {
				result = "paused for SPEC-QUESTION"
			}
			if a.ResumedFromCursor {
				result += "; resumed from cursor"
			}
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %.0f |\n", t.ID, a.N, a.Profile, result, gateSummary(r.Root, a.RunDir), a.Seconds)
		}
	}
	for _, task := range st.Tasks {
		for _, attempt := range task.Attempts {
			if attempt.SpecQuestion != nil {
				fmt.Fprintf(&b, "\n%s attempt %d: %s\n", task.ID, attempt.N, attempt.SpecQuestion.Summary())
			}
			for _, v := range attempt.Reviews {
				fmt.Fprintf(&b, "\n%s attempt %d: review **%s** · Profile: `%s` · Provider: `%s` · SHA: `%s` · Verdict: **%s** · Prompt: `%s`\n", task.ID, attempt.N, v.Role, v.Profile, v.Provider, v.SHA, v.Verdict, v.PromptSource)
			}
			if len(attempt.RolesNotRun) > 0 {
				fmt.Fprintf(&b, "\n%s attempt %d: roles not run: %s\n", task.ID, attempt.N, strings.Join(attempt.RolesNotRun, ", "))
			}
			writeDriftEvidence(&b, r.Root, attempt.RunDir, fmt.Sprintf("%s attempt %d", task.ID, attempt.N))
		}
	}
	if st.FinalRun != "" {
		fmt.Fprintf(&b, "\n## Final check on the integrated branch\n\n%s · `%s`\n", gateSummary(r.Root, st.FinalRun), st.FinalRun)
		writeDriftEvidence(&b, r.Root, st.FinalRun, "Final check")
	}
	for _, task := range st.Tasks {
		for _, attempt := range task.Attempts {
			if attempt.Worktree != "" {
				fmt.Fprintf(&b, "\nRetained worktree for %s attempt %d: `%s`\n", task.ID, attempt.N, attempt.Worktree)
			}
		}
	}
	if len(st.Answers) > 0 {
		b.WriteString("\n## Answers from the owner\n\n")
		for _, a := range st.Answers {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	path := r.ReportPath(o.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		ru.logf("warning: create report directory: %v", err)
		return ""
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		ru.logf("warning: write report: %v", err)
		return ""
	}
	return rel(r.Root, path)
}

// gateSummary renders "hash ok, scope ok, build FAIL" from a run directory's gates.json.
func gateSummary(root, runDir string) string {
	gates := readGateResults(root, runDir)
	parts := make([]string, len(gates))
	for i, g := range gates {
		status := "ok"
		if g.Skipped {
			status = "skipped"
		} else if !g.Passed {
			status = "FAIL"
		}
		if g.Sandboxed {
			status += " (sandbox)"
		}
		parts[i] = g.Name + " " + status
	}
	return strings.Join(parts, ", ")
}

func readGateResults(root, runDir string) []GateResult {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(runDir), "gates.json"))
	if err != nil {
		return nil
	}
	var gates []GateResult
	if json.Unmarshal(data, &gates) != nil {
		return nil
	}
	return gates
}

func writeDriftEvidence(b *strings.Builder, root, runDir, label string) {
	for _, g := range readGateResults(root, runDir) {
		if g.Name != "drift" {
			continue
		}
		status := "ok"
		if g.Skipped {
			status = "skipped"
		} else if !g.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(b, "\n### %s: drift %s\n\n%s\n", label, status, g.Output)
		for _, warning := range g.Warnings {
			fmt.Fprintf(b, "\nWarning: %s\n", warning)
		}
	}
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// reportCommand runs while the project lock is still held, including on failed commands.
func (a *adoptSession) reportCommand(commandErr *error) {
	st, err := adoptionStatus(a)
	if err == nil {
		err = writeAdoptReport(a, st)
	}
	if err != nil && *commandErr == nil {
		*commandErr = redactErr(fmt.Errorf("adopt report: %w", err))
	}
}

func writeAdoptReport(a *adoptSession, st AdoptStatusSnapshot) error {
	var b strings.Builder
	b.WriteString("# Adoption\n\nSecret filtering is best effort. Excluded files minimize agent input; cooperative read-only checks detect changes and do not isolate agent processes.\n\n")
	fmt.Fprintf(&b, "Inventory: %s\n\n", st.Inventory)
	if st.Base != "" {
		fmt.Fprintf(&b, "Accepted inventory base SHA: %s; date: %s.\n\n", st.Base, st.Date)
	}
	root, err := loadAdoptState(a.repo, "")
	if err != nil {
		return err
	}
	if root.state.Next != nil {
		fmt.Fprintf(&b, "Pending inventory base SHA: %s; date: %s. Accept with `aeh adopt accept system`.\n\n", root.state.Next.Base, root.state.Next.Date)
	}
	for _, area := range st.Areas {
		fmt.Fprintf(&b, "- %s: %s; paths: %s\n", area.Feature, area.State, strings.Join(area.Paths, ", "))
	}
	b.WriteString("\n## Inspections\n\n")
	sessions := []*adoptSession{root}
	for _, area := range st.Areas {
		session, err := loadAdoptSession(a.repo, area.Feature)
		if err != nil {
			return err
		}
		sessions = append(sessions, session)
	}
	// Failed first area attempts have state but no memory area yet.
	entries, err := os.ReadDir(filepath.Join(a.repo.Root, ".aeh", "adopt"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || checkSlug(e.Name()) != nil {
			continue
		}
		found := false
		for _, session := range sessions {
			if session.feature == e.Name() {
				found = true
			}
		}
		if !found {
			session, err := loadAdoptSession(a.repo, e.Name())
			if err != nil {
				return err
			}
			sessions = append(sessions, session)
		}
	}
	for _, session := range sessions {
		label := session.feature
		if label == "" {
			label = "system"
		}
		for _, attempt := range session.state.Attempts {
			fmt.Fprintf(&b, "- %s attempt %d: %s; base %s; profile %s; excluded paths %d; wrote into code: %t.\n", label, attempt.N,
				attempt.Result, attempt.Base, discoveryDiagnostic(attempt.Profile), attempt.Excluded, attempt.WroteCode)
			if attempt.Feedback != "" {
				fmt.Fprintf(&b, "\n%s\n\n", discoveryDiagnostic(attempt.Feedback))
			}
		}
		if p := session.state.Promotion; p != nil {
			fmt.Fprintf(&b, "\nPromoted %s to %s (%s). Owner approval is still required.\n", p.Name, p.Target, p.Date)
		}
	}
	if v := st.Verification; v != nil {
		fmt.Fprintf(&b, "\n## Verification\n\nBase SHA: %s; date: %s; config SHA-256: %s; passed: %t.\n\n", v.Base, v.Date, v.ConfigSHA, v.Passed)
		if v.Sandbox != "" {
			fmt.Fprintf(&b, "Commands ran in the sandbox (image %s).\n\n", v.Sandbox)
		}
		for _, c := range v.Commands {
			fmt.Fprintf(&b, "- %s: exit %d; %.3fs; passed: %t; command: %s; integrity: %s\n", c.Name, c.Exit, c.Seconds, c.Passed,
				discoveryDiagnostic(c.Command), discoveryDiagnostic(c.Integrity))
			if c.Class != "" && c.Class != ClassProcess {
				fmt.Fprintf(&b, "  ended as %s: %s\n", c.Class, discoveryDiagnostic(c.Reason))
			}
		}
	}
	b.WriteString("\n## Uncovered groups at the accepted inventory base\n\n")
	for _, group := range st.Uncovered {
		fmt.Fprintf(&b, "- %s\n", group)
	}
	text := b.String()
	if err := gateBatchFiles(map[string][]byte{"adopt.md": []byte(text)}); err != nil {
		return err
	}
	return writeFileAtomic(a.repo.ReportPath("adopt"), []byte(text))
}
