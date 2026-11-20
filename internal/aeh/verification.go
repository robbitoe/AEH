package aeh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SandboxPolicy is the sandbox policy version a flow pins: 0 runs everything on the host;
// 1 runs gate commands in containers (stage 6, S1).
const sandboxPolicyGates = 1

// sandboxContract is what a resumable flow pins about isolation. It is nil without a
// sandbox, so contracts of flows created before stage 6 do not change.
func sandboxContract(sb *sandbox) any {
	if sb == nil {
		return nil
	}
	return struct {
		Policy  int
		Engine  string
		Image   string
		Memory  string
		CPUs    string
		GateEnv map[string]string
	}{sandboxPolicyGates, sb.engine.ID, sb.image.ID, sb.cfg.Memory, sb.cfg.CPUs, sb.cfg.GateEnv}
}

// openFlowSandbox opens the project's sandbox for one flow, or returns nil when the
// configuration has none. A configured sandbox that cannot open stops the flow: a
// sandboxed project never falls back to the host.
func openFlowSandbox(r Repo, cfg Config, flow string) (*sandbox, error) {
	if cfg.Sandbox == nil {
		return nil, nil
	}
	return openSandbox(r, *cfg.Sandbox, flow)
}

// verification runs the ordered commands of one verification sequence on one commit:
// on the host in a worktree checked out at the commit, or in a sandbox work volume
// exported from it. Callers read the same GateResults either way.
type verification struct {
	wt, commit string
	timeout    time.Duration
	passEnv    []string
	seq        *gateSequence
}

func newVerification(ctx context.Context, sb *sandbox, root, wt, commit string, cfg Config) (*verification, error) {
	v := &verification{wt: wt, commit: commit, timeout: time.Duration(cfg.Commands.TimeoutMinutes) * time.Minute, passEnv: cfg.PassEnv}
	if sb != nil {
		seq, err := sb.newGateSequence(ctx, root, commit)
		if err != nil {
			return nil, fmt.Errorf("sandbox: %w", err)
		}
		v.seq = seq
	}
	return v, nil
}

// Close removes the sandbox volume of the sequence, if any. The host worktree belongs
// to the caller.
func (v *verification) Close() error {
	if v == nil || v.seq == nil {
		return nil
	}
	return v.seq.Close()
}

// raw runs a configured command. On the host the caller checks the checkout afterwards
// (damaged); in the sandbox the integrity check already ran and a changed volume fails.
func (v *verification) raw(ctx context.Context, name, command string) GateResult {
	if v.seq != nil {
		g := v.seq.command(ctx, name, command, v.timeout, nil)
		g.Sandboxed = true
		return g
	}
	return gateCommand(ctx, name, v.wt, command, v.timeout, v.passEnv)
}

// checked runs a command and fails it when it changed the checkout.
func (v *verification) checked(ctx context.Context, name, command string) GateResult {
	g := v.raw(ctx, name, command)
	if v.seq != nil {
		return g
	}
	return checkoutGate(g, v.wt, v.commit)
}

// damaged reports a checkout or volume that no longer matches the commit.
func (v *verification) damaged() error {
	if v.seq != nil {
		if b := v.seq.Broken(); b != "" {
			return fmt.Errorf("%s", b)
		}
		return nil
	}
	return verifyCheckout(v.wt, v.commit)
}

// acceptance runs the acceptance command template on a runner-written manifest that
// lists the given repository-relative test files. The manifest path reaches the shell
// only as a fixed, safe string: on the host a relative path inside the worktree's
// ignored .aeh/, in the sandbox a file of the read-only runner view.
func (v *verification) acceptance(ctx context.Context, name, template string, files []string) (GateResult, error) {
	rel := ".aeh/manifest-" + name + ".txt"
	if !safeManifestName.MatchString(rel) {
		return GateResult{}, fmt.Errorf("unsafe manifest name %q", rel)
	}
	body := []byte(strings.Join(files, "\n") + "\n")
	if v.seq != nil {
		file := "manifest-" + name + ".txt"
		command := strings.Replace(template, "{{manifest}}", sandboxRunner+"/"+file, 1)
		g := v.seq.command(ctx, name, command, v.timeout, map[string][]byte{file: body})
		g.Sandboxed = true
		return g, nil
	}
	full := filepath.Join(v.wt, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return GateResult{}, err
	}
	if err := os.WriteFile(full, body, 0o600); err != nil {
		return GateResult{}, err
	}
	defer os.Remove(full)
	command := strings.Replace(template, "{{manifest}}", rel, 1)
	return gateCommand(ctx, name, v.wt, command, v.timeout, v.passEnv), nil
}
