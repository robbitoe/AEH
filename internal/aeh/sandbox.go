package aeh

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robbitoe/aeh/internal/egress"
	"gopkg.in/yaml.v3"
)

// SandboxConfig is the optional sandbox section of aeh/config.yaml. Without it, gates and
// agents run on the host as before (sandbox policy version 0).
type SandboxConfig struct {
	Image   string            `yaml:"image"`
	Memory  string            `yaml:"memory"`
	CPUs    string            `yaml:"cpus"`
	GateEnv map[string]string `yaml:"gate_env"`
	// AgentImage, when set, runs every agent in a container from this image (stage 6,
	// S2, sandbox policy version 2). Without it, only gate commands are sandboxed.
	AgentImage string `yaml:"agent_image"`
}

// UnmarshalYAML rejects misspelled sandbox settings.
func (s *SandboxConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("sandbox must be a mapping of image, agent_image, memory, cpus and gate_env")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "image", "agent_image", "memory", "cpus", "gate_env":
		default:
			return fmt.Errorf("sandbox: unknown key %q", k)
		}
	}
	type plain SandboxConfig
	return n.Decode((*plain)(s))
}

var (
	pinnedImage   = regexp.MustCompile(`^([^@\s]+@)?sha256:[0-9a-f]{64}$`)
	memoryLimit   = regexp.MustCompile(`^[1-9][0-9]*[kmg]?$`)
	cpuLimit      = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	envNamePlain  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sandboxLabels = []string{"aeh.project", "aeh.launch", "aeh.kind", "aeh.role"}
)

// proxyVars are set explicitly empty in every container, so a proxy configured for the
// Docker client (which may hold credentials) is never injected into a launch.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "ftp_proxy", "all_proxy", "no_proxy"}

// reservedSandboxEnv cannot be set through gate_env: aeh sets them itself.
var reservedSandboxEnv = map[string]bool{"HOME": true, "TMPDIR": true}

func (s SandboxConfig) withDefaults() SandboxConfig {
	if s.Memory == "" {
		s.Memory = "4g"
	}
	if s.CPUs == "" {
		s.CPUs = "2"
	}
	return s
}

func (s SandboxConfig) validate() error {
	s = s.withDefaults()
	switch {
	case !pinnedImage.MatchString(s.Image):
		return errors.New("config: sandbox.image must be pinned: name@sha256:<digest> or sha256:<image id>")
	case s.AgentImage != "" && !pinnedImage.MatchString(s.AgentImage):
		return errors.New("config: sandbox.agent_image must be pinned: name@sha256:<digest> or sha256:<image id>")
	case !memoryLimit.MatchString(s.Memory):
		return errors.New("config: sandbox.memory must be a number with an optional k, m or g suffix, for example 4g")
	case !cpuLimit.MatchString(s.CPUs) || strings.Trim(s.CPUs, "0.") == "":
		return errors.New("config: sandbox.cpus must be a positive number, for example 2 or 1.5")
	}
	for name, value := range s.GateEnv {
		upper := strings.ToUpper(name)
		switch {
		case !envNamePlain.MatchString(name):
			return fmt.Errorf("config: sandbox.gate_env: invalid variable name %q", name)
		case reservedSandboxEnv[upper]:
			return fmt.Errorf("config: sandbox.gate_env: %s is set by aeh", name)
		case isProxyVar(name):
			return fmt.Errorf("config: sandbox.gate_env: %s is not allowed: launches have no proxy", name)
		case strings.ContainsAny(value, "\x00\r\n"):
			return fmt.Errorf("config: sandbox.gate_env: %s contains a newline or NUL", name)
		}
	}
	return nil
}

// checkAgentEnvName refuses a variable an agent profile cannot set or forward inside the
// sandbox: aeh sets HOME, TMPDIR and the proxy variables itself.
func checkAgentEnvName(name string) error {
	switch {
	case !envNamePlain.MatchString(name):
		return fmt.Errorf("invalid variable name %q", name)
	case reservedSandboxEnv[strings.ToUpper(name)] || isProxyVar(name):
		return fmt.Errorf("%s is set by aeh inside the sandbox", name)
	}
	return nil
}

func isProxyVar(name string) bool {
	for _, p := range proxyVars {
		if name == p || strings.EqualFold(name, p) {
			return true
		}
	}
	return false
}

// LaunchResult classifies how a sandboxed (or host) process ended. Only class process
// proves a red or a green; the others are honest about not knowing.
type LaunchResult struct {
	Class    string `json:"class"`
	ExitCode int    `json:"exit_code"`
	Reason   string `json:"reason,omitempty"`
}

const (
	ClassProcess        = "process"        // the process started and exited by itself
	ClassInfrastructure = "infrastructure" // the runtime failed; nothing ran or nothing is known
	ClassResource       = "resource"       // the kernel killed the process for memory
	ClassTerminated     = "terminated"     // timeout, cancellation or lost connection
)

// dockerEnvNames are the variables the Docker client needs besides baseEnv.
var dockerEnvNames = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// dockerCLI runs the Docker client with a filtered environment.
type dockerCLI struct{ env []string }

func newDockerCLI() dockerCLI { return dockerCLI{env: commandEnv(dockerEnvNames)} }

// run executes one docker command. stdin may be nil; stdout, when given, receives the
// command's standard output, otherwise it is returned (trimmed) with stderr on failure.
func (d dockerCLI) run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = d.env
	cmd.Stdin = stdin
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &out
	}
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 2000 {
			msg = msg[:2000] + "..."
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("docker %s: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// attach runs one docker command on the terminal of an interactive launch. The process
// gets the terminal's own file as stdin, so the client can put it in raw mode.
//
// When ctx ends, the container is killed rather than the client: the attachment then
// ends normally and the client restores the terminal it put in raw mode. The client is
// killed only if it does not exit after that.
func (d dockerCLI) attach(ctx context.Context, t *interactiveIO, cid string) error {
	cmd := exec.Command("docker", "start", "--attach", "--interactive", cid)
	cmd.Env = d.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = t.In, t.Out, t.Err
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker start: %w", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			// The kill has its own deadline and runs beside the watchdog, so a stalled
			// daemon delays the client's exit by 15 seconds at most.
			go func() {
				kctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = d.run(kctx, nil, nil, "kill", cid)
			}()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	if err != nil {
		return fmt.Errorf("docker start: %w", err)
	}
	return nil
}

// stream runs one docker command and hands its standard output to fn as it arrives. The
// reader is the process's own pipe, so a slow consumer (the capture hashes as it reads)
// only slows the command down: no copy deadline runs after the process exits. When fn
// fails, the command is stopped; when it succeeds, the rest of the output is drained
// before the exit status is read.
func (d dockerCLI) stream(ctx context.Context, fn func(io.Reader) error, args ...string) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, "docker", args...)
	cmd.Env = d.env
	cmd.WaitDelay = 5 * time.Second
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	ferr := fn(out)
	if ferr == nil {
		// What follows the parsed archive is its padding; anything large is not.
		const tailLimit = 1 << 20
		n, err := io.Copy(io.Discard, io.LimitReader(out, tailLimit+1))
		switch {
		case err != nil:
			ferr = err
		case n > tailLimit:
			ferr = errors.New("unexpected output after the archive")
		}
	}
	if ferr != nil {
		cancel()
	}
	werr := cmd.Wait()
	switch {
	case ferr != nil:
		return ferr
	case ctx.Err() != nil:
		return fmt.Errorf("docker %s: %w", args[0], ctx.Err())
	case werr != nil:
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 2000 {
			msg = msg[:2000] + "..."
		}
		return fmt.Errorf("docker %s: %v: %s", args[0], werr, msg)
	}
	return nil
}

// isolatedDockerCLI talks to the resolved local endpoint with a runner-private, empty
// client configuration. The user's Docker configuration can define proxies (possibly
// with credentials) that the client injects into every container it creates, even over
// variables set empty; a fresh configuration has none. No pull happens, so no registry
// credentials are needed.
func isolatedDockerCLI(dir, endpoint string) (dockerCLI, error) {
	cfgDir := filepath.Join(dir, "docker-config")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return dockerCLI{}, fmt.Errorf("sandbox docker config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{}\n"), 0o600); err != nil {
		return dockerCLI{}, fmt.Errorf("sandbox docker config: %w", err)
	}
	env := append(commandEnv(nil), "DOCKER_HOST="+endpoint, "DOCKER_CONFIG="+cfgDir)
	return dockerCLI{env: env}, nil
}

// admin runs a short management command with its own bounded context.
func (d dockerCLI) admin(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return d.run(ctx, nil, nil, args...)
}

// sandboxEngine describes the local Linux engine a sandbox uses.
type sandboxEngine struct {
	Endpoint string `json:"endpoint"`
	ID       string `json:"id"`
	Version  string `json:"version"`
}

const minDockerMajor = 24

// localEngine checks that the Docker client points at a local Linux engine.
// localEndpoint resolves the endpoint the user's Docker client would use (DOCKER_HOST,
// DOCKER_CONTEXT or the current context) and accepts only a local one: a Unix socket, or
// a Windows named pipe on this machine (npipe:////./pipe/...). A pipe on another server
// (npipe:////server/pipe/...) is remote.
func localEndpoint(user dockerCLI) (string, error) {
	host, err := user.admin("context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return "", fmt.Errorf("sandbox: find the Docker endpoint: %w", err)
	}
	if err := checkLocalEndpoint(host); err != nil {
		return "", err
	}
	return host, nil
}

func checkLocalEndpoint(host string) error {
	if p, ok := strings.CutPrefix(host, "unix://"); ok && strings.HasPrefix(p, "/") {
		return nil
	}
	if p, ok := strings.CutPrefix(host, "npipe://"); ok {
		p = strings.ReplaceAll(p, `\`, "/")
		if rest, ok := strings.CutPrefix(p, "//./pipe/"); ok && rest != "" && !strings.Contains(rest, "/") {
			return nil
		}
	}
	return fmt.Errorf("sandbox: Docker endpoint %q is not local; only a Unix socket or a named pipe on this machine (npipe:////./pipe/...) is supported", host)
}

// localEngine checks the engine behind an already validated endpoint, through the same
// client that every later command uses.
func localEngine(d dockerCLI, endpoint string) (sandboxEngine, error) {
	e := sandboxEngine{Endpoint: endpoint}
	out, err := d.admin("info", "--format", "{{json .ID}} {{json .OSType}} {{json .ServerVersion}}")
	if err != nil {
		return e, fmt.Errorf("sandbox: Docker engine unreachable (is Docker running?): %w", err)
	}
	var osType string
	fields := strings.Fields(out)
	if len(fields) != 3 || json.Unmarshal([]byte(fields[0]), &e.ID) != nil || json.Unmarshal([]byte(fields[1]), &osType) != nil || json.Unmarshal([]byte(fields[2]), &e.Version) != nil {
		return e, fmt.Errorf("sandbox: unexpected docker info output %q", out)
	}
	if osType != "linux" {
		return e, fmt.Errorf("sandbox: the Docker engine runs %s containers; aeh needs Linux containers", osType)
	}
	var major int
	if _, err := fmt.Sscanf(e.Version, "%d", &major); err != nil || major < minDockerMajor {
		return e, fmt.Errorf("sandbox: Docker engine %s is too old; aeh needs %d or later", e.Version, minDockerMajor)
	}
	return e, nil
}

// sandboxImage is a resolved image: its ID and the environment it defines.
type sandboxImage struct {
	ID  string
	Env []string
}

func resolveImage(d dockerCLI, ref string) (sandboxImage, error) {
	out, err := d.admin("image", "inspect", "--format", "{{json .Id}} {{json .Config.Env}}", ref)
	if err != nil {
		return sandboxImage{}, fmt.Errorf("sandbox: image %s is not present locally (aeh never pulls images): %w", ref, err)
	}
	id, env, ok := strings.Cut(out, " ")
	var img sandboxImage
	if !ok || json.Unmarshal([]byte(id), &img.ID) != nil || json.Unmarshal([]byte(env), &img.Env) != nil {
		return img, fmt.Errorf("sandbox: unexpected image inspect output for %s", ref)
	}
	if strings.HasPrefix(ref, "sha256:") && img.ID != ref {
		return img, fmt.Errorf("sandbox: image %s resolved to %s", ref, img.ID)
	}
	return img, nil
}

// sandbox runs launches for one flow of one project.
type sandbox struct {
	cfg    SandboxConfig
	d      dockerCLI
	engine sandboxEngine
	image  sandboxImage // gates
	agent  sandboxImage // agents (S2); empty until an agent image is configured
	// agentEnv is each profile's sandbox.pass_env, pinned with the policy: which host
	// values reach an agent is an isolation field.
	agentEnv map[string][]string
	// agentSession is each profile's sandbox.session, pinned the same way.
	agentSession map[string][]string
	// agentEgress is each profile's sandbox.egress, pinned the same way.
	agentEgress map[string][]string
	project     string // label: first 12 hex digits of sha256(project root)
	dir         string // runner-private directory: .aeh/sandbox
	flow        string // intents of this flow live in dir/intents/<flow>
}

// root is the project checkout whose .aeh/sandbox the sandbox uses.
func (s *sandbox) root() string { return filepath.Dir(filepath.Dir(s.dir)) }

func projectLabel(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(sum[:])[:12]
}

var flowKeyUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// openSandbox checks the engine and image, then removes what an interrupted launch of
// the same flow left behind. Removal must be confirmed before the flow continues.
func openSandbox(r Repo, cfg SandboxConfig, flow string) (*sandbox, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &sandbox{cfg: cfg.withDefaults(), d: newDockerCLI(), project: projectLabel(r.Root),
		dir: filepath.Join(r.Root, ".aeh", "sandbox"), flow: flowKeyUnsafe.ReplaceAllString(flow, "_")}
	endpoint, err := localEndpoint(s.d)
	if err != nil {
		return nil, err
	}
	// From here on every command, info included, goes to the validated endpoint only.
	if s.d, err = isolatedDockerCLI(s.dir, endpoint); err != nil {
		return nil, err
	}
	if s.engine, err = localEngine(s.d, endpoint); err != nil {
		return nil, err
	}
	if s.image, err = resolveImage(s.d, s.cfg.Image); err != nil {
		return nil, err
	}
	if s.cfg.AgentImage != "" {
		if s.agent, err = resolveImage(s.d, s.cfg.AgentImage); err != nil {
			return nil, err
		}
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// launchIntent is persisted before any Docker object of a launch exists, so recovery
// finds every container and volume by labels even when an answer from Docker was lost.
type launchIntent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Project   string    `json:"project"`
	Created   time.Time `json:"created"`
	Container string    `json:"container,omitempty"`
}

func newLaunchID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("launch id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *sandbox) intentDir() string { return filepath.Join(s.dir, "intents", s.flow) }

func (s *sandbox) saveIntent(in launchIntent) error {
	if err := os.MkdirAll(s.intentDir(), 0o700); err != nil {
		return fmt.Errorf("sandbox intent: %w", err)
	}
	return writePrivateJSON(filepath.Join(s.intentDir(), in.ID+".json"), in)
}

// newIntent records a launch before anything is created.
func (s *sandbox) newIntent(kind string) (launchIntent, error) {
	id, err := newLaunchID()
	if err != nil {
		return launchIntent{}, err
	}
	in := launchIntent{ID: id, Kind: kind, Project: s.project, Created: time.Now().UTC()}
	return in, s.saveIntent(in)
}

func (s *sandbox) labels(launch, kind, role string) []string {
	return []string{"--label", "aeh.project=" + s.project, "--label", "aeh.launch=" + launch,
		"--label", "aeh.kind=" + kind, "--label", "aeh.role=" + role}
}

// release removes every container and volume of a launch and confirms with a second
// lookup. The intent is deleted only after the confirmation.
func (s *sandbox) release(id string) error {
	filters := []string{"--filter", "label=aeh.project=" + s.project, "--filter", "label=aeh.launch=" + id}
	containers, err := s.d.admin(append([]string{"ps", "-a", "-q", "--no-trunc"}, filters...)...)
	if err != nil {
		return fmt.Errorf("sandbox cleanup %s: %w", id, err)
	}
	if ids := strings.Fields(containers); len(ids) > 0 {
		if _, err := s.d.admin(append([]string{"rm", "-f", "-v"}, ids...)...); err != nil {
			return fmt.Errorf("sandbox cleanup %s: %w", id, err)
		}
	}
	volumes, err := s.d.admin(append([]string{"volume", "ls", "-q"}, filters...)...)
	if err != nil {
		return fmt.Errorf("sandbox cleanup %s: %w", id, err)
	}
	if names := strings.Fields(volumes); len(names) > 0 {
		if _, err := s.d.admin(append([]string{"volume", "rm", "-f"}, names...)...); err != nil {
			return fmt.Errorf("sandbox cleanup %s: %w", id, err)
		}
	}
	left, err := s.d.admin(append([]string{"ps", "-a", "-q"}, filters...)...)
	if err == nil {
		var vols string
		vols, err = s.d.admin(append([]string{"volume", "ls", "-q"}, filters...)...)
		left += vols
	}
	if err != nil || strings.TrimSpace(left) != "" {
		return fmt.Errorf("sandbox cleanup %s: removal not confirmed; Docker objects labeled aeh.launch=%s remain: %v", id, id, err)
	}
	// Launch-local files go with the Docker objects; the intent is the last to go.
	for _, name := range []string{id + ".env", id + "-egress.env"} {
		if err := os.Remove(filepath.Join(s.dir, "env", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sandbox cleanup %s: %w", id, err)
		}
	}
	if err := os.Remove(filepath.Join(s.intentDir(), id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox cleanup %s: %w", id, err)
	}
	return nil
}

// recover releases every launch an interrupted run of this flow recorded.
func (s *sandbox) recover() error {
	entries, err := os.ReadDir(s.intentDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox recovery: %w", err)
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		if err := s.release(id); err != nil {
			return fmt.Errorf("sandbox recovery: %w", err)
		}
	}
	return nil
}

// sandboxVolume is one mount of a launch. A volume with a Fill is created, filled
// through a loader container and removed with the launch; one with a Name already
// exists (a verification sequence's work volume) and outlives the launch.
type sandboxVolume struct {
	Name     string
	Target   string
	ReadOnly bool
	Fill     tarSource
	// Private fills with directories 0700 and files 0600 (a HOME with session files).
	Private bool
}

// interactiveIO attaches a launch to a terminal (aeh sandbox login): the container gets
// stdin, and a TTY when the input is one.
type interactiveIO struct {
	In       *os.File
	Out, Err io.Writer
	TTY      bool
}

// sandboxLaunch describes one container. The caller decides every view; nothing is
// inferred from a profile name.
type sandboxLaunch struct {
	Kind    string
	Argv    []string
	Volumes []sandboxVolume
	Env     []string // NAME=value, beyond HOME, TMPDIR and the empty proxy variables
	Workdir string
	Pids    bool // agents get a PIDs limit; gates do not
	Timeout time.Duration
	Output  io.Writer // stdout, and stderr too unless Stderr is set
	Stderr  io.Writer
	// Agent selects the agent image. Gates run in the gate image.
	Agent bool
	// Network is Docker's network mode; empty means none. Only an egress proxy has one.
	Network string
	// Egress lists the host:port pairs the launch may reach (S3). When set, a proxy
	// container with the network serves them on a Unix socket in a volume shared with
	// the launch, which keeps no network; aeh-egress launch relays its loopback port
	// 3128 to that socket and then runs Argv. EgressLog receives the proxy's log.
	Egress    []string
	EgressLog io.Writer
	// Interactive attaches the launch to a terminal instead of collecting its output.
	Interactive *interactiveIO
	// SaveSession runs while the stopped container still exists, before after, when the
	// process exited by itself: it reads the profile's session files back from HOME. It
	// reports its own problems; it never changes the launch's result.
	SaveSession func(container string, res LaunchResult)
}

// homeVolume reports whether the launch mounts a volume at HOME (a profile with a
// session), which replaces the HOME tmpfs.
func (l sandboxLaunch) homeVolume() bool {
	for _, v := range l.Volumes {
		if v.Target == sandboxHome {
			return true
		}
	}
	return false
}

// launchImage and launchNetwork are what a launch must run with.
func (s *sandbox) launchImage(l sandboxLaunch) sandboxImage {
	if l.Agent {
		return s.agent
	}
	return s.image
}

func launchNetwork(l sandboxLaunch) string {
	if l.Network != "" {
		return l.Network
	}
	return "none"
}

// containerState is the part of docker inspect aeh checks.
type containerState struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		User       string   `json:"User"`
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode    string            `json:"NetworkMode"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		Privileged     bool              `json:"Privileged"`
		CapDrop        []string          `json:"CapDrop"`
		CapAdd         []string          `json:"CapAdd"`
		Binds          []string          `json:"Binds"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		LogConfig      struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	State struct {
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
}

func (s *sandbox) inspect(id string) (containerState, error) {
	var st []containerState
	out, err := s.d.admin("inspect", "--type", "container", id)
	if err != nil {
		return containerState{}, err
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || len(st) != 1 {
		return containerState{}, fmt.Errorf("docker inspect %s: unexpected output", id)
	}
	return st[0], nil
}

const (
	sandboxUser = "10001:10001"
	sandboxHome = "/home/aeh"
	// sandboxLogKeep is a log size no launch reaches: Docker never rotates the output away,
	// and a daemon default rotation is overridden. Disk use during a step is out of model.
	sandboxLogKeep = "1024g"
)

// launchEnv is the environment a container must end up with: the image's, then
// HOME, TMPDIR, the proxy variables and the launch's own. The proxy variables are empty,
// or name the launch's forwarder when it has egress.
func (s *sandbox) launchEnv(img sandboxImage, extra []string, proxied bool) ([]string, map[string]string, error) {
	want := map[string]string{}
	for _, kv := range img.Env {
		k, v, _ := strings.Cut(kv, "=")
		want[k] = v
	}
	own := []string{"HOME=" + sandboxHome, "TMPDIR=/tmp"}
	for _, p := range proxyVars {
		own = append(own, p+"="+proxyValue(p, proxied))
	}
	own = append(own, extra...)
	for _, kv := range own {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !envNamePlain.MatchString(k) || strings.ContainsAny(v, "\x00\r\n") {
			return nil, nil, fmt.Errorf("sandbox: invalid environment entry for %q", k)
		}
		want[k] = v
	}
	return own, want, nil
}

// checkCreated compares the created container with the launch before it starts.
func (s *sandbox) checkCreated(c containerState, l sandboxLaunch, vols []sandboxVolume, want map[string]string) error {
	var problems []string
	if c.Image != s.launchImage(l).ID {
		problems = append(problems, "image "+c.Image)
	}
	if c.Config.WorkingDir != l.Workdir || len(c.Config.Entrypoint) != 1 || c.Config.Entrypoint[0] != l.Argv[0] {
		problems = append(problems, "entrypoint or working directory")
	}
	if c.Config.User != sandboxUser {
		problems = append(problems, "user "+c.Config.User)
	}
	if c.HostConfig.NetworkMode != launchNetwork(l) {
		problems = append(problems, "network "+c.HostConfig.NetworkMode)
	}
	if !c.HostConfig.ReadonlyRootfs || c.HostConfig.Privileged || len(c.HostConfig.CapAdd) > 0 || len(c.HostConfig.Binds) > 0 {
		problems = append(problems, "root filesystem, privileges or binds")
	}
	if len(c.HostConfig.CapDrop) != 1 || !strings.EqualFold(c.HostConfig.CapDrop[0], "ALL") {
		problems = append(problems, "capabilities not dropped")
	}
	if !slicesContainsPrefix(c.HostConfig.SecurityOpt, "no-new-privileges") {
		problems = append(problems, "no-new-privileges missing")
	}
	if p := checkLogConfig(c.HostConfig.LogConfig.Type, c.HostConfig.LogConfig.Config); p != "" {
		problems = append(problems, p)
	}
	got := map[string]string{}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, v := range got {
		if w, ok := want[k]; !ok {
			problems = append(problems, "unexpected variable "+k)
		} else if w != v {
			problems = append(problems, "variable "+k+" has an unexpected value")
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			problems = append(problems, "missing variable "+k)
		}
	}
	expected := map[string]sandboxVolume{}
	for _, v := range vols {
		expected[v.Target] = v
	}
	if len(c.Mounts) != len(vols) {
		problems = append(problems, fmt.Sprintf("%d mounts, expected %d", len(c.Mounts), len(vols)))
	}
	for _, m := range c.Mounts {
		v, ok := expected[m.Destination]
		if !ok || m.Type != "volume" || m.Name != v.Name || m.RW == v.ReadOnly {
			problems = append(problems, "unexpected mount at "+m.Destination)
		}
	}
	for target := range c.HostConfig.Tmpfs {
		if target != "/tmp" && (target != sandboxHome || l.homeVolume()) {
			problems = append(problems, "unexpected tmpfs at "+target)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("sandbox: created container does not match the launch: %s", strings.Join(problems, "; "))
	}
	return nil
}

func slicesContainsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// createVolume makes a labeled volume that belongs to launch id.
func (s *sandbox) createVolume(id, kind string, n int) (string, error) {
	name := fmt.Sprintf("aeh-%s-%s-%d", s.project, id[:16], n)
	args := append([]string{"volume", "create"}, s.labels(id, kind, "volume")...)
	if _, err := s.d.admin(append(args, name)...); err != nil {
		return "", err
	}
	return name, nil
}

// fill copies each volume's tree through a loader container that is created, never
// started (no product code runs), then removed.
func (s *sandbox) fill(ctx context.Context, id, kind, image string, vols []sandboxVolume) error {
	args := append([]string{"create", "--pull=never", "--network", "none"}, s.labels(id, kind, "loader")...)
	n := 0
	for _, v := range vols {
		if v.Fill != nil {
			args = append(args, "--mount", "type=volume,src="+v.Name+",dst="+v.Target+",volume-nocopy")
			n++
		}
	}
	if n == 0 {
		return nil
	}
	loader, err := s.d.admin(append(args, "--entrypoint", "true", image)...)
	if err != nil {
		return err
	}
	defer s.d.admin("rm", "-f", "-v", loader) //nolint:errcheck // release confirms
	for _, v := range vols {
		if v.Fill == nil {
			continue
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeVolumeTarMode(pw, filepath.Base(v.Target), v.Fill, v.Private)) }()
		_, err := s.d.run(ctx, pr, nil, "cp", "-a", "-", loader+":"+parentDir(v.Target))
		pr.CloseWithError(err)
		if err != nil {
			return fmt.Errorf("fill %s: %w", v.Target, err)
		}
	}
	return nil
}

func parentDir(target string) string {
	i := strings.LastIndex(target, "/")
	if i <= 0 {
		return "/"
	}
	return target[:i]
}

// prepare creates and fills the launch's own volumes. Volumes with a name are reused.
func (s *sandbox) prepare(ctx context.Context, id, kind, image string, vols []sandboxVolume) ([]sandboxVolume, error) {
	out := append([]sandboxVolume(nil), vols...)
	for i := range out {
		if !strings.HasPrefix(out[i].Target, "/") || strings.Contains(out[i].Target, "..") || strings.ContainsAny(out[i].Target, ",=") {
			return nil, fmt.Errorf("sandbox: invalid mount target %q", out[i].Target)
		}
		if out[i].Name != "" {
			continue
		}
		name, err := s.createVolume(id, kind, i)
		if err != nil {
			return nil, err
		}
		out[i].Name = name
	}
	return out, s.fill(ctx, id, kind, image, out)
}

// run executes one launch: intent, volumes, create, check, start, inspect. after, when
// not nil, runs while the stopped container still exists (to read volumes back). Every
// Docker object of the launch is removed before run returns; a failed removal is an
// error, and the intent stays for recovery.
func (s *sandbox) run(ctx context.Context, l sandboxLaunch, after func(container string) error) (res LaunchResult, err error) {
	in, err := s.newIntent(l.Kind)
	if err != nil {
		return LaunchResult{Class: ClassInfrastructure, Reason: err.Error()}, err
	}
	defer func() {
		if rerr := s.release(in.ID); rerr != nil && err == nil {
			err = rerr
		}
	}()
	infra := func(e error) (LaunchResult, error) {
		return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: e.Error()}, nil
	}
	img := s.launchImage(l)
	if img.ID == "" {
		return infra(errors.New("sandbox: no agent image is configured"))
	}
	vols, err := s.prepare(ctx, in.ID, l.Kind, img.ID, l.Volumes)
	if err != nil {
		return infra(err)
	}
	var proxy string // the egress proxy's container
	if len(l.Egress) > 0 {
		sock, cid, err := s.startEgress(ctx, in.ID, l.Kind, l.Egress)
		if err != nil {
			return infra(fmt.Errorf("egress proxy: %w", err))
		}
		proxy = cid
		vols = append(vols, sandboxVolume{Name: sock, Target: egressDir, ReadOnly: true})
		l.Argv = append([]string{"aeh-egress", "launch", "--socket", egressSocket, "--listen", egressListen, "--"}, l.Argv...)
	}
	own, want, err := s.launchEnv(img, l.Env, proxy != "")
	if err != nil {
		return infra(err)
	}
	envFile, err := s.writeEnvFile(in.ID, own)
	if err != nil {
		return infra(err)
	}
	defer os.Remove(envFile)
	if len(l.Argv) == 0 || l.Workdir == "" {
		return infra(errors.New("sandbox: launch needs a program and a working directory"))
	}
	args := append([]string{"create", "--pull=never"}, s.labels(in.ID, l.Kind, "consumer")...)
	args = append(args, "--network", launchNetwork(l), "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", s.cfg.Memory, "--memory-swap", s.cfg.Memory, "--cpus", s.cfg.CPUs, "--read-only",
		"--tmpfs", "/tmp:mode=1777",
		"--user", sandboxUser, "--workdir", l.Workdir, "--env-file", envFile, "--entrypoint", l.Argv[0],
		"--log-driver", "json-file", "--log-opt", "max-size="+sandboxLogKeep)
	if !l.homeVolume() {
		// HOME is disposable; a profile with a session gets a volume instead, read back
		// after the process exits.
		args = append(args, "--tmpfs", sandboxHome+":uid=10001,gid=10001,mode=0700")
	}
	if l.Pids {
		args = append(args, "--pids-limit", "512")
	}
	if l.Interactive != nil {
		args = append(args, "--interactive")
		if l.Interactive.TTY {
			args = append(args, "--tty")
		}
	}
	for _, v := range vols {
		// volume-nocopy: Docker would otherwise copy the image's directory at the mount
		// point (content, owner and mode) into a volume that looks empty, such as an
		// agent's /out that is also the working directory.
		m := "type=volume,src=" + v.Name + ",dst=" + v.Target + ",volume-nocopy"
		if v.ReadOnly {
			m += ",readonly"
		}
		args = append(args, "--mount", m)
	}
	args = append(append(args, img.ID), l.Argv[1:]...)
	cid, err := s.d.admin(args...)
	if err != nil {
		return infra(err)
	}
	in.Container = cid
	if err := s.saveIntent(in); err != nil {
		return infra(err)
	}
	created, err := s.inspect(cid)
	if err != nil {
		return infra(err)
	}
	if err := s.checkCreated(created, l, vols, want); err != nil {
		return infra(err)
	}
	runCtx := ctx
	if l.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, l.Timeout)
		defer cancel()
	}
	// No attach: the status comes from docker wait and inspect, the output from docker
	// logs afterwards. Each has its own error, so a failure to stream or store the output
	// can never pass for the container's result.
	if l.Interactive != nil {
		// The terminal is attached until the process exits; its status still comes from
		// docker wait and inspect. A container that never started is a runtime failure.
		if err := s.d.attach(runCtx, l.Interactive, cid); err != nil && runCtx.Err() == nil {
			if st, ierr := s.inspect(cid); ierr != nil || st.State.Status == "created" {
				return infra(err)
			}
		}
	} else if _, err := s.d.run(runCtx, nil, nil, "start", cid); err != nil && runCtx.Err() == nil {
		return infra(err)
	}
	waited, waitErr := s.d.run(runCtx, nil, nil, "wait", cid)
	if runCtx.Err() != nil {
		// The container may still run. Stop it before anything else.
		_, _ = s.d.admin("kill", cid)
		reason := "cancelled"
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			reason = fmt.Sprintf("timed out after %s", l.Timeout)
		}
		if proxy != "" {
			if err := s.endEgress(proxy, l.EgressLog); err != nil {
				reason += " (egress proxy: " + err.Error() + ")"
			}
		}
		return LaunchResult{Class: ClassTerminated, ExitCode: -1, Reason: reason}, nil
	}
	st, err := s.inspect(cid)
	if err != nil {
		return LaunchResult{Class: ClassTerminated, ExitCode: -1, Reason: "lost the container state: " + err.Error()}, nil
	}
	res = classify(st)
	if msg := waitMismatch(waited, waitErr, res); msg != "" {
		return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: msg}, nil
	}
	if proxy != "" {
		// The proxy must have served the whole launch: one that stopped leaves the
		// agent's failures unexplained. The launcher's own exit code says it could not
		// run the program behind the forwarder.
		var problem error
		if err := s.endEgress(proxy, l.EgressLog); err != nil {
			problem = fmt.Errorf("egress proxy: %w", err)
		} else if res.Class == ClassProcess && res.ExitCode == egress.LauncherFailed {
			problem = fmt.Errorf("the egress launcher failed (exit %d); its reason is in the launch's stderr", egress.LauncherFailed)
		}
		if problem != nil {
			// The output still explains what happened; the result stays infrastructure.
			if l.Output != nil {
				_ = s.logs(ctx, cid, l.Output, l.Stderr)
			}
			return infra(problem)
		}
	}
	if l.SaveSession != nil && res.Class == ClassProcess {
		l.SaveSession(cid, res)
	}
	if l.Output != nil {
		if err := s.logs(ctx, cid, l.Output, l.Stderr); err != nil {
			return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: "could not read the container output: " + err.Error()}, nil
		}
	}
	if after != nil && (res.Class == ClassProcess || res.Class == ClassResource) {
		if err := after(cid); err != nil {
			return infra(err)
		}
	}
	return res, nil
}

// waitMismatch compares docker wait's answer with the inspected state. A failed wait, or
// an exit code that differs from the inspected one, leaves the result unproven.
func waitMismatch(waited string, waitErr error, res LaunchResult) string {
	if res.Class != ClassProcess && res.Class != ClassResource {
		return ""
	}
	if waitErr != nil {
		return "docker wait failed: " + waitErr.Error()
	}
	if code, err := strconv.Atoi(strings.TrimSpace(waited)); err != nil || code != res.ExitCode {
		return fmt.Sprintf("docker wait reported %q but the container exited %d", waited, res.ExitCode)
	}
	return ""
}

// errWriter records the first write error and keeps accepting bytes, so the copy from a
// subprocess never hides it (os/exec drops copy errors when the process fails). It is
// safe for the two copy goroutines of stdout and stderr; last keeps the final bytes for
// error messages.
type errWriter struct {
	mu   sync.Mutex
	w    io.Writer
	err  error
	last []byte
}

func (e *errWriter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err == nil {
		if _, err := e.w.Write(p); err != nil {
			e.err = err
		}
	}
	e.last = append(e.last, p...)
	if len(e.last) > 512 {
		e.last = append(e.last[:0], e.last[len(e.last)-512:]...)
	}
	return len(p), nil
}

// logs copies a stopped container's stdout to w and its stderr to errw, or to w too when
// errw is nil. Any error of the client or of a writer is returned; the caller then treats
// the launch as unproven.
func (s *sandbox) logs(ctx context.Context, cid string, w, errw io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	ew := &errWriter{w: w}
	ee := ew
	if errw != nil {
		ee = &errWriter{w: errw}
	}
	cmd := exec.CommandContext(ctx, "docker", "logs", cid)
	cmd.Env, cmd.Stdout, cmd.Stderr = s.d.env, ew, ee
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker logs: %v: %s", err, redactSecrets(string(ee.last)))
	}
	if ew.err != nil {
		return ew.err
	}
	return ee.err
}

// unset reports a Docker timestamp that was never set.
func unset(ts string) bool { return ts == "" || strings.HasPrefix(ts, "0001-01-01") }

// classify reads the final state of a container that was started.
func classify(st containerState) LaunchResult {
	switch {
	case unset(st.State.StartedAt):
		return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: "the container never started" + stateError(st)}
	case unset(st.State.FinishedAt):
		return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: "the container has no completion time" + stateError(st)}
	case st.State.OOMKilled:
		return LaunchResult{Class: ClassResource, ExitCode: st.State.ExitCode, Reason: "killed for exceeding the memory limit (OOM)"}
	case st.State.Status != "exited" || st.State.Error != "":
		return LaunchResult{Class: ClassInfrastructure, ExitCode: -1, Reason: "container state " + st.State.Status + stateError(st)}
	}
	return LaunchResult{Class: ClassProcess, ExitCode: st.State.ExitCode}
}

func stateError(st containerState) string {
	if st.State.Error == "" {
		return ""
	}
	return ": " + st.State.Error
}

// writeEnvFile writes the launch environment into a runner-private file that no
// container mounts.
func (s *sandbox) writeEnvFile(id string, env []string) (string, error) {
	dir := filepath.Join(s.dir, "env")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("sandbox env: %w", err)
	}
	path := filepath.Join(dir, id+".env")
	if err := os.WriteFile(path, []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("sandbox env: %w", err)
	}
	return path, nil
}

// readBack streams a directory of a stopped container as a tar to fn.
func (s *sandbox) readBack(ctx context.Context, container, target string, fn func(io.Reader) error) error {
	if err := s.d.stream(ctx, fn, "cp", container+":"+target, "-"); err != nil {
		return fmt.Errorf("read back %s: %w", target, err)
	}
	return nil
}

// sequenceVolume is a work volume shared by the ordered commands of one verification
// sequence (build, test, acceptance). It has its own intent, so recovery finds it.
type sequenceVolume struct {
	s    *sandbox
	id   string
	Name string
}

// newSequence creates and fills a work volume for a verification sequence.
func (s *sandbox) newSequence(ctx context.Context, kind, target string, tree tarSource) (*sequenceVolume, error) {
	in, err := s.newIntent(kind)
	if err != nil {
		return nil, err
	}
	seq := &sequenceVolume{s: s, id: in.ID}
	name, err := s.createVolume(in.ID, kind, 0)
	if err != nil {
		return seq, err
	}
	seq.Name = name
	if err := s.fill(ctx, in.ID, kind, s.image.ID, []sandboxVolume{{Name: name, Target: target, Fill: tree}}); err != nil {
		return seq, err
	}
	return seq, nil
}

// Close removes the sequence's volume and confirms it.
func (q *sequenceVolume) Close() error { return q.s.release(q.id) }

// lockSandboxFlow serializes a sandbox flow that no outcome or adoption lock covers.
// Runs, plans and adoption open their flows under their own locks. A crashed holder
// leaves the lock behind, and the error says how to clear it.
func lockSandboxFlow(r Repo, flow string) (func(), error) {
	dir := filepath.Join(r.Root, ".aeh", "sandbox", "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox lock: %w", err)
	}
	lock := filepath.Join(dir, flowKeyUnsafe.ReplaceAllString(flow, "_"))
	if err := os.Mkdir(lock, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("another aeh sandbox %s is running; if it crashed, verify it stopped before removing %s", flow, lock)
		}
		return nil, fmt.Errorf("sandbox lock: %w", err)
	}
	return func() { _ = os.Remove(lock) }, nil
}

// checkLogConfig accepts the effective log configuration only if the whole output is
// kept: the output is read with docker logs after the process ends. The size must be
// the unreachable one aeh sets, and the mode blocking (Docker's default). aeh does not
// force the mode: Docker refuses mode=blocking together with a daemon's inherited
// max-buffer-size. A daemon defaulting to non-blocking mode, which drops output when its
// buffer fills, is refused with what to change. Other merged options are harmless.
func checkLogConfig(typ string, cfg map[string]string) string {
	switch {
	case typ != "json-file":
		return "log driver " + typ + " (aeh needs json-file to read the whole output)"
	case cfg["max-size"] != sandboxLogKeep:
		return "log max-size " + cfg["max-size"] + " (the whole output must be kept)"
	case cfg["mode"] != "" && cfg["mode"] != "blocking":
		return "log mode " + cfg["mode"] + " can drop output; remove the non-blocking log-opts default from the Docker daemon configuration (daemon.json)"
	}
	return ""
}
