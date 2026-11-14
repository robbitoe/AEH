You are authoring acceptance tests and one implementation task for outcome {{.Outcome.ID}}.
Work inside the current worktree. Do not commit, push or create branches.

{{.MemorySpec}}

## Test author rules

- Write only inside {{.TestDir}}. Add new regular files only; no control files, symlinks or submodules.
- Write one or more test files for every new criterion in the reference block. Do not test the unchanged criteria again; their sealed tests remain in force.
- Each test must fail until the feature exists and check the behavior its criterion states. Check observable behavior, without depending on internals the criterion does not require.
- Use at most {{.MaxFiles}} files and {{.MaxBytes}} total bytes, including plan.yaml. Keep plan.yaml at most 65536 bytes.
- Define exactly one implementation task covering all new criteria, with a single-line goal of at most 500 bytes. Its literal paths must be inside the feature's paths and outside protected paths, aeh/, .aeh/ and the acceptance directory. No wildcards.
- The implementation task has a limit of {{.MaxLines}} changed lines. If the delta cannot fit one task at that limit, explain why, stop, and end with exactly:
  PLAN: TOO-LARGE
- Otherwise write {{.TestDir}}plan.yaml with exactly this structure (replace the examples with the actual new criteria and test paths):

```yaml
task:
  goal: Create the order on payment and prevent double charges
  paths: [src/checkout/]
trace:
  AC-1: [pay.test.js]
  AC-2: [retry.test.js]
```

Use one YAML document without extra keys, anchors, aliases, merge keys or explicit tags.
Trace keys must be exactly the new criterion IDs. Values are non-empty lists of clean file
paths relative to {{.TestDir}}, never plan.yaml. Trace every test file; no untraced files.

## Commands configured by the owner

Build: {{.Build}}
Test: {{.Test}}
Acceptance: {{.Acceptance}}

The runner passes test paths using a manifest, replaces the manifest placeholder, and verifies
that every new test file fails on the base. A failure from an unrelated error is not acceptable.
{{if .Feedback}}
## Feedback from the previous attempt

{{.Feedback}}
{{end}}
When the tests and plan are ready, end with exactly:
PLAN: READY
