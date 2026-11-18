You are authoring acceptance tests and one implementation task for outcome {{.Outcome.ID}}.
Work inside the current worktree. Do not commit, push or create branches.

{{.MemorySpec}}

## Test author rules

- Write only inside {{.TestDir}}. Add new regular files only; no control files, symlinks or submodules.
- Write test files for every new, changed and recovery criterion in the reference block. The runner deletes the listed retired files; do not edit or delete them yourself. Other unchanged coverage stays sealed.
- Tests for new and changed criteria must fail on the base. Recovery tests for unchanged criteria must pass on the base. Red and green criteria must never share a file. Check observable behavior, without depending on internals the criterion does not require.
- Use at most {{.MaxFiles}} files and {{.MaxBytes}} total bytes, including plan.yaml. Keep plan.yaml at most 65536 bytes.
{{if .NoImplementation}}- This delta has nothing to implement: no new criteria and no behavior changes. Do not define a task: omit `task:` from plan.yaml. Every test you write must pass on the base.
{{else}}- Define exactly one implementation task covering all new and changed criteria, with a single-line goal of at most 500 bytes. Its literal paths must be inside the feature's paths and outside protected paths, aeh/, .aeh/ and the acceptance directory. No wildcards.
- The implementation task has a limit of {{.MaxLines}} changed lines. If the delta cannot fit one task at that limit, explain why, stop, and end with exactly:
  PLAN: TOO-LARGE
{{end}}- Otherwise write {{.TestDir}}plan.yaml with exactly this structure (replace the examples with the actual new, changed and recovery criteria and test paths):

```yaml
task:
  goal: Create the order on payment and prevent double charges
  paths: [src/checkout/]
trace:
  AC-1: [pay.test.js]
  AC-2: [retry.test.js]
```

Use one YAML document without extra keys, anchors, aliases, merge keys or explicit tags.
Trace keys must be exactly the new, changed, clarified and recovery criterion IDs. Values are non-empty lists of clean file
paths relative to {{.TestDir}}, never plan.yaml. Trace every test file; no untraced files.

## Commands configured by the owner

Build: {{.Build}}
Test: {{.Test}}
Acceptance: {{.Acceptance}}

The runner passes test paths using a manifest, replaces the manifest placeholder, and verifies
that each new or changed test file fails on the base and each recovery file passes.
A failure from an unrelated error is not acceptable.
{{if .Feedback}}
## Feedback from the previous attempt

{{.Feedback}}
{{end}}
When the tests and plan are ready, end with exactly:
PLAN: READY
