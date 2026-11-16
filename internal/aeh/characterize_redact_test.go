package aeh

import (
	"strings"
	"testing"
)

// noSecretIn fails when the plan's state file, the command log or the question carries the token.
func noSecretIn(t *testing.T, f *planFixture, st PlanState, log string) {
	t.Helper()
	statePath, err := planStatePath(f.repo, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"state": readFile(t, statePath), "log": log, "question": st.Question} {
		if strings.Contains(text, charSecret) {
			t.Fatalf("the secret reached the %s", where)
		}
	}
}

// A red base whose suite prints a credential blocks with a redacted question.
func TestAdoptSealRedactsBaselineOutput(t *testing.T) {
	f, _ := newCharFixture(t, charAuthorOK)
	write(t, f.dir, "tests/suite.sh", "echo token "+charSecret+"\nexit 1\n")
	f.commitAll("red suite")
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked || !strings.Contains(st.Question, "not green") || len(st.Attempts) != 0 {
		t.Fatalf("a red base must block before any author runs: %v %+v", err, st)
	}
	if !strings.Contains(st.Question, "[redacted") {
		t.Fatalf("the question should show the redaction: %s", st.Question)
	}
	noSecretIn(t, f, st, log)
}

// A build that fails once the tests exist, printing a credential, gives redacted feedback.
func TestAdoptSealRedactsBuildOutput(t *testing.T) {
	f, _ := newCharFixture(t, charAuthorOK)
	write(t, f.dir, "tests/build.sh", "if [ -d tests/acceptance ]; then echo token "+charSecret+"; exit 1; fi\n")
	f.writeConfig("")
	cfg := readFile(t, f.dir+"/aeh/config.yaml")
	write(t, f.dir, "aeh/config.yaml", strings.Replace(cfg, `build: "git --version"`, `build: "sh tests/build.sh"`, 1))
	f.commitAll("build script")
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked || len(st.Attempts) == 0 {
		t.Fatalf("a failing build must block after the ladder: %v %+v\n%s", err, st, log)
	}
	a := st.Attempts[0]
	if a.FailedStep != "build" || !strings.Contains(a.Feedback, "[redacted") {
		t.Fatalf("attempt: %+v", a)
	}
	noSecretIn(t, f, st, log)
}
