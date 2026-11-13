package aeh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeReport writes .aeh/reports/<id>.md: criteria, sealed tests, every attempt and its gates.
// It returns the report path relative to the repository.
func (ru *runner) writeReport() string {
	o, st, r := ru.outcome, ru.state, ru.repo
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", o.ID, o.Title)
	fmt.Fprintf(&b, "Status: **%s** · Branch: `%s` · Base: `%s`\n\n", st.Status, st.Branch, short(st.Base))
	fmt.Fprintf(&b, "Recorded branch head: `%s` · Contract SHA-256: `%s`\n\n", st.Head, st.Contract)
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
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %.0f |\n", t.ID, a.N, a.Profile, result, gateSummary(r.Root, a.RunDir), a.Seconds)
		}
	}
	for _, task := range st.Tasks {
		for _, attempt := range task.Attempts {
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
