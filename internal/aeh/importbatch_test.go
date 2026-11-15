package aeh

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var roundManifest = outputManifest{
	Name: "round",
	Rules: []outputRule{
		{Pattern: "questions.yaml", Kind: outputYAML, MaxBytes: 1000, MaxCount: 1, Required: true},
		{Pattern: "features/*.md", Kind: outputMarkdown, MaxBytes: 1000, MaxCount: 2},
		{Pattern: "mockups/*/*.html", Kind: outputHTML, MaxBytes: 1000, MaxCount: 3},
	},
	MaxFiles: 5,
	MaxTotal: 2500,
}

func writeOutput(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFreezeOutputAndReload(t *testing.T) {
	dir := writeOutput(t, map[string]string{
		"questions.yaml":         "questions:\n  - id: R1-Q1\n",
		"features/cart.md":       "# Cart\n",
		"mockups/cart/list.html": "<p>Cart</p>\n",
	})
	f, err := freezeOutput(dir, roundManifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Files) != 3 || string(f.Files["features/cart.md"]) != "# Cart\n" || len(f.Digest) != 64 {
		t.Fatalf("frozen %+v", f)
	}
	// The snapshot is independent of the directory: later edits do not reach it.
	write(t, dir, "features/cart.md", "# Changed\n")
	if again, err := freezeOutput(dir, roundManifest); err != nil || again.Digest == f.Digest {
		t.Fatalf("an edited output must freeze to another digest: %v", err)
	}

	p := filepath.Join(t.TempDir(), "snap.json")
	if err := saveFrozen(p, f); err != nil {
		t.Fatal(err)
	}
	got, err := loadFrozen(p, roundManifest)
	if err != nil || got.Digest != f.Digest || string(got.Files["features/cart.md"]) != "# Cart\n" {
		t.Fatalf("reload: %+v %v", got, err)
	}
	other := roundManifest
	other.Name = "inventory"
	if _, err := loadFrozen(p, other); err == nil || !strings.Contains(err.Error(), "not \"inventory\"") {
		t.Fatalf("expected a manifest mismatch, got %v", err)
	}

	tampered := f
	tampered.Files = map[string][]byte{}
	for k, v := range f.Files {
		tampered.Files[k] = v
	}
	tampered.Files["features/cart.md"] = []byte("# Evil\n")
	if err := saveFrozen(p, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFrozen(p, roundManifest); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("expected a digest mismatch, got %v", err)
	}
	tampered.Files["notes.txt"] = []byte("x")
	tampered.Digest = outputDigest(roundManifest.Name, tampered.Files)
	if err := saveFrozen(p, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFrozen(p, roundManifest); err == nil || !strings.Contains(err.Error(), "not an expected file") {
		t.Fatalf("a reloaded snapshot must pass the manifest again, got %v", err)
	}
}

func TestFreezeOutputRejects(t *testing.T) {
	base := map[string]string{"questions.yaml": "q: 1\n"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"unexpected file", with(map[string]string{"notes.txt": "x"}), "not an expected file"},
		{"unexpected depth", with(map[string]string{"features/a/b.md": "x"}), "not an expected file"},
		{"hidden file", with(map[string]string{"features/.x.md": "x"}), "not starting with '.'"},
		{"reserved name", with(map[string]string{"features/log.md": "x"}), "only aeh writes"},
		{"approval file", map[string]string{"questions.yaml": "q: 1\n", "approval.yaml": "x: 1\n"}, "only aeh writes"},
		{"too big", with(map[string]string{"features/a.md": strings.Repeat("x", 1001)}), "exceeds 1000 bytes"},
		{"too many of a kind", with(map[string]string{"features/a.md": "a", "features/b.md": "b", "features/c.md": "c"}), "more than 2 files match"},
		{"too many files", with(map[string]string{"features/a.md": "a", "features/b.md": "b", "mockups/a/1.html": "1", "mockups/a/2.html": "2", "mockups/a/3.html": "3"}), "more than 5 files"},
		{"too many bytes", with(map[string]string{"features/a.md": strings.Repeat("a", 900), "features/b.md": strings.Repeat("b", 900), "mockups/a/1.html": strings.Repeat("c", 900)}), "in total"},
		{"anchor", map[string]string{"questions.yaml": "a: &x 1\nb: *x\n"}, "anchors"},
		{"tag", map[string]string{"questions.yaml": "a: !!str 1\n"}, "tags"},
		{"two documents", map[string]string{"questions.yaml": "a: 1\n---\nb: 2\n"}, "exactly one"},
		{"empty yaml", map[string]string{"questions.yaml": ""}, "empty"},
		{"invalid utf8", with(map[string]string{"features/a.md": "\xff\xfe"}), "UTF-8"},
		{"nul byte", with(map[string]string{"features/a.md": "a\x00b"}), "NUL"},
		{"missing required", map[string]string{"features/a.md": "x"}, "missing questions.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := freezeOutput(writeOutput(t, c.files), roundManifest)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// Names a case-insensitive disk cannot hold are checked on the file set itself.
func TestOutputCheckRejectsNames(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"case collision": {"questions.yaml": []byte("q: 1\n"), "features/A.md": []byte("a"), "features/a.md": []byte("a")},
		"device name":    {"questions.yaml": []byte("q: 1\n"), "features/CON.md": []byte("a")},
		"trailing dot":   {"questions.yaml": []byte("q: 1\n"), "features/a.md.": []byte("a")},
		"format char":    {"questions.yaml": []byte("q: 1\n"), "features/a‮.md": []byte("a")},
		"dot segment":    {"questions.yaml": []byte("q: 1\n"), "features/../x.md": []byte("a")},
		"non-ascii":      {"questions.yaml": []byte("q: 1\n"), "features/café.md": []byte("a")},
		"leading dash":   {"questions.yaml": []byte("q: 1\n"), "features/-a.md": []byte("a")},
		"directory case": {"questions.yaml": []byte("q: 1\n"), "mockups/Cart/a.html": []byte("a"), "mockups/cart/b.html": []byte("b")},
	} {
		if err := roundManifest.check(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFreezeOutputRejectsLinks(t *testing.T) {
	dir := writeOutput(t, map[string]string{"questions.yaml": "q: 1\n"})
	target := filepath.Join(t.TempDir(), "secret.md")
	write(t, filepath.Dir(target), "secret.md", "secret")
	if err := os.MkdirAll(filepath.Join(dir, "features"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "features", "a.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := freezeOutput(dir, roundManifest); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("expected a link error, got %v", err)
	}
}

func TestFreezeOutputRejectsJunctions(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are a Windows reparse point")
	}
	dir := writeOutput(t, map[string]string{"questions.yaml": "q: 1\n", "mockups/x/.keep": ""})
	if err := os.Remove(filepath.Join(dir, "mockups", "x", ".keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "mockups", "x")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	write(t, outside, "a.html", "<p>outside</p>")
	if out, err := exec.Command("cmd", "/C", "mklink", "/J", filepath.Join(dir, "mockups", "x"), outside).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if _, err := freezeOutput(dir, roundManifest); err == nil || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("expected a reparse point error, got %v", err)
	}
}

func memoryHead(t *testing.T, r Repo) string {
	t.Helper()
	return mustGit(t, memDir(t, r), "rev-parse", "HEAD")
}

func TestImportBatchCommitsOnceWithMarker(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	head := memoryHead(t, r)
	b := memoryBatch{
		Session:      "discover/shop",
		ExpectedHead: head,
		Files: map[string][]byte{
			"wiki/discovery/shop/idea.md":          []byte("# Idea\n\nA small shop.\n"),
			"wiki/discovery/shop/drafts/vision.md": []byte("# Vision\n"),
		},
		Subject: "discover shop round 1",
		Log:     "round 1 draft",
	}
	commit, imported, err := importMemoryBatch(r, b)
	if err != nil || !imported {
		t.Fatalf("import: imported=%v err=%v", imported, err)
	}
	if got := memoryHead(t, r); got != commit || commit == head {
		t.Fatalf("HEAD %s, commit %s, before %s", got, commit, head)
	}
	msg := mustGit(t, dir, "log", "-1", "--format=%B")
	if !strings.Contains(msg, importMarker("discover/shop", batchDigest(b.Files))) {
		t.Fatalf("commit lacks the marker:\n%s", msg)
	}
	changed := mustGit(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if changed != "wiki/discovery/shop/drafts/vision.md\nwiki/discovery/shop/idea.md\nwiki/log.md" {
		t.Fatalf("commit changed:\n%s", changed)
	}
	logText, _ := os.ReadFile(filepath.Join(dir, "wiki", "log.md"))
	if !strings.Contains(string(logText), "## [2026-11-13] import | discover/shop | round 1 draft") {
		t.Fatalf("log:\n%s", logText)
	}
	if err := memoryClean(dir); err != nil {
		t.Fatal(err)
	}

	// A retry of the same batch, even against a stale head, is a no-op that returns the commit.
	again, imported, err := importMemoryBatch(r, b)
	if err != nil || imported || again != commit {
		t.Fatalf("retry: commit=%s imported=%v err=%v", again, imported, err)
	}
	// Another batch prepared against the old head is a conflict.
	b2 := b
	b2.Files = map[string][]byte{"wiki/discovery/shop/idea.md": []byte("# Idea\n\nOther.\n")}
	if _, _, err := importMemoryBatch(r, b2); !errors.Is(err, ErrMemoryChanged) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	// Against the current head it imports.
	b2.ExpectedHead = commit
	if _, imported, err := importMemoryBatch(r, b2); err != nil || !imported {
		t.Fatalf("second batch: imported=%v err=%v", imported, err)
	}
}

func TestImportBatchRefusesDirtyMemory(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	head := memoryHead(t, r)
	write(t, dir, "wiki/notes.md", "owner draft")
	b := memoryBatch{Session: "discover/shop", ExpectedHead: head, Subject: "s", Log: "l",
		Files: map[string][]byte{"wiki/discovery/shop/idea.md": []byte("x\n")}}
	if _, _, err := importMemoryBatch(r, b); !errors.Is(err, ErrMemoryDirty) {
		t.Fatalf("expected dirty memory, got %v", err)
	}
}

func TestImportBatchRejectsInvalidBatches(t *testing.T) {
	r := newMemoryProject(t)
	head := memoryHead(t, r)
	ok := memoryBatch{Session: "discover/shop", ExpectedHead: head, Subject: "s", Log: "l",
		Files: map[string][]byte{"wiki/discovery/shop/idea.md": []byte("x\n")}}
	cases := map[string]func(b *memoryBatch){
		"generated log":     func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/log.md": []byte("x")} },
		"generated index":   func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/index.md": []byte("x")} },
		"approval":          func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/features/shop/approval.yaml": []byte("x")} },
		"implementation":    func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/features/shop/implemented.yaml": []byte("x")} },
		"feature extra":     func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/features/shop/notes.md": []byte("x")} },
		"outside wiki":      func(b *memoryBatch) { b.Files = map[string][]byte{"schema.md": []byte("x")} },
		"git dir":           func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/.git/config": []byte("x")} },
		"dot dot":           func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/../x.md": []byte("x")} },
		"case pair":         func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/a.md": []byte("x"), "wiki/A.md": []byte("y")} },
		"binary":            func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/a.md": []byte("a\x00")} },
		"empty":             func(b *memoryBatch) { b.Files = nil },
		"bad session":       func(b *memoryBatch) { b.Session = "Discover/../x" },
		"short head":        func(b *memoryBatch) { b.ExpectedHead = head[:7] },
		"marker in subject": func(b *memoryBatch) { b.Subject = "AEH-Record: OUT-1 " + fakeSHA('a') },
		"spaced marker":     func(b *memoryBatch) { b.Subject = " AEH-Import: discover/other " + strings.Repeat("a", 64) },
		"inner marker":      func(b *memoryBatch) { b.Subject = "x aeh-import: y" },
		"non-ascii path":    func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/café.md": []byte("x")} },
		"features case":     func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/FEATURES/shop/approval.yaml.md": []byte("x")} },
		"directory case":    func(b *memoryBatch) { b.Files = map[string][]byte{"wiki/a/x.md": []byte("x"), "wiki/A/y.md": []byte("y")} },
		"two-line log":      func(b *memoryBatch) { b.Log = "a\nAEH-Import: x y" },
	}
	for name, mutate := range cases {
		b := ok
		mutate(&b)
		if _, _, err := importMemoryBatch(r, b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := memoryHead(t, r); got != head {
		t.Fatalf("a rejected batch committed: %s", got)
	}
	if err := memoryClean(memDir(t, r)); err != nil {
		t.Fatal(err)
	}
}

func TestImportBatchRejectsTrackedCaseAlias(t *testing.T) {
	r := newMemoryProject(t)
	b := memoryBatch{Session: "discover/shop", ExpectedHead: memoryHead(t, r), Subject: "s", Log: "l",
		Files: map[string][]byte{"wiki/Log.md": []byte("x\n")}}
	if _, _, err := importMemoryBatch(r, b); err == nil || !strings.Contains(err.Error(), "only aeh writes") {
		t.Fatalf("expected a reserved-name error, got %v", err)
	}
	b.Files = map[string][]byte{"wiki/Index.MD": []byte("x\n")}
	if _, _, err := importMemoryBatch(r, b); err == nil {
		t.Fatal("index alias accepted")
	}
	// A tracked owner file in another case is one file on a case-insensitive disk.
	dir := memDir(t, r)
	write(t, dir, "wiki/Notes.md", "owner\n")
	mustGit(t, dir, "add", "wiki/Notes.md")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "notes")
	b.ExpectedHead = memoryHead(t, r)
	b.Files = map[string][]byte{"wiki/notes.md": []byte("x\n")}
	if _, _, err := importMemoryBatch(r, b); err == nil || !strings.Contains(err.Error(), "only in case") {
		t.Fatalf("expected a case error, got %v", err)
	}
	// A directory spelled in another case than a tracked one is refused too.
	b.Files = map[string][]byte{"wiki/discovery/shop/idea.md": []byte("x\n")}
	head, _, err := importMemoryBatch(r, b)
	if err != nil {
		t.Fatal(err)
	}
	b.ExpectedHead = head
	b.Files = map[string][]byte{"wiki/Discovery/shop/other.md": []byte("x\n")}
	if _, _, err := importMemoryBatch(r, b); err == nil || !strings.Contains(err.Error(), "only in case") {
		t.Fatalf("expected a directory case error, got %v", err)
	}
}

// A commit that carries the marker but not the batch's bytes never counts as the import.
func TestImportBatchMarkerNeedsContent(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	b := memoryBatch{Session: "discover/shop", ExpectedHead: memoryHead(t, r), Subject: "s", Log: "l",
		Files: map[string][]byte{"wiki/discovery/shop/idea.md": []byte("real\n")}}
	write(t, dir, "wiki/discovery/shop/idea.md", "forged\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "forged", "-m", importMarker(b.Session, batchDigest(b.Files)))
	if _, _, err := importMemoryBatch(r, b); err == nil || !strings.Contains(err.Error(), "carries this batch's marker") {
		t.Fatalf("expected a forged-marker error, got %v", err)
	}
}

// The right bytes on the wrong base are not the import either: files the batch did not name
// would come from a commit the batch was never checked against.
func TestImportBatchMarkerNeedsExpectedParent(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	b := memoryBatch{Session: "discover/shop", ExpectedHead: memoryHead(t, r), Subject: "s", Log: "l",
		Files: map[string][]byte{"wiki/discovery/shop/idea.md": []byte("real\n")}}
	write(t, dir, "wiki/notes.md", "someone else\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "between")
	write(t, dir, "wiki/discovery/shop/idea.md", "real\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "import", "-m", importMarker(b.Session, batchDigest(b.Files)))
	if _, _, err := importMemoryBatch(r, b); err == nil || !strings.Contains(err.Error(), "not a single commit on top of") {
		t.Fatalf("expected a wrong-parent error, got %v", err)
	}
}

const importedSpec = "---\nfeature: shop\ntitle: Shop\nversion: 0.1.0\npaths: [src/shop/**]\n---\n# Shop\n\nSell things.\n\n" +
	"## Changelog\n- 0.1.0 (2026-11-13) created; not implemented yet.\n"

func TestImportBatchWritesSpecAndIndex(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	b := memoryBatch{Session: "discover/shop", ExpectedHead: memoryHead(t, r), Subject: "promote shop", Log: "promote shop",
		Files: map[string][]byte{"wiki/features/shop/spec.md": []byte(importedSpec)}}
	if _, imported, err := importMemoryBatch(r, b); err != nil || !imported {
		t.Fatalf("import: %v %v", imported, err)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "wiki", "index.md"))
	if !strings.Contains(string(index), "[shop](features/shop/spec.md)") {
		t.Fatalf("index:\n%s", index)
	}
	snap, err := LoadMemory(r)
	if err != nil || len(snap.Specs) != 1 || snap.Specs[0].Approved() {
		t.Fatalf("snapshot %+v err %v", snap, err)
	}
}

func TestImportBatchRestoresOnInvalidSpec(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	head := memoryHead(t, r)
	b := memoryBatch{Session: "discover/shop", ExpectedHead: head, Subject: "s", Log: "l",
		Files: map[string][]byte{
			"wiki/discovery/shop/idea.md": []byte("x\n"),
			"wiki/features/shop/spec.md":  []byte("not a spec\n"),
		}}
	if _, _, err := importMemoryBatch(r, b); err == nil {
		t.Fatal("an invalid spec was imported")
	}
	if got := memoryHead(t, r); got != head {
		t.Fatalf("committed %s", got)
	}
	if err := memoryClean(dir); err != nil {
		t.Fatalf("restore left changes: %v", err)
	}
	for _, p := range []string{"wiki/discovery/shop/idea.md", "wiki/features/shop"} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
}

func TestImportBatchSkipsUnchangedFiles(t *testing.T) {
	r := newMemoryProject(t)
	dir := memDir(t, r)
	files := map[string][]byte{"wiki/discovery/shop/idea.md": []byte("x\n"), "wiki/discovery/shop/b.md": []byte("b\n")}
	c1, _, err := importMemoryBatch(r, memoryBatch{Session: "discover/shop", ExpectedHead: memoryHead(t, r), Subject: "s", Log: "one", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	files2 := map[string][]byte{"wiki/discovery/shop/idea.md": []byte("x\n"), "wiki/discovery/shop/b.md": []byte("b2\n")}
	if _, _, err := importMemoryBatch(r, memoryBatch{Session: "discover/shop", ExpectedHead: c1, Subject: "s", Log: "two", Files: files2}); err != nil {
		t.Fatal(err)
	}
	changed := mustGit(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if changed != "wiki/discovery/shop/b.md\nwiki/log.md" {
		t.Fatalf("commit changed:\n%s", changed)
	}
}
