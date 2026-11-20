package aeh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Discovery (stage 4, entry A): an idea becomes a draft book of specs through rounds of an
// agent's questions and the owner's answers. The agent writes only into an output directory
// aeh opened before it ran; aeh freezes that output, validates it, and imports it into memory
// under wiki/discovery/<slug>/ with one marked commit per step. Drafts are not features:
// they claim no paths and no run sees them until the owner promotes one.

// Discovery phases.
const (
	DiscoveryAwaiting = "awaiting_answers" // blocking questions of the last round are open
	DiscoveryReady    = "ready_to_run"     // another round can run
	DiscoveryDraft    = "draft_ready"      // a validated draft book is in memory; not an approval
	DiscoveryReview   = "pending_review"   // frozen book awaiting advisory review and import
)

// Agent verdicts: the last non-empty line of the agent's output.
const (
	discoveryQuestions = "DISCOVERY: QUESTIONS"
	discoveryBook      = "DISCOVERY: READY"
)

const (
	discoveryStateVersion = 1
	discoveryDocBytes     = 65536
	discoveryAnswerBytes  = 8192
	discoveryLineBytes    = 500
	discoveryOptionBytes  = 200
	discoveryMaxOptions   = 6
	discoveryImageBytes   = 65536
)

// DiscoveryConfig is the discovery section of aeh/config.yaml.
type DiscoveryConfig struct {
	Ladder         []string `yaml:"ladder"`
	MaxQuestions   int      `yaml:"max_questions"`
	MaxFeatures    int      `yaml:"max_features"`
	MaxMockupBytes int      `yaml:"max_mockup_bytes"`
	Research       bool     `yaml:"research"`
}

// UnmarshalYAML rejects misspelled discovery settings.
func (d *DiscoveryConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("discovery must be a mapping of ladder, limits and research")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "ladder", "max_questions", "max_features", "max_mockup_bytes", "research":
		default:
			return fmt.Errorf("discovery: unknown key %q", k)
		}
	}
	type plain DiscoveryConfig
	return n.Decode((*plain)(d))
}

// discovery applies defaults. Zero limits mean omitted; negative ones stay invalid.
func (c Config) discovery() DiscoveryConfig {
	d := c.Discovery
	if len(d.Ladder) == 0 {
		d.Ladder = c.Ladder
	}
	if d.MaxQuestions == 0 {
		d.MaxQuestions = 7
	}
	if d.MaxFeatures == 0 {
		d.MaxFeatures = 12
	}
	if d.MaxMockupBytes == 0 {
		d.MaxMockupBytes = 204800
	}
	return d
}

func (c Config) validateDiscoveryConfig() error {
	d := c.discovery()
	switch {
	case d.MaxQuestions < 1 || d.MaxQuestions > 20:
		return errors.New("config: discovery.max_questions must be from 1 through 20")
	case d.MaxFeatures < 1 || d.MaxFeatures > 50:
		return errors.New("config: discovery.max_features must be from 1 through 50")
	case d.MaxMockupBytes < 1024 || d.MaxMockupBytes > 2<<20:
		return errors.New("config: discovery.max_mockup_bytes must be from 1024 through 2097152")
	}
	seen := map[string]bool{}
	for _, name := range d.Ladder {
		if seen[name] {
			return fmt.Errorf("config: profile %q appears twice in the discovery ladder", name)
		}
		seen[name] = true
		if err := c.checkProfile(name); err != nil {
			return fmt.Errorf("config: discovery ladder: %w", err)
		}
	}
	return nil
}

// DiscoveryState is the local record of one discovery session, in the main checkout's
// .aeh/discovery/<slug>/state.json. Memory holds the readable record; this file holds what
// aeh needs to resume.
type DiscoveryState struct {
	Version     int                `json:"version"`
	Slug        string             `json:"slug"`
	Idea        string             `json:"idea"`
	Phase       string             `json:"phase"`
	Status      string             `json:"status,omitempty"` // running or blocked while a run needs attention
	Question    string             `json:"question,omitempty"`
	Rounds      []DiscoveryRound   `json:"rounds,omitempty"`
	Attempts    []DiscoveryAttempt `json:"attempts,omitempty"`
	LadderFrom  int                `json:"ladder_from"`
	Pending     *pendingImport     `json:"pending,omitempty"` // a memory import that must finish first
	Book        *pendingBook       `json:"pending_book,omitempty"`
	Promotions  []DiscoveryAction  `json:"promotions,omitempty"`
	Acceptances []DiscoveryAction  `json:"acceptances,omitempty"`
	Discards    []DiscoveryAction  `json:"discards,omitempty"` // pending revisions the owner discarded
	UpdatedAt   time.Time          `json:"updated_at"`
}

// DiscoveryRound is one accepted agent round: questions, or the draft book.
type DiscoveryRound struct {
	N         int                  `json:"n"`
	Kind      string               `json:"kind"` // questions or book
	Profile   string               `json:"profile"`
	Snapshot  string               `json:"snapshot_sha256"`
	Questions []DiscoveryQuestion  `json:"questions,omitempty"`
	Files     []string             `json:"files,omitempty"` // book: the draft files in memory
	Review    *DiscoveryBookReview `json:"review,omitempty"`
}

// DiscoveryQuestion is one agent question with the owner's answers, latest last.
type DiscoveryQuestion struct {
	ID          string            `json:"id"`
	Text        string            `json:"text"`
	Why         string            `json:"why"`
	Blocking    bool              `json:"blocking"`
	Options     []string          `json:"options,omitempty"`
	Topic       string            `json:"topic,omitempty"` // "stack": the technology stack, always blocking
	Answers     []DiscoveryAnswer `json:"answers,omitempty"`
	Skipped     bool              `json:"skipped,omitempty"`
	Attribution string            `json:"attribution,omitempty"`
}

// DiscoveryAnswer is one revision of the owner's answer.
type DiscoveryAnswer struct {
	Text string `json:"text"`
	Date string `json:"date"`
}

// DiscoveryAttempt is one run of a discovery agent.
type DiscoveryAttempt struct {
	N        int     `json:"n"`
	Round    int     `json:"round"`
	Profile  string  `json:"profile"`
	RunDir   string  `json:"run_dir"`
	Result   string  `json:"result"` // running, failed, interrupted or accepted
	Feedback string  `json:"feedback,omitempty"`
	Snapshot string  `json:"snapshot_sha256,omitempty"`
	Seconds  float64 `json:"seconds"`
}

// pendingImport is a memory batch saved with the state change it records, so a crash between
// the two never loses or doubles it.
type pendingImport struct {
	Files        map[string][]byte `json:"files"`
	Remove       []string          `json:"remove,omitempty"` // files the batch deletes`
	ExpectedHead string            `json:"expected_head"`
	Subject      string            `json:"subject"`
	Log          string            `json:"log"`
}

// Kinds of rounds.
const (
	roundQuestions = "questions"
	roundBook      = "book"
)

var discoveryQID = regexp.MustCompile(`^R([1-9][0-9]*)-Q([1-9][0-9]*)$`)

func discoveryDir(r Repo, slug string) (string, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return "", err
	}
	return filepath.Join(main, ".aeh", "discovery", slug), nil
}

// LoadDiscovery reads a session; ok is false when it does not exist.
func LoadDiscovery(r Repo, slug string) (DiscoveryState, bool, error) {
	var s DiscoveryState
	if err := checkSlug(slug); err != nil {
		return s, false, fmt.Errorf("discovery: %w", err)
	}
	dir, err := discoveryDir(r, slug)
	if err != nil {
		return s, false, err
	}
	if err := rejectLink(dir); err != nil {
		return s, false, fmt.Errorf("discovery %s: %w", slug, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, false, fmt.Errorf("discovery %s: %w", slug, err)
	}
	if s.Slug != slug || s.Version != discoveryStateVersion {
		return s, false, fmt.Errorf("discovery %s: unexpected slug %q or version %d", slug, s.Slug, s.Version)
	}
	return s, true, nil
}

// ListDiscoveries returns every session, most recently updated first.
func ListDiscoveries(r Repo) ([]DiscoveryState, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(main, ".aeh", "discovery"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []DiscoveryState
	for _, e := range entries {
		if !e.IsDir() || checkSlug(e.Name()) != nil {
			continue
		}
		s, ok, err := LoadDiscovery(r, e.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// discoverySession carries one discovery command.
type discoverySession struct {
	repo   Repo
	cfg    Config
	dcfg   DiscoveryConfig
	memDir string
	dir    string // .aeh/discovery/<slug>
	state  *DiscoveryState
	log    io.Writer
	stack  *projectStack // detected when the round's prompt is rendered
}

func (d *discoverySession) logf(format string, args ...any) {
	if d.log != nil {
		fmt.Fprintf(d.log, format+"\n", args...)
	}
}

func (d *discoverySession) save() error {
	d.state.UpdatedAt = time.Now().UTC()
	return writePrivateJSON(filepath.Join(d.dir, "state.json"), d.state)
}

// openDiscovery takes the project lock and loads configuration, memory and the session. The
// caller must call the returned unlock.
func openDiscovery(r Repo, slug string, log io.Writer) (*discoverySession, func(), error) {
	if err := checkSlug(slug); err != nil {
		return nil, nil, fmt.Errorf("discovery: %w", err)
	}
	main, err := MainCheckout(r)
	if err != nil {
		return nil, nil, err
	}
	r = Repo{Root: main}
	unlock, err := lockLiveProject(r)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*discoverySession, func(), error) {
		unlock()
		return nil, nil, err
	}
	cfg, err := r.LoadConfig()
	if err != nil {
		return fail(err)
	}
	memory, err := LoadMemory(r)
	if err != nil {
		return fail(err)
	}
	if !memory.Present {
		return fail(errors.New("memory is not initialized; run: aeh memory init"))
	}
	dir, err := discoveryDir(r, slug)
	if err != nil {
		return fail(err)
	}
	d := &discoverySession{repo: r, cfg: cfg, dcfg: cfg.discovery(), memDir: memory.Dir, dir: dir, log: log}
	st, ok, err := LoadDiscovery(r, slug)
	if err != nil {
		return fail(err)
	}
	if ok {
		d.state = &st
	}
	return d, unlock, nil
}

// redactedError shows a redacted message and still unwraps to the original error.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr is applied to every error a discovery command returns: paths and diagnostics may
// carry agent-chosen text.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: discoveryDiagnostic(err.Error()), err: err}
}

// DiscoverNew starts a discovery session for an idea and records the idea in memory.
func DiscoverNew(r Repo, slug, idea string) (_ DiscoveryState, err error) {
	defer func() { err = redactErr(err) }()
	d, unlock, err := openDiscovery(r, slug, nil)
	if err != nil {
		return DiscoveryState{}, err
	}
	defer unlock()
	defer d.reportCommand(&err)
	if d.state != nil {
		return *d.state, fmt.Errorf("discovery %s already exists; continue it with: aeh discover run %s", slug, slug)
	}
	idea = normalizeOwnerText(idea)
	if err := checkOwnerText("idea", idea, discoveryAnswerBytes); err != nil {
		return DiscoveryState{}, err
	}
	if err := memoryClean(d.memDir); err != nil {
		return DiscoveryState{}, fmt.Errorf("discover %s: %w", slug, err)
	}
	d.state = &DiscoveryState{Version: discoveryStateVersion, Slug: slug, Idea: idea, Phase: DiscoveryReady}
	files := map[string][]byte{d.memRel("idea.md"): []byte("# Idea\n\n" + idea + "\n")}
	if err := d.record(files, "discover "+slug+": idea", "new session"); err != nil {
		return *d.state, err
	}
	return *d.state, nil
}

// normalizeOwnerText trims the text and turns CRLF into LF, as a terminal may pass it.
func normalizeOwnerText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// checkOwnerText applies the display policy and the secrets filter to the owner's text: it is
// stored in memory, shown again and sent to the next agent.
func checkOwnerText(what, text string, max int) error {
	if text == "" {
		return fmt.Errorf("the %s is empty", what)
	}
	if err := checkDisplayMarkdown(text, max); err != nil {
		return fmt.Errorf("the %s: %w (write plain Markdown without HTML or character references)", what, err)
	}
	if err := checkNoSecrets(what, []byte(text)); err != nil {
		return fmt.Errorf("the %s may contain a secret; remove it:\n%w", what, err)
	}
	return nil
}

func (d *discoverySession) memRel(name string) string {
	return "wiki/discovery/" + d.state.Slug + "/" + name
}

// record saves the state change together with the memory batch that records it, then imports
// the batch. A crash after the save is finished by the next discovery command.
func (d *discoverySession) record(files map[string][]byte, subject, log string) error {
	return d.recordWith(files, nil, subject, log)
}

// recordWith also removes files (pending revisions) in the same memory commit.
func (d *discoverySession) recordWith(files map[string][]byte, remove []string, subject, log string) error {
	p, err := newPendingImport(d.memDir, files, subject, log)
	if err != nil {
		return fmt.Errorf("discover %s: %w", d.state.Slug, err)
	}
	p.Remove = remove
	d.state.Pending = p
	if err := d.save(); err != nil {
		return err
	}
	return d.finishPending()
}

// finishPending imports the saved batch, if any.
func (d *discoverySession) finishPending() error {
	if d.state.Pending == nil {
		return nil
	}
	if err := finishImport(d.repo, d.memDir, "discover/"+d.state.Slug, d.state.Pending, d.save); err != nil {
		return fmt.Errorf("discover %s: %w", d.state.Slug, err)
	}
	d.state.Pending = nil
	return d.save()
}

// batchFilesChanged names the first batch file whose committed content differs between two
// memory commits, or returns "".
func batchFilesChanged(dir, from, to string, files map[string][]byte) string {
	for _, rel := range sortedKeys(files) {
		a, aok := gitBlob(dir, from, rel)
		b, bok := gitBlob(dir, to, rel)
		if aok != bok || a != b {
			return rel
		}
	}
	return ""
}

// resume finishes a pending import and checks that memory is clean, as every command needs.
func (d *discoverySession) resume() error {
	if d.state == nil {
		return fmt.Errorf("discovery does not exist; start it with: aeh discover new <slug> \"<idea>\"")
	}
	if err := recoverPending(d.memDir, "discover/"+d.state.Slug, d.state.Pending); err != nil {
		return fmt.Errorf("discover %s: %w", d.state.Slug, err)
	}
	return d.finishPending()
}

// discardPartialImport undoes what an import interrupted before its commit left on disk, and
// nothing else: each dirty path must be a batch file holding exactly the batch's bytes, or
// the log with exactly this import's entry appended. Anything else stays for the owner, and
// memoryClean reports it.
func discardPartialImport(dir string, p *pendingImport, session string) error {
	out, err := gitRaw(dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || out == "" {
		return err
	}
	var paths []string
	var removed []string
	for _, e := range strings.Split(strings.TrimRight(out, "\x00"), "\x00") {
		if len(e) >= 4 && (e[:2] == " D" || e[:2] == "D ") && slices.Contains(p.Remove, e[3:]) {
			removed = append(removed, e[3:]) // a deletion this batch makes
			continue
		}
		if len(e) < 4 || strings.ContainsAny(e[:2], "RCUD") || e[:2] == "AA" {
			return nil // renames, copies, deletions and conflicts are not aeh's
		}
		paths = append(paths, e[3:])
	}
	// Both the working file and the index entry must be aeh's: the index may hold HEAD's
	// content (not staged yet) or exactly what aeh wrote (staged by the import). A path in
	// HEAD must still be in the index: removing it from the index was someone's decision.
	for _, rel := range paths {
		disk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil
		}
		head, inHead := gitBlob(dir, "HEAD", rel)
		index, inIndex := gitBlob(dir, "", rel)
		want, ours := p.Files[rel]
		switch {
		case ours:
		case rel == "wiki/log.md" && inHead && partialLogEntry(head, string(disk), session, p.Log):
			want = disk
		default:
			return nil
		}
		if !bytes.Equal(disk, want) {
			return nil
		}
		if inHead && !inIndex {
			return nil
		}
		if inIndex && !(inHead && index == head) && index != string(want) {
			return nil
		}
	}
	for _, rel := range removed {
		if _, err := git(dir, "reset", "-q", "--", rel); err != nil {
			return err
		}
		if _, err := git(dir, "checkout", "-q", "HEAD", "--", rel); err != nil {
			return err
		}
	}
	for _, rel := range paths {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := git(dir, "reset", "-q", "--", rel); err != nil {
			return err
		}
		if _, inHead := gitBlob(dir, "HEAD", rel); inHead {
			if _, err := git(dir, "checkout", "-q", "HEAD", "--", rel); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(full); err != nil {
			return err
		}
		for p := filepath.Dir(full); len(p) > len(dir); p = filepath.Dir(p) {
			if os.Remove(p) != nil {
				break // not empty
			}
		}
	}
	return nil
}

// gitRaw runs git and returns its output untrimmed, for porcelain formats where leading
// spaces carry meaning.
func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), args...)...)
	cmd.Dir = dir
	cmd.Env = commandEnv(nil)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// partialLogEntry reports whether disk is head plus exactly the entry appendLog writes for
// this import, on any date.
func partialLogEntry(head, disk, session, log string) bool {
	if head != "" && !strings.HasSuffix(head, "\n") {
		head += "\n"
	}
	rest, ok := strings.CutPrefix(disk, head)
	return ok && importLogEntry.MatchString(rest) && strings.HasSuffix(rest, "] import | "+session+" | "+log+"\n")
}

var importLogEntry = regexp.MustCompile(`^\n## \[[0-9]{4}-[0-9]{2}-[0-9]{2}\] import \| [^\n]*\n$`)

// DiscoverRun runs one discovery round with the discovery ladder. The round ends with the
// agent's questions for the owner, or with a validated draft book in memory.
func DiscoverRun(ctx context.Context, r Repo, slug string, log io.Writer) (_ DiscoveryState, err error) {
	defer func() { err = redactErr(err) }()
	d, unlock, err := openDiscovery(r, slug, log)
	if err != nil {
		return DiscoveryState{}, err
	}
	defer unlock()
	defer d.reportCommand(&err)
	if err := d.resume(); err != nil {
		return d.snapshot(), err
	}
	st := d.state
	if st.Book != nil {
		err := d.resumeBook(ctx)
		return *st, err
	}
	switch st.Phase {
	case DiscoveryAwaiting:
		return *st, fmt.Errorf("discovery %s waits for answers; see them with: aeh discover show %s", slug, slug)
	case DiscoveryDraft:
		return *st, fmt.Errorf("discovery %s already has a draft book in memory under %s", slug, d.memRel("drafts/"))
	}
	if len(d.dcfg.Ladder) == 0 {
		return *st, errors.New("config: discovery needs a ladder (discovery.ladder or ladder)")
	}
	for _, name := range d.dcfg.Ladder {
		if err := codexRepoCheckError(name, d.cfg.Profiles[name]); err != nil {
			return *st, err
		}
	}
	for i := range st.Attempts {
		if st.Attempts[i].Result == "running" {
			st.Attempts[i].Result, st.Attempts[i].Feedback = "interrupted", "the previous aeh discover run stopped during this attempt"
		}
	}
	if st.Status == StatusBlocked {
		// Running again after a block is the owner's retry: a new ladder round starts.
		st.LadderFrom, st.Status, st.Question = len(st.Attempts), "", ""
	}
	st.Status = StatusRunning
	if err := d.save(); err != nil {
		return *st, err
	}
	round := len(st.Rounds) + 1
	feedback := ""
	for _, profile := range d.dcfg.Ladder {
		done := 0
		for _, prev := range st.Attempts[min(st.LadderFrom, len(st.Attempts)):] {
			if prev.Profile == profile && prev.Round == round {
				done++
			}
		}
		for try := done; try < 2; try++ {
			n := len(st.Attempts) + 1
			st.Attempts = append(st.Attempts, DiscoveryAttempt{N: n, Round: round, Profile: profile, Result: "running",
				RunDir: rel(d.repo.Root, d.repo.RunDir("discover-"+slug, fmt.Sprintf("a%d", n)))})
			if err := d.save(); err != nil {
				return *st, err
			}
			start := time.Now()
			out, blockQuestion, err := d.attempt(ctx, &st.Attempts[n-1], round, feedback)
			st.Attempts[n-1].Seconds = time.Since(start).Seconds()
			if err != nil {
				_ = d.save()
				return *st, err
			}
			if blockQuestion != "" {
				st.Status, st.Question = StatusBlocked, blockQuestion
				return *st, d.save()
			}
			if out != nil {
				if out.kind == roundBook {
					err := d.startBook(ctx, round, &st.Attempts[n-1])
					return *st, err
				}
				err := d.accept(round, &st.Attempts[n-1], out)
				return *st, err
			}
			if err := d.save(); err != nil {
				return *st, err
			}
			d.logf("    attempt %d failed: %s", n, firstLine(st.Attempts[n-1].Feedback))
			feedback = "Your previous attempt was rejected:\n" + st.Attempts[n-1].Feedback
		}
	}
	last := st.Attempts[len(st.Attempts)-1]
	st.Status = StatusBlocked
	st.Question = fmt.Sprintf("No discovery agent produced a valid round. Last failure:\n%s\nAdjust the discovery ladder or the prompt, then run again: aeh discover run %s", tail(last.Feedback, 2000), slug)
	return *st, d.save()
}

// snapshot returns the session state for an error path, or an empty one.
func (d *discoverySession) snapshot() DiscoveryState {
	if d.state == nil {
		return DiscoveryState{}
	}
	return *d.state
}

// discoveryOutput is an accepted round, ready to record.
type discoveryOutput struct {
	kind      string
	snapshot  frozenOutput
	questions []DiscoveryQuestion
	files     map[string][]byte // book: memory files under drafts/
}

// attempt runs one discovery agent. It returns the accepted output, or a question that blocks
// the session, or neither when the attempt failed with feedback for the next one.
func (d *discoverySession) attempt(ctx context.Context, a *DiscoveryAttempt, round int, feedback string) (*discoveryOutput, string, error) {
	fail := func(msg string) (*discoveryOutput, string, error) {
		a.Result, a.Feedback = "failed", discoveryDiagnostic(msg)
		return nil, "", nil
	}
	outDir := d.repo.WorktreePath(fmt.Sprintf("discover-%s-a%d", d.state.Slug, a.N))
	_ = os.RemoveAll(outDir) // a leftover of an interrupted attempt with this number
	root, err := openOutputDir(outDir)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		root.Close()
		_ = os.RemoveAll(outDir)
	}()
	prompt, err := d.prompt(round, feedback)
	if err != nil {
		return nil, "", err
	}
	runDir := filepath.Join(d.repo.Root, filepath.FromSlash(a.RunDir))
	d.logf("  discovery round %d, attempt %d with profile %q", round, a.N, a.Profile)
	outPath, err := runAgent(ctx, d.cfg.Profiles[a.Profile], d.cfg.PassEnv, outDir, prompt, runDir, "discover")
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", err
		}
		if uncertainExit(err) {
			a.Result, a.Feedback = "failed", discoveryDiagnostic(err.Error())
			return nil, fmt.Sprintf("The discovery agent %q was interrupted or timed out. Confirm its processes stopped, then run again: aeh discover run %s", a.Profile, d.state.Slug), nil
		}
		return fail(err.Error())
	}
	out, _ := os.ReadFile(outPath)
	var m outputManifest
	switch discoveryVerdict(string(out)) {
	case discoveryQuestions:
		m = questionsManifest()
	case discoveryBook:
		m = bookManifest(d.dcfg)
	default:
		return fail(fmt.Sprintf("the output must end with a line %q or %q", discoveryQuestions, discoveryBook))
	}
	frozen, err := freezeOutput(root, m)
	if err != nil {
		return fail(err.Error())
	}
	for _, rel := range sortedKeys(frozen.Files) {
		if err := checkNoSecrets(rel, frozen.Files[rel]); err != nil {
			return fail(err.Error())
		}
	}
	res := &discoveryOutput{snapshot: frozen}
	if m.Name == questionsManifest().Name {
		res.kind = roundQuestions
		res.questions, err = parseDiscoveryQuestions(frozen.Files["questions.yaml"], round, d.dcfg.MaxQuestions)
	} else {
		res.kind = roundBook
		res.files, err = d.bookFiles(frozen)
	}
	if err != nil {
		return fail(err.Error())
	}
	if msg := d.checkStackRule(res); msg != "" {
		return fail(msg)
	}
	if err := saveFrozen(filepath.Join(d.dir, "snapshots", fmt.Sprintf("a%d.json", a.N)), frozen); err != nil {
		return nil, "", err
	}
	a.Result, a.Snapshot = "accepted", frozen.Digest
	return res, "", nil
}

func discoveryVerdict(out string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(out, "\r\n", "\n"), "\n \t"), "\n")
	switch last := strings.TrimSpace(lines[len(lines)-1]); last {
	case discoveryQuestions, discoveryBook:
		return last
	}
	return ""
}

func questionsManifest() outputManifest {
	return outputManifest{
		Name:     "discovery-questions",
		Rules:    []outputRule{{Pattern: "questions.yaml", Kind: outputYAML, MaxBytes: discoveryDocBytes, MaxCount: 1, Required: true}},
		MaxFiles: 1,
		MaxTotal: discoveryDocBytes,
	}
}

func bookManifest(d DiscoveryConfig) outputManifest {
	mockups := 4 * d.MaxFeatures
	return outputManifest{
		Name: "discovery-book",
		Rules: []outputRule{
			{Pattern: "vision.md", Kind: outputMarkdown, MaxBytes: discoveryDocBytes, MaxCount: 1, Required: true},
			{Pattern: "data.md", Kind: outputMarkdown, MaxBytes: discoveryDocBytes, MaxCount: 1, Required: true},
			{Pattern: "features/*.md", Kind: outputMarkdown, MaxBytes: discoveryDocBytes, MaxCount: d.MaxFeatures, Required: true},
			{Pattern: "mockups/*/*.html", Kind: outputHTML, MaxBytes: int64(d.MaxMockupBytes), MaxCount: mockups},
		},
		MaxFiles: 2 + d.MaxFeatures + mockups,
		MaxTotal: int64(2+d.MaxFeatures)*discoveryDocBytes + int64(mockups)*int64(d.MaxMockupBytes),
	}
}

// questionsFile is the schema of questions.yaml.
type questionsFile struct {
	Questions []struct {
		Text     string   `yaml:"text"`
		Why      string   `yaml:"why"`
		Blocking *bool    `yaml:"blocking"`
		Options  []string `yaml:"options"`
		Topic    string   `yaml:"topic"`
	} `yaml:"questions"`
}

// parseDiscoveryQuestions reads questions.yaml; aeh assigns the IDs R<round>-Q<n> in order.
func parseDiscoveryQuestions(b []byte, round, max int) ([]DiscoveryQuestion, error) {
	var f questionsFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("questions.yaml must be a mapping with a questions list of text, why, blocking and options")
	}
	if len(f.Questions) == 0 || len(f.Questions) > max {
		return nil, fmt.Errorf("questions.yaml must hold from 1 through %d questions", max)
	}
	var out []DiscoveryQuestion
	for i, q := range f.Questions {
		n := i + 1
		if err := checkDisplayLine(q.Text, discoveryLineBytes); err != nil {
			return nil, fmt.Errorf("question %d text: %w", n, err)
		}
		if err := checkDisplayLine(q.Why, discoveryLineBytes); err != nil {
			return nil, fmt.Errorf("question %d why: %w", n, err)
		}
		if q.Blocking == nil {
			return nil, fmt.Errorf("question %d must say blocking: true or false", n)
		}
		switch q.Topic {
		case "":
		case topicStack:
			if !*q.Blocking {
				return nil, fmt.Errorf("question %d is about the technology stack (topic: stack) and must be blocking", n)
			}
		default:
			return nil, fmt.Errorf("question %d: topic must be empty or stack", n)
		}
		if len(q.Options) > discoveryMaxOptions {
			return nil, fmt.Errorf("question %d has more than %d options", n, discoveryMaxOptions)
		}
		for j, o := range q.Options {
			if err := checkDisplayLine(o, discoveryOptionBytes); err != nil {
				return nil, fmt.Errorf("question %d option %d: %w", n, j+1, err)
			}
		}
		// YAML escapes can spell a secret the raw bytes did not show.
		decoded := strings.Join(append([]string{q.Text, q.Why}, q.Options...), "\n")
		if err := checkNoSecrets(fmt.Sprintf("question %d", n), []byte(decoded)); err != nil {
			return nil, err
		}
		out = append(out, DiscoveryQuestion{ID: fmt.Sprintf("R%d-Q%d", round, n), Text: strings.TrimSpace(q.Text),
			Why: strings.TrimSpace(q.Why), Blocking: *q.Blocking, Options: q.Options, Topic: q.Topic})
	}
	return out, nil
}

// bookFiles validates a draft book and returns its memory files. Every document passes the
// display policy; every mockup is rebuilt from validated tokens.
func (d *discoverySession) bookFiles(f frozenOutput) (map[string][]byte, error) {
	for _, rel := range sortedKeys(f.Files) {
		if err := checkNoSecrets(rel, f.Files[rel]); err != nil {
			return nil, err
		}
	}
	features := map[string]bool{}
	for _, rel := range sortedKeys(f.Files) {
		if name, ok := strings.CutPrefix(rel, "features/"); ok {
			slug := strings.TrimSuffix(name, ".md")
			if err := checkSlug(slug); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			features[slug] = true
		}
	}
	files := map[string][]byte{}
	for _, rel := range sortedKeys(f.Files) {
		b := f.Files[rel]
		if err := checkNoSecrets("a file name", []byte(rel)); err != nil {
			return nil, err
		}
		if strings.HasSuffix(rel, ".html") {
			parts := strings.Split(strings.TrimSuffix(rel, ".html"), "/") // mockups/<feature>/<screen>
			if !features[parts[1]] {
				return nil, fmt.Errorf("%s: mockups belong under a feature of this book", rel)
			}
			if err := checkSlug(parts[2]); err != nil {
				return nil, fmt.Errorf("%s: screen %w", rel, err)
			}
			page, err := sanitizeMockup(b, parts[1]+" / "+parts[2], d.dcfg.MaxMockupBytes, discoveryImageBytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if err := checkNoSecrets(rel, page); err != nil {
				return nil, err
			}
			b = page
		} else if err := checkDisplayMarkdown(string(b), discoveryDocBytes); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		files[d.memRel("drafts/"+rel)] = b
	}
	if err := d.validateBookFeatures(f); err != nil {
		return nil, err
	}
	return files, nil
}

// accept records an accepted round in state and memory, in one pending step.
func (d *discoverySession) accept(round int, a *DiscoveryAttempt, out *discoveryOutput) error {
	return d.acceptReviewed(round, a, out, nil)
}

func (d *discoverySession) acceptReviewed(round int, a *DiscoveryAttempt, out *discoveryOutput, review *DiscoveryBookReview) error {
	st := d.state
	r := DiscoveryRound{N: round, Kind: out.kind, Profile: a.Profile, Snapshot: out.snapshot.Digest, Questions: out.questions, Review: review}
	files := map[string][]byte{}
	subject := fmt.Sprintf("discover %s: round %d questions", st.Slug, round)
	if out.kind == roundBook {
		files = out.files
		for _, rel := range sortedKeys(files) {
			r.Files = append(r.Files, strings.TrimPrefix(rel, d.memRel("")))
		}
		subject = fmt.Sprintf("discover %s: round %d draft book", st.Slug, round)
		// Drafts of existing features are revisions: they are imported as spec.next.md.
		revisions, err := d.revisionFiles(out.files)
		if err != nil {
			return err
		}
		for rel, b := range revisions {
			files[rel] = b
		}
	}
	st.Rounds = append(st.Rounds, r)
	files[d.roundRel(round)] = []byte(d.renderRound(st.Rounds[len(st.Rounds)-1]))
	st.Phase, st.Status, st.Question = d.phase(), "", ""
	if err := d.record(files, subject, fmt.Sprintf("round %d %s", round, out.kind)); err != nil {
		return err
	}
	switch st.Phase {
	case DiscoveryDraft:
		d.logf("round %d: the draft book is in memory under %s", round, d.memRel("drafts/"))
	case DiscoveryAwaiting:
		d.logf("round %d: the agent has questions; see them with: aeh discover show %s", round, st.Slug)
	default:
		d.logf("round %d: the agent has optional questions; answer them, or run the next round: aeh discover run %s", round, st.Slug)
	}
	return nil
}

func (d *discoverySession) roundRel(n int) string { return d.memRel(fmt.Sprintf("rounds/%02d.md", n)) }

// phase derives the session phase from its rounds.
func (d *discoverySession) phase() string {
	rounds := d.state.Rounds
	if len(rounds) == 0 {
		return DiscoveryReady
	}
	last := rounds[len(rounds)-1]
	if last.Kind == roundBook {
		return DiscoveryDraft
	}
	for _, q := range last.Questions {
		if q.Blocking && len(q.Answers) == 0 {
			return DiscoveryAwaiting
		}
	}
	return DiscoveryReady
}

// renderRound is the readable record of a round in memory. It depends only on state, so a
// pending import can be rebuilt byte for byte.
func (d *discoverySession) renderRound(r DiscoveryRound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Round %d\n\nGenerated by aeh from the discovery agent (profile %s) and the owner's answers. Do not edit by hand.\n", r.N, discoveryDiagnostic(r.Profile))
	if r.Kind == roundBook {
		b.WriteString("\nThe agent proposed a draft book in this session's directory. Drafts are not features: they claim no paths until the owner promotes one.\n\n")
		for _, f := range r.Files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		if r.Review != nil {
			renderBookReview(&b, r.Review)
		}
	}
	for _, q := range r.Questions {
		kind := "optional"
		if q.Blocking {
			kind = "blocking"
		}
		fmt.Fprintf(&b, "\n## %s (%s)\n\n%s\n\nWhy: %s\n", q.ID, kind, q.Text, q.Why)
		if q.Attribution != "" {
			fmt.Fprintf(&b, "\n%s\n", q.Attribution)
		}
		if len(q.Options) > 0 {
			b.WriteString("\nOptions:\n\n")
			for _, o := range q.Options {
				fmt.Fprintf(&b, "- %s\n", o)
			}
		}
		for i, a := range q.Answers {
			label := "Answer"
			if i > 0 {
				label = "Revised answer"
			}
			fmt.Fprintf(&b, "\n%s (%s):\n\n%s\n", label, a.Date, a.Text)
		}
		if q.Skipped {
			b.WriteString("\nSkipped by the owner.\n")
		}
	}
	return b.String()
}

// findQuestion locates a question by ID.
func (d *discoverySession) findQuestion(qid string) (*DiscoveryRound, *DiscoveryQuestion, error) {
	m := discoveryQID.FindStringSubmatch(qid)
	if m == nil {
		return nil, nil, fmt.Errorf("question ID %q must look like R1-Q2", qid)
	}
	for i := range d.state.Rounds {
		r := &d.state.Rounds[i]
		for j := range r.Questions {
			if r.Questions[j].ID == qid {
				return r, &r.Questions[j], nil
			}
		}
	}
	return nil, nil, fmt.Errorf("discovery %s has no question %s; see them with: aeh discover show %s", d.state.Slug, qid, d.state.Slug)
}

// DiscoverAnswer records the owner's answer to a question. Answering again records a revision;
// the next round receives only the latest one.
func DiscoverAnswer(r Repo, slug, qid, text string) (_ DiscoveryState, err error) {
	defer func() { err = redactErr(err) }()
	d, unlock, err := openDiscovery(r, slug, nil)
	if err != nil {
		return DiscoveryState{}, err
	}
	defer unlock()
	defer d.reportCommand(&err)
	if err := d.resume(); err != nil {
		return d.snapshot(), err
	}
	text = normalizeOwnerText(text)
	if err := checkOwnerText("answer", text, discoveryAnswerBytes); err != nil {
		return *d.state, err
	}
	round, q, err := d.findQuestion(qid)
	if err != nil {
		return *d.state, err
	}
	q.Answers = append(q.Answers, DiscoveryAnswer{Text: text, Date: memoryNow().UTC().Format("2006-01-02")})
	q.Skipped = false
	return d.recordQuestion(round, fmt.Sprintf("answer %s", qid))
}

// DiscoverSkip records that the owner skips an optional question.
func DiscoverSkip(r Repo, slug, qid string) (_ DiscoveryState, err error) {
	defer func() { err = redactErr(err) }()
	d, unlock, err := openDiscovery(r, slug, nil)
	if err != nil {
		return DiscoveryState{}, err
	}
	defer unlock()
	defer d.reportCommand(&err)
	if err := d.resume(); err != nil {
		return d.snapshot(), err
	}
	round, q, err := d.findQuestion(qid)
	if err != nil {
		return *d.state, err
	}
	switch {
	case q.Blocking:
		return *d.state, fmt.Errorf("%s is a blocking question; answer it: aeh discover answer %s %s \"...\"", qid, slug, qid)
	case len(q.Answers) > 0:
		return *d.state, fmt.Errorf("%s is already answered", qid)
	case q.Skipped:
		return *d.state, nil
	}
	q.Skipped = true
	return d.recordQuestion(round, fmt.Sprintf("skip %s", qid))
}

func (d *discoverySession) recordQuestion(round *DiscoveryRound, what string) (DiscoveryState, error) {
	st := d.state
	if st.Phase != DiscoveryDraft {
		st.Phase = d.phase()
	}
	files := map[string][]byte{d.roundRel(round.N): []byte(d.renderRound(*round))}
	if err := d.record(files, fmt.Sprintf("discover %s: %s", st.Slug, what), what); err != nil {
		return *st, err
	}
	return *st, nil
}

// prompt renders the discovery prompt for a round.
func (d *discoverySession) prompt(round int, feedback string) (string, error) {
	text, err := loadPrompt(d.repo, "discover.md")
	if err != nil {
		return "", err
	}
	memory, err := LoadMemory(d.repo)
	if err != nil {
		return "", err
	}
	stack, err := detectStack(d.repo)
	if err != nil {
		return "", err
	}
	d.stack = &stack
	data := promptData{
		Greenfield:   stack.Greenfield,
		Stack:        stack.Evidence,
		StackAsked:   d.stackQuestion(false),
		StackAnswer:  d.stackQuestion(true),
		Idea:         d.state.Idea,
		History:      d.history(),
		Features:     discoveryFeatureBlock(memory),
		Research:     d.dcfg.Research,
		Round:        round,
		MaxQuestions: d.dcfg.MaxQuestions,
		MaxFeatures:  d.dcfg.MaxFeatures,
		MaxBytes:     d.dcfg.MaxMockupBytes,
		Feedback:     feedback,
	}
	return renderPrompt("discover.md", text, data)
}

// stackQuestion reports whether an earlier round asked the owner about the technology stack;
// with answered, whether the owner answered it.
func (d *discoverySession) stackQuestion(answered bool) bool {
	for _, r := range d.state.Rounds {
		for _, q := range r.Questions {
			if q.Topic == topicStack && (!answered || len(q.Answers) > 0) {
				return true
			}
		}
	}
	return false
}

// checkStackRule is the runner's rule for a project that starts from scratch: the owner
// chooses its technology stack. Until a stack question is answered, a book is refused, and a
// questions round must ask it unless an earlier round already did. For an existing project
// the prompt asks the agent to keep the detected stack.
func (d *discoverySession) checkStackRule(out *discoveryOutput) string {
	if d.stack == nil || !d.stack.Greenfield || d.stackQuestion(true) {
		return ""
	}
	ask := "this project starts from scratch, so the owner chooses its technology stack: ask it as one blocking question with topic: stack (language, framework, storage and delivery, with the options you recommend)"
	if out.kind == roundBook {
		return "the book cannot be written yet: " + ask
	}
	if d.stackQuestion(false) {
		return ""
	}
	for _, q := range out.questions {
		if q.Topic == topicStack {
			return ""
		}
	}
	return "this round must ask about the stack: " + ask
}

// history lists earlier questions with the latest answer of each, or that the owner skipped
// or has not answered it.
func (d *discoverySession) history() string {
	var b strings.Builder
	for _, r := range d.state.Rounds {
		if r.Kind != roundQuestions {
			continue
		}
		for _, q := range r.Questions {
			topic := ""
			if q.Topic != "" {
				topic = " (topic: " + q.Topic + ")"
			}
			fmt.Fprintf(&b, "%s%s: %s\n", q.ID, topic, q.Text)
			switch {
			case len(q.Answers) > 0:
				fmt.Fprintf(&b, "Answer: %s\n\n", q.Answers[len(q.Answers)-1].Text)
			case q.Skipped:
				b.WriteString("Skipped by the owner.\n\n")
			default:
				b.WriteString("Not answered.\n\n")
			}
		}
	}
	return b.String()
}

// codexRepoCheckError reports a Codex profile that would refuse discovery's output
// directory: it is not a Git repository, and `codex exec` stops there unless it gets
// --skip-git-repo-check. Checking before launch saves a whole agent round.
func codexRepoCheckError(name string, p Profile) error {
	if len(p.Command) < 2 {
		return nil
	}
	base := strings.ToLower(filepath.Base(p.Command[0]))
	for _, ext := range []string{".cmd", ".exe", ".bat", ".ps1"} {
		base = strings.TrimSuffix(base, ext)
	}
	if base != "codex" || !slices.Contains(p.Command[1:], "exec") || slices.Contains(p.Command[1:], "--skip-git-repo-check") {
		return nil
	}
	return fmt.Errorf("config: profile %q runs codex exec without --skip-git-repo-check; discovery runs in a directory that is not a Git repository and Codex refuses it there, so add the flag to the profile's command", name)
}
