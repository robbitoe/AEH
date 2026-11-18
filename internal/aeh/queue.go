package aeh

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// QueuedApproval is an approved immutable target, kept in the owner's versioned memory.
// spec.md and approval.yaml continue to describe the version in force until activation.
// Activated entries remain as history, so the version rule also sees queued approvals.
type QueuedApproval struct {
	Feature           string        `json:"feature"`
	CurrentVersion    string        `json:"current_version"`
	InForceVersion    string        `json:"in_force_version,omitempty"`
	NoCurrentApproval bool          `json:"no_current_approval,omitempty"`
	Spec              string        `json:"spec"`
	Approval          SpecApproval  `json:"approval"`
	Paths             []string      `json:"paths"`
	PathsChanged      bool          `json:"paths_changed"`
	Blockers          []PendingWork `json:"blockers"`
	Activated         bool          `json:"activated,omitempty"`
	ActivationWaiting string        `json:"-"` // transient diagnostic; never changes the owner's queue
}

// PendingWork combines all unfinished records of one outcome into one blocker.
type PendingWork struct {
	ID      string   `json:"id"`
	Feature string   `json:"feature,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Reason  string   `json:"reason"`
}

func queueRel(feature string) string { return "wiki/features/" + feature + "/queue.json" }

func parseQueue(b []byte) (QueuedApproval, error) {
	var q QueuedApproval
	if err := json.Unmarshal(b, &q); err != nil {
		return q, fmt.Errorf("approval queue: %w", err)
	}
	s, err := parseSpec(q.Spec)
	if err != nil {
		return q, fmt.Errorf("queued spec: %w", err)
	}
	if s.Feature != q.Feature || s.Version != q.Approval.Version || approvalDigest(s.Title, s.Paths, s.Body) != q.Approval.Content {
		return q, errors.New("approval queue: spec does not match its approval")
	}
	if err := checkSlug(q.Feature); err != nil {
		return q, err
	}
	if q.InForceVersion != "" {
		if _, err := parseSemver(q.InForceVersion); err != nil {
			return q, fmt.Errorf("approval queue version in force: %w", err)
		}
	}
	return q, nil
}

func listQueues(dir string) ([]QueuedApproval, error) {
	files, err := filepath.Glob(filepath.Join(dir, "wiki", "features", "*", "queue.json"))
	if err != nil {
		return nil, err
	}
	var out []QueuedApproval
	for _, file := range files {
		if err := rejectLink(file); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		q, err := parseQueue(b)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", queueRel(filepath.Base(filepath.Dir(file))), err)
		}
		if q.Feature != filepath.Base(filepath.Dir(file)) {
			return nil, errors.New("approval queue: feature directory mismatch")
		}
		if !q.Activated {
			out = append(out, q)
		}
	}
	return out, nil
}

func queueJSON(q QueuedApproval) string {
	b, _ := json.MarshalIndent(q, "", "  ")
	return string(b) + "\n"
}

// pendingWork uses the registry rather than branches: branches and evidence survive both
// acceptance and abandonment. A plan requiring no implementation is done only once logged.
func pendingWork(r Repo) ([]PendingWork, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, err
	}
	r = Repo{Root: main}
	plans, err := ListPlanStates(r)
	if err != nil {
		return nil, err
	}
	states, err := ListStates(r)
	if err != nil {
		return nil, err
	}
	pending, err := ListPending(r)
	if err != nil {
		return nil, err
	}
	byID := map[string]PendingWork{}
	runs := map[string]State{}
	for _, s := range states {
		runs[s.ID] = s
	}
	for _, p := range plans {
		if p.Status == StatusAbandoned {
			continue
		}
		s := runs[p.ID]
		done := (p.Kind == PlanCharacterize || p.NoImplementation) && p.Phase == PlanLogged || s.Status == StatusAccepted && s.MemoryError == ""
		if done {
			continue
		}
		paths := []string(nil)
		if p.MemoryCommit != "" {
			dir, err := MemoryDir(r)
			if err != nil {
				return nil, err
			}
			sp, err := pinnedSpec(dir, p.MemoryCommit, p.Feature)
			if err != nil {
				return nil, err
			}
			paths = sp.Paths
		}
		byID[p.ID] = PendingWork{ID: p.ID, Feature: p.Feature, Paths: paths, Reason: "plan " + p.Phase}
	}
	for _, s := range states {
		if s.Status == StatusAbandoned || s.Status == StatusAccepted && s.MemoryError == "" {
			continue
		}
		w, ok := byID[s.ID]
		if !ok {
			w = PendingWork{ID: s.ID, Feature: s.Feature, Paths: s.Paths}
			if o, err := r.LoadOutcome(s.ID); err == nil {
				w.Feature = o.Feature
				for _, t := range o.Tasks {
					w.Paths = append(w.Paths, t.Paths...)
				}
			}
			// Old ephemeral do states had no contract paths; uncertainty reserves all paths.
			if len(w.Paths) == 0 {
				w.Paths = []string{"**"}
			}
		}
		w.Reason = "run " + s.Status
		byID[s.ID] = w
	}
	for _, p := range pending {
		w := byID[p.OutcomeID]
		if len(w.Paths) == 0 {
			w.Paths = runs[p.OutcomeID].Paths
			if o, err := r.LoadOutcome(p.OutcomeID); err == nil {
				for _, t := range o.Tasks {
					w.Paths = append(w.Paths, t.Paths...)
				}
			}
		}
		w.ID, w.Feature, w.Reason = p.OutcomeID, p.Feature, "pending memory record"
		byID[p.OutcomeID] = w
	}
	var out []PendingWork
	for _, w := range byID {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func queueBlockers(q QueuedApproval, work []PendingWork, snap MemorySnapshot) []PendingWork {
	var out []PendingWork
	for _, w := range work {
		owns := false
		if s, ok := snap.Spec(w.Feature); ok {
			owns = pathsOverlap(s.OwnedPaths(), q.Paths)
		}
		if w.Feature == q.Feature || q.PathsChanged && (owns || pathsOverlap(w.Paths, q.Paths)) {
			out = append(out, w)
		}
	}
	return out
}

// admitWork is called under the project lock before reservation, execution or ownership
// mutation. Only an outcome already registered as a blocker may resume through a queue.
func admitWork(r Repo, feature string, paths []string, id string) error {
	return admitWorkExceptQueue(r, feature, paths, id, "")
}

func admitWorkExceptQueue(r Repo, feature string, paths []string, id, except string) error {
	dir, err := MemoryDir(r)
	if err != nil {
		return err
	}
	queues, err := listQueues(dir)
	if err != nil {
		return err
	}
	if len(queues) == 0 {
		return nil
	}
	if feature != "" {
		snap, err := LoadMemory(r)
		if err != nil {
			return err
		}
		if s, ok := snap.Spec(feature); ok {
			paths = append(append([]string{}, paths...), s.OwnedPaths()...)
		}
	}
	for _, q := range queues {
		if q.Feature == except {
			continue
		}
		if feature != q.Feature && !pathsOverlap(paths, q.Paths) {
			continue
		}
		allowed := false
		for _, b := range q.Blockers {
			if id != "" && id == b.ID {
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("queued approval %s %s reserves this feature or its paths; finish or abandon blockers: %s", q.Feature, q.Approval.Version, blockerNames(q.Blockers))
		}
	}
	return nil
}

func blockerNames(work []PendingWork) string {
	var names []string
	for _, w := range work {
		names = append(names, w.ID+" ("+w.Reason+")")
	}
	return strings.Join(names, ", ")
}

func admitFeatureImports(r Repo, dir string, files map[string][]byte) error {
	// Ownership first: a batch prepared before another feature claimed its paths (and
	// finished after a crash) must not publish a second owner.
	memory, err := loadMemoryDir(dir)
	if err != nil {
		return err
	}
	for _, rel := range sortedKeys(files) {
		if !strings.HasPrefix(rel, "wiki/features/") || !strings.HasSuffix(rel, "/spec.md") {
			continue
		}
		spec, err := parseSpec(string(files[rel]))
		if err != nil {
			return err
		}
		base := spec.Paths
		if cur, ok := memory.Spec(spec.Feature); ok {
			base = cur.Paths
		}
		if err := checkPathClaim(dir, memory, spec.Feature, spec.Paths, base); err != nil {
			return err
		}
	}
	queues, err := listQueues(dir)
	if err != nil || len(queues) == 0 {
		return err
	}
	for rel, content := range files {
		if !strings.HasPrefix(rel, "wiki/features/") || !strings.HasSuffix(rel, "/spec.md") {
			continue
		}
		spec, err := parseSpec(string(content))
		if err != nil {
			return err
		}
		if err := admitWork(r, spec.Feature, spec.Paths, ""); err != nil {
			return err
		}
	}
	return nil
}

// previousApprovedSpec finds the spec that the approval in force actually approved,
// including runner-authored changelog additions; the owner's newer draft is queued intact.
func previousApprovedSpec(dir string, s Spec) (string, error) {
	if s.Approval == nil {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(specRel(s.Feature))))
		return string(b), err
	}
	log, err := git(dir, "log", "--full-history", "--format=%H", "--", approvalRel(s.Feature))
	if err != nil {
		return "", err
	}
	for _, sha := range strings.Fields(log) {
		text, ok := gitBlob(dir, sha, specRel(s.Feature))
		if !ok {
			continue
		}
		sp, err := parseSpec(text)
		if err == nil && approvalDigest(sp.Title, sp.Paths, sp.Body) == s.Approval.Content {
			return text, nil
		}
	}
	return "", errors.New("previous approved spec is missing from memory history")
}

func enqueueApproval(r Repo, snap MemorySnapshot, spec Spec, a SpecApproval) (*QueuedApproval, error) {
	queues, err := listQueues(snap.Dir)
	if err != nil {
		return nil, err
	}
	var existing *QueuedApproval
	for _, q := range queues {
		if q.Feature == spec.Feature {
			copy := q
			existing = &copy
		}
	}
	work, err := pendingWork(r)
	if err != nil {
		return nil, err
	}
	prior, err := previousApprovedSpec(snap.Dir, spec)
	if existing != nil {
		prior, err = queuedVersionInForce(snap.Dir, spec, *existing)
	}
	if err != nil {
		return nil, err
	}
	old, _ := parseSpec(prior)
	q := QueuedApproval{Feature: spec.Feature, CurrentVersion: old.Version, InForceVersion: old.Version, NoCurrentApproval: spec.Approval == nil, Approval: a, Paths: append(append([]string{}, old.Paths...), spec.Paths...), PathsChanged: !reflect.DeepEqual(old.Paths, spec.Paths)}
	if q.NoCurrentApproval {
		q.CurrentVersion = "none"
	} else {
		q.CurrentVersion = spec.Approval.Version
	}
	q.Blockers = queueBlockers(q, work, snap)
	if len(q.Blockers) == 0 && existing == nil {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(snap.Dir, filepath.FromSlash(specRel(spec.Feature))))
	if err != nil {
		return nil, err
	}
	q.Spec = string(raw)
	log, err := os.ReadFile(filepath.Join(snap.Dir, "wiki", "log.md"))
	if err != nil {
		return nil, err
	}
	files := map[string]string{specRel(spec.Feature): prior, queueRel(spec.Feature): queueJSON(q), "wiki/log.md": string(log) + fmt.Sprintf("\n[%s] queue approval | %s %s | %s\n", a.Date, spec.Feature, a.Version, blockerNames(q.Blockers))}
	if err := beginLiveWrite(r, snap.Dir, "queue approval "+spec.Feature+" "+a.Version, "AEH-Queue: "+spec.Feature+" "+a.Version+" "+a.Content, files); err != nil {
		return nil, err
	}
	return &q, nil
}

// reconcileQueues always recomputes before activation, even if all originally persisted
// blockers appear finished. A newly discovered blocker is persisted and keeps the queue.
func reconcileQueues(r Repo) error {
	_, err := reconcileQueueStatus(r)
	return err
}

func reconcileQueueStatus(r Repo) ([]QueuedApproval, error) {
	dir, err := MemoryDir(r)
	if err != nil {
		return nil, err
	}
	queues, err := listQueues(dir)
	if err != nil || len(queues) == 0 {
		return nil, err
	}
	var waiting []QueuedApproval
	for _, q := range queues {
		if err := reconcileQueue(r, dir, &q); err != nil {
			q.ActivationWaiting = err.Error()
			fmt.Fprintf(os.Stderr, "warning: queued approval %s %s: activation waiting: %v\n", q.Feature, q.Approval.Version, err)
		}
		if !q.Activated {
			waiting = append(waiting, q)
		}
	}
	return waiting, nil
}

func reconcileQueue(r Repo, dir string, q *QueuedApproval) error {
	// A failed earlier activation may have partially written memory. Recover only at
	// command entry, before taking snapshots, so another queue cannot use stale bytes.
	if _, err := os.Stat(liveWritePath(r)); err == nil {
		return errors.New("pending memory transaction in .aeh/live-write.json; retry the command to recover it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check pending memory transaction: %w", err)
	}
	snap, err := LoadMemory(r)
	if err != nil {
		return err
	}
	work, err := pendingWork(r)
	if err != nil {
		return err
	}
	blockers := queueBlockers(*q, work, snap)
	if len(blockers) > 0 {
		if !reflect.DeepEqual(q.Blockers, blockers) {
			q.Blockers = blockers
			// Updating blockers is a memory transaction too: interrupted writes recover.
			if err := beginLiveWrite(r, dir, "refresh queue "+q.Feature, "AEH-Queue-Refresh: "+q.Feature+" "+q.Approval.Version+" "+queueDigest(*q), map[string]string{queueRel(q.Feature): queueJSON(*q)}); err != nil {
				return err
			}
		}
		return nil
	}
	q.Blockers = nil
	current, ok := snap.Spec(q.Feature)
	if !ok {
		return fmt.Errorf("queued feature %s disappeared", q.Feature)
	}
	unchanged := current.Approved()
	if q.NoCurrentApproval {
		target, err := parseSpec(q.Spec)
		if err != nil {
			return err
		}
		unchanged = current.Approval == nil && approvalDigest(current.Title, current.Paths, withoutAcceptanceLines(current.Body)) == approvalDigest(target.Title, target.Paths, withoutAcceptanceLines(target.Body))
	}
	if !unchanged || !queueCurrentVersionMatches(*q, current) {
		return fmt.Errorf("version in force was edited; save your draft outside spec.md, restore %s from the last memory commit where it matched approval.yaml (including acceptance records), and commit the restoration in memory; the queued target is in %s; AEH will never overwrite your edit", specRel(q.Feature), queueRel(q.Feature))
	}
	if version := queueCollision(*q, current); version != "" {
		return fmt.Errorf("recorded version in force %s is >= queued version %s; copy the target spec from %s into %s, raise version above both, commit in memory, then run: aeh memory approve %s", version, q.Approval.Version, queueRel(q.Feature), specRel(q.Feature), q.Feature)
	}
	// Preserve records made while the old version was finishing. Only runner-validated
	// acceptance lines are carried into the immutable target's changelog.
	text := strings.TrimRight(q.Spec, "\n") + "\n"
	for _, line := range strings.Split(current.Body, "\n") {
		if acceptedLine.MatchString(line) && !strings.Contains(text, line) {
			text += line + "\n"
		}
	}
	sp, err := parseSpec(text)
	if err != nil {
		return err
	}
	a := q.Approval
	a.Content = approvalDigest(sp.Title, sp.Paths, sp.Body)
	b, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	activated := *q
	activated.Activated = true
	log, err := os.ReadFile(filepath.Join(dir, "wiki", "log.md"))
	if err != nil {
		return err
	}
	for i := range snap.Specs {
		if snap.Specs[i].Feature == q.Feature {
			snap.Specs[i] = sp
		}
	}
	files := map[string]string{specRel(q.Feature): text, approvalRel(q.Feature): string(b), queueRel(q.Feature): queueJSON(activated), "wiki/index.md": renderIndex(snap.Specs), "wiki/log.md": string(log) + fmt.Sprintf("\n[%s] activate approval | %s %s\n", memoryNow().UTC().Format("2006-01-02"), q.Feature, a.Version)}
	if err := beginLiveWrite(r, dir, "activate approval "+q.Feature+" "+a.Version, "AEH-Queue-Activate: "+q.Feature+" "+a.Version+" "+q.Approval.Content, files); err != nil {
		return err
	}
	q.Activated = true
	return nil
}

// Only recorded versions count: an owner's draft frontmatter is not an acceptance.
func queueCollision(q QueuedApproval, current Spec) string {
	var high string
	for _, line := range strings.Split(current.Body, "\n") {
		if m := acceptedLine.FindStringSubmatch(line); m != nil {
			v, err := parseSemver(m[1])
			if err == nil && !semverLess(v, mustSemver(q.Approval.Version)) && (high == "" || semverLess(mustSemver(high), v)) {
				high = m[1]
			}
		}
	}
	return high
}

// Approval digests omit frontmatter versions because records bump them. Distinguish
// those recorded bumps from an owner's version-only draft, which must also be kept.
func queueCurrentVersionMatches(q QueuedApproval, current Spec) bool {
	base := q.CurrentVersion
	if q.InForceVersion != "" {
		base = q.InForceVersion
	} else if q.NoCurrentApproval {
		target, err := parseSpec(q.Spec)
		if err != nil {
			return false
		}
		base = target.Version
	}
	high, err := parseSemver(base)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(current.Body, "\n") {
		if m := acceptedLine.FindStringSubmatch(line); m != nil {
			if v, err := parseSemver(m[1]); err == nil && semverLess(high, v) {
				base, high = m[1], v
			}
		}
	}
	return current.Version == base
}

// A collision is the sole exception to refusing another approval while queued. The
// owner may raise the same immutable target, without changing its content or scope.
func queuedReplacement(snap MemorySnapshot, spec Spec) (bool, error) {
	queues, err := listQueues(snap.Dir)
	if err != nil {
		return false, err
	}
	for _, q := range queues {
		if q.Feature != spec.Feature {
			continue
		}
		prior, err := queuedVersionInForce(snap.Dir, spec, q)
		if err != nil {
			return false, err
		}
		current, err := parseSpec(prior)
		if err != nil {
			return false, err
		}
		collision := queueCollision(q, current)
		if collision == "" {
			return false, nil
		}
		if approvalDigest(spec.Title, spec.Paths, spec.Body) != q.Approval.Content || !semverLess(mustSemver(q.Approval.Version), mustSemver(spec.Version)) || !semverLess(mustSemver(collision), mustSemver(spec.Version)) {
			return false, fmt.Errorf("queued approval %s: collision with recorded version %s; approve the same queued content with a version above %s and %s", q.Feature, collision, collision, q.Approval.Version)
		}
		return true, nil
	}
	return false, nil
}

func queuedVersionInForce(dir string, spec Spec, q QueuedApproval) (string, error) {
	if !q.NoCurrentApproval {
		return previousApprovedSpec(dir, spec)
	}
	// A first approval can queue behind manual work without approval.yaml. Find the
	// runner's last record rather than mistaking the owner's replacement draft for it.
	target, err := parseSpec(q.Spec)
	if err != nil {
		return "", err
	}
	log, err := git(dir, "log", "--full-history", "--format=%H", "--fixed-strings", "--grep=AEH-Record:", "--", specRel(q.Feature))
	if err != nil {
		return "", fmt.Errorf("find recorded version in force for %s: %w", q.Feature, err)
	}
	for _, sha := range strings.Fields(log) {
		text, ok := gitBlob(dir, sha, specRel(q.Feature))
		current, err := parseSpec(text)
		if ok && err == nil && approvalDigest(current.Title, current.Paths, withoutAcceptanceLines(current.Body)) == approvalDigest(target.Title, target.Paths, withoutAcceptanceLines(target.Body)) {
			return text, nil
		}
	}
	return q.Spec, nil
}

func queueDigest(q QueuedApproval) string {
	return outputDigest("approval-queue", map[string][]byte{"queue": []byte(queueJSON(q))})
}

func withoutAcceptanceLines(body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if !acceptedLine.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// belowQueuedTarget keeps a record's version below an approval queued for the same
// feature. Owners queue the next minor version while work is pending; a minor bump by
// the record would reach it and make activation wait for a manual recovery. A patch bump
// stays below it. When even that collides, the version is unchanged and activation
// reports the collision as before.
func belowQueuedTarget(dir, feature, from, version, current string) string {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(queueRel(feature))))
	if err != nil {
		return version
	}
	q, err := parseQueue(b)
	if err != nil || q.Activated {
		return version
	}
	target, err := parseSemver(q.Approval.Version)
	if err != nil || semverLess(mustSemver(version), target) {
		return version
	}
	patch, err := bumpVersion(from, "patch")
	if err != nil {
		return version
	}
	p := mustSemver(patch)
	if cur, err := parseSemver(current); err == nil && !semverLess(cur, p) || !semverLess(p, target) {
		return version
	}
	return patch
}
