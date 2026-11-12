package aeh

import (
	"encoding/json"
	"errors"
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
	ID        string      `json:"id"`
	Title     string      `json:"title"`
	Status    string      `json:"status"`
	Base      string      `json:"base"`
	Branch    string      `json:"branch"`
	Tasks     []TaskState `json:"tasks"`
	Question  string      `json:"question,omitempty"`
	Answers   []string    `json:"answers,omitempty"`
	UpdatedAt time.Time   `json:"updated_at"`
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
	b, err := os.ReadFile(r.StatePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return State{ID: id, Status: StatusPending}, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func saveState(r Repo, s *State) error {
	s.UpdatedAt = time.Now().UTC()
	return writeJSON(r.StatePath(s.ID), s)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
