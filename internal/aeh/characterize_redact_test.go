package aeh

import (
	"context"
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

// An author that prints a credential before PLAN: TOO-LARGE blocks with a redacted question.
func TestAdoptSealRedactsAuthorOutput(t *testing.T) {
	f, _ := newCharFixture(t, `echo "token `+charSecret+`"; echo "PLAN: TOO-LARGE"`)
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked || len(st.Attempts) != 1 || !strings.Contains(st.Question, "[redacted") {
		t.Fatalf("too-large must block with a redacted question: %v %+v", err, st)
	}
	noSecretIn(t, f, st, log)
}

// A build that names an untracked file after a credential: the checkout-damage diagnostic,
// which comes from Git status, is redacted too.
func TestAdoptSealRedactsCheckoutDamage(t *testing.T) {
	f, _ := newCharFixture(t, charAuthorOK)
	write(t, f.dir, "tests/build.sh", "if [ -d tests/acceptance ]; then touch "+charSecret+".txt; fi\n")
	cfg := readFile(t, f.dir+"/aeh/config.yaml")
	write(t, f.dir, "aeh/config.yaml", strings.Replace(cfg, `build: "git --version"`, `build: "sh tests/build.sh"`, 1))
	f.commitAll("build script")
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked || len(st.Attempts) == 0 || st.Attempts[0].FailedStep != "build" {
		t.Fatalf("a build that damages the checkout must fail the attempt: %v %+v\n%s", err, st, log)
	}
	noSecretIn(t, f, st, log)
}

// A passing test whose file name is a credential: the check names in the state are redacted.
func TestAdoptSealRedactsTestFileNames(t *testing.T) {
	f, _ := newCharFixture(t, `d=`+planDir+`; mkdir -p $d
printf 'grep -q "paid" src/pay.txt\n' > $d/`+charSecret+`.sh
printf 'grep -q "EUR" src/pay.txt\n' > $d/currency.sh
printf 'kind: characterize\ntrace:\n  AC-1: [`+charSecret+`.sh]\n  AC-3: [currency.sh]\n' > $d/plan.yaml
echo "PLAN: READY"`)
	f.setScript("qa", `echo "VERDICT: REJECT"`)
	st, log, err := f.seal()
	if err != nil || st.Status != StatusBlocked {
		t.Fatalf("seal: %v %+v", err, st)
	}
	noSecretIn(t, f, st, log)
}

// Errors returned by aeh plan are redacted: a memory change named after a credential appears
// in Git status, which the plan reports when memory is not clean.
func TestPlanErrorsAreRedacted(t *testing.T) {
	f := newPlanFixture(t, authorOK, reviewOK)
	write(t, memDir(t, f.repo), charSecret+".md", "x\n")
	_, err := PlanFeature(context.Background(), f.repo, "pay", nil)
	if err == nil || strings.Contains(err.Error(), charSecret) || !strings.Contains(err.Error(), "[redacted") {
		t.Fatalf("plan error must be redacted: %v", err)
	}
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
