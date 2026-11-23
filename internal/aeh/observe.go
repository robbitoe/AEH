package aeh

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Adopted areas (stage 4, entry B2). An area of an existing system is described by
// observations: behavior an agent saw in the code at a pinned commit. Each observation has its
// own identity, kept in an append-only event log in memory:
//
//	wiki/adopt/<feature>/observations.log.yaml   observe and confirm events, oldest first
//	wiki/adopt/<feature>/area.md                 title, paths, prose, and a generated criteria section
//	wiki/adopt/<feature>/defects.md              generated: observations the owner called bugs
//
// The log is replayed from the memory repository's history, so a rewritten or deleted log is an
// error and a retired number never comes back. The criteria section of area.md and defects.md
// are renderings of the log; a hand edit of either is reported, never accepted silently.

// Observation states and the owner's decisions.
const (
	ObservationActive    = "active"    // observed; the owner has not decided yet
	ObservationConfirmed = "confirmed" // keep: the owner confirmed the current behavior
	ObservationRetired   = "retired"   // bug or drop

	DecisionKeep = "keep"
	DecisionBug  = "bug"
	DecisionDrop = "drop"
)

const (
	observationTextBytes    = 500
	maxObservationLocations = 10
	adoptStateVersion       = 1
)

// observationEvent is one line of an area's history.
type observationEvent struct {
	Event     string   `yaml:"event" json:"event"` // observe or confirm
	ID        int      `yaml:"id" json:"id"`
	Text      string   `yaml:"text" json:"text"`
	TextSHA   string   `yaml:"text_sha256" json:"text_sha256"`
	CodeSHA   string   `yaml:"code_sha" json:"code_sha"`
	Locations []string `yaml:"locations,omitempty" json:"locations,omitempty"`
	Decision  string   `yaml:"decision,omitempty" json:"decision,omitempty"`
	Date      string   `yaml:"date" json:"date"`
}

// Observation is the current state of one observation ID.
type Observation struct {
	ID        int      `json:"id"`
	Text      string   `json:"text"`
	TextSHA   string   `json:"text_sha256"`
	CodeSHA   string   `json:"code_sha"`
	Locations []string `json:"locations,omitempty"`
	Status    string   `json:"status"`
	Decision  string   `json:"decision,omitempty"`
	Date      string   `json:"date"`
}

// Token binds an owner's decision to the observation as the owner saw it: its text and the
// commit it was observed at.
func (o Observation) Token() string {
	sum := sha256.Sum256([]byte(o.TextSHA + "\x00" + o.CodeSHA))
	return hex.EncodeToString(sum[:])[:12]
}

// areaIdentity is an area's replayed history.
type areaIdentity struct {
	Events []observationEvent
	Obs    map[int]*Observation
	High   int // the highest ID ever assigned
}

// sorted returns the observations by ID.
func (a areaIdentity) sorted() []Observation {
	out := make([]Observation, 0, len(a.Obs))
	for _, o := range a.Obs {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func areaRel(feature, name string) string { return "wiki/adopt/" + feature + "/" + name }

func observationDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

var (
	observationDate     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	observationLocation = regexp.MustCompile(`^([^:]+):([1-9][0-9]*)(?:-([1-9][0-9]*))?$`)
)

// checkObservationText accepts one display line that can be a criterion's text.
func checkObservationText(text string) error {
	if text != strings.TrimSpace(text) {
		return errors.New("observation text must not start or end with spaces")
	}
	if err := checkDisplayLine(text, observationTextBytes); err != nil {
		return fmt.Errorf("observation text: %w", err)
	}
	if strayLine.MatchString(text) {
		return errors.New(`observation text must not contain "AC-<n>:"`)
	}
	return checkNoSecrets("observation text", []byte(text))
}

// checkObservationLocation accepts path:line or path:start-end inside the repository.
func checkObservationLocation(loc string) error {
	if err := checkNoSecrets("observation location", []byte(loc)); err != nil {
		return err
	}
	if err := checkDisplayLine(loc, 500); err != nil {
		return fmt.Errorf("observation location: %w", err)
	}
	m := observationLocation.FindStringSubmatch(loc)
	if m == nil {
		return fmt.Errorf("location %q must be path:line or path:start-end", redactDiagnostic(loc))
	}
	if err := checkPlanTestPath(m[1]); err != nil {
		return fmt.Errorf("location path: %w", err)
	}
	if m[3] != "" {
		a, _ := strconv.Atoi(m[2])
		b, err := strconv.Atoi(m[3])
		if err != nil || b < a {
			return errors.New("location range must not end before it starts")
		}
	}
	return nil
}

// parseObservationLog reads an area's event log: one YAML list, strict.
func parseObservationLog(b []byte) ([]observationEvent, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	if err := checkPlainYAML(b); err != nil {
		return nil, fmt.Errorf("observation log: %w", err)
	}
	var events []observationEvent
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&events); err != nil {
		return nil, fmt.Errorf("observation log: %w", err)
	}
	return events, nil
}

// replayObservations applies events in order and rejects any event the rules forbid.
func replayObservations(events []observationEvent) (areaIdentity, error) {
	a := areaIdentity{Events: events, Obs: map[int]*Observation{}}
	for i, e := range events {
		at := fmt.Sprintf("observation log event %d (AC-%d)", i+1, e.ID)
		switch {
		case e.ID < 1:
			return a, fmt.Errorf("%s: IDs start at 1", at)
		case !observationDate.MatchString(e.Date):
			return a, fmt.Errorf("%s: date must be YYYY-MM-DD", at)
		case !isFullSHA(e.CodeSHA):
			return a, fmt.Errorf("%s: code_sha must be a full commit SHA", at)
		case e.TextSHA != observationDigest(e.Text):
			return a, fmt.Errorf("%s: text_sha256 does not match the text", at)
		}
		if err := checkObservationText(e.Text); err != nil {
			return a, fmt.Errorf("%s: %w", at, err)
		}
		o := a.Obs[e.ID]
		switch e.Event {
		case "observe":
			if e.Decision != "" || len(e.Locations) == 0 || len(e.Locations) > maxObservationLocations {
				return a, fmt.Errorf("%s: an observe event has 1 to %d locations and no decision", at, maxObservationLocations)
			}
			for _, l := range e.Locations {
				if err := checkObservationLocation(l); err != nil {
					return a, fmt.Errorf("%s: %w", at, err)
				}
			}
			if o == nil {
				if e.ID != a.High+1 {
					return a, fmt.Errorf("%s: a new observation takes the next number, AC-%d", at, a.High+1)
				}
				o = &Observation{ID: e.ID}
				a.Obs[e.ID] = o
				a.High = e.ID
			} else if o.Status != ObservationActive {
				return a, fmt.Errorf("%s: AC-%d is %s and cannot be observed again", at, e.ID, o.Status)
			}
			o.Text, o.TextSHA, o.CodeSHA, o.Locations, o.Status, o.Date = e.Text, e.TextSHA, e.CodeSHA, e.Locations, ObservationActive, e.Date
		case "confirm":
			if len(e.Locations) != 0 {
				return a, fmt.Errorf("%s: a confirm event has no locations", at)
			}
			if o == nil || o.Status != ObservationActive {
				return a, fmt.Errorf("%s: only an active observation can be confirmed", at)
			}
			if e.TextSHA != o.TextSHA || e.CodeSHA != o.CodeSHA {
				return a, fmt.Errorf("%s: the decision is not bound to the current observation", at)
			}
			switch e.Decision {
			case DecisionKeep:
				o.Status = ObservationConfirmed
			case DecisionBug, DecisionDrop:
				o.Status = ObservationRetired
			default:
				return a, fmt.Errorf("%s: decision must be keep, bug or drop", at)
			}
			o.Decision, o.Date = e.Decision, e.Date
		default:
			return a, fmt.Errorf("%s: event must be observe or confirm", at)
		}
	}
	return a, nil
}

// loadAreaIdentity replays an area's log at memory HEAD after checking that every committed
// version of the log extends the one before it. ok is false when the area has no history.
func loadAreaIdentity(memDir, feature string) (areaIdentity, bool, error) {
	rel := areaRel(feature, "observations.log.yaml")
	// Every version of the log reachable from HEAD, on every branch and through every merge
	// (no history simplification), must be a prefix of the log at HEAD: a version that a merge
	// or a restore dropped would otherwise revive retired numbers.
	raw, err := gitRaw(memDir, "log", "HEAD", "--full-history", "-m", "--raw", "--no-abbrev", "--no-renames", "--format=", "--", rel)
	if err != nil {
		return areaIdentity{}, false, fmt.Errorf("observation log history: %w", err)
	}
	versions := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		// :<old mode> <new mode> <old oid> <new oid> <status>\t<path>
		fields := strings.Fields(strings.SplitN(line, "\t", 2)[0])
		if len(fields) < 5 || !strings.HasPrefix(fields[0], ":") {
			continue
		}
		if oid := fields[3]; strings.Trim(oid, "0") != "" {
			versions[oid] = true
		}
	}
	if len(versions) == 0 {
		return areaIdentity{Obs: map[int]*Observation{}}, false, nil
	}
	headText, ok := gitBlob(memDir, "HEAD", rel)
	if !ok {
		return areaIdentity{}, true, fmt.Errorf("%s is missing at memory HEAD: the log only grows; restore it from the memory history", rel)
	}
	head, err := parseObservationLog([]byte(headText))
	if err != nil {
		return areaIdentity{}, true, fmt.Errorf("%s: %w", rel, err)
	}
	oids := make([]string, 0, len(versions))
	for oid := range versions {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	blobs, err := areaHistoryBlobs(memDir, oids)
	if err != nil {
		return areaIdentity{}, true, err
	}
	for _, b := range blobs {
		events, err := parseObservationLog(b)
		if err != nil {
			return areaIdentity{}, true, fmt.Errorf("an earlier version of %s: %w", rel, err)
		}
		if len(events) > len(head) || (len(events) > 0 && !reflect.DeepEqual(head[:len(events)], events)) {
			return areaIdentity{}, true, fmt.Errorf("%s at memory HEAD drops or rewrites events of an earlier version: the log only grows; restore it from the memory history", rel)
		}
	}
	ident, err := replayObservations(head)
	return ident, true, err
}

// areaHistoryBlobs reads all unique historical log objects in one Git process. Every object
// is still compared with HEAD, including versions on merge parents dropped by the merge.
func areaHistoryBlobs(dir string, oids []string) ([][]byte, error) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), "cat-file", "--batch")...)
	cmd.Dir, cmd.Env = dir, commandEnv(nil)
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("observation log history: cat-file: %w", err)
	}
	r := bufio.NewReader(bytes.NewReader(b))
	var out [][]byte
	for _, oid := range oids {
		line, err := r.ReadString('\n')
		fields := strings.Fields(line)
		if err != nil || len(fields) != 3 || fields[0] != oid || fields[1] != "blob" {
			return nil, errors.New("observation log history: invalid cat-file header")
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 0 || n > len(b) {
			return nil, errors.New("observation log history: invalid blob size")
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("observation log history: %w", err)
		}
		if end, err := r.ReadByte(); err != nil || end != '\n' {
			return nil, errors.New("observation log history: truncated blob")
		}
		out = append(out, body)
	}
	return out, nil
}

// areaFront is the front matter of area.md.
type areaFront struct {
	Title string   `yaml:"title"`
	Paths []string `yaml:"paths"`
}

func (f areaFront) validate() error {
	if err := checkDisplayLine(f.Title, 120); err != nil {
		return fmt.Errorf("area title: %w", err)
	}
	if len(f.Paths) < 1 || len(f.Paths) > 20 {
		return errors.New("area paths must have 1 through 20 entries")
	}
	for _, p := range f.Paths {
		if err := checkSpecPath(p); err != nil {
			return err
		}
		if err := checkDisplayLine(p, 500); err != nil {
			return fmt.Errorf("area path: %w", err)
		}
	}
	return checkNoSecrets("area front matter", []byte(f.Title+"\n"+strings.Join(f.Paths, "\n")))
}

// checkAreaProse accepts the area's description: display Markdown with no criteria section and
// no criterion-like text, so it can become a spec body later.
func checkAreaProse(prose string) error {
	if err := checkDisplayMarkdown(prose, 65536); err != nil {
		return fmt.Errorf("area description: %w", err)
	}
	if _, present, err := parseCriteria(prose); err != nil || present {
		return errors.New(`area description must not hold "## Acceptance criteria" or "AC-<n>:"; aeh writes that section`)
	}
	return checkNoSecrets("area description", []byte(prose))
}

// parseAreaFile splits area.md into front matter, prose and the criteria section aeh owns.
func parseAreaFile(text string) (areaFront, string, string, error) {
	var f areaFront
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return f, "", "", errors.New("area.md must start with front matter")
	}
	fm, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return f, "", "", errors.New("area.md front matter must end with ---")
	}
	if err := checkPlainYAML([]byte(fm)); err != nil {
		return f, "", "", fmt.Errorf("area.md front matter: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, "", "", errors.New("area.md front matter must hold exactly title and paths")
	}
	if err := f.validate(); err != nil {
		return f, "", "", err
	}
	heading := criteriaHeading + "\n"
	var prose, section string
	if strings.HasPrefix(body, heading) {
		section = body
	} else if i := strings.Index(body, "\n"+heading); i >= 0 {
		prose, section = body[:i+1], body[i+1:]
	} else {
		return f, "", "", fmt.Errorf("area.md must end with the %q section aeh writes", criteriaHeading)
	}
	return f, prose, section, nil
}

func renderAreaFile(f areaFront, prose string, a areaIdentity) ([]byte, error) {
	fm, err := yaml.Marshal(f)
	if err != nil {
		return nil, err
	}
	if prose != "" && !strings.HasSuffix(prose, "\n") {
		prose += "\n"
	}
	return []byte("---\n" + string(fm) + "---\n" + prose + renderAreaCriteria(a)), nil
}

// renderAreaCriteria lists active observations as "(observed)" and confirmed ones as plain
// criteria, by ID. Retired IDs are gone for good.
func renderAreaCriteria(a areaIdentity) string {
	var b strings.Builder
	b.WriteString(criteriaHeading + "\n")
	for _, o := range a.sorted() {
		switch o.Status {
		case ObservationActive:
			fmt.Fprintf(&b, "- AC-%d (observed): %s\n", o.ID, o.Text)
		case ObservationConfirmed:
			fmt.Fprintf(&b, "- AC-%d: %s\n", o.ID, o.Text)
		}
	}
	return b.String()
}

// renderDefects lists the observations the owner called bugs: the history of the area.
func renderDefects(feature string, a areaIdentity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Known defects of %s\n\nGenerated by aeh from the observation log: behavior the owner called a bug while adopting this area. A fix comes as a new criterion in a later approved version.\n\n", feature)
	n := 0
	for _, o := range a.sorted() {
		if o.Decision == DecisionBug {
			fmt.Fprintf(&b, "- AC-%d: %s (observed at %s, reported %s)\n", o.ID, o.Text, o.CodeSHA[:7], o.Date)
			n++
		}
	}
	if n == 0 {
		b.WriteString("None recorded.\n")
	}
	return b.String()
}

// reconcileArea checks that the generated parts of the area still read as the log says.
func reconcileArea(memDir, feature string, a areaIdentity) (areaFront, string, error) {
	rel := areaRel(feature, "area.md")
	text, ok := gitBlob(memDir, "HEAD", rel)
	if !ok {
		return areaFront{}, "", fmt.Errorf("%s is missing from memory; restore it from the memory history", rel)
	}
	f, prose, section, err := parseAreaFile(text)
	if err != nil {
		return f, "", fmt.Errorf("%s: %w", rel, err)
	}
	if err := checkAreaProse(prose); err != nil {
		return f, "", fmt.Errorf("%s: %w", rel, err)
	}
	if want := renderAreaCriteria(a); section != want {
		got, exp := strings.Split(section, "\n"), strings.Split(want, "\n")
		line := 0
		for line < len(got) && line < len(exp) && got[line] == exp[line] {
			line++
		}
		return f, "", fmt.Errorf("%s: line %d of the acceptance criteria section does not match the observation log; aeh writes that section from the log. Restore the file in memory (git checkout) and record changes with aeh adopt confirm; a criterion is never added or changed by hand", rel, line+1)
	}
	if got, _ := gitBlob(memDir, "HEAD", areaRel(feature, "defects.md")); got != renderDefects(feature, a) {
		return f, "", fmt.Errorf("%s does not match the observation log; restore it in memory (git checkout); aeh writes it", areaRel(feature, "defects.md"))
	}
	return f, prose, nil
}

// adoptState is the local record of one adopted area, in the main checkout's
// .aeh/adopt/<feature>/state.json.
type adoptState struct {
	Version      int              `json:"version"`
	Feature      string           `json:"feature"`
	Pending      *pendingImport   `json:"pending,omitempty"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Attempts     []AdoptAttempt   `json:"attempts,omitempty"`
	Inspection   *AdoptInspection `json:"inspection,omitempty"`
	Current      *AdoptInspection `json:"current,omitempty"`
	Next         *AdoptInspection `json:"next,omitempty"`
	Promotion    *DiscoveryAction `json:"promotion,omitempty"`
	PromotedSpec string           `json:"promoted_spec_sha256,omitempty"` // exact promoted bytes, checked against memory HEAD
	Suggested    []byte           `json:"suggested,omitempty"`
	Input        string           `json:"input_sha256,omitempty"`
	LadderFrom   int              `json:"ladder_from,omitempty"`
	RunStatus    string           `json:"run_status,omitempty"`
}

// adoptSession carries one adoption command on one area.
type adoptSession struct {
	repo               Repo
	memDir             string
	feature            string
	path               string
	state              adoptState
	resumedInspections map[string]bool
}

func (a *adoptSession) session() string {
	if a.feature == "" {
		return "adopt/system"
	}
	return "adopt/" + a.feature
}

func (a *adoptSession) save() error {
	a.state.UpdatedAt = time.Now().UTC()
	return writePrivateJSON(a.path, a.state)
}

// loadAdoptSession reads an area's local state. The caller holds the project lock.
func loadAdoptSession(r Repo, feature string) (*adoptSession, error) {
	if err := checkSlug(feature); err != nil {
		return nil, fmt.Errorf("adopt: %w", err)
	}
	return loadAdoptState(r, feature)
}

// An empty feature is the inventory session, stored beside the area directories.
func loadAdoptState(r Repo, feature string) (*adoptSession, error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, err
	}
	r = Repo{Root: main}
	memory, err := LoadMemory(r)
	if err != nil {
		return nil, err
	}
	if !memory.Present {
		return nil, errors.New("memory is not initialized; run: aeh memory init")
	}
	dir := filepath.Join(main, ".aeh", "adopt", feature)
	if err := rejectLink(dir); err != nil {
		return nil, fmt.Errorf("adopt %s: %w", feature, err)
	}
	a := &adoptSession{repo: r, memDir: memory.Dir, feature: feature, path: filepath.Join(dir, "state.json"),
		state: adoptState{Version: adoptStateVersion, Feature: feature}}
	b, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a.state); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("adopt %s: state: %w", feature, err)
	}
	if a.state.Feature != feature || a.state.Version != adoptStateVersion {
		return nil, fmt.Errorf("adopt %s: unexpected state for %q, version %d", feature, a.state.Feature, a.state.Version)
	}
	return a, nil
}

// openAdopt takes the project lock and loads the area. The caller calls unlock.
func openAdopt(r Repo, feature string) (*adoptSession, func(), error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := lockLiveProject(Repo{Root: main})
	if err != nil {
		return nil, nil, err
	}
	a, err := loadAdoptSession(r, feature)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return a, unlock, nil
}

// resume finishes an interrupted step, as every adoption command does first.
func (a *adoptSession) resume() error {
	if err := recoverPending(a.memDir, a.session(), a.state.Pending); err != nil {
		return fmt.Errorf("adopt %s: %w", a.feature, err)
	}
	return a.finishPending()
}

func (a *adoptSession) finishPending() error {
	if a.state.Pending == nil {
		return a.writeSuggested()
	}
	if err := finishImport(a.repo, a.memDir, a.session(), a.state.Pending, a.save); err != nil {
		return fmt.Errorf("adopt %s: %w", a.feature, err)
	}
	a.state.Pending = nil
	if err := a.save(); err != nil {
		return err
	}
	return a.writeSuggested()
}

// record saves a pending batch with the state, then imports it.
func (a *adoptSession) record(files map[string][]byte, subject, log string) error {
	p, err := newPendingImport(a.memDir, files, subject, log)
	if err != nil {
		return fmt.Errorf("adopt %s: %w", a.feature, err)
	}
	a.state.Pending = p
	if err := a.save(); err != nil {
		return err
	}
	return a.finishPending()
}

// areaFiles renders every file of an area for a log.
func (a *adoptSession) areaFiles(f areaFront, prose string, ident areaIdentity) (map[string][]byte, error) {
	logBytes, err := yaml.Marshal(ident.Events)
	if err != nil {
		return nil, err
	}
	area, err := renderAreaFile(f, prose, ident)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		areaRel(a.feature, "observations.log.yaml"): logBytes,
		areaRel(a.feature, "area.md"):               area,
		areaRel(a.feature, "defects.md"):            []byte(renderDefects(a.feature, ident)),
	}, nil
}

// areaDraft describes a new area: its title, paths and prose.
type areaDraft struct {
	Front areaFront
	Prose string
}

// observationInput is one observation an agent reports. ID 0 is a new observation; an ID of
// an active observation re-observes it.
type observationInput struct {
	ID        int
	Text      string
	Locations []string
}

// recordAreaObservations records observations of an area made at codeSHA. A new area needs a
// draft; an existing one keeps the owner's front matter and prose. The caller holds the
// project lock.
func recordAreaObservations(r Repo, feature string, create *areaDraft, inputs []observationInput, codeSHA string) (_ areaIdentity, err error) {
	defer func() { err = redactErr(err) }()
	a, err := loadAdoptSession(r, feature)
	if err != nil {
		return areaIdentity{}, err
	}
	if err := a.resume(); err != nil {
		return areaIdentity{}, err
	}
	if !isFullSHA(codeSHA) {
		return areaIdentity{}, fmt.Errorf("adopt %s: observed commit %q is not a full SHA", feature, codeSHA)
	}
	ident, exists, err := loadAreaIdentity(a.memDir, feature)
	if err != nil {
		return ident, fmt.Errorf("adopt %s: %w", feature, err)
	}
	var front areaFront
	var prose string
	_, areaExists := gitBlob(a.memDir, "HEAD", areaRel(feature, "area.md"))
	switch {
	case exists || areaExists:
		if front, prose, err = reconcileArea(a.memDir, feature, ident); err != nil {
			return ident, fmt.Errorf("adopt %s: %w", feature, err)
		}
	case create == nil:
		return ident, fmt.Errorf("adopt %s: the area does not exist yet", feature)
	default:
		front, prose = create.Front, create.Prose
		if err := front.validate(); err != nil {
			return ident, fmt.Errorf("adopt %s: %w", feature, err)
		}
		if err := checkAreaProse(prose); err != nil {
			return ident, fmt.Errorf("adopt %s: %w", feature, err)
		}
	}
	today := memoryNow().UTC().Format("2006-01-02")
	events := append([]observationEvent{}, ident.Events...)
	high := ident.High
	added := 0
	for i, in := range inputs {
		if err := checkObservationText(in.Text); err != nil {
			return ident, fmt.Errorf("adopt %s: observation %d: %w", feature, i+1, err)
		}
		e := observationEvent{Event: "observe", ID: in.ID, Text: in.Text, TextSHA: observationDigest(in.Text), CodeSHA: codeSHA, Locations: in.Locations, Date: today}
		if in.ID == 0 {
			high++
			e.ID = high
		} else if o := ident.Obs[in.ID]; o == nil || o.Status != ObservationActive {
			return ident, fmt.Errorf("adopt %s: observation %d: AC-%d is not an active observation", feature, i+1, in.ID)
		} else if o.TextSHA == e.TextSHA && o.CodeSHA == codeSHA && reflect.DeepEqual(o.Locations, in.Locations) {
			continue // nothing new about it
		}
		events = append(events, e)
		added++
	}
	next, err := replayObservations(events)
	if err != nil {
		return ident, fmt.Errorf("adopt %s: %w", feature, err)
	}
	if added == 0 && exists {
		return next, nil
	}
	files, err := a.areaFiles(front, prose, next)
	if err != nil {
		return ident, err
	}
	if err := a.record(files, fmt.Sprintf("adopt %s: %d observation(s) at %s", feature, added, codeSHA[:7]), fmt.Sprintf("observe %d at %s", added, codeSHA[:7])); err != nil {
		return ident, err
	}
	return next, nil
}

// AdoptArea is what aeh adopt show prints.
type AdoptArea struct {
	Feature      string        `json:"feature"`
	Title        string        `json:"title"`
	Paths        []string      `json:"paths"`
	Observations []Observation `json:"observations"`
}

// LoadAdoptArea reads an area and checks it against its log.
func LoadAdoptArea(r Repo, feature string) (_ AdoptArea, err error) {
	defer func() { err = redactErr(err) }()
	a, unlock, err := openAdopt(r, feature)
	if err != nil {
		return AdoptArea{}, err
	}
	defer unlock()
	defer a.reportCommand(&err)
	if err := resumeAdoptImports(a.repo); err != nil {
		return AdoptArea{}, err
	}
	ident, exists, err := loadAreaIdentity(a.memDir, feature)
	if err != nil {
		return AdoptArea{}, redactErr(fmt.Errorf("adopt %s: %w", feature, err))
	}
	if !exists {
		return AdoptArea{}, fmt.Errorf("area %s has no observations; adopt it first", feature)
	}
	f, _, err := reconcileArea(a.memDir, feature, ident)
	if err != nil {
		return AdoptArea{}, redactErr(fmt.Errorf("adopt %s: %w", feature, err))
	}
	return AdoptArea{Feature: feature, Title: f.Title, Paths: f.Paths, Observations: ident.sorted()}, nil
}

var observationRef = regexp.MustCompile(`^AC-([1-9][0-9]*)$`)

// AdoptConfirm records the owner's decision on an active observation. seen is the token the
// owner saw (aeh adopt show prints it): when the observation changed since, nothing is recorded
// and the error shows the new text. The same decision again is a no-op; another one is an error.
func AdoptConfirm(r Repo, feature, ref, decision, seen string) (_ Observation, err error) {
	defer func() { err = redactErr(err) }()
	m := observationRef.FindStringSubmatch(ref)
	if m == nil {
		return Observation{}, fmt.Errorf("observation %q must look like AC-3", ref)
	}
	id, _ := strconv.Atoi(m[1])
	switch decision {
	case DecisionKeep, DecisionBug, DecisionDrop:
	default:
		return Observation{}, fmt.Errorf("decision %q must be keep, bug or drop", decision)
	}
	a, unlock, err := openAdopt(r, feature)
	if err != nil {
		return Observation{}, err
	}
	defer unlock()
	defer a.reportCommand(&err)
	if err := resumeAdoptImports(a.repo); err != nil {
		return Observation{}, err
	}
	ident, exists, err := loadAreaIdentity(a.memDir, feature)
	if err != nil {
		return Observation{}, fmt.Errorf("adopt %s: %w", feature, err)
	}
	if !exists {
		return Observation{}, fmt.Errorf("area %s has no observations; adopt it first", feature)
	}
	front, prose, err := reconcileArea(a.memDir, feature, ident)
	if err != nil {
		return Observation{}, fmt.Errorf("adopt %s: %w", feature, err)
	}
	o := ident.Obs[id]
	if o == nil {
		return Observation{}, fmt.Errorf("area %s has no observation AC-%d", feature, id)
	}
	if o.Status != ObservationActive {
		if o.Decision == decision {
			return *o, nil
		}
		return *o, fmt.Errorf("AC-%d is already decided (%s); a different decision is not allowed", id, o.Decision)
	}
	if seen != o.Token() {
		return *o, fmt.Errorf("AC-%d is not the observation you saw (token %q). It now reads, as observed at %s:\n  %s\nConfirm it with --seen %s", id, seen, o.CodeSHA[:7], o.Text, o.Token())
	}
	events := append(append([]observationEvent{}, ident.Events...), observationEvent{Event: "confirm", ID: id, Text: o.Text,
		TextSHA: o.TextSHA, CodeSHA: o.CodeSHA, Decision: decision, Date: memoryNow().UTC().Format("2006-01-02")})
	next, err := replayObservations(events)
	if err != nil {
		return *o, err
	}
	files, err := a.areaFiles(front, prose, next)
	if err != nil {
		return *o, err
	}
	if err := a.record(files, fmt.Sprintf("adopt %s: AC-%d %s", feature, id, decision), fmt.Sprintf("confirm AC-%d %s", id, decision)); err != nil {
		return *o, err
	}
	return *next.Obs[id], nil
}
