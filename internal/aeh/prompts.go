package aeh

import (
	"bytes"
	"embed"
	"os"
	"strings"
	"text/template"
)

//go:embed prompts/*.md
var defaultPrompts embed.FS

// promptData is everything a prompt template can use.
type promptData struct {
	Outcome    Outcome
	Task       Task
	Protected  []string
	MaxLines   int
	Build      string
	Test       string
	Answers    []string
	Attempt    int
	FailedGate string
	Feedback   string
	Diff       string
}

// renderPrompt uses aeh/prompts/<name> when the project overrides it, otherwise the embedded default.
func renderPrompt(r Repo, name string, data promptData) (string, error) {
	text, err := os.ReadFile(r.PromptOverride(name))
	if err != nil {
		text, err = defaultPrompts.ReadFile("prompts/" + name)
		if err != nil {
			return "", err
		}
	}
	tmpl, err := template.New(name).Funcs(template.FuncMap{"join": strings.Join}).Parse(string(text))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
