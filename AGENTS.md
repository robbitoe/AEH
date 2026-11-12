# Rules for agents working on AEH

AEH is a small, deterministic runner. Keep it that way.

## Code

- Go standard library only. The single allowed dependency is `gopkg.in/yaml.v3`.
- One file per responsibility inside `internal/aeh`. No new packages without a reason written in the PR.
- No interfaces with a single implementation, no dependency injection frameworks, no generics for their own sake.
- Plain functions and structs. Errors are wrapped with context: `fmt.Errorf("seal %s: %w", id, err)`.
- No global state except embedded prompt templates.
- The whole MVP should stay under ~3,000 lines of Go. If a change pushes past that, stop and explain why.

## Behaviour that must not change without discussion

- The runner decides; models only propose. Acceptance is decided by gates, never by an agent's own report.
- Gates run in order: hash, scope, build, test. They never call a model.
- Acceptance test paths and `aeh/` are always protected. No agent run may modify them.
- Every attempt starts from a fresh worktree; failed attempts are discarded.
- The ladder is: retry with the same profile, then escalate one profile, then block with a question.
- The target project's stack is unknown to AEH. Build and test are shell commands from config.

## Checks before finishing

```
go vet ./...
go test ./...
```

Both must pass. Tests that need `sh` are skipped on Windows.
