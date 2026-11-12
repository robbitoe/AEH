# ADR 0001: Runner hardening for MVP-1

Status: Accepted (owner approval, 2026-11-12)

## Context

A QA and security review of the MVP-1 runner found that the verified candidate could differ from the integrated commit, that level 2 could be accepted without review, that a quoted `VERDICT: APPROVE` could count as approval, that timeouts fell through to the gates, and that resume reused completed tasks without checking the contract. Fixing these changes decisions that need the owner's approval: the gate order, the ladder and how agent failures are handled.

## Decision

1. **Candidate commit.** After the agent finishes, AEH freezes the worktree as one commit on the pinned base and runs every gate on a clean, detached verification worktree of that commit. The integration branch advances to that exact SHA with a compare-and-swap (`git update-ref <ref> <new> <old>`). If the agent moved `HEAD`, the attempt fails before the hash gate and is reported as `scope`. Build, test or review that change the verification checkout fail their gate.
2. **Agent failures.** A non-zero exit from the agent CLI fails the attempt without running the gates. The ladder continues as before: retry with feedback, then the next profile, then block.
3. **Timeouts and interruptions.** A timeout or interruption of the agent, a gate or the reviewer blocks the outcome immediately, without retry, and retains the affected worktree. On Windows a child process can outlive its cancelled launcher, so the operator confirms that processes stopped before answering.
4. **Level 2 review.** Level 2 requires a reviewer profile separate from the task's implementation profiles. A verdict counts only when the reviewer exits successfully and writes `VERDICT: APPROVE` or `VERDICT: REJECT` as the final nonempty line of plain stdout.
5. **Execution contract.** State records a SHA-256 of the outcome, sealed tests, prompts, build and test commands, `pass_env`, protected paths and limits, plus the verified branch head. Resume refuses when either differs. Profiles, task start profiles, ladder, reviewer, notify and timeouts are excluded, so a blocked outcome can resume after fixing a CLI flag or raising a timeout.
6. **Environment.** Agents, gate commands and the harness's own git calls inherit only OS and tool-discovery variables plus the names listed in `pass_env`. Credentials reach an agent only through `pass_env` or the profile's `env`. Harness git calls run with `core.fsmonitor=false`, because a worktree's config could point it at an agent-written program. This reduces accidental exposure; it is not isolation, since processes still run with the user's permissions.
7. **Diff limit.** Changes Git cannot count in lines (binaries) fail the scope gate.
8. **Agent instruction files.** `AGENTS.md`, `CLAUDE.md`, `CLAUDE.local.md`, `.claude/` and `.codex/` fail the scope gate in any directory, so a candidate cannot plant instructions that steer the reviewer. Path matching is case-insensitive on Windows and macOS.

## Consequences

- Accepted work is exactly the commit that passed the gates.
- Recovery after a crash is manual: remove the outcome lock and any retained worktrees and attempt branches.
- Outcomes cannot change binary files until a measurable limit for them is defined.
- A build or test command that leaves a background process holding its output (for example a Gradle or MSBuild daemon) ends with `exec.ErrWaitDelay`, so the gate counts as interrupted and the outcome blocks. Disable such daemons in `commands` until AEH handles it.
- Process isolation remains pending (sandbox without network, only the worktree mounted).
