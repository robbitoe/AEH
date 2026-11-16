You are authoring characterization tests for outcome {{.Outcome.ID}}: tests that pin behavior
the existing code already has, confirmed by the owner. Work inside the current worktree. Do
not commit, push or create branches. Do not change any code: nothing is implemented here.

{{.MemorySpec}}

## Test author rules

- Write only inside {{.TestDir}}. Add new regular files only; no control files, symlinks or submodules.
- Write one or more test files for every criterion listed as new in the reference block. Each criterion describes behavior the code has today.
- Each test must pass on the existing code, on its own and together with the others. Assert the observable behavior the criterion states, never internals it does not require, and never a test that passes without exercising that behavior.
- Use at most {{.MaxFiles}} files and {{.MaxBytes}} total bytes, including plan.yaml. Keep plan.yaml at most 65536 bytes.
- Write {{.TestDir}}plan.yaml with exactly this structure (replace the examples with the actual criteria and test paths):

```yaml
kind: characterize
trace:
  AC-1: [pay.test.js]
  AC-3: [refund.test.js]
```

Use one YAML document without extra keys, anchors, aliases, merge keys or explicit tags. There
is no task. Trace keys must be exactly the criterion IDs listed as new. Values are non-empty
lists of clean file paths relative to {{.TestDir}}, never plan.yaml. Trace every test file.

## Commands configured by the owner

Build: {{.Build}}
Test: {{.Test}}
Acceptance: {{.Acceptance}}

The runner passes test paths using a manifest, replaces the manifest placeholder, and verifies
that every new test file passes on the existing code, alone and together, and that the
previous acceptance suite still passes with them added.
{{if .Feedback}}
## Feedback from the previous attempt

{{.Feedback}}
{{end}}
When the tests and plan are ready, end with exactly:
PLAN: READY
