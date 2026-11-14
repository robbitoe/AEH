You are classifying a requested change before any code is written. Do not modify any file.

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

## Tasks

{{range .Outcome.Tasks}}- {{.ID}}: {{.Goal}}{{if .Paths}} (paths: {{join .Paths ", "}}){{end}}
{{end}}
{{- if .Features}}
{{.Features}}
{{- end}}

## Routes

- trivial: wording, comments, formatting, a configuration value or a small fix that does not
  change what a feature does. Nothing about data, storage, authentication, permissions, money
  or a public interface.
- change: changes the behavior of an existing feature.
- new: adds a feature or capability, changes the data model or storage, authentication or
  authorization, or spans several features.

When in doubt, choose the higher route. Explain your reasoning in a few lines. Then end your
answer with exactly one line:

ROUTE: trivial
or
ROUTE: change
or
ROUTE: new
