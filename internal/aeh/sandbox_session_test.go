package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSessionConfig(t *testing.T) {
	good := minimalConfig + "    sandbox:\n      session: [.codex/auth.json, .claude.json]\n      login: [codex, login, --device-auth]\n"
	c, err := loadTestConfig(t, good)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Profiles["b"].sandboxSession(); len(got) != 2 || c.Profiles["b"].Sandbox.Login[2] != "--device-auth" {
		t.Fatalf("parsed %+v", c.Profiles["b"].Sandbox)
	}
	many := make([]string, maxSessionFiles+1)
	for i := range many {
		many[i] = "f" + strings.Repeat("x", i)
	}
	for name, list := range map[string]string{
		"parent":         "[../x]",
		"absolute":       "[/etc/passwd]",
		"space":          "[\"a b\"]",
		"colon":          "[\"a:b\"]",
		"device":         "[.cfg/con.txt]",
		"trailing dot":   "[\"x.\"]",
		"case duplicate": "[.Codex/auth.json, .codex/auth.json]",
		"file and dir":   "[.codex, .codex/auth.json]",
		"too many":       "[" + strings.Join(many, ", ") + "]",
	} {
		if _, err := loadTestConfig(t, minimalConfig+"    sandbox:\n      session: "+list+"\n"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, body := range map[string]string{
		"login without session": "      login: [codex, login]\n",
		"empty login argument":  "      session: [a]\n      login: [codex, \"\"]\n",
	} {
		if _, err := loadTestConfig(t, minimalConfig+"    sandbox:\n"+body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A profile's session file list is pinned with policy 2; profiles without one leave the
// contract as before.
func TestSandboxContractPinsSessions(t *testing.T) {
	sb := &sandbox{cfg: SandboxConfig{Memory: "4g", CPUs: "2"}, engine: sandboxEngine{ID: "e"}, image: sandboxImage{ID: "sha256:i"},
		agent: sandboxImage{ID: "sha256:a"}}
	before, err := sandboxSnapshot(sb)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before, "AgentSession") {
		t.Fatalf("empty session list in the contract: %s", before)
	}
	sb.agentSession = map[string][]string{"b": {".codex/auth.json"}}
	after, err := sandboxSnapshot(sb)
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ AgentSession map[string][]string }
	if err := json.Unmarshal([]byte(after), &c); err != nil || c.AgentSession["b"][0] != ".codex/auth.json" {
		t.Fatalf("snapshot %s (%v)", after, err)
	}
	if checkPlanSandbox("p", before, sb) == nil {
		t.Fatal("a plan pinned without the session list was accepted")
	}
}

func TestPrivateVolumeTar(t *testing.T) {
	var buf bytes.Buffer
	if err := writeVolumeTarMode(&buf, "aeh", treeFiles(map[string][]byte{".codex/auth.json": []byte("{}")}, map[string]bool{".codex/auth.json": true}), true); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		modes[h.Name] = h.Mode
		if h.Uid != sandboxUID || h.Gid != sandboxUID {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
	}
	want := map[string]int64{"aeh/": 0o700, "aeh/.codex/": 0o700, "aeh/.codex/auth.json": 0o600}
	for name, mode := range want {
		if modes[name] != mode {
			t.Errorf("%s mode %o, want %o (all: %v)", name, modes[name], mode, modes)
		}
	}
}

func TestSessionStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "p")
	paths := []string{".codex/auth.json", ".other"}
	files, err := loadSession(dir, paths)
	if err != nil || len(files) != 0 {
		t.Fatalf("empty store: %v %v", files, err)
	}
	if err := storeSessionFile(dir, ".codex/auth.json", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := storeSessionFile(dir, ".codex/auth.json", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	files, err = loadSession(dir, paths)
	if err != nil || len(files) != 1 || string(files[".codex/auth.json"]) != "v2" {
		t.Fatalf("store: %q %v", files, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".codex"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, ".codex", "auth.json"))
		di, _ := os.Stat(filepath.Join(dir, ".codex"))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("modes %v %v", fi.Mode(), di.Mode())
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".other"), bytes.Repeat([]byte("x"), int(sessionFileLimit)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(dir, paths); err == nil {
		t.Fatal("an oversized stored file was loaded")
	}
}

func TestLockSessionWaits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions", "p")
	unlock, err := lockSession(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := lockSession(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second holder: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		u, err := lockSession(context.Background(), dir)
		if err == nil {
			u()
		}
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting holder never got the lock")
	}
}

// A profile with a session runs with its stored files in a private HOME volume; what
// the agent leaves there replaces the store, and anything unusable keeps the previous
// copy.
func TestDockerSessionRoundTrip(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	s.agent = s.image
	p := Profile{TimeoutMinutes: 2, Sandbox: &ProfileSandbox{Session: []string{".cfg/tok", ".cfg/extra"}}}
	dir := s.sessionDir("medio")
	launch := func(script string) (string, LaunchResult, sessionReport) {
		t.Helper()
		p.Command = []string{"sh", "-c", script}
		runDir := t.TempDir()
		outPath, res, err := s.runAgent(context.Background(), agentLaunch{Kind: "review", ProfileName: "medio", Profile: p, Prompt: "x", RunDir: runDir, LogName: "a"})
		if err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(outPath)
		var rep sessionReport
		if b, err := os.ReadFile(filepath.Join(runDir, "a.session.json")); err == nil {
			if err := json.Unmarshal(b, &rep); err != nil {
				t.Fatal(err)
			}
		}
		return string(out), res, rep
	}
	_, res, _ := launch("true")
	if res.Class != ClassInfrastructure || !strings.Contains(res.Reason, "aeh sandbox login medio") {
		t.Fatalf("no saved session: %+v", res)
	}
	if err := storeSessionFile(dir, ".cfg/tok", []byte("v1\n")); err != nil {
		t.Fatal(err)
	}
	out, res, rep := launch(`cat "$HOME/.cfg/tok"; stat -c '%a %u' "$HOME" "$HOME/.cfg" "$HOME/.cfg/tok"; echo v2 > "$HOME/.cfg/tok"; echo junk > "$HOME/.cfg/other"; exit 3`)
	if res.Class != ClassProcess || res.ExitCode != 3 {
		t.Fatalf("%+v", res)
	}
	if out != "v1\n700 10001\n700 10001\n600 10001\n" {
		t.Fatalf("inside the container:\n%s", out)
	}
	if got := readFile(t, filepath.Join(dir, ".cfg", "tok")); got != "v2\n" || len(rep.Saved) != 1 || rep.Saved[0] != ".cfg/tok" || len(rep.Kept) != 0 {
		t.Fatalf("store %q, report %s", got, rep)
	}
	if _, err := os.Stat(filepath.Join(dir, ".cfg", "other")); err == nil {
		t.Fatal("an undeclared file was saved")
	}
	for _, script := range []string{`rm "$HOME/.cfg/tok"`, `: > "$HOME/.cfg/tok"`, `rm "$HOME/.cfg/tok"; ln -s /etc/passwd "$HOME/.cfg/tok"`,
		`head -c 1048577 /dev/zero > "$HOME/.cfg/tok"`} {
		_, res, rep := launch(script)
		if res.Class != ClassProcess || readFile(t, filepath.Join(dir, ".cfg", "tok")) != "v2\n" || len(rep.Kept) != 1 || !strings.HasPrefix(rep.Kept[0], ".cfg/tok: ") || len(rep.Saved) != 0 {
			t.Fatalf("%s: %+v, store changed or not reported: %s", script, res, rep)
		}
	}
	assertNoLeftovers(t, s)
}

// aeh sandbox login runs the profile's login with an empty HOME and saves the declared
// files it leaves; a failed login saves nothing.
func TestDockerSandboxLogin(t *testing.T) {
	image := testImageID(t)
	r := Repo{Root: t.TempDir()}
	cfg := func(login string) string {
		return "project: t\ncommands:\n  test: \"true\"\nladder: [a]\nreviewer: b\nprofiles:\n  a:\n    provider: x\n    command: [a]\n" +
			"  b:\n    provider: y\n    command: [b]\n    sandbox:\n      session: [.cfg/tok]\n      login: [sh, -c, " + strconvQuote(login) + "]\n" +
			"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	}
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	write(t, r.Root, "aeh/config.yaml", cfg(`test -z "$(ls -A "$HOME")" || exit 9; mkdir "$HOME/.cfg"; echo t > "$HOME/.cfg/tok"`))
	var out, errw bytes.Buffer
	if err := SandboxLogin(context.Background(), r, "b", in, &out, &errw); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), errw.String())
	}
	sb, err := openSandbox(r, SandboxConfig{Image: image}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sb.sessionDir("b"), ".cfg", "tok")); got != "t\n" {
		t.Fatalf("stored %q", got)
	}
	write(t, r.Root, "aeh/config.yaml", cfg(`mkdir "$HOME/.cfg"; echo u > "$HOME/.cfg/tok"; exit 1`))
	if err := SandboxLogin(context.Background(), r, "b", in, &out, &errw); err == nil || !strings.Contains(err.Error(), "no session was saved") {
		t.Fatalf("failed login: %v", err)
	}
	write(t, r.Root, "aeh/config.yaml", cfg(`true`))
	if err := SandboxLogin(context.Background(), r, "b", in, &out, &errw); err == nil || !strings.Contains(err.Error(), "none of the declared files") {
		t.Fatalf("login without files: %v", err)
	}
	if got := readFile(t, filepath.Join(sb.sessionDir("b"), ".cfg", "tok")); got != "t\n" {
		t.Fatalf("stored %q after failed logins", got)
	}
	if err := SandboxLogin(context.Background(), r, "a", in, &out, &errw); err == nil || !strings.Contains(err.Error(), "declares no sandbox.session") {
		t.Fatalf("profile without session: %v", err)
	}
	assertNoLeftovers(t, sb)
}
