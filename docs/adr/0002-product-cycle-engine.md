# ADR 0002: Product-cycle engine with private memory

Status: Accepted (owner decision, 2026-11-13)

## Context

MVP-1 built only the verification loop: an approved outcome with hand-written acceptance tests becomes verified code on its own branch. To prove that loop first, everything else (discovery, planning, specs) was marked as optional future work. That scope did not match the owner's intent. The goal is to build complete products in full development cycles, starting from a vague idea, with an engine that remembers what it already did and does not over-engineer simple changes.

## Decision

1. **AEH is a product-cycle engine.** It has four stages: discover (idea → spec book), approve (the owner, per feature), build (triage → tasks → implementation loop) and remember (a living wiki). The MVP-1 engine is the build stage and keeps its guarantees (ADR 0001).
2. **Vocabulary.** A product has features. A feature is a complete capability a user can use and is the unit of approval. Each feature has one versioned spec. A spec produces tasks; a task produces attempts.
3. **Models propose, the runner decides.** Models are used to discover, write specs, triage, implement and review. No gate calls a model.
4. **Triage.** Every request is classified as trivial, change or new. A model proposes the route; deterministic rules can raise it but never lower it. Touching migrations requires a DBA reviewer; touching auth requires a Security reviewer.
5. **Reviewers.** QA and Security review every code change, and a DBA reviews data model changes. Each reviewer is a different agent from the author.
6. **Private memory.** The product memory follows the LLM Wiki pattern (raw sources, a maintained wiki, a schema, `index.md` and `log.md`) and lives in `.aeh/memory/` in the project's main checkout.
   - `.aeh/` is git-ignored, so the memory is never published.
   - It is its own local Git repository with no remote. Each spec version is a commit that records the code commit that implemented it.
   - Agents read it. Only the runner writes it, after a change passes the gates.
   - Cleanup after a merge removes branches and worktrees, never the memory.
7. **Drift gate.** When a task changes the behavior of a feature, its spec must bump its version and add a changelog line. The check is deterministic.
8. **Scope limits.** The fixed line limit and the rule that discovery and planning are optional are dropped. Each roadmap stage ships as pull requests that can be reviewed in one pass.

## Consequences

- The roadmap becomes: memory, triage and reviewers, specs to code, discovery, live review, sandbox. Each stage must be usable on its own and is measured against using an agent CLI directly.
- A clone of the repository does not include the memory. Sharing or backing it up needs a private remote, which is the owner's choice.
- The memory can disagree with the code if the runner is bypassed. The drift gate and a periodic lint pass detect this; they cannot prevent manual edits outside AEH.
- The owner's role moves up a level: from approving acceptance tests to approving features. Acceptance tests are derived from approved specs and locked by the engine.
