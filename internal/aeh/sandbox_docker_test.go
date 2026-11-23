package aeh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerSandbox opens a sandbox on a real engine. These tests run only with
// AEH_TEST_DOCKER=1; AEH_TEST_IMAGE names a local image with sh (default alpine:3.20).
func dockerSandbox(t *testing.T, cfg SandboxConfig) (*sandbox, Repo) {
	t.Helper()
	if os.Getenv("AEH_TEST_DOCKER") != "1" {
		t.Skip("set AEH_TEST_DOCKER=1 to run tests against a local Docker engine")
	}
	ref := os.Getenv("AEH_TEST_IMAGE")
	if ref == "" {
		ref = "alpine:3.20"
	}
	out, err := exec.Command("docker", "image", "inspect", "-f", "{{.Id}}", ref).Output()
	if err != nil {
		t.Fatalf("image %s must be present locally: %v", ref, err)
	}
	cfg.Image = strings.TrimSpace(string(out))
	r := Repo{Root: t.TempDir()}
	s, err := openSandbox(r, cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func assertNoLeftovers(t *testing.T, s *sandbox) {
	t.Helper()
	for _, args := range [][]string{{"ps", "-a", "-q"}, {"volume", "ls", "-q"}} {
		out, err := s.d.admin(append(args, "--filter", "label=aeh.project="+s.project)...)
		if err != nil || out != "" {
			t.Fatalf("docker %s left %q (%v)", args[0], out, err)
		}
	}
}

func TestDockerLaunchExactTree(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	files := map[string][]byte{"tool": []byte("#!/bin/sh\necho tool-ran\n"), "plain": []byte("#!/bin/sh\necho plain-ran\n"),
		"crlf.txt": []byte("a\r\nb\r\n"), "bin/data": {0, 255, 10, 13}}
	script := `set -e
./tool
if ./plain 2>/dev/null; then echo PLAIN-RAN; fi
od -An -tx1 crlf.txt | tr -d ' \n'; echo
od -An -tx1 bin/data | tr -d ' \n'; echo
stat -c '%u:%g %a' tool plain .
cat /run/aeh/r
if echo x > /run/aeh/w 2>/dev/null; then echo RO-WRITABLE; fi
mkdir out && echo built > out/artifact && chmod 755 out/artifact`
	var out bytes.Buffer
	var back map[string]treeEntry
	res, err := s.run(context.Background(), sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", script}, Workdir: "/work",
		Volumes: []sandboxVolume{{Target: "/work", Fill: treeFiles(files, map[string]bool{"tool": true})},
			{Target: "/run/aeh", ReadOnly: true, Fill: treeFiles(map[string][]byte{"r": []byte("runner-file\n")}, nil)}},
		Timeout: 2 * time.Minute, Output: &out},
		func(c string) error {
			return s.readBack(context.Background(), c, "/work", func(r io.Reader) (err error) {
				back, err = readTree(r, "work", 1<<20)
				return err
			})
		})
	if err != nil || res.Class != ClassProcess || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, out.String())
	}
	text := out.String()
	for _, want := range []string{"tool-ran", "610d0a620d0a", "00ff0a0d", "10001:10001 755\n10001:10001 644\n10001:10001 755", "runner-file"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"PLAIN-RAN", "RO-WRITABLE"} {
		if strings.Contains(text, bad) {
			t.Errorf("output has %s:\n%s", bad, text)
		}
	}
	if len(back) != 8 || !back["out/artifact"].Executable || !back["tool"].Executable || back["plain"].Executable {
		t.Fatalf("read back %+v", back)
	}
	assertNoLeftovers(t, s)
}

func TestDockerLaunchClasses(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{Memory: "32m"})
	run := func(script string, timeout time.Duration) LaunchResult {
		t.Helper()
		res, err := s.run(context.Background(), sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", script}, Workdir: "/tmp", Timeout: timeout}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run("exit 7", time.Minute); r.Class != ClassProcess || r.ExitCode != 7 {
		t.Errorf("exit 7: %+v", r)
	}
	if r := run("sleep 30", 2*time.Second); r.Class != ClassTerminated {
		t.Errorf("timeout: %+v", r)
	}
	if r := run("tail /dev/zero", time.Minute); r.Class != ClassResource {
		t.Errorf("OOM: %+v", r)
	}
	if r := run("wget -q -T 3 -O- http://1.1.1.1 && echo NET", time.Minute); r.Class != ClassProcess || r.ExitCode == 0 {
		t.Errorf("network reachable: %+v", r)
	}
	assertNoLeftovers(t, s)
}

func TestDockerRecoveryByLabels(t *testing.T) {
	s, r := dockerSandbox(t, SandboxConfig{})
	in, err := s.newIntent("gate")
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted launch: a volume and a container exist, the container ID was never saved.
	if _, err := s.createVolume(in.ID, "gate", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.admin(append(append([]string{"create", "--pull=never", "--network", "none"}, s.labels(in.ID, "gate", "consumer")...), s.image.ID, "true")...); err != nil {
		t.Fatal(err)
	}
	envFile, err := s.writeEnvFile(in.ID, []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := openSandbox(r, s.cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, again)
	if _, err := os.Stat(filepath.Join(s.intentDir(), in.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("intent kept after confirmed recovery: %v", err)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("env file of the recovered launch kept: %v", err)
	}
}

func TestDockerClientProxyNeverReachesContainers(t *testing.T) {
	cfgDir := t.TempDir()
	canary := "http://user:canary-secret@proxy.invalid:3128"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"proxies":{"default":{"httpProxy":"`+canary+`","httpsProxy":"`+canary+`","noProxy":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			t.Skip("no docker context")
		}
		host = strings.TrimSpace(string(out))
	}
	t.Setenv("DOCKER_HOST", host)
	t.Setenv("DOCKER_CONFIG", cfgDir)
	s, _ := dockerSandbox(t, SandboxConfig{})
	var out bytes.Buffer
	res, err := s.run(context.Background(), sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", "env"}, Workdir: "/tmp", Timeout: time.Minute, Output: &out}, nil)
	if err != nil || res.Class != ClassProcess || res.ExitCode != 0 {
		t.Fatalf("%+v %v\n%s", res, err, out.String())
	}
	if strings.Contains(out.String(), "canary-secret") {
		t.Fatalf("client proxy reached the container:\n%s", out.String())
	}
}

func TestDockerOutputFailureProvesNothing(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	// A failing output writer together with a nonzero exit: the red is not evidence.
	res, err := s.run(context.Background(), sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"},
		Workdir: "/tmp", Timeout: time.Minute, Output: failingWriter{}}, nil)
	if err != nil || res.Class != ClassInfrastructure || !strings.Contains(res.Reason, "output") {
		t.Fatalf("%+v %v", res, err)
	}
	var out bytes.Buffer
	res, err = s.run(context.Background(), sandboxLaunch{Kind: "gate", Argv: []string{"sh", "-c", "echo out; echo err >&2; exit 3"},
		Workdir: "/tmp", Timeout: time.Minute, Output: &out}, nil)
	if err != nil || res.Class != ClassProcess || res.ExitCode != 3 || !strings.Contains(out.String(), "out") || !strings.Contains(out.String(), "err") {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	assertNoLeftovers(t, s)
}

func TestDockerLongOutputIsKeptWhole(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	// About 80 MB of output, above any common rotation size, ending with a marker; the
	// head marker proves nothing was rotated away.
	for _, exit := range []string{"0", "4"} {
		var head, out bytes.Buffer
		w := &markerWriter{head: &head, max: 64}
		res, err := s.run(context.Background(), sandboxLaunch{Kind: "gate", Workdir: "/tmp", Timeout: 5 * time.Minute, Output: io.MultiWriter(w, &tailOnly{buf: &out}),
			Argv: []string{"sh", "-c", "echo HEAD-MARKER; yes 0123456789012345678901234567890123456789 | head -c 80000000; echo; echo TAIL-MARKER; exit " + exit}}, nil)
		if err != nil || res.Class != ClassProcess || fmt.Sprint(res.ExitCode) != exit {
			t.Fatalf("exit %s: %+v %v", exit, res, err)
		}
		if !strings.HasPrefix(head.String(), "HEAD-MARKER") || !strings.Contains(out.String(), "TAIL-MARKER") {
			t.Fatalf("exit %s: output not whole: head %q", exit, head.String())
		}
	}
	assertNoLeftovers(t, s)
}

// markerWriter keeps the first max bytes written.
type markerWriter struct {
	head *bytes.Buffer
	max  int
}

func (m *markerWriter) Write(p []byte) (int, error) {
	if left := m.max - m.head.Len(); left > 0 {
		m.head.Write(p[:min(left, len(p))])
	}
	return len(p), nil
}

// tailOnly keeps the last 4 KiB written.
type tailOnly struct{ buf *bytes.Buffer }

func (t *tailOnly) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 4096 {
		b := t.buf.Bytes()[t.buf.Len()-4096:]
		nb := append([]byte(nil), b...)
		t.buf.Reset()
		t.buf.Write(nb)
	}
	return len(p), nil
}
