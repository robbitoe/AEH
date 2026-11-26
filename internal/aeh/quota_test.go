package aeh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The line Claude Code printed at its usage limit in the stage 6 demo.
const quotaLine = "You've hit your 5-hour limit · resets 12:50pm (America/New_York)"

func writeLogs(t *testing.T, stdout, stderr string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "agent.out")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.err"), []byte(stderr), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuotaSignal(t *testing.T) {
	p := Profile{QuotaExitCodes: []int{75}, QuotaMessages: []string{"  Hit your 5-hour limit "}}
	long := strings.Repeat("x", quotaTail)
	for _, c := range []struct {
		name           string
		code           int
		stdout, stderr string
		want           string
	}{
		{"exit code", 75, "", "", "exit code 75"},
		{"message on stdout", 1, quotaLine + "\n", "", "message #1"},
		{"message on stderr", 1, "", "error: " + quotaLine, "message #1"},
		{"case and ANSI", 1, "\x1b[31mYOU'VE HIT YOUR \x1b[1m5-HOUR LIMIT\x1b[0m", "", "message #1"},
		{"exit 0 never", 0, quotaLine, "", ""},
		{"killed by a signal never", -1, quotaLine, "", ""},
		{"other failure", 1, "build failed", "", ""},
		{"beyond the tail", 1, quotaLine + long, "", ""},
		{"at the end of a long output", 1, long + quotaLine, "", "message #1"},
	} {
		err := quotaSignal(p, c.code, writeLogs(t, c.stdout, c.stderr))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var q *quotaError
		if !errors.As(err, &q) || q.Rule != c.want || !errors.Is(err, errProviderQuota) || !uncertainExit(err) {
			t.Errorf("%s: %v, want rule %q", c.name, err, c.want)
		}
		if strings.Contains(err.Error(), "resets") {
			t.Errorf("%s: the error quotes the agent's output: %v", c.name, err)
		}
	}
	if err := quotaSignal(Profile{}, 1, writeLogs(t, quotaLine, "")); err != nil {
		t.Errorf("a profile without rules: %v", err)
	}
	if err := quotaSignal(p, 1, filepath.Join(t.TempDir(), "missing.out")); err != nil {
		t.Errorf("missing logs: %v", err)
	}
}

func TestCheckQuotaMessages(t *testing.T) {
	if err := checkQuotaMessages([]string{"hit your 5-hour limit", "  usage limit reached  "}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"short"},
		{"        "},
		{"12345678 90"},
		{strings.Repeat("a", 121)},
		{"limit\x07reached"},
		{"a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8", "i9"},
	} {
		if err := checkQuotaMessages(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	_, err := writeConfig(t, "commands: {test: \"go test\"}\nladder: [p]\nprofiles:\n  p:\n    provider: x\n    command: [x]\n    quota_messages: [\"tiny\"]\n")
	if err == nil || !strings.Contains(err.Error(), `profile "p"`) {
		t.Fatalf("invalid quota_messages accepted: %v", err)
	}
}

func TestQuotaNote(t *testing.T) {
	cfg := Config{Profiles: map[string]Profile{"fuerte": {Provider: "anthropic"}}}
	note := quotaNote(cfg, "fuerte", errors.Join(errors.New("x"), &quotaError{Rule: "message #1"}))
	if !strings.Contains(note, `profile "fuerte"`) || !strings.Contains(note, "anthropic") || !strings.Contains(note, "message #1") {
		t.Fatalf("note %q", note)
	}
	if quotaNote(cfg, "fuerte", errors.New("agent exited with code 1")) != "" || quotaNote(cfg, "fuerte", nil) != "" {
		t.Fatal("a note for another error")
	}
}

// quotaProfile adds quota_messages to a fixture's profile.
func (f *fixture) quotaProfile(profile string) {
	f.t.Helper()
	cfg := readFile(f.t, f.repo.ConfigPath())
	head := "  " + profile + ":\n    provider: " + profile + "\n"
	if !strings.Contains(cfg, head) {
		f.t.Fatalf("no profile %s", profile)
	}
	write(f.t, f.dir, "aeh/config.yaml", strings.Replace(cfg, head, head+"    quota_messages: [\"hit your 5-hour limit\"]\n", 1))
}

const agentQuota = `echo "` + quotaLine + `"; exit 1`

func TestQuotaBlocksWithoutSpendingTheLadder(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentQuota, "fuerte": agentGood}, []string{"medio", "fuerte"}, "")
	f.quotaProfile("medio")
	st, log := f.run()
	if st.Status != StatusBlocked || len(st.Tasks[0].Attempts) != 1 {
		t.Fatalf("want one blocked attempt: %+v\n%s", st, log)
	}
	if !strings.Contains(st.Question, `provider quota reached for profile "medio"`) || !strings.Contains(st.Question, "answer to retry") ||
		strings.Contains(st.Question, "resets") || strings.Contains(st.Question, "Confirm all processes") {
		t.Fatalf("question %q", st.Question)
	}
	if st.Tasks[0].Attempts[0].Worktree != "" {
		t.Fatal("a quota attempt kept its worktree")
	}
	// After the limit resets, the answer restarts the task's ladder.
	f.setScript("medio", agentGood)
	if err := Answer(f.repo, "OUT-001", "the limit reset"); err != nil {
		t.Fatal(err)
	}
	st, log = f.run()
	if st.Status != StatusAccepted || st.Tasks[0].Attempts[1].Profile != "medio" {
		t.Fatalf("after the reset: %+v\n%s", st, log)
	}
}

func TestQuotaPhraseWithASuccessfulExitIsNoSignal(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood + `; echo "` + quotaLine + `"`}, []string{"medio"}, "")
	f.quotaProfile("medio")
	st, log := f.run()
	if st.Status != StatusAccepted {
		t.Fatalf("status %s\n%s", st.Status, log)
	}
}

func TestQuotaOfAReviewerBlocks(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood, "rev": agentQuota}, []string{"medio"}, "rev")
	f.quotaProfile("rev")
	st, log := f.run()
	if st.Status != StatusBlocked || len(st.Tasks[0].Attempts) != 1 || !strings.Contains(st.Question, `profile "rev"`) {
		t.Fatalf("want a quota block: %+v\n%s", st, log)
	}
	f.setScript("rev", reviewOK)
	if err := Answer(f.repo, "OUT-001", "the limit reset"); err != nil {
		t.Fatal(err)
	}
	if st, log = f.run(); st.Status != StatusAccepted {
		t.Fatalf("after the reset: %+v\n%s", st, log)
	}
}

func TestQuotaOfADamagingReviewerIsItsDamage(t *testing.T) {
	f := newFixture(t, map[string]string{"medio": agentGood, "rev": `echo tampered > src/fees.txt; ` + agentQuota}, []string{"medio"}, "rev")
	f.quotaProfile("rev")
	st, log := f.run()
	if strings.Contains(st.Question, "provider quota") {
		t.Fatalf("damage reported as quota: %q\n%s", st.Question, log)
	}
}

func TestQuotaOfTriageTriagesAgain(t *testing.T) {
	f := newFixture(t, map[string]string{"author": agentGood, "triage": agentQuota}, []string{"author"}, "")
	f.quotaProfile("triage")
	f.configText("triage:\n  profile: triage\n")
	st, err := RunOutcome(context.Background(), f.repo, "OUT-001", nil)
	if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "triage"`) || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("want a quota stop: %v", err)
	}
	if st.InitialRoute != "" || st.TriageSource != "" || len(st.Tasks) > 0 && len(st.Tasks[0].Attempts) > 0 {
		t.Fatalf("triage left a result or an attempt ran: %+v", st)
	}
	if _, err := os.Stat(f.repo.WorktreePath("OUT-001-triage")); err == nil {
		t.Fatal("triage kept its worktree")
	}
	f.setScript("triage", "echo 'ROUTE: trivial'")
	st, log := f.run()
	if st.TriageSource != "model" || st.InitialRoute != RouteTrivial || st.Status != StatusAccepted {
		t.Fatalf("triage did not run again: %+v\n%s", st, log)
	}
}

// addQuota adds quota_messages to a profile of a test configuration.
func addQuota(t *testing.T, r Repo, profile, provider string) {
	t.Helper()
	cfg := readFile(t, r.ConfigPath())
	head := "  " + profile + ":\n    provider: " + provider + "\n"
	if !strings.Contains(cfg, head) {
		t.Fatalf("no profile %s:\n%s", profile, cfg)
	}
	write(t, r.Root, "aeh/config.yaml", strings.Replace(cfg, head, head+"    quota_messages: [\"hit your 5-hour limit\"]\n", 1))
}

func TestQuotaOfThePlanAuthorBlocks(t *testing.T) {
	f := newPlanFixture(t, agentQuota, reviewOK)
	addQuota(t, f.repo, "author", "author")
	f.commitAll("quota")
	st := f.plan()
	if st.Status != StatusBlocked || len(st.Attempts) != 1 || !strings.Contains(st.Question, `provider quota reached for profile "author"`) ||
		strings.Contains(st.Question, "resets") {
		t.Fatalf("want a quota block: %+v", st)
	}
	f.setScript("author", authorOK)
	if err := Answer(f.repo, planID, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	if st = f.plan(); st.Status != PlanLogged {
		t.Fatalf("after the reset: %+v", st)
	}
}

func TestQuotaOfAPlanReviewerBlocks(t *testing.T) {
	f := newPlanFixture(t, authorOK, agentQuota)
	addQuota(t, f.repo, "qa", "qa")
	f.commitAll("quota")
	st := f.plan()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "review of the plan stopped") || !strings.Contains(st.Question, `profile "qa"`) {
		t.Fatalf("want a quota block: %+v", st)
	}
	f.setScript("qa", reviewOK)
	if err := Answer(f.repo, planID, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	if st = f.plan(); st.Status != PlanLogged {
		t.Fatalf("after the reset: %+v", st)
	}
}

func TestQuotaOfTheDiscoveryAuthorSpendsNoAttempt(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, agentQuota)
	addQuota(t, f.repo, "agent", "a")
	f.commitCode("quota")
	f.start()
	st := f.run()
	if !strings.Contains(st.Question, `provider quota reached for profile "agent"`) || !strings.Contains(st.Question, "aeh discover run shop") {
		t.Fatalf("want a quota stop: %+v", st)
	}
	f.setAgent(writeBook)
	if st = f.run(); strings.Contains(st.Question, "quota") || st.Phase != DiscoveryReview && st.Book == nil && len(st.Rounds) == 0 {
		t.Fatalf("after the reset: %+v", st)
	}
}

func TestQuotaOfTheBookReviewerKeepsTheBook(t *testing.T) {
	endToEnd(t)
	f := newDiscoveryFixture(t, writeBook)
	f.setReviewer(agentQuota, "b")
	addQuota(t, f.repo, "qa", "b")
	f.start()
	for i := 0; i < 3; i++ {
		_, err := DiscoverRun(context.Background(), f.repo, "shop", nil)
		if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "qa"`) || !strings.Contains(err.Error(), "aeh discover run shop") {
			t.Fatalf("run %d: %v", i, err)
		}
		st, _, err := LoadDiscovery(f.repo, "shop")
		if err != nil || st.Book == nil || st.Book.Review.Status != "not_run" || st.Book.Running || len(st.Attempts) != 1 {
			t.Fatalf("run %d: the book did not stay pending: %+v %v", i, st, err)
		}
	}
	write(t, f.tmp, "review.sh", "#!/bin/sh\n"+approveBook+"\n")
	st := f.run()
	if st.Book != nil || len(st.Attempts) != 1 {
		t.Fatalf("after the reset the book was not reviewed and imported: %+v", st)
	}
}

func TestQuotaOfAnInspectorBlocks(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, agentQuota)
	addQuota(t, f.repo, "agent", "a")
	base := f.commitCode("quota")
	st, err := AdoptInventory(context.Background(), f.repo, base, nil)
	if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "agent"`) || st.Status != StatusBlocked {
		t.Fatalf("want a quota block: %+v %v", st, err)
	}
}

func TestQuotaOfADamagingInspectorIsItsDamage(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, "echo changed > src/users/user.txt; "+agentQuota)
	addQuota(t, f.repo, "agent", "a")
	base := f.commitCode("quota")
	st, err := AdoptInventory(context.Background(), f.repo, base, nil)
	if err != nil && strings.Contains(err.Error(), "provider quota") || len(st.Attempts) == 0 ||
		!strings.Contains(st.Attempts[0].Feedback, "agent wrote into the code") || strings.Contains(st.Attempts[0].Feedback, "provider quota") {
		t.Fatalf("damage reported as quota: %+v %v", st, err)
	}
}

// A quota on the launch that resumes a SPEC-QUESTION blocks like any quota; the answer is
// kept in the history, and the next answer restarts the task.
func TestQuotaAfterASpecQuestion(t *testing.T) {
	impl := `n=$(cat "$AEH_COUNTER" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$AEH_COUNTER"; ` +
		`case $n in 1) ` + askSpec + `;; 2) ` + agentQuota + `;; *) ` + agentGood + `;; esac`
	f := newFixture(t, map[string]string{"impl": impl}, []string{"impl"}, "")
	f.quotaProfile("impl")
	st, _ := f.run()
	if st.Cursor == nil {
		t.Fatal("did not ask")
	}
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st, log := f.run()
	if st.Status != StatusBlocked || !strings.Contains(st.Question, "provider quota reached") || st.Cursor != nil {
		t.Fatalf("want a quota block without a cursor: %+v\n%s", st, log)
	}
	if err := Answer(f.repo, st.ID, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	if st, log = f.run(); st.Status != StatusAccepted {
		t.Fatalf("after the reset: %+v\n%s", st, log)
	}
	if !strings.Contains(fmt.Sprint(st.Answers), "owner-choice") {
		t.Fatalf("the earlier answer was lost: %+v", st.Answers)
	}
}

// Profiles without quota_messages serialize as before, so the digests of plans reserved
// before this field existed do not change.
func TestQuotaMessagesKeepOldDigests(t *testing.T) {
	b, err := json.Marshal(Profile{Provider: "a", Command: []string{"x"}})
	if err != nil || strings.Contains(string(b), "QuotaMessages") {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = json.Marshal(Profile{QuotaMessages: []string{"hit your 5-hour limit"}})
	if !strings.Contains(string(b), "QuotaMessages") {
		t.Fatalf("set rules must reach the digest: %s", b)
	}
}
