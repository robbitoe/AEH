package aeh

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// agentStep is one agent launch of a flow under sandbox policy 2. The flow decides the
// views from the step it runs (stage 6, section 4); the host never checks out, reads or
// runs Git on what the container wrote.
type agentStep struct {
	Kind    string // implement, review, triage, plan, plan-review, discover, book-review, inspect
	Profile string
	Prompt  string
	RunDir  string
	LogName string
	// Work is the commit exported at /work (read-only); "" for no /work.
	Work string
	// Capture, when set, makes /work writable and records what the agent left as a
	// candidate commit on Work with this message.
	Capture string
	// MemoryDir and MemoryCommit name the memory export mounted read-only at /memory;
	// an empty commit mounts nothing.
	MemoryDir, MemoryCommit string
	// Out gives the agent a writable /out; ReadOut receives it after a clean exit.
	Out     bool
	ReadOut func(io.Reader) error
}

// agentStepResult is what a sandboxed step produced. AgentErr is the agent's failure,
// as the host launcher reports it (errSandboxInfrastructure when nothing is known);
// the error returned beside it is aeh's own failure.
type agentStepResult struct {
	OutPath   string
	Candidate string
	AgentErr  error
	Launch    LaunchResult
}

// sandboxNote tells a sandboxed agent where its views are. It precedes the rendered
// prompt, so project prompt overrides receive it too.
func sandboxNote(st agentStep) string {
	note := "You run in an isolated container.\n"
	switch {
	case st.Work != "" && st.Capture != "":
		note += "- The project is at /work (writable): your changes there are the result.\n"
	case st.Work != "":
		note += "- The project is at /work (read-only).\n"
	}
	if st.Out {
		note += "- Write output files to /out.\n"
	}
	if st.MemoryCommit != "" {
		note += "- Product memory is at /memory (read-only), as of memory commit " + st.MemoryCommit + ".\n"
	}
	return note + "\n"
}

// agentStep runs one step in an agent container.
func (s *sandbox) agentStep(ctx context.Context, root string, cfg Config, st agentStep) (agentStepResult, error) {
	var r agentStepResult
	p, ok := cfg.Profiles[st.Profile]
	if !ok {
		return r, fmt.Errorf("unknown profile %q", st.Profile)
	}
	if err := os.MkdirAll(st.RunDir, 0o700); err != nil {
		return r, err
	}
	infra := func(err error) (agentStepResult, error) {
		r.Launch = LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: err.Error()}
		r.AgentErr = fmt.Errorf("%w: %v", errSandboxInfrastructure, err)
		return r, writeLaunchRecord(s, st, r)
	}
	a := agentLaunch{Kind: st.Kind, Profile: p, PassEnv: p.Sandbox.PassEnv, Prompt: sandboxNote(st) + st.Prompt,
		RunDir: st.RunDir, LogName: st.LogName, Out: st.Out}
	var reads []func(func(string, func(io.Reader) error) error) error
	if st.Work != "" {
		e, err := newCommitExport(root, st.Work)
		if err != nil {
			return infra(err)
		}
		a.Work, a.WorkRW = e.source(), st.Capture != ""
		if st.Capture != "" {
			rules, err := newIgnoreChecker(root, st.Work, e.Files)
			if err != nil {
				return infra(err)
			}
			defer rules.Close()
			spool, err := os.MkdirTemp(st.RunDir, ".capture-")
			if err != nil {
				return r, err
			}
			defer os.RemoveAll(spool)
			reads = append(reads, func(read func(string, func(io.Reader) error) error) error {
				return read(sandboxWork, func(in io.Reader) (err error) {
					r.Candidate, err = captureCandidate(ctx, root, st.Work, e, rules, in, spool, st.Capture)
					return err
				})
			})
		}
	}
	if st.MemoryCommit != "" {
		m, err := newCommitExport(st.MemoryDir, st.MemoryCommit)
		if err != nil {
			return infra(fmt.Errorf("memory export: %w", err))
		}
		a.Memory = m.source()
	}
	if st.Out && st.ReadOut != nil {
		reads = append(reads, func(read func(string, func(io.Reader) error) error) error {
			return read(sandboxOut, st.ReadOut)
		})
	}
	if len(reads) > 0 {
		a.ReadBack = func(read func(string, func(io.Reader) error) error) error {
			for _, fn := range reads {
				if err := fn(read); err != nil {
					return err
				}
			}
			return nil
		}
	}
	out, res, err := s.runAgent(ctx, a)
	r.OutPath, r.Launch = out, res
	if err != nil {
		return r, err
	}
	r.AgentErr = agentError(p, res)
	if r.AgentErr != nil {
		r.Candidate = ""
	}
	return r, writeLaunchRecord(s, st, r)
}

// launchRecord is the evidence of one agent launch, beside its output in the run directory.
type launchRecord struct {
	Kind         string `json:"kind"`
	Profile      string `json:"profile"`
	Class        string `json:"class"`
	ExitCode     int    `json:"exit_code"`
	Reason       string `json:"reason,omitempty"`
	Image        string `json:"image"`
	Network      string `json:"network"`
	Work         string `json:"work,omitempty"`
	WorkWritable bool   `json:"work_writable,omitempty"`
	Out          bool   `json:"out,omitempty"`
	MemoryCommit string `json:"memory_commit,omitempty"`
	Candidate    string `json:"candidate,omitempty"`
}

func writeLaunchRecord(s *sandbox, st agentStep, r agentStepResult) error {
	return writePrivateJSON(filepath.Join(st.RunDir, st.LogName+".launch.json"), launchRecord{
		Kind: st.Kind, Profile: st.Profile, Class: r.Launch.Class, ExitCode: r.Launch.ExitCode, Reason: r.Launch.Reason,
		Image: s.agent.ID, Network: "bridge", Work: st.Work, WorkWritable: st.Capture != "", Out: st.Out,
		MemoryCommit: st.MemoryCommit, Candidate: r.Candidate})
}

// stepMemory is the memory commit a sandboxed agent of an unpinned flow sees: memory's
// HEAD, or none when the project has no memory yet.
func stepMemory(m MemorySnapshot) (dir, commit string, err error) {
	if m.Commit != "" {
		return m.Dir, m.Commit, nil
	}
	if !m.Present || m.Dir == "" {
		return "", "", nil
	}
	head, err := memoryHeadCommit(m.Dir)
	if err != nil {
		return "", "", err
	}
	return m.Dir, head, nil
}

// hostAgents refuses to launch an agent on the host when the configuration runs agents
// in the sandbox: a flow whose agents are not sandboxed yet stops instead of falling back.
func (c Config) hostAgents(flow string) error {
	if c.Sandbox != nil && c.Sandbox.AgentImage != "" {
		return fmt.Errorf("%s agents do not run in the sandbox yet, and sandbox.agent_image forbids running them on the host; remove sandbox.agent_image to use %s", flow, flow)
	}
	return nil
}
