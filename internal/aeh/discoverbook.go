package aeh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DiscoveryBookReview is advisory evidence about one immutable book, never an approval.
type DiscoveryBookReview struct {
	Profile  string              `json:"profile,omitempty"`
	Provider string              `json:"provider,omitempty"`
	Prompt   string              `json:"prompt_sha256,omitempty"`
	Book     string              `json:"book_sha256"`
	Status   string              `json:"status"`
	Findings []DiscoveryQuestion `json:"findings,omitempty"`
	Feedback string              `json:"feedback,omitempty"`
	Stale    bool                `json:"-"` // computed against memory HEAD for show/report
}

// pendingBook identifies the saved author snapshot before any reviewer is started.
type pendingBook struct {
	Attempt int                  `json:"attempt"`
	Round   int                  `json:"round"`
	Review  *DiscoveryBookReview `json:"review,omitempty"`
	Running bool                 `json:"running,omitempty"`
}

// DiscoveryAction records an owner's explicit promotion or document acceptance.
type DiscoveryAction struct {
	Name    string   `json:"name"`
	Target  string   `json:"target"`
	Content string   `json:"content_sha256"`
	Date    string   `json:"date"`
	Paths   []string `json:"paths,omitempty"` // promoted path claims survive later owner edits
}

type draftFeature struct {
	Title string   `yaml:"title"`
	Paths []string `yaml:"paths"`
	Body  string   `yaml:"-"`
}

func parseDraftFeature(b []byte) (draftFeature, error) {
	var f draftFeature
	if err := checkNoSecrets("draft", b); err != nil {
		return f, err
	}
	if len(b) > discoveryDocBytes {
		return f, errors.New("draft exceeds the byte limit")
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if err := checkDisplayMarkdown(text, discoveryDocBytes); err != nil {
		return f, err
	}
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return f, errors.New("draft must start with YAML front matter")
	}
	fm, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return f, errors.New("draft front matter must end with ---")
	}
	if err := checkPlainYAML([]byte(fm)); err != nil {
		return f, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return f, errors.New("invalid draft front matter")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || len(doc.Content[0].Content) != 4 {
		return f, errors.New("draft front matter must contain exactly title and paths")
	}
	nodes := doc.Content[0].Content
	for i := 0; i < len(nodes); i += 2 {
		switch nodes[i].Value {
		case "title":
			if nodes[i+1].Kind != yaml.ScalarNode || nodes[i+1].Tag != "!!str" {
				return f, errors.New("draft title must be a string")
			}
		case "paths":
			if nodes[i+1].Kind != yaml.SequenceNode {
				return f, errors.New("draft paths must be a list of strings")
			}
			for _, entry := range nodes[i+1].Content {
				if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
					return f, errors.New("draft paths must be a list of strings")
				}
			}
		default:
			return f, errors.New("draft front matter must contain exactly title and paths")
		}
	}
	dec := yaml.NewDecoder(strings.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, errors.New("draft front matter must contain exactly title and paths")
	}
	if err := checkDisplayLine(f.Title, 200); err != nil {
		return f, fmt.Errorf("draft title: %w", err)
	}
	if len(f.Paths) < 1 || len(f.Paths) > 20 {
		return f, errors.New("draft paths must have 1 through 20 entries")
	}
	for _, p := range f.Paths {
		if err := checkSpecPath(p); err != nil {
			return f, err
		}
		if err := checkDisplayLine(p, discoveryDocBytes); err != nil {
			return f, err
		}
	}
	// Scan decoded values too: YAML escapes must not bypass either policy.
	if err := checkNoSecrets("draft front matter", []byte(f.Title+"\n"+strings.Join(f.Paths, "\n"))); err != nil {
		return f, err
	}
	crit, present, err := parseCriteria(body)
	if err != nil {
		return f, err
	}
	if !present || len(crit) == 0 {
		return f, errors.New("draft needs a non-empty Acceptance criteria section")
	}
	for i, c := range crit {
		if c.ID != i+1 {
			return f, errors.New("draft criteria must be numbered 1 through n without gaps")
		}
	}
	f.Body = body
	return f, nil
}

func (d *discoverySession) validateBookFeatures(f frozenOutput) error {
	var memory MemorySnapshot
	if d.repo.Root != "" {
		var err error
		memory, err = LoadMemory(d.repo)
		if err != nil {
			return err
		}
	}
	drafts := map[string]draftFeature{}
	for _, rel := range sortedKeys(f.Files) {
		if !strings.HasPrefix(rel, "features/") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(rel, "features/"), ".md")
		if _, exists := memory.Spec(slug); exists {
			return fmt.Errorf("draft %s is an existing feature; changes to features require stage 5", slug)
		}
		draft, err := parseDraftFeature(f.Files[rel])
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		for _, other := range sortedDraftNames(drafts) {
			if pathsOverlap(draft.Paths, drafts[other].Paths) {
				return fmt.Errorf("draft paths overlap: %s and %s", slug, other)
			}
		}
		drafts[slug] = draft
	}
	return nil
}

func sortedDraftNames(m map[string]draftFeature) []string {
	files := map[string][]byte{}
	for k := range m {
		files[k] = nil
	}
	return sortedKeys(files)
}

// bookDigest uses final draft-relative paths and bytes, including rebuilt mockups.
func (d *discoverySession) bookDigest(files map[string][]byte) string {
	relative := map[string][]byte{}
	for rel, b := range files {
		relative[strings.TrimPrefix(rel, d.memRel("drafts/"))] = b
	}
	return outputDigest("discovery-drafts", relative)
}

func (d *discoverySession) currentBookDigest() (string, error) {
	prefix := d.memRel("drafts/")
	paths, err := gitRaw(d.memDir, "ls-tree", "-r", "--name-only", "-z", "HEAD", "--", prefix)
	if err != nil {
		return "", err
	}
	files := map[string][]byte{}
	for _, rel := range strings.Split(paths, "\x00") {
		if rel == "" {
			continue
		}
		b, ok := gitBlob(d.memDir, "HEAD", rel)
		if !ok {
			return "", errors.New("cannot read draft from memory HEAD")
		}
		files[rel] = []byte(b)
	}
	return d.bookDigest(files), nil
}

func bookReviewManifest() outputManifest {
	return outputManifest{Name: "discovery-book-review", Rules: []outputRule{
		{Pattern: "findings.yaml", Kind: outputYAML, MaxBytes: discoveryDocBytes, MaxCount: 1},
	}, MaxFiles: 1, MaxTotal: discoveryDocBytes}
}

func parseBookFindings(b []byte, round, max int, attribution string) ([]DiscoveryQuestion, error) {
	if b == nil {
		return nil, nil
	}
	if err := checkPlainYAML(b); err != nil {
		return nil, err
	}
	if err := checkNoSecrets("findings.yaml", b); err != nil {
		return nil, err
	}
	var f struct {
		Findings []struct {
			Text string `yaml:"text"`
			Why  string `yaml:"why"`
		} `yaml:"findings"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("findings.yaml must contain findings with text and why only")
	}
	if f.Findings == nil || len(f.Findings) > max {
		return nil, fmt.Errorf("findings.yaml needs a findings list with at most %d entries", max)
	}
	var questions []DiscoveryQuestion
	for i, finding := range f.Findings {
		for _, value := range []string{finding.Text, finding.Why} {
			if err := checkDisplayLine(value, discoveryLineBytes); err != nil {
				return nil, err
			}
			if err := checkNoSecrets("review finding", []byte(value)); err != nil {
				return nil, err
			}
		}
		questions = append(questions, DiscoveryQuestion{ID: fmt.Sprintf("R%d-Q%d", round, i+1), Text: finding.Text, Why: finding.Why, Attribution: attribution})
	}
	return questions, nil
}

func (d *discoverySession) startBook(ctx context.Context, round int, a *DiscoveryAttempt) error {
	d.state.Book = &pendingBook{Attempt: a.N, Round: round}
	d.state.Phase = DiscoveryReview
	if err := d.save(); err != nil {
		return err
	}
	return d.resumeBook(ctx)
}

func (d *discoverySession) resumeBook(ctx context.Context) error {
	p := d.state.Book
	if p == nil {
		return errors.New("pending book is missing")
	}
	if p.Attempt < 1 || p.Attempt > len(d.state.Attempts) {
		return errors.New("pending book author attempt is missing")
	}
	f, err := loadFrozen(filepath.Join(d.dir, "snapshots", fmt.Sprintf("a%d.json", p.Attempt)), bookManifest(d.dcfg))
	if err != nil {
		return err
	}
	if f.Digest != d.state.Attempts[p.Attempt-1].Snapshot {
		return errors.New("pending book differs from the recorded author snapshot")
	}
	files, err := d.bookFiles(f)
	if err != nil {
		return err
	}
	if p.Review == nil {
		p.Review = &DiscoveryBookReview{Book: d.bookDigest(files), Status: "not_run"}
	}
	if p.Review.Book != d.bookDigest(files) {
		return errors.New("pending review book digest differs from snapshot")
	}
	// A stopped process cannot be reattached. Retry once on this exact snapshot on rerun.
	retrying := p.Running || p.Review.Status == "interrupted"
	if retrying {
		p.Running = false
		p.Review.Status = "not_run"
		p.Review.Feedback, p.Review.Findings = "", nil
	}
	if p.Review.Status == "not_run" {
		if err := d.reviewBook(ctx, p, files); err != nil {
			// The first cancellation leaves a resumable book. If that recovery cannot
			// complete either, preserve interrupted evidence and import it anyway.
			if !retrying || p.Review.Status != "interrupted" {
				return err
			}
		}
	}
	if err := d.save(); err != nil {
		return err
	}
	out := &discoveryOutput{kind: roundBook, snapshot: f, files: files, questions: p.Review.Findings}
	review := p.Review
	d.state.Book = nil
	return d.acceptReviewed(p.Round, &d.state.Attempts[p.Attempt-1], out, review)
}

func (d *discoverySession) reviewBook(ctx context.Context, p *pendingBook, files map[string][]byte) error {
	v := p.Review
	text, templateErr := loadPrompt(d.repo, "review-book.md")
	if templateErr == nil {
		v.Prompt = fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
	}
	profile, err := d.cfg.reviewerFor(RoleQA, d.state.Attempts[p.Attempt-1].Profile)
	if err != nil {
		v.Status, v.Feedback = "unavailable", discoveryDiagnostic(err.Error())
		return nil
	}
	v.Profile, v.Provider = discoveryDiagnostic(profile), discoveryDiagnostic(d.cfg.Profiles[profile].Provider)
	if templateErr != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(templateErr.Error())
		return nil
	}
	var book strings.Builder
	for _, rel := range sortedKeys(files) {
		fmt.Fprintf(&book, "File: %s\n%s\n\n", strings.TrimPrefix(rel, d.memRel("drafts/")), files[rel])
	}
	prompt, err := renderPrompt("review-book.md", text, promptData{Book: book.String(), MaxQuestions: d.dcfg.MaxQuestions})
	if err != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(err.Error())
		return nil
	}
	outDir := d.repo.WorktreePath(fmt.Sprintf("discover-%s-review-%d", d.state.Slug, p.Round))
	if err := os.RemoveAll(outDir); err != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(err.Error())
		return nil
	}
	root, err := openOutputDir(outDir)
	if err != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(err.Error())
		return nil
	}
	defer func() { root.Close(); _ = os.RemoveAll(outDir) }()
	p.Running = true
	if err := d.save(); err != nil {
		return err
	}
	runDir := d.repo.RunDir("discover-"+d.state.Slug, fmt.Sprintf("review%d", p.Round))
	outPath, runErr := runAgent(ctx, d.cfg.Profiles[profile], d.cfg.PassEnv, outDir, prompt, runDir, "review-book")
	p.Running = false
	if runErr != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(runErr.Error())
		if ctx.Err() != nil || uncertainExit(runErr) {
			v.Status = "interrupted"
		}
		if ctx.Err() != nil {
			if err := d.save(); err != nil {
				return err
			}
			return ctx.Err()
		}
		return nil
	}
	frozen, err := freezeOutput(root, bookReviewManifest())
	if err != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(err.Error())
		return nil
	}
	output, err := os.ReadFile(outPath)
	if err != nil {
		v.Status, v.Feedback = "invalid", discoveryDiagnostic(err.Error())
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	switch strings.TrimSpace(lines[len(lines)-1]) {
	case "BOOK-REVIEW: APPROVE":
		v.Status = "approve"
	case "BOOK-REVIEW: REJECT":
		v.Status = "reject"
	default:
		v.Status, v.Feedback = "invalid", "review must end with BOOK-REVIEW: APPROVE or BOOK-REVIEW: REJECT"
		return nil
	}
	attribution := "Reviewer " + discoveryDiagnostic(v.Profile) + " (" + discoveryDiagnostic(v.Provider) + ")"
	v.Findings, err = parseBookFindings(frozen.Files["findings.yaml"], p.Round, d.dcfg.MaxQuestions, attribution)
	if err != nil {
		v.Status, v.Feedback, v.Findings = "invalid", discoveryDiagnostic(err.Error()), nil
	}
	return nil
}

// discoveryDiagnostic first redacts secrets, then removes active Markdown syntax from diagnostics.
// Diagnostics may include a rejected agent filename or parser error, which never passed F4.
func discoveryDiagnostic(s string) string {
	s = redactDiagnostic(s)
	return strings.NewReplacer("<", "?", ">", "?", "&", "?", "[", "?", "]", "?", "`", "?", "~", "?").Replace(s)
}

func (d *discoverySession) draftAtHead(name string) ([]byte, error) {
	text, ok := gitBlob(d.memDir, "HEAD", d.memRel("drafts/"+name))
	if !ok {
		return nil, fmt.Errorf("draft %s is missing from memory HEAD; edit and commit the draft in memory", name)
	}
	return []byte(text), nil
}

// DiscoverPromote creates a real, unapproved spec from the owner's committed draft.
func DiscoverPromote(r Repo, slug, feature string) (_ DiscoveryState, err error) {
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
	if d.state.Phase != DiscoveryDraft {
		return *d.state, errors.New("promote requires a draft_ready session")
	}
	if err := checkSlug(feature); err != nil {
		return *d.state, err
	}
	b, err := d.draftAtHead("features/" + feature + ".md")
	if err != nil {
		return *d.state, err
	}
	f, err := parseDraftFeature(b)
	if err != nil {
		return *d.state, err
	}
	digest := outputDigest("discovery-promotion", map[string][]byte{feature: b})
	for _, a := range d.state.Promotions {
		if a.Name == feature && a.Content == digest {
			return *d.state, nil
		}
		if a.Name != feature && pathsOverlap(f.Paths, a.Paths) {
			return *d.state, fmt.Errorf("draft %s paths overlap promoted draft %s; edit and commit the draft in memory", feature, a.Name)
		}
	}
	memory, err := LoadMemory(d.repo)
	if err != nil {
		return *d.state, err
	}
	for _, spec := range memory.Specs {
		if spec.Feature == feature {
			return *d.state, fmt.Errorf("feature %s already exists; edit and commit the draft in memory", feature)
		}
		if pathsOverlap(f.Paths, spec.Paths) {
			return *d.state, fmt.Errorf("draft %s paths overlap feature %s; edit and commit the draft in memory", feature, spec.Feature)
		}
	}
	front, err := yaml.Marshal(specFront{Feature: feature, Title: f.Title, Version: "0.1.0", Paths: f.Paths})
	if err != nil {
		return *d.state, err
	}
	date := memoryNow().UTC().Format("2006-01-02")
	text := "---\n" + string(front) + "---\n" + strings.TrimRight(f.Body, "\n") + "\n\n## Changelog\n- 0.1.0 (" + date + ") promoted from discovery " + slug + "; not implemented yet.\n"
	spec, err := parseSpec(text)
	if err != nil {
		return *d.state, err
	}
	if _, _, err := spec.Criteria(); err != nil {
		return *d.state, err
	}
	action := DiscoveryAction{Name: feature, Target: specRel(feature), Content: digest, Date: date, Paths: f.Paths}
	d.state.Promotions = append(d.state.Promotions, action)
	return *d.state, d.recordAction(action, true, []byte(text))
}

// DiscoverAccept records vision/data as advisory documents, outside every spec/run digest.
func DiscoverAccept(r Repo, slug, document string) (_ DiscoveryState, err error) {
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
	if d.state.Phase != DiscoveryDraft {
		return *d.state, errors.New("accept requires a draft_ready session")
	}
	if document != "vision" && document != "data" {
		return *d.state, errors.New("accept requires vision or data")
	}
	b, err := d.draftAtHead(document + ".md")
	if err != nil {
		return *d.state, err
	}
	if err := checkOwnerText("draft document", string(b), discoveryDocBytes); err != nil {
		return *d.state, err
	}
	digest := outputDigest("discovery-acceptance", map[string][]byte{document: b})
	for _, a := range d.state.Acceptances {
		if a.Name == document && a.Content == digest {
			return *d.state, nil
		}
	}
	target := "wiki/" + document + ".md"
	if _, exists := gitBlob(d.memDir, "HEAD", target); exists {
		target = "wiki/" + document + ".next.md"
	}
	action := DiscoveryAction{Name: document, Target: target, Content: digest, Date: memoryNow().UTC().Format("2006-01-02")}
	d.state.Acceptances = append(d.state.Acceptances, action)
	return *d.state, d.recordAction(action, false, b)
}

func (d *discoverySession) recordAction(a DiscoveryAction, promotion bool, b []byte) error {
	verb := "accept"
	if promotion {
		verb = "promote"
	}
	var record strings.Builder
	fmt.Fprintf(&record, "# Discovery %s\n\nOwner actions; these are not feature approvals.\n", d.state.Slug)
	for _, group := range []struct {
		verb    string
		actions []DiscoveryAction
	}{{"Promoted", d.state.Promotions}, {"Accepted", d.state.Acceptances}} {
		for _, action := range group.actions {
			fmt.Fprintf(&record, "\n- %s %s to %s (%s), content SHA-256 %s.\n", group.verb, action.Name, action.Target, action.Date, action.Content)
		}
	}
	return d.record(map[string][]byte{a.Target: b, d.memRel("record.md"): []byte(record.String())}, "discover "+d.state.Slug+": "+verb+" "+a.Name, verb+" "+a.Name+" to "+a.Target)
}

// DiscoverShow holds the same project lock as mutating commands and refreshes review validity.
func DiscoverShow(r Repo, slug string) (_ DiscoveryState, err error) {
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
	if err := d.refreshReview(); err != nil {
		return *d.state, err
	}
	return *d.state, nil
}

func (d *discoverySession) refreshReview() error {
	if d.state == nil {
		return nil
	}
	digest, err := d.currentBookDigest()
	if err != nil {
		return err
	}
	for i := range d.state.Rounds {
		if v := d.state.Rounds[i].Review; v != nil {
			v.Stale = v.Book != digest
		}
	}
	return nil
}
