package aeh

import (
	"bytes"
	"embed"
	"errors"
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

// loadPrompt uses the project override when present, otherwise the embedded default.
func loadPrompt(r Repo, name string) (string, error) {
	text, err := os.ReadFile(r.PromptOverride(name))
	if errors.Is(err, os.ErrNotExist) {
		text, err = defaultPrompts.ReadFile("prompts/" + name)
	}
	return string(text), err
}

func renderPrompt(name, text string, data promptData) (string, error) {
	tmpl, err := template.New(name).Funcs(template.FuncMap{"join": strings.Join}).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
