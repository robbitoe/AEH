package aeh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// liveWrite journals a runner-owned memory transaction before touching memory. Recovery
// accepts only its original bytes or its intended bytes, never overwriting an owner edit.
type liveWrite struct {
	Head    string             `json:"head"`
	Subject string             `json:"subject"`
	Marker  string             `json:"marker"`
	Before  map[string]*string `json:"before"`
	After   map[string]string  `json:"after"`
}

func liveWritePath(r Repo) string { return filepath.Join(r.Root, ".aeh", "live-write.json") }

func beginLiveWrite(r Repo, dir, subject, marker string, files map[string]string) error {
	main, err := MainCheckout(r)
	if err != nil {
		return err
	}
	r = Repo{Root: main}
	// Never replace a pending journal, even when this transaction is unrelated.
	if _, err := os.Stat(liveWritePath(r)); err == nil {
		return errors.New("pending memory transaction in .aeh/live-write.json; retry the command to recover it before another memory write")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check pending memory transaction: %w", err)
	}
	if err := memoryClean(dir); err != nil {
		return err
	}
	head, err := memoryHeadCommit(dir)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(marker, "AEH-Abandon: ") {
		marker += " " + head
	}
	tx := liveWrite{Head: head, Subject: subject, Marker: marker, Before: map[string]*string{}, After: map[string]string{}}
	for rel := range files {
		b, ok, err := fileAt(dir, head, rel)
		if err != nil {
			return err
		}
		if ok && b == files[rel] {
			continue
		}
		tx.After[rel] = files[rel]
		if ok {
			tx.Before[rel] = &b
		} else {
			tx.Before[rel] = nil
		}
	}
	if len(tx.After) == 0 {
		return nil
	}
	if err := writePrivateJSON(liveWritePath(r), tx); err != nil {
		return fmt.Errorf("persist memory transaction: %w", err)
	}
	return resumeLiveWrite(r)
}

func resumeLiveWrite(r Repo) error {
	b, err := os.ReadFile(liveWritePath(r))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var tx liveWrite
	if err := json.Unmarshal(b, &tx); err != nil {
		return fmt.Errorf("memory transaction: %w", err)
	}
	dir, err := MemoryDir(r)
	if err != nil {
		return err
	}
	if err := rejectLinksInside(dir); err != nil {
		return err
	}
	if !isFullSHA(tx.Head) || tx.Marker == "" || len(tx.After) == 0 || len(tx.Before) != len(tx.After) {
		return errors.New("invalid memory transaction")
	}
	if _, _, ok, err := findRecord(dir, tx.Marker); err != nil {
		return err
	} else if ok {
		return os.Remove(liveWritePath(r))
	}
	head, err := memoryHeadCommit(dir)
	if err != nil {
		return err
	}
	if head != tx.Head {
		return errors.New("memory transaction: HEAD changed; inspect .aeh/live-write.json relative to the main project checkout and its intended change in memory; after checking that the change is committed or no longer wanted, remove .aeh/live-write.json, then retry (AEH will not overwrite your edits)")
	}
	// Include staged and untracked paths; unrelated edits must remain the owner's.
	changed, err := git(dir, "diff", "HEAD", "--name-only", "-z")
	if err != nil {
		return err
	}
	untracked, err := git(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(changed+untracked, "\x00") {
		if rel != "" {
			if _, ok := tx.After[rel]; !ok {
				return fmt.Errorf("memory transaction: unrelated edit to %s", rel)
			}
		}
	}
	var paths []string
	for rel, after := range tx.After {
		if err := checkRelPath(rel); err != nil {
			return fmt.Errorf("memory transaction path: %w", err)
		}
		if !strings.HasPrefix(rel, "wiki/") {
			return errors.New("memory transaction outside wiki")
		}
		before, ok := tx.Before[rel]
		if !ok {
			return errors.New("memory transaction missing original content")
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		now, err := os.ReadFile(full)
		original := before == nil && errors.Is(err, os.ErrNotExist) || err == nil && before != nil && bytes.Equal(now, []byte(*before))
		if !original && (err != nil || !bytes.Equal(now, []byte(after))) {
			return fmt.Errorf("memory transaction: %s changed; nothing was overwritten", rel)
		}
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	// A crash releases no reservation. If work nevertheless appeared before an unfinished
	// activation recovers, put its writes back and let reconciliation persist the new set.
	if strings.HasPrefix(tx.Subject, "activate approval ") {
		for rel, before := range tx.Before {
			if !strings.HasSuffix(rel, "/queue.json") || before == nil {
				continue
			}
			q, err := parseQueue([]byte(*before))
			if err != nil {
				return err
			}
			snap, err := loadMemoryAt(dir, tx.Head)
			if err != nil {
				return err
			}
			work, err := pendingWork(r)
			if err != nil {
				return err
			}
			if len(queueBlockers(q, work, snap)) == 0 {
				continue
			}
			for _, path := range paths {
				full := filepath.Join(dir, filepath.FromSlash(path))
				if original := tx.Before[path]; original != nil {
					if err := memoryWriteFile(full, []byte(*original)); err != nil {
						return err
					}
				} else if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if _, err := git(dir, append([]string{"reset", "-q", "HEAD", "--"}, paths...)...); err != nil {
				return err
			}
			return os.Remove(liveWritePath(r))
		}
	}
	for _, rel := range paths {
		if err := memoryWriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(tx.After[rel])); err != nil {
			return fmt.Errorf("recover memory transaction: %w", err)
		}
	}
	if err := commitMemory(dir, paths, tx.Subject, tx.Marker); err != nil {
		return err
	}
	return os.Remove(liveWritePath(r))
}

// lockLiveProject preserves the fail-fast project lock, then recovers durable intents.
// Internal helpers called below never reacquire it. Outcome locks always come second.
func lockLiveProject(r Repo) (func(), error) {
	unlock, _, err := lockLiveProjectMode(r, false)
	return unlock, err
}

type liveRecovery struct {
	Queues []QueuedApproval
	Errors []string
}

// Read-only callers keep the fail-fast lock but receive recovery diagnostics alongside
// useful state. They must not continue any recovery writes after a journal error.
func lockLiveProjectMode(r Repo, readOnly bool) (func(), liveRecovery, error) {
	var result liveRecovery
	unlock, err := lockProject(r)
	if err != nil {
		return nil, result, err
	}
	main, err := MainCheckout(r)
	if err != nil {
		unlock()
		return nil, result, err
	}
	r = Repo{Root: main}
	err = resumeLiveWrite(r)
	if err == nil {
		err = recoverAbandons(r)
	}
	if err == nil {
		result.Queues, err = reconcileQueueStatus(r)
	} else if readOnly {
		if dir, derr := MemoryDir(r); derr == nil {
			var qerr error
			result.Queues, qerr = listQueues(dir)
			if qerr != nil {
				result.Errors = append(result.Errors, qerr.Error())
			}
			for i := range result.Queues {
				result.Queues[i].ActivationWaiting = err.Error()
			}
		}
	}
	if err != nil {
		if !readOnly {
			unlock()
			return nil, result, fmt.Errorf("recover live work: %w", err)
		}
		result.Errors = append(result.Errors, fmt.Sprintf("recover live work: %v", err))
	}
	return unlock, result, nil
}

// RecoverLiveWork recovers queued approvals and abandon intents for read-only commands.
func RecoverLiveWork(r Repo) error {
	unlock, result, err := lockLiveProjectMode(r, true)
	if err != nil {
		return err
	}
	unlock()
	for _, diagnostic := range result.Errors {
		fmt.Fprintln(os.Stderr, "warning:", diagnostic)
	}
	return nil
}
