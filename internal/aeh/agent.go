package aeh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runAgent launches one profile's CLI in dir with the prompt and logs its output to runDir.
// The caller decides how to handle CLI failures.
func runAgent(ctx context.Context, p Profile, passEnv []string, dir, prompt, runDir, logName string) (string, error) {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", err
	}
	promptFile := filepath.Join(runDir, logName+".prompt.md")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return "", err
	}
	args := make([]string, len(p.Command))
	for i, a := range p.Command {
		a = strings.ReplaceAll(a, "{{prompt_file}}", promptFile)
		a = strings.ReplaceAll(a, "{{prompt}}", prompt)
		args[i] = a
	}
	timeout := time.Duration(p.TimeoutMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(actx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = commandEnv(passEnv)
	for k, v := range p.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	if p.Stdin {
		cmd.Stdin = strings.NewReader(prompt)
	}
	outPath := filepath.Join(runDir, logName+".out")
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	errf, err := os.OpenFile(filepath.Join(runDir, logName+".err"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer errf.Close()
	cmd.Stdout, cmd.Stderr = out, errf

	runErr := runProcessTree(cmd)
	if ctx.Err() != nil {
		return outPath, ctx.Err() // the whole run was cancelled
	}
	if actx.Err() != nil {
		return outPath, fmt.Errorf("agent timed out after %s: %w", timeout, actx.Err())
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		for _, code := range p.QuotaExitCodes {
			if exit.ExitCode() == code {
				return outPath, fmt.Errorf("provider quota signal (exit %d): %w", code, exit)
			}
		}
		return outPath, fmt.Errorf("agent exited with code %d: %w", exit.ExitCode(), exit)
	}
	return outPath, runErr
}

// baseEnv lists the OS and tool-discovery variables every agent and gate command inherits.
var baseEnv = []string{
	"PATH", "PATHEXT", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "USER", "USERNAME", "LOGNAME",
	"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "OS", "COMPUTERNAME",
	"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS",
	"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "COMMONPROGRAMFILES", "COMMONPROGRAMFILES(X86)",
	"PROGRAMDATA", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "TMPDIR", "SHELL", "TERM", "TZ",
	"LANG", "LC_ALL", "LC_CTYPE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"CODEX_HOME", "CLAUDE_CONFIG_DIR",
}

// commandEnv passes baseEnv plus the names listed in pass_env. Credentials stay out
// unless the config names them in pass_env or in a profile's env.
func commandEnv(passEnv []string) []string {
	allowed := map[string]bool{}
	for _, name := range append(append([]string{}, baseEnv...), passEnv...) {
		allowed[strings.ToUpper(name)] = true
	}
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(name)] {
			env = append(env, entry)
		}
	}
	return env
}
