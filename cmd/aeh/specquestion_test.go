package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robbitoe/aeh/internal/aeh"
)

func TestStatusShowsSpecQuestionAndCursorResume(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, b)
	}
	r := aeh.Repo{Root: dir}
	if err := os.MkdirAll(r.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &aeh.SpecCursor{Task: "T1", Phase: "review", Role: "security", Profile: "inspect", Provider: "review-provider", Question: "Which behavior?"}
	st := aeh.State{Version: 2, ID: "OUT-1", Status: aeh.StatusBlocked, Question: c.Question, Cursor: c,
		Tasks: []aeh.TaskState{{ID: "T1", Attempts: []aeh.Attempt{{N: 1, SpecQuestion: c}}}}}
	for _, resumed := range []bool{false, true} {
		if resumed {
			st.Status, st.Question, st.Cursor = aeh.StatusAccepted, "", nil
			st.Tasks[0].Attempts = append(st.Tasks[0].Attempts, aeh.Attempt{N: 2, ResumedFromCursor: true})
		}
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.StatePath(st.ID), b, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.CreateTemp(t.TempDir(), "status-")
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = f
		err = printStatus(r)
		os.Stdout = stdout
		f.Close()
		b, readErr := os.ReadFile(filepath.Clean(f.Name()))
		if err != nil || readErr != nil {
			t.Fatalf("status: %v %v", err, readErr)
		}
		for _, text := range []string{"Which behavior?", "role security", "profile inspect", "provider review-provider"} {
			if !strings.Contains(string(b), text) {
				t.Errorf("status missing %q: %s", text, b)
			}
		}
		if resumed && !strings.Contains(string(b), "resumed from cursor") {
			t.Errorf("status missing resume: %s", b)
		}
	}
}
