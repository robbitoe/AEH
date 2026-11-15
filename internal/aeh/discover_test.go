package aeh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	askTwo = `cat > questions.yaml <<'EOF'
questions:
  - text: "Who buys in the shop?"
    why: "Users decide the features."
    blocking: true
    options: ["Consumers", "Companies"]
  - text: "Do you need coupons?"
    why: "Coupons add a feature."
    blocking: false
EOF
echo "DISCOVERY: QUESTIONS"`

	writeBook = `mkdir -p features mockups/cart
printf '# Vision\n\nA shop for consumers.\n' > vision.md
printf '# Data\n\n- Cart: items, total.\n' > data.md
printf '# Cart\n\nThe cart holds items.\n\n## Acceptance criteria\n- AC-1: Given an empty cart, when the user adds an item, then the cart has one item.\n' > features/cart.md
printf '<style>.x{color:red}</style><main class="x"><h1>Cart</h1><button>Pay</button></main>\n' > mockups/cart/list.html
echo "DISCOVERY: READY"`

	// bookAfterAnswer writes the book only when the prompt carries the owner's answer.
	bookAfterAnswer = `if printf '%s' "$1" | grep -q "Answer: Consumers only"; then
` + writeBook + `
else
` + askTwo + `
fi`
)

type discoveryFixture struct {
	t    *testing.T
	repo Repo
	dir  string
	tmp  string
}

func newDiscoveryFixture(t *testing.T, agent string) *discoveryFixture {
	t.Helper()
	for _, tool := range []string{"sh", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("discovery tests require " + tool)
		}
	}
	f := &discoveryFixture{t: t, dir: filepath.Join(t.TempDir(), "shop & co"), tmp: t.TempDir()}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.repo = Repo{Root: f.dir}
	mustGit(t, f.dir, "init", "-q", "-b", "main")
	mustGit(t, f.dir, "config", "core.autocrlf", "false")
	write(t, f.dir, ".gitignore", ".aeh/\n")
	f.setAgent(agent)
	f.writeConfig("")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	if _, err := InitMemory(f.repo); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *discoveryFixture) writeConfig(discovery string) {
	f.t.Helper()
	var cfg strings.Builder
	cfg.WriteString("project: test\ncommands:\n  test: \"git --version\"\nladder: [agent]\n")
	cfg.WriteString(discovery)
	fmt.Fprintf(&cfg, "profiles:\n  agent:\n    provider: a\n    timeout_minutes: 1\n    command: [\"sh\", %q, \"{{prompt}}\"]\n",
		filepath.ToSlash(filepath.Join(f.tmp, "agent.sh")))
	write(f.t, f.dir, "aeh/config.yaml", cfg.String())
}

func (f *discoveryFixture) setAgent(script string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.tmp, "agent.sh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *discoveryFixture) run() DiscoveryState {
	f.t.Helper()
	var log strings.Builder
	st, err := DiscoverRun(context.Background(), f.repo, "shop", &log)
	if err != nil {
		f.t.Fatalf("discover run: %v\n%s", err, log.String())
	}
	return st
}

func (f *discoveryFixture) memFile(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "memory", filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestDiscoveryRoundsToDraftBook(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, bookAfterAnswer)
	if _, err := DiscoverNew(f.repo, "shop", "A small online shop."); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverNew(f.repo, "shop", "again"); err == nil {
		t.Fatal("a second session with the same slug was created")
	}
	if got := f.memFile("wiki/discovery/shop/idea.md"); got != "# Idea\n\nA small online shop.\n" {
		t.Fatalf("idea.md:\n%s", got)
	}

	st := f.run()
	if st.Phase != DiscoveryAwaiting || len(st.Rounds) != 1 || len(st.Rounds[0].Questions) != 2 {
		t.Fatalf("after round 1: %+v", st)
	}
	q := st.Rounds[0].Questions
	if q[0].ID != "R1-Q1" || !q[0].Blocking || q[1].ID != "R1-Q2" || q[1].Blocking || len(q[0].Options) != 2 {
		t.Fatalf("questions: %+v", q)
	}
	round := f.memFile("wiki/discovery/shop/rounds/01.md")
	if !strings.Contains(round, "## R1-Q1 (blocking)") || !strings.Contains(round, "Who buys in the shop?") {
		t.Fatalf("rounds/01.md:\n%s", round)
	}
	if _, err := DiscoverRun(context.Background(), f.repo, "shop", nil); err == nil || !strings.Contains(err.Error(), "waits for answers") {
		t.Fatalf("a round ran with blocking questions open: %v", err)
	}

	// Blocking questions cannot be skipped; optional ones can.
	if _, err := DiscoverSkip(f.repo, "shop", "R1-Q1"); err == nil {
		t.Fatal("a blocking question was skipped")
	}
	if st, err := DiscoverSkip(f.repo, "shop", "R1-Q2"); err != nil || st.Phase != DiscoveryAwaiting {
		t.Fatalf("skip: %v %s", err, st.Phase)
	}
	if _, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", "Companies"); err != nil {
		t.Fatal(err)
	}
	// A revision replaces the answer the next round receives.
	st, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", "Consumers only")
	if err != nil || st.Phase != DiscoveryReady {
		t.Fatalf("answer: %v %s", err, st.Phase)
	}
	round = f.memFile("wiki/discovery/shop/rounds/01.md")
	for _, want := range []string{"Answer (2026-", "Companies", "Revised answer (2026-", "Consumers only", "Skipped by the owner."} {
		if !strings.Contains(round, want) {
			t.Fatalf("rounds/01.md lacks %q:\n%s", want, round)
		}
	}

	st = f.run()
	if st.Phase != DiscoveryDraft || len(st.Rounds) != 2 || st.Rounds[1].Kind != roundBook {
		t.Fatalf("after round 2: %+v", st)
	}
	prompt, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "runs", "discover-shop", "a2", "discover.prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "Answer: Companies") || !strings.Contains(string(prompt), "R1-Q2: Do you need coupons?\nSkipped by the owner.") {
		t.Fatalf("round 2 prompt history:\n%s", prompt)
	}
	for _, rel := range []string{"vision.md", "data.md", "features/cart.md"} {
		f.memFile("wiki/discovery/shop/drafts/" + rel)
	}
	page := f.memFile("wiki/discovery/shop/drafts/mockups/cart/list.html")
	if !strings.Contains(page, "Content-Security-Policy") || !strings.Contains(page, `<button type="button">Pay</button>`) {
		t.Fatalf("mockup was not rebuilt:\n%s", page)
	}
	// Drafts are not features: no spec exists, and memory is clean.
	snap, err := LoadMemory(f.repo)
	if err != nil || len(snap.Specs) != 0 {
		t.Fatalf("drafts became features: %+v %v", snap.Specs, err)
	}
	if err := memoryClean(snap.Dir); err != nil {
		t.Fatal(err)
	}
	log := mustGit(t, snap.Dir, "log", "--format=%s")
	for _, want := range []string{"discover shop: idea", "discover shop: round 1 questions", "discover shop: skip R1-Q2",
		"discover shop: answer R1-Q1", "discover shop: round 2 draft book"} {
		if !strings.Contains(log, want) {
			t.Fatalf("memory log lacks %q:\n%s", want, log)
		}
	}
	if _, err := DiscoverRun(context.Background(), f.repo, "shop", nil); err == nil || !strings.Contains(err.Error(), "draft book") {
		t.Fatalf("a round ran after the draft book: %v", err)
	}
	// The agent's output directory is gone.
	if _, err := os.Stat(f.repo.WorktreePath("discover-shop-a2")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output directory left behind: %v", err)
	}
}

func TestDiscoveryLadderBlocksAndRetries(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, "")
	// The first try has no verdict; the second writes a file the manifest does not admit.
	try := filepath.ToSlash(filepath.Join(f.tmp, "try"))
	f.setAgent(`if [ ! -f "` + try + `" ]; then echo 1 > "` + try + `"; echo "no verdict"; else echo extra > notes.txt; echo "DISCOVERY: QUESTIONS"; fi`)
	if _, err := DiscoverNew(f.repo, "shop", "A shop."); err != nil {
		t.Fatal(err)
	}
	st := f.run()
	if st.Status != StatusBlocked || len(st.Attempts) != 2 || !strings.Contains(st.Question, "notes.txt is not an expected file") {
		t.Fatalf("expected a blocked session after two failures: %+v", st)
	}
	if !strings.Contains(st.Attempts[0].Feedback, "DISCOVERY: QUESTIONS") {
		t.Fatalf("first feedback: %q", st.Attempts[0].Feedback)
	}
	// Running again is the owner's retry: a new ladder round with the fixed agent.
	f.setAgent(askTwo)
	st = f.run()
	if st.Status != "" || st.Phase != DiscoveryAwaiting || len(st.Attempts) != 3 || st.LadderFrom != 2 {
		t.Fatalf("retry: %+v", st)
	}
}

func TestDiscoveryRejectsSecretsAndUnsafeText(t *testing.T) {
	endToEnd(t)
	secret := "ghp_" + strings.Repeat("A", 36)
	f := newDiscoveryFixture(t, `printf 'questions:\n  - text: "Use token `+secret+`?"\n    why: "x"\n    blocking: true\n' > questions.yaml; echo "DISCOVERY: QUESTIONS"`)
	if _, err := DiscoverNew(f.repo, "shop", "Use <script>alert(1)</script>"); err == nil {
		t.Fatal("an idea with raw HTML was accepted")
	}
	if _, err := DiscoverNew(f.repo, "shop", "Key "+secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("an idea with a secret: %v", err)
	}
	if _, err := DiscoverNew(f.repo, "shop", "A shop."); err != nil {
		t.Fatal(err)
	}
	st := f.run()
	if st.Status != StatusBlocked {
		t.Fatalf("a round with a secret was accepted: %+v", st)
	}
	for _, a := range st.Attempts {
		if strings.Contains(a.Feedback, secret) || !strings.Contains(a.Feedback, "possible GitHub token") {
			t.Fatalf("feedback: %q", a.Feedback)
		}
	}
	if strings.Contains(st.Question, secret) {
		t.Fatal("the question shows the secret")
	}
	f.setAgent(askTwo)
	f.run()
	for _, bad := range []string{"<b>x</b>", "token " + secret, "", "&lt;"} {
		if _, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", bad); err == nil {
			t.Errorf("answer %q was accepted", bad)
		}
	}
	for _, id := range []string{"R9-Q1", "R1-Q3", "Q1", "R01-Q1"} {
		if _, err := DiscoverAnswer(f.repo, "shop", id, "x"); err == nil {
			t.Errorf("question %s was answered", id)
		}
	}
}

func TestParseDiscoveryQuestions(t *testing.T) {
	ok := "questions:\n  - text: \"A?\"\n    why: \"B\"\n    blocking: false\n"
	if q, err := parseDiscoveryQuestions([]byte(ok), 3, 2); err != nil || q[0].ID != "R3-Q1" || q[0].Blocking {
		t.Fatalf("%+v %v", q, err)
	}
	for name, src := range map[string]string{
		"too many":      "questions:\n  - {text: a, why: b, blocking: true}\n  - {text: a, why: b, blocking: true}\n  - {text: a, why: b, blocking: true}\n",
		"none":          "questions: []\n",
		"no blocking":   "questions:\n  - {text: a, why: b}\n",
		"unknown field": "questions:\n  - {text: a, why: b, blocking: true, id: X}\n",
		"html":          "questions:\n  - {text: \"<b>a</b>\", why: b, blocking: true}\n",
		"empty why":     "questions:\n  - {text: a, why: \" \", blocking: true}\n",
		"many options":  "questions:\n  - {text: a, why: b, blocking: true, options: [1,2,3,4,5,6,7]}\n",
	} {
		if _, err := parseDiscoveryQuestions([]byte(src), 1, 2); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBookRejectsOrphanMockupsAndUnsafeDocs(t *testing.T) {
	d := &discoverySession{dcfg: DiscoveryConfig{MaxFeatures: 2, MaxMockupBytes: 4096}, state: &DiscoveryState{Slug: "shop"}}
	base := map[string][]byte{"vision.md": []byte("# V\n"), "data.md": []byte("# D\n"), "features/cart.md": []byte("# C\n")}
	with := func(rel, body string) frozenOutput {
		files := map[string][]byte{}
		for k, v := range base {
			files[k] = v
		}
		files[rel] = []byte(body)
		return frozenOutput{Files: files}
	}
	for name, f := range map[string]frozenOutput{
		"orphan mockup": with("mockups/pay/a.html", "<p>x</p>"),
		"script":        with("mockups/cart/a.html", "<script>x</script>"),
		"bad screen":    with("mockups/cart/A_b.html", "<p>x</p>"),
		"bad feature":   with("features/Cart_x.md", "# x\n"),
		"html in doc":   with("data.md", "<b>x</b>\n"),
		"external img":  with("vision.md", "![x](https://evil.example/x.png)\n"),
	} {
		if _, err := d.bookFiles(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	files, err := d.bookFiles(with("mockups/cart/list.html", "<p>x</p>"))
	if err != nil || len(files) != 4 || !strings.HasPrefix(string(files["wiki/discovery/shop/drafts/mockups/cart/list.html"]), "<!doctype html>") {
		t.Fatalf("%v %v", files, err)
	}
}

func TestDiscoveryConfig(t *testing.T) {
	base := "project: t\ncommands:\n  test: x\nladder: [a]\nprofiles:\n  a:\n    command: [x]\n"
	for name, extra := range map[string]string{
		"unknown key":   "discovery:\n  ladder: [a]\n  max_question: 3\n",
		"negative":      "discovery:\n  max_questions: -1\n",
		"too many":      "discovery:\n  max_features: 51\n",
		"small mockups": "discovery:\n  max_mockup_bytes: 10\n",
		"no profile":    "discovery:\n  ladder: [b]\n",
	} {
		dir := t.TempDir()
		write(t, dir, "aeh/config.yaml", base+extra)
		if _, err := (Repo{Root: dir}).LoadConfig(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	write(t, dir, "aeh/config.yaml", base)
	cfg, err := Repo{Root: dir}.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.discovery()
	if d.MaxQuestions != 7 || d.MaxFeatures != 12 || d.MaxMockupBytes != 204800 || d.Research || len(d.Ladder) != 1 {
		t.Fatalf("defaults: %+v", d)
	}
}

// A memory write that fails after the state was saved is finished by the next command, also
// when the owner committed other memory files in between.
func TestDiscoveryFinishesPendingImport(t *testing.T) {
	if testing.Short() {
		t.Skip("runs an agent")
	}
	f := newDiscoveryFixture(t, askTwo)
	if _, err := DiscoverNew(f.repo, "shop", "A shop."); err != nil {
		t.Fatal(err)
	}
	f.run()
	memoryWriteFile = func(p string, b []byte) error { return errors.New("disk full") }
	_, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", "Consumers")
	memoryWriteFile = writeFileAtomic
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected the write to fail: %v", err)
	}
	st, _, err := LoadDiscovery(f.repo, "shop")
	if err != nil || st.Pending == nil || len(st.Rounds[0].Questions[0].Answers) != 1 {
		t.Fatalf("the answer and its pending import were not saved: %+v %v", st, err)
	}
	mem := filepath.Join(f.dir, ".aeh", "memory")
	write(t, mem, "wiki/notes.md", "owner notes\n")
	mustGit(t, mem, "add", "wiki/notes.md")
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "owner notes")
	if _, err := DiscoverSkip(f.repo, "shop", "R1-Q2"); err != nil {
		t.Fatal(err)
	}
	round := f.memFile("wiki/discovery/shop/rounds/01.md")
	if !strings.Contains(round, "Consumers") || !strings.Contains(round, "Skipped by the owner.") {
		t.Fatalf("rounds/01.md:\n%s", round)
	}
	if st, _, _ := LoadDiscovery(f.repo, "shop"); st.Pending != nil || st.Phase != DiscoveryReady {
		t.Fatalf("state after recovery: %+v", st)
	}
	// The owner's commit touched a batch file: aeh does not move the batch over it.
	memoryWriteFile = func(p string, b []byte) error { return errors.New("disk full") }
	_, err = DiscoverAnswer(f.repo, "shop", "R1-Q1", "Consumers, revised")
	memoryWriteFile = writeFileAtomic
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	write(t, mem, "wiki/discovery/shop/rounds/01.md", "owner edit\n")
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qam", "owner edit")
	if _, err := DiscoverAnswer(f.repo, "shop", "R1-Q2", "No"); err == nil || !strings.Contains(err.Error(), "rounds/01.md") {
		t.Fatalf("expected a conflict on rounds/01.md: %v", err)
	}
}
