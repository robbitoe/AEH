package aeh

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSpecQuestionBytes = 500

// SpecCursor identifies the exact operation suspended by an agent question. Policy
// (route, roles and rules) remains in State; the candidate and approvals remain in
// the recorded Attempt. An answer leaves this cursor in place until a result is saved.
type SpecCursor struct {
	Task        string `json:"task"`
	Phase       string `json:"phase"`
	Role        string `json:"role"`
	Profile     string `json:"profile"`
	Provider    string `json:"provider"`
	LadderIndex int    `json:"ladder_index"`
	RetriesUsed int    `json:"retries_used"`
	First       string `json:"first_profile"`
	Author      string `json:"author"`
	Candidate   string `json:"candidate_sha,omitempty"`
	Attempt     int    `json:"attempt"`
	AddedReview bool   `json:"added_review,omitempty"`
	Question    string `json:"question"`
	Answer      string `json:"answer,omitempty"`
}

// agentTerminal reserves whole trimmed lines starting with SPEC-QUESTION: or
// VERDICT: anywhere in stdout. With a question present there must be exactly one
// reserved line, and it must be the last nonempty line. Thus a verdict before OR
// after a question competes, even inside a code fence or with an invalid value;
// inline prose mentioning a marker does not. Legacy verdict parsing is unchanged
// when there is no question marker.
func agentTerminal(out string) (question string, invalid bool) {
	lines := strings.Split(out, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	markers, questions, last := 0, 0, -1
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "SPEC-QUESTION:") {
			questions++
			markers++
			last = i
			// Remove only protocol whitespace; controls in the payload must be rejected.
			question = strings.Trim(strings.TrimPrefix(strings.TrimLeft(strings.TrimSuffix(raw, "\r"), " \t"), "SPEC-QUESTION:"), " ")
		} else if strings.HasPrefix(line, "VERDICT:") {
			markers++
		}
	}
	if questions == 0 {
		return "", false
	}
	if questions != 1 || markers != 1 || last != len(lines)-1 || question == "" || len(question) > maxSpecQuestionBytes || !utf8.ValidString(question) {
		return "", true
	}
	for _, r := range question {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || isArtifactBidi(r) || r == '\u2028' || r == '\u2029' {
			return "", true
		}
	}
	question = redactSecrets(question)
	if len(question) > maxSpecQuestionBytes {
		return "", true
	}
	return question, false
}

func (c SpecCursor) Summary() string {
	action := "paused at cursor"
	if c.Answer != "" {
		action = "resumed from cursor"
	}
	return fmt.Sprintf("SPEC-QUESTION: %s; %s; task %s, %s, role %s, profile %s, provider %s, ladder index %d, retries used %d",
		c.Question, action, c.Task, c.Phase, c.Role, discoveryDiagnostic(c.Profile), discoveryDiagnostic(c.Provider), c.LadderIndex, c.RetriesUsed)
}

func (ru *runner) pauseSpec(t Task, res *attemptResult, index int, first string) {
	c := res.SpecQuestion
	c.Task, c.Attempt, c.LadderIndex, c.First = t.ID, res.N, index, first
	c.RetriesUsed = index
	if index == maxAttempts-1 {
		c.RetriesUsed = 0
	}
	c.Author = res.Profile
	if c.Phase == "review" {
		c.Candidate = res.Commit
	}
	ru.state.Cursor = c
	ru.state.Status, ru.state.Question = StatusBlocked, c.Question
}
