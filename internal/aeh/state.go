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
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusBlocked   = "blocked"
	StatusAccepted  = "accepted"
	StatusDone      = "done"
	StatusAbandoned = "abandoned"
)

// Attempt is one try of a task with one profile.
type Attempt struct {
	SpecQuestion      *SpecCursor     `json:"spec_question,omitempty"`
	ResumedFromCursor bool            `json:"resumed_from_cursor,omitempty"`
	LadderIndex       int             `json:"ladder_index,omitempty"`
	Reviews           []ReviewVerdict `json:"reviews,omitempty"`
	RolesNotRun       []string        `json:"roles_not_run,omitempty"`
	N                 int             `json:"n"`
	Profile           string          `json:"profile"`
	Passed            bool            `json:"passed"`
	FailedGate        string          `json:"failed_gate,omitempty"`
	RunDir            string          `json:"run_dir"`
	Base              string          `json:"base,omitempty"`
	Commit            string          `json:"commit,omitempty"`
	Worktree          string          `json:"retained_worktree,omitempty"`
	StartedAt         time.Time       `json:"started_at"`
	Seconds           float64         `json:"seconds"`
}

// ReviewVerdict binds a role's decision to the exact candidate and selected reviewer.
type ReviewVerdict struct {
	Question     string `json:"question,omitempty"`
	Role         string `json:"role"`
	Profile      string `json:"profile"`
	Provider     string `json:"provider"`
	SHA          string `json:"sha"`
	Verdict      string `json:"verdict"`
	PromptSource string `json:"prompt_source"`
}

// TaskState tracks one task across runs.
type TaskState struct {
	ID       string    `json:"id"`
	Status   string    `json:"status"`
	Attempts []Attempt `json:"attempts,omitempty"`
}

// State is the local, ignored record of an outcome's execution.
type State struct {
	Cursor          *SpecCursor `json:"spec_cursor,omitempty"`
	ResumeOutcome   *Outcome    `json:"resume_outcome,omitempty"` // ephemeral aeh do contract
	Version         int         `json:"version"`
	InitialRoute    string      `json:"initial_route"`
	EffectiveRoute  string      `json:"effective_route"`
	Roles           []string    `json:"roles"`
	TriageSource    string      `json:"triage_source"`
	TriageOutput    string      `json:"triage_output"`
	TriageWorktree  string      `json:"triage_worktree,omitempty"`
	RulesHit        []string    `json:"rules_hit"`
	RulesDeferred   []string    `json:"rules_deferred,omitempty"` // may match below a broad task path; the candidate's files decide
	RestartRequired bool        `json:"restart_required,omitempty"`
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	Feature         string      `json:"feature,omitempty"`
	Paths           []string    `json:"paths,omitempty"`
	Change          string      `json:"change,omitempty"`
	Changelog       string      `json:"changelog,omitempty"`
	Status          string      `json:"status"`
	Base            string      `json:"base"`
	Head            string      `json:"verified_head"`
	Contract        string      `json:"contract_sha256"`
	FinalRun        string      `json:"final_run,omitempty"`
	MemoryVersion   string      `json:"memory_version,omitempty"`
	MemoryCommit    string      `json:"memory_commit,omitempty"`
	MemoryError     string      `json:"memory_error,omitempty"`
	Branch          string      `json:"branch"`
	Tasks           []TaskState `json:"tasks"`
	Question        string      `json:"question,omitempty"`
	Answers         []string    `json:"answers,omitempty"`
	UpdatedAt       time.Time   `json:"updated_at"`
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
		return State{Version: 2, ID: id, Status: StatusPending}, nil
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".plan.json") {
			continue // plan records are listed by ListPlanStates
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
	main, err := MainCheckout(r)
	if err != nil {
		return err
	}
	r = Repo{Root: main}
	unlockProject, err := lockLiveProject(r)
	if err != nil {
		return err
	}
	defer unlockProject()
	unlock, err := lockOutcome(r, id)
	if err != nil {
		return err
	}
	defer unlock()
	// A blocked plan has no run state yet; its answer goes to the plan state.
	if _, err := os.Stat(r.StatePath(id)); errors.Is(err, os.ErrNotExist) {
		if _, ok, err := LoadPlanState(r, id); err != nil {
			return err
		} else if ok {
			return answerPlan(r, id, text)
		}
	}
	s, err := loadState(r, id)
	if err != nil {
		return err
	}
	if s.Status == StatusAbandoned {
		return fmt.Errorf("outcome %s is abandoned and cannot receive answers", id)
	}
	if s.Version != 2 {
		return errors.New("pre-stage-2 state cannot resume; relaunch with another outcome ID")
	}
	if s.RestartRequired {
		return errors.New(s.Question)
	}
	if s.Status != StatusBlocked {
		return errors.New("outcome " + id + " is not blocked (status: " + s.Status + ")")
	}
	text = redactDiagnostic(strings.TrimSpace(text))
	if text == "" {
		return errors.New("answer must not be empty")
	}
	if s.Cursor != nil {
		s.Cursor.Answer = text
		// Keep the clarification with its question in every later prompt, even
		// after a rejection creates a different candidate and clears the cursor.
		text = fmt.Sprintf("Question: %s\nOwner answer: %s", s.Cursor.Question, text)
	}
	s.Answers = append(s.Answers, text)
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
