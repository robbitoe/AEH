# AEH — Autonomous Engineering Harness

AEH turns an approved outcome into verified code on its own branch. You decide what to build and approve the acceptance tests; AEH runs agent CLIs (Codex, Claude Code, a local model) in isolated git worktrees and accepts a change only when deterministic gates pass.

The runner decides; models only propose.

> **Status: MVP-1.** Agents run with your user's permissions. There is no container sandbox yet, so run AEH only on repositories and machines you trust, and keep secrets out of the project directory.

## Install

```sh
go install github.com/robbitoe/aeh/cmd/aeh@latest
```

Requires Go 1.24+, git, and the agent CLIs your profiles use.

## Quick start

```sh
cd your-project
aeh init                               # creates aeh/config.yaml and an example outcome
# 1. edit aeh/config.yaml: build and test commands, agent profiles
# 2. write aeh/outcomes/OUT-001.yaml and its acceptance tests
aeh tests approve OUT-001              # seal the tests you approved
git add -A && git commit -m "OUT-001 contract and acceptance tests"
aeh run OUT-001                        # runs until accepted or blocked
aeh status
```

When an outcome is accepted, the work is on branch `aeh/OUT-001`. Review it and merge it yourself. AEH never touches your other branches.

For small changes that don't need sealed tests:

```sh
aeh do "fix the null check in FeeService.Calculate" --paths src/Fees/
```

## How a task runs

1. A fresh worktree is created from `aeh/<ID>`.
2. The profile's CLI receives the prompt: outcome, criteria, task, rules, and the previous failure if there was one.
3. Gates run in order and stop at the first failure. None of them calls a model:
   - **hash**: the sealed acceptance tests are unchanged;
   - **scope**: no protected path touched, only the task's paths, within the diff limit;
   - **build**: your build command (put lint here);
   - **test**: your test command.
4. Level-2 outcomes are then reviewed by the `reviewer` profile, which must answer `VERDICT: APPROVE`.
5. If everything passes, the change is committed and `aeh/<ID>` advances.

On failure the attempt is discarded. The ladder is: retry with the same profile and the failure as feedback, then try the next profile, then block with a question. Answer it with `aeh answer <ID> "..."` and run again.

After the last task, build and tests run once more on the integrated branch. Accepted means proven there.

## Levels

| Level | Use for | What runs |
| --- | --- | --- |
| 0 | Typos, obvious fixes | Nothing: use your agent CLI directly |
| 1 | Small change with a clear criterion | `aeh do`, or an outcome with `level: 1`: gates only |
| 2 | Business rules, data, money, concurrency | Sealed acceptance tests, gates and cross review |

Start at the lowest level that fits.

## Files

```
aeh/                      committed
  config.yaml             commands, limits, ladder, profiles, webhook
  outcomes/OUT-001.yaml   the contract
  seals/OUT-001.json      the acceptance tests you approved, by hash
  prompts/*.md            optional overrides of the built-in prompts
.aeh/                     local, git-ignored
  state/                  progress per outcome
  runs/<ID>/<task>-a<n>/  prompt, agent output, diff, gate results
  reports/<ID>.md         evidence report
```

Attempt worktrees are created in your user cache directory, outside the repository, and removed after each attempt. Keeping them outside stops agent CLIs from loading instruction files from the parent project and stops build tools from globbing them.

Make sure your project's `.gitignore` covers build output (`bin/`, `obj/`, `node_modules/`). Otherwise the scope gate sees it as part of the change.

## Commands

| Command | What it does |
| --- | --- |
| `aeh init` | Creates `aeh/config.yaml`, an example outcome, and ignores `.aeh/`. |
| `aeh tests approve <ID>` | Seals the outcome's acceptance tests. |
| `aeh run <ID>` | Runs the outcome. Exit code 0 accepted, 3 blocked, 1 error. |
| `aeh do "<change>" [--paths a/,b/] [--profile name]` | Level-1 change with one task. |
| `aeh answer <ID> "<text>"` | Answers a blocked outcome; the answer goes into the next prompt. |
| `aeh status` | Status, tasks and attempts of every outcome. |
| `aeh notify test` | Sends a test event to the webhook. |

## Not in this version

- Container sandbox for agents.
- `discover`, `ingest`, `scope`, `outcomes propose` and `clarify`: outcomes and tests are written by hand or with your agent CLI.
- Automatic planning: tasks are listed in the outcome file.
- Parallel tasks, metrics and a dashboard.

## Development

Go standard library only, plus `gopkg.in/yaml.v3`. One file per responsibility in `internal/aeh`. Before any change:

```sh
go vet ./...
go test ./...
```

Agent instruction files (`AGENTS.md`, `CLAUDE.md`, `.claude/`, `.codex/`) are git-ignored: keep your own locally.
