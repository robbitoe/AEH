package aeh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The sandbox policy version a flow pins: 0 runs everything on the host; 1 runs gate
// commands in containers (stage 6, S1); 2 also ran every agent in a container on
// Docker's default network (S2); 3 runs agents with no network but their profile's
// egress pairs, through a proxy per launch (S3). Agents always run under 3 now.
const (
	sandboxPolicyGates  = 1
	sandboxPolicyAgents = 2
	sandboxPolicyEgress = 3
)

// sandboxContract is what a resumable flow pins about isolation. It is nil without a
// sandbox, so contracts of flows created before stage 6 do not change; the agent fields
// are empty under policy 1, so its contracts do not change either.
func sandboxContract(sb *sandbox) any {
	if sb == nil {
		return nil
	}
	c := struct {
		Policy       int
		Engine       string
		Image        string
		Memory       string
		CPUs         string
		GateEnv      map[string]string
		AgentImage   string              `json:",omitempty"`
		AgentNetwork string              `json:",omitempty"`
		AgentEnv     map[string][]string `json:",omitempty"`
		AgentSession map[string][]string `json:",omitempty"`
		AgentEgress  map[string][]string `json:",omitempty"`
	}{Policy: sandboxPolicyGates, Engine: sb.engine.ID, Image: sb.image.ID, Memory: sb.cfg.Memory, CPUs: sb.cfg.CPUs, GateEnv: sb.cfg.GateEnv}
	if sb.agents() {
		c.Policy, c.AgentImage, c.AgentNetwork, c.AgentEnv = sandboxPolicyEgress, sb.agent.ID, "none", sb.agentEnv
		c.AgentSession, c.AgentEgress = sb.agentSession, sb.agentEgress
	}
	return c
}

// agents reports whether agents run in containers (policy 3).
func (s *sandbox) agents() bool { return s != nil && s.agent.ID != "" }

// openFlowSandbox opens the project's sandbox for one flow, or returns nil when the
// configuration has none. A configured sandbox that cannot open stops the flow: a
// sandboxed project never falls back to the host.
func openFlowSandbox(r Repo, cfg Config, flow string) (*sandbox, error) {
	if cfg.Sandbox == nil {
		return nil, nil
	}
	sb, err := openSandbox(r, *cfg.Sandbox, flow)
	if err != nil {
		return nil, err
	}
	if sb.agents() {
		sb.agentEnv, sb.agentSession, sb.agentEgress = map[string][]string{}, map[string][]string{}, map[string][]string{}
		for name, p := range cfg.Profiles {
			if env := p.sandboxPassEnv(); len(env) > 0 {
				sb.agentEnv[name] = append([]string(nil), env...)
			}
			if files := p.sandboxSession(); len(files) > 0 {
				sb.agentSession[name] = append([]string(nil), files...)
			}
			if pairs := p.sandboxEgress(); len(pairs) > 0 {
				sb.agentEgress[name] = append([]string(nil), pairs...)
			}
		}
	}
	return sb, nil
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

// sandboxSnapshot is the canonical text of a flow's isolation (sandboxContract): empty for
// the host. A plan pins it; its run and any recovery that records its evidence must see
// the same, so evidence is never reused under another isolation.
func sandboxSnapshot(sb *sandbox) (string, error) {
	c := sandboxContract(sb)
	if c == nil {
		return "", nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("sandbox snapshot: %w", err)
	}
	return string(b), nil
}

// checkPlanSandbox refuses to use a plan's evidence under an isolation other than the
// one its checks ran under.
func checkPlanSandbox(id, pinned string, sb *sandbox) error {
	now, err := sandboxSnapshot(sb)
	if err != nil {
		return err
	}
	if now != pinned {
		return fmt.Errorf("%s was verified under another sandbox configuration (%s) than the current one (%s); restore it, or abandon %s and plan again",
			id, describeSnapshot(pinned), describeSnapshot(now), id)
	}
	return nil
}

func describeSnapshot(s string) string {
	if s == "" {
		return "none: the host"
	}
	return s
}
