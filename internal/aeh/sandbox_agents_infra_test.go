package aeh

import (
	"context"
	"strings"
	"testing"
)

// unforwarded makes a profile's launch fail before any container starts: its env refers
// to a variable the profile does not forward, which is an infrastructure failure.
const unforwarded = "    env:\n      X: \"$NOT_FORWARDED\"\n"

// An agent image missing from the engine stops discovery before an attempt is recorded.
func TestDockerSandboxedDiscoverySetupFailureSpendsNoAttempt(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, askTwo)
	sandboxedDiscoveryConfig(t, f, image, askTwo, "")
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "agent_image: \""+image+"\"", "agent_image: \""+testDigest+"\"", 1)
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "missing agent image")
	if _, err := DiscoverNew(f.repo, "shop", "A small online shop."); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		st, err := DiscoverRun(context.Background(), f.repo, "shop", nil)
		if err == nil || !strings.Contains(err.Error(), "not present locally") || len(st.Attempts) != 0 {
			t.Fatalf("run %d: %v, %d attempts", i, err, len(st.Attempts))
		}
	}
}

// A book reviewer that cannot run in the sandbox leaves the review not run and the book
// pending, on every rerun: the book is never imported unreviewed.
func TestDockerSandboxedBookReviewInfrastructureFailureKeepsTheBook(t *testing.T) {
	image := testImageID(t)
	f := newDiscoveryFixture(t, writeBook)
	cfg := "project: test\ncommands:\n  test: \"true\"\nladder: [agent]\nreviewers:\n  qa: [qa]\n" +
		"profiles:\n  agent:\n    provider: a\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote(writeBook) + ", aeh, \"{{prompt}}\"]\n" +
		"  qa:\n    provider: b\n    timeout_minutes: 2\n    command: [sh, -c, " + strconvQuote("cd /out; "+approveBook) + "]\n" + unforwarded +
		"sandbox:\n  image: \"" + image + "\"\n  agent_image: \"" + image + "\"\n  memory: 256m\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	mustGit(t, f.dir, "add", "-A")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "sandbox")
	f.start()
	for i := 0; i < 3; i++ {
		st, err := DiscoverRun(context.Background(), f.repo, "shop", nil)
		if err == nil || !strings.Contains(err.Error(), "book review could not run in the sandbox") {
			t.Fatalf("run %d: %v", i, err)
		}
		if st.Book == nil || st.Book.Review == nil || st.Book.Review.Status != "not_run" || st.Book.Running || len(st.Rounds) != 0 {
			t.Fatalf("run %d: book %+v, rounds %d", i, st.Book, len(st.Rounds))
		}
	}
}

// An adoption inspector that cannot run in the sandbox blocks after one attempt instead
// of trying every agent of the ladder.
func TestDockerSandboxedInspectionInfrastructureFailureBlocks(t *testing.T) {
	image := testImageID(t)
	f := newAdoptFixture(t, inventoryOK)
	sandboxedDiscoveryConfig(t, f, image, inventoryOK, "")
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "    command: [sh, -c,", unforwarded+"    command: [sh, -c,", 1)
	write(t, f.dir, "aeh/config.yaml", cfg)
	f.commitCode("sandbox")
	st, err := AdoptInventory(context.Background(), f.repo, "", nil)
	if err == nil || !strings.Contains(err.Error(), "sandbox infrastructure failure") || st.Status != StatusBlocked || len(st.Attempts) != 1 {
		t.Fatalf("%v: %+v", err, st)
	}
}
