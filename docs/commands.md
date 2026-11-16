# aeh command reference

Every command of the `aeh` CLI, in the order of the product cycle. This file is the source for end-user documentation. A test (`cmd/aeh/commands_test.go`) fails when a command of `aeh help` is missing here, or when this file documents a command that does not exist.

## Conventions

- Run every command inside the project's Git repository. Linked worktrees resolve to the main checkout for state, memory and locks.
- Exit codes: `0` done, `1` error, `2` wrong usage, `3` blocked: the command stopped with a question for the owner.
- `<slug>` and `<feature>` are lowercase letters, digits and hyphens. `<ID>` is an outcome ID such as `OUT-001` or `billing-v0_2_0`.
- State and evidence are local and never committed: `.aeh/` (state, runs, reports, memory). Configuration, outcomes, seals and prompt overrides are versioned in `aeh/`.
- Commands that change state take a project lock. A second command does not wait: it fails at once with an error naming the lock. If a command crashed, verify it stopped before removing the lock it names.
- Agent and command output shown on the terminal, saved in state or written to reports is redacted: recognized secrets become `[redacted <kind>]`.

## Setup

### `aeh init`

Creates `aeh/config.yaml` (profiles, ladder, commands, reviewers) and an example outcome, and adds `.aeh/` to `.gitignore`. Existing files are kept; prints `already initialized` when nothing was created.

### `aeh notify test`

Sends a test event to `notify.webhook` in `aeh/config.yaml`.

## Discovery: from an idea to a draft book of specs

### `aeh discover new`

`aeh discover new <slug> "<idea>"` starts a discovery session and records the idea in memory (`wiki/discovery/<slug>/idea.md`).

### `aeh discover run`

`aeh discover run <slug>` runs one round. The agent either asks questions (`DISCOVERY: QUESTIONS`) or writes the draft book (`DISCOVERY: READY`): `vision.md`, `data.md`, `features/<feature>.md` with acceptance criteria, and optional HTML mockups. Every file passes the display and secrets policy; mockups are rebuilt by aeh's own tokenizer.

- Ladder: `discovery.ladder` (default: `ladder`), two tries per profile per round, with the failure as feedback. Exit `3` when every try failed; run it again to retry.
- Technology stack: a project that starts from scratch (no committed file besides `aeh/` and housekeeping such as `.gitignore`, README and LICENSE) gets a blocking question with `topic: stack` before any book; aeh refuses a book until the owner answered it. In an existing project the agent receives the detected stack (manifests, file types, configured commands) and keeps it unless the idea or the owner's answers ask for another.
- A draft book gets an advisory review by an independent QA reviewer before it is imported. Its findings become optional questions; the review never blocks.

### `aeh discover show`

`aeh discover show <slug>` prints the session: phase, rounds, questions with their IDs and answers, the review (and whether the book changed since), and the owner's next actions.

### `aeh discover answer`

`aeh discover answer <slug> <QID> "<text>"` answers a question (`R<round>-Q<n>`). Answering again revises the answer. When every blocking question is answered, run the next round.

### `aeh discover skip`

`aeh discover skip <slug> <QID>` skips an optional question. Blocking questions, including the stack question, cannot be skipped.

### `aeh discover review`

`aeh discover review <slug>` runs the advisory review again on the draft book as memory has it now. It is for a review without a verdict (invalid, unavailable, interrupted), for example after fixing the reviewer's profile. A review with a verdict is never replaced.

### `aeh discover promote`

`aeh discover promote <slug> <feature>` creates `wiki/features/<feature>/spec.md` at 0.1.0 from a committed draft, unapproved. Its paths must not overlap an existing feature. Approve it with `aeh memory approve`.

### `aeh discover accept`

`aeh discover accept <slug> vision|data` copies the draft vision or data model into `wiki/vision.md` or `wiki/data.md`. When the document exists, it goes to `.next.md` for the owner to merge.

## Adoption: an existing system, area by area

### `aeh adopt inventory`

`aeh adopt inventory [--base <rev>]` inspects a pinned commit (default: the main checkout's HEAD) in a disposable detached worktree. Before the agent runs, aeh removes files that may hold secrets (`.env*`, `*.pem`, `*.key`, `id_*`, `*.pfx`, `credentials*`, `secrets*`, `adopt.exclude`) and every tracked link; the agent receives only their paths. A write into the checkout fails the attempt.

- Output: `wiki/system.md` in memory, with base SHA, date and profile (later inventories go to `wiki/system.next.md`), and `.aeh/adopt/config.suggested.yaml`, which aeh never executes: copy what you accept into `aeh/config.yaml`.
- Ladder: `adopt.ladder` (default: `ladder`). A profile names the output directory with `{{output_dir}}`, for example `codex exec --sandbox workspace-write --add-dir {{output_dir}} -`.

### `aeh adopt accept system`

Replaces `wiki/system.md` with the pending `wiki/system.next.md` in one memory commit. Repeating it is a no-op.

### `aeh adopt area`

`aeh adopt area <feature> --paths src/billing/,... [--title "..."] [--base <rev>]` inspects an area and records observations: behavior, in the form "Given ..., when ..., then ...", with the code locations behind it. Each observation gets its own number, `AC-<n>`, in an append-only log in memory (`wiki/adopt/<feature>/`).

- `--paths` are fixed when the area is created; they must not overlap a feature or another area. `--title` is only for a new area.
- Every location must name existing lines of a file inside the area at the pinned base.
- Running it again re-observes active observations; decided ones keep their number.

### `aeh adopt show`

`aeh adopt show <feature>` lists an area's observations with their state, where they were observed, and the token to decide on each.

### `aeh adopt confirm`

`aeh adopt confirm <feature> AC-<n> keep|bug|drop --seen <token>` decides on an observation as you saw it. `keep` confirms the current behavior; `bug` records a defect in `defects.md` and retires the number; `drop` retires it. The token binds the decision to the text you saw: if an agent observed it again since, the command fails and shows the new text. The same decision again is a no-op; a different one is refused.

### `aeh adopt verify`

`aeh adopt verify [--base <rev>]` runs only `commands.build` and `commands.test` from `aeh/config.yaml` at the base, in a disposable worktree, and checks the checkout after each. Evidence (base, command, exit code, duration, configuration digest) goes to `.aeh/adopt/verify.json`. Exit `1` when a command fails.

### `aeh adopt promote`

`aeh adopt promote <feature>` creates an unapproved 0.1.0 spec with only the `keep` observations, keeping their numbers (gaps included). Every observation must be decided first. Retired numbers are never reused.

### `aeh adopt seal`

`aeh adopt seal <feature>` writes characterization tests of an approved adopted feature: the plan of `aeh plan` in a `characterize` mode with no task. Each new test must pass on the existing code alone and together, and the previous acceptance suite must still pass. QA and security review it. The verified commit is published on `aeh/<ID>` and recorded in memory as the first implementation (`code_sha` is that commit, `observed_base_sha` the inspected one). There is nothing to implement: merge the branch. Exit `3` when blocked; answer with `aeh answer <ID>` and run it again.

### `aeh adopt status`

Lists the inventory state, each area as observed, confirmed, promoted, characterizing, characterized, published or merged, and the groups of files at the inventory's base that no feature or area covers.

## Definition

### `aeh tests approve`

`aeh tests approve <ID>` seals the acceptance tests of a hand-written outcome (`aeh/seals/<ID>.json`). Commit the tests and the seal before `aeh run`.

### `aeh plan`

`aeh plan <feature>` plans the approved spec's new criteria: a test author writes acceptance tests and one task; aeh checks that the base is green, that each new test fails before the feature exists and that the previous suite still passes; QA and security review it; the result is sealed and published on `aeh/<ID>`. Requires `commands.acceptance`. An adopted feature must be characterized first (`aeh adopt seal`), and a later version plans only after the previous implementation is merged. Exit `3` when blocked; answer with `aeh answer <ID>` and run it again.

## Execution

### `aeh run`

`aeh run <ID>` runs an outcome until it is accepted or blocked: per task, an agent attempt, then the gates (sealed tests hash, scope, drift, build, test, acceptance) and the route's reviewers, with the ladder of retries and profiles. Accepted work stays on `aeh/<ID>` for the owner to merge; memory records the implementation. A characterization is refused: merge it. Exit `0` accepted, `3` blocked, `1` error.

### `aeh do`

`aeh do "<change>" [flags]` runs a level-1 change from one sentence: gates and route-based reviews, without sealed tests.

- `--paths a/,b/` restricts the change to these paths.
- `--profile <name>` starts with this profile.
- `--route trivial|change|new` declares the route instead of model triage; rules may raise it, never lower it.
- `--feature <f> --change patch|minor|major --note "..."` names the feature the change modifies; the drift gate checks it and memory records it.

### `aeh answer`

`aeh answer <ID> "<text>"` answers a blocked outcome or plan so it can run again (`aeh run`, `aeh plan` or `aeh adopt seal`).

## Memory: local and private (`.aeh/memory/`)

### `aeh memory init`

Creates the memory repository, its own local Git repository without a remote. It is never published.

### `aeh memory new`

`aeh memory new <feature> --title "..." --paths a/**,b/**` adds a feature spec at 0.1.0. Describe it in `wiki/features/<feature>/spec.md` and commit it in the memory repository.

### `aeh memory approve`

`aeh memory approve <feature>` approves the spec as it reads now. Editing its title, paths or text voids the approval. Route `new` and `aeh plan` require it.

### `aeh memory status`

Lists features with version, approval and last implementation, uncommitted memory changes and pending records.

### `aeh memory lint`

Checks memory and its privacy settings: errors and warnings.

### `aeh memory record`

`aeh memory record <ID>` retries a pending memory record of an accepted outcome.

## Tracking

### `aeh status`

Progress of every outcome, plan and discovery session.

### `aeh version`

Prints the version.

### `aeh help`

Prints the list of commands. `-h` and `--help` do the same.
