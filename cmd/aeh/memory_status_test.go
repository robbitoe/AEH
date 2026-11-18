package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robbitoe/aeh/internal/aeh"
)

func TestMemoryStatusShowsPendingWorkAndQueues(t *testing.T) {
	work := []aeh.PendingWork{{ID: "OLD-1", Feature: "pay", Reason: "plan reserved"}, {ID: "OTHER-1", Feature: "other", Reason: "run blocked"}}
	queues := []aeh.QueuedApproval{{Feature: "pay", CurrentVersion: "0.1.0", Approval: aeh.SpecApproval{Version: "0.2.0"}, Blockers: work, ActivationWaiting: "memory has uncommitted changes"}}
	text := pendingMemoryText("pay", work, queues)
	for _, want := range []string{"pending work: OLD-1 (plan reserved)", "queued approval: 0.2.0 (in force: 0.1.0)", "blocker: OLD-1", "blocker: OTHER-1", "activation waiting: memory has uncommitted changes"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "pending work: OTHER-1") {
		t.Fatal("other feature's pending work printed under pay")
	}
}

func TestMemoryCommandsPrintStateWithInvalidQueueOrJournal(t *testing.T) {
	for _, kind := range []string{"queue", "journal"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if b, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, b)
				}
			}
			git("init", "-q")
			if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".aeh/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", ".gitignore")
			git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "test fixture")
			r := aeh.Repo{Root: dir}
			if _, err := aeh.InitMemory(r); err != nil {
				t.Fatal(err)
			}
			if _, err := aeh.CreateSpec(r, "checkout", "Checkout", []string{"src/checkout/**"}); err != nil {
				t.Fatal(err)
			}
			path, want := filepath.Join(dir, ".aeh", "live-write.json"), "invalid memory transaction"
			if kind == "queue" {
				path, want = filepath.Join(dir, ".aeh", "memory", "wiki", "features", "checkout", "queue.json"), "approval queue"
			}
			content := "{}\n"
			if kind == "queue" {
				content = "{broken\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			capture := func(run func() error) (string, error) {
				t.Helper()
				f, err := os.CreateTemp(t.TempDir(), "output-")
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				stdout, stderr := os.Stdout, os.Stderr
				os.Stdout, os.Stderr = f, f
				defer func() { os.Stdout, os.Stderr = stdout, stderr }()
				runErr := run()
				b, err := os.ReadFile(f.Name())
				if err != nil {
					t.Fatal(err)
				}
				return string(b), runErr
			}
			text, err := capture(func() error {
				_, err := memoryCommand(r, []string{"status"})
				return err
			})
			if err != nil || !strings.Contains(text, "checkout") || !strings.Contains(text, "memory:") || !strings.Contains(text, want) {
				t.Fatalf("memory status: %v\n%s", err, text)
			}
			text, err = capture(func() error { return printStatus(r) })
			if err != nil || !strings.Contains(text, "no outcomes have run yet") || !strings.Contains(text, want) {
				t.Fatalf("status: %v\n%s", err, text)
			}
			text, err = capture(func() error {
				_, err := memoryCommand(r, []string{"lint"})
				return err
			})
			if err == nil || !strings.Contains(text, "error:") || !strings.Contains(text, want) {
				t.Fatalf("memory lint: %v\n%s", err, text)
			}
		})
	}
}
