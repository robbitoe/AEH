package aeh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"
)

// GateResult is the outcome of one deterministic check. Gates never call a model.
type GateResult struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Output  string  `json:"output,omitempty"`
	Seconds float64 `json:"seconds"`
}

const maxGateOutput = 6000

// gateHash fails if the sealed acceptance tests differ inside the worktree.
func gateHash(wt string, s Seal, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	hash, _, err := hashPaths(wt, paths)
	switch {
	case err != nil:
		g.Output = "acceptance tests unreadable: " + err.Error()
	case hash != s.Hash:
		g.Output = "acceptance tests were modified; they are sealed and must not change"
	default:
		g.Passed = true
	}
	return g
}

// gateScope fails if the change touches protected paths, leaves the task's paths, or is too large.
func gateScope(changes []fileChange, protected, allowed []string, maxLines int) GateResult {
	g := GateResult{Name: "scope"}
	if len(changes) == 0 {
		g.Output = "the agent produced no changes"
		return g
	}
	var problems []string
	total := 0
	for _, c := range changes {
		total += c.Lines
		if matchAny(c.Path, protected) {
			problems = append(problems, "protected path modified: "+c.Path)
		} else if len(allowed) > 0 && !matchAny(c.Path, allowed) {
			problems = append(problems, "outside the task's paths: "+c.Path)
		}
	}
	if maxLines > 0 && total > maxLines {
		problems = append(problems, fmt.Sprintf("diff has %d changed lines; the limit is %d", total, maxLines))
	}
	if len(problems) > 0 {
		g.Output = strings.Join(problems, "\n")
		return g
	}
	g.Passed = true
	g.Output = fmt.Sprintf("%d files, %d changed lines", len(changes), total)
	return g
}

// matchAny reports whether file is covered by any pattern.
// "src/Fees/", "src/Fees" and "src/Fees/**" cover the whole directory; other patterns use path.Match.
func matchAny(file string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
		base := strings.TrimSuffix(strings.TrimSuffix(p, "**"), "/")
		if base == "" {
			return true
		}
		if file == base || strings.HasPrefix(file, base+"/") {
			return true
		}
		if ok, _ := path.Match(p, file); ok {
			return true
		}
	}
	return false
}

// gateCommand runs a configured shell command (build or test) in the worktree.
func gateCommand(ctx context.Context, name, wt, command string, timeout time.Duration) GateResult {
	g := GateResult{Name: name}
	if strings.TrimSpace(command) == "" {
		g.Passed = true
		g.Output = "not configured"
		return g
	}
	start := time.Now()
	out, err := runShell(ctx, wt, command, timeout)
	g.Seconds = time.Since(start).Seconds()
	g.Output = tail(out, maxGateOutput)
	g.Passed = err == nil
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		g.Output = fmt.Sprintf("timed out after %s\n%s", timeout, g.Output)
	}
	return g
}

// runShell runs a command line through the platform shell and returns combined output.
func runShell(ctx context.Context, dir, command string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return buf.String(), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
