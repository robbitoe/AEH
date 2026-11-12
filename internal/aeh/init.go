package aeh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const configTemplate = `# AEH constitution for this project. Committed with the code.
project: my-project

# The only thing AEH knows about your stack: how to build (lint included) and test it.
commands:
  build: ""            # e.g. "dotnet build -warnaserror" or "npm run build && npm run lint"
  test: "echo set commands.test && exit 1"   # e.g. "dotnet test" or "npm test"
  timeout_minutes: 20

# Agents and build/test commands inherit only OS and tool-discovery variables
# (PATH, HOME, TEMP...). List any other variable names they need, e.g. JAVA_HOME.
pass_env: []

# Paths no agent may modify. aeh/ and each outcome's acceptance tests are always protected.
protected: []

limits:
  max_diff_lines: 400  # per task, unless the task sets its own

# Escalation order. A task starts at the first profile (or its own "start"),
# retries once with feedback, then tries the next profile, then blocks.
ladder: [medio, fuerte]

# Level 2 requires a separate reviewer profile with plain-text final output.
reviewer: codex-review

notify:
  webhook: ""          # POST {event, outcome, title, message} when an outcome blocks or is accepted

# How each agent CLI is launched. {{prompt}} is replaced with the prompt text,
# {{prompt_file}} with a file holding it; set stdin: true to pipe it instead.
# Check the flags against the CLI version you have installed.
profiles:
  medio:
    command: ["codex", "exec", "--sandbox", "workspace-write", "--json", "-"]
    stdin: true
    timeout_minutes: 30
    # quota_exit_codes: []  # Set only when the CLI documents a distinct quota exit code.
  codex-review:
    command: ["codex", "exec", "--sandbox", "read-only", "-"]
    stdin: true
    timeout_minutes: 15
  fuerte:
    command: ["claude", "-p", "{{prompt}}", "--permission-mode", "acceptEdits", "--output-format", "json"]
    timeout_minutes: 30
  local:
    command: ["claude", "-p", "{{prompt}}", "--permission-mode", "acceptEdits", "--model", "openai/gpt-oss-20b"]
    env:
      ANTHROPIC_BASE_URL: "http://localhost:1234"
      ANTHROPIC_AUTH_TOKEN: "lmstudio"
    timeout_minutes: 30
`

const outcomeExample = `# Copy to OUT-001.yaml and edit. Level 2 = sealed tests + review; level 1 = gates only.
id: OUT-001
title: The administrator generates the month's fees without duplicate charges
level: 2
actor: administrator
description: |
  Generate one fee per unit for a given month. Running it twice must not create duplicates.

criteria:
  - given: unit 12 has no fee for March
    when: the administrator generates the March fees
    then: unit 12 has exactly one fee for March
  - given: the March fees were already generated
    when: the administrator generates them again
    then: no unit has more than one fee for March

# Acceptance tests you approve with "aeh tests approve OUT-001". Agents can never change them.
acceptance:
  - tests/Acceptance/OUT001/

# Optional. Without tasks the outcome runs as a single task.
tasks:
  - id: T1
    goal: Implement fee generation for a month, idempotent per unit and month
    paths: [src/Fees/]
    # start: fuerte        # profile to begin with; default is the first in the ladder
    # max_diff_lines: 300
`

// Init creates aeh/config.yaml, an example outcome and ignores .aeh/. It never overwrites files.
func Init(r Repo) ([]string, error) {
	var created []string
	write := func(path, content string) error {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		created = append(created, rel(r.Root, path))
		return nil
	}
	if err := write(r.ConfigPath(), configTemplate); err != nil {
		return created, err
	}
	if err := write(filepath.Join(r.Root, "aeh", "outcomes", "OUT-001.example.yaml"), outcomeExample); err != nil {
		return created, err
	}
	added, err := ensureIgnored(filepath.Join(r.Root, ".gitignore"), ".aeh/")
	if err != nil {
		return created, err
	}
	if added {
		created = append(created, ".gitignore (added .aeh/)")
	}
	return created, nil
}

func ensureIgnored(path, entry string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == entry {
			return false, nil
		}
	}
	prefix := ""
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n# AEH local state: runs, worktrees, reports\n%s\n", prefix, entry)
	return err == nil, err
}
