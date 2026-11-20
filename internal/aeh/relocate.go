package aeh

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Relocation is an owner-declared, literal move. The approval digest binds both prefixes.
type Relocation struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// ParseRelocations parses the CLI declaration; validation also runs when reading approvals.
func ParseRelocations(value string) ([]Relocation, error) {
	var pairs []Relocation
	for _, item := range strings.Split(value, ",") {
		from, to, ok := strings.Cut(item, "=")
		if !ok || strings.Contains(to, "=") {
			return nil, fmt.Errorf("--relocate: expected <from>=<to>, got %q", item)
		}
		pairs = append(pairs, Relocation{From: from, To: to})
	}
	if err := validateRelocationPairs(pairs); err != nil {
		return nil, fmt.Errorf("--relocate: %w", err)
	}
	return pairs, nil
}

func validateRelocationPairs(pairs []Relocation) error {
	var prefixes []string
	for _, pair := range pairs {
		for _, prefix := range []string{pair.From, pair.To} {
			plain := strings.TrimSuffix(prefix, "/")
			if strings.ContainsAny(plain, "*?[]") {
				return fmt.Errorf("relocation prefix %q must be literal, without wildcards", prefix)
			}
			if err := checkPlanTestPath(plain); err != nil {
				return fmt.Errorf("relocation prefix %q: %w", prefix, err)
			}
			for i, segment := range strings.Split(plain, "/") {
				if strings.EqualFold(segment, ".git") || strings.EqualFold(segment, ".agents") ||
					(i == 0 && (strings.EqualFold(segment, "aeh") || strings.EqualFold(segment, ".aeh"))) ||
					agentInstructionFile(segment) || gitControlFile(segment) {
					return fmt.Errorf("relocation prefix %q contains a protected control path", prefix)
				}
			}
			for _, prior := range prefixes {
				if matchAny(plain, []string{prior}) || matchAny(prior, []string{plain}) {
					return fmt.Errorf("relocation prefixes %q and %q overlap; pairs must be distinct and non-nested", prior, prefix)
				}
			}
			prefixes = append(prefixes, plain)
		}
	}
	return nil
}

// validateRelocations uses the gate's effective path coverage, not a difference of patterns.
// Ownership includes the last implementation until the move is recorded.
func validateRelocations(memory MemorySnapshot, spec Spec, cfg Config, pairs []Relocation) error {
	if err := validateRelocationPairs(pairs); err != nil {
		return err
	}
	if len(pairs) == 0 {
		return nil
	}
	if _, ok := spec.LastImplementation(); !ok {
		return fmt.Errorf("relocate %s: the feature has no last implementation", spec.Feature)
	}
	for _, pair := range pairs {
		from, to := strings.TrimSuffix(pair.From, "/"), strings.TrimSuffix(pair.To, "/")
		for _, prefix := range []string{from, to} {
			protected := append([]string{cfg.plan().AcceptanceDir}, cfg.Protected...)
			if pathsOverlap([]string{prefix}, protected) {
				return fmt.Errorf("relocation prefix %q overlaps a protected path", prefix)
			}
		}
		if !matchAny(from, spec.OwnedPaths()) {
			return fmt.Errorf("relocation source %q is not owned by feature %s", pair.From, spec.Feature)
		}
		if pathsOverlap([]string{from}, spec.Paths) {
			return fmt.Errorf("relocation source %q is still covered by the approved paths", pair.From)
		}
		if !matchAny(to, spec.Paths) {
			return fmt.Errorf("relocation destination %q is not covered by the approved paths", pair.To)
		}
		if err := checkPathClaim(memory.Dir, memory, spec.Feature, []string{to}, []string{to}); err != nil {
			return fmt.Errorf("relocation destination %q: %w", pair.To, err)
		}
	}
	return nil
}

func relocationPreconditions(root, base string, pairs []Relocation) error {
	if len(pairs) == 0 {
		return nil
	}
	out, err := git(root, "ls-tree", "-r", "-z", "--name-only", base)
	if err != nil {
		return fmt.Errorf("relocation base files: %w", err)
	}
	for _, pair := range pairs {
		hasFrom := false
		for _, file := range strings.Split(out, "\x00") {
			if file == "" {
				continue
			}
			if matchAny(file, []string{pair.To}) {
				return fmt.Errorf("relocation destination %q already has tracked files on base %s", pair.To, short(base))
			}
			hasFrom = hasFrom || matchAny(file, []string{pair.From})
		}
		if !hasFrom {
			return fmt.Errorf("relocation source %q has no tracked files on base %s", pair.From, short(base))
		}
	}
	return nil
}

// containsRelocationFrom is a plain substring check on purpose. It decides which sealed tests
// retire and which new tests are refused, so a miss is the dangerous error: a sealed test that
// still loads the source (cat <src/x, cc -Isrc/) would stay sealed and break after the move. A
// false match is safe: the test retires only after passing on the base and gets replacement
// coverage, or a new test is refused visibly. Declaring directories with a trailing slash
// (src/) keeps false matches such as srcset rare.
func containsRelocationFrom(b []byte, pairs []Relocation) bool {
	for _, pair := range pairs {
		if bytes.Contains(b, []byte(pair.From)) {
			return true
		}
	}
	return false
}

// relocateCriteria adds unchanged coverage whose sealed base bytes name a source. Sharing a
// retired file also requires recovery. Relocated takes precedence over recovery in the list.
func relocateCriteria(root, base string, d *CriteriaDelta, pairs []Relocation) error {
	if len(pairs) == 0 {
		return nil
	}
	retiring := map[string]bool{}
	for _, c := range d.Unchanged {
		moves := false
		for _, file := range c.Tests {
			b, ok := gitBlob(root, base, file)
			if !ok {
				return fmt.Errorf("inspect relocated criterion AC-%d: sealed test %s is missing on the base", c.ID, file)
			}
			if containsRelocationFrom([]byte(b), pairs) {
				moves, retiring[file] = true, true
			}
		}
		if moves {
			d.Relocated = append(d.Relocated, c)
		}
	}
	recovery := map[int]bool{}
	for _, c := range d.Recovery {
		recovery[c.ID] = true
	}
	d.Recovery = nil
	for _, c := range d.Unchanged {
		moved := false
		for _, r := range d.Relocated {
			moved = moved || r.ID == c.ID
		}
		if moved {
			continue
		}
		for _, file := range c.Tests {
			recovery[c.ID] = recovery[c.ID] || retiring[file]
		}
		if recovery[c.ID] {
			d.Recovery = append(d.Recovery, c)
		}
	}
	return nil
}

// mapRelocationTest makes one left-to-right pass over the original bytes, longest destination
// first. Replacements are never scanned again. All source literals in the result come from
// this mapping, so tests that still reference a source fail the attempt before any command.
func mapRelocationTest(b []byte, pairs []Relocation) ([]byte, error) {
	if containsRelocationFrom(b, pairs) {
		return nil, fmt.Errorf("new test contains a relocation source literal; reference only the destination")
	}
	ordered := append([]Relocation(nil), pairs...)
	for i := range ordered {
		ordered[i].From = strings.TrimSuffix(ordered[i].From, "/")
		ordered[i].To = strings.TrimSuffix(ordered[i].To, "/")
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i].To) > len(ordered[j].To) })
	var mapped bytes.Buffer
	for i := 0; i < len(b); {
		matched := false
		for _, pair := range ordered {
			end := i + len(pair.To)
			if !bytes.HasPrefix(b[i:], []byte(pair.To)) || !relocationLeftBoundary(b, i) ||
				!relocationRightBoundary(b, end, pair.To) {
				continue
			}
			mapped.WriteString(pair.From)
			i, matched = end, true
			break
		}
		if !matched {
			mapped.WriteByte(b[i])
			i++
		}
	}
	return mapped.Bytes(), nil
}

func relocationLeftBoundary(b []byte, i int) bool {
	if i == 0 {
		return true
	}
	left, _ := utf8.DecodeLastRune(b[:i])
	return unicode.IsSpace(left) || strings.ContainsRune("\"'`(=/", left)
}

func relocationRightBoundary(b []byte, end int, to string) bool {
	if strings.HasSuffix(to, "/") || end == len(b) {
		return true
	}
	right, _ := utf8.DecodeRune(b[end:])
	return unicode.IsSpace(right) || strings.ContainsRune("/\"'`)]},;", right)
}

func planTestFiles(plan PlanFile, dir string) []string {
	seen := map[string]bool{}
	for _, files := range plan.Trace {
		for _, file := range files {
			seen[dir+file] = true
		}
	}
	var files []string
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

// mappedPlanCommit changes only the new tests in a private index of the frozen plan. It
// never edits the author's checkout or the bytes that are sealed, published and reviewed.
func (p *planner) mappedPlanCommit(commit string, plan PlanFile) (string, error) {
	if len(p.state.Relocations) == 0 {
		return commit, nil
	}
	tmp, err := os.MkdirTemp("", "aeh-relocate-index-")
	if err != nil {
		return "", fmt.Errorf("mapped plan index: %w", err)
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	root := p.repo.Root
	if _, err := gitEnv(root, env, nil, "read-tree", commit); err != nil {
		return "", fmt.Errorf("read mapped plan tree: %w", err)
	}
	for _, file := range planTestFiles(plan, p.testDir()) {
		b, ok := gitBlob(root, commit, file)
		if !ok {
			return "", fmt.Errorf("map test %s: missing from frozen plan", file)
		}
		mapped, err := mapRelocationTest([]byte(b), p.state.Relocations)
		if err != nil {
			return "", fmt.Errorf("map test %s: %w", file, err)
		}
		sha, err := gitEnv(root, nil, mapped, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return "", fmt.Errorf("map test %s: %w", file, err)
		}
		mode, err := git(root, "ls-tree", commit, "--", file)
		if err != nil {
			return "", fmt.Errorf("map test %s mode: %w", file, err)
		}
		if len(strings.Fields(mode)) < 1 {
			return "", fmt.Errorf("map test %s: missing file mode", file)
		}
		if _, err := gitEnv(root, env, nil, "update-index", "--cacheinfo", strings.Fields(mode)[0]+","+sha+","+file); err != nil {
			return "", fmt.Errorf("map test %s: %w", file, err)
		}
	}
	tree, err := gitEnv(root, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write mapped plan tree: %w", err)
	}
	mapped, err := git(root, "-c", "user.name=aeh", "-c", "user.email=aeh@localhost", "-c", "commit.gpgsign=false",
		"commit-tree", tree, "-p", commit, "-m", "aeh: mapped base checks for "+p.state.ID)
	if err != nil {
		return "", fmt.Errorf("commit mapped plan: %w", err)
	}
	// The state file names the mapped commit but does not keep it reachable; a ref does, so
	// a git gc between an interruption and the resume cannot remove it.
	if _, err := git(root, "update-ref", "refs/aeh/mapped/"+p.state.ID, mapped); err != nil {
		return "", fmt.Errorf("retain mapped plan: %w", err)
	}
	return mapped, nil
}

func (p *planner) relocationWarnings() {
	commands := []struct{ name, value string }{
		{"build", p.cfg.Commands.Build}, {"test", p.cfg.Commands.Test}, {"acceptance", p.cfg.Commands.Acceptance},
	}
	for _, pair := range p.state.Relocations {
		p.logf("  relocation: %s -> %s", pair.From, pair.To)
		for _, command := range commands {
			if strings.Contains(command.value, pair.From) {
				p.logf("warning: commands.%s mentions relocation source %q; aeh/config.yaml is owner-protected; update it if needed", command.name, pair.From)
			}
		}
	}
	for _, c := range p.state.Delta.Relocated {
		p.logf("  relocated AC-%d: %s", c.ID, c.Text)
	}
}
