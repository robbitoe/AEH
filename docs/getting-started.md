# Getting started with AEH

This guide takes you from installing AEH to your first verified feature. It assumes you know Git and have used at least one agent CLI (Codex or Claude Code). For every flag and rule, see the [command reference](commands.md); for the why, see the [vision](vision.md).

## 1. What you need

- Git, and a project in a Git repository with at least one commit.
- At least two agent CLIs from **different providers**, signed in. AEH never lets an agent review its own work, and by default a reviewer must come from another provider than the author. The `aeh init` template uses Codex CLI and Claude Code.
- Your project's build and test commands, runnable from a shell. AEH does not care about the stack: `dotnet test`, `npm test` and `go test ./...` are all fine.
- On Windows, Git Bash (it ships with Git for Windows) if your commands use `sh`.

## 2. Install

Download the archive for your system from the [releases page](https://github.com/robbitoe/aeh/releases), unpack it and put `aeh` (or `aeh.exe`) on your `PATH`. Each release lists SHA-256 checksums in `checksums.txt`.

With Go 1.24 or later you can build it instead:

```sh
go install github.com/robbitoe/aeh/cmd/aeh@latest
```

Check it:

```sh
aeh version
```

## 3. Set up a project

```sh
cd your-project
aeh init            # aeh/config.yaml, an example outcome, and .aeh/ in .gitignore
aeh memory init     # the private product memory in .aeh/memory/
```

Two directories matter from now on:

| Directory | What it holds | Committed? |
| --- | --- | --- |
| `aeh/` | Configuration, outcomes, sealed test hashes, prompt overrides | Yes, with your code |
| `.aeh/` | State, run logs, reports and the product memory | Never. It stays on your machine |

The memory in `.aeh/memory/` is its own Git repository with no remote. AEH never publishes it. Do not run `git clean -x` in the project: it would delete it.

### Edit `aeh/config.yaml`

Set at least these:

```yaml
project: my-site

commands:
  build: "npm run build"
  test: "npm test"
  # Runs only the test files listed, one per line, in the file AEH writes to {{manifest}}.
  acceptance: 'node scripts/acceptance.js "{{manifest}}"'
  timeout_minutes: 20
```

`commands.acceptance` is needed to plan features (step 5). It must run exactly the files in the manifest and fail if any of them fails. A Node project can use a script like this one:

```js
// scripts/acceptance.js
const { readFileSync } = require("node:fs");
const { spawnSync } = require("node:child_process");
const files = readFileSync(process.argv[2], "utf8").split("\n").filter(Boolean);
const r = spawnSync(process.execPath, ["--test", ...files], { stdio: "inherit" });
process.exit(r.status ?? 1);
```

The template already defines four agent profiles: `medio` (Codex) and `fuerte` (Claude Code) implement; `codex-review` and `claude-review` review. Keep them unless you know you need something else. The [README](../README.md#agent-profiles) explains each flag. The ones that matter most:

- Every profile reads its prompt from stdin (`stdin: true`).
- Reviewers and implementers need plain text output (`--output-format text` for Claude Code). AEH reads verdicts and questions from the last line.
- Codex profiles need `--skip-git-repo-check`, because discovery runs in a directory that is not a Git repository. AEH refuses to start discovery if a Codex profile lacks it.
- Agents only receive OS and tool-discovery variables. List anything else they need in `pass_env`, for example `CLAUDE_CODE_OAUTH_TOKEN` for long unattended runs.

Commit the configuration and the acceptance runner. Plans and runs work in worktrees made from commits, so anything they need must be committed:

```sh
git add aeh .gitignore scripts/acceptance.js
git commit -m "Set up AEH"
```

## 4. From an idea to specs (discovery)

Start with an idea, even a vague one. AEH runs rounds in which an agent asks you questions, until it can write a book of specs: a vision, a data model, one spec per feature with acceptance criteria, and optional HTML mockups.

```sh
aeh discover new site "A personal website that presents my work and how to contact me"
aeh discover run site          # the agent asks questions or writes the book
aeh discover show site         # read the questions: R1-Q1, R1-Q2, ...
aeh discover answer site R1-Q1 "Static site, plain HTML and CSS, deployed to GitHub Pages"
aeh discover run site          # next round
```

In a new project the first question is always the technology stack: you choose it, not the agent. When the book is ready, `aeh discover show site` lists it. The drafts live in memory under `wiki/discovery/site/drafts/`. Read them, edit them if you want (commit inside `.aeh/memory/`), then turn each draft into a feature:

```sh
aeh discover promote site home        # wiki/features/home/spec.md, version 0.1.0
aeh discover accept site vision       # optional: the vision and data documents
```

If you already know what you want, skip discovery and write the spec yourself:

```sh
aeh memory new home --title "Home page" --paths "site/**,tests/**"
# edit .aeh/memory/wiki/features/home/spec.md and commit it inside .aeh/memory/
```

## 5. Approve and build a feature

Approval is your decision. AEH plans only what you approved, exactly as you approved it.

```sh
aeh memory approve home     # approve this exact version of the spec
aeh plan home               # writes, checks, reviews and seals the acceptance tests
```

`aeh plan` asks a test author to write acceptance tests for the new criteria. It then checks that they fail on the current code and that the rest of the suite stays green, gets them reviewed by QA and security, and seals them. It prints an outcome ID such as `home-v0_1_0`. Then:

```sh
aeh run home-v0_1_0
```

The run creates a fresh worktree per attempt and asks an implementer to make the change. It then runs the gates in a fixed order: sealed-test hashes, scope, drift, build, test and, for planned features, the sealed acceptance tests. Reviewers from another provider review the change. A failed attempt is retried with the failure as feedback, then escalated to the next profile. When the ladder is exhausted, the run pauses with a question.

When it finishes, the work is on the branch `aeh/home-v0_1_0`. AEH never touches your other branches. Review it and merge it yourself:

```sh
git merge aeh/home-v0_1_0
```

AEH records the implementation in memory: the spec's version, the code commit, and which test covers which criterion.

## 6. When AEH stops

A blocked run or plan exits with code 3 and a question. Read it with `aeh status`, then answer:

```sh
aeh status
aeh answer home-v0_1_0 "Use the existing nav component; do not add a new one"
aeh run home-v0_1_0         # or aeh plan home, for a blocked plan
```

An implementer can also stop on its own with a `SPEC-QUESTION` when the spec contradicts itself or the code. The same `aeh answer` resumes it.

To drop unfinished work: `aeh abandon <ID>`. The branch and the evidence are kept.

## 7. Small changes

Not everything needs a spec. For a small fix:

```sh
aeh do "fix the broken link in the footer" --paths site/ --feature home --change patch --note "Fix the footer link"
```

It runs the same gates and the reviewers its route requires, without sealed tests. A change to paths a feature owns must name that feature (the drift gate checks it), and it is recorded in the feature's history.

## 8. Changing a feature later

Specs keep evolving. Edit the spec in memory, raise its `version:`, commit it and approve again:

```sh
aeh memory approve home              # the new version
aeh plan home                        # plans only what changed: new, changed and removed criteria
```

Changed criteria get new tests and their old sealed tests are retired. Removed criteria only retire tests. A pure rewording can be approved with `--clarify AC-<n>`, and nothing is reimplemented. `aeh memory status` shows what is in force, what is queued and what blocks it.

## 9. An existing project without documentation

`aeh adopt` documents a system that already exists. It takes an inventory, then inspects one area at a time and records what the code does today as observed criteria. You confirm each one as `keep`, `bug` or `drop`. AEH then seals characterization tests that pass on the current code. From then on the area is a feature like any other. See [Adoption](commands.md#adoption-an-existing-system-area-by-area).

## 10. Where to look

| Question | Command or file |
| --- | --- |
| What is running or blocked? | `aeh status` |
| Which features exist, and in which version? | `aeh memory status` |
| What happened in a run? | `.aeh/reports/<ID>.md` |
| Is memory healthy? | `aeh memory lint` |
| Every command and flag | `aeh help`, [commands.md](commands.md) |

## Safety notes

- Agents and build/test commands run with your user's permissions, limited only by each CLI's own sandbox. Use AEH on repositories and commands you trust.
- AEH does not manage credentials: it uses the sessions your CLIs already have. Prompts and agent output are kept under `.aeh/runs/` on your machine; anything sensitive that reaches them stays there.
- Your idea, your answers and the relevant spec go to the agent providers inside prompts.
