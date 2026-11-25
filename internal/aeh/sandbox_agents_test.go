package aeh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func loadTestConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	r := Repo{Root: t.TempDir()}
	write(t, r.Root, "aeh/config.yaml", body)
	return r.LoadConfig()
}

const minimalConfig = "project: t\ncommands:\n  test: \"true\"\nladder: [a]\nreviewer: b\nprofiles:\n  a:\n    provider: x\n    command: [a]\n  b:\n    provider: y\n    command: [b]\n"

func TestAgentSandboxConfig(t *testing.T) {
	good := minimalConfig + "    sandbox:\n      pass_env: [OPENAI_API_KEY]\nsandbox:\n  image: \"" + testDigest + "\"\n  agent_image: \"" + testDigest + "\"\n"
	c, err := loadTestConfig(t, good)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sandbox.AgentImage != testDigest || len(c.Profiles["b"].sandboxPassEnv()) != 1 {
		t.Fatalf("parsed %+v %+v", c.Sandbox, c.Profiles["b"])
	}
	for name, body := range map[string]string{
		"unpinned agent image": minimalConfig + "sandbox:\n  image: \"" + testDigest + "\"\n  agent_image: \"agents:latest\"\n",
		"unknown profile key":  minimalConfig + "    sandbox:\n      session: [x]\n",
		"reserved name":        minimalConfig + "    sandbox:\n      pass_env: [HOME]\n",
		"proxy name":           minimalConfig + "    sandbox:\n      pass_env: [https_proxy]\n",
		"invalid name":         minimalConfig + "    sandbox:\n      pass_env: [\"A-B\"]\n",
	} {
		if _, err := loadTestConfig(t, body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// Policy 1 contracts keep their exact bytes, so flows created under S1 still resume; an
// agent image moves the contract to policy 2 and pins the agent fields.
func TestSandboxContractPolicies(t *testing.T) {
	sb := &sandbox{cfg: SandboxConfig{Memory: "4g", CPUs: "2"}, engine: sandboxEngine{ID: "e"}, image: sandboxImage{ID: "sha256:i"}}
	got, err := sandboxSnapshot(sb)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"Policy":1,"Engine":"e","Image":"sha256:i","Memory":"4g","CPUs":"2","GateEnv":null}`; got != want {
		t.Fatalf("policy 1 snapshot\n got %s\nwant %s", got, want)
	}
	sb.agent, sb.agentEnv = sandboxImage{ID: "sha256:a"}, map[string][]string{"b": {"OPENAI_API_KEY"}}
	got, err = sandboxSnapshot(sb)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Policy       int
		AgentImage   string
		AgentNetwork string
		AgentEnv     map[string][]string
	}
	if err := json.Unmarshal([]byte(got), &c); err != nil || c.Policy != 2 || c.AgentImage != "sha256:a" || c.AgentNetwork != "bridge" || c.AgentEnv["b"][0] != "OPENAI_API_KEY" {
		t.Fatalf("policy 2 snapshot %s (%v)", got, err)
	}
	if checkPlanSandbox("p", `{"Policy":1,"Engine":"e","Image":"sha256:i","Memory":"4g","CPUs":"2","GateEnv":null}`, sb) == nil {
		t.Fatal("a policy 1 plan was accepted under policy 2")
	}
}

func TestFlowsNotSandboxedRefuseHostAgents(t *testing.T) {
	var c Config
	if err := c.hostAgents("plan"); err != nil {
		t.Fatal(err)
	}
	c.Sandbox = &SandboxConfig{Image: testDigest}
	if err := c.hostAgents("plan"); err != nil {
		t.Fatalf("policy 1 refused host agents: %v", err)
	}
	c.Sandbox.AgentImage = testDigest
	if err := c.hostAgents("plan"); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("got %v", err)
	}
}

func TestSandboxNoteNamesTheViews(t *testing.T) {
	note := sandboxNote(agentStep{Work: "c", Capture: "m", MemoryCommit: "abc"})
	for _, want := range []string{"/work (writable)", "/memory (read-only)", "abc"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if note := sandboxNote(agentStep{Work: "c"}); !strings.Contains(note, "/work (read-only)") || strings.Contains(note, "/memory") {
		t.Errorf("review note:\n%s", note)
	}
}

// agentsFixture runs the standard outcome with gates and agents in containers. Profiles
// are inline shell scripts, since no host path exists inside the container.
// extra, when given, is more profiles and then more top-level settings.
func agentsFixture(t *testing.T, agent, reviewer string, extra ...string) *fixture {
	t.Helper()
	image := testImageID(t)
	extra = append(extra, "", "")
	f := newFixture(t, map[string]string{"medio": agentNoop}, []string{"medio"}, "")
	cfg := "project: test\ncommands:\n  build: \"test ! -e .git\"\n  test: \"sh tests/acceptance/check.sh\"\n  timeout_minutes: 1\n" +
		"limits:\n  max_diff_lines: 50\nladder: [medio]\nreviewer: qa\nprofiles:\n" +
		"  medio:\n    provider: medio\n    command: [\"sh\", \"-c\", " + strconvQuote(agent) + "]\n    timeout_minutes: 2\n" +
		"  qa:\n    provider: qa\n    command: [\"sh\", \"-c\", " + strconvQuote(reviewer) + "]\n    timeout_minutes: 2\n" +
		extra[0] + "sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n" + extra[1]
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "sandboxed agents")
	return f
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestDockerSandboxedAgentsRunWithoutHostCheckouts(t *testing.T) {
	agent := `test ! -e .git && test "$(pwd)" = /work && head -1 /run/aeh/prompt.md | grep -q isolated && mkdir -p src && echo "fees ok" > src/fees.txt`
	reviewer := `test "$(cat /work/src/fees.txt)" = "fees ok" || exit 3; if (echo x > /work/src/fees.txt) 2>/dev/null; then echo WRITABLE; exit 4; fi; echo "VERDICT: APPROVE"`
	f := agentsFixture(t, agent, reviewer)
	st, log := f.run()
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
	if list := mustGit(t, f.dir, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("host worktrees were created:\n%s", list)
	}
	if blob, _ := gitBlob(f.dir, IntegrationBranch("OUT-001"), "src/fees.txt"); blob != "fees ok\n" {
		t.Fatalf("integrated src/fees.txt %q", blob)
	}
	attempt := filepath.Join(f.dir, filepath.FromSlash(st.Tasks[0].Attempts[0].RunDir))
	for _, name := range []string{"agent.launch.json", "review-qa.launch.json"} {
		b, err := os.ReadFile(filepath.Join(attempt, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var rec launchRecord
		if err := json.Unmarshal(b, &rec); err != nil || rec.Class != ClassProcess || rec.Network != "bridge" || rec.Image == "" {
			t.Fatalf("%s: %s", name, b)
		}
		if (name == "agent.launch.json") != rec.WorkWritable {
			t.Fatalf("%s writable=%v", name, rec.WorkWritable)
		}
	}
	for _, g := range readGateResults(f.dir, st.FinalRun) {
		if (g.Name == "build" || g.Name == "test") && !g.Sandboxed {
			t.Fatalf("final %s ran on the host", g.Name)
		}
	}
}

// An agent that leaves a symbolic link fails its attempt like any other agent failure: the
// ladder moves on instead of blocking on infrastructure.
func TestDockerSandboxedAgentContentRefusalSpendsTheAttempt(t *testing.T) {
	f := agentsFixture(t, `ln -s /etc/passwd leak`, `echo "VERDICT: APPROVE"`)
	st, log := f.run()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "failed") || strings.Contains(st.Question, "Confirm all processes stopped") {
		t.Fatalf("status %s, question %q\n%s", st.Status, st.Question, log)
	}
	if !strings.Contains(log, "leak") {
		t.Fatalf("refused path not reported:\n%s", log)
	}
}

func TestDockerSandboxCheckProbesTheAgentImage(t *testing.T) {
	image := testImageID(t)
	r := Repo{Root: t.TempDir()}
	mustGit(t, r.Root, "init", "-q")
	write(t, r.Root, "aeh/config.yaml", "project: t\ncommands:\n  test: \"true\"\nladder: [a]\nreviewer: b\nprofiles:\n"+
		"  a:\n    provider: x\n    command: [sh, -c, \"true\"]\n  b:\n    provider: y\n    command: [no-such-agent-cli]\n"+
		"sandbox:\n  image: \""+image+"\"\n  agent_image: \""+image+"\"\n")
	var out strings.Builder
	if err := SandboxCheck(context.Background(), r, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"agents    " + image, "no-such-agent-cli (profiles b) was not found in the agent image", "sandbox ready"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sh (profiles") {
		t.Errorf("sh reported missing:\n%s", text)
	}
}
