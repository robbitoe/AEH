# AEH — Autonomous Engineering Harness

AEH turns an approved outcome into verified code on its own branch. You decide what to build and approve the acceptance tests; AEH runs agent CLIs (Codex, Claude Code, a local model) in isolated git worktrees and accepts a change only when deterministic gates pass.

The runner decides; models only propose.

> **Status: MVP-1.** Worktrees separate Git changes. Agent CLIs and build/test commands run with your user's permissions, subject to any sandbox configured on the CLI itself. Use trusted repositories, profiles, and commands until AEH has a process sandbox.

## Install

```sh
go install github.com/robbitoe/aeh/cmd/aeh@latest
```

Requires Go 1.24+, git, and the agent CLIs your profiles use.

### Agent profiles

The `aeh init` template was checked against Codex CLI 0.167 and Claude Code 2.1 on Windows:

- Every profile reads the prompt from stdin (`stdin: true`). On Windows the CLIs are `.cmd` shims, and `cmd.exe` cuts a multi-line argument at the first newline.
- Claude Code runs with `--safe-mode`, so your personal `CLAUDE.md`, plugins, hooks and MCP servers do not change the run, and with `--permission-prompts none`, so anything that would ask for permission is denied instead of waiting. `acceptEdits` lets it edit files; add `--allowedTools` entries if it should run your build or tests.
- `claude-review` is a read-only reviewer (`--tools Read,Grep,Glob`) for when Codex implements; `codex-review` is the reverse.
- `local` points Claude Code at LM Studio's Anthropic-compatible endpoint. Set the port your server uses and a model it has loaded.
- Unattended runs need sign-ins that last: `claude setup-token` for Claude Code and `codex login` for Codex.

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
3. AEH creates a candidate commit on the pinned base and checks it out in a clean verification worktree. Gates run in order and stop at the first failure. None of them calls a model:
   - **hash**: the sealed acceptance tests are unchanged;
   - **scope**: compare the candidate with its pinned base and enforce protected paths, task paths and the diff limit. Agent instruction files (`AGENTS.md`, `CLAUDE.md`, `.claude/`, `.codex/`) and Git control files (`.gitattributes`, `.gitmodules`, `.lfsconfig`) and submodule pointers may not change in any directory. Binary changes fail when Git cannot count their lines;
   - **build**: your build command (put lint here);
   - **test**: your test command.
4. Level-2 outcomes require a separate `reviewer` profile that exits successfully and writes one verdict on the final nonempty line of plain stdout. Use `VERDICT: APPROVE` or `VERDICT: REJECT`. Configure a plain-text reviewer rather than a JSON/JSONL stream.
5. If everything passes and the verification checkout remains clean, `aeh/<ID>` advances to the candidate commit. The runner checks that the branch still points to the expected base before advancing.

On failure the attempt is discarded. The ladder is: retry with the same profile and the failure as feedback, then try the next profile, then block with a question. Answer it with `aeh answer <ID> "..."` and run again.

Timeouts and interruptions block immediately and retain the affected worktree. Confirm the processes have stopped and inspect possible effects before answering. When you have finished inspecting the retained worktree, remove it with Git. Resume requires the same contract and recorded branch head. The contract covers the outcome, sealed tests, prompts, build and test commands, `pass_env`, protected paths and limits; profiles, task start profiles, ladder, reviewer, notify and timeouts may change between runs. Use a new outcome ID when intentionally changing the contract or when old state lacks those records.

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
  runs/<ID>/<task>-a<n>/  prompt, agent output, candidate SHA, diff, gate results
  reports/<ID>.md         evidence report
```

Attempt and verification worktrees are created in your user cache directory. Ordinary completed attempts are removed. Keeping them outside the repository avoids inheriting instructions from the parent project and prevents build tools from globbing the temporary worktrees.

Agent and gate processes inherit a limited set of OS and tool-discovery variables. List any other variable names they need (for example `JAVA_HOME` or a proxy) in `pass_env`. Use the profile's `env` mapping for explicit agent variables, with references to existing environment variables for credentials. Keep values out of tracked config. Home directories remain accessible with the user's permissions. Git variables such as `GIT_CONFIG_GLOBAL` are not inherited either; set `safe.directory` and similar options in your global or system Git config. Provider quota stays unknown unless a profile supplies a distinct, documented `quota_exit_codes` signal. That signal is recorded as an agent failure and follows the configured ladder.

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

## License

[MIT](LICENSE).
