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
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Agent output and batch imports (stage 4, foundations F1 and F2).
//
// An agent never writes memory. It writes into an output directory aeh created; aeh freezes
// those bytes once against an exact manifest, and every later step (validation, review,
// import) uses that same frozen snapshot. importMemoryBatch then writes a whole batch into
// memory in one commit, marked so a retry of the same batch is a no-op.

// Output file kinds. Each kind gets the checks every file of that kind needs; richer checks
// (restricted Markdown, safe mockups, secrets) run on the frozen snapshot afterwards.
const (
	outputMarkdown = "markdown"
	outputYAML     = "yaml"
	outputHTML     = "html"
)

// outputRule admits the files whose slash-separated path matches Pattern (path.Match, so "*"
// never crosses a "/").
type outputRule struct {
	Pattern  string
	Kind     string
	MaxBytes int64
	MaxCount int
	Required bool
}

// outputManifest is everything an agent's output may contain. A file no rule admits is an error.
type outputManifest struct {
	Name     string
	Rules    []outputRule
	MaxFiles int
	MaxTotal int64
}

// frozenOutput is an agent's output as aeh accepted it. Digest covers the manifest name and
// every path and byte, so a snapshot read back from disk proves it is the one that was frozen.
type frozenOutput struct {
	Manifest string            `json:"manifest"`
	Digest   string            `json:"digest"`
	Files    map[string][]byte `json:"files"`
}

// reservedOutputNames are files only aeh writes. No agent output may carry one, whatever the
// manifest admits.
var reservedOutputNames = map[string]bool{
	approvalFile: true, implementedFile: true, "index.md": true, "log.md": true, "schema.md": true,
}

// freezeOutput reads dir against the manifest and returns its accepted bytes. Every read goes
// through an os.Root, so a directory swapped for a link while aeh walks cannot lead outside dir.
func freezeOutput(dir string, m outputManifest) (frozenOutput, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return frozenOutput{}, fmt.Errorf("%s output: %w", m.Name, err)
	}
	if !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return frozenOutput{}, fmt.Errorf("%s output: %s must be a plain directory", m.Name, dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return frozenOutput{}, fmt.Errorf("%s output: %w", m.Name, err)
	}
	defer root.Close()
	files := map[string][]byte{}
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := root.Lstat(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("%s is a link or reparse point; write plain files only", rel)
		}
		if err := checkOutputName(rel); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		if len(files) >= m.MaxFiles {
			return fmt.Errorf("more than %d files", m.MaxFiles)
		}
		rule, ok := m.rule(rel)
		if !ok {
			return fmt.Errorf("%s is not an expected file", rel)
		}
		b, err := readRegularFile(root, rel, info, rule.MaxBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if total += int64(len(b)); total > m.MaxTotal {
			return fmt.Errorf("files exceed %d bytes in total", m.MaxTotal)
		}
		files[rel] = b
		return nil
	})
	if err != nil {
		return frozenOutput{}, fmt.Errorf("%s output: %w", m.Name, err)
	}
	if err := m.check(files); err != nil {
		return frozenOutput{}, err
	}
	return frozenOutput{Manifest: m.Name, Digest: outputDigest(m.Name, files), Files: files}, nil
}

// readRegularFile reads at most limit bytes from the file Lstat described, refusing a file
// that was swapped for another one between the Lstat and the open.
func readRegularFile(root *os.Root, rel string, before fs.FileInfo, limit int64) ([]byte, error) {
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("changed while aeh was reading it")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return b, nil
}

func (m outputManifest) rule(rel string) (outputRule, bool) {
	for _, r := range m.Rules {
		if ok, _ := path.Match(r.Pattern, rel); ok {
			return r, true
		}
	}
	return outputRule{}, false
}

// check validates a whole file set against the manifest. freezeOutput and loadFrozen share it,
// so a snapshot read back from disk passes exactly the checks the original output passed.
func (m outputManifest) check(files map[string][]byte) error {
	if len(files) > m.MaxFiles {
		return fmt.Errorf("%s output: more than %d files", m.Name, m.MaxFiles)
	}
	counts := map[string]int{}
	var total int64
	if err := checkCaseAliases(sortedKeys(files), nil); err != nil {
		return fmt.Errorf("%s output: %w", m.Name, err)
	}
	for _, rel := range sortedKeys(files) {
		b := files[rel]
		if err := checkOutputName(rel); err != nil {
			return fmt.Errorf("%s output: %s: %w", m.Name, rel, err)
		}
		r, ok := m.rule(rel)
		if !ok {
			return fmt.Errorf("%s output: %s is not an expected file", m.Name, rel)
		}
		if counts[r.Pattern]++; counts[r.Pattern] > r.MaxCount {
			return fmt.Errorf("%s output: more than %d files match %s", m.Name, r.MaxCount, r.Pattern)
		}
		if int64(len(b)) > r.MaxBytes {
			return fmt.Errorf("%s output: %s exceeds %d bytes", m.Name, rel, r.MaxBytes)
		}
		if total += int64(len(b)); total > m.MaxTotal {
			return fmt.Errorf("%s output: files exceed %d bytes in total", m.Name, m.MaxTotal)
		}
		if err := checkOutputContent(r.Kind, b); err != nil {
			return fmt.Errorf("%s output: %s: %w", m.Name, rel, err)
		}
	}
	for _, r := range m.Rules {
		if r.Required && counts[r.Pattern] == 0 {
			return fmt.Errorf("%s output: missing %s", m.Name, r.Pattern)
		}
	}
	return nil
}

// outputSegment is a portable ASCII name: it cannot be hidden, quoted by Git, or spelled two
// ways by Unicode.
var outputSegment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)

// checkOutputName accepts a canonical relative path of portable ASCII names that checks out on
// every platform and is not a file only aeh writes.
func checkOutputName(rel string) error {
	if err := checkPlanTestPath(rel); err != nil {
		return err
	}
	for _, s := range strings.Split(rel, "/") {
		if !outputSegment.MatchString(s) {
			return fmt.Errorf("name %q must be ASCII letters, digits, '.', '_' or '-', not starting with '.' or '-'", s)
		}
	}
	if reservedOutputNames[strings.ToLower(path.Base(rel))] {
		return errors.New("only aeh writes this file")
	}
	return nil
}

// checkCaseAliases refuses two paths, or a path and a known path, that spell one directory or
// file in two cases: on a case-insensitive disk they are the same.
func checkCaseAliases(paths, known []string) error {
	seen := map[string]string{}
	for _, p := range known {
		segs := strings.Split(p, "/")
		for i := range segs {
			prefix := strings.Join(segs[:i+1], "/")
			if _, ok := seen[strings.ToLower(prefix)]; !ok && p != "" {
				seen[strings.ToLower(prefix)] = prefix
			}
		}
	}
	for _, p := range paths {
		segs := strings.Split(p, "/")
		for i := range segs {
			prefix := strings.Join(segs[:i+1], "/")
			low := strings.ToLower(prefix)
			if other, ok := seen[low]; ok && other != prefix {
				return fmt.Errorf("%s and %s differ only in case", other, prefix)
			}
			seen[low] = prefix
		}
	}
	return nil
}

// checkOutputContent applies the checks every file of a kind needs.
func checkOutputContent(kind string, b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("must be valid UTF-8")
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return errors.New("must not contain NUL bytes")
	}
	switch kind {
	case outputMarkdown, outputHTML:
		return nil
	case outputYAML:
		return checkPlainYAML(b)
	}
	return fmt.Errorf("unknown output kind %q", kind)
}

// checkPlainYAML accepts exactly one YAML document without anchors, aliases, merge keys or
// explicit tags, so it reads the same to the owner and to aeh.
func checkPlainYAML(b []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("YAML file is empty")
		}
		return fmt.Errorf("parse YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("use exactly one YAML document")
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return fmt.Errorf("line %d: YAML anchors and aliases are not allowed", n.Line)
		}
		if n.Style&yaml.TaggedStyle != 0 {
			return fmt.Errorf("line %d: explicit YAML tags are not allowed", n.Line)
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "<<" && n.Content[i].Tag == "!!merge" {
					return fmt.Errorf("line %d: YAML merge keys are not allowed", n.Content[i].Line)
				}
			}
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&doc)
}

// outputDigest hashes a labeled, length-prefixed file set, so no two different sets share a digest.
func outputDigest(label string, files map[string][]byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "aeh-output-v1\x00%s\x00%d\x00", label, len(files))
	for _, rel := range sortedKeys(files) {
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(files[rel]))
		h.Write(files[rel])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// saveFrozen stores a frozen snapshot for recovery.
func saveFrozen(p string, f frozenOutput) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	if err := writeFileAtomic(p, b); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	return nil
}

// loadFrozen reads a saved snapshot back. It must be for this manifest, still pass it, and
// hash to the recorded digest; otherwise it is not the snapshot aeh froze.
func loadFrozen(p string, m outputManifest) (frozenOutput, error) {
	if err := rejectLink(p); err != nil {
		return frozenOutput{}, fmt.Errorf("load snapshot: %w", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return frozenOutput{}, fmt.Errorf("load snapshot: %w", err)
	}
	var f frozenOutput
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return frozenOutput{}, fmt.Errorf("load snapshot %s: %w", p, err)
	}
	if f.Manifest != m.Name {
		return frozenOutput{}, fmt.Errorf("load snapshot %s: it is a %q snapshot, not %q", p, f.Manifest, m.Name)
	}
	if f.Files == nil {
		f.Files = map[string][]byte{}
	}
	if err := m.check(f.Files); err != nil {
		return frozenOutput{}, fmt.Errorf("load snapshot %s: %w", p, err)
	}
	if got := outputDigest(m.Name, f.Files); got != f.Digest {
		return frozenOutput{}, fmt.Errorf("load snapshot %s: content does not match its digest", p)
	}
	return f, nil
}

// memoryBatch is a set of memory files written in one commit.
type memoryBatch struct {
	Session      string            // who imports: one or two slugs joined by "/", e.g. "discover/shop"
	ExpectedHead string            // the memory HEAD the batch was prepared against
	Files        map[string][]byte // memory-relative paths under wiki/, final bytes
	Subject      string            // memory commit subject
	Log          string            // log entry text; aeh adds the date and the session
}

var batchSession = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*(/[a-z0-9]+(-[a-z0-9]+)*)?$`)

// batchDigest identifies a batch by its files alone, so a retry on another day still matches.
func batchDigest(files map[string][]byte) string { return outputDigest("memory-batch", files) }

func importMarker(session, digest string) string { return "AEH-Import: " + session + " " + digest }

// importMemoryBatch writes a batch into memory in one commit with an import marker. A batch
// whose marker is already in memory's history is a no-op that returns that commit. Otherwise
// memory must be clean and still at ExpectedHead, or the batch is a conflict. Index and log are
// regenerated in the same commit. The caller must hold the project lock.
func importMemoryBatch(r Repo, b memoryBatch) (commit string, imported bool, err error) {
	if err := b.validate(); err != nil {
		return "", false, err
	}
	digest := batchDigest(b.Files)
	marker := importMarker(b.Session, digest)
	dir, err := MemoryDir(r)
	if err != nil {
		return "", false, err
	}
	snap, err := loadMemoryDir(dir)
	if err != nil {
		return "", false, err
	}
	if !snap.Present {
		return "", false, fmt.Errorf("import %s: memory is not initialized", b.Session)
	}
	if err := memoryClean(dir); err != nil {
		return "", false, fmt.Errorf("import %s: %w", b.Session, err)
	}
	if _, sha, ok, err := findRecord(dir, marker); err != nil {
		return "", false, err
	} else if ok {
		// The marker only counts when its commit really holds the batch.
		if err := verifyImported(dir, sha, b.Files); err != nil {
			return "", false, fmt.Errorf("import %s: memory commit %s carries this batch's marker but %w; check the memory repository by hand", b.Session, short(sha), err)
		}
		return sha, false, nil
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("import %s: %w", b.Session, err)
	}
	if head != b.ExpectedHead {
		return "", false, fmt.Errorf("import %s: %w (expected %s, found %s)", b.Session, ErrMemoryChanged, short(b.ExpectedHead), short(head))
	}
	if err := checkTrackedCase(dir, b.Files); err != nil {
		return "", false, fmt.Errorf("import %s: %w", b.Session, err)
	}
	w := memoryWrites{}
	fail := func(err error) (string, bool, error) {
		return "", false, fmt.Errorf("import %s: %w", b.Session, w.restore(dir, err))
	}
	for _, rel := range sortedKeys(b.Files) {
		if cur, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err == nil && bytes.Equal(cur, b.Files[rel]) {
			continue // already reads like this; a commit lists only real changes
		}
		if err := w.write(dir, rel, b.Files[rel]); err != nil {
			return fail(err)
		}
	}
	after, err := loadMemoryDir(dir)
	if err != nil {
		return fail(err)
	}
	if index := []byte(renderIndex(after.Specs)); !fileEquals(filepath.Join(dir, "wiki", "index.md"), index) {
		if err := w.write(dir, "wiki/index.md", index); err != nil {
			return fail(err)
		}
	}
	today := memoryNow().UTC().Format("2006-01-02")
	if err := appendLog(dir, w, "["+today+"] import | "+b.Session+" | "+b.Log); err != nil {
		return fail(err)
	}
	if now, err := git(dir, "rev-parse", "HEAD"); err != nil || now != b.ExpectedHead {
		return fail(fmt.Errorf("%w while aeh was writing the batch", ErrMemoryChanged))
	}
	if err := w.commit(dir, b.Subject, marker); err != nil {
		return fail(err)
	}
	commit, err = git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("import %s: %w", b.Session, err)
	}
	// Files the batch left untouched come from the parent, so the parent must be the expected
	// head and every batch file must read as the batch says. A retry re-checks this too.
	parent, err := git(dir, "rev-parse", commit+"^")
	if err == nil && parent != b.ExpectedHead {
		err = fmt.Errorf("its parent is %s, not %s", short(parent), short(b.ExpectedHead))
	}
	if err == nil {
		err = verifyImported(dir, commit, b.Files)
	}
	if err != nil {
		return "", false, fmt.Errorf("import %s: memory commit %s %w; check the memory repository by hand", b.Session, short(commit), err)
	}
	return commit, true, nil
}

// verifyImported checks that commit holds every batch file with the batch's exact bytes.
func verifyImported(dir, commit string, files map[string][]byte) error {
	for _, rel := range sortedKeys(files) {
		got, ok := gitBlob(dir, commit, rel)
		if !ok || got != string(files[rel]) {
			return fmt.Errorf("does not hold %s as the batch wrote it", rel)
		}
	}
	return nil
}

func (b memoryBatch) validate() error {
	if !batchSession.MatchString(b.Session) || len(b.Session) > 129 {
		return fmt.Errorf("import: session %q must be one or two lowercase slugs joined by /", b.Session)
	}
	for _, s := range strings.Split(b.Session, "/") {
		if err := checkSlug(s); err != nil {
			return fmt.Errorf("import: session %q: %w", b.Session, err)
		}
	}
	if !isFullSHA(b.ExpectedHead) {
		return fmt.Errorf("import %s: expected memory HEAD %q is not a full SHA", b.Session, b.ExpectedHead)
	}
	if err := checkBatchLine(b.Subject, 200); err != nil || b.Subject != strings.TrimSpace(b.Subject) ||
		strings.Contains(strings.ToUpper(b.Subject), "AEH-") {
		return fmt.Errorf("import %s: commit subject must be one trimmed line of at most 200 bytes without \"AEH-\"", b.Session)
	}
	if err := checkBatchLine(b.Log, 300); err != nil {
		return fmt.Errorf("import %s: log entry: %w", b.Session, err)
	}
	if len(b.Files) == 0 {
		return fmt.Errorf("import %s: the batch has no files", b.Session)
	}
	if err := checkCaseAliases(sortedKeys(b.Files), nil); err != nil {
		return fmt.Errorf("import %s: %w", b.Session, err)
	}
	for _, rel := range sortedKeys(b.Files) {
		if err := checkBatchPath(rel); err != nil {
			return fmt.Errorf("import %s: %s: %w", b.Session, rel, err)
		}
		if !utf8.Valid(b.Files[rel]) || bytes.IndexByte(b.Files[rel], 0) >= 0 {
			return fmt.Errorf("import %s: %s must be UTF-8 text without NUL bytes", b.Session, rel)
		}
	}
	return nil
}

// checkBatchPath admits files under wiki/ that aeh does not generate. A feature directory takes
// only its spec: approvals and implementation records have their own commands.
func checkBatchPath(rel string) error {
	if err := checkOutputName(rel); err != nil {
		return err
	}
	segs := strings.Split(rel, "/")
	if len(segs) < 2 || segs[0] != "wiki" {
		return errors.New("imports write only under wiki/")
	}
	if strings.EqualFold(segs[1], "features") && (segs[1] != "features" || len(segs) != 4 || segs[3] != "spec.md" || checkSlug(segs[2]) != nil) {
		return errors.New("under wiki/features/ an import writes only <feature>/spec.md")
	}
	return nil
}

func checkBatchLine(s string, max int) error {
	if strings.TrimSpace(s) == "" || len(s) > max || strings.ContainsFunc(s, unicode.IsControl) ||
		strings.ContainsAny(s, "\u0085\u2028\u2029") {
		return fmt.Errorf("must be one non-empty line of at most %d bytes", max)
	}
	return nil
}

// checkTrackedCase refuses a batch path that names a tracked memory file or directory in another
// case: on a case-insensitive disk both would be one.
func checkTrackedCase(dir string, files map[string][]byte) error {
	tracked, err := git(dir, "ls-files", "-z")
	if err != nil {
		return fmt.Errorf("memory files: %w", err)
	}
	if err := checkCaseAliases(sortedKeys(files), strings.Split(tracked, "\x00")); err != nil {
		return fmt.Errorf("against tracked memory files: %w", err)
	}
	return nil
}

func fileEquals(p string, b []byte) bool {
	cur, err := os.ReadFile(p)
	return err == nil && bytes.Equal(cur, b)
}
