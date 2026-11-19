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
