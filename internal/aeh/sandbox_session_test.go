package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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

// dirLink links link to the directory target: a symbolic link, or on Windows without
// that privilege a junction.
func dirLink(target, link string) error {
	err := os.Symlink(target, link)
	if err != nil && runtime.GOOS == "windows" {
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); jerr != nil {
			return fmt.Errorf("%v; junction: %v %s", err, jerr, out)
		}
		return nil
	}
	return err
}

// privateProject makes root a Git repository that ignores .aeh/, as aeh init does.
func privateProject(t *testing.T, root string) {
	t.Helper()
	mustGit(t, root, "init", "-q")
	write(t, root, ".gitignore", ".aeh/\n")
}

// The store lives in the main checkout of a project that keeps .aeh/ private, and every
// directory on the way must be a plain directory.
func TestSessionStoreLocation(t *testing.T) {
	root := t.TempDir()
	mustGit(t, root, "init", "-q")
	if _, err := sessionStore(root, "p", true); err == nil || !strings.Contains(err.Error(), "not ignored") {
		t.Fatalf("store in a project that does not ignore .aeh/: %v", err)
	}
	write(t, root, ".gitignore", ".aeh/\n")
	store, err := sessionStore(root, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".aeh", "sandbox", "sessions", sessionKey("p")); store != want {
		t.Fatalf("store %s, want %s", store, want)
	}
	if sessionKey("a/b") == sessionKey("a_b") {
		t.Fatal("two profile names share a store")
	}
	// A file where a directory of a declared path belongs is refused, never replaced.
	write(t, store, ".cfg", "x")
	if err := storeSessionFile(store, ".cfg/tok", []byte("v")); err == nil {
		t.Fatal("stored through a file")
	}
	if _, err := loadSession(store, []string{".cfg/tok"}); err == nil {
		t.Fatal("loaded through a file")
	}
	outside := t.TempDir()
	other := filepath.Join(root, ".aeh", "sandbox", "sessions", sessionKey("q"))
	if err := dirLink(outside, other); err != nil {
		t.Skipf("cannot create a link here: %v", err)
	}
	if _, err := sessionStore(root, "q", true); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("a linked store was used: %v", err)
	}
	if err := dirLink(outside, filepath.Join(store, ".codex")); err != nil {
		t.Fatal(err)
	}
	if err := storeSessionFile(store, ".codex/auth.json", []byte("v")); err == nil {
		t.Fatal("stored through a linked directory")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the store: %v", entries)
	}
}

// On Windows the sessions directory gets a protected access list: the current user and
// SYSTEM only, nothing inherited from the checkout.
func TestSessionStoreIsPrivate(t *testing.T) {
	root := t.TempDir()
	privateProject(t, root)
	store, err := sessionStore(root, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Dir(store)
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(sessions)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("sessions mode %v (%v)", fi.Mode(), err)
		}
		return
	}
	if err := storeSessionFile(store, ".cfg/tok", []byte("v")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{sessions, store, filepath.Join(store, ".cfg", "tok")} {
		out, err := exec.Command("icacls", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		text := string(out)
		entries := 0
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), p) // the first entry follows the path
			if strings.Contains(line, ":(") {
				entries++
			}
			for _, bad := range []string{"Everyone", "BUILTIN\\Users", "Authenticated Users", "Administrators"} {
				if strings.Contains(line, bad) {
					t.Errorf("%s grants %s:\n%s", p, bad, text)
				}
			}
		}
		if entries != 2 || !strings.Contains(text, "SYSTEM") {
			t.Errorf("%s access list:\n%s", p, text)
		}
	}
}

func TestLockSessionWaits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
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
	s, r := dockerSandbox(t, SandboxConfig{})
	privateProject(t, r.Root)
	s.agent = s.image
	p := Profile{TimeoutMinutes: 2, Sandbox: &ProfileSandbox{Session: []string{".cfg/tok", ".cfg/extra"}}}
	dir, err := sessionStore(r.Root, "medio", true)
	if err != nil {
		t.Fatal(err)
	}
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
	privateProject(t, r.Root)
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
	store, err := sessionStore(r.Root, "b", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(store, ".cfg", "tok")); got != "t\n" {
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
	if got := readFile(t, filepath.Join(store, ".cfg", "tok")); got != "t\n" {
		t.Fatalf("stored %q after failed logins", got)
	}
	if err := SandboxLogin(context.Background(), r, "a", in, &out, &errw); err == nil || !strings.Contains(err.Error(), "declares no sandbox.session") {
		t.Fatalf("profile without session: %v", err)
	}
	assertNoLeftovers(t, sb)
}

// A login that outlives its context is stopped through its container, and leaves no
// session and no Docker object.
func TestDockerSandboxLoginCancelled(t *testing.T) {
	image := testImageID(t)
	r := Repo{Root: t.TempDir()}
	privateProject(t, r.Root)
	write(t, r.Root, "aeh/config.yaml", "project: t\ncommands:\n  test: \"true\"\nladder: [a]\nreviewer: a\nprofiles:\n  a:\n    provider: x\n    command: [a]\n"+
		"    sandbox:\n      session: [tok]\n      login: [sh, -c, \"echo t > $HOME/tok; sleep 60\"]\n"+
		"sandbox:\n  image: \""+image+"\"\n  agent_image: \""+image+"\"\n  memory: 256m\n")
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	var out bytes.Buffer
	if err := SandboxLogin(ctx, r, "a", in, &out, &out); err == nil || !strings.Contains(err.Error(), "no session was saved") {
		t.Fatalf("cancelled login: %v\n%s", err, out.String())
	}
	if d := time.Since(start); d > 40*time.Second {
		t.Fatalf("the cancelled login took %s", d)
	}
	store, err := sessionStore(r.Root, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if files, err := loadSession(store, []string{"tok"}); err != nil || len(files) != 0 {
		t.Fatalf("saved %v (%v)", files, err)
	}
	sb, err := openSandbox(r, SandboxConfig{Image: image}, "x")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, sb)
}
