package aeh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fixed container paths of an agent launch. Prompts name these paths, never host paths.
const (
	sandboxOut        = "/out"
	sandboxMemory     = "/memory"
	sandboxPromptFile = sandboxRunner + "/prompt.md"
	// agentOutputLimit caps an agent's stdout and stderr each. A larger output is an
	// explicit failure of the agent, never a parse of part of it.
	agentOutputLimit = int64(64) << 20
)

// errOutputLimit marks an agent whose output exceeded agentOutputLimit.
var errOutputLimit = errors.New("agent output exceeds the limit")

// agentLaunch describes one agent in a container. The caller decides every view from the
// step it runs (the view matrix of stage 6, section 4); nothing is inferred from the
// profile.
type agentLaunch struct {
	Kind    string // implement, review, triage, plan, plan-review, discover, book-review, inspect, characterize
	Profile Profile
	PassEnv []string // host variable names the profile forwards into the container
	// Work is the tree at /work, read-only unless WorkRW. Nil: no /work.
	Work   tarSource
	WorkRW bool
	// Memory is the pinned memory export at /memory, always read-only. Nil: none.
	Memory tarSource
	// Out gives the agent a writable /out for its output files.
	Out     bool
	Prompt  string
	RunDir  string
	LogName string
	// ReadBack runs while the stopped container still exists, after the process exited
	// by itself: the caller copies /work or /out out through read.
	ReadBack func(read func(target string, fn func(io.Reader) error) error) error
}

// agentArgv renders a profile's command with container paths. A profile that reads its
// prompt from stdin gets a fixed wrapper that redirects the read-only prompt file, since
// launches take no stdin.
func agentArgv(p Profile, prompt string, out bool) ([]string, error) {
	if len(p.Command) == 0 {
		return nil, errors.New("the profile has no command")
	}
	if !out && usesOutputDir(p) {
		return nil, fmt.Errorf("the profile's command uses %s, but this step has no output directory; use that profile only in adopt.ladder", outputDirPlaceholder)
	}
	args := make([]string, len(p.Command))
	for i, a := range p.Command {
		a = strings.ReplaceAll(a, "{{prompt_file}}", sandboxPromptFile)
		a = strings.ReplaceAll(a, outputDirPlaceholder, sandboxOut)
		a = strings.ReplaceAll(a, "{{prompt}}", prompt) // last: the prompt's text is never expanded
		args[i] = a
	}
	if p.Stdin {
		args = append([]string{"/bin/sh", "-c", `exec "$@" < ` + sandboxPromptFile, "aeh-agent"}, args...)
	}
	return args, nil
}

// agentEnv is the profile's environment inside the container: its env map and the host
// values of the names it forwards. HOME, TMPDIR and the proxy variables belong to aeh.
func agentEnv(p Profile, passEnv []string) ([]string, error) {
	var env []string
	check := func(name string) error {
		if !envNamePlain.MatchString(name) {
			return fmt.Errorf("invalid variable name %q", name)
		}
		if reservedSandboxEnv[strings.ToUpper(name)] || isProxyVar(name) {
			return fmt.Errorf("%s is set by aeh inside the sandbox", name)
		}
		return nil
	}
	for _, name := range passEnv {
		if err := check(name); err != nil {
			return nil, err
		}
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for k, v := range p.Env {
		if err := check(k); err != nil {
			return nil, err
		}
		env = append(env, k+"="+os.ExpandEnv(v))
	}
	return env, nil
}

// cappedFile writes up to limit bytes and then fails, so an agent cannot fill the disk
// through its output and a cut output is never parsed as a whole one.
type cappedFile struct {
	f    *os.File
	left int64
}

func (c *cappedFile) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errOutputLimit
	}
	n, err := c.f.Write(p)
	c.left -= int64(n)
	return n, err
}

// runAgent runs one agent launch and writes its stdout to <LogName>.out and its stderr
// to <LogName>.err in RunDir, like the host launcher. The result's class says whether the
// process ran: only ClassProcess has a meaningful exit code. err is set only when aeh
// itself failed (files, cleanup); the caller then stops.
func (s *sandbox) runAgent(ctx context.Context, a agentLaunch) (outPath string, res LaunchResult, err error) {
	infra := func(e error) (string, LaunchResult, error) {
		return outPath, LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: e.Error()}, nil
	}
	if err := os.MkdirAll(a.RunDir, 0o700); err != nil {
		return "", LaunchResult{}, err
	}
	// The host copy of the prompt is evidence only; the container reads its own copy.
	if err := os.WriteFile(filepath.Join(a.RunDir, a.LogName+".prompt.md"), []byte(a.Prompt), 0o600); err != nil {
		return "", LaunchResult{}, err
	}
	outPath = filepath.Join(a.RunDir, a.LogName+".out")
	argv, err := agentArgv(a.Profile, a.Prompt, a.Out)
	if err != nil {
		return infra(err)
	}
	env, err := agentEnv(a.Profile, a.PassEnv)
	if err != nil {
		return infra(fmt.Errorf("profile environment: %w", err))
	}
	vols := []sandboxVolume{{Target: sandboxRunner, ReadOnly: true, Fill: treeFiles(map[string][]byte{"prompt.md": []byte(a.Prompt)}, nil)}}
	workdir := sandboxRunner
	if a.Out {
		vols = append(vols, sandboxVolume{Target: sandboxOut, Fill: treeFiles(nil, nil)})
		workdir = sandboxOut
	}
	if a.Work != nil {
		vols = append(vols, sandboxVolume{Target: sandboxWork, ReadOnly: !a.WorkRW, Fill: a.Work})
		workdir = sandboxWork
	}
	if a.Memory != nil {
		vols = append(vols, sandboxVolume{Target: sandboxMemory, ReadOnly: true, Fill: a.Memory})
	}
	timeout := time.Duration(a.Profile.TimeoutMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", LaunchResult{}, err
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(a.RunDir, a.LogName+".err"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", LaunchResult{}, err
	}
	defer errf.Close()
	var after func(string) error
	if a.ReadBack != nil {
		after = func(cid string) error {
			return a.ReadBack(func(target string, fn func(io.Reader) error) error {
				return s.readBack(ctx, cid, target, fn)
			})
		}
	}
	res, err = s.run(ctx, sandboxLaunch{Kind: a.Kind, Argv: argv, Volumes: vols, Env: env, Workdir: workdir,
		Pids: true, Timeout: timeout, Output: &cappedFile{f: out, left: agentOutputLimit},
		Stderr: &cappedFile{f: errf, left: agentOutputLimit}, Agent: true}, after)
	if err != nil {
		return outPath, res, err
	}
	if res.Class == ClassInfrastructure && strings.Contains(res.Reason, errOutputLimit.Error()) {
		// The process ran; its output is too large to trust. That is the agent's failure.
		res = LaunchResult{Class: ClassProcess, ExitCode: -1, Reason: fmt.Sprintf("the agent wrote more than %d MiB to stdout or stderr", agentOutputLimit>>20)}
	}
	return outPath, res, nil
}

// agentError turns a sandboxed agent's result into the error the host launcher would
// return, so the flows keep one way of handling failures. Infrastructure failures carry
// errSandboxInfrastructure: nothing is known about the agent, and no ladder step is spent.
func agentError(p Profile, res LaunchResult) error {
	switch res.Class {
	case ClassProcess:
		if res.Reason != "" {
			return errors.New(res.Reason)
		}
		if res.ExitCode == 0 {
			return nil
		}
		for _, code := range p.QuotaExitCodes {
			if res.ExitCode == code {
				return fmt.Errorf("provider quota signal (exit %d)", code)
			}
		}
		return fmt.Errorf("agent exited with code %d", res.ExitCode)
	case ClassResource:
		return fmt.Errorf("agent %s", res.Reason)
	case ClassTerminated:
		if strings.HasPrefix(res.Reason, "timed out") {
			return fmt.Errorf("agent %s: %w", res.Reason, context.DeadlineExceeded)
		}
		return fmt.Errorf("agent %s: %w", res.Reason, context.Canceled)
	}
	return fmt.Errorf("%w: %s", errSandboxInfrastructure, res.Reason)
}

// errSandboxInfrastructure marks a launch the runtime could not carry out.
var errSandboxInfrastructure = errors.New("sandbox infrastructure failure")
