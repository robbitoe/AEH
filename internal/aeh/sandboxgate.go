package aeh

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"
)

const (
	sandboxWork   = "/work"
	sandboxRunner = "/run/aeh"
	// readBackLimit caps the bytes hashed when a work volume is read back.
	readBackLimit = int64(16) << 30
)

// tailBuffer keeps the last n bytes written to it, so gate output never grows without
// bound in memory. tail() redacts and cuts it further.
type tailBuffer struct {
	n    int
	buf  []byte
	lost bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.n:]...)
		t.lost = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.lost {
		return "...\n" + string(t.buf)
	}
	return string(t.buf)
}

var _ io.Writer = (*tailBuffer)(nil)

// gateSequence is one verification sequence: the ordered commands that share a work
// directory today (build, test and acceptance of an attempt, the checks of one plan
// verification, build and test of aeh adopt verify). Each command is a new container on
// the same volume, filled once from the commit; integrity is checked after every
// command, before the next one uses the volume. The volume is removed by Close.
type gateSequence struct {
	s      *sandbox
	vol    *sequenceVolume
	export *commitExport
	rules  *ignoreChecker
	broken string // set when integrity failed: the volume is no longer trusted
}

func (s *sandbox) newGateSequence(ctx context.Context, root, commit string) (*gateSequence, error) {
	e, err := newCommitExport(root, commit)
	if err != nil {
		return nil, err
	}
	rules, err := newIgnoreChecker(root, commit, e.Files)
	if err != nil {
		return nil, err
	}
	vol, err := s.newSequence(ctx, "gate", sandboxWork, e.source())
	if err != nil {
		rules.Close()
		if vol != nil {
			_ = vol.Close()
		}
		return nil, err
	}
	return &gateSequence{s: s, vol: vol, export: e, rules: rules}, nil
}

// Broken returns why the work volume is no longer trusted, or "" while it is intact.
// Callers treat it like a damaged checkout: the gate that changed it failed.
func (q *gateSequence) Broken() string { return q.broken }

// Close removes the work volume and confirms the removal.
func (q *gateSequence) Close() error {
	q.rules.Close()
	return q.vol.Close()
}

// gateEnv is the literal, non-secret gate_env map in a stable order.
func (s *sandbox) gateEnv() []string {
	env := make([]string, 0, len(s.cfg.GateEnv))
	for k, v := range s.cfg.GateEnv {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

// command runs one gate command. runnerFiles, when given, are mounted read-only at
// /run/aeh (for example an acceptance manifest). A command that ran is followed by the
// integrity read-back; a broken volume fails this gate and every later one.
func (q *gateSequence) command(ctx context.Context, name, command string, timeout time.Duration, runnerFiles map[string][]byte) GateResult {
	if command == "" {
		return GateResult{Name: name, Passed: true, Output: "not configured"}
	}
	if q.broken != "" {
		return GateResult{Name: name, Output: q.broken, Class: ClassInfrastructure, Reason: "not run: the work volume is no longer trusted"}
	}
	vols := []sandboxVolume{{Name: q.vol.Name, Target: sandboxWork}}
	if runnerFiles != nil {
		vols = append(vols, sandboxVolume{Target: sandboxRunner, ReadOnly: true, Fill: treeFiles(runnerFiles, nil)})
	}
	out := &tailBuffer{n: 64 << 10}
	var integrity error
	verified := false
	start := time.Now()
	res, err := q.s.run(ctx, sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", command}, Volumes: vols,
		Env: q.s.gateEnv(), Workdir: sandboxWork, Timeout: timeout, Output: out},
		func(container string) error {
			return q.s.readBack(ctx, container, sandboxWork, func(r io.Reader) error {
				back, err := readTree(r, "work", readBackLimit)
				if err != nil {
					integrity = err
				} else {
					integrity = checkExport(q.export, back, q.rules)
				}
				verified = integrity == nil
				return nil
			})
		})
	seconds := time.Since(start).Seconds()
	if err != nil {
		res = LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: err.Error()}
	}
	g := gateFromLaunch(name, res, out.String(), seconds)
	switch {
	case integrity != nil:
		q.broken = fmt.Sprintf("the work volume was changed by %s: %v", name, integrity)
		g.Passed = false
		// The diagnostic keeps its own share of the output, so a long command log never
		// pushes the changed paths out.
		g.Output = tail(integrity.Error(), maxGateOutput/2) + "\n" + tail(g.Output, maxGateOutput/2)
	case !verified:
		// The command may have run (timeout, lost state, unreadable output) but its effect
		// on the volume is unknown: nothing may run on it again.
		q.broken = fmt.Sprintf("the work volume could not be verified after %s", name)
	}
	return g
}
