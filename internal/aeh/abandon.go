package aeh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type abandonIntent struct {
	ID        string `json:"id"`
	Feature   string `json:"feature,omitempty"`
	Date      string `json:"date"`
	Done      bool   `json:"done,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func abandonPath(r Repo, id string) string {
	return filepath.Join(r.Root, ".aeh", "abandon", id+".json")
}

// Abandon keeps accepted work, and otherwise persists intent before changing either state.
// No branch, attempt, report or other evidence is removed. The project lock is always first.
func Abandon(r Repo, id string) error {
	if !validID(id) {
		return fmt.Errorf("abandon: invalid outcome ID %q", id)
	}
	main, err := MainCheckout(r)
	if err != nil {
		return err
	}
	r = Repo{Root: main}
	unlock, err := lockLiveProject(r)
	if err != nil {
		return err
	}
	defer unlock()
	unlockOutcome, err := lockOutcome(r, id)
	if err != nil {
		return err
	}
	defer unlockOutcome()
	p, planned, err := LoadPlanState(r, id)
	if err != nil {
		return err
	}
	s, err := loadState(r, id)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(r.StatePath(id))
	if !planned && errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("abandon %s: no registered plan or run", id)
	}
	if accepted, err := keepAccepted(r, id, p, planned, &s); err != nil {
		return err
	} else if accepted {
		if err := reconcileQueues(r); err != nil {
			fmt.Fprintf(os.Stderr, "warning: reconcile queues after acceptance: %v\n", err)
		}
		return fmt.Errorf("cannot abandon %s: accepted work is kept (its memory record is complete)", id)
	}
	if s.Status == StatusAbandoned {
		return nil
	}
	feature := p.Feature
	if feature == "" {
		feature = s.Feature
		if o, err := r.LoadOutcome(id); err == nil {
			feature = o.Feature
		}
	}
	i := abandonIntent{ID: id, Feature: feature, Date: memoryNow().UTC().Format("2006-01-02")}
	if err := writePrivateJSON(abandonPath(r, id), i); err != nil {
		return fmt.Errorf("persist abandon %s: %w", id, err)
	}
	if err := finishAbandon(r, &i); err != nil {
		return err
	}
	if err := reconcileQueues(r); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reconcile queues after abandon: %v\n", err)
	}
	return nil
}

// keepAccepted completes pending acceptance before reporting that abandonment is forbidden.
// A published characterization/revision is accepted by planning, even before its log lands.
func keepAccepted(r Repo, id string, p PlanState, planned bool, s *State) (bool, error) {
	rec, pending, err := LoadPending(r, id)
	if err != nil {
		return false, err
	}
	if planned && p.Phase == PlanPublishing {
		if sha, ok := branchCommit(r.Root, IntegrationBranch(id)); ok && sha == p.PlanCommit {
			p.Phase = PlanPublished
		}
	}
	acceptedPlan := planned && (p.Kind == PlanCharacterize || p.NoImplementation) && (p.Phase == PlanPublished || p.Phase == PlanLogged)
	if acceptedPlan && p.Status != StatusAbandoned {
		if p.Phase == PlanLogged {
			return true, nil
		}
		dir, err := MemoryDir(r)
		if err != nil {
			return true, err
		}
		var sp Spec
		if p.MemoryCommit != "" {
			sp, err = pinnedSpec(dir, p.MemoryCommit, p.Feature)
		} else {
			var memory MemorySnapshot
			memory, err = LoadMemory(r)
			var ok bool
			sp, ok = memory.Spec(p.Feature)
			if err == nil && !ok {
				err = fmt.Errorf("accepted plan %s: feature %s is missing", id, p.Feature)
			}
		}
		if err != nil {
			return true, err
		}
		cfg, err := r.LoadConfig()
		if err != nil {
			return true, err
		}
		pl := planner{repo: r, cfg: cfg, pcfg: cfg.plan(), spec: sp, memDir: dir, state: &p}
		if err := pl.recordPlan(); err != nil {
			return true, err
		}
		return true, nil
	}
	if !pending && s.Status != StatusAccepted {
		return false, nil
	}
	if !pending && s.MemoryError != "" {
		// Recover the rare acceptance whose SavePending failed. New ephemeral do states
		// retain the memory link; older registered outcomes can be read from their contract.
		rec = PendingRecord{OutcomeID: id, Feature: s.Feature, Change: s.Change, Note: s.Changelog, Commit: s.Head}
		if planned {
			cfg, err := r.LoadConfig()
			if err != nil {
				return true, err
			}
			o, gen, err := loadGeneratedRun(r, cfg, p)
			if err != nil {
				return true, err
			}
			memory, err := LoadMemory(r)
			if err != nil {
				return true, err
			}
			if p.MemoryCommit != "" {
				memory, err = loadMemoryAt(memory.Dir, p.MemoryCommit)
				if err != nil {
					return true, err
				}
			}
			ru := runner{repo: r, cfg: cfg, outcome: o, state: s, generated: gen, memory: memory}
			rec.Feature, rec.Change, rec.Note, rec.MemoryBase = o.Feature, o.Change, o.Changelog, p.MemoryCommit
			if rec.Implementation, err = ru.implementation(); err != nil {
				return true, err
			}
		} else if o, err := r.LoadOutcome(id); err == nil {
			rec.Feature, rec.Change, rec.Note = o.Feature, o.Change, o.Changelog
		}
		if rec.Feature == "" {
			return true, fmt.Errorf("accepted %s: missing pending memory link; restore the acceptance record", id)
		}
		if err := SavePending(r, rec); err != nil {
			return true, err
		}
		pending = true
	}
	if pending {
		v, c, err := RecordChange(r, rec, "")
		if err != nil {
			return true, fmt.Errorf("complete accepted %s before refusing abandon: %w", id, err)
		}
		s.Status, s.MemoryVersion, s.MemoryCommit, s.MemoryError = StatusAccepted, v, c, ""
		if err := saveState(r, s); err != nil {
			return true, err
		}
		if err := ClearPending(r, id); err != nil {
			return true, err
		}
	}
	return true, nil
}

func recoverAbandons(r Repo) error {
	files, err := filepath.Glob(filepath.Join(r.Root, ".aeh", "abandon", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := rejectLink(file); err != nil {
			return err
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var i abandonIntent
		if err := json.Unmarshal(b, &i); err != nil {
			return fmt.Errorf("abandon intent: %w", err)
		}
		if !validID(i.ID) || strings.TrimSuffix(filepath.Base(file), ".json") != i.ID {
			return errors.New("invalid abandon intent ID")
		}
		if i.Done || i.Cancelled {
			continue
		}
		unlock, err := lockOutcome(r, i.ID)
		if err != nil {
			return err
		}
		err = finishAbandon(r, &i)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func finishAbandon(r Repo, i *abandonIntent) error {
	p, planned, err := LoadPlanState(r, i.ID)
	if err != nil {
		return err
	}
	s, err := loadState(r, i.ID)
	if err != nil {
		return err
	}
	if accepted, err := keepAccepted(r, i.ID, p, planned, &s); err != nil {
		return err
	} else if accepted {
		i.Cancelled, i.Reason = true, "accepted work is kept (its memory record is complete)"
		if err := writePrivateJSON(abandonPath(r, i.ID), i); err != nil {
			return fmt.Errorf("cancel abandon intent %s: %w", i.ID, err)
		}
		return nil
	}
	if planned {
		p.Status, p.Question = StatusAbandoned, ""
		if err := savePlanState(r, &p); err != nil {
			return err
		}
	}
	s.Status, s.Question = StatusAbandoned, ""
	if err := saveState(r, &s); err != nil {
		return err
	}
	dir, err := MemoryDir(r)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if _, err := initMemoryLocked(r); err != nil {
			return fmt.Errorf("initialize abandon memory: %w", err)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		marker := "AEH-Abandon: " + i.ID
		if _, _, found, err := findRecord(dir, marker); err != nil {
			return err
		} else if !found {
			log, err := os.ReadFile(filepath.Join(dir, "wiki", "log.md"))
			if err != nil {
				return err
			}
			if err := beginLiveWrite(r, dir, "abandon "+i.ID, marker, map[string]string{"wiki/log.md": string(log) + fmt.Sprintf("\n[%s] abandon | %s | %s\n", i.Date, i.ID, i.Feature)}); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	i.Done = true
	return writePrivateJSON(abandonPath(r, i.ID), i)
}
