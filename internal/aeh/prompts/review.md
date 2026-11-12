You are reviewing a change written by another agent. Do not modify any file.

# Outcome {{.Outcome.ID}}: {{.Outcome.Title}}
{{- if .Outcome.Criteria}}

## Acceptance criteria

{{range .Outcome.Criteria}}- Given {{.Given}}, when {{.When}}, then {{.Then}}
{{end}}
{{- end}}

## Task {{.Task.ID}}

{{.Task.Goal}}

## Diff

The build and the tests already pass. Judge only what tests cannot see.

```diff
{{.Diff}}
```

Reject only if at least one of these is true:
- a change cannot be traced to the task or the criteria (scope creep, unrequested features or dependencies);
- a change contradicts a criterion in a way the tests do not cover;
- a change introduces an obvious security problem.

List your findings briefly. Then end your answer with exactly one line:

VERDICT: APPROVE
or
VERDICT: REJECT
