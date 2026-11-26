package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEgressConfig(t *testing.T) {
	good := minimalConfig + "    sandbox:\n      egress: [api.openai.com:443, chatgpt.com:443]\n"
	c, err := loadTestConfig(t, good)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Profiles["b"].sandboxEgress(); len(got) != 2 || got[1] != "chatgpt.com:443" {
		t.Fatalf("parsed %v", got)
	}
	for name, body := range map[string]string{
		"no port":   minimalConfig + "    sandbox:\n      egress: [api.openai.com]\n",
		"wildcard":  minimalConfig + "    sandbox:\n      egress: [\"*.openai.com:443\"]\n",
		"uppercase": minimalConfig + "    sandbox:\n      egress: [API.openai.com:443]\n",
		"duplicate": minimalConfig + "    sandbox:\n      egress: [a.com:443, a.com:443]\n",
	} {
		if _, err := loadTestConfig(t, body); err == nil || !strings.Contains(err.Error(), "sandbox.egress") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Only a launch with egress names the forwarder; every other launch keeps the proxy
// variables empty. FTP_PROXY and ALL_PROXY stay empty either way.
func TestEgressEnvironment(t *testing.T) {
	s := &sandbox{}
	for _, proxied := range []bool{false, true} {
		_, want, err := s.launchEnv(sandboxImage{}, nil, proxied)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range proxyVars {
			v := want[name]
			switch {
			case !proxied || strings.Contains(strings.ToUpper(name), "FTP") || strings.Contains(strings.ToUpper(name), "ALL"):
				if v != "" {
					t.Errorf("proxied=%v: %s=%q", proxied, name, v)
				}
			case strings.Contains(strings.ToUpper(name), "NO_PROXY"):
				if v != egressNoProxy {
					t.Errorf("%s=%q", name, v)
				}
			default:
				if v != "http://127.0.0.1:3128" {
					t.Errorf("%s=%q", name, v)
				}
			}
		}
	}
}

// egressTestImage builds an image from the test image with aeh-egress added, for the
// architecture of the local engine, and removes it when the test ends.
func egressTestImage(t *testing.T, s *sandbox) sandboxImage {
	t.Helper()
	arch, err := s.d.admin("info", "--format", "{{.Architecture}}")
	if err != nil {
		t.Fatal(err)
	}
	goarch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[arch]
	if goarch == "" {
		t.Skipf("engine architecture %s", arch)
	}
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "aeh-egress"), "../../cmd/aeh-egress")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aeh-egress: %v\n%s", err, out)
	}
	// The program goes in through a container and a commit: a build cannot start from an
	// image ID without a registry name.
	bin, err := os.ReadFile(filepath.Join(dir, "aeh-egress"))
	if err != nil {
		t.Fatal(err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "aeh-egress", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(bin)
	_ = tw.Close()
	cid, err := s.d.admin("create", s.image.ID, "true")
	if err != nil {
		t.Fatal(err)
	}
	defer s.d.admin("rm", "-f", cid) //nolint:errcheck
	if _, err := s.d.run(context.Background(), &tb, nil, "cp", "-", cid+":/usr/local/bin"); err != nil {
		t.Fatal(err)
	}
	// A stop signal of the image's own must not reach the proxy.
	id, err := s.d.admin("commit", "--change", "STOPSIGNAL SIGQUIT", cid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", id).Run() })
	img, err := resolveImage(s.d, id)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// An agent with egress has no network of its own: it reaches only its declared pairs,
// only through the proxy, and the proxy refuses private addresses even for a declared
// name. The proxy's log comes back with the launch, and nothing is left behind.
func TestDockerEgressReachesOnlyDeclaredPairs(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	s.agent = egressTestImage(t, s)
	script := `
ask() { (printf 'CONNECT %s HTTP/1.1\r\n\r\n' "$1"; sleep 2) | nc 127.0.0.1 3128 2>&1 | head -n 1; }
echo "proxy=$HTTPS_PROXY"
echo "unlisted: $(ask other.example.com:443)"
echo "private: $(ask localhost:80)"
if wget -q -T 3 -O /dev/null http://1.1.1.1/ 2>/dev/null; then echo DIRECT-REACHED; else echo direct-refused; fi
if [ -n "$AEH_TEST_EGRESS_NET" ]; then
  echo "allowed: $(ask example.com:443)"
  # A tunnel still open when the agent exits.
  (printf 'CONNECT example.com:443 HTTP/1.1\r\n\r\n'; sleep 60) | nc 127.0.0.1 3128 >/dev/null 2>&1 &
  sleep 2
fi
ls /egress`
	p := Profile{Command: []string{"sh", "-c", script}, Sandbox: &ProfileSandbox{Egress: []string{"example.com:443", "localhost:80"}}}
	if os.Getenv("AEH_TEST_EGRESS_NET") != "" {
		p.Env = map[string]string{"AEH_TEST_EGRESS_NET": "1"}
	}
	dir := t.TempDir()
	outPath, res, err := s.runAgent(context.Background(), agentLaunch{Kind: "review", Profile: p, Prompt: "x", RunDir: dir, LogName: "a"})
	out, _ := os.ReadFile(outPath)
	if err != nil || res.Class != ClassProcess || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, out)
	}
	text := string(out)
	for _, want := range []string{"proxy=http://127.0.0.1:3128", "unlisted: HTTP/1.1 403", "private: HTTP/1.1 403", "direct-refused", "proxy.sock"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if os.Getenv("AEH_TEST_EGRESS_NET") != "" && !strings.Contains(text, "allowed: HTTP/1.1 200") {
		t.Errorf("declared pair not reached:\n%s", text)
	}
	if os.Getenv("AEH_TEST_EGRESS_NET") != "" {
		// The tunnel left open when the agent exits is still recorded.
		if l, _ := os.ReadFile(filepath.Join(dir, "a.egress.log")); strings.Count(string(l), `"verdict":"allow"`) != 2 {
			t.Errorf("open tunnel not logged:\n%s", l)
		}
	}
	log, err :=os.ReadFile(filepath.Join(dir, "a.egress.log"))
	if err != nil || !strings.Contains(string(log), `"host":"other.example.com"`) || !strings.Contains(string(log), "not in the egress list") ||
		!strings.Contains(string(log), "no public address") {
		t.Fatalf("proxy log %v:\n%s", err, log)
	}
	assertNoLeftovers(t, s)
}

// A proxy that stops during the launch, and a launcher that cannot run its program, are
// infrastructure failures: nothing is known about the agent.
func TestDockerEgressFailuresAreInfrastructure(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	s.agent = egressTestImage(t, s)
	p := Profile{Command: []string{"/no/such/program"}, Sandbox: &ProfileSandbox{Egress: []string{"example.com:443"}}}
	dir := t.TempDir()
	_, res, err := s.runAgent(context.Background(), agentLaunch{Kind: "review", Profile: p, Prompt: "x", RunDir: dir, LogName: "a"})
	if err != nil || res.Class != ClassInfrastructure || !strings.Contains(res.Reason, "egress launcher failed") {
		t.Fatalf("missing program: %+v %v", res, err)
	}
	// The launcher's reason is kept, and no launch file stays behind.
	if stderr, _ := os.ReadFile(filepath.Join(dir, "a.err")); !strings.Contains(string(stderr), "aeh-egress: start /no/such/program") {
		t.Fatalf("launcher stderr %q", stderr)
	}
	if left, _ := os.ReadDir(filepath.Join(s.dir, "env")); len(left) != 0 {
		t.Fatalf("environment files left: %v", left)
	}
	assertNoLeftovers(t, s)

	p = Profile{Command: []string{"sh", "-c", "sleep 8; echo done"}, Sandbox: &ProfileSandbox{Egress: []string{"example.com:443"}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 60; i++ {
			out, _ := s.d.admin("ps", "-q", "--filter", "label=aeh.project="+s.project, "--filter", "label=aeh.role=egress")
			if out != "" {
				time.Sleep(time.Second) // the agent has started behind it
				_, _ = s.d.admin("kill", out)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	_, res, err = s.runAgent(context.Background(), agentLaunch{Kind: "review", Profile: p, Prompt: "x", RunDir: t.TempDir(), LogName: "b"})
	<-done
	if err != nil || res.Class != ClassInfrastructure || !strings.Contains(res.Reason, "proxy stopped") {
		t.Fatalf("stopped proxy: %+v %v", res, err)
	}
	assertNoLeftovers(t, s)

	// A cancelled launch still keeps the proxy's log.
	p = Profile{Command: []string{"sh", "-c", `(printf 'CONNECT other.example.com:443 HTTP/1.1

'; sleep 1) | nc 127.0.0.1 3128; sleep 60`},
		Sandbox: &ProfileSandbox{Egress: []string{"example.com:443"}}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(8*time.Second, cancel)
	dir = t.TempDir()
	_, res, err = s.runAgent(ctx, agentLaunch{Kind: "review", Profile: p, Prompt: "x", RunDir: dir, LogName: "c"})
	if err != nil || res.Class != ClassTerminated {
		t.Fatalf("cancelled: %+v %v", res, err)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "c.egress.log")); !strings.Contains(string(log), "other.example.com") {
		t.Fatalf("cancelled launch lost the proxy log: %q", log)
	}
	assertNoLeftovers(t, s)

}
