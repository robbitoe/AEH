package aeh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// GateResult is the outcome of one deterministic check. Gates never call a model.
type GateResult struct {
	Name      string   `json:"name"`
	Passed    bool     `json:"passed"`
	Output    string   `json:"output,omitempty"`
	Seconds   float64  `json:"seconds"`
	Uncertain bool     `json:"uncertain,omitempty"`
	Skipped   bool     `json:"skipped,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	// Class says how a command gate's process ended (see LaunchResult). Only
	// ClassProcess proves a red or a green; empty for gates that run no command.
	Class     string `json:"class,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Sandboxed bool   `json:"sandboxed,omitempty"` // the command ran in a sandbox container
	ExitCode  int    `json:"exit_code,omitempty"` // sandboxed commands of class process
}

// sandboxFailure is the gate for a verification sequence the sandbox could not set up
// (engine unreachable, image missing, a tree it cannot export). It blocks with the
// reason and consumes no ladder step; a sandboxed project never falls back to the host.
func sandboxFailure(err error) GateResult {
	return GateResult{Name: "sandbox", Output: err.Error(), Uncertain: true, Class: ClassInfrastructure, Reason: err.Error(), Sandboxed: true}
}

// provesResult reports whether a command gate's pass or failure is evidence about the
// code: its process ran and exited by itself. Gates without a command always prove.
func (g GateResult) provesResult() bool {
	return !g.Uncertain && (g.Class == "" || g.Class == ClassProcess)
}

// gateFromLaunch turns a sandboxed command's result into a gate. Infrastructure failures
// and terminations are uncertain: they block without consuming a ladder step. An OOM kill
// is a failure with its reason, never evidence of a red.
func gateFromLaunch(name string, res LaunchResult, output string, seconds float64) GateResult {
	g := GateResult{Name: name, Seconds: seconds, Output: tail(output, maxGateOutput), Class: res.Class, Reason: res.Reason}
	switch res.Class {
	case ClassProcess:
		g.Passed, g.ExitCode = res.ExitCode == 0, res.ExitCode
	case ClassResource:
		g.Output = strings.TrimSpace(res.Reason + "\n" + g.Output)
	default:
		g.Uncertain = true
		g.Output = strings.TrimSpace("sandbox " + res.Class + ": " + res.Reason + "\n" + g.Output)
	}
	return g
}

const maxGateOutput = 6000

// gateDrift checks path ownership against the run's memory snapshot, without I/O.
// Renames must supply both paths; deletions supply the deleted path.
func gateDrift(paths []string, memory MemorySnapshot, declared string) GateResult {
	g := GateResult{Name: "drift", Passed: true}
	if !memory.Present && declared == "" {
		g.Skipped = true
		g.Output = "skipped: no product memory and no declared feature"
		return g
	}
	files := append([]string(nil), paths...)
	sort.Strings(files)
	touched := map[string][]string{}
	var unowned []string
	for i, file := range files {
		if i > 0 && file == files[i-1] {
			continue
		}
		owned := false
		for _, spec := range memory.Specs {
			if matchAny(file, spec.OwnedPaths()) {
				touched[spec.Feature] = append(touched[spec.Feature], file)
				owned = true
			}
		}
		if !owned {
			unowned = append(unowned, file)
		}
	}
	features := make([]string, 0, len(touched))
	for feature := range touched {
		features = append(features, feature)
	}
	sort.Strings(features)
	if len(unowned) > 0 {
		g.Warnings = append(g.Warnings, "files belong to no feature: "+strings.Join(unowned, ", "))
	}
	if declared != "" && len(touched[declared]) == 0 {
		g.Warnings = append(g.Warnings, fmt.Sprintf("declared feature %q is not touched", declared))
	}
	var details []string
	for _, feature := range features {
		details = append(details, fmt.Sprintf("%s: %s", feature, strings.Join(touched[feature], ", ")))
	}
	switch {
	case len(features) > 1:
		g.Passed = false
		g.Output = "multiple features touched; split into separate outcomes:\n" + strings.Join(details, "\n")
	case len(features) == 1 && features[0] != declared:
		g.Passed = false
		g.Output = fmt.Sprintf("touched feature %q must be declared by this outcome (declared: %q):\n%s", features[0], declared, details[0])
	default:
		g.Output = "feature path ownership checked"
	}
	return g
}

// gateHash fails if the sealed acceptance tests differ inside the worktree.
func gateHash(wt string, s Seal, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	_, files, err := hashPaths(wt, paths)
	for _, file := range s.Retired {
		if e := checkPlanTestPath(file); e != nil {
			g.Output = e.Error()
			return g
		}
		if _, e := os.Lstat(filepath.Join(wt, filepath.FromSlash(file))); !errors.Is(e, os.ErrNotExist) {
			g.Output = "retired acceptance path must remain absent: " + file
			return g
		}
	}
	hash := sealHash(files, s.Retired)
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
	args := append([]string{"--literal-pathspecs", "diff", "--name-only", "--no-renames", "--ignore-submodules=none", "--no-ext-diff", "--no-textconv", base, commit, "--"}, paths...)
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

// sealedGitContents reads a complete manifest from Git objects, including file types.
// Literal pathspecs prevent a filename in a prior implementation from widening the check.
func sealedGitContents(root, commit string, paths []string) (map[string][]byte, error) {
	args := append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", commit, "--"}, paths...)
	out, err := git(root, args...)
	if err != nil {
		return nil, err
	}
	contents := map[string][]byte{}
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("sealed entry is not a regular file: %s", file)
		}
		b, ok := gitBlob(root, commit, file)
		if !ok {
			return nil, fmt.Errorf("read sealed file %s at %s", file, commit)
		}
		contents[file] = []byte(b)
	}
	if len(contents) == 0 {
		return nil, errors.New("sealed paths contain no files")
	}
	return contents, nil
}

// gateSealedCheckout fails if the checked-out sealed files differ from their Git objects
// at commit, the ones the sealed gate verified. Generated seals hash Git objects, so a
// checkout that converts line endings (core.autocrlf, eol attributes) must not fail:
// only CRLF for LF in a blob without carriage returns is tolerated. Any other difference,
// such as a smudge filter's substitution, an extra file or a retired path on disk, fails.
func gateSealedCheckout(root, wt, commit string, retired, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	for _, file := range retired {
		if e := checkPlanTestPath(file); e != nil {
			g.Output = e.Error()
			return g
		}
		if _, e := os.Lstat(filepath.Join(wt, filepath.FromSlash(file))); !errors.Is(e, os.ErrNotExist) {
			g.Output = "retired acceptance path must remain absent: " + file
			return g
		}
	}
	want, err := sealedGitContents(root, commit, paths)
	if err != nil {
		g.Output = "acceptance tests unreadable: " + err.Error()
		return g
	}
	_, files, err := hashPaths(wt, paths)
	if err != nil {
		g.Output = "acceptance tests unreadable: " + err.Error()
		return g
	}
	modified := "acceptance tests were modified; they are sealed and must not change"
	if len(files) != len(want) {
		g.Output = modified
		return g
	}
	for _, f := range files {
		blob, ok := want[f.Path]
		if !ok {
			g.Output = modified
			return g
		}
		disk, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(f.Path)))
		if err != nil {
			g.Output = "acceptance tests unreadable: " + err.Error()
			return g
		}
		if bytes.Equal(disk, blob) {
			continue
		}
		if bytes.IndexByte(blob, '\r') >= 0 || !bytes.Equal(bytes.ReplaceAll(disk, []byte("\r\n"), []byte("\n")), blob) {
			g.Output = modified + ": " + f.Path
			return g
		}
	}
	g.Passed = true
	return g
}

// gateSealedManifest checks the original manifest, never the immediate candidate base.
// Listing the whole acceptance directory also detects newly added files.
func gateSealedManifest(root, commit string, seal Seal, paths []string) GateResult {
	g := GateResult{Name: "hash"}
	for _, file := range seal.Retired {
		if err := checkPlanTestPath(file); err != nil {
			g.Output = err.Error()
			return g
		}
		out, err := git(root, "--literal-pathspecs", "ls-tree", "-z", commit, "--", file)
		if err != nil {
			g.Output = fmt.Sprintf("inspect retired path %s: %v", file, err)
			return g
		}
		if out != "" {
			g.Output = "retired acceptance path must remain absent: " + file
			return g
		}
	}
	contents, err := sealedGitContents(root, commit, paths)
	if err != nil {
		g.Output = "acceptance tests unreadable: " + err.Error()
		return g
	}
	actual := sealFromContents(seal.Outcome, contents, seal.SpecContent, seal.ApprovedAt, seal.Retired)
	if actual.Hash != seal.Hash {
		g.Output = "acceptance tests were modified; they are sealed and must not change"
		return g
	}
	g.Passed = true
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
	var exit *exec.ExitError
	switch {
	case err == nil || errors.As(err, &exit):
		g.Class = ClassProcess
	case g.Uncertain:
		g.Class, g.Reason = ClassTerminated, err.Error()
	default:
		// The shell could not start: nothing ran, so the gate proves nothing and blocks
		// (uncertain) instead of consuming a ladder attempt.
		g.Class, g.Reason, g.Uncertain = ClassInfrastructure, err.Error(), true
	}
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
	err := runProcessTree(cmd)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return buf.String(), err
}

// uncertainExit reports an agent or command whose outcome is unknown: it was stopped, or
// the sandbox could not carry it out. Such a result blocks without spending a ladder step.
func uncertainExit(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, exec.ErrWaitDelay) ||
		errors.Is(err, errSandboxInfrastructure)
}

// tail keeps the last n bytes of command or agent output. It redacts recognized secrets in the
// whole text first: a cut made before redaction could drop a credential's prefix and leave the
// rest unrecognizable.
func tail(s string, n int) string {
	s = redactSecrets(s)
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
