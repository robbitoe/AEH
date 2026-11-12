You are implementing one task of a software outcome inside an isolated git worktree.
Work only inside the current directory. Do not commit, push or create branches: the harness does that.

# Outcome {{.Outcome.ID}}: {{.Outcome.Title}}
{{- if .Outcome.Actor}}

Actor: {{.Outcome.Actor}}
{{- end}}
{{- if .Outcome.Description}}

{{.Outcome.Description}}
{{- end}}
{{- if .Outcome.Criteria}}

## Acceptance criteria

{{range .Outcome.Criteria}}- Given {{.Given}}, when {{.When}}, then {{.Then}}
{{end}}
{{- end}}

## Your task: {{.Task.ID}}

{{.Task.Goal}}

## Rules

{{- if .Task.Paths}}
- Only modify files under: {{join .Task.Paths ", "}}
{{- end}}
- Never modify: {{join .Protected ", "}}. These hold the approved acceptance tests and the harness configuration.
- Do only what the task and the criteria require. Do not add features, endpoints, tables or dependencies that they do not ask for.
- Keep the change under {{.MaxLines}} changed lines.
- When you finish, these commands must succeed from the repository root:
{{- if .Build}}
  - build: `{{.Build}}`
{{- end}}
  - test: `{{.Test}}`
{{- if .Answers}}

## Answers from the owner

{{range .Answers}}- {{.}}
{{end}}
{{- end}}
{{- if .Feedback}}

## The previous attempt was rejected

Attempt {{.Attempt}}. The previous attempt was discarded after failing the "{{.FailedGate}}" check:

```
{{.Feedback}}
```

You are starting again from a clean copy. Fix the cause; do not work around the check.
{{- end}}
