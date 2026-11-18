package aeh

import (
	"fmt"
	"sort"
	"strings"
)

// A plan pins the memory commit it was reserved at. Everything the plan and its run read from
// memory (the spec, its approval and criteria, the last implementation, and which code every
// feature owns) comes from that commit, so one plan never mixes two versions of a spec, even
// when the owner approves a newer one while it is in flight.

// memoryHeadCommit is the memory repository's current commit.
func memoryHeadCommit(dir string) (string, error) {
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("memory head: %w", err)
	}
	return head, nil
}

// committedMemory reads memory as its HEAD commit has it, ignoring uncommitted edits.
func committedMemory(r Repo) (MemorySnapshot, error) {
	dir, err := MemoryDir(r)
	if err != nil {
		return MemorySnapshot{}, err
	}
	head, err := memoryHeadCommit(dir)
	if err != nil {
		return MemorySnapshot{}, err
	}
	return loadMemoryAt(dir, head)
}

// loadMemoryAt reads every spec as memory had it at a commit of its own history. The commit
// must still be an ancestor of memory's HEAD: rewritten history voids the pin.
func loadMemoryAt(dir, commit string) (MemorySnapshot, error) {
	snap := MemorySnapshot{Dir: dir, Commit: commit}
	if !isFullSHA(commit) {
		return snap, fmt.Errorf("pinned memory commit %q is not a full SHA", commit)
	}
	if err := verifyMemoryRepo(dir); err != nil {
		return snap, err
	}
	if _, err := git(dir, "merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return snap, fmt.Errorf("pinned memory commit %s is no longer in memory's history", short(commit))
	}
	snap.Present = true
	tree, err := git(dir, "ls-tree", "-z", commit+":wiki/features")
	if err != nil {
		return snap, fmt.Errorf("memory at %s: wiki/features: %w", short(commit), err)
	}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		meta, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return snap, fmt.Errorf("memory at %s: unreadable tree entry", short(commit))
		}
		if fields[1] != "tree" {
			if strings.HasPrefix(name, ".") {
				continue
			}
			return snap, fmt.Errorf("memory at %s: wiki/features/%s: only feature directories belong here", short(commit), name)
		}
		if err := checkSlug(name); err != nil {
			return snap, fmt.Errorf("memory at %s: wiki/features/%s: %w", short(commit), name, err)
		}
		s, err := specAt(dir, commit, name)
		if err != nil {
			return snap, err
		}
		snap.Specs = append(snap.Specs, s)
	}
	sort.Slice(snap.Specs, func(i, j int) bool { return snap.Specs[i].Feature < snap.Specs[j].Feature })
	snap.Fingerprint = fingerprint(snap)
	return snap, nil
}

// specAt reads one feature's spec, approval and implementations at a memory commit.
func specAt(dir, commit, feature string) (Spec, error) {
	text, ok, err := fileAt(dir, commit, specRel(feature))
	if err != nil {
		return Spec{}, err
	}
	if !ok {
		return Spec{}, fmt.Errorf("memory at %s: feature %s has no spec.md", short(commit), feature)
	}
	s, err := parseSpec(text)
	if err != nil {
		return Spec{}, fmt.Errorf("memory at %s: feature %s: %w", short(commit), feature, err)
	}
	if s.Feature != feature {
		return Spec{}, fmt.Errorf("memory at %s: feature %s: spec declares feature %q", short(commit), feature, s.Feature)
	}
	if b, ok, err := fileAt(dir, commit, approvalRel(feature)); err != nil {
		return Spec{}, err
	} else if ok {
		if s.Approval, err = parseApproval([]byte(b)); err != nil {
			return Spec{}, fmt.Errorf("memory at %s: feature %s: %w", short(commit), feature, err)
		}
	}
	if b, ok, err := fileAt(dir, commit, implementedRel(feature)); err != nil {
		return Spec{}, err
	} else if ok {
		if s.Implemented, err = parseImplementations([]byte(b)); err != nil {
			return Spec{}, fmt.Errorf("memory at %s: feature %s: %w", short(commit), feature, err)
		}
	}
	legacyImplementationPaths(dir, &s)
	s.rev = commit
	return s, nil
}

// fileAt reads a regular file at a memory commit. A missing path is (false, nil); a path that
// is a directory, a link or a submodule is an error, as loadMemoryDir rejects them on disk.
func fileAt(dir, commit, rel string) (string, bool, error) {
	out, err := git(dir, "ls-tree", "-z", commit, "--", rel)
	if err != nil {
		return "", false, fmt.Errorf("memory at %s: %s: %w", short(commit), rel, err)
	}
	if out == "" {
		return "", false, nil
	}
	meta, name, _ := strings.Cut(strings.TrimSuffix(out, "\x00"), "\t")
	fields := strings.Fields(meta)
	if name != rel || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return "", false, fmt.Errorf("memory at %s: %s must be a regular file", short(commit), rel)
	}
	b, ok := gitBlob(dir, commit, rel)
	if !ok {
		return "", false, fmt.Errorf("memory at %s: %s is unreadable", short(commit), rel)
	}
	return b, true, nil
}

// pinnedSpec reads one feature's spec at a pinned memory commit, after checking the pin.
func pinnedSpec(dir, commit, feature string) (Spec, error) {
	snap, err := loadMemoryAt(dir, commit)
	if err != nil {
		return Spec{}, err
	}
	s, ok := snap.Spec(feature)
	if !ok {
		return Spec{}, fmt.Errorf("feature %q does not exist at pinned memory commit %s", feature, short(commit))
	}
	return s, nil
}
