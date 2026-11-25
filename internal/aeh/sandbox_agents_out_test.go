package aeh

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// outTar builds a docker cp style archive of /out.
func outTar(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "out/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, h := range entries {
		h := h
		body := strings.Repeat("x", int(h.Size))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReadAgentOutput(t *testing.T) {
	ok := outTar(t, tar.Header{Name: "out/features/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "out/features/cart.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	got, err := readAgentOutput(bytes.NewReader(ok), 4, 100)
	if err != nil || string(got.Files["features/cart.md"]) != "xxx" || len(got.Dirs) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	for name, archive := range map[string][]byte{
		"link":       outTar(t, tar.Header{Name: "out/leak", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"too many":   outTar(t, tar.Header{Name: "out/a", Typeflag: tar.TypeReg, Size: 1}, tar.Header{Name: "out/b", Typeflag: tar.TypeReg, Size: 1}),
		"too large":  outTar(t, tar.Header{Name: "out/a", Typeflag: tar.TypeReg, Size: 200}),
		"escape":     outTar(t, tar.Header{Name: "elsewhere/a", Typeflag: tar.TypeReg, Size: 1}),
		"device":     outTar(t, tar.Header{Name: "out/null", Typeflag: tar.TypeChar}),
		"stream cap": outTar(t, tar.Header{Name: "out/a", Typeflag: tar.TypeReg, Size: 1 << 20}),
	} {
		maxFiles := 1
		if name == "link" || name == "escape" || name == "device" {
			maxFiles = 4
		}
		_, err := readAgentOutput(bytes.NewReader(archive), maxFiles, 100)
		var content *agentContentError
		if !errors.As(err, &content) {
			t.Errorf("%s: %v is not the agent's failure", name, err)
		}
	}
	// Names the host could not represent fail the manifest check, as on the host.
	bad := agentOutput{Files: map[string][]byte{"questions.yaml": []byte("questions: []\n")}, Dirs: []string{"CON"}}
	if _, err := bad.freeze(questionsManifest()); err == nil {
		t.Fatal("a reserved directory name was accepted")
	}
}

func TestInspectionExportStripsLikeTheHost(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	for _, p := range []string{"src/a.go", ".env", "keys/server.pem", "vendor/x.txt", "docs/secrets.md"} {
		write(t, dir, p, "x\n")
	}
	mustGit(t, dir, "add", "-A")
	blob := mustGitInput(t, dir, "target", "hash-object", "-w", "--stdin")
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "120000,"+blob+",link")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "tree")
	e, excluded, err := newInspectionExport(dir, mustGit(t, dir, "rev-parse", "HEAD"), []string{"vendor/**"})
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range e.Files {
		kept = append(kept, f.Path)
	}
	if strings.Join(kept, ",") != "src/a.go" || strings.Join(excluded, ",") != ".env,docs/secrets.md,keys/server.pem,link,vendor/x.txt" {
		t.Fatalf("kept %v, excluded %v", kept, excluded)
	}
}

// sandboxedDiscoveryConfig turns the discovery fixture's config into inline profiles with
// agents in containers.
func sandboxedDiscoveryConfig(t *testing.T, f *discoveryFixture, image, agent, extra string) {
	t.Helper()
	cfg := "project: test\ncommands:\n  build: \"sh tests/build.sh\"\n  test: \"sh tests/test.sh\"\n  timeout_minutes: 1\nladder: [agent]\n" + extra +
		"profiles:\n  agent:\n    provider: a\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(agent) + ", aeh, \"{{prompt}}\"]\n" +
		"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
}

// A discovery round under the sandbox: the agent writes to /out, sees memory read-only,
// and nothing it writes reaches the host's file system except through the round's checks.
func TestDockerSandboxedDiscoveryRound(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, askTwo)
	agent := `test "$(pwd)" = /out || exit 5; test -d /memory/wiki || exit 6; ` + askTwo
	sandboxedDiscoveryConfig(t, f, image, agent, "")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "sandbox")
	if _, err := DiscoverNew(f.repo, "shop", "A small online shop."); err != nil {
		t.Fatal(err)
	}
	st := f.run()
	if len(st.Rounds) != 1 || len(st.Rounds[0].Questions) != 2 {
		t.Fatalf("round: %+v", st)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(st.Attempts[0].RunDir), "discover.launch.json"))
	var rec launchRecord
	if err != nil || json.Unmarshal(b, &rec) != nil || !rec.Out || rec.MemoryCommit == "" || rec.Class != ClassProcess {
		t.Fatalf("launch %s %v", b, err)
	}
}

// An adoption inventory under the sandbox: the inspector reads a filtered, read-only
// export at /work and writes its document to /out.
func TestDockerSandboxedAdoptInventory(t *testing.T) {
	image := testImageID(t)
	f := newAdoptFixture(t, inventoryOK)
	write(t, f.dir, ".env", "TOKEN=x\n")
	agent := `test "$(pwd)" = /work || exit 5; test ! -e .env || exit 6; test -r src/billing/fee.txt || exit 7; ` +
		`if (echo x >> src/billing/fee.txt) 2>/dev/null; then exit 8; fi; ` +
		// The prompt names /out; the fixed path avoids parsing it with the image's sed.
		strings.Replace(inventoryOK, adoptOutputDir, "set -eu\nout=/out\ngrep -q '^/out' /run/aeh/prompt.md\n", 1)
	sandboxedDiscoveryConfig(t, f, image, agent, "")
	base := f.commitCode("sandbox")
	var log strings.Builder
	st, err := AdoptInventory(context.Background(), f.repo, "", &log)
	if err != nil || st.Status != "accepted" {
		errText, _ := os.ReadFile(filepath.Join(f.repo.RunDir("adopt", "a1"), "adopt.err"))
		outText, _ := os.ReadFile(filepath.Join(f.repo.RunDir("adopt", "a1"), "adopt.out"))
		t.Fatalf("inventory: %+v %v\n%s\nstderr:\n%s\nstdout:\n%s", st, err, log.String(), errText, outText)
	}
	if st.Inspection.Base != base || st.Inspection.Excluded != 1 {
		t.Fatalf("inventory: %+v", st)
	}
	if !strings.Contains(f.memFile("wiki/system.md"), "src/billing/fee.txt:1") {
		t.Fatal("system.md not recorded")
	}
}

// The book reviewer under the sandbox reads the book at /work, read-only, and writes its
// findings to /out.
func TestDockerSandboxedBookReview(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, writeBook)
	review := `test -r /work/features/cart.md || exit 5; if (echo x >> /work/features/cart.md) 2>/dev/null; then exit 6; fi; cd /out; ` + approveBook
	cfg := "project: test\ncommands:\n  test: \"true\"\nladder: [agent]\nreviewers:\n  qa: [qa]\n" +
		"profiles:\n  agent:\n    provider: a\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(writeBook) + ", aeh, \"{{prompt}}\"]\n" +
		"  qa:\n    provider: b\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(review) + ", aeh, \"{{prompt}}\"]\n" +
		"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "sandbox")
	f.start()
	st := f.run()
	if st.Phase != DiscoveryDraft || len(st.Rounds) != 1 {
		t.Fatalf("book state: %+v", st)
	}
	if r := st.Rounds[0]; r.Review == nil || r.Review.Status != "approve" || len(r.Questions) != 2 {
		t.Fatalf("review: %+v", r)
	}
}
