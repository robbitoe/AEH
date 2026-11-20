package aeh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AdoptConfig selects cooperative read-only profiles and additional secret exclusions.
type AdoptConfig struct {
	Ladder  []string `yaml:"ladder"`
	Exclude []string `yaml:"exclude"`
}

func (a *AdoptConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return errors.New("adopt must be a mapping of ladder and exclude")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "ladder", "exclude":
		default:
			return fmt.Errorf("adopt: unknown key %q", n.Content[i].Value)
		}
	}
	type plain AdoptConfig
	return n.Decode((*plain)(a))
}

func (c Config) adopt() AdoptConfig {
	a := c.Adopt
	if len(a.Ladder) == 0 {
		a.Ladder = c.Ladder
	}
	return a
}

func (c Config) validateAdoptConfig() error {
	seen := map[string]bool{}
	for _, p := range c.adopt().Ladder {
		if err := checkDisplayLine(p, 120); err != nil {
			return fmt.Errorf("config: adopt profile: %w", err)
		}
		if err := checkNoSecrets("adopt profile", []byte(p)); err != nil {
			return err
		}
		if seen[p] {
			return fmt.Errorf("config: profile %q appears twice in the adopt ladder", p)
		}
		seen[p] = true
		if err := c.checkProfile(p); err != nil {
			return fmt.Errorf("config: adopt ladder: %w", err)
		}
	}
	for _, p := range c.Adopt.Exclude {
		if err := checkAdoptExclusion(p); err != nil {
			return fmt.Errorf("config: adopt.exclude: %w", err)
		}
	}
	return nil
}

// Patterns keep matchAny semantics, with canonical spelling so validation and matching agree.
func checkAdoptPattern(p string) error {
	if err := checkAdoptExclusion(p); err != nil {
		return err
	}
	return checkSpecPath(p)
}

// Unlike ownership, exclusion may cover aeh/config.yaml: it can contain credentials too.
// Git metadata remains the runner's, and a whole-repository exclusion is not an inventory.
func checkAdoptExclusion(p string) error {
	if strings.TrimSpace(p) != p || strings.ContainsAny(p, "\\\x00\r\n") || strings.HasPrefix(p, "./") {
		return errors.New("adopt paths must be canonical repository-relative patterns with forward slashes")
	}
	for _, s := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if s == "" || s == "." || s == ".." {
			return errors.New("adopt paths must have no empty, . or .. segments")
		}
	}
	if p == "*" || p == "**" || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return errors.New("adopt exclusions must be relative and must not exclude the whole repository")
	}
	first := strings.Split(p, "/")[0]
	if strings.EqualFold(first, ".git") {
		return errors.New("adopt exclusions must not address Git metadata")
	}
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("adopt exclusion: %w", err)
	}
	if err := checkDisplayLine(p, 500); err != nil {
		return err
	}
	return checkNoSecrets("adopt path", []byte(p))
}

// AdoptInspection records which immutable code tree an accepted document describes.
type AdoptInspection struct {
	Base     string `json:"base_sha"`
	Date     string `json:"date"`
	Profile  string `json:"profile"`
	Excluded int    `json:"excluded_paths"`
	Snapshot string `json:"snapshot_sha256"`
}

// AdoptAttempt preserves failed integrity checks and ladder progress across interruption.
type AdoptAttempt struct {
	AdoptInspection
	N         int     `json:"n"`
	Input     string  `json:"input_sha256"`
	Result    string  `json:"result"`
	Feedback  string  `json:"feedback,omitempty"`
	WroteCode bool    `json:"wrote_code,omitempty"`
	Seconds   float64 `json:"seconds"`
}

// AdoptRun is the result of one inventory or area command.
type AdoptRun struct {
	Status     string           `json:"status"`
	Inspection *AdoptInspection `json:"inspection,omitempty"`
	Attempts   []AdoptAttempt   `json:"attempts"`
}

// openAdoption owns one project lock, then resumes all adoption imports before reading memory.
func openAdoption(r Repo) (*adoptSession, func(), error) {
	main, err := MainCheckout(r)
	if err != nil {
		return nil, nil, err
	}
	r = Repo{Root: main}
	unlock, err := lockLiveProject(r)
	if err != nil {
		return nil, nil, err
	}
	completed := map[string]bool{}
	if err := recoverAdoptImports(r, completed); err != nil {
		unlock()
		return nil, nil, err
	}
	a, err := loadAdoptState(r, "")
	if err != nil {
		unlock()
		return nil, nil, err
	}
	a.resumedInspections = completed
	return a, unlock, nil
}

func resumeAdoptImports(r Repo) error {
	return recoverAdoptImports(r, nil)
}

func recoverAdoptImports(r Repo, completed map[string]bool) error {
	// Resume the inventory first, then every area's independently marked import.
	a, err := loadAdoptState(r, "")
	if err != nil {
		return err
	}
	sessions := []*adoptSession{a}
	entries, err := os.ReadDir(filepath.Join(r.Root, ".aeh", "adopt"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := checkSlug(e.Name()); err != nil {
			continue
		}
		a, err := loadAdoptSession(r, e.Name())
		if err != nil {
			return err
		}
		sessions = append(sessions, a)
	}
	// Recover partial files before checking cleanliness; the dirt may belong to any session.
	for _, session := range sessions {
		if session.state.Pending != nil {
			if err := discardPartialImport(session.memDir, session.state.Pending, session.session()); err != nil {
				return err
			}
		}
	}
	if err := memoryClean(a.memDir); err != nil {
		return err
	}
	for _, session := range sessions {
		p := session.state.Pending
		inspection := p != nil && session.state.Inspection != nil && (strings.HasPrefix(p.Log, "inventory ") || strings.HasPrefix(p.Log, "observe "))
		if err := session.finishPending(); err != nil {
			return err
		}
		if inspection {
			session.state.RunStatus = "accepted"
			if n := len(session.state.Attempts); n > 0 && session.state.Attempts[n-1].Snapshot == session.state.Inspection.Snapshot {
				session.state.Attempts[n-1].Result, session.state.Attempts[n-1].Feedback = "accepted", ""
			}
			if err := session.save(); err != nil {
				return err
			}
			if completed != nil {
				completed[session.feature] = true
			}
		}
	}
	return nil
}

func (a *adoptSession) writeSuggested() error {
	if a.feature != "" || a.state.Suggested == nil {
		return nil
	}
	if err := checkSuggested(a.state.Suggested); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.repo.Root, ".aeh", "adopt", "config.suggested.yaml"), a.state.Suggested)
}

func checkSuggested(b []byte) error {
	if len(b) > 16384 {
		return errors.New("config.suggested.yaml exceeds 16 KiB")
	}
	if err := checkPlainYAML(b); err != nil {
		return err
	}
	if err := checkNoSecrets("config.suggested.yaml", b); err != nil {
		return err
	}
	// Also inspect decoded scalars, before any typed decoding: YAML escapes must not conceal a
	// token or terminal control, and a type error quotes a (truncated) value that redaction
	// could no longer recognize.
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("config.suggested.yaml must be one YAML mapping")
	}
	if err := checkYAMLScalars("config.suggested.yaml", &doc, func(v string) error {
		fence := promptFence(v)
		return checkDisplayMarkdown(fence+"\n"+v+"\n"+fence, 16384+2*len(fence)+2)
	}); err != nil {
		return err
	}
	var suggested Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&suggested); err != nil {
		return fmt.Errorf("config.suggested.yaml must use only aeh/config.yaml keys, unique mappings and valid value types: %v", err)
	}
	return nil
}

// checkYAMLScalars checks every decoded scalar of a document for secrets, then with check.
func checkYAMLScalars(name string, n *yaml.Node, check func(string) error) error {
	if n.Kind == yaml.ScalarNode {
		if err := checkNoSecrets(name+" scalar", []byte(n.Value)); err != nil {
			return err
		}
		if check != nil {
			if err := check(n.Value); err != nil {
				return err
			}
		}
	}
	for _, child := range n.Content {
		if err := checkYAMLScalars(name, child, check); err != nil {
			return err
		}
	}
	return nil
}

func resolveAdoptBase(r Repo, rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	sha, err := git(r.Root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("adopt base: %w", err)
	}
	if !isFullSHA(sha) {
		return "", errors.New("adopt base did not resolve to a full commit SHA")
	}
	return sha, nil
}

// inspectionWorktree is a disposable checkout. File digests include ignored files and the
// .git pointer; no Git index flag, staging operation or ignore rule can hide a write.
type inspectionWorktree struct {
	repo      Repo
	dir, base string
	excluded  []string
	files     map[string]string
}

func newInspectionWorktree(r Repo, base string, exclude []string, strip bool, name string) (*inspectionWorktree, error) {
	if !validID(name) {
		return nil, errors.New("invalid inspection worktree name")
	}
	dir := r.WorktreePath(name)
	w := &inspectionWorktree{repo: r, dir: dir, base: base}
	// Reuse plan's detached checkout setup and stale-attempt cleanup.
	p := planner{repo: r}
	if err := p.freshWorktree(dir, base); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			w.close()
		}
	}()
	var err error
	if strip {
		tracked, err := baseFiles(r, base)
		if err != nil {
			return nil, err
		}
		err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := rel(dir, p)
			if rel == "." || rel == ".git" {
				return nil
			}
			linked := d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0
			if regular, ok := tracked[rel]; ok && !regular {
				linked = true // Git may materialize a symlink as a plain file on Windows.
			}
			if d.IsDir() && !linked {
				return nil
			}
			if linked || possibleAdoptSecret(rel, exclude) {
				if err := checkDisplayLine(rel, 500); err != nil {
					return err
				}
				if err := checkNoSecrets("excluded path", []byte(rel)); err != nil {
					return err
				}
				if err := os.Remove(p); err != nil {
					return err
				}
				w.excluded = append(w.excluded, rel)
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	w.files, err = inspectionFiles(dir)
	if err != nil {
		return nil, err
	}
	sort.Strings(w.excluded)
	ok = true
	return w, nil
}

func (w *inspectionWorktree) close() { removeWorktree(w.repo.Root, w.dir) }

// runName names the run directory of an inspection: adopt-<feature> for an area, and adopt
// for the inventory, which no feature name can produce.
func (a *adoptSession) runName() string {
	if a.feature == "" {
		return "adopt"
	}
	return "adopt-" + a.feature
}

func (a *adoptSession) inspectionName(n int) string {
	if a.feature == "" {
		return fmt.Sprintf("adopt-inventory-a%d", n)
	}
	return fmt.Sprintf("adopt-area-%s-a%d", a.feature, n)
}

func possibleAdoptSecret(p string, exclude []string) bool {
	b := strings.ToLower(path.Base(p))
	for _, pattern := range []string{".env*", "*.pem", "*.key", "id_*", "*.pfx", "credentials*", "secrets*"} {
		if ok, _ := path.Match(pattern, b); ok {
			return true
		}
	}
	return matchAny(p, exclude)
}

func inspectionFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			files[rel(dir, p)] = "link:" + target
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			files[rel(dir, p)+"/"] = "directory"
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("inspection requires regular checkout files")
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		files[rel(dir, p)] = fmt.Sprintf("%s:%o", hex.EncodeToString(h.Sum(nil)), info.Mode().Perm())
		return nil
	})
	return files, err
}

func (w *inspectionWorktree) integrity(allowIgnored bool) error {
	after, err := inspectionFiles(w.dir)
	if err != nil {
		return fmt.Errorf("checkout changed: %w", err)
	}
	for p, digest := range w.files {
		if after[p] != digest {
			return fmt.Errorf("checkout changed an existing file: %s", discoveryDiagnostic(p))
		}
	}
	if !allowIgnored && !reflect.DeepEqual(after, w.files) {
		return errors.New("agent wrote into the code: checkout contains new files")
	}
	if head, err := headCommit(w.dir); err != nil || head != w.base {
		return errors.New("checkout HEAD changed")
	}
	index, err := git(w.dir, "diff", "--cached", "--name-only", w.base, "--")
	if err != nil {
		return err
	}
	if index != "" {
		return errors.New("checkout index changed")
	}
	flags, err := git(w.dir, "ls-files", "-v")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(flags, "\n") {
		if line != "" && (line[0] == 'S' || line[0] >= 'a' && line[0] <= 'z') {
			return errors.New("checkout index flags changed")
		}
	}
	if allowIgnored {
		newFiles, err := w.unignoredFiles()
		if err != nil {
			return err
		}
		if newFiles != "" {
			return errors.New("checkout contains new files not ignored by the project's .gitignore")
		}
	}
	return nil
}

// unignoredFiles consults only .gitignore files present in the pinned checkout. A command
// cannot create a new nested .gitignore to hide its own outputs, nor use global excludes or
// .git/info/exclude. The trusted copies keep each ignore file's original directory semantics.
func (w *inspectionWorktree) unignoredFiles() (string, error) {
	marker, err := os.CreateTemp(w.dir, ".aeh-ignore-*")
	if err != nil {
		return "", err
	}
	name := filepath.Base(marker.Name())
	if err := marker.Close(); err != nil {
		_ = os.Remove(marker.Name())
		return "", err
	}
	var copies []string
	copies = append(copies, marker.Name())
	defer func() {
		for _, p := range copies {
			_ = os.Remove(p)
		}
	}()
	generated := map[string]bool{name: true}
	for p, digest := range w.files {
		if path.Base(p) != ".gitignore" || strings.HasPrefix(digest, "link:") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(w.dir, filepath.FromSlash(p)))
		if err != nil {
			return "", err
		}
		target := path.Join(path.Dir(p), name)
		full := filepath.Join(w.dir, filepath.FromSlash(target))
		if target == name {
			if err := os.WriteFile(full, b, 0600); err != nil {
				return "", err
			}
		} else {
			f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return "", err
			}
			copies = append(copies, full)
			_, writeErr := f.Write(b)
			if err := errors.Join(writeErr, f.Close()); err != nil {
				return "", err
			}
		}
		generated[target] = true
	}
	raw, err := gitRaw(w.dir, "ls-files", "-z", "--others", "--exclude-per-directory="+name)
	if err != nil {
		return "", err
	}
	for _, p := range strings.Split(raw, "\x00") {
		if p != "" && !generated[p] {
			return p, nil
		}
	}
	return "", nil
}

func inventoryManifest() outputManifest {
	return outputManifest{Name: "adopt-inventory", Rules: []outputRule{
		{Pattern: "system.md", Kind: outputMarkdown, MaxBytes: 65536, MaxCount: 1, Required: true},
		{Pattern: "config.suggested.yaml", Kind: outputYAML, MaxBytes: 16384, MaxCount: 1},
	}, MaxFiles: 2, MaxTotal: 81920}
}

func areaManifest() outputManifest {
	return outputManifest{Name: "adopt-area", Rules: []outputRule{
		{Pattern: "observations.yaml", Kind: outputYAML, MaxBytes: 65536, MaxCount: 1, Required: true},
	}, MaxFiles: 1, MaxTotal: 65536}
}

// runInspection runs two tries per profile. Its validator operates on frozen bytes and may
// record a pending import only after all semantic checks have succeeded.
func (a *adoptSession) runInspection(ctx context.Context, base string, cfg Config, data promptData, manifest outputManifest,
	accept func(frozenOutput, AdoptInspection) error, out io.Writer) (AdoptRun, error) {
	inputBytes, _ := json.Marshal(struct {
		Base   string
		Config Config
		Data   promptData
	}{base, cfg, data})
	input := observationDigest(string(inputBytes))
	for i := range a.state.Attempts {
		previous := &a.state.Attempts[i]
		if previous.Result != "running" {
			continue
		}
		dir := a.repo.WorktreePath(a.inspectionName(previous.N))
		removeWorktree(a.repo.Root, dir)
		if err := os.RemoveAll(dir); err != nil {
			return AdoptRun{}, err
		}
		if err := os.RemoveAll(dir + "-output"); err != nil {
			return AdoptRun{}, err
		}
		previous.Result, previous.Feedback = "interrupted", "the previous adoption command stopped during this attempt"
	}
	// Resume an interrupted ladder for these exact inputs. An exhausted ladder is a new retry.
	if n := len(a.state.Attempts); n > 0 && a.state.Attempts[n-1].Result == "accepted" {
		a.state.RunStatus = "accepted"
	}
	if a.state.Input != input || a.state.RunStatus != StatusRunning {
		a.state.LadderFrom, a.state.Input = len(a.state.Attempts), input
	}
	startAt := a.state.LadderFrom
	if startAt < 0 || startAt > len(a.state.Attempts) {
		return AdoptRun{}, errors.New("adopt state has an invalid ladder position")
	}
	a.state.RunStatus = StatusRunning
	counts := map[string]int{}
	for _, prev := range a.state.Attempts[startAt:] {
		counts[prev.Profile]++
	}
	result := func(status string) AdoptRun {
		return AdoptRun{Status: status, Inspection: a.state.Inspection, Attempts: a.state.Attempts[startAt:]}
	}
	feedback := ""
	if len(a.state.Attempts) > startAt {
		feedback = a.state.Attempts[len(a.state.Attempts)-1].Feedback
	}
	for _, profile := range cfg.adopt().Ladder {
		for try := counts[profile]; try < 2; try++ {
			n := len(a.state.Attempts) + 1
			attempt := AdoptAttempt{N: n, Input: input, Result: "running", AdoptInspection: AdoptInspection{
				Base: base, Profile: profile, Date: memoryNow().UTC().Format("2006-01-02")}}
			a.state.Attempts = append(a.state.Attempts, attempt)
			if err := a.save(); err != nil {
				return result(StatusRunning), err
			}
			if out != nil {
				fmt.Fprintf(out, "adopt %s: attempt %d with %s at %s\n", a.feature, n, discoveryDiagnostic(profile), base)
			}
			start := time.Now()
			frozen, inspection, wrote, err := a.inspectAttempt(ctx, n, base, profile, cfg, data, manifest, feedback)
			at := &a.state.Attempts[n-1]
			at.Seconds, at.WroteCode, at.Excluded = time.Since(start).Seconds(), wrote, inspection.Excluded
			if err == nil {
				at.Result, at.Snapshot = "accepted", frozen.Digest
				inspection.Snapshot = frozen.Digest
				// A failed validator has no memory or local content side effects.
				err = accept(frozen, inspection)
			}
			at = &a.state.Attempts[n-1]
			if err == nil {
				a.state.RunStatus = "accepted"
				return result("accepted"), a.save()
			}
			at.Result, at.Feedback = "failed", discoveryDiagnostic(err.Error())
			feedback = at.Feedback
			if saveErr := a.save(); saveErr != nil {
				return result(StatusRunning), saveErr
			}
			if out != nil {
				fmt.Fprintf(out, "  rejected: %s\n", firstLine(feedback))
			}
			if ctx.Err() != nil || uncertainExit(err) {
				a.state.RunStatus = StatusBlocked
				if saveErr := a.save(); saveErr != nil {
					return result(StatusBlocked), saveErr
				}
				return result(StatusBlocked), err
			}
			// A pending batch is a runner/import problem, never feedback for another agent.
			var recordErr *adoptRecordError
			if a.state.Pending != nil || errors.As(err, &recordErr) {
				a.state.RunStatus = StatusBlocked
				if saveErr := a.save(); saveErr != nil {
					return result(StatusBlocked), saveErr
				}
				return result(StatusBlocked), err
			}
		}
	}
	a.state.RunStatus = StatusBlocked
	return result(StatusBlocked), a.save()
}

func (a *adoptSession) inspectAttempt(ctx context.Context, n int, base, profile string, cfg Config, data promptData,
	manifest outputManifest, feedback string) (frozenOutput, AdoptInspection, bool, error) {
	info := AdoptInspection{Base: base, Profile: profile, Date: memoryNow().UTC().Format("2006-01-02")}
	w, err := newInspectionWorktree(a.repo, base, cfg.Adopt.Exclude, true, a.inspectionName(n))
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	defer w.close()
	info.Excluded = len(w.excluded)
	outDir := w.dir + "-output"
	root, err := openOutputDir(outDir)
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	defer func() { root.Close(); _ = os.RemoveAll(outDir) }()
	data.BaseSHA, data.OutputDir, data.Excluded, data.Feedback = base, filepath.ToSlash(outDir), strings.Join(w.excluded, "\n"), feedback
	name, verdict := "adopt-inventory.md", "INVENTORY: READY"
	if manifest.Name == "adopt-area" {
		name, verdict = "adopt-area.md", "AREA: READY"
	}
	template, err := loadPrompt(a.repo, name)
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	prompt, err := renderPrompt(name, template, data)
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	outPath, runErr := runAgentWithOutput(ctx, cfg.Profiles[profile], cfg.PassEnv, w.dir, outDir, prompt,
		a.repo.RunDir(a.runName(), fmt.Sprintf("a%d", n)), "adopt")
	if err := w.integrity(false); err != nil {
		if uncertainExit(runErr) {
			return frozenOutput{}, info, true, fmt.Errorf("agent wrote into the code; execution was interrupted: %w", runErr)
		}
		return frozenOutput{}, info, true, fmt.Errorf("agent wrote into the code: %w", err)
	}
	if runErr != nil {
		return frozenOutput{}, info, false, runErr
	}
	output, err := os.ReadFile(outPath)
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != verdict {
		return frozenOutput{}, info, false, fmt.Errorf("agent output must end with %q", verdict)
	}
	frozen, err := freezeOutput(root, manifest)
	if err != nil {
		return frozenOutput{}, info, false, err
	}
	if err := gateBatchFiles(frozen.Files); err != nil {
		return frozenOutput{}, info, false, err
	}
	return frozen, info, false, nil
}

// AdoptInventory inspects the pinned main checkout commit and proposes a system document.
func AdoptInventory(ctx context.Context, r Repo, base string, out io.Writer) (_ AdoptRun, err error) {
	defer func() { err = redactErr(err) }()
	a, unlock, err := openAdoption(r)
	if err != nil {
		return AdoptRun{}, err
	}
	defer unlock()
	defer a.reportCommand(&err)
	cfg, err := a.repo.LoadConfig()
	if err != nil {
		return AdoptRun{}, err
	}
	base, err = resolveAdoptBase(a.repo, base)
	if err != nil {
		return AdoptRun{}, err
	}
	if a.resumedInspections[""] && a.state.Inspection.Base == base {
		a.printSuggested(out)
		return a.recoveredRun(), nil
	}
	result, err := a.runInspection(ctx, base, cfg, promptData{}, inventoryManifest(), func(f frozenOutput, info AdoptInspection) error {
		if strings.TrimSpace(string(f.Files["system.md"])) == "" {
			return errors.New("system.md must contain the system inventory")
		}
		if err := checkDisplayMarkdown(string(f.Files["system.md"]), 65536); err != nil {
			return err
		}
		if b, ok := f.Files["config.suggested.yaml"]; ok {
			if err := checkSuggested(b); err != nil {
				return err
			}
		}
		target := "wiki/system.md"
		if _, exists := gitBlob(a.memDir, "HEAD", target); exists {
			target = "wiki/system.next.md"
		}
		header := fmt.Sprintf("Inspected base SHA: %s\n\nDate: %s\n\nProfile: %s\n\n", info.Base, info.Date, discoveryDiagnostic(info.Profile))
		files := map[string][]byte{target: []byte(header + string(f.Files["system.md"]))}
		if err := gateBatchFiles(files); err != nil {
			return err
		}
		a.state.Inspection = &info
		if target == "wiki/system.md" {
			a.state.Current = &info
		} else {
			a.state.Next = &info
		}
		a.state.Suggested = f.Files["config.suggested.yaml"]
		if previous, ok := gitBlob(a.memDir, "HEAD", target); ok && previous == string(files[target]) {
			// A repeated inventory can produce the exact same advisory bytes. Its local
			// provenance/config still refresh, without preparing a second batch marker.
			if err := a.save(); err != nil {
				return &adoptRecordError{err}
			}
			return a.writeSuggested()
		}
		if err := a.record(files, "adopt: system inventory at "+base[:7], "inventory "+base); err != nil {
			return &adoptRecordError{err}
		}
		return nil
	}, out)
	if err == nil && result.Status == "accepted" {
		a.printSuggested(out)
	}
	return result, err
}

func (a *adoptSession) printSuggested(out io.Writer) {
	if a.state.Suggested != nil && out != nil {
		fmt.Fprintln(out, "Suggested configuration: .aeh/adopt/config.suggested.yaml (local, never executed by aeh). Copy only what you accept into aeh/config.yaml.")
	}
}

func (a *adoptSession) recoveredRun() AdoptRun {
	start := max(0, min(a.state.LadderFrom, len(a.state.Attempts)))
	return AdoptRun{Status: "accepted", Inspection: a.state.Inspection, Attempts: a.state.Attempts[start:]}
}

// AdoptAcceptSystem replaces the current advisory inventory in one recoverable memory commit.
func AdoptAcceptSystem(r Repo) (err error) {
	defer func() { err = redactErr(err) }()
	a, unlock, err := openAdoption(r)
	if err != nil {
		return err
	}
	defer unlock()
	defer a.reportCommand(&err)
	if a.state.Next == nil {
		return nil
	}
	b, ok := gitBlob(a.memDir, "HEAD", "wiki/system.next.md")
	if !ok {
		return errors.New("system.next.md is missing from memory")
	}
	if err := gateBatchFiles(map[string][]byte{"wiki/system.md": []byte(b)}); err != nil {
		return err
	}
	a.state.Current, a.state.Next = a.state.Next, nil
	// importBatch writes files, rather than deleting them. The retained next document is an
	// audit copy; state distinguishes it from a pending proposal after acceptance.
	return a.record(map[string][]byte{"wiki/system.md": []byte(b)}, "adopt: accept system inventory", "accept system")
}

type areaOutput struct {
	Title        string `yaml:"title"`
	Description  string `yaml:"description"`
	Observations []struct {
		ID        *int     `yaml:"id"`
		Text      string   `yaml:"text"`
		Locations []string `yaml:"locations"`
	} `yaml:"observations"`
}

func baseFiles(r Repo, base string) (map[string]bool, error) {
	raw, err := gitRaw(r.Root, "ls-tree", "-r", "-z", base)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, entry := range strings.Split(raw, "\x00") {
		header, p, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if ok && len(fields) == 3 && fields[1] == "blob" {
			files[p] = fields[0] == "100644" || fields[0] == "100755"
		}
	}
	return files, nil
}

func adoptAreaNames(memDir string) ([]string, error) {
	raw, err := gitRaw(memDir, "ls-tree", "--name-only", "-z", "HEAD:wiki/adopt")
	if err != nil {
		// An absent adopt tree is normal for initialized memory.
		if _, e := git(memDir, "cat-file", "-e", "HEAD:wiki/adopt"); e != nil {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(raw, "\x00") {
		if name == "" {
			continue
		}
		if err := checkSlug(name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func checkAreaOwnership(r Repo, memDir, feature string, paths []string) error {
	if err := admitWork(r, feature, paths, ""); err != nil {
		return err
	}
	memory, err := LoadMemory(r)
	if err != nil {
		return err
	}
	for _, s := range memory.Specs {
		if s.Feature == feature || pathsOverlap(paths, s.OwnedPaths()) {
			return fmt.Errorf("area %s paths overlap existing feature %s", feature, s.Feature)
		}
	}
	names, err := adoptAreaNames(memDir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == feature {
			continue
		}
		ident, _, err := loadAreaIdentity(memDir, name)
		if err != nil {
			return err
		}
		front, _, err := reconcileArea(memDir, name, ident)
		if err != nil {
			return err
		}
		if pathsOverlap(paths, front.Paths) {
			return fmt.Errorf("area %s paths overlap area %s", feature, name)
		}
	}
	return nil
}

// AdoptInspectArea proposes externally observable behavior within the owner's fixed paths.
func AdoptInspectArea(ctx context.Context, r Repo, feature string, paths []string, title, base string, out io.Writer) (_ AdoptRun, err error) {
	defer func() { err = redactErr(err) }()
	root, unlock, err := openAdoption(r)
	if err != nil {
		return AdoptRun{}, err
	}
	defer unlock()
	defer root.reportCommand(&err)
	a, err := loadAdoptSession(root.repo, feature)
	if err != nil {
		return AdoptRun{}, err
	}
	if len(paths) < 1 || len(paths) > 20 {
		return AdoptRun{}, errors.New("--paths requires 1 through 20 repository-relative patterns")
	}
	for _, p := range paths {
		if err := checkAdoptPattern(p); err != nil {
			return AdoptRun{}, err
		}
	}
	if title != "" {
		if err := checkDisplayLine(title, 120); err != nil {
			return AdoptRun{}, err
		}
		if err := checkNoSecrets("area title", []byte(title)); err != nil {
			return AdoptRun{}, err
		}
	}
	ident, exists, err := loadAreaIdentity(a.memDir, feature)
	if err != nil {
		return AdoptRun{}, err
	}
	if exists {
		if title != "" {
			return AdoptRun{}, errors.New("--title is only for a new area; edit area.md to change the title")
		}
		front, _, err := reconcileArea(a.memDir, feature, ident)
		if err != nil {
			return AdoptRun{}, err
		}
		x, y := append([]string{}, paths...), append([]string{}, front.Paths...)
		sort.Strings(x)
		sort.Strings(y)
		if !reflect.DeepEqual(x, y) {
			return AdoptRun{}, errors.New("--paths must equal the area's recorded paths; changing area paths is out of scope")
		}
	}
	if err := checkAreaOwnership(a.repo, a.memDir, feature, paths); err != nil {
		return AdoptRun{}, err
	}
	cfg, err := a.repo.LoadConfig()
	if err != nil {
		return AdoptRun{}, err
	}
	base, err = resolveAdoptBase(a.repo, base)
	if err != nil {
		return AdoptRun{}, err
	}
	if root.resumedInspections[feature] && a.state.Inspection != nil && a.state.Inspection.Base == base {
		return a.recoveredRun(), nil
	}
	files, err := baseFiles(a.repo, base)
	if err != nil {
		return AdoptRun{}, err
	}
	for _, p := range paths {
		found := false
		for f, regular := range files {
			if regular && matchAny(f, []string{p}) {
				found = true
				break
			}
		}
		if !found {
			return AdoptRun{}, fmt.Errorf("area path %q matches no files at the pinned base", p)
		}
	}
	var reference strings.Builder
	lineCounts := map[string]int{}
	for _, o := range ident.sorted() {
		fmt.Fprintf(&reference, "AC-%d (%s): %s\n", o.ID, o.Status, o.Text)
	}
	return a.runInspection(ctx, base, cfg, promptData{AreaPaths: strings.Join(paths, "\n"), Observations: reference.String(), AreaTitle: title}, areaManifest(),
		func(f frozenOutput, info AdoptInspection) error {
			var tree yaml.Node
			if err := yaml.Unmarshal(f.Files["observations.yaml"], &tree); err != nil {
				return fmt.Errorf("observations.yaml: %v", err)
			}
			if err := checkYAMLScalars("observations.yaml", &tree, nil); err != nil {
				return err
			}
			var doc areaOutput
			dec := yaml.NewDecoder(bytes.NewReader(f.Files["observations.yaml"]))
			dec.KnownFields(true)
			if err := dec.Decode(&doc); err != nil {
				return fmt.Errorf("observations.yaml must hold title, description and observations (id, text, and locations as \"path:line\" strings): %v", err)
			}
			if len(doc.Observations) < 1 || len(doc.Observations) > 30 {
				return errors.New("observations.yaml must hold 1 through 30 observations")
			}
			if doc.Title != "" {
				if err := checkOwnerText("area title", doc.Title, 120); err != nil {
					return err
				}
			}
			if err := checkAreaProse(doc.Description); err != nil {
				return err
			}
			if strings.TrimSpace(doc.Description) == "" {
				return errors.New("area description must contain display Markdown prose")
			}
			var inputs []observationInput
			seen := map[int]bool{}
			for _, o := range doc.Observations {
				if o.ID == nil {
					return errors.New("every observation must specify id: 0 or an existing ACTIVE ID")
				}
				if err := checkObservationText(o.Text); err != nil {
					return err
				}
				if *o.ID != 0 {
					prev := ident.Obs[*o.ID]
					if seen[*o.ID] || prev == nil || prev.Status != ObservationActive {
						return fmt.Errorf("AC-%d is not a unique ACTIVE ID", *o.ID)
					}
					seen[*o.ID] = true
				}
				if len(o.Locations) < 1 || len(o.Locations) > maxObservationLocations {
					return errors.New("each observation needs 1 through 10 locations")
				}
				for _, l := range o.Locations {
					if err := checkObservationLocation(l); err != nil {
						return err
					}
					location := observationLocation.FindStringSubmatch(l)
					p := location[1]
					if !matchAny(p, paths) || !files[p] || possibleAdoptSecret(p, cfg.Adopt.Exclude) {
						return errors.New("observation location is outside the area, excluded, or missing as a file at the pinned base")
					}
					lines, ok := lineCounts[p]
					if !ok {
						blob, err := gitRaw(a.repo.Root, "cat-file", "blob", base+":"+p)
						if err != nil {
							return fmt.Errorf("read observation location at pinned base: %w", err)
						}
						lines = strings.Count(blob, "\n")
						if blob != "" && !strings.HasSuffix(blob, "\n") {
							lines++
						}
						lineCounts[p] = lines
					}
					for _, number := range location[2:] {
						if number == "" {
							continue
						}
						line, err := strconv.Atoi(number)
						if err != nil || line > lines {
							return errors.New("observation location lines exceed the file's line count at the pinned base")
						}
					}
				}
				inputs = append(inputs, observationInput{ID: *o.ID, Text: o.Text, Locations: o.Locations})
			}
			var draft *areaDraft
			if !exists {
				areaTitle := title
				if areaTitle == "" {
					areaTitle = doc.Title
				}
				draft = &areaDraft{Front: areaFront{Title: areaTitle, Paths: paths}, Prose: doc.Description}
				if err := draft.Front.validate(); err != nil {
					return err
				}
			}
			// Persist provenance first. D5 owns the recoverable observation batch.
			a.state.Inspection = &info
			if err := a.save(); err != nil {
				return &adoptRecordError{err}
			}
			_, recordErr := recordAreaObservations(a.repo, feature, draft, inputs, base)
			// D5 saved pending state through another session object; reload it before saving.
			next, err := loadAdoptSession(a.repo, feature)
			if err != nil {
				return &adoptRecordError{err}
			}
			a.state = next.state
			if recordErr != nil {
				return &adoptRecordError{recordErr}
			}
			return nil
		}, out)
}

// A memory or local-state failure stops the command without consuming the next agent try.
type adoptRecordError struct{ error }

func (e *adoptRecordError) Unwrap() error { return e.error }

// AdoptVerifyCommand includes the exact configured command and its process exit status.
type AdoptVerifyCommand struct {
	Name      string  `json:"name"`
	Command   string  `json:"command"`
	Exit      int     `json:"exit"` // -1 when cancelled or the process could not start
	Seconds   float64 `json:"seconds"`
	Passed    bool    `json:"passed"`
	Integrity string  `json:"integrity"`
	Output    string  `json:"output,omitempty"`
	Uncertain bool    `json:"uncertain,omitempty"`
}

// AdoptVerification is local evidence, never an approval or a suggested configuration run.
type AdoptVerification struct {
	Base      string               `json:"base_sha"`
	Date      string               `json:"date"`
	ConfigSHA string               `json:"config_sha256"`
	Passed    bool                 `json:"passed"`
	Commands  []AdoptVerifyCommand `json:"commands"`
	Sandbox   string               `json:"sandbox_image,omitempty"` // image ID when the commands ran sandboxed
}

// AdoptVerify uses the gate shell/process-tree runner with the owner's current configuration.
func AdoptVerify(ctx context.Context, r Repo, base string, out io.Writer) (_ AdoptVerification, err error) {
	defer func() { err = redactErr(err) }()
	a, unlock, err := openAdoption(r)
	if err != nil {
		return AdoptVerification{}, err
	}
	defer unlock()
	defer a.reportCommand(&err)
	cfg, b, err := loadAdoptVerifyConfig(a.repo)
	if err != nil {
		return AdoptVerification{}, err
	}
	base, err = resolveAdoptBase(a.repo, base)
	if err != nil {
		return AdoptVerification{}, err
	}
	w, err := newInspectionWorktree(a.repo, base, nil, false, "adopt-verify")
	if err != nil {
		return AdoptVerification{}, err
	}
	defer w.close()
	// Under the sandbox, the commands run on a full export of the base in a container;
	// the inspection worktree stays for the host-side checks only.
	sb, err := openFlowSandbox(a.repo, cfg, "adopt-verify")
	if err != nil {
		return AdoptVerification{}, err
	}
	var sv *verification
	if sb != nil {
		if sv, err = newVerification(ctx, sb, a.repo.Root, w.dir, base, cfg); err != nil {
			return AdoptVerification{}, err
		}
		defer func() {
			if cerr := sv.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}()
	}
	v := AdoptVerification{Base: base, Date: memoryNow().UTC().Format(time.RFC3339), ConfigSHA: observationDigest(string(b)), Passed: true}
	if sb != nil {
		v.Sandbox = sb.image.ID
	}
	for _, step := range []struct{ name, text string }{{"build", cfg.Commands.Build}, {"test", cfg.Commands.Test}} {
		if err := checkNoSecrets("configured command", []byte(step.text)); err != nil {
			return v, err
		}
		c := AdoptVerifyCommand{Name: step.name, Command: redactDiagnostic(step.text), Integrity: "unchanged", Passed: true}
		if strings.TrimSpace(step.text) != "" && sv != nil {
			g := sv.raw(ctx, step.name, step.text)
			c.Seconds, c.Output, c.Passed, c.Uncertain, c.Exit = g.Seconds, discoveryDiagnostic(g.Output), g.Passed, g.Uncertain, -1
			if g.Class == ClassProcess {
				c.Exit = g.ExitCode
			}
			if broken := sv.damaged(); broken != nil {
				c.Passed, c.Integrity = false, discoveryDiagnostic(broken.Error())
			}
		} else if strings.TrimSpace(step.text) != "" {
			start := time.Now()
			output, runErr := runShell(ctx, w.dir, step.text, time.Duration(cfg.Commands.TimeoutMinutes)*time.Minute, cfg.PassEnv)
			c.Seconds, c.Output, c.Passed = time.Since(start).Seconds(), discoveryDiagnostic(tail(output, maxGateOutput)), runErr == nil
			c.Uncertain = uncertainExit(runErr)
			if runErr != nil {
				c.Exit = -1
				var exit *exec.ExitError
				if errors.As(runErr, &exit) {
					c.Exit = exit.ExitCode()
				}
				if c.Output == "" {
					c.Output = discoveryDiagnostic(runErr.Error())
				}
			}
		}
		if sv == nil {
			if err := w.integrity(true); err != nil {
				c.Passed, c.Integrity = false, discoveryDiagnostic(err.Error())
			}
		}
		v.Commands = append(v.Commands, c)
		v.Passed = v.Passed && c.Passed
		if err := writePrivateJSON(filepath.Join(a.repo.Root, ".aeh", "adopt", "verify.json"), v); err != nil {
			return v, err
		}
		if out != nil {
			fmt.Fprintf(out, "%s at %s: exit %d, %.3fs, passed %t; integrity: %s\ncommand: %s\nconfig SHA-256: %s\n", c.Name, base, c.Exit, c.Seconds, c.Passed, c.Integrity, c.Command, v.ConfigSHA)
		}
		if c.Integrity != "unchanged" || c.Uncertain || ctx.Err() != nil {
			break
		}
	}
	return v, nil
}

// Decode the exact bytes whose digest the verification records, using the existing config
// validation. No second file read can race command selection against its provenance.
func loadAdoptVerifyConfig(r Repo) (Config, []byte, error) {
	var c Config
	b, err := os.ReadFile(r.ConfigPath())
	if err != nil {
		return c, nil, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, nil, fmt.Errorf("parse config: %w", err)
	}
	declared, err := checkConfigYAML(b)
	if err != nil {
		return c, nil, err
	}
	c.Reviewers.declared = declared
	if err := c.validate(); err != nil {
		return c, nil, err
	}
	return c, b, nil
}

// AdoptPromote creates an unapproved 0.1.0 spec containing only kept observations.
func AdoptPromote(r Repo, feature string) (err error) {
	defer func() { err = redactErr(err) }()
	root, unlock, err := openAdoption(r)
	if err != nil {
		return err
	}
	defer unlock()
	defer root.reportCommand(&err)
	a, err := loadAdoptSession(root.repo, feature)
	if err != nil {
		return err
	}
	ident, exists, err := loadAreaIdentity(a.memDir, feature)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("area has no observations; adopt it first")
	}
	front, prose, err := reconcileArea(a.memDir, feature, ident)
	if err != nil {
		return err
	}
	if err := admitWork(a.repo, feature, front.Paths, ""); err != nil {
		return err
	}
	var active []string
	var keep []Observation
	for _, o := range ident.sorted() {
		if o.Status == ObservationActive {
			active = append(active, fmt.Sprintf("AC-%d: %s", o.ID, o.Text))
		}
		if o.Decision == DecisionKeep {
			keep = append(keep, o)
		}
	}
	if len(active) > 0 {
		return fmt.Errorf("decide on active observations before promoting:\n%s", strings.Join(active, "\n"))
	}
	if len(keep) == 0 {
		return errors.New("promote requires at least one keep observation")
	}
	digestBytes, _ := json.Marshal(struct {
		Front  areaFront
		Prose  string
		Events []observationEvent
	}{front, prose, ident.Events})
	digest := observationDigest(string(digestBytes))
	if p := a.state.Promotion; p != nil && p.Content == digest {
		if spec, ok := gitBlob(a.memDir, "HEAD", specRel(feature)); ok && observationDigest(spec) == a.state.PromotedSpec {
			return nil
		}
	}
	if err := checkAreaOwnership(a.repo, a.memDir, feature, front.Paths); err != nil {
		return err
	}
	fm, err := yaml.Marshal(specFront{Feature: feature, Title: front.Title, Version: "0.1.0", Paths: front.Paths})
	if err != nil {
		return err
	}
	var body strings.Builder
	body.WriteString("---\n" + string(fm) + "---\n" + strings.TrimRight(prose, "\n") + "\n\n" + criteriaHeading + "\n")
	for _, o := range keep {
		fmt.Fprintf(&body, "- AC-%d: %s\n", o.ID, o.Text)
	}
	date := memoryNow().UTC().Format("2006-01-02")
	base := ident.Events[len(ident.Events)-1].CodeSHA
	if a.state.Inspection != nil {
		base = a.state.Inspection.Base
	}
	fmt.Fprintf(&body, "\n## Changelog\n- 0.1.0 (%s) promoted from adopted area %s observed at %s; not implemented yet.\n", date, feature, base[:7])
	spec, err := parseSpec(body.String())
	if err != nil {
		return err
	}
	if _, _, err := spec.Criteria(); err != nil {
		return err
	}
	files := map[string][]byte{specRel(feature): []byte(body.String())}
	if err := gateBatchFiles(files); err != nil {
		return err
	}
	a.state.Promotion = &DiscoveryAction{Name: feature, Target: specRel(feature), Content: digest, Date: date, Paths: front.Paths}
	a.state.PromotedSpec = observationDigest(body.String())
	return a.record(files, "adopt "+feature+": promote", "promote "+feature)
}

// AdoptAreaStatus is intentionally extensible for characterized/published/merged in D7.
type AdoptAreaStatus struct {
	Feature string   `json:"feature"`
	State   string   `json:"state"`
	Paths   []string `json:"paths"`
}

// AdoptStatusSnapshot describes coverage at the accepted inventory's immutable base.
type AdoptStatusSnapshot struct {
	Inventory    string             `json:"inventory"`
	Base         string             `json:"base_sha,omitempty"`
	Date         string             `json:"date,omitempty"`
	Areas        []AdoptAreaStatus  `json:"areas"`
	Uncovered    []string           `json:"uncovered"`
	Verification *AdoptVerification `json:"verification,omitempty"`
}

func adoptionStatus(a *adoptSession) (AdoptStatusSnapshot, error) {
	st := AdoptStatusSnapshot{Inventory: "none"}
	if a.feature != "" {
		var err error
		a, err = loadAdoptState(a.repo, "")
		if err != nil {
			return st, err
		}
	}
	if a.state.Current != nil {
		st.Inventory, st.Base, st.Date = "current", a.state.Current.Base, a.state.Current.Date
	}
	if a.state.Next != nil {
		st.Inventory = "next pending acceptance"
	}
	memory, err := LoadMemory(a.repo)
	if err != nil {
		return st, err
	}
	var paths []string
	for _, spec := range memory.Specs {
		paths = append(paths, spec.Paths...)
	}
	names, err := adoptAreaNames(a.memDir)
	if err != nil {
		return st, err
	}
	for _, name := range names {
		ident, _, err := loadAreaIdentity(a.memDir, name)
		if err != nil {
			return st, err
		}
		front, _, err := reconcileArea(a.memDir, name, ident)
		if err != nil {
			return st, err
		}
		state := "confirmed"
		for _, o := range ident.Obs {
			if o.Status == ObservationActive {
				state = "observed"
			}
		}
		if spec, ok := memory.Spec(name); ok {
			if state, err = characterizationState(a.repo, spec); err != nil {
				return st, err
			}
		}
		st.Areas = append(st.Areas, AdoptAreaStatus{Feature: name, State: state, Paths: front.Paths})
		paths = append(paths, front.Paths...)
	}
	if st.Base != "" {
		files, err := baseFiles(a.repo, st.Base)
		if err != nil {
			return st, err
		}
		// Group uncovered files after matching ownership, so a covered sibling never hides
		// an unowned file in the same two-segment group.
		groups := map[string]bool{}
		for f := range files {
			if matchAny(f, paths) {
				continue
			}
			parts := strings.Split(f, "/")
			group := strings.Join(parts[:min(2, len(parts))], "/")
			groups[group] = true
		}
		for group := range groups {
			st.Uncovered = append(st.Uncovered, discoveryDiagnostic(group))
		}
		sort.Strings(st.Uncovered)
	}
	if b, err := os.ReadFile(filepath.Join(a.repo.Root, ".aeh", "adopt", "verify.json")); err == nil {
		var v AdoptVerification
		if err := json.Unmarshal(b, &v); err != nil {
			return st, err
		}
		st.Verification = &v
	}
	return st, nil
}

// characterizationState is the state of a promoted area: promoted until a characterization
// plan runs; characterizing while it runs or is blocked; characterized once its sealed tests
// are verified; published once aeh/<ID> holds them and memory records that commit; merged once
// the main checkout's HEAD contains it.
func characterizationState(r Repo, spec Spec) (string, error) {
	if im, ok := spec.LastImplementation(); ok {
		if _, err := git(r.Root, "merge-base", "--is-ancestor", im.Code, "HEAD"); err == nil {
			return "merged", nil
		}
		return "published", nil
	}
	if spec.Approval == nil {
		return "promoted", nil
	}
	id, err := generatedOutcomeID(spec.Feature, spec.Version)
	if err != nil {
		return "", err
	}
	plan, ok, err := LoadPlanState(r, id)
	if err != nil || !ok || plan.Kind != PlanCharacterize {
		return "promoted", err
	}
	switch {
	case plan.Phase == PlanPublishing || plan.Phase == PlanPublished || plan.Phase == PlanLogged:
		return "characterized", nil
	case plan.Status == StatusBlocked:
		return "characterizing (blocked)", nil
	}
	return "characterizing", nil
}

// AdoptStatus holds the project lock and regenerates the adoption report.
func AdoptStatus(r Repo) (_ AdoptStatusSnapshot, err error) {
	defer func() { err = redactErr(err) }()
	a, unlock, err := openAdoption(r)
	if err != nil {
		return AdoptStatusSnapshot{}, err
	}
	defer unlock()
	defer a.reportCommand(&err)
	return adoptionStatus(a)
}
