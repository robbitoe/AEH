package aeh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// exportRepo builds a repository whose checkout would differ from its objects:
// autocrlf, an eol attribute, a smudge filter and export-subst all apply on checkout.
func exportRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitattributes", "*.txt eol=crlf\n*.sub export-subst\n*.f filter=evil\n")
	write(".gitignore", "out/\n*.log\n!keep.log\n")
	write("sub/.gitignore", "gen/\n")
	write("a.txt", "one\ntwo\n")
	write("raw.bin", "\x00\xff\r\n")
	write("v.sub", "$Format:%H$\n")
	write("x.f", "filtered\n")
	write("tool.sh", "#!/bin/sh\necho tool-ran\n")
	write("plain.sh", "#!/bin/sh\necho plain-ran\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "update-index", "--chmod=+x", "tool.sh")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "export fixture")
	mustGit(t, dir, "config", "core.autocrlf", "true")
	mustGit(t, dir, "config", "filter.evil.smudge", "echo SMUDGED")
	return dir, mustGit(t, dir, "rev-parse", "HEAD")
}

func TestCommitExportStreamsRawObjects(t *testing.T) {
	dir, commit := exportRepo(t)
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeVolumeTar(&buf, "work", e.source()); err != nil {
		t.Fatal(err)
	}
	back, err := readTree(bytes.NewReader(buf.Bytes()), "work", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range e.Files {
		blob, _ := gitBlob(dir, commit, f.Path)
		sum := sha256.Sum256([]byte(blob))
		if back[f.Path].SHA256 != hex.EncodeToString(sum[:]) || e.sums[f.Path] != back[f.Path].SHA256 {
			t.Errorf("%s: exported bytes differ from the object", f.Path)
		}
		if back[f.Path].Executable != (f.Path == "tool.sh") {
			t.Errorf("%s: executable %v", f.Path, back[f.Path].Executable)
		}
	}
	if err := checkExport(e, back, mustIgnore(t, dir, commit, e)); err != nil {
		t.Fatalf("export then read back with no change must pass: %v", err)
	}
}

func mustIgnore(t *testing.T, dir, commit string, e *commitExport) *ignoreChecker {
	t.Helper()
	c, err := newIgnoreChecker(dir, commit, e.Files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCommitExportRefusesLinksAndSubmodules(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "a")
	target := mustGit(t, dir, "hash-object", "-w", "a")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+target+",link")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",mod")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "links")
	_, err := newCommitExport(dir, mustGit(t, dir, "rev-parse", "HEAD"))
	if err == nil || !strings.Contains(err.Error(), "symbolic link link") || !strings.Contains(err.Error(), "submodule mod") {
		t.Fatalf("expected refusal naming both, got %v", err)
	}
}

func TestCheckExportFindsChanges(t *testing.T) {
	dir, commit := exportRepo(t)
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeVolumeTar(&buf, "work", e.source()); err != nil {
		t.Fatal(err)
	}
	base, err := readTree(bytes.NewReader(buf.Bytes()), "work", 0)
	if err != nil {
		t.Fatal(err)
	}
	rules := mustIgnore(t, dir, commit, e)
	clone := func() map[string]treeEntry {
		m := map[string]treeEntry{}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	allowed := clone()
	allowed["out/bin/app"] = treeEntry{Executable: true, SHA256: "x"}
	allowed["build.log"] = treeEntry{SHA256: "x"}
	allowed["sub/gen/code.go"] = treeEntry{SHA256: "x"}
	if err := checkExport(e, allowed, rules); err != nil {
		t.Fatalf("ignored build output rejected: %v", err)
	}
	cases := map[string]func(map[string]treeEntry){
		"modified: a.txt":                                         func(m map[string]treeEntry) { v := m["a.txt"]; v.SHA256 = "y"; m["a.txt"] = v },
		"removed or replaced: x.f":                                func(m map[string]treeEntry) { delete(m, "x.f") },
		"mode changed: plain.sh":                                  func(m map[string]treeEntry) { v := m["plain.sh"]; v.Mode = 0o755; m["plain.sh"] = v },
		"not ignored by the commit's .gitignore: gen/x":           func(m map[string]treeEntry) { m["gen/x"] = treeEntry{} },
		"mode changed: tool.sh (0655":                             func(m map[string]treeEntry) { v := m["tool.sh"]; v.Mode = 0o655; m["tool.sh"] = v },
		"mode changed: x.f (0000":                                 func(m map[string]treeEntry) { v := m["x.f"]; v.Mode = 0; m["x.f"] = v },
		"mode changed: sub/ (0700":                                func(m map[string]treeEntry) { v := m["sub"]; v.Mode = 0o700; m["sub"] = v },
		"removed or replaced: sub/":                               func(m map[string]treeEntry) { delete(m, "sub") },
		"removed or replaced: a.txt":                              func(m map[string]treeEntry) { m["a.txt"] = treeEntry{Dir: true, Mode: 0o755} },
		"not ignored by the commit's .gitignore: OUT/injected.go": func(m map[string]treeEntry) { m["OUT/injected.go"] = treeEntry{} },
		"not ignored by the commit's .gitignore: keep.log":        func(m map[string]treeEntry) { m["keep.log"] = treeEntry{} },
	}
	for want, mutate := range cases {
		m := clone()
		mutate(m)
		if err := checkExport(e, m, rules); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestIgnoreCheckerIgnoresUserExcludes(t *testing.T) {
	dir, commit := exportRepo(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "git", "ignore"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", home)
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mustIgnore(t, dir, commit, e).ignored([]string{"secret.txt", "x.log"})
	if err != nil {
		t.Fatal(err)
	}
	if got["secret.txt"] || !got["x.log"] {
		t.Fatalf("ignored %v", got)
	}
}

func TestDockerGateSequence(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{GateEnv: map[string]string{"GREETING": "hi"}})
	dir, commit := exportRepo(t)
	ctx := context.Background()
	seq, err := s.newGateSequence(ctx, dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	// Build produces an ignored executable; test and acceptance need it.
	g := seq.command(ctx, "build", `mkdir -p out && printf '#!/bin/sh\necho "built $GREETING"\n' > out/app && chmod +x out/app && echo log > b.log`, time.Minute, nil)
	if !g.Passed {
		t.Fatalf("build: %+v", g)
	}
	g = seq.command(ctx, "test", `./out/app && ./tool.sh && ! grep -q "$(printf '\r')" a.txt`, time.Minute, nil)
	if !g.Passed || !strings.Contains(g.Output, "built hi") || !strings.Contains(g.Output, "tool-ran") {
		t.Fatalf("test: %+v", g)
	}
	g = seq.command(ctx, "acceptance", `test "$(cat /run/aeh/manifest)" = "tests/a.test" && ./plain.sh`, time.Minute, map[string][]byte{"manifest": []byte("tests/a.test")})
	if g.Passed || g.Class != ClassProcess || !g.provesResult() {
		t.Fatalf("a 100644 file must not run: %+v", g)
	}
	if err := seq.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, s)
}

func TestDockerGateSequenceCanaries(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	dir, commit := exportRepo(t)
	ctx := context.Background()
	cases := map[string]struct{ cmd, want string }{
		"link planted":         {"ln -s /etc/passwd out-link", "not a regular file"},
		"ignored link planted": {"mkdir out && ln -s /etc out/etc", "not a regular file"},
		"sealed file replaced": {"echo forged > a.txt", "modified: a.txt"},
		"tracked file removed": {"rm x.f", "removed or replaced: x.f"},
		"made executable":      {"chmod +x plain.sh", "mode changed: plain.sh"},
		"new tracked-looking":  {"echo x > new.go", "not ignored by the commit's .gitignore: new.go"},
		"file to directory":    {"rm a.txt && mkdir a.txt", "removed or replaced: a.txt"},
	}
	for name, c := range cases {
		seq, err := s.newGateSequence(ctx, dir, commit)
		if err != nil {
			t.Fatal(err)
		}
		g := seq.command(ctx, "build", c.cmd, time.Minute, nil)
		if g.Passed || !strings.Contains(g.Output, c.want) || seq.Broken() == "" {
			t.Errorf("%s: %+v", name, g)
		}
		if next := seq.command(ctx, "test", "true", time.Minute, nil); next.Passed || next.provesResult() {
			t.Errorf("%s: a command ran on a broken volume: %+v", name, next)
		}
		if err := seq.Close(); err != nil {
			t.Fatal(err)
		}
	}
	assertNoLeftovers(t, s)
}

func TestHostPathKeys(t *testing.T) {
	if err := checkHostPaths([]string{"a/.gitignore", "b/.gitignore"}); err != nil {
		t.Fatal(err)
	}
	clash := checkHostPaths([]string{"A/.gitignore", "a/.gitignore"})
	if folded := runtime.GOOS == "windows" || runtime.GOOS == "darwin"; folded != (clash != nil) {
		t.Fatalf("case collision on %s: %v", runtime.GOOS, clash)
	}
	if runtime.GOOS == "windows" {
		for _, bad := range []string{"dir./.gitignore", "dir /.gitignore", "con/.gitignore", "aux.txt/.gitignore", "a:b/.gitignore"} {
			if err := checkHostPaths([]string{bad}); err == nil {
				t.Errorf("%s accepted", bad)
			}
		}
	}
}

func TestIgnoreCheckerRefusesCollidingRules(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-insensitive hosts only")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "core.ignorecase", "false")
	empty := mustGit(t, dir, "hash-object", "-w", "--stdin")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+empty+",A/.gitignore")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+empty+",a/.gitignore")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "colliding rules")
	commit := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newIgnoreChecker(dir, commit, e.Files); err == nil || !strings.Contains(err.Error(), "same file") {
		t.Fatalf("colliding ignore files accepted: %v", err)
	}
}

func TestDockerGateSequenceUnverifiedAndLongOutput(t *testing.T) {
	s, _ := dockerSandbox(t, SandboxConfig{})
	dir, commit := exportRepo(t)
	ctx := context.Background()
	seq, err := s.newGateSequence(ctx, dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	// The command changes a tracked file, then times out: nothing was read back, so the
	// volume must not be used again.
	g := seq.command(ctx, "build", "echo forged > a.txt; sleep 60", 3*time.Second, nil)
	if g.Passed || !g.Uncertain || seq.Broken() == "" {
		t.Fatalf("timeout: %+v broken %q", g, seq.Broken())
	}
	if next := seq.command(ctx, "test", "true", time.Minute, nil); next.Passed {
		t.Fatalf("ran on an unverified volume: %+v", next)
	}
	if err := seq.Close(); err != nil {
		t.Fatal(err)
	}
	seq, err = s.newGateSequence(ctx, dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	g = seq.command(ctx, "build", "yes 'a long build log line' | head -c 300000; echo forged > a.txt", time.Minute, nil)
	if g.Passed || !strings.Contains(g.Output, "modified: a.txt") || !strings.Contains(g.Output, "long build log") {
		t.Fatalf("integrity diagnostic lost in long output:\n%s", tail(g.Output, 800))
	}
	if err := seq.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, s)
}
