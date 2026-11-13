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
	Name      string  `json:"name"`
	Passed    bool    `json:"passed"`
	Output    string  `json:"output,omitempty"`
	Seconds   float64 `json:"seconds"`
	Uncertain bool    `json:"uncertain,omitempty"`
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

// gateSealedTree passes when the candidate leaves the acceptance paths identical to the base.
// It compares Git objects, so it runs before anything from the candidate is checked out.
// The base holds the sealed tests: runs start from a clean, sealed checkout and advance only
// through candidates that passed this gate.
func gateSealedTree(root, base, commit string, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	args := append([]string{"--literal-pathspecs", "diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", base, commit, "--"}, paths...)
	out, err := git(root, args...)
	switch {
	case err != nil:
		g.Output = "compare acceptance tests: " + err.Error()
	case out != "":
		g.Output = "acceptance tests were modified; they are sealed and must not change:\n" + out
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
		if c.Lines < 0 {
			problems = append(problems, "cannot enforce the diff limit for: "+c.Path)
		} else {
			total += c.Lines
		}
		if agentInstructionFile(c.Path) {
			problems = append(problems, "agent instruction file modified: "+c.Path)
		} else if gitControlFile(c.Path) {
			problems = append(problems, "git control file modified: "+c.Path)
		} else if c.Gitlink {
			problems = append(problems, "submodule pointer changed (its content cannot be checked): "+c.Path)
		} else if matchAny(c.Path, protected) {
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

// gitControlFile reports files that tell Git how to transform or fetch content. A candidate
// could otherwise select clean/smudge filters so build and test see bytes other than the
// committed blobs; with these fixed, verification uses the base's attributes only.
func gitControlFile(file string) bool {
	name := strings.ToLower(file[strings.LastIndexAny(file, "/\\")+1:])
	return name == ".gitattributes" || name == ".gitmodules" || name == ".lfsconfig"
}

// agentInstructionFile reports files that agent CLIs load as instructions or settings.
// A candidate could otherwise plant one to steer the reviewer, so none may change anywhere.
func agentInstructionFile(file string) bool {
	parts := strings.Split(strings.ReplaceAll(file, "\\", "/"), "/")
	for i, part := range parts {
		p := strings.ToLower(part)
		if p == ".claude" || p == ".codex" {
			return true
		}
		if i == len(parts)-1 && (p == "agents.md" || p == "claude.md" || p == "claude.local.md") {
			return true
		}
	}
	return false
}

// matchAny reports whether file is covered by any pattern.
// "src/Fees/", "src/Fees" and "src/Fees/**" cover the whole directory; other patterns use path.Match.
func matchAny(file string, patterns []string) bool {
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin" // case-insensitive filesystems by default
	if fold {
		file = strings.ToLower(file)
	}
	for _, p := range patterns {
		if fold {
			p = strings.ToLower(p)
		}
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
func gateCommand(ctx context.Context, name, wt, command string, timeout time.Duration, passEnv []string) GateResult {
	g := GateResult{Name: name}
	if strings.TrimSpace(command) == "" {
		g.Passed = true
		g.Output = "not configured"
		return g
	}
	start := time.Now()
	out, err := runShell(ctx, wt, command, timeout, passEnv)
	g.Seconds = time.Since(start).Seconds()
	g.Output = tail(out, maxGateOutput)
	g.Passed = err == nil
	if errors.Is(err, context.DeadlineExceeded) {
		g.Output = fmt.Sprintf("timed out after %s\n%s", timeout, g.Output)
	}
	g.Uncertain = uncertainExit(err)
	if err != nil && strings.TrimSpace(g.Output) == "" {
		g.Output = err.Error()
	}
	return g
}

// runShell runs a command line through the platform shell and returns combined output.
func runShell(ctx context.Context, dir, command string, timeout time.Duration, passEnv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/D", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = commandEnv(passEnv)
	cmd.WaitDelay = 2 * time.Second
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return buf.String(), err
}

func uncertainExit(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, exec.ErrWaitDelay)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
