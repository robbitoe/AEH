package aeh

import (
	"strings"
	"testing"
)

// A plan reads only VERDICT lines, so its reviewers must not be told to ask SPEC-QUESTION;
// outcome reviews keep the question.
func TestPlanReviewPromptsHaveNoSpecQuestion(t *testing.T) {
	for _, role := range []string{RoleQA, RoleSecurity, RoleDBA} {
		name := "review-" + role + ".md"
		b, err := defaultPrompts.ReadFile("prompts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := renderPrompt(name, string(b), promptData{Plan: true})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan, "SPEC-QUESTION") || !strings.Contains(plan, "name the decision the owner must make") {
			t.Errorf("%s in a plan still offers SPEC-QUESTION:\n%s", name, plan)
		}
		run, err := renderPrompt(name, string(b), promptData{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(run, "SPEC-QUESTION: <question>") {
			t.Errorf("%s in a run lost SPEC-QUESTION", name)
		}
	}
	if !newPlanPromptData(PlanInput{}).Plan {
		t.Error("plan prompt data must set Plan")
	}
}

// Outcome reviews list changed files with their status so an added file is visible
// without reading the whole diff; plan reviews (no Files) keep their layout.
func TestReviewPromptsListFileStatus(t *testing.T) {
	for _, role := range []string{RoleQA, RoleSecurity, RoleDBA} {
		name := "review-" + role + ".md"
		b, err := defaultPrompts.ReadFile("prompts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		// A Windows checkout may convert the embedded template to CRLF.
		b = []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
		got, err := renderPrompt(name, string(b), promptData{Files: "A src/new.js\nM src/old.js", Diff: "diff"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "\n\n## Files changed\n") || !strings.Contains(got, "A src/new.js\nM src/old.js") ||
			strings.Index(got, "## Files changed") > strings.Index(got, "## Diff") || !strings.Contains(got, "```\n\n## Diff\n") {
			t.Errorf("%s lacks the file status list:\n%s", name, got)
		}
		plain, err := renderPrompt(name, string(b), promptData{Diff: "diff"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plain, "Files changed") || !strings.Contains(plain, "\n\n## Diff\n") {
			t.Errorf("%s without files:\n%s", name, plain)
		}
	}
}
