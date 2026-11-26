package aeh

import (
	"context"
	"strings"
	"testing"
)

const quotaRule = "    quota_messages: [\"hit your 5-hour limit\"]\n"

// A sandboxed triage that hits its provider's limit leaves no route and stops the command:
// the next invocation triages again.
func TestDockerSandboxedTriageQuotaTriagesAgain(t *testing.T) {
	tri := "  tri:\n    provider: tri\n    command: [sh, -c, " + strconvQuote(agentQuota) + "]\n" + quotaRule
	f := agentsFixture(t, `true`, `echo "VERDICT: APPROVE"`, tri, "triage:\n  profile: tri\n")
	var log strings.Builder
	_, err := RunOutcome(context.Background(), f.repo, "OUT-001", &log)
	if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "tri"`) {
		t.Fatalf("got %v\n%s", err, log.String())
	}
	st, err := loadState(f.repo, "OUT-001")
	if err != nil {
		t.Fatal(err)
	}
	if st.InitialRoute != "" || st.TriageSource != "" {
		t.Fatalf("provisional route kept: %q %q", st.InitialRoute, st.TriageSource)
	}
}

// A sandboxed book reviewer that hits its limit leaves the review not run and the book
// pending, on every rerun.
func TestDockerSandboxedBookReviewQuotaKeepsTheBook(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, writeBook)
	cfg := "project: test\ncommands:\n  test: \"true\"\nladder: [agent]\nreviewers:\n  qa: [qa]\n" +
		"profiles:\n  agent:\n    provider: a\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(writeBook) + ", aeh, \"{{prompt}}\"]\n" +
		"  qa:\n    provider: b\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(agentQuota) + "]\n" + quotaRule +
		"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "sandbox")
	f.start()
	for i := 0; i < 2; i++ {
		_, err := DiscoverRun(context.Background(), f.repo, "shop", nil)
		if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "qa"`) || !strings.Contains(err.Error(), "aeh discover run shop") {
			t.Fatalf("run %d: %v", i, err)
		}
		st, _, err := LoadDiscovery(f.repo, "shop")
		if err != nil || st.Book == nil || st.Book.Review.Status != "not_run" || st.Book.Running {
			t.Fatalf("run %d: the book did not stay pending: %+v %v", i, st, err)
		}
	}
}

// A sandboxed implementer that hits its limit blocks without spending the ladder.
func TestDockerSandboxedImplementerQuotaBlocks(t *testing.T) {
	f := agentsFixture(t, agentQuota, `echo "VERDICT: APPROVE"`)
	addQuota(t, f.repo, "medio", "medio")
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "quota")
	st, log := f.run()
	if st.Status != StatusBlocked || len(st.Tasks[0].Attempts) != 1 || !strings.Contains(st.Question, `provider quota reached for profile "medio"`) {
		t.Fatalf("want a quota block: %+v\n%s", st, log)
	}
}

// A sandboxed inspector that hits its limit blocks after one attempt.
func TestDockerSandboxedInspectionQuotaBlocks(t *testing.T) {
	image := testImageID(t)
	f := newAdoptFixture(t, agentQuota)
	sandboxedDiscoveryConfig(t, f, image, agentQuota, "")
	addQuota(t, f.repo, "agent", "a")
	f.commitCode("sandbox")
	st, err := AdoptInventory(context.Background(), f.repo, "", nil)
	if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "agent"`) || st.Status != StatusBlocked || len(st.Attempts) != 1 {
		t.Fatalf("%v: %+v", err, st)
	}
}

// An explicit, sandboxed review of the book that hits its limit keeps the round and says
// to repeat aeh discover review.
func TestDockerSandboxedExplicitBookReviewQuota(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, writeBook)
	cfg := func(review string, quota bool) string {
		q := ""
		if quota {
			q = quotaRule
		}
		return "project: test\ncommands:\n  test: \"true\"\nladder: [agent]\nreviewers:\n  qa: [qa]\n" +
			"profiles:\n  agent:\n    provider: a\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(writeBook) + ", aeh, \"{{prompt}}\"]\n" +
			"  qa:\n    provider: b\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(review) + "]\n" + q +
			"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	}
	write(t, f.dir, "aeh/config.yaml", cfg("exit 3", false))
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "sandbox")
	f.start()
	if st := f.run(); len(st.Rounds) != 1 || st.Rounds[0].Review.Status != "invalid" {
		t.Fatalf("want an invalid review: %+v", st)
	}
	write(t, f.dir, "aeh/config.yaml", cfg(agentQuota, true))
	_, err := DiscoverReview(context.Background(), f.repo, "shop", nil)
	if err == nil || !strings.Contains(err.Error(), `provider quota reached for profile "qa"`) || !strings.Contains(err.Error(), "aeh discover review shop") {
		t.Fatalf("want a quota stop naming discover review: %v", err)
	}
	if st, _, err := LoadDiscovery(f.repo, "shop"); err != nil || st.Rounds[0].Review.Status != "invalid" {
		t.Fatalf("the round changed: %+v %v", st, err)
	}
}
