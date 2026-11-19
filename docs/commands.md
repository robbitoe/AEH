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

`aeh discover review <slug>` runs the advisory review again on the draft book as memory has it now. It is for a review without a verdict (invalid, unavailable, interrupted), for example after fixing the reviewer's profile (a Codex reviewer needs `--skip-git-repo-check`, since the review runs outside a Git repository). A review with a verdict is never replaced.

### `aeh discover promote`

`aeh discover promote <slug> <feature>` creates `wiki/features/<feature>/spec.md` at 0.1.0 from a committed draft, unapproved. Its paths must not overlap an existing feature. Approve it with `aeh memory approve`.

For an existing feature, a draft of the book is a revision. When the book is imported, the revision is validated against the feature's history (kept criteria keep their numbers, removed numbers are never reused, new criteria go above the highest number ever used; new paths must not belong to another feature or to an adopted area, and an adopted feature keeps its paths) and written to `wiki/features/<feature>/spec.next.md`, with the session, the draft digest, the memory commit it revises and the criteria diff. From then on `spec.next.md` is the revision's only source: edit and commit it in memory to change it. A feature has one pending revision at a time; another session's revision of it is refused. `aeh discover promote` then writes it as the next, unapproved version of `spec.md` (one minor version above every version the spec had or the owner approved), keeps the changelog, history and implementations, adds a changelog line with `from_revision`, and removes `spec.next.md` in the same memory commit. It is refused if `spec.md` changed after the revision was imported (discard it and revise again). Approve the new version with `aeh memory approve` (and `--clarify` when a change only rewords a criterion).

When a version changes the feature's paths, the feature owns the union of the old and new paths until that version is implemented, so its plan may move or retire the old code and the drift gate counts both. Moving code needs new or changed criteria; a version that only changes paths is planned as a plan without implementation that reassigns ownership. Once it is recorded, the old paths are released and another feature may claim them.

Promotion is refused if its feature or paths are reserved by a queued approval.

### `aeh discover discard`

`aeh discover discard <slug> <feature>` removes the feature's pending revision from this session (`spec.next.md`) with a memory commit. Use it to unblock a stale revision or one you do not want.

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

Promotion is refused if its feature or paths are reserved by a queued approval.

### `aeh adopt seal`

`aeh adopt seal <feature>` writes characterization tests of an approved adopted feature: the plan of `aeh plan` in a `characterize` mode with no task. Each new test must pass on the existing code alone and together, and the previous acceptance suite must still pass. QA and security review it. The verified commit is published on `aeh/<ID>` and recorded in memory as the first implementation (`code_sha` is that commit, `observed_base_sha` the inspected one). There is nothing to implement: merge the branch. Exit `3` when blocked; answer with `aeh answer <ID>` and run it again.

New characterization work is refused while a queued approval reserves the feature or its paths. A registered blocker may resume; abandoned plans cannot resume.

### `aeh adopt status`

Lists the inventory state, each area as observed, confirmed, promoted, characterizing, characterized, published or merged, and the groups of files at the inventory's base that no feature or area covers.

## Definition

### `aeh tests approve`

`aeh tests approve <ID>` seals the acceptance tests of a hand-written outcome (`aeh/seals/<ID>.json`). Commit the tests and the seal before `aeh run`.

### `aeh plan`

`aeh plan <feature>` plans the approved spec's new, changed and removed criteria: a test author writes replacement or new acceptance tests and one task; aeh checks that the base is green, that new and changed tests fail on the base, that re-coverage tests pass and that the previous suite minus retired files stays green; QA and security review it; the result is sealed and published on `aeh/<ID>`. The runner deletes sealed files covering changed or removed criteria and seals their absence. Unchanged criteria sharing those files receive green re-coverage in separate files from red tests. Retired paths cannot be recreated. Acceptance records effective coverage and retired files with their reasons. A delta with nothing to implement (only removed criteria, clarifications or re-coverage) is a plan without a task: its outcome has kind `revise`, the runner retires the tests (with no test author when there is nothing to write), checks and reviews the commit, publishes it and records it in memory right away; `aeh run` refuses it, and you merge `aeh/<ID>`. To remove every criterion of an implemented feature, delete its whole `## Acceptance criteria` section: the plan retires all their tests, and the numbers stay retired. Requires `commands.acceptance`. An adopted feature must be characterized first (`aeh adopt seal`), and a later version plans only after the previous implementation is merged. Exit `3` when blocked; answer with `aeh answer <ID>` and run it again.

A plan pins memory as it was when the plan was reserved. It, and the `aeh run` of its outcome, read the spec, criteria, last implementation and every feature's paths from that point, so edits and newer approvals never reach them; the implementation is recorded against the planned version.

A newer approval with pending work is queued. New plans affecting the queued feature or reserved paths are refused; a registered blocker continues from its pinned version. Abandoned plans are terminal.

## Execution

### `aeh run`

`aeh run <ID>` runs an outcome until it is accepted or blocked: per task, an agent attempt, then the gates (sealed tests hash, scope, drift, build, test, acceptance) and the route's reviewers, with the ladder of retries and profiles. Accepted work stays on `aeh/<ID>` for the owner to merge; memory records the implementation. A characterization is refused: merge it. Exit `0` accepted, `3` blocked, `1` error.

A new outcome linked to a queued feature or touching its reserved paths is refused under the project lock and names the queue. Registered blockers may resume against the old version. Abandoned outcomes cannot resume.

### `aeh do`

`aeh do "<change>" [flags]` runs a level-1 change from one sentence: gates and route-based reviews, without sealed tests.

- `--paths a/,b/` restricts the change to these paths.
- `--profile <name>` starts with this profile.
- `--route trivial|change|new` declares the route instead of model triage; rules may raise it, never lower it.
- `--feature <f> --change patch|minor|major --note "..."` names the feature the change modifies; the drift gate checks it and memory records it.

A queued approval refuses new changes linked through `--feature` or touching its reserved paths, before agents run.

### `aeh answer`

`aeh answer <ID> "<text>"` answers a blocked outcome or plan so it can run again (`aeh run`, `aeh plan` or `aeh adopt seal`).

For an agent's terminal `SPEC-QUESTION`, the answer preserves the saved cursor and does not consume a ladder step. Then run `aeh run <ID>`: an implementer uses the same profile in a new attempt/worktree; a reviewer uses the same profile and candidate SHA, preserving the other reviews of that SHA. This also resumes a question from `aeh do` by its printed `DO-...` ID. Implementation and review prompts receive the owner's answers. Empty answers and answers to work that is no longer blocked are refused without changing state; recognized secrets in answers are redacted. State is saved atomically before returning. Status and evidence show the question, asking role/profile/provider and attempts resumed from the cursor. A queued spec approval never replaces an answer: answer and finish, or `aeh abandon <ID>`. Plans keep their existing ladder/answer behavior.

Abandoned work refuses answers.

### `aeh abandon`

`aeh abandon <ID>` abandons a registered unfinished plan or run. It persists intent first, marks both states terminal `abandoned`, records an idempotent memory log entry, and reconciles queued approvals. Its branch `aeh/<ID>` and all run/report evidence remain. Accepted work is kept: the command completes any pending acceptance record, then refuses to abandon. No abandoned attempt counts as an implementation. Interrupted intents recover at the next command that reads the state, under the project lock followed by the outcome lock.

## Memory: local and private (`.aeh/memory/`)

### `aeh memory init`

Creates the memory repository, its own local Git repository without a remote. It is never published.

### `aeh memory new`

`aeh memory new <feature> --title "..." --paths a/**,b/**` adds a feature spec at 0.1.0. Describe it in `wiki/features/<feature>/spec.md` and commit it in the memory repository.

### `aeh memory approve`

`aeh memory approve <feature> [--clarify AC-<n>,...|none]` approves the spec as it reads now. Editing its title, paths or text voids the approval. Route `new` and `aeh plan` require it.

An approved version never changes: to approve new content, raise `version:` in the spec's front matter above the highest version you approved (the error names the lowest valid one). Each approval records which implementation its criteria delta was computed against. If another implementation was recorded since (for example the previous version's run finished after you approved this one), `aeh plan` asks you to approve again; approving again keeps the version and content and updates only that base. A spec whose version is above its approval (you raised only the version, or recording an implementation raised it) is planned only after you approve that version.

`--clarify AC-<n>,...|none`: A clarification is a changed criterion whose new text only rewords the old one. Mark it when approving with `--clarify AC-<n>,...`: its replacement tests must pass on the base, live in files apart from tests that must fail, and the QA reviewer must answer `EQUIVALENT AC-<n>: yes` for each one before the verdict (a missing, repeated or malformed line invalidates the review; `no` rejects it). The marks are bound to the exact old and new texts; `--clarify none` removes them. Without the flag, an approval that is still current keeps its marks, and a new approval has none.

With pending work, approval is recorded but queued: the output names every blocker and the approval in force remains the previous one. Implementation plans block until a run is accepted and recorded; characterizations and revisions finish when logged. Stopped runs and pending memory records also block. Path changes add pending work of other features owning or touching old or new paths. The target is preserved in memory's `wiki/features/<feature>/queue.json`; `spec.md` and `approval.yaml` continue to describe the version in force. Do not edit the version in force while queued. A queued version is an approved version for the immutable version rule.

While queued, a second approval of the feature is refused. New work affecting the feature or reserved paths, and creation, approval or promotion of another feature claiming those paths, is refused and names the queue. Finish registered blockers with `plan`/`run`, or use `aeh abandon <ID>`. After their acceptance records or abandonment, AEH recomputes blockers under the project lock and activates only when the set is empty. Acceptance records made while queued are preserved. Activation recovers after interruption when a command next reads the queue. An implementation completed meanwhile may make the approved delta stale; reapprove it before planning.

If an old acceptance record reaches or passes the queued version, activation waits. Only in this collision case, copy the same target spec from `queue.json` into `spec.md`, raise only `version:` above both versions, commit in memory and approve again to replace the target. Different target content remains forbidden. See the README's queued-approval recovery instructions for restoring owner edits and handling interrupted journals.

### `aeh memory status`

Lists each feature's version in force, approval, last implementation, pending revision (`spec.next.md` and its discovery session), pending work and queued approvals with their blockers, plus uncommitted memory changes and pending records. Recovers interrupted activation and abandonment first.

Blocked activations show `activation waiting: <reason>` and emit warnings without failing unrelated commands or accepted work. Status prints queue and transaction recovery errors alongside useful state. It never overwrites an owner's edited version in force.

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
