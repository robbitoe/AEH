package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSandboxConfigValidate(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	ok := []SandboxConfig{
		{Image: id},
		{Image: "tools@" + id, Memory: "512m", CPUs: "1.5"},
		{Image: id, GateEnv: map[string]string{"DOTNET_CLI_TELEMETRY_OPTOUT": "1"}},
	}
	for _, c := range ok {
		if err := c.validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []SandboxConfig{
		{Image: "tools:latest"},
		{Image: "tools@sha256:abc"},
		{Image: id, Memory: "4 GB"},
		{Image: id, CPUs: "0"},
		{Image: id, GateEnv: map[string]string{"HOME": "/x"}},
		{Image: id, GateEnv: map[string]string{"https_proxy": "http://p"}},
		{Image: id, GateEnv: map[string]string{"A": "x\ny"}},
		{Image: id, GateEnv: map[string]string{"A-B": "x"}},
	}
	for _, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("%+v: accepted", c)
		}
	}
	var c SandboxConfig
	if err := yaml.Unmarshal([]byte("image: x\nnetwork: host\n"), &c); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("unknown key accepted: %v", err)
	}
}

func TestVolumeTarRoundTrip(t *testing.T) {
	files := map[string][]byte{"run.sh": []byte("#!/bin/sh\n"), "src/a.txt": []byte("a\r\n"), "src/deep/b.bin": {0, 1, 2}}
	var buf bytes.Buffer
	if err := writeVolumeTar(&buf, "work", treeFiles(files, map[string]bool{"run.sh": true})); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	seen := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 10001 || h.Gid != 10001 {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		seen[h.Name] = h.Mode
	}
	want := map[string]int64{"work/": 0o755, "work/run.sh": 0o755, "work/src/": 0o755, "work/src/a.txt": 0o644, "work/src/deep/": 0o755, "work/src/deep/b.bin": 0o644}
	if len(seen) != len(want) {
		t.Fatalf("entries %v", seen)
	}
	for name, mode := range want {
		if seen[name] != mode {
			t.Errorf("%s mode %o, want %o", name, seen[name], mode)
		}
	}
	back, err := readTree(bytes.NewReader(buf.Bytes()), "work", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || !back["run.sh"].Executable || back["src/a.txt"].Executable || back["src/a.txt"].Size != 3 {
		t.Fatalf("read back %+v", back)
	}
	for _, bad := range []string{"../x", "/abs", "a//b", "a/./b", `a\b`} {
		err := writeVolumeTar(io.Discard, "work", treeFiles(map[string][]byte{bad: nil}, nil))
		if err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestReadTreeRefusesLinksAndLimits(t *testing.T) {
	tarOf := func(hs ...*tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hs {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if h.Size > 0 {
				tw.Write(bytes.Repeat([]byte("x"), int(h.Size)))
			}
		}
		tw.Close()
		return buf.Bytes()
	}
	dir := &tar.Header{Name: "work/", Typeflag: tar.TypeDir, Mode: 0o755}
	// Each case has room under the limit, so the rejection comes from the check named.
	cases := map[string]struct {
		tar  []byte
		want string
	}{
		"symlink": {tarOf(dir, &tar.Header{Name: "work/l", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}), "not a regular file"},
		"hard":    {tarOf(dir, &tar.Header{Name: "work/h", Typeflag: tar.TypeLink, Linkname: "work/x"}), "not a regular file"},
		"fifo":    {tarOf(dir, &tar.Header{Name: "work/f", Typeflag: tar.TypeFifo}), "not a regular file"},
		"outside": {tarOf(dir, &tar.Header{Name: "other/x", Typeflag: tar.TypeReg, Size: 1}), "unexpected entry"},
	}
	for name, c := range cases {
		if _, err := readTree(bytes.NewReader(c.tar), "work", 1<<20); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	big := tarOf(dir, &tar.Header{Name: "work/big", Typeflag: tar.TypeReg, Size: 4096})
	if _, err := readTree(bytes.NewReader(big), "work", 2048); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("content over the limit: %v", err)
	}
}

func TestClassifyContainer(t *testing.T) {
	st := func(status string, code int, oom bool, started, errText string) containerState {
		var c containerState
		c.State.Status, c.State.ExitCode, c.State.OOMKilled, c.State.StartedAt, c.State.Error = status, code, oom, started, errText
		if status != "created" && status != "running" {
			c.State.FinishedAt = "2026-11-20T10:00:05Z"
		}
		return c
	}
	ran := "2026-11-20T10:00:00Z"
	cases := []struct {
		c     containerState
		class string
		exit  int
	}{
		{st("exited", 0, false, ran, ""), ClassProcess, 0},
		{st("exited", 3, false, ran, ""), ClassProcess, 3},
		{st("exited", 137, true, ran, ""), ClassResource, 137},
		{st("created", 0, false, "0001-01-01T00:00:00Z", ""), ClassInfrastructure, -1},
		{st("exited", 127, false, ran, "exec: not found"), ClassInfrastructure, -1},
		{st("running", 0, false, ran, ""), ClassInfrastructure, -1},
	}
	for i, c := range cases {
		r := classify(c.c)
		if r.Class != c.class || r.ExitCode != c.exit {
			t.Errorf("case %d: %+v", i, r)
		}
	}
}

func TestGateFromLaunch(t *testing.T) {
	g := gateFromLaunch("test", LaunchResult{Class: ClassProcess, ExitCode: 1}, "boom", 1)
	if g.Passed || g.Uncertain || !g.provesResult() {
		t.Fatalf("process red: %+v", g)
	}
	g = gateFromLaunch("test", LaunchResult{Class: ClassResource, ExitCode: 137, Reason: "OOM"}, "", 1)
	if g.Passed || g.Uncertain || g.provesResult() || !strings.Contains(g.Output, "OOM") {
		t.Fatalf("OOM: %+v", g)
	}
	for _, class := range []string{ClassInfrastructure, ClassTerminated} {
		g = gateFromLaunch("build", LaunchResult{Class: class, ExitCode: -1, Reason: "x"}, "", 1)
		if g.Passed || !g.Uncertain || g.provesResult() {
			t.Fatalf("%s: %+v", class, g)
		}
	}
	if !(GateResult{Name: "scope", Passed: true}).provesResult() {
		t.Fatal("a gate without a command must prove its result")
	}
}

func TestHostGateCommandClass(t *testing.T) {
	dir := t.TempDir()
	g := gateCommand(context.Background(), "test", dir, "exit 2", time.Minute, nil)
	if g.Passed || g.Class != ClassProcess || !g.provesResult() {
		t.Fatalf("exit 2: %+v", g)
	}
	cmd := "sleep 5"
	if runtime.GOOS == "windows" {
		cmd = "ping -n 6 127.0.0.1 >NUL"
	}
	g = gateCommand(context.Background(), "test", dir, cmd, 200*time.Millisecond, nil)
	if !g.Uncertain || g.Class != ClassTerminated || g.provesResult() {
		t.Fatalf("timeout: %+v", g)
	}
}

func TestCheckCreated(t *testing.T) {
	s := &sandbox{image: sandboxImage{ID: "sha256:img", Env: []string{"PATH=/bin", "LANG=C"}}}
	own, want, err := s.launchEnv([]string{"A=1"})
	if err != nil || len(own) != 13 {
		t.Fatalf("env %v %v", own, err)
	}
	l := sandboxLaunch{Argv: []string{"sh", "-c", "x"}, Workdir: "/work"}
	vols := []sandboxVolume{{Name: "v1", Target: "/work"}, {Name: "v2", Target: "/run/aeh", ReadOnly: true}}
	good := func() containerState {
		var c containerState
		c.Image, c.Config.User, c.Config.WorkingDir, c.Config.Entrypoint = "sha256:img", "10001:10001", "/work", []string{"sh"}
		c.HostConfig.NetworkMode, c.HostConfig.ReadonlyRootfs = "none", true
		c.HostConfig.LogConfig.Type, c.HostConfig.LogConfig.Config = "json-file", map[string]string{"max-size": sandboxLogKeep}
		c.HostConfig.CapDrop, c.HostConfig.SecurityOpt = []string{"ALL"}, []string{"no-new-privileges"}
		c.HostConfig.Tmpfs = map[string]string{"/tmp": "mode=1777", sandboxHome: "uid=10001"}
		for k, v := range want {
			c.Config.Env = append(c.Config.Env, k+"="+v)
		}
		c.Mounts = append(c.Mounts, struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}{"volume", "v1", "/work", true}, struct {
			Type        string `json:"Type"`
			Name        string `json:"Name"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}{"volume", "v2", "/run/aeh", false})
		return c
	}
	if err := s.checkCreated(good(), l, vols, want); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*containerState){
		"proxy injected": func(c *containerState) {
			for i, kv := range c.Config.Env {
				if strings.HasPrefix(kv, "HTTPS_PROXY=") {
					c.Config.Env[i] = "HTTPS_PROXY=http://u:p@h"
				}
			}
		},
		"extra variable":  func(c *containerState) { c.Config.Env = append(c.Config.Env, "SECRET=x") },
		"network":         func(c *containerState) { c.HostConfig.NetworkMode = "bridge" },
		"user":            func(c *containerState) { c.Config.User = "" },
		"writable review": func(c *containerState) { c.Mounts[1].RW = true },
		"anonymous volume": func(c *containerState) {
			c.Mounts = append(c.Mounts, c.Mounts[0])
			c.Mounts[2].Destination, c.Mounts[2].Name = "/var/lib/data", "anon"
		},
		"bind":       func(c *containerState) { c.HostConfig.Binds = []string{"C:\\:/host"} },
		"image":      func(c *containerState) { c.Image = "sha256:other" },
		"entrypoint": func(c *containerState) { c.Config.Entrypoint = []string{"bash"} },
		"caps":       func(c *containerState) { c.HostConfig.CapDrop = nil },
		"log rotation": func(c *containerState) {
			c.HostConfig.LogConfig.Config = map[string]string{"max-size": "64m", "max-file": "1"}
		},
		"log driver": func(c *containerState) { c.HostConfig.LogConfig.Type = "local" },
		"non-blocking log": func(c *containerState) {
			c.HostConfig.LogConfig.Config["mode"] = "non-blocking"
		},
	}
	for name, m := range mutations {
		c := good()
		m(&c)
		if err := s.checkCreated(c, l, vols, want); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Options a daemon merges in (Docker's documented max-size=10m, max-file=3 defaults
	// with our max-size overriding) must not block every launch.
	inherited := good()
	inherited.HostConfig.LogConfig.Config["max-file"] = "3"
	inherited.HostConfig.LogConfig.Config["compress"] = "true"
	if err := s.checkCreated(inherited, l, vols, want); err != nil {
		t.Fatalf("inherited daemon log options refused: %v", err)
	}
}

func TestCommandPrograms(t *testing.T) {
	got := commandPrograms(`npm ci && npm run build | tee "log"; CI=1 dotnet test`, `./run.sh $X`, `sh tests/run.sh {{manifest}}`)
	want := "dotnet npm sh tee"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	if !gitWord.MatchString("git describe") || !gitWord.MatchString("make && git rev-parse HEAD") || gitWord.MatchString("npm run digit") || gitWord.MatchString("gitleaks") {
		t.Fatal("git detection")
	}
}

func TestClassifyRequiresCompletionTime(t *testing.T) {
	var c containerState
	c.State.Status, c.State.StartedAt, c.State.FinishedAt = "exited", "2026-11-20T10:00:00Z", "0001-01-01T00:00:00Z"
	if r := classify(c); r.Class != ClassInfrastructure {
		t.Fatalf("no completion time proved a result: %+v", r)
	}
}

func TestCheckLocalEndpoint(t *testing.T) {
	for _, ok := range []string{"unix:///var/run/docker.sock", "npipe:////./pipe/dockerDesktopLinuxEngine", `npipe://\\.\pipe\docker_engine`} {
		if err := checkLocalEndpoint(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"tcp://127.0.0.1:2375", "ssh://host", "npipe:////remote/pipe/docker_engine", `npipe://\\server\pipe\docker`,
		"npipe:////./pipe/", "npipe:////./pipe/a/b", "unix://relative.sock", ""} {
		if err := checkLocalEndpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestWaitMismatch(t *testing.T) {
	proc := func(code int) LaunchResult { return LaunchResult{Class: ClassProcess, ExitCode: code} }
	cases := []struct {
		waited string
		err    error
		res    LaunchResult
		fail   bool
	}{
		{"0", nil, proc(0), false},
		{"3\n", nil, proc(3), false},
		{"137", nil, LaunchResult{Class: ClassResource, ExitCode: 137}, false},
		{"0", nil, proc(3), true},
		{"", errors.New("daemon lost"), proc(0), true},
		{"x", nil, proc(0), true},
		{"", errors.New("x"), LaunchResult{Class: ClassInfrastructure}, false}, // already unproven
	}
	for i, c := range cases {
		if got := waitMismatch(c.waited, c.err, c.res) != ""; got != c.fail {
			t.Errorf("case %d: mismatch %v, want %v", i, got, c.fail)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestErrWriterKeepsTheFirstError(t *testing.T) {
	ew := &errWriter{w: failingWriter{}}
	if n, err := ew.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("write %d %v", n, err)
	}
	if ew.err == nil || ew.err.Error() != "disk full" {
		t.Fatalf("error %v", ew.err)
	}
}

func TestIsolatedDockerCLIUsesOnlyTheValidatedEndpoint(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "elsewhere")
	t.Setenv("DOCKER_HOST", "tcp://remote:2375")
	t.Setenv("DOCKER_CERT_PATH", "/certs")
	d, err := isolatedDockerCLI(t.TempDir(), "npipe:////./pipe/docker_engine")
	if err != nil {
		t.Fatal(err)
	}
	hosts := 0
	for _, kv := range d.env {
		name, value, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "DOCKER_HOST":
			hosts++
			if value != "npipe:////./pipe/docker_engine" {
				t.Errorf("DOCKER_HOST=%s", value)
			}
		case "DOCKER_CONTEXT", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY":
			t.Errorf("%s reaches the isolated client", name)
		}
	}
	if hosts != 1 {
		t.Fatalf("%d DOCKER_HOST entries", hosts)
	}
}

func TestReadTreeBoundsMetadata(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "work/", Typeflag: tar.TypeDir, Mode: 0o755})
	for i := 0; i < 200; i++ {
		tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("work/empty-%d", i), Typeflag: tar.TypeReg, Mode: 0o644})
	}
	tw.Close()
	if _, err := readTree(bytes.NewReader(buf.Bytes()), "work", 16*1024); err == nil {
		t.Fatal("empty files escaped the raw stream limit")
	}
	if _, err := readTree(bytes.NewReader(buf.Bytes()), "work", 0); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	tw = tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "work/" + strings.Repeat("a/", 2100) + "x", Typeflag: tar.TypeReg, Mode: 0o644})
	tw.Close()
	if _, err := readTree(bytes.NewReader(buf.Bytes()), "work", 0); err == nil || !strings.Contains(err.Error(), "longer") {
		t.Fatalf("long path: %v", err)
	}
}

func TestHostShellStartFailureIsUncertain(t *testing.T) {
	g := gateCommand(context.Background(), "build", filepath.Join(t.TempDir(), "missing"), "true", time.Minute, nil)
	if g.Passed || !g.Uncertain || g.Class != ClassInfrastructure || g.provesResult() {
		t.Fatalf("%+v", g)
	}
}

func TestSandboxFlowLock(t *testing.T) {
	r := Repo{Root: t.TempDir()}
	unlock, err := lockSandboxFlow(r, "check")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockSandboxFlow(r, "check"); err == nil {
		t.Fatal("a second check got the lock")
	}
	unlock()
	again, err := lockSandboxFlow(r, "check")
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestCheckLogConfig(t *testing.T) {
	keep := map[string]string{"max-size": sandboxLogKeep}
	if p := checkLogConfig("json-file", keep); p != "" {
		t.Fatal(p)
	}
	ok := map[string]string{"max-size": sandboxLogKeep, "max-file": "3", "compress": "true", "mode": "blocking"}
	if p := checkLogConfig("json-file", ok); p != "" {
		t.Fatal(p)
	}
	// Docker's merge of a daemon default {"mode":"non-blocking","max-buffer-size":"4m"}:
	// refused with what to change, never accepted.
	daemon := map[string]string{"max-size": sandboxLogKeep, "mode": "non-blocking", "max-buffer-size": "4m"}
	if p := checkLogConfig("json-file", daemon); !strings.Contains(p, "daemon.json") {
		t.Fatalf("non-blocking daemon default: %q", p)
	}
	if p := checkLogConfig("json-file", map[string]string{"max-size": "10m"}); p == "" {
		t.Fatal("rotation accepted")
	}
	if p := checkLogConfig("local", keep); p == "" {
		t.Fatal("local driver accepted")
	}
}

func TestCommandProgramsHonorsQuotes(t *testing.T) {
	cases := map[string]string{
		`python -c "print(42)"`:                "python",
		`node -e 'a(); b | c && d'`:            "node",
		`sh -c "x; y" && make test`:            "make sh",
		`echo a\;b; true`:                      "echo true",
		`CI=1 "my tool" --flag`:                "",
		`npm test $(echo x); later`:            "npm",
		`go test ./... "unclosed && rm -rf x`:  "go",
		"dotnet build\ndotnet test --no-build": "dotnet",
		`./run.sh "a b" | tee out.log`:         "tee",
	}
	for line, want := range cases {
		if got := strings.Join(commandPrograms(line), " "); got != want {
			t.Errorf("%q: got %q, want %q", line, got, want)
		}
	}
}
