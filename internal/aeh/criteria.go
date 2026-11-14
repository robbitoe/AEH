package aeh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// criteriaHeading opens the only section aeh plans from.
const criteriaHeading = "## Acceptance criteria"

// SpecCriterion is one acceptance criterion of a spec: "- AC-<ID>: <Text>". Text is normalized
// (runs of white space outside inline code become one space) so versions compare by meaning,
// not layout; inline code keeps its exact spacing.
type SpecCriterion struct {
	ID   int    `yaml:"id"`
	Text string `yaml:"text"`
}

var (
	criterionLine = regexp.MustCompile(`^- AC-([1-9][0-9]*): (.*)$`)
	observedLine  = regexp.MustCompile(`^- AC-[0-9]+ \(observed\):`)
	strayLine     = regexp.MustCompile(`(?i)^\s*(?:>\s*)*(?:[-*+]|[0-9]+[.)])\s+AC-[0-9]`)
	htmlStart     = regexp.MustCompile(`<[A-Za-z/!?]`)
	// A URI or email autolink, or an angle-bracketed link destination: Markdown, not HTML.
	// "<!--" is rejected before this exemption applies, so "<!--@x>" cannot pass as an email.
	notHTML = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9+.\-]{1,31}:[^\s<>]*>|<[A-Za-z0-9][A-Za-z0-9.+_\-]*@[A-Za-z0-9.\-]+>|\]\(<[^<>\n]*>|^\[[^\]]+\]:[ \t]*<[^<>\n]*>`)
)

// headingAt0 returns a column-0 ATX heading as its level and text (closing hashes removed),
// or level 0 when the line is not one.
func headingAt0(line string) (int, string) {
	n := len(line) - len(strings.TrimLeft(line, "#"))
	if n == 0 || n > 6 || (len(line) > n && line[n] != ' ' && line[n] != '\t') {
		return 0, ""
	}
	text := strings.TrimSpace(line[n:])
	if strings.Trim(text, "#") == "" {
		text = ""
	} else if i := strings.LastIndexAny(text, " \t"); i >= 0 && strings.Trim(text[i+1:], "#") == "" {
		text = strings.TrimSpace(text[:i])
	}
	return n, text
}

// codeSegment is a piece of one line: inline code (shown literally) or other text.
type codeSegment struct {
	text string
	code bool
}

// splitCodeSpans splits a line into inline code spans and the text around them. A span opens
// at an unescaped backtick run and closes at the next run of the same length; a backtick
// escaped with a backslash is literal text, as in CommonMark.
func splitCodeSpans(line string) []codeSegment {
	var out []codeSegment
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, codeSegment{text: b.String()})
			b.Reset()
		}
	}
	escaped := false
	for i := 0; i < len(line); {
		c := line[i]
		if c != '`' || escaped {
			escaped = c == '\\' && !escaped
			b.WriteByte(c)
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		end := -1
		for k := j; k < len(line); {
			if line[k] != '`' {
				k++
				continue
			}
			m := k
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-k == j-i {
				end = m
				break
			}
			k = m
		}
		if end < 0 {
			b.WriteString(line[i:j])
			i = j
			continue
		}
		flush()
		out = append(out, codeSegment{text: line[i:end], code: true})
		i = end
	}
	flush()
	return out
}

// normalizeCriterion collapses runs of white space outside inline code and trims the ends.
// Code spans stay byte for byte: their spacing can be the requirement.
func normalizeCriterion(text string) string {
	var b strings.Builder
	space := false
	put := func(s string) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteString(s)
	}
	for _, seg := range splitCodeSpans(text) {
		if seg.code {
			put(seg.text)
			continue
		}
		for _, r := range seg.text {
			if unicode.IsSpace(r) {
				space = true
				continue
			}
			put(string(r))
		}
	}
	return b.String()
}

// hasRawHTML reports raw HTML outside inline code. "<!--" counts anywhere, code spans
// included, because it is checked before any exemption.
func hasRawHTML(line string) bool {
	if strings.Contains(line, "<!--") {
		return true
	}
	for _, seg := range splitCodeSpans(line) {
		if !seg.code && htmlStart.MatchString(notHTML.ReplaceAllString(seg.text, "")) {
			return true
		}
	}
	return false
}

func isASCIIBlank(line string) bool { return strings.Trim(line, " \t") == "" }

// parseCriteria reads the "## Acceptance criteria" section of a spec body. present is false
// when the section is absent: such a spec can be approved, not planned.
//
// The section has a closed grammar, so what aeh plans is exactly what the owner reads in any
// Markdown renderer: it starts at the column-0 heading "## Acceptance criteria", ends at the
// next column-0 heading of level 1 or 2, and holds only criterion lines "- AC-<n>: <text>"
// (n a positive integer without leading zeros) and blank lines. Prose, quotes, nested or
// indented lines, sub-headings and fences belong outside it. In a spec that has the section,
// these are errors anywhere: a criterion-like list item outside the section, a fence that is
// not at column 0 (it could belong to a list item), raw HTML or an HTML comment outside code
// spans (it can hide text), and a lone carriage return (Markdown reads it as a line break).
func parseCriteria(body string) (crit []SpecCriterion, present bool, err error) {
	var f codeFence
	in, sections := false, 0
	issue, issueAt := "", 0
	note := func(n int, msg string) {
		if issueAt == 0 {
			issue, issueAt = msg, n
		}
	}
	seen := map[int]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		n := i + 1
		if strings.Contains(line, "\r") {
			note(n, "a lone carriage return is a line break in Markdown; use LF or CRLF line endings")
		}
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != line && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			note(n, "fences must start at column 0 in a spec with acceptance criteria")
		}
		if f.step(line) {
			continue
		}
		if hasRawHTML(line) {
			note(n, "raw HTML and HTML comments are not allowed in a spec with acceptance criteria, even in code spans for comments; they can hide text (write it in a fence)")
		}
		if level, text := headingAt0(line); level > 0 {
			if level <= 2 {
				in = level == 2 && text == strings.TrimPrefix(criteriaHeading, "## ")
				if in {
					sections++
				}
			} else if in {
				note(n, "sub-headings are not allowed in the acceptance criteria section")
			}
			continue
		}
		if !in {
			if strayLine.MatchString(line) || observedLine.MatchString(line) {
				note(n, fmt.Sprintf("a criterion outside the %q section would not be planned; move it into the section", criteriaHeading))
			}
			continue
		}
		if isASCIIBlank(line) {
			continue
		}
		if observedLine.MatchString(line) {
			note(n, "observed criteria are reserved for adopting existing systems")
			continue
		}
		m := criterionLine.FindStringSubmatch(line)
		if m == nil {
			note(n, `the acceptance criteria section holds only lines "- AC-<n>: <text>" (n from 1, no leading zeros) and blank lines; put prose before its heading`)
			continue
		}
		id, cerr := strconv.Atoi(m[1])
		if cerr != nil {
			note(n, fmt.Sprintf("AC-%s is too large", m[1]))
			continue
		}
		text := normalizeCriterion(m[2])
		switch {
		case text == "":
			note(n, fmt.Sprintf("AC-%d has no text", id))
		case seen[id]:
			note(n, fmt.Sprintf("AC-%d appears twice", id))
		default:
			seen[id] = true
			crit = append(crit, SpecCriterion{ID: id, Text: text})
		}
	}
	switch {
	case sections == 0:
		return nil, false, nil
	case issueAt > 0:
		return nil, true, fmt.Errorf("acceptance criteria line %d: %s", issueAt, issue)
	case sections > 1:
		return nil, true, fmt.Errorf("the spec has %d %q sections; keep one", sections, criteriaHeading)
	case len(crit) == 0:
		return nil, true, fmt.Errorf("the %q section has no criteria", criteriaHeading)
	}
	return crit, true, nil
}

// Implementation records that one approved content of a spec was implemented and accepted:
// which outcome, on which exact commit, and the criteria it covered with their sealed tests.
// Only the runner writes it, in the same memory commit as the accepted change's record.
type Implementation struct {
	Outcome      string                 `yaml:"outcome"`
	Approval     string                 `yaml:"approval_content_sha256"` // digest of the approved spec that was planned
	Code         string                 `yaml:"code_sha"`                // full SHA accepted on aeh/<outcome>
	Criteria     []ImplementedCriterion `yaml:"criteria"`                // every criterion of that approval, sorted by ID
	MaxCriterion int                    `yaml:"max_criterion"`           // highest criterion ID ever approved, retired ones included
}

// ImplementedCriterion is one criterion as implemented, with the sealed tests that check it.
type ImplementedCriterion struct {
	ID    int      `yaml:"id"`
	Text  string   `yaml:"text"`
	Tests []string `yaml:"tests"`
}

const implementedFile = "implemented.yaml"

func implementedRel(feature string) string {
	return "wiki/features/" + feature + "/" + implementedFile
}

// readImplementations reads a feature's implementation records, oldest first. A missing file
// means none.
func readImplementations(p string) ([]Implementation, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := checkConfigYAML(b); err != nil {
		return nil, fmt.Errorf("%s: %w", implementedFile, err)
	}
	var list []Implementation
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: %w", implementedFile, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: must hold exactly one YAML document", implementedFile)
	}
	high := 0
	for i, im := range list {
		if err := im.validate(); err != nil {
			return nil, fmt.Errorf("%s: entry %d: %w", implementedFile, i+1, err)
		}
		if im.MaxCriterion < high {
			return nil, fmt.Errorf("%s: entry %d: max_criterion went down from %d to %d", implementedFile, i+1, high, im.MaxCriterion)
		}
		high = im.MaxCriterion
	}
	return list, nil
}

func (im Implementation) validate() error {
	if !validID(im.Outcome) {
		return fmt.Errorf("invalid outcome %q", im.Outcome)
	}
	if !isSHA256Hex(im.Approval) {
		return errors.New("approval_content_sha256 must be a sha256 in hex")
	}
	if !isFullSHA(im.Code) {
		return fmt.Errorf("code_sha %q is not a full commit SHA", im.Code)
	}
	if len(im.Criteria) == 0 {
		return errors.New("no criteria")
	}
	for i, c := range im.Criteria {
		if c.ID < 1 || (i > 0 && c.ID <= im.Criteria[i-1].ID) {
			return errors.New("criteria IDs must be positive, unique and sorted")
		}
		if c.ID > im.MaxCriterion {
			return fmt.Errorf("AC-%d is above max_criterion %d", c.ID, im.MaxCriterion)
		}
		if c.Text == "" || c.Text != normalizeCriterion(c.Text) {
			return fmt.Errorf("AC-%d: text must be non-empty and normalized", c.ID)
		}
		if len(c.Tests) == 0 {
			return fmt.Errorf("AC-%d has no tests", c.ID)
		}
		for _, t := range c.Tests {
			if err := checkRelPath(t); err != nil {
				return fmt.Errorf("AC-%d: test %q: %w", c.ID, t, err)
			}
		}
	}
	return nil
}

// checkRelPath accepts a clean, relative, forward-slash file path inside the repository.
func checkRelPath(p string) error {
	switch {
	case p == "" || strings.ContainsAny(p, "\\*?[\x00") || strings.HasPrefix(p, "/") || strings.Contains(p, ":"):
		return errors.New("must be a plain relative path with forward slashes")
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return errors.New("must be clean: no empty, . or .. segments")
		}
	}
	return nil
}

func isSHA256Hex(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// LastImplementation returns the feature's most recent implementation record, if any.
func (s Spec) LastImplementation() (Implementation, bool) {
	if len(s.Implemented) == 0 {
		return Implementation{}, false
	}
	return s.Implemented[len(s.Implemented)-1], true
}

// criteriaIdentity is what memory's history says about a feature's criterion numbers.
type criteriaIdentity struct {
	Latest  map[int]bool // IDs of the newest genuine approval: they keep their place
	Retired map[int]bool // IDs a genuine approval had and a later genuine approval removed
	High    int          // highest ID ever approved or implemented
}

// gitBlob reads a file at a commit byte for byte (git() trims its output).
func gitBlob(dir, rev, rel string) (string, bool) {
	cmd := exec.Command("git", append(append([]string{}, gitSafety...), "cat-file", "blob", rev+":"+rel)...)
	cmd.Dir = dir
	cmd.Env = commandEnv(nil)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// loadCriteriaIdentity replays the feature's memory history, oldest first. A commit whose
// approval.yaml matches the spec next to it is a genuine approval; an ID that one genuine
// approval had and a later one lacks is retired for good, so restoring old files cannot bring
// it back. Every approval and implementation file ever committed, and the current files,
// raise the high-water mark, so lowering max_criterion or dropping implemented.yaml by hand
// frees nothing.
func loadCriteriaIdentity(dir string, s Spec) (criteriaIdentity, error) {
	id := criteriaIdentity{Latest: map[int]bool{}, Retired: map[int]bool{}}
	raise := func(a *SpecApproval, impls []Implementation) {
		if a != nil {
			id.High = max(id.High, a.MaxCriterion)
			for _, n := range a.Criteria {
				id.High = max(id.High, n)
			}
		}
		for _, im := range impls {
			id.High = max(id.High, im.MaxCriterion)
			for _, c := range im.Criteria {
				id.High = max(id.High, c.ID)
			}
		}
	}
	raise(s.Approval, s.Implemented)
	aRel, iRel, sRel := approvalRel(s.Feature), implementedRel(s.Feature), specRel(s.Feature)
	log, err := git(dir, "log", "--reverse", "--format=%H", "--", aRel, iRel)
	if err != nil {
		return id, fmt.Errorf("criteria history of %s: %w", s.Feature, err)
	}
	var prev map[int]bool
	for _, sha := range strings.Fields(log) {
		var a *SpecApproval
		if b, ok := gitBlob(dir, sha, aRel); ok {
			var v SpecApproval
			if yaml.Unmarshal([]byte(b), &v) == nil {
				a = &v
			}
		}
		var impls []Implementation
		if b, ok := gitBlob(dir, sha, iRel); ok {
			_ = yaml.Unmarshal([]byte(b), &impls)
		}
		raise(a, impls)
		if a == nil {
			continue
		}
		text, ok := gitBlob(dir, sha, sRel)
		if !ok {
			continue
		}
		sp, err := parseSpec(text)
		if err != nil || approvalDigest(sp.Title, sp.Paths, sp.Body) != a.Content {
			continue
		}
		crit, _, err := sp.Criteria()
		if err != nil {
			continue
		}
		ids := map[int]bool{}
		for _, c := range crit {
			ids[c.ID] = true
			id.High = max(id.High, c.ID)
		}
		for n := range prev {
			if !ids[n] {
				id.Retired[n] = true
			}
		}
		prev = ids
	}
	if prev != nil {
		id.Latest = prev
	}
	return id, nil
}

// check rejects a retired ID, and a new ID at or below the high-water mark. An ID the newest
// approval had keeps its place, even with new text.
func (id criteriaIdentity) check(crit []SpecCriterion) error {
	for _, c := range crit {
		switch {
		case id.Retired[c.ID]:
			return fmt.Errorf("AC-%d was removed in an approved version; its number is retired; new criteria start at AC-%d", c.ID, id.High+1)
		case !id.Latest[c.ID] && c.ID <= id.High:
			return fmt.Errorf("AC-%d reuses a retired or skipped number; new criteria start at AC-%d", c.ID, id.High+1)
		}
	}
	return nil
}

// checkSpecCriteria parses a spec's criteria and checks their numbers against history.
func checkSpecCriteria(dir string, s Spec) ([]SpecCriterion, criteriaIdentity, error) {
	crit, _, err := s.Criteria()
	if err != nil {
		return nil, criteriaIdentity{}, err
	}
	id, err := loadCriteriaIdentity(dir, s)
	if err != nil {
		return nil, id, err
	}
	return crit, id, id.check(crit)
}

// CriteriaDelta compares the approved criteria with the last implementation. New criteria are
// what a plan builds; unchanged ones keep their sealed tests; changed and removed ones need a
// test revision, which stage 3 does not do.
type CriteriaDelta struct {
	New       []SpecCriterion
	Unchanged []ImplementedCriterion
	Changed   []int
	Removed   []int
}

func criteriaDelta(crit []SpecCriterion, last *Implementation) CriteriaDelta {
	var d CriteriaDelta
	prev := map[int]ImplementedCriterion{}
	if last != nil {
		for _, c := range last.Criteria {
			prev[c.ID] = c
		}
	}
	now := map[int]bool{}
	for _, c := range crit {
		now[c.ID] = true
		p, ok := prev[c.ID]
		switch {
		case !ok:
			d.New = append(d.New, c)
		case p.Text == c.Text:
			d.Unchanged = append(d.Unchanged, p)
		default:
			d.Changed = append(d.Changed, c.ID)
		}
	}
	for id := range prev {
		if !now[id] {
			d.Removed = append(d.Removed, id)
		}
	}
	sort.Ints(d.Removed)
	return d
}
