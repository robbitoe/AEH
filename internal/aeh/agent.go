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
// A non-zero exit is reported but not fatal: the gates decide whether the work is acceptable.
func runAgent(ctx context.Context, p Profile, dir, prompt, runDir, logName string) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", err
	}
	promptFile := filepath.Join(runDir, logName+".prompt.md")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
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
	cmd.Env = os.Environ()
	for k, v := range p.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	if p.Stdin {
		cmd.Stdin = strings.NewReader(prompt)
	}
	outPath := filepath.Join(runDir, logName+".out")
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	errf, err := os.Create(filepath.Join(runDir, logName+".err"))
	if err != nil {
		return "", err
	}
	defer errf.Close()
	cmd.Stdout, cmd.Stderr = out, errf

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return outPath, ctx.Err() // the whole run was cancelled
	}
	if actx.Err() != nil {
		return outPath, fmt.Errorf("agent timed out after %s", timeout)
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		return outPath, fmt.Errorf("agent exited with code %d", exit.ExitCode())
	}
	return outPath, runErr
}
