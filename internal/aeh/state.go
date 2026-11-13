package aeh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Outcome and task statuses. Kept deliberately few.
const (
	StatusPending  = "pending"
	StatusRunning  = "running"
	StatusBlocked  = "blocked"
	StatusAccepted = "accepted"
	StatusDone     = "done"
)

// Attempt is one try of a task with one profile.
type Attempt struct {
	N          int       `json:"n"`
	Profile    string    `json:"profile"`
	Passed     bool      `json:"passed"`
	FailedGate string    `json:"failed_gate,omitempty"`
	RunDir     string    `json:"run_dir"`
	Commit     string    `json:"commit,omitempty"`
	Worktree   string    `json:"retained_worktree,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	Seconds    float64   `json:"seconds"`
}

// TaskState tracks one task across runs.
type TaskState struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"`
	Attempts []Attempt `json:"attempts,omitempty"`
}

// State is the local, ignored record of an outcome's execution.
type State struct {
	ID            string      `json:"id"`
	Title         string      `json:"title"`
	Status        string      `json:"status"`
	Base          string      `json:"base"`
	Head          string      `json:"verified_head"`
	Contract      string      `json:"contract_sha256"`
	FinalRun      string      `json:"final_run,omitempty"`
	MemoryVersion string      `json:"memory_version,omitempty"`
	MemoryCommit  string      `json:"memory_commit,omitempty"`
	MemoryError   string      `json:"memory_error,omitempty"`
	Branch        string      `json:"branch"`
	Tasks         []TaskState `json:"tasks"`
	Question      string      `json:"question,omitempty"`
	Answers       []string    `json:"answers,omitempty"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

func (s *State) task(id string) *TaskState {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i]
		}
	}
	s.Tasks = append(s.Tasks, TaskState{ID: id, Status: StatusPending})
	return &s.Tasks[len(s.Tasks)-1]
}

func loadState(r Repo, id string) (State, error) {
	var s State
	if !validID(id) {
		return s, fmt.Errorf("invalid outcome ID %q", id)
	}
	b, err := os.ReadFile(r.StatePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return State{ID: id, Status: StatusPending}, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.ID != id {
		return s, fmt.Errorf("state for %q has ID %q", id, s.ID)
	}
	return s, nil
}

// lockOutcome serializes state and branch updates for one outcome in this checkout.
func lockOutcome(r Repo, id string) (func(), error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid outcome ID %q", id)
	}
	if err := os.MkdirAll(r.StateDir(), 0o700); err != nil {
		return nil, err
	}
	lock := filepath.Join(r.StateDir(), id+".lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("outcome %s is locked; if a run crashed, verify it stopped before removing %s", id, lock)
		}
		return nil, err
	}
	return func() { _ = os.Remove(lock) }, nil
}

func saveState(r Repo, s *State) error {
	s.UpdatedAt = time.Now().UTC()
	return writePrivateJSON(r.StatePath(s.ID), s)
}

// ListStates returns every recorded outcome, most recently updated first.
func ListStates(r Repo) ([]State, error) {
	entries, err := os.ReadDir(r.StateDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := loadState(r, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Answer records the owner's reply to a blocked outcome and makes it runnable again.
func Answer(r Repo, id, text string) error {
	unlock, err := lockOutcome(r, id)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadState(r, id)
	if err != nil {
		return err
	}
	if s.Status != StatusBlocked {
		return errors.New("outcome " + id + " is not blocked (status: " + s.Status + ")")
	}
	s.Answers = append(s.Answers, strings.TrimSpace(text))
	s.Question = ""
	s.Status = StatusPending
	for i := range s.Tasks {
		if s.Tasks[i].Status == StatusBlocked {
			s.Tasks[i].Status = StatusPending
		}
	}
	return saveState(r, &s)
}

// writeJSON writes v atomically: a crash never leaves half a file.
func writeJSON(path string, v any) error {
	return writeJSONMode(path, v, 0o755, 0o644)
}

func writePrivateJSON(path string, v any) error {
	return writeJSONMode(path, v, 0o700, 0o600)
}

func writeJSONMode(path string, v any, dirMode, fileMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aeh-json-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(append(b, '\n'))
	if err := errors.Join(writeErr, tmp.Chmod(fileMode), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
