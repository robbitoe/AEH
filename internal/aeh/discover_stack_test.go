package aeh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent asks the stack question; with the owner's answer in its prompt it writes the book.
const askStack = `if printf '%s' "$1" | grep -q "Answer: Go and SQLite"; then
` + writeBook + `
else
cat > questions.yaml <<'EOF'
questions:
  - text: "Which technology stack should the shop use?"
    why: "The language and storage shape every feature."
    blocking: true
    options: ["Go and SQLite", "Node.js and PostgreSQL"]
    topic: stack
EOF
echo "DISCOVERY: QUESTIONS"
fi`

// newGreenfieldFixture is a project without code: only aeh's files and housekeeping.
func newGreenfieldFixture(t *testing.T, agent string) *discoveryFixture {
	t.Helper()
	f := newDiscoveryFixture(t, agent)
	write(t, f.dir, "README.md", "# Shop\n")
	mustGit(t, f.dir, "rm", "-q", "src/shop.js")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "start from scratch")
	return f
}

func (f *discoveryFixture) prompt(attempt int) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "runs", "discover-shop", "a"+string(rune('0'+attempt)), "discover.prompt.md"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestDiscoveryFromScratchRequiresTheStackQuestion(t *testing.T) {
	endToEnd(t)
	// A book straight away is refused, as is a questions round without the stack question.
	for name, agent := range map[string]string{"book": writeBook, "other questions": askTwo} {
		t.Run(name, func(t *testing.T) {
			f := newGreenfieldFixture(t, agent)
			f.start()
			st := f.run()
			if st.Status != StatusBlocked || len(st.Rounds) != 0 || !strings.Contains(st.Attempts[0].Feedback, "topic: stack") {
				t.Fatalf("a project from scratch must ask the stack first: %+v", st)
			}
			if p := f.prompt(1); !strings.Contains(p, "starts from scratch") || !strings.Contains(p, "topic: stack") {
				t.Fatalf("prompt:\n%s", p)
			}
		})
	}
	f := newGreenfieldFixture(t, askStack)
	f.start()
	st := f.run()
	if st.Phase != DiscoveryAwaiting || st.Rounds[0].Questions[0].Topic != topicStack || !st.Rounds[0].Questions[0].Blocking {
		t.Fatalf("the stack question waits for the owner: %+v", st)
	}
	if _, err := DiscoverSkip(f.repo, "shop", "R1-Q1"); err == nil {
		t.Fatal("the stack question cannot be skipped")
	}
	if _, err := DiscoverAnswer(f.repo, "shop", "R1-Q1", "Go and SQLite"); err != nil {
		t.Fatal(err)
	}
	st = f.run()
	if st.Phase != DiscoveryDraft {
		t.Fatalf("with the stack answered the book is accepted: %+v", st)
	}
	if p := f.prompt(2); !strings.Contains(p, "R1-Q1 (topic: stack)") || !strings.Contains(p, "decides it") {
		t.Fatalf("second prompt:\n%s", p)
	}
}

func TestDiscoveryKeepsTheExistingStack(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	write(t, f.dir, "package.json", "{}\n")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "node project")
	f.start()
	if st := f.run(); st.Phase != DiscoveryDraft {
		t.Fatalf("an existing project needs no stack question: %+v", st)
	}
	p := f.prompt(1)
	for _, want := range []string{"Keep this stack", "manifest: package.json", "files: 1 *.js", "test command: git --version"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestDiscoveryStackQuestionIsBlocking(t *testing.T) {
	_, err := parseDiscoveryQuestions([]byte("questions:\n  - {text: \"Stack?\", why: \"x\", blocking: false, topic: stack}\n"), 1, 7)
	if err == nil || !strings.Contains(err.Error(), "must be blocking") {
		t.Fatalf("a non-blocking stack question: %v", err)
	}
	if _, err := parseDiscoveryQuestions([]byte("questions:\n  - {text: \"Q?\", why: \"x\", blocking: true, topic: color}\n"), 1, 7); err == nil {
		t.Fatal("unknown topic accepted")
	}
}

func TestDetectStack(t *testing.T) {
	f := newDiscoveryFixture(t, writeBook)
	if st, err := detectStack(f.repo); err != nil || st.Greenfield {
		t.Fatalf("committed code is an existing project: %+v %v", st, err)
	}
	g := newGreenfieldFixture(t, writeBook)
	if st, err := detectStack(g.repo); err != nil || !st.Greenfield || st.Evidence != "" {
		t.Fatalf("aeh files and housekeeping only: %+v %v", st, err)
	}
	for p, want := range map[string]bool{"aeh/config.yaml": true, "README.md": true, "LICENSE": true, ".gitignore": true,
		"NOTES.md": true, "docs/guide.md": false, "src/a.go": false, "package.json": false} {
		if housekeeping(p) != want {
			t.Errorf("housekeeping(%q) = %v", p, !want)
		}
	}
}

func TestDiscoverReviewRerunsAReviewWithoutVerdict(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.setReviewer(`exit 3`, "b")
	f.start()
	st := f.run()
	if st.Rounds[0].Review.Status != "invalid" {
		t.Fatalf("a failing reviewer gives an invalid review: %+v", st.Rounds[0].Review)
	}
	// The owner fixes the reviewer and asks for the review again.
	write(t, f.tmp, "review.sh", "#!/bin/sh\n"+approveBook+"\n")
	var log strings.Builder
	st, err := DiscoverReview(context.Background(), f.repo, "shop", &log)
	if err != nil {
		t.Fatalf("review again: %v\n%s", err, log.String())
	}
	r := st.Rounds[0]
	if r.Review.Status != "approve" || len(r.Questions) != 2 || r.Questions[0].Blocking || st.Phase != DiscoveryDraft {
		t.Fatalf("the new review and its findings: %+v", r)
	}
	if round := f.memFile("wiki/discovery/shop/rounds/01.md"); !strings.Contains(round, "Can an empty cart be paid?") {
		t.Fatalf("memory round record:\n%s", round)
	}
	// A review with a verdict is never replaced, and a session without a book has nothing to review.
	if _, err := DiscoverReview(context.Background(), f.repo, "shop", nil); err == nil || !strings.Contains(err.Error(), "already has a verdict") {
		t.Fatalf("a verdict must not be replaced: %v", err)
	}
	g := newDiscoveryFixture(t, askTwo)
	g.start()
	g.run()
	if _, err := DiscoverReview(context.Background(), g.repo, "shop", nil); err == nil || !strings.Contains(err.Error(), "no draft book") {
		t.Fatalf("no book: %v", err)
	}
}
