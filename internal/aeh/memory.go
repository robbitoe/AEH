package aeh

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The product memory is a local wiki in <main checkout>/.aeh/memory/. It is its own Git
// repository with no remote: every spec version is a commit there. The project ignores
// .aeh/, so nothing in it is published by AEH. Only aeh commands write it; agents receive
// what they need inside the prompt.
//
//	.aeh/memory/
//	  schema.md                      conventions
//	  wiki/index.md                  generated: feature, version, last accepted implementation
//	  wiki/log.md                    append-only
//	  wiki/features/<slug>/spec.md   one spec per feature
//
// Without a sandbox this is a cooperation rule, not isolation: agents, builds and tests run
// with the user's permissions. The engine detects changes against a snapshot and refuses to
// record on top of them.

var (
	ErrMemoryDirty   = errors.New("memory has uncommitted changes")
	ErrMemoryChanged = errors.New("memory changed since the run started")
)

// memoryNow and memoryWriteFile are replaced in tests.
var (
	memoryNow       = time.Now
	memoryWriteFile = writeFileAtomic
)

// memoryGitIdentity signs memory commits. They never leave the machine.
// author.* and committer.* override user.*, so all of them are pinned.
var memoryGitIdentity = []string{
	"-c", "user.name=aeh", "-c", "user.email=aeh@localhost",
	"-c", "author.name=aeh", "-c", "author.email=aeh@localhost",
	"-c", "committer.name=aeh", "-c", "committer.email=aeh@localhost",
	"-c", "commit.gpgsign=false",
}

// Spec is one feature's document. Paths name the code the feature owns.
type Spec struct {
	Feature string
	Title   string
	Version string
	Paths   []string
	Body    string
	// Approval is the owner's last approval of this spec, or nil; see Approved.
	Approval *SpecApproval
}

// SpecApproval records that the owner approved one exact content of a spec. Transitional
// until stage 3, where approving a feature becomes part of the spec flow.
type SpecApproval struct {
	Version string `yaml:"version"`
	Content string `yaml:"content_sha256"`
	Date    string `yaml:"date"`
}

// Approved reports whether the owner approved the spec as it reads now. The approval covers
// the title, the paths and the whole body. It survives only what recording accepted changes
// adds: a version bump in the frontmatter and changelog lines appended at the end in the exact
// format aeh writes. Any other edit voids it. No Markdown is interpreted, so no construct can
// hide approved text from the digest.
func (s Spec) Approved() bool {
	if s.Approval == nil {
		return false
	}
	body := strings.TrimRight(s.Body, "\n")
	for {
		if approvalDigest(s.Title, s.Paths, body) == s.Approval.Content {
			return true
		}
		i := strings.LastIndex(body, "\n")
		if i < 0 || !acceptedLine.MatchString(body[i+1:]) {
			return false
		}
		body = strings.TrimRight(body[:i], "\n")
	}
}

// approvalDigest hashes what the owner approves: title, paths and body text.
func approvalDigest(title string, paths []string, body string) string {
	v := struct {
		Title string
		Paths []string
		Body  string
	}{title, paths, strings.TrimRight(body, "\n")}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lastHeading returns the last second-level heading outside fenced code and its offset.
func lastHeading(body string) (string, int) {
	last, at := "", -1
	var f codeFence
	offset := 0
	for _, l := range strings.SplitAfter(body, "\n") {
		if !f.step(l) && strings.HasPrefix(l, "## ") {
			last, at = strings.TrimSpace(l), offset
		}
		offset += len(l)
	}
	return last, at
}

// codeFence follows Markdown fenced code blocks: a run of three or more backticks or tildes,
// indented at most three spaces, opens a block that only a run of the same character at least
// as long closes. A shorter or different run inside the block is content.
type codeFence struct {
	char byte
	n    int
}

// step reports whether the line belongs to a fenced block (its fences included).
func (f *codeFence) step(line string) bool {
	t := strings.TrimRight(line, "\r\n")
	trimmed := strings.TrimLeft(t, " ")
	if len(t)-len(trimmed) > 3 || trimmed == "" {
		return f.n > 0
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return f.n > 0
	}
	n := len(trimmed) - len(strings.TrimLeft(trimmed, string(c)))
	if n < 3 {
		return f.n > 0
	}
	rest := trimmed[n:]
	if f.n == 0 {
		if c == '`' && strings.Contains(rest, "`") {
			return false // an inline code span, not a fence
		}
		f.char, f.n = c, n
		return true
	}
	if c == f.char && n >= f.n && strings.Trim(rest, " \t") == "" { // only spaces or tabs may follow a closing fence
		f.n = 0
	}
	return true
}

// MemorySnapshot is what a run reads from memory. Fingerprint covers everything the drift
// gate uses, so a run can detect that memory changed under it.
type MemorySnapshot struct {
	Present     bool
	Dir         string
	Specs       []Spec
	Fingerprint string
}

// Spec returns the spec of a feature.
func (m MemorySnapshot) Spec(feature string) (Spec, bool) {
	for _, s := range m.Specs {
		if s.Feature == feature {
			return s, true
		}
	}
	return Spec{}, false
}

// MemoryLink ties one outcome or `aeh do` change to the feature it modifies.
type MemoryLink struct {
	Feature string
	Change  string // patch | minor | major
	Note    string // changelog line
}

// Validate checks the link's form. An empty link is valid: the change declares no feature.
func (l MemoryLink) Validate() error {
	if l.Feature == "" {
		if l.Change != "" || l.Note != "" {
			return errors.New("a change level or note needs a feature")
		}
		return nil
	}
	if err := checkSlug(l.Feature); err != nil {
		return err
	}
	if _, err := bumpVersion("0.0.0", l.Change); err != nil {
		return err
	}
	if _, err := changelogNote(l.Note); err != nil {
		return err
	}
	return nil
}

// PendingRecord is an accepted change that still has to be written to memory.
type PendingRecord struct {
	OutcomeID string
	Feature   string
	Change    string
	Note      string
	Commit    string // verified head of the integration branch
}

// MemoryDir is the memory of the project's main checkout, also from linked worktrees.
func MemoryDir(r Repo) (string, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return "", err
	}
	return filepath.Join(main, ".aeh", "memory"), nil
}

// InitMemory creates the memory repository. It returns false when it already exists.
func InitMemory(r Repo) (bool, error) {
	unlock, err := lockProject(r)
	if err != nil {
		return false, err
	}
	defer unlock()
	main, err := MainCheckout(r)
	if err != nil {
		return false, err
	}
	if err := checkProjectPrivacy(main); err != nil {
		return false, err
	}
	dir := filepath.Join(main, ".aeh", "memory")
	if err := rejectLink(dir); err != nil {
		return false, fmt.Errorf("memory init: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		if err := rejectLinksInside(dir); err != nil {
			return false, err
		}
		if err := verifyMemoryRepo(dir); err != nil {
			return false, err
		}
		if _, err := git(dir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
			// Finish an init that stopped before its first commit, but only when nothing in
			// the directory could be someone else's work.
			if err := checkUnfinishedInit(dir); err != nil {
				return false, err
			}
			return true, seedMemory(dir)
		}
		return false, checkMemoryLayout(dir)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return false, fmt.Errorf("memory init: %s exists and is not a memory repository; move it away first", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("memory init: %w", err)
	}
	if _, err := git(dir, "init", "-q", "-b", "main"); err != nil {
		return false, fmt.Errorf("memory init: %w", err)
	}
	if _, err := git(dir, "config", "core.autocrlf", "false"); err != nil {
		return false, fmt.Errorf("memory init: %w", err)
	}
	if err := verifyMemoryRepo(dir); err != nil {
		return false, err
	}
	return true, seedMemory(dir)
}

// memoryLayout lists the files every initialized memory has.
var memoryLayout = []string{"schema.md", "wiki/features/.gitkeep", "wiki/index.md", "wiki/log.md"}

// seedMemory writes the initial files and makes the first commit.
func seedMemory(dir string) error {
	today := memoryNow().UTC().Format("2006-01-02")
	files := map[string]string{
		"schema.md":              memorySchema,
		"wiki/index.md":          renderIndex(nil),
		"wiki/log.md":            "# Log" + nl + nl + "Append-only. One entry per memory change." + nl + nl + "## [" + today + "] init | memory created" + nl,
		"wiki/features/.gitkeep": "",
	}
	for _, name := range memoryLayout {
		if err := writeFileAtomic(filepath.Join(dir, filepath.FromSlash(name)), []byte(files[name])); err != nil {
			return fmt.Errorf("memory init: %w", err)
		}
	}
	return commitMemory(dir, memoryLayout, "aeh: initialize memory")
}

const nl = "\n"

// checkUnfinishedInit accepts a repository only when HEAD is unborn, anything staged is one of
// aeh's init files exactly as on disk, and every file present is one aeh's init writes, with the
// content aeh writes. Anything else may be the owner's work, so init refuses instead of replacing it.
func checkUnfinishedInit(dir string) error {
	if _, err := git(dir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("memory init: %s has no readable HEAD; inspect it or move it away", dir)
	}
	expected := map[string]bool{}
	for _, name := range memoryLayout {
		expected[name] = true
	}
	// An init may stop after staging its seed files: those may stay staged, exactly as on disk.
	staged, err := git(dir, "ls-files", "-s")
	if err != nil {
		return fmt.Errorf("memory init: %w", err)
	}
	for _, line := range strings.Split(staged, "\n") {
		if line == "" {
			continue
		}
		meta, rel, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || !expected[rel] {
			return fmt.Errorf("memory init: %s has no commit and stages %s, which aeh did not write; move the directory away first", dir, rel)
		}
		disk, err := git(dir, "hash-object", "--", rel)
		if err != nil || disk != fields[1] {
			return fmt.Errorf("memory init: %s has no commit and its staged %s differs from the file; move the directory away first", dir, rel)
		}
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == filepath.Join(dir, ".git") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !expected[rel] || !isSeedContent(rel, b) {
			return fmt.Errorf("memory init: %s has no commit and contains %s, which aeh did not write; move the directory away first", dir, rel)
		}
		return nil
	})
}

// isSeedContent reports whether b is what seedMemory writes for name (the log's date may differ).
func isSeedContent(name string, b []byte) bool {
	switch name {
	case "schema.md":
		return string(b) == memorySchema
	case "wiki/index.md":
		return string(b) == renderIndex(nil)
	case "wiki/features/.gitkeep":
		return len(b) == 0
	case "wiki/log.md":
		head := "# Log" + nl + nl + "Append-only. One entry per memory change." + nl + nl + "## ["
		tail := "] init | memory created" + nl
		s := string(b)
		return strings.HasPrefix(s, head) && strings.HasSuffix(s, tail) && len(s) == len(head)+len("2006-01-02")+len(tail)
	}
	return false
}

// checkMemoryLayout reports files an initialized memory is missing from its last commit.
func checkMemoryLayout(dir string) error {
	var missing []string
	for _, name := range memoryLayout {
		if _, err := git(dir, "cat-file", "-e", "HEAD:"+name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("memory init: %s is incomplete (missing from its last commit: %s); restore them from its history or move the directory away", dir, strings.Join(missing, ", "))
	}
	return nil
}

// LoadMemory reads every spec. A missing memory is not an error; a corrupt one is.
func LoadMemory(r Repo) (MemorySnapshot, error) {
	dir, err := MemoryDir(r)
	if err != nil {
		return MemorySnapshot{}, err
	}
	return loadMemoryDir(dir)
}

func loadMemoryDir(dir string) (MemorySnapshot, error) {
	snap := MemorySnapshot{Dir: dir}
	if err := rejectLink(filepath.Dir(dir)); err != nil {
		return snap, fmt.Errorf("memory: %w", err)
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		snap.Fingerprint = fingerprint(snap)
		return snap, nil
	}
	if err := rejectLink(dir); err != nil {
		return snap, fmt.Errorf("memory: %w", err)
	}
	if err := rejectLinksInside(dir); err != nil {
		return snap, err
	}
	if err := verifyMemoryRepo(dir); err != nil {
		return snap, err
	}
	snap.Present = true
	featuresDir := filepath.Join(dir, "wiki", "features")
	entries, err := os.ReadDir(featuresDir)
	if err != nil {
		return snap, fmt.Errorf("memory: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			return snap, fmt.Errorf("memory: wiki/features/%s: only feature directories belong here", e.Name())
		}
		if err := checkSlug(e.Name()); err != nil {
			return snap, fmt.Errorf("memory: wiki/features/%s: %w", e.Name(), err)
		}
		b, err := os.ReadFile(filepath.Join(featuresDir, e.Name(), "spec.md"))
		if err != nil {
			return snap, fmt.Errorf("memory: feature %s: %w", e.Name(), err)
		}
		s, err := parseSpec(string(b))
		if err != nil {
			return snap, fmt.Errorf("memory: feature %s: %w", e.Name(), err)
		}
		if s.Feature != e.Name() {
			return snap, fmt.Errorf("memory: feature %s: spec declares feature %q", e.Name(), s.Feature)
		}
		if s.Approval, err = readApproval(filepath.Join(featuresDir, e.Name(), approvalFile)); err != nil {
			return snap, fmt.Errorf("memory: feature %s: %w", e.Name(), err)
		}
		snap.Specs = append(snap.Specs, s)
	}
	sort.Slice(snap.Specs, func(i, j int) bool { return snap.Specs[i].Feature < snap.Specs[j].Feature })
	snap.Fingerprint = fingerprint(snap)
	return snap, nil
}

// fingerprint hashes what the drift gate uses: presence, and each feature's slug, version and paths.
func fingerprint(m MemorySnapshot) string {
	type entry struct {
		Feature, Version string
		Paths            []string
	}
	v := struct {
		Present bool
		Specs   []entry
	}{Present: m.Present}
	for _, s := range m.Specs {
		v.Specs = append(v.Specs, entry{s.Feature, s.Version, s.Paths})
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ContractFingerprint is what a run's contract depends on: which code every feature owns, and
// the declared feature's title, version and text (everything the prompt shows of it). Other features' versions are left out, so recording
// another outcome does not stop a blocked outcome from resuming.
func (m MemorySnapshot) ContractFingerprint(declared string) string {
	type owner struct {
		Feature string
		Paths   []string
	}
	v := struct {
		Present          bool
		Owners           []owner
		Declared         string
		DeclaredTitle    string
		DeclaredVersion  string
		DeclaredBody     string
		DeclaredApproved bool
	}{Present: m.Present, Declared: declared}
	for _, s := range m.Specs {
		v.Owners = append(v.Owners, owner{s.Feature, s.Paths})
		if s.Feature == declared {
			v.DeclaredTitle, v.DeclaredVersion = s.Title, s.Version
			sum := sha256.Sum256([]byte(s.Body))
			v.DeclaredBody = hex.EncodeToString(sum[:])
			v.DeclaredApproved = s.Approved()
		}
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CreateSpec adds a feature at version 0.1.0 with an empty body for the owner to write.
func CreateSpec(r Repo, feature, title string, paths []string) (Spec, error) {
	unlock, err := lockProject(r)
	if err != nil {
		return Spec{}, err
	}
	defer unlock()
	snap, err := LoadMemory(r)
	if err != nil {
		return Spec{}, err
	}
	if !snap.Present {
		return Spec{}, errors.New("memory is not initialized; run: aeh memory init")
	}
	if err := checkSlug(feature); err != nil {
		return Spec{}, err
	}
	if _, ok := snap.Spec(feature); ok {
		return Spec{}, fmt.Errorf("feature %s already exists", feature)
	}
	if err := memoryClean(snap.Dir); err != nil {
		return Spec{}, err
	}
	front := specFront{Feature: feature, Title: strings.TrimSpace(title), Version: "0.1.0", Paths: paths}
	if err := front.validate(); err != nil {
		return Spec{}, err
	}
	fm, err := yaml.Marshal(front)
	if err != nil {
		return Spec{}, err
	}
	today := memoryNow().UTC().Format("2006-01-02")
	text := "---\n" + string(fm) + "---\n# " + front.Title + "\n\n" +
		specPlaceholder + "\n\n" +
		"## Changelog\n- 0.1.0 (" + today + ") created; not implemented yet.\n"
	spec, err := parseSpec(text)
	if err != nil {
		return Spec{}, err
	}
	w := memoryWrites{}
	err = w.write(snap.Dir, specRel(feature), []byte(text))
	if err == nil {
		err = writeIndexAndLog(snap.Dir, w, "["+today+"] create | "+feature+" 0.1.0")
	}
	if err == nil {
		err = w.commit(snap.Dir, "create "+feature+" 0.1.0")
	}
	if err != nil {
		return Spec{}, w.restore(snap.Dir, err)
	}
	return spec, nil
}

const approvalFile = "approval.yaml"

func approvalRel(feature string) string { return "wiki/features/" + feature + "/" + approvalFile }

func readApproval(p string) (*SpecApproval, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a SpecApproval
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%s: %w", approvalFile, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: must hold exactly one YAML document", approvalFile)
	}
	if _, err := parseSemver(a.Version); err != nil {
		return nil, fmt.Errorf("%s: %w", approvalFile, err)
	}
	if len(a.Content) != 64 || strings.Trim(a.Content, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("%s: content_sha256 must be a sha256 in hex", approvalFile)
	}
	return &a, nil
}

// ApproveSpec records the owner's approval of a feature's spec as it reads now, in one memory
// commit. Approving an unchanged, already approved spec does nothing and returns false.
func ApproveSpec(r Repo, feature string) (Spec, bool, error) {
	unlock, err := lockProject(r)
	if err != nil {
		return Spec{}, false, err
	}
	defer unlock()
	snap, err := LoadMemory(r)
	if err != nil {
		return Spec{}, false, err
	}
	if !snap.Present {
		return Spec{}, false, errors.New("memory is not initialized; run: aeh memory init")
	}
	spec, ok := snap.Spec(feature)
	if !ok {
		return Spec{}, false, fmt.Errorf("feature %q does not exist in memory", feature)
	}
	if err := memoryClean(snap.Dir); err != nil {
		return Spec{}, false, fmt.Errorf("approve %s: %w; commit your spec edits in the memory repository first", feature, err)
	}
	if spec.Approved() {
		return spec, false, nil
	}
	if strings.Contains(spec.Body, specPlaceholder) {
		return Spec{}, false, fmt.Errorf("approve %s: the spec still has the placeholder text; describe the feature first", feature)
	}
	today := memoryNow().UTC().Format("2006-01-02")
	a := SpecApproval{Version: spec.Version, Content: approvalDigest(spec.Title, spec.Paths, spec.Body), Date: today}
	b, err := yaml.Marshal(a)
	if err != nil {
		return Spec{}, false, err
	}
	w := memoryWrites{}
	err = w.write(snap.Dir, approvalRel(feature), b)
	if err == nil {
		err = appendLog(snap.Dir, w, "["+today+"] approve | "+feature+" "+spec.Version) // the index does not show approvals
	}
	if err == nil {
		err = w.commit(snap.Dir, "approve "+feature+" "+spec.Version)
	}
	if err != nil {
		return Spec{}, false, w.restore(snap.Dir, err)
	}
	spec.Approval = &a
	return spec, true, nil
}

// RecordChange writes an accepted change into memory: it bumps the feature's version, adds a
// changelog line with the verified commit, regenerates the index and appends to the log, in one
// memory commit. It is idempotent by (outcome, commit): a repeated call returns the first result.
// An empty expectedFingerprint skips the snapshot check (the owner retrying by hand).
// The caller must hold the project lock.
func RecordChange(r Repo, p PendingRecord, expectedFingerprint string) (version, memoryCommit string, err error) {
	if !validID(p.OutcomeID) {
		return "", "", fmt.Errorf("record: invalid outcome ID %q", p.OutcomeID)
	}
	if !isFullSHA(p.Commit) {
		return "", "", fmt.Errorf("record %s: commit %q is not a full SHA", p.OutcomeID, p.Commit)
	}
	note, err := changelogNote(p.Note)
	if err != nil {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, err)
	}
	dir, err := MemoryDir(r)
	if err != nil {
		return "", "", err
	}
	snap, err := loadMemoryDir(dir)
	if err != nil {
		return "", "", err
	}
	if !snap.Present {
		return "", "", fmt.Errorf("record %s: memory is not initialized", p.OutcomeID)
	}
	if err := memoryClean(dir); err != nil {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, err)
	}
	marker := "AEH-Record: " + p.OutcomeID + " " + p.Commit
	if v, c, ok, err := findRecord(dir, marker); err != nil {
		return "", "", err
	} else if ok {
		return v, c, nil
	}
	if expectedFingerprint != "" && snap.Fingerprint != expectedFingerprint {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, ErrMemoryChanged)
	}
	spec, ok := snap.Spec(p.Feature)
	if !ok {
		return "", "", fmt.Errorf("record %s: feature %q does not exist in memory", p.OutcomeID, p.Feature)
	}
	version, err = bumpVersion(spec.Version, p.Change)
	if err != nil {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, err)
	}
	rel := specRel(p.Feature)
	full := filepath.Join(dir, filepath.FromSlash(rel))
	raw, err := os.ReadFile(full)
	if err != nil {
		return "", "", err
	}
	today := memoryNow().UTC().Format("2006-01-02")
	short := p.Commit[:7]
	line := fmt.Sprintf("- %s (%s) %s accepted on %s @ %s: %s\n", version, today, p.OutcomeID, IntegrationBranch(p.OutcomeID), short, note)
	text, err := setSpecVersion(string(raw), version)
	if err != nil {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, err)
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += line
	if _, err := parseSpec(text); err != nil {
		return "", "", fmt.Errorf("record %s: updated spec is invalid: %w", p.OutcomeID, err)
	}
	w := memoryWrites{}
	err = w.write(dir, rel, []byte(text))
	if err == nil {
		err = writeIndexAndLog(dir, w, fmt.Sprintf("[%s] record | %s %s | %s @ %s", today, p.Feature, version, p.OutcomeID, short))
	}
	if err == nil {
		err = w.commit(dir, fmt.Sprintf("record %s %s (%s)", p.Feature, version, p.OutcomeID), marker, "AEH-Version: "+version)
	}
	if err != nil {
		return "", "", fmt.Errorf("record %s: %w", p.OutcomeID, w.restore(dir, err))
	}
	memoryCommit, err = git(dir, "rev-parse", "HEAD")
	return version, memoryCommit, err
}

// findRecord looks for an earlier memory commit with the same record marker.
func findRecord(dir, marker string) (version, commit string, ok bool, err error) {
	out, err := git(dir, "log", "--fixed-strings", "--grep="+marker, "--format=%H%x00%B%x01")
	if err != nil {
		return "", "", false, fmt.Errorf("memory log: %w", err)
	}
	for _, rec := range strings.Split(out, "\x01") {
		sha, body, found := strings.Cut(strings.TrimSpace(rec), "\x00")
		if !found {
			continue
		}
		var v string
		match := false
		for _, l := range strings.Split(body, "\n") {
			l = strings.TrimSpace(l)
			if l == marker {
				match = true
			}
			if after, ok := strings.CutPrefix(l, "AEH-Version: "); ok {
				v = after
			}
		}
		if match {
			return v, sha, true, nil
		}
	}
	return "", "", false, nil
}

// Pending records live next to the outcome state, so `aeh memory record <ID>` can retry them.
// They are kept out of the state directory, which holds only outcome states.
func pendingDir(r Repo) string             { return filepath.Join(r.Root, ".aeh", "pending") }
func pendingPath(r Repo, id string) string { return filepath.Join(pendingDir(r), id+".json") }

// SavePending stores a record that still has to reach memory.
func SavePending(r Repo, p PendingRecord) error {
	if !validID(p.OutcomeID) {
		return fmt.Errorf("pending record: invalid outcome ID %q", p.OutcomeID)
	}
	return writePrivateJSON(pendingPath(r, p.OutcomeID), p)
}

// LoadPending returns the pending record of an outcome, if any.
func LoadPending(r Repo, id string) (PendingRecord, bool, error) {
	if !validID(id) {
		return PendingRecord{}, false, fmt.Errorf("invalid outcome ID %q", id)
	}
	b, err := os.ReadFile(pendingPath(r, id))
	if errors.Is(err, os.ErrNotExist) {
		return PendingRecord{}, false, nil
	}
	if err != nil {
		return PendingRecord{}, false, err
	}
	var p PendingRecord
	if err := json.Unmarshal(b, &p); err != nil {
		return p, false, fmt.Errorf("pending record %s: %w", id, err)
	}
	if p.OutcomeID != id {
		return p, false, fmt.Errorf("pending record for %q has ID %q", id, p.OutcomeID)
	}
	return p, true, nil
}

// ClearPending removes the pending record of an outcome.
func ClearPending(r Repo, id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid outcome ID %q", id)
	}
	err := os.Remove(pendingPath(r, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListPending returns every pending record of this checkout.
func ListPending(r Repo) ([]PendingRecord, error) {
	matches, err := filepath.Glob(filepath.Join(pendingDir(r), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []PendingRecord
	for _, m := range matches {
		id := strings.TrimSuffix(filepath.Base(m), ".json")
		p, ok, err := LoadPending(r, id)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// RecordPending retries the pending record of an outcome by hand. The owner asked for it, so the
// snapshot check is skipped; the record stays idempotent.
func RecordPending(r Repo, id string) (version, memoryCommit string, err error) {
	unlock, err := lockProject(r)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	p, ok, err := LoadPending(r, id)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("no pending memory record for %s", id)
	}
	version, memoryCommit, err = RecordChange(r, p, "")
	if err != nil {
		return "", "", err
	}
	return version, memoryCommit, ClearPending(r, id)
}

// MemoryStatus summarizes memory for `aeh memory status`.
type MemoryStatus struct {
	Dir     string
	Present bool
	Dirty   string // git status of the memory repository; empty when clean
	Specs   []Spec
	Pending []PendingRecord
}

func ReadMemoryStatus(r Repo) (MemoryStatus, error) {
	snap, err := LoadMemory(r)
	st := MemoryStatus{Dir: snap.Dir, Present: snap.Present, Specs: snap.Specs}
	if err != nil {
		return st, err
	}
	if snap.Present {
		st.Dirty, err = git(snap.Dir, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return st, err
		}
	}
	st.Pending, err = ListPending(r)
	return st, err
}

// LastAccepted returns the newest "accepted on" changelog entry of a spec, or "".
func LastAccepted(s Spec) string {
	last := ""
	for _, l := range strings.Split(s.Body, "\n") {
		if m := acceptedLine.FindStringSubmatch(l); m != nil {
			last = fmt.Sprintf("%s @ %s (%s)", m[3], m[5], m[2])
		}
	}
	return last
}

var acceptedLine = regexp.MustCompile(`^- (\S+) \((\d{4}-\d{2}-\d{2})\) (\S+) accepted on (\S+) @ ([0-9a-f]+):`)

// LintMemory checks memory and its privacy settings. Errors make memory unusable; warnings do not.
func LintMemory(r Repo) (errs, warnings []string, err error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, nil, err
	}
	if perr := checkProjectPrivacy(main); perr != nil {
		errs = append(errs, perr.Error())
	}
	snap, lerr := LoadMemory(r)
	if lerr != nil {
		return append(errs, lerr.Error()), warnings, nil
	}
	if !snap.Present {
		return append(errs, "memory is not initialized; run: aeh memory init"), warnings, nil
	}
	if dirty, _ := git(snap.Dir, "status", "--porcelain", "--untracked-files=all"); dirty != "" {
		warnings = append(warnings, "memory has uncommitted changes; aeh will not record until they are committed:\n"+dirty)
	}
	for i, a := range snap.Specs {
		for _, b := range snap.Specs[i+1:] {
			if pathsOverlap(a.Paths, b.Paths) {
				warnings = append(warnings, fmt.Sprintf("features %s and %s claim overlapping paths; a change there fails the drift gate", a.Feature, b.Feature))
			}
		}
	}
	for _, s := range snap.Specs {
		if s.Approval != nil && !s.Approved() {
			warnings = append(warnings, fmt.Sprintf("feature %s was edited after its approval of %s; approve it again with: aeh memory approve %s", s.Feature, s.Approval.Version, s.Feature))
		}
	}
	if b, err := os.ReadFile(filepath.Join(snap.Dir, "wiki", "index.md")); err != nil || string(b) != renderIndex(snap.Specs) {
		warnings = append(warnings, "wiki/index.md is out of date; aeh regenerates it on the next write")
	}
	return errs, warnings, nil
}

func pathsOverlap(a, b []string) bool {
	base := func(p string) string {
		p = strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
		if i := strings.IndexAny(p, "*?["); i >= 0 {
			p = p[:i]
		}
		return strings.TrimSuffix(p, "/")
	}
	for _, pa := range a {
		for _, pb := range b {
			ba, bb := base(pa), base(pb)
			if ba == "" || bb == "" || matchAny(ba, []string{pb}) || matchAny(bb, []string{pa}) {
				return true
			}
		}
	}
	return false
}

// --- spec format ---

type specFront struct {
	Feature string   `yaml:"feature"`
	Title   string   `yaml:"title"`
	Version string   `yaml:"version"`
	Paths   []string `yaml:"paths,flow"`
}

func (f specFront) validate() error {
	if err := checkSlug(f.Feature); err != nil {
		return err
	}
	if f.Title == "" || strings.ContainsAny(f.Title, "\r\n") || len(f.Title) > 120 {
		return errors.New("title must be one line of 1 to 120 characters")
	}
	if _, err := parseSemver(f.Version); err != nil {
		return err
	}
	if len(f.Paths) == 0 {
		return errors.New("paths must name at least one path the feature owns")
	}
	for _, p := range f.Paths {
		if err := checkSpecPath(p); err != nil {
			return err
		}
	}
	return nil
}

func parseSpec(text string) (Spec, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Spec{}, errors.New("spec must start with a --- frontmatter block")
	}
	fm, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Spec{}, errors.New("spec frontmatter is not closed with ---")
	}
	var f specFront
	dec := yaml.NewDecoder(strings.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return Spec{}, fmt.Errorf("spec frontmatter: %w", err)
	}
	if err := f.validate(); err != nil {
		return Spec{}, err
	}
	if err := checkChangelog(body); err != nil {
		return Spec{}, err
	}
	return Spec{Feature: f.Feature, Title: f.Title, Version: f.Version, Paths: f.Paths, Body: body}, nil
}

// checkChangelog requires "## Changelog" as the last second-level heading, so records append at the end.
func checkChangelog(body string) error {
	if last, _ := lastHeading(body); last != "## Changelog" {
		return errors.New(`spec must end with a "## Changelog" section`)
	}
	return nil
}

// setSpecVersion replaces the version line inside the frontmatter and nothing else.
func setSpecVersion(text, version string) (string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", errors.New("spec has no frontmatter")
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			break
		}
		if strings.HasPrefix(lines[i], "version:") {
			lines[i] = "version: " + version
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", errors.New("spec frontmatter has no version line")
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// checkSlug accepts names that are safe directory names on every platform.
func checkSlug(s string) error {
	if len(s) > 64 || !slugPattern.MatchString(s) {
		return fmt.Errorf("feature %q must be lowercase letters, digits and single hyphens (at most 64)", s)
	}
	switch s {
	case "con", "prn", "aux", "nul":
		return fmt.Errorf("feature %q is a reserved name on Windows", s)
	}
	if len(s) == 4 && (strings.HasPrefix(s, "com") || strings.HasPrefix(s, "lpt")) && s[3] >= '0' && s[3] <= '9' {
		return fmt.Errorf("feature %q is a reserved name on Windows", s)
	}
	return nil
}

// checkSpecPath accepts repository-relative patterns outside AEH's own directories.
func checkSpecPath(p string) error {
	q := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "./")
	switch {
	case q == "" || q == "**" || q == "*":
		return fmt.Errorf("path %q would claim the whole repository", p)
	case strings.HasPrefix(q, "/") || strings.Contains(q, ":"):
		return fmt.Errorf("path %q must be relative to the repository", p)
	case q == ".." || strings.HasPrefix(q, "../") || strings.Contains(q, "/../"):
		return fmt.Errorf("path %q leaves the repository", p)
	}
	low := strings.ToLower(q)
	for _, own := range []string{".git", ".aeh", "aeh"} {
		if low == own || strings.HasPrefix(low, own+"/") {
			return fmt.Errorf("path %q is inside %s/, which no feature owns", p, own)
		}
	}
	if _, err := path.Match(q, ""); err != nil {
		return fmt.Errorf("path %q: %w", p, err)
	}
	return nil
}

func parseSemver(v string) ([3]int, error) {
	var n [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		if p == "" || len(p) > 1 && p[0] == '0' || strings.Trim(p, "0123456789") != "" {
			return n, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", v)
		}
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, fmt.Errorf("version %q must be MAJOR.MINOR.PATCH", v)
		}
		n[i] = x
	}
	return n, nil
}

func bumpVersion(v, change string) (string, error) {
	n, err := parseSemver(v)
	if err != nil {
		return "", err
	}
	switch change {
	case "patch":
		n[2]++
	case "minor":
		n[1], n[2] = n[1]+1, 0
	case "major":
		n[0], n[1], n[2] = n[0]+1, 0, 0
	default:
		return "", fmt.Errorf("change %q must be patch, minor or major", change)
	}
	return fmt.Sprintf("%d.%d.%d", n[0], n[1], n[2]), nil
}

func changelogNote(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", errors.New("a changelog note is required")
	}
	if len(s) > 200 {
		return "", errors.New("the changelog note must be at most 200 characters")
	}
	return s, nil
}

func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

const specPlaceholder = "Describe the behavior, acceptance criteria, data and risks of this feature."

func specRel(feature string) string { return "wiki/features/" + feature + "/spec.md" }

// --- files and git ---

func renderIndex(specs []Spec) string {
	var b strings.Builder
	b.WriteString("# Index\n\nGenerated by aeh. Do not edit by hand.\n\n")
	if len(specs) == 0 {
		b.WriteString("No features yet. Create one with: aeh memory new <feature> --title \"...\" --paths src/feature/**\n")
		return b.String()
	}
	b.WriteString("| Feature | Title | Version | Last accepted |\n| --- | --- | --- | --- |\n")
	for _, s := range specs {
		last := LastAccepted(s)
		if last == "" {
			last = "not implemented yet"
		}
		fmt.Fprintf(&b, "| [%s](features/%s/spec.md) | %s | %s | %s |\n", s.Feature, s.Feature,
			strings.ReplaceAll(s.Title, "|", `\|`), s.Version, last)
	}
	return b.String()
}

func writeIndexAndLog(dir string, w memoryWrites, logEntry string) error {
	snap, err := loadMemoryDir(dir)
	if err != nil {
		return err
	}
	if err := w.write(dir, "wiki/index.md", []byte(renderIndex(snap.Specs))); err != nil {
		return err
	}
	return appendLog(dir, w, logEntry)
}

// appendLog adds one entry to wiki/log.md.
func appendLog(dir string, w memoryWrites, logEntry string) error {
	logPath := filepath.Join(dir, "wiki", "log.md")
	b, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	b = append(b, []byte("\n## "+logEntry+"\n")...)
	return w.write(dir, "wiki/log.md", b)
}

// commit stages exactly the files this command wrote, after checking nobody changed them.
func (w memoryWrites) commit(dir, subject string, trailers ...string) error {
	files := make([]string, 0, len(w))
	for rel, wrote := range w {
		if now, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err != nil || !bytes.Equal(now, wrote) {
			return fmt.Errorf("memory commit: %s changed while aeh was writing it", rel)
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return commitMemory(dir, files, subject, trailers...)
}

func commitMemory(dir string, files []string, subject string, trailers ...string) error {
	if _, err := git(dir, append([]string{"add", "--"}, files...)...); err != nil {
		return fmt.Errorf("memory commit: %w", err)
	}
	// With paths, commit records only those paths: anything else staged in the shared
	// index (for example by the owner) stays staged and out of this commit.
	args := append(append([]string{}, memoryGitIdentity...), "commit", "-q", "--no-verify", "-m", subject)
	if len(trailers) > 0 {
		args = append(args, "-m", strings.Join(trailers, "\n"))
	}
	args = append(append(args, "--"), files...)
	if _, err := git(dir, args...); err != nil {
		return fmt.Errorf("memory commit: %w", err)
	}
	if files[0] == "." {
		return nil
	}
	changed, err := git(dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "HEAD")
	if err != nil {
		return fmt.Errorf("memory commit: %w", err)
	}
	got := strings.Split(changed, "\n")
	sort.Strings(got)
	want := append([]string{}, files...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("memory commit: HEAD changed %v, expected exactly %v; check the memory repository", got, want)
	}
	return nil
}

// memoryWrites remembers what this command wrote, so a rollback touches only its own work.
type memoryWrites map[string][]byte

// write replaces a file only while it still matches HEAD (memory was clean when the command
// started), so an edit saved in the meantime is never overwritten.
func (w memoryWrites) write(dir, rel string, b []byte) error {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if _, mine := w[rel]; !mine {
		if err := unchangedSinceHead(dir, rel); err != nil {
			return err
		}
	}
	if err := memoryWriteFile(full, b); err != nil {
		if _, statErr := os.Lstat(full); errors.Is(statErr, os.ErrNotExist) {
			_ = os.Remove(filepath.Dir(full)) // only succeeds when empty, e.g. a new feature directory
		}
		return err
	}
	w[rel] = b
	return nil
}

func unchangedSinceHead(dir, rel string) error {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	headBlob, inHead := git(dir, "rev-parse", "--verify", "--quiet", "HEAD:"+rel)
	_, statErr := os.Lstat(full)
	switch {
	case inHead != nil && errors.Is(statErr, os.ErrNotExist):
		return nil
	case inHead != nil:
		return fmt.Errorf("memory write: %s appeared while aeh was writing; nothing was overwritten", rel)
	case statErr != nil:
		return fmt.Errorf("memory write: %s: %w", rel, statErr)
	}
	disk, err := git(dir, "hash-object", "--", rel)
	if err != nil {
		return fmt.Errorf("memory write: %w", err)
	}
	if disk != headBlob {
		return fmt.Errorf("memory write: %s changed while aeh was writing; nothing was overwritten", rel)
	}
	return nil
}

// restore undoes this command's uncommitted writes. A file whose content is no longer what
// this command wrote was edited by someone else meanwhile: it is kept and reported.
func (w memoryWrites) restore(dir string, cause error) error {
	var kept []string
	for rel, wrote := range w {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if now, err := os.ReadFile(full); err != nil || !bytes.Equal(now, wrote) {
			kept = append(kept, rel)
			continue
		}
		_, _ = git(dir, "reset", "-q", "HEAD", "--", rel)
		if _, err := git(dir, "cat-file", "-e", "HEAD:"+rel); err == nil {
			_, _ = git(dir, "checkout", "-q", "HEAD", "--", rel)
		} else {
			_ = os.Remove(full)
			_ = os.Remove(filepath.Dir(full)) // only succeeds when empty, e.g. a new feature directory
		}
	}
	if len(kept) > 0 {
		sort.Strings(kept)
		return fmt.Errorf("%w; these files changed during the write and were left as they are, check them by hand: %s", cause, strings.Join(kept, ", "))
	}
	return cause
}

func memoryClean(dir string) error {
	// assume-unchanged (lowercase tag) and skip-worktree (S) hide edits from status.
	flags, err := git(dir, "ls-files", "-v")
	if err != nil {
		return fmt.Errorf("memory status: %w", err)
	}
	for _, line := range strings.Split(flags, "\n") {
		if line != "" && (line[0] == 'S' || line[0] >= 'a' && line[0] <= 'z') {
			return fmt.Errorf("%w: an index flag hides changes to %s; clear it with git update-index --no-assume-unchanged --no-skip-worktree", ErrMemoryDirty, line[2:])
		}
	}
	// Refreshing the index exits nonzero when a tracked file changed; status reports that below.
	_, _ = git(dir, "update-index", "-q", "--really-refresh")
	out, err := git(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("memory status: %w", err)
	}
	if out != "" {
		return fmt.Errorf("%w (commit or discard them in %s):\n%s", ErrMemoryDirty, dir, out)
	}
	return nil
}

// verifyMemoryRepo checks that dir is its own repository and has no remote.
func verifyMemoryRepo(dir string) error {
	// Checked before Git runs: a .git file or .git/commondir would point Git at another repository.
	gitPath := filepath.Join(dir, ".git")
	if fi, err := os.Lstat(gitPath); err != nil || !fi.IsDir() {
		return fmt.Errorf("memory: %s must be a plain .git directory", gitPath)
	}
	if _, err := os.Lstat(filepath.Join(gitPath, "commondir")); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: %s has a commondir file; memory must not share another repository's Git data", gitPath)
	}
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if !samePath(common, gitPath) {
		return fmt.Errorf("memory: Git data resolves to %s, not %s", common, gitPath)
	}
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if !samePath(top, dir) {
		return fmt.Errorf("memory: %s is not its own Git repository (found %s); run: aeh memory init", dir, top)
	}
	gitDir, err := git(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if !samePath(gitDir, filepath.Join(dir, ".git")) {
		return fmt.Errorf("memory: %s must keep its Git data in its own .git directory", dir)
	}
	remotes, err := git(dir, "remote")
	if err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if remotes != "" {
		return fmt.Errorf("memory: repository has remotes (%s); memory is local by default, remove them or keep a private backup outside aeh",
			strings.ReplaceAll(remotes, "\n", ", "))
	}
	return nil
}

// checkProjectPrivacy requires .aeh/ to be ignored and untracked in the project.
func checkProjectPrivacy(main string) error {
	// icase: on Windows and macOS, .AEH/ names the same files as .aeh/.
	tracked, err := git(main, "ls-files", "--", ":(icase).aeh")
	if err != nil {
		return fmt.Errorf("memory privacy: %w", err)
	}
	if tracked != "" {
		return fmt.Errorf("memory privacy: the project tracks files under .aeh/; untrack them (git rm -r --cached .aeh) before using memory")
	}
	// The directory itself must be ignored: then Git never descends into it and no rule
	// inside can re-include a file. A probe below it guards against odd patterns.
	for _, probe := range []string{".aeh/", ".aeh/memory/wiki/features/probe/spec.md", ".aeh/pending/probe.json"} {
		if _, err := git(main, "check-ignore", "-q", "--no-index", probe); err != nil {
			return errors.New("memory privacy: the .aeh/ directory is not ignored by the project; add \".aeh/\" to .gitignore (aeh init does)")
		}
	}
	return nil
}

// rejectLinksInside walks memory, its .git included, and fails on any link or reparse point,
// so no Git command or write can be redirected outside the memory directory.
func rejectLinksInside(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("memory: %s is a link or reparse point; memory must contain only plain files", p)
		}
		return nil
	})
}

func writeFileAtomic(p string, b []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".aeh-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

const memorySchema = `# Memory schema

This is the product memory of the project: a local wiki with its own Git repository and no
remote. aeh created it in .aeh/memory/ of the main checkout. The project ignores .aeh/, so
aeh never publishes it. A private backup is the owner's choice.

## Layout

- wiki/index.md: generated by aeh; one row per feature with its version and last accepted change.
- wiki/log.md: append-only; one "## [date] operation | subject" entry per change.
- wiki/features/<feature>/spec.md: one spec per feature.
- wiki/features/<feature>/approval.yaml: written by "aeh memory approve <feature>"; the owner
  approved the spec's title, paths and text as they read then. Editing any of them voids the
  approval; recording an accepted change (version bump, changelog line appended by aeh) does not.

## Spec format

    ---
    feature: checkout              # the directory name: a-z, 0-9 and hyphens
    title: Checkout
    version: 1.3.1                 # MAJOR.MINOR.PATCH
    paths: [src/checkout/**]       # code this feature owns
    ---
    # Checkout
    Behavior, acceptance criteria, data and risks.

    ## Changelog
    - 1.3.1 (2026-11-13) OUT-007 accepted on aeh/OUT-007 @ a1b2c3d: Pay button reads "Confirm payment".

"## Changelog" must be the last section. A changelog entry records the verified branch and
commit; it does not mean the change was merged.

## Rules

- aeh writes version bumps, changelog entries, index.md and log.md after a change is accepted.
- The owner may edit spec text and paths by hand, then commit here (git commit -am "...").
  aeh refuses to record on top of uncommitted changes.
- Agents never write here; they receive the spec of the declared feature inside their prompt.
`
