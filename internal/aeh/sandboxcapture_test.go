package aeh

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// exportedTree reads a commit's export back into memory, as an agent would find it.
func exportedTree(t *testing.T, dir string, e *commitExport) (map[string][]byte, map[string]bool) {
	t.Helper()
	files, exec := map[string][]byte{}, map[string]bool{}
	err := e.source()(func(name string, executable bool, size int64, r io.Reader) error {
		var b bytes.Buffer
		if _, err := b.ReadFrom(r); err != nil {
			return err
		}
		files[name], exec[name] = b.Bytes(), executable
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, exec
}

func captureFixture(t *testing.T) (string, string, *commitExport, *ignoreChecker) {
	t.Helper()
	dir, base := exportRepo(t)
	e, err := newCommitExport(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	return dir, base, e, mustIgnore(t, dir, base, e)
}

func captureTar(t *testing.T, files map[string][]byte, exec map[string]bool) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	if err := writeVolumeTar(&buf, "work", treeFiles(files, exec)); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestCaptureWithoutChangesGivesTheBaseTree(t *testing.T) {
	dir, base, e, rules := captureFixture(t)
	files, exec := exportedTree(t, dir, e)
	commit, err := captureCandidate(dir, base, e, rules, captureTar(t, files, exec), t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustGit(t, dir, "rev-parse", commit+"^{tree}"), mustGit(t, dir, "rev-parse", base+"^{tree}"); got != want {
		t.Fatalf("capture changed the tree: %s, base %s\n%s", got, want, mustGit(t, dir, "diff", "--stat", base, commit))
	}
	if parent := mustGit(t, dir, "rev-parse", commit+"^"); parent != base {
		t.Fatalf("parent %s, want %s", parent, base)
	}
}

func TestCaptureRecordsChangesWithoutFilters(t *testing.T) {
	dir, base, e, rules := captureFixture(t)
	files, exec := exportedTree(t, dir, e)
	files["a.txt"] = []byte("one\r\nchanged\n")    // bytes kept as written: no eol conversion
	files["x.f"] = []byte("no smudge or clean\n")  // the evil filter never runs
	files["new.txt"] = []byte("new\n")             // new, not ignored
	files["out/build.bin"] = []byte("ignored")     // new under an ignored directory
	files["debug.log"] = []byte("ignored")         // new, ignored by pattern
	files["keep.log"] = []byte("re-included")      // new, re-included by !keep.log
	files["sub/gen/x"] = []byte("ignored by sub/") // nested .gitignore
	files["raw.bin/inner"] = []byte("file became a dir")
	delete(files, "raw.bin")
	delete(files, "v.sub")
	exec["plain.sh"] = true
	exec["tool.sh"] = false
	commit, err := captureCandidate(dir, base, e, rules, captureTar(t, files, exec), t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	status := mustGit(t, dir, "diff", "--name-status", "--no-renames", base, commit)
	for _, want := range []string{"M\ta.txt", "M\tx.f", "A\tnew.txt", "A\tkeep.log", "D\traw.bin", "A\traw.bin/inner", "D\tv.sub", "M\tplain.sh", "M\ttool.sh"} {
		if !strings.Contains(status, want) {
			t.Errorf("missing %q in:\n%s", want, status)
		}
	}
	for _, absent := range []string{"out/build.bin", "debug.log", "sub/gen/x"} {
		if strings.Contains(status, absent) {
			t.Errorf("ignored %s was captured:\n%s", absent, status)
		}
	}
	if blob, _ := gitBlob(dir, commit, "a.txt"); blob != "one\r\nchanged\n" {
		t.Errorf("a.txt stored as %q", blob)
	}
	if blob, _ := gitBlob(dir, commit, "x.f"); blob != "no smudge or clean\n" {
		t.Errorf("x.f stored as %q", blob)
	}
	modes := mustGit(t, dir, "ls-tree", commit, "plain.sh", "tool.sh")
	if !strings.Contains(modes, "100755 blob") || !strings.Contains(modes, "100644 blob") ||
		strings.Index(modes, "100755") > strings.Index(modes, "tool.sh") {
		t.Errorf("modes not captured: %s", modes)
	}
}

func TestCaptureRefusesLinksAndGitPaths(t *testing.T) {
	dir, base, e, rules := captureFixture(t)
	for name, entry := range map[string]*tar.Header{
		"symlink":  {Name: "work/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777},
		"hardlink": {Name: "work/hard", Typeflag: tar.TypeLink, Linkname: "work/a.txt", Mode: 0o644},
		"fifo":     {Name: "work/pipe", Typeflag: tar.TypeFifo, Mode: 0o644},
		"git file": {Name: "work/.git", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"git dir":  {Name: "work/sub/.GIT/config", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"escape":   {Name: "work/../etc/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "work/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)})
		entry.ModTime = time.Unix(0, 0)
		if err := tw.WriteHeader(entry); err != nil {
			t.Fatal(err)
		}
		if entry.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		if _, err := captureCandidate(dir, base, e, rules, bytes.NewReader(buf.Bytes()), t.TempDir(), "capture"); err == nil {
			t.Errorf("%s: captured", name)
		}
	}
}

func TestCaptureLimitsFileSize(t *testing.T) {
	dir, base, e, rules := captureFixture(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "work/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: captureFileLimit + 1, ModTime: time.Unix(0, 0)})
	// The header alone must stop the capture: no byte of the body is needed.
	_, err := captureCandidate(dir, base, e, rules, bytes.NewReader(buf.Bytes()), t.TempDir(), "capture")
	if err == nil || !strings.Contains(err.Error(), "per file") {
		t.Fatalf("got %v", err)
	}
}

func TestAgentArgvUsesContainerPaths(t *testing.T) {
	p := Profile{Command: []string{"codex", "exec", "--add-dir", "{{output_dir}}", "-f", "{{prompt_file}}", "{{prompt}}"}}
	argv, err := agentArgv(p, "do {{prompt_file}}", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "exec", "--add-dir", "/out", "-f", "/run/aeh/prompt.md", "do {{prompt_file}}"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Fatalf("argv %q", argv)
	}
	if _, err := agentArgv(p, "x", false); err == nil {
		t.Fatal("an output directory placeholder without /out must be refused")
	}
	argv, _ = agentArgv(Profile{Command: []string{"claude", "-p"}, Stdin: true}, "x", false)
	if argv[0] != "/bin/sh" || argv[2] != `exec "$@" < /run/aeh/prompt.md` || argv[4] != "claude" {
		t.Fatalf("stdin wrapper %q", argv)
	}
}

func TestAgentEnvRefusesReservedNames(t *testing.T) {
	t.Setenv("AEH_TEST_TOKEN", "secret")
	env, err := agentEnv(Profile{Env: map[string]string{"MODE": "x"}}, []string{"AEH_TEST_TOKEN", "AEH_TEST_UNSET"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env, ",") != "AEH_TEST_TOKEN=secret,MODE=x" {
		t.Fatalf("env %q", env)
	}
	for _, bad := range []Profile{{Env: map[string]string{"HOME": "/x"}}, {Env: map[string]string{"https_proxy": "x"}}, {Env: map[string]string{"A-B": "x"}}} {
		if _, err := agentEnv(bad, nil); err == nil {
			t.Errorf("accepted %v", bad.Env)
		}
	}
	if _, err := agentEnv(Profile{}, []string{"TMPDIR"}); err == nil {
		t.Error("accepted TMPDIR in pass_env")
	}
}

func TestAgentErrorKeepsTheHostMeanings(t *testing.T) {
	p := Profile{QuotaExitCodes: []int{75}}
	cases := map[LaunchResult]string{
		{Class: ClassProcess}:                                     "",
		{Class: ClassProcess, ExitCode: 2}:                        "agent exited with code 2",
		{Class: ClassProcess, ExitCode: 75}:                       "provider quota signal (exit 75)",
		{Class: ClassTerminated, Reason: "timed out after 1m0s"}:  "deadline exceeded",
		{Class: ClassInfrastructure, Reason: "image missing"}:     "sandbox infrastructure failure: image missing",
		{Class: ClassResource, Reason: "killed for exceeding..."}: "agent killed",
	}
	for res, want := range cases {
		err := agentError(p, res)
		if want == "" {
			if err != nil {
				t.Errorf("%v: %v", res, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: got %v, want %q", res, err, want)
		}
	}
	if !uncertainExit(agentError(p, LaunchResult{Class: ClassTerminated, Reason: "timed out after 1m0s"})) {
		t.Error("a timeout must stay uncertain")
	}
}
