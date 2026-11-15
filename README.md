# AEH — Autonomous Engineering Harness

AEH is a product-cycle engine: it takes a product from a first idea to verified, documented code, and keeps a private memory of everything it learned along the way. See the [vision](docs/vision.md) for the full cycle: discover, approve, build, remember.

![AEH map](docs/aeh-map.svg)

The runner decides; models only propose.

## What works today

The build stage. AEH turns an approved feature spec into a plan, sealed acceptance tests and verified code on its own branch. You approve the spec; AEH generates, checks, independently reviews and seals the tests. Manual outcomes remain supported with owner-approved tests. AEH runs agent CLIs (Codex, Claude Code, a local model) in separate git worktrees and accepts a change only when deterministic gates and independent reviews pass.

> Worktrees separate Git changes. Agent CLIs and build/test commands run with your user's permissions, subject to any sandbox configured on the CLI itself. Use trusted repositories, profiles, and commands until AEH has a process sandbox.

## Install

```sh
go install github.com/robbitoe/aeh/cmd/aeh@latest
```

Requires Go 1.24+, git, and the agent CLIs your profiles use.

### Agent profiles

The `aeh init` template was checked against Codex CLI 0.167.2 and Claude Code 2.1.322 on Windows:

- Every profile reads the prompt from stdin (`stdin: true`). On Windows the CLIs are `.cmd` shims, and `cmd.exe` cuts a multi-line argument at the first newline.
- Claude Code runs with `--safe-mode`, so your personal `CLAUDE.md`, plugins, hooks and MCP servers do not change the run (this also turns off any personal hooks you use as safety checks; managed policies still apply), and with `--permission-prompts none`, so anything that would ask for permission is denied instead of waiting. `acceptEdits` lets it edit files; add `--allowedTools` entries if it should run your build or tests.
- `claude-review` is a read-only reviewer (`--tools Read,Grep,Glob`) for when Codex implements; `codex-review` is the reverse.
- `local` points Claude Code at LM Studio's Anthropic-compatible endpoint. Set the port your server uses and a model it has loaded.
- Unattended runs need sign-ins that last. Codex keeps the one from `codex login`. For Claude Code, `claude setup-token` prints a long-lived token without saving it: put it in the `CLAUDE_CODE_OAUTH_TOKEN` environment variable and list `CLAUDE_CODE_OAUTH_TOKEN` in `pass_env`, since agents receive only OS and tool-discovery variables plus `pass_env`.

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

## From an approved spec to code

Initialize product memory and create a feature spec with literal ownership paths (see [Product memory](#product-memory)). The spec must have structured criteria:

```markdown
## Acceptance criteria
- AC-1: Given a cart, when the user pays, then one order is created.
- AC-2: Given a failed payment, when the user retries, then no second charge is made.
```

Use positive, unique criterion numbers without leading zeros. Criteria stay on one line; put prose and examples outside this section. Criterion numbers are never reused. Commit spec edits in the private memory repository before approving.

Configure `commands.acceptance` to run exactly the repository-relative test files listed, one per line, in the runner-written `{{manifest}}` file. The placeholder must occur exactly once. For example, a project with shell acceptance tests can use:

```yaml
commands:
  build: "your-build-command"
  test: "your-test-command"
  acceptance: "sh tests/run-manifest.sh {{manifest}}"
plan:
  ladder: [test-author]
  acceptance_dir: tests/acceptance
  max_diff_lines: 400
  max_files: 20
  max_bytes: 262144
ladder: [implementer]
```

Define those profiles and independent QA/security reviewers in the same config. `plan.ladder` defaults to the implementation ladder when omitted, so configure separate authors for generated outcomes. Every reachable implementer must differ from the accepted test author, and also from its provider under the default `provider` independence policy. With `reviewers.independence: profile`, different profiles of one provider are allowed. The run checks the whole reachable ladder before any agent starts.

```sh
aeh memory approve checkout           # owner approval of this exact spec
aeh plan checkout                     # generates, checks, reviews and seals the plan
aeh run checkout-v0_1_0                # use the ID printed by plan
```

Planning requires a clean code checkout and memory, an approved spec with criteria, a green baseline and independent reviewers. It supports new criteria that fit one task. Changed or removed implemented criteria block until a future test-revision workflow; an empty delta needs no plan. A prior implementation must already be integrated into the planning base. An occupied outcome ID also blocks, ignoring case. Resolve environment failures or answer a blocked plan with `aeh answer <ID> "..."`, then repeat `aeh plan <feature>`; changing pinned planning inputs requires a fresh plan/ID.

The test author writes tests and `plan.yaml` under `<acceptance_dir>/<ID>/`. AEH checks scope, paths and criterion-to-file traceability, requires every new test file to fail before implementation, checks the previous acceptance suite and obtains independent reviews on the frozen plan commit. These checks provide evidence; reviewers judge whether the tests fail for the right reason. The owner approves the spec, not the generated tests. `aeh tests approve <ID>` refuses any ID with plan state.

The published plan already owns branch `aeh/<ID>`. Running it starts at the pinned plan commit and reads the outcome and runner seal from Git objects. An unfinished plan, a moved branch or a different outcome/seal file shadowing it in the main checkout blocks execution. A changed or re-approved spec whose content digest differs from the planned approval blocks with `RestartRequired`: approve the spec and plan again. Candidates may never add, edit or delete files in the outcome's acceptance directory; historical tests of unchanged criteria retain their original bytes too. The run executes both the normal project suite and the new/unchanged criteria through `commands.acceptance`, including at the final check.

Resume requires the recorded integration head and execution contract, including plan provenance, plan commit and seal manifest. The report includes plan checks, the test author and every plan review's role, profile, provider, SHA and verdict. Acceptance writes `implemented.yaml` with the planned approval digest, accepted code SHA and all criterion/test mappings in the same private memory commit as the change record. A failed memory write leaves that implementation in the pending record for `aeh memory record <ID>` to retry.

Manual outcomes keep the quick-start workflow with `aeh tests approve`; both kinds coexist.

## How a task runs

1. A fresh worktree is created from `aeh/<ID>`.
2. The profile's CLI receives the prompt: outcome, criteria, task, rules, and the previous failure if there was one.
3. AEH creates a candidate commit on the pinned base and checks it out in a clean verification worktree. Gates run in order and stop at the first failure. None of them calls a model:
   - **hash**: the sealed acceptance tests are unchanged;
   - **scope**: compare the candidate with its pinned base and enforce protected paths, task paths and the diff limit. Agent instruction files (`AGENTS.md`, `CLAUDE.md`, `.claude/`, `.codex/`) and Git control files (`.gitattributes`, `.gitmodules`, `.lfsconfig`) and submodule pointers may not change in any directory. Binary changes fail when Git cannot count their lines;
   - **drift**: enforce the declared feature against product memory;
   - **build**: your build command (put lint here);
   - **test**: your test command.
4. Every route requires independent reviews, at level 1 too: QA for `trivial`; QA then security for `change` and `new`. Rules and the outcome can add DBA. Each reviewer must exit successfully and end plain stdout with exactly `VERDICT: APPROVE` or `VERDICT: REJECT`. Reviews stop at the first rejection, missing verdict or checkout modification. Every new candidate repeats all roles, and every approval records its role, profile, provider and exact SHA.
5. If everything passes and the verification checkout remains clean, `aeh/<ID>` advances to the candidate commit. The runner checks that the branch still points to the expected base before advancing.

On failure the attempt is discarded. The ladder is: retry with the same profile and the failure as feedback, then try the next profile, then block with a question. Answer it with `aeh answer <ID> "..."` and run again.

Only timeouts and cancellations kill the agent, reviewer or gate command's process tree on Windows (a Job Object) and Unix (a dedicated process group), block immediately and retain the affected worktree. Background processes left by a command that exits normally keep running. Windows cancellation waits for member process objects to become signaled, within a two-second cleanup deadline; a failed wait returns a cleanup error. External services such as file scanners can still briefly hold log files after those processes exit. This is cleanup, not a process sandbox: Unix descendants that leave the process group can escape it. Windows processes started on the agent's behalf by another service can also escape the job, for example through WMI `Win32_Process.Create`, `schtasks`, out-of-process COM or `ShellExecute` via Explorer. Confirm the processes have stopped and inspect possible effects before answering. When you have finished inspecting the retained worktree, remove it with Git. Resume requires the same contract and recorded branch head. The contract covers the outcome and its added roles, triage rules, initial route, saved triage output, implementation/triage/review prompt texts, sealed tests, build and test commands, `pass_env`, protected paths and limits. Profiles and their reviewer/triage assignments, task start profiles, ladder, independence policy, notify and timeouts may change between runs; independence is checked again on resume. Use a new outcome ID when intentionally changing the contract or when old state lacks those records.

After the last task, build and tests run once more on the integrated branch. Accepted means proven there.

## Levels

| Level | Use for | What runs |
| --- | --- | --- |
| 0 | Typos, obvious fixes | Nothing: use your agent CLI directly |
| 1 | Small change with a clear criterion | `aeh do`, or an outcome with `level: 1`: gates and route-based reviews |
| 2 | Business rules, data, money, concurrency | Sealed acceptance tests, gates and route-based reviews |

Start at the lowest level that fits.

## Triage, routes and reviewers

Before implementation, AEH chooses a route once per outcome. An explicit `route: trivial|change|new` (or `aeh do --route ...`) skips the model. Otherwise `triage.profile` runs read-only with `triage.md` and a feature index containing titles and paths, without spec bodies. It must end with exactly `ROUTE: trivial`, `ROUTE: change` or `ROUTE: new`. Failure, interruption, a modified checkout or an invalid answer selects `new`. Without a triage profile, AEH defaults to `change` and warns in the console and report.

- `trivial`: wording, formatting or a small fix that does not change feature behavior; QA reviews it.
- `change`: existing feature behavior changes; QA and security review it.
- `new`: a new capability, data/storage model, authentication/authorization or work spanning features; QA and security review it, with the prerequisites below.

Levels control sealed tests; routes control reviewers. Rules and outcome `reviewers: [dba]` add roles. QA, security and DBA always run in that order. The effective route and accumulated roles can only increase across rejected candidates, tasks and resumes; an owner answer cannot lower them. Before any agent runs, a rule applies when a task path names something inside the rule's area (`src/auth/` or `src/*/auth/*.go` for `**/auth`, `db/*.sql` for `**/*.sql`; wildcards that could name the area count) or the rule names a literal area below a task path (`src/auth` for `src/`); a task without paths, or whose path starts with a wildcard, triggers every rule. A rule that only may match somewhere below a broad task path (`**/auth` for `src/`) is deferred: the report lists it, and the actual candidate paths decide. Those are checked on every attempt before any gate, including deletions and both sides of renames; if they raise the route to `new` without its requirements, the outcome blocks without integrating. Preflight still requires reviewers for every role any rule can add. Rules are never gates and do not call a model.

```yaml
triage:
  profile: claude-review
  rules:
    - name: storage
      paths: [db/**, migrations/**]
      route: new
      reviewers: [dba]
    - name: payments
      paths: [src/payments/**]
      route: change
reviewers:
  independence: provider
  qa: [claude-review, codex-review]
  security: [claude-review, codex-review]
  dba: [claude-review, codex-review]
profiles:
  codex:
    provider: openai
    transport: cli
    command: [codex, exec, --skip-git-repo-check, --sandbox, workspace-write, "-"]
    stdin: true
  codex-review:
    provider: openai
    transport: cli
    command: [codex, exec, --sandbox, read-only, "-"]
    stdin: true
  claude-review:
    provider: anthropic
    transport: cli
    command: [claude, -p, --safe-mode, --tools, "Read,Grep,Glob", --permission-prompts, none, --output-format, text]
    stdin: true
```

Use trusted CLI options appropriate to your installation; triage and reviewer profiles must be configured read-only. AEH additionally checks the checkout after each reviewer. `provider` is the provider identity used for independence. `transport` defaults to `cli`; `api` is reserved and is currently rejected. The preferred reviewer is the first independent profile in each role's list, selected against the author of that attempt, including after escalation. By default the reviewer must come from another provider. `reviewers.independence: profile` allows another profile of the same provider; a profile can never review itself. Preflight checks every reachable author and every role that any rule could require.

For route `new`, prepare a declared feature with a spec in product memory, approve its current content with `aeh memory approve <feature>`, and use a level-2 outcome with sealed acceptance tests. Commit spec edits in the memory repository before approving. Any change to the spec title, paths or whole body voids approval. A rule that raises a candidate to `new` without these requirements blocks immediately, preserves the candidate evidence and leaves the integration head unchanged without consuming the rest of the ladder. Prepare the missing requirements and create an outcome with another ID: `aeh answer` cannot resume this block because the contract changes. For `aeh do`, move the request into `aeh/outcomes/<another-ID>.yaml` with feature, change, changelog, level 2, criteria, acceptance paths and tasks, approve the spec and tests, then use `aeh run`.

If a later candidate raises the policy, AEH runs only the missing role reviews for earlier accepted tasks before the final gates. Each review uses that task's original base-to-commit diff and a fresh detached checkout of its accepted SHA, with a reviewer independent of its original author. A rejection stops the outcome and requires another outcome ID: the runner preserves the integrated tasks because it has no transactional branch/state rewind and replay mechanism. A timeout, cancellation, missing verdict, checkout modification or review error blocks the outcome; after resolving the failure, use `aeh answer` and `aeh run` to retry only the missing reviews. For a timeout or cancellation, confirm all processes stopped and review possible effects before answering; the affected worktree is retained for inspection.

Triage prompt, output and parsed result live under `.aeh/runs/<ID>/triage/`. State records initial/effective routes, source and rules hit before contract creation, so resume never calls triage again. A crash during triage conservatively retains `new`. Reports include route evidence, every role's selection and verdict, roles not run and prompt sources. Overrides use `review-qa.md`, `review-security.md` and `review-dba.md`; a legacy `aeh/prompts/review.md` override still supplies QA and is named in the report.

### Migrating existing configs and runs

Add `provider:` to author and reviewer profiles, with reviewers available from another provider, or explicitly set `reviewers.independence: profile` and configure different reviewer profiles per role. Without a `reviewers:` section, the legacy `reviewer:` key still covers every required role when independent. Adding a `reviewers:` section replaces that legacy fallback, so list all required roles. Existing level-1 runs now require reviews. States written before stage 2 cannot resume; relaunch the work with another outcome ID.

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
  pending/<ID>.json       accepted changes still to be written to memory
  memory/                 product memory: its own local Git repository, no remote
```

Attempt and verification worktrees are created in your user cache directory. Ordinary completed attempts are removed. Keeping them outside the repository avoids inheriting instructions from the parent project and prevents build tools from globbing the temporary worktrees.

Agent and gate processes inherit a limited set of OS and tool-discovery variables. List any other variable names they need (for example `JAVA_HOME` or a proxy) in `pass_env`. Use the profile's `env` mapping for explicit agent variables, with references to existing environment variables for credentials. Keep values out of tracked config. Home directories remain accessible with the user's permissions. Git variables such as `GIT_CONFIG_GLOBAL` are not inherited either; set `safe.directory` and similar options in your global or system Git config. Provider quota stays unknown unless a profile supplies a distinct, documented `quota_exit_codes` signal. That signal is recorded as an agent failure and follows the configured ladder.

Make sure your project's `.gitignore` covers build output (`bin/`, `obj/`, `node_modules/`). Otherwise the scope gate sees it as part of the change.

## Commands

| Command | What it does |
| --- | --- |
| `aeh init` | Creates `aeh/config.yaml`, an example outcome, and ignores `.aeh/`. |
| `aeh tests approve <ID>` | Seals a manual outcome's acceptance tests; refuses generated outcomes. |
| `aeh plan <feature>` | Plans an approved spec, reviews and seals its tests, and publishes `aeh/<ID>`; resumes an unfinished plan. |
| `aeh run <ID>` | Runs the outcome. Exit code 0 accepted, 3 blocked, 1 error. |
| `aeh do "<change>" [--paths a/,b/] [--profile name] [--route trivial\|change\|new] [--feature slug --change patch\|minor\|major --note "..."]` | Level-1 change with one task; optionally links and records a feature. |
| `aeh answer <ID> "<text>"` | Answers a blocked outcome or plan; the answer goes into the next prompt. |
| `aeh status` | Status, tasks and attempts of every outcome. |
| `aeh notify test` | Sends a test event to the webhook. |
| `aeh memory init` | Creates the product memory in `.aeh/memory/` of the main checkout. |
| `aeh memory new <feature> --title "..." --paths a/**` | Adds a feature spec at version 0.1.0. |
| `aeh memory approve <feature>` | Approves the exact title, paths and full body of a spec. |
| `aeh memory status` | Features, versions, last accepted change and pending records. |
| `aeh memory lint` | Checks the memory and its privacy settings. |
| `aeh memory record <ID>` | Retries a memory record that failed after an outcome was accepted. |

### Product memory

The memory is a local wiki in `.aeh/memory/` of the project's main checkout, also when you run from a linked worktree. It is its own Git repository with no remote, and the project ignores `.aeh/`, so aeh does not publish it. Each feature has a spec with a version and the paths it owns; `index.md` and `log.md` are maintained by aeh. Only aeh commands write it, one at a time per project. You can edit spec text by hand and commit it inside the memory repository; aeh refuses to record on top of uncommitted changes.

Link an outcome using `feature: checkout`, `change: patch` (or `minor` / `major`), and a one-line `changelog: ...`. For `aeh do`, use `--feature checkout --change patch --note "..."`. The implementation prompt includes only that feature's spec, as reference data sent to the configured agent provider. Prompt overrides can include it with `{{.MemorySpec}}`; the value is empty without a declared feature.

The deterministic drift gate runs after scope and before build on each attempt, and again on the accumulated diff in the final check. Changes to a feature's paths must declare that feature; touching multiple features, including overlapping paths or a rename across features, fails with a request to split the outcome. Untouched declared features and files without an owning feature produce warnings in the evidence and report. With no memory and no declared feature, drift is skipped. Candidates may never modify `.aeh/`.

The execution contract includes which paths every feature owns and the declared feature's title, version and text, so a change to any of those rejects resume. Other features' versions are not part of it: a blocked outcome can resume after another outcome records memory. A changed memory fingerprint before acceptance blocks the outcome. After acceptance, aeh bumps the feature's version and records the verified code commit in its changelog. If recording fails (for example, memory has uncommitted edits), the outcome stays accepted and a pending record remains. Commit or discard those edits, then retry with `aeh memory record <ID>`. Outcomes without a feature do not write memory.

## Roadmap

The engine above is the build stage of the [vision](docs/vision.md). Next, in order:

1. **Memory.** A private wiki in `.aeh/memory/` with versioned specs, kept in sync with the code by a deterministic drift gate.
2. **Triage and reviewers (implemented).** Trivial, change and new routes, with QA, security and DBA reviewers chosen by route and rules.
3. **Specs to code (implemented).** An approved feature produces one task and independently reviewed, runner-sealed acceptance tests with `aeh plan`.
4. **Discovery.** From a vague idea to a spec book with HTML mockups.
5. **Live review.** Specs keep evolving while approved features are built.
6. **Sandbox.** Agents in a container with no network by default.

Discovery and live spec/test revision remain future work. Manual outcomes, tasks and acceptance tests can also be written by hand or with your agent CLI.

## Development

Go standard library only, plus `gopkg.in/yaml.v3`. One file per responsibility in `internal/aeh`. Before any change:

```sh
go vet ./...
go test -timeout 30m ./...
```

The full suite takes about 12 minutes on Windows; the extended timeout is required.

Agent instruction files (`AGENTS.md`, `CLAUDE.md`, `.claude/`, `.codex/`), agent coordination notes (`.agents/`) and local state (`.aeh/`) are git-ignored: they stay on your machine. Design decisions are recorded in [docs/adr](docs/adr).

## License

[MIT](LICENSE).
