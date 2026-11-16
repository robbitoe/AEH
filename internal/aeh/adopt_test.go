package aeh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Shell fixtures get the absolute output directory from the runner's prompt. Their cwd is
// the inspected checkout, unlike discovery agents whose cwd is their output directory.
const adoptOutputDir = `set -eu
out=$(printf '%s\n' "$1" | sed -n '/^Write output ONLY into this absolute directory opened by aeh:/{n;p;}')
[ -d "$out" ]
`

const inventoryOK = adoptOutputDir + `cat > "$out/system.md" <<'EOF'
# Existing system

Billing computes fees in src/billing/fee.txt:1. There are no billing tests in src/billing/fee.txt:1.
EOF
cat > "$out/config.suggested.yaml" <<'EOF'
commands:
  build: echo suggested > suggested-marker
  test: echo suggested > suggested-marker
EOF
echo 'INVENTORY: READY'
`

const areaOK = adoptOutputDir + `cat > "$out/observations.yaml" <<'EOF'
title: Billing
description: "# Billing\n\nComputes fees.\n"
observations:
  - id: 0
    text: Given an order of 100, when billed, then the fee is 2.
    locations: [src/billing/fee.txt:1]
  - id: 0
    text: Given a refund, when billed, then the fee is kept.
    locations: [src/billing/fee.txt:2-3]
  - id: 0
    text: Given no currency, when billed, then USD is assumed.
    locations: [src/billing/fee.txt:3]
EOF
echo 'AREA: READY'
`

func newAdoptFixture(t *testing.T, script string) *discoveryFixture {
	t.Helper()
	f := newDiscoveryFixture(t, script)
	write(t, f.dir, "src/billing/fee.txt", "fee 2\nrefund keeps fee\nUSD\n")
	write(t, f.dir, "src/users/user.txt", "user\n")
	write(t, f.dir, "docs/guide.txt", "guide\n")
	write(t, f.dir, ".gitignore", ".aeh/\nignored/\n")
	write(t, f.dir, "tests/build.sh", "set -eu\n[ ! -e suggested-marker ]\nmkdir -p ignored\necho build > ignored/build.txt\n")
	write(t, f.dir, "tests/test.sh", "set -eu\n[ ! -e suggested-marker ]\n[ -e ignored/build.txt ]\n")
	f.adoptConfig("")
	f.commitCode("adoption fixture")
	return f
}

func (f *discoveryFixture) adoptConfig(extra string) {
	f.t.Helper()
	f.writeConfig(extra)
	b, err := os.ReadFile(f.repo.ConfigPath())
	if err != nil {
		f.t.Fatal(err)
	}
	text := strings.Replace(string(b), "  test: \"git --version\"", "  build: \"sh tests/build.sh\"\n  test: \"sh tests/test.sh\"\n  timeout_minutes: 1", 1)
	write(f.t, f.dir, "aeh/config.yaml", text)
}

func (f *discoveryFixture) commitCode(subject string) string {
	f.t.Helper()
	mustGit(f.t, f.dir, "add", "-A")
	mustGit(f.t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", subject)
	return mustGit(f.t, f.dir, "rev-parse", "HEAD")
}

func (f *discoveryFixture) inventory(base string) AdoptRun {
	f.t.Helper()
	var log strings.Builder
	st, err := AdoptInventory(context.Background(), f.repo, base, &log)
	if err != nil || st.Status != "accepted" {
		f.t.Fatalf("inventory: %+v %v\n%s", st, err, log.String())
	}
	return st
}

func (f *discoveryFixture) inspectArea() AdoptRun {
	f.t.Helper()
	var log strings.Builder
	st, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "", "", &log)
	if err != nil || st.Status != "accepted" {
		f.t.Fatalf("area: %+v %v\n%s", st, err, log.String())
	}
	return st
}

func TestAdoptInventoryAndAcceptance(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	marker := filepath.Join(f.tmp, "suggested-marker")
	write(t, f.tmp, "suggested-marker.sh", "echo executed > \""+filepath.ToSlash(marker)+"\"\n")
	script := strings.ReplaceAll(inventoryOK, "echo suggested > suggested-marker", "sh "+filepath.ToSlash(filepath.Join(f.tmp, "suggested-marker.sh")))
	f.setAgent(script)
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	st := f.inventory("")
	if st.Inspection.Base != base || !isSHA256Hex(st.Inspection.Snapshot) {
		t.Fatalf("provenance: %+v", st)
	}
	text := f.memFile("wiki/system.md")
	for _, want := range []string{base, "Date: ", "Profile: agent", "src/billing/fee.txt:1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("system.md lacks %q:\n%s", want, text)
		}
	}
	suggested := filepath.Join(f.dir, ".aeh", "adopt", "config.suggested.yaml")
	if b, err := os.ReadFile(suggested); err != nil || !strings.Contains(string(b), "suggested-marker") {
		t.Fatalf("suggested: %s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "suggested-marker")); !os.IsNotExist(err) {
		t.Fatal("suggested command executed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("suggested command created the absolute marker")
	}
	if tracked := mustGit(t, f.dir, "ls-files", ".aeh"); tracked != "" {
		t.Fatalf("local config tracked: %s", tracked)
	}
	if tracked := mustGit(t, memDir(t, f.repo), "ls-files", "*suggested*"); tracked != "" {
		t.Fatalf("suggested config imported: %s", tracked)
	}
	if _, err := os.Stat(f.repo.ReportPath("adopt")); err != nil {
		t.Fatal(err)
	}
	f.setAgent(strings.Replace(script, "Existing system", "Refreshed system", 1))
	f.inventory(base[:10])
	if f.memFile("wiki/system.md") != text {
		t.Fatal("refresh overwrote current")
	}
	next := f.memFile("wiki/system.next.md")
	status, err := AdoptStatus(f.repo)
	if err != nil || status.Inventory != "next pending acceptance" {
		t.Fatalf("status: %+v %v", status, err)
	}
	mem := memDir(t, f.repo)
	before := mustGit(t, mem, "rev-list", "--count", "HEAD")
	beforeHead := mustGit(t, mem, "rev-parse", "HEAD")
	if err := AdoptAcceptSystem(f.repo); err != nil {
		t.Fatal(err)
	}
	if f.memFile("wiki/system.md") != next {
		t.Fatal("accept did not replace current")
	}
	after := mustGit(t, mem, "rev-list", "--count", "HEAD")
	if before == after {
		t.Fatal("accept made no commit")
	}
	if count := mustGit(t, mem, "rev-list", "--count", beforeHead+"..HEAD"); count != "1" {
		t.Fatalf("accept made %s commits", count)
	}
	head := mustGit(t, mem, "rev-parse", "HEAD")
	if err := AdoptAcceptSystem(f.repo); err != nil {
		t.Fatal(err)
	}
	if mustGit(t, mem, "rev-parse", "HEAD") != head {
		t.Fatal("accept was not idempotent")
	}
	status, err = AdoptStatus(f.repo)
	if err != nil || status.Inventory != "current" || status.Base != base {
		t.Fatalf("status: %+v %v", status, err)
	}
}

func TestAdoptExcludesSecretsBeforeAgentAndPinsBase(t *testing.T) {
	endToEnd(t)
	check := `set -eu
for p in .ENV.prod cert.PEM tls.key id_rsa cert.pfx credentials.json secrets.txt private/config.txt; do
  [ ! -e "$p" ]
  printf '%s\n' "$1" | grep -F "$p" >/dev/null
done
[ "$(cat src/billing/fee.txt | head -n 1)" = 'fee 2' ]
printf '%s\n' "$1" | grep -F 'Excluded as possible secrets' >/dev/null
`
	f := newAdoptFixture(t, check+inventoryOK)
	for _, p := range []string{".ENV.prod", "cert.PEM", "tls.key", "id_rsa", "cert.pfx", "credentials.json", "secrets.txt", "private/config.txt"} {
		write(t, f.dir, p, "ghp_"+strings.Repeat("S", 36)+"\n")
	}
	f.adoptConfig("adopt:\n  ladder: [agent]\n  exclude: [private/**]\n")
	base := f.commitCode("secret fixture")
	write(t, f.dir, "src/billing/fee.txt", "later\n")
	f.commitCode("later code")
	st := f.inventory(base[:8])
	if st.Inspection.Base != base || st.Inspection.Excluded != 8 {
		t.Fatalf("exclusions: %+v", st)
	}
	if len(st.Attempts) != 1 {
		t.Fatalf("excluded files reached agent: %+v", st.Attempts)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, ".ENV.prod")); !strings.Contains(string(b), "ghp_") {
		t.Fatal("main checkout secret changed")
	}
	prompt, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "runs", "adopt", "a1", "adopt.prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), strings.Repeat("S", 36)) {
		t.Fatal("secret reached prompt")
	}
}

func TestAdoptRejectsCodeWritesIncludingIgnoredAndRetries(t *testing.T) {
	endToEnd(t)
	for _, mutation := range []string{"echo changed > src/billing/fee.txt", "mkdir -p ignored; echo new > ignored/new.txt", "echo leaked > .env"} {
		t.Run(mutation, func(t *testing.T) {
			f := newAdoptFixture(t, mutation+"\n"+inventoryOK)
			st, err := AdoptInventory(context.Background(), f.repo, "", nil)
			if err != nil || st.Status != StatusBlocked || len(st.Attempts) != 2 {
				t.Fatalf("write: %+v %v", st, err)
			}
			for _, a := range st.Attempts {
				if !a.WroteCode || !strings.Contains(a.Feedback, "wrote into the code") {
					t.Fatalf("write not recorded: %+v", a)
				}
			}
			if _, ok := gitBlob(memDir(t, f.repo), "HEAD", "wiki/system.md"); ok {
				t.Fatal("writer output imported")
			}
			if b, _ := os.ReadFile(filepath.Join(f.dir, "src/billing/fee.txt")); !strings.HasPrefix(string(b), "fee 2") {
				t.Fatal("main checkout modified")
			}
			f.setAgent(inventoryOK)
			st = f.inventory("")
			if len(st.Attempts) != 1 || st.Attempts[0].N != 3 {
				t.Fatalf("retry did not reset ladder: %+v", st.Attempts)
			}
		})
	}
}

func TestAdoptAreaRefreshOwnershipAndLocations(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, areaOK)
	f.inspectArea()
	area, err := LoadAdoptArea(f.repo, "billing")
	if err != nil || len(area.Observations) != 3 {
		t.Fatalf("area: %+v %v", area, err)
	}
	if area.Title != "Billing" || area.Observations[0].CodeSHA != mustGit(t, f.dir, "rev-parse", "HEAD") {
		t.Fatalf("area: %+v", area)
	}
	refresh := strings.Replace(areaOK, "  - id: 0", "  - id: 1", 1)
	// Re-observe only AC-1; the other active observations remain available for a decision.
	refresh = strings.Replace(refresh, "Given an order of 100, when billed, then the fee is 2.", "Given an order, when billed, then a fee of 2 is shown.", 1)
	i := strings.Index(refresh, "  - id: 0")
	refresh = refresh[:i] + "EOF\necho 'AREA: READY'\n"
	f.setAgent(refresh)
	f.inspectArea()
	area, err = LoadAdoptArea(f.repo, "billing")
	if err != nil || len(area.Observations) != 3 || !strings.Contains(area.Observations[0].Text, "is shown") {
		t.Fatalf("refresh: %+v %v", area, err)
	}
	prompt, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "runs", "adopt-billing", "a2", "adopt.prompt.md"))
	if err != nil || !strings.Contains(string(prompt), "AC-1 (active): Given an order of 100") {
		t.Fatalf("reference: %s %v", prompt, err)
	}
	// An owner error must fail before any agent starts.
	f.setAgent("echo 'unexpected agent' > src/billing/fee.txt\n" + areaOK)
	if _, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "Renamed", "", nil); err == nil || !strings.Contains(err.Error(), "edit area.md") {
		t.Fatalf("existing area title: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "runs", "adopt-billing", "a3")); !os.IsNotExist(err) {
		t.Fatal("agent ran before title check")
	}
	for _, paths := range [][]string{{"src/"}, {"src/missing/"}, {"../src/"}} {
		if _, err := AdoptInspectArea(context.Background(), f.repo, "billing", paths, "", "", nil); err == nil {
			t.Fatalf("changed paths accepted: %v", paths)
		}
	}
	if _, err := AdoptInspectArea(context.Background(), f.repo, "other", []string{"src/billing/"}, "Other", "", nil); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "runs", "adopt-other")); !os.IsNotExist(err) {
		t.Fatal("agent ran before ownership check")
	}
	for _, bad := range []string{"src/users/user.txt:1", "src/billing/missing.txt:1"} {
		f.setAgent(strings.ReplaceAll(refresh, "src/billing/fee.txt:1", bad))
		st, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "", "", nil)
		if err != nil || st.Status != StatusBlocked || !strings.Contains(st.Attempts[len(st.Attempts)-1].Feedback, "observation location") {
			t.Fatalf("bad location: %+v %v", st, err)
		}
	}
}

func TestAdoptVerifyUsesOwnerCommandsAndDetectsChanges(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	f.inventory("")
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	var log strings.Builder
	v, err := AdoptVerify(context.Background(), f.repo, "", &log)
	if err != nil || !v.Passed || len(v.Commands) != 2 {
		t.Fatalf("verify: %+v %v\n%s", v, err, log.String())
	}
	b, _ := os.ReadFile(f.repo.ConfigPath())
	if v.Base != base || v.ConfigSHA != observationDigest(string(b)) || v.Commands[0].Command != "sh tests/build.sh" || v.Commands[1].Exit != 0 {
		t.Fatalf("provenance: %+v", v)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "ignored", "build.txt")); !os.IsNotExist(err) {
		t.Fatal("verify ran in main checkout")
	}
	for _, scenario := range []struct {
		name, script, want string
		exit               int
	}{
		{"tracked", "echo changed > src/billing/fee.txt\n", "changed an existing file", 0},
		{"untracked", "echo new > new.txt\n", "not ignored", 0},
		{"new ignore", "mkdir -p new\necho '*' > new/.gitignore\necho hidden > new/output.txt\n", "not ignored", 0},
		{"ignored new ignore", "mkdir -p ignored\necho '*' > ignored/.gitignore\necho hidden > ignored/output.txt\necho build > ignored/build.txt\n", "unchanged", 0},
		{"exit", "exit 7\n", "unchanged", 7},
	} {
		write(t, f.dir, "tests/build.sh", scenario.script)
		f.commitCode(scenario.name)
		v, err = AdoptVerify(context.Background(), f.repo, "", nil)
		wantPassed := scenario.name == "ignored new ignore"
		if err != nil || v.Passed != wantPassed || !strings.Contains(v.Commands[0].Integrity, scenario.want) || v.Commands[0].Exit != scenario.exit {
			t.Fatalf("%s: %+v %v", scenario.name, v, err)
		}
		stored, err := os.ReadFile(filepath.Join(f.dir, ".aeh", "adopt", "verify.json"))
		if err != nil || !strings.Contains(string(stored), v.Base) {
			t.Fatalf("verify evidence: %s %v", stored, err)
		}
	}
}

func TestAdoptPromoteKeepsGapsAndRetiresAreaIDs(t *testing.T) {
	// The D5 fixture deliberately retires its highest ID, above every kept criterion.
	r, mem := newAreaProject(t)
	if err := AdoptPromote(r, "billing"); err == nil || !strings.Contains(err.Error(), "AC-1:") {
		t.Fatalf("active: %v", err)
	}
	for id, decision := range []string{DecisionKeep, DecisionDrop, DecisionBug} {
		o := observation(t, r, id+1)
		if _, err := AdoptConfirm(r, "billing", fmt.Sprintf("AC-%d", id+1), decision, o.Token()); err != nil {
			t.Fatal(err)
		}
	}
	// Add another active observation after the retired numbers to make the kept gap visible.
	_, err := recordAreaObservations(r, "billing", nil, []observationInput{{Text: "Given a coupon, when billed, then a discount is shown.", Locations: []string{"src/billing/fee.go:1"}}}, obsSHA)
	if err != nil {
		t.Fatal(err)
	}
	o := observation(t, r, 4)
	if _, err := AdoptConfirm(r, "billing", "AC-4", DecisionKeep, o.Token()); err != nil {
		t.Fatal(err)
	}
	// Reserve a final retired number above the largest kept number.
	_, err = recordAreaObservations(r, "billing", nil, []observationInput{{Text: "Given a token, when billed, then it expires.", Locations: []string{"src/billing/fee.go:2"}}}, obsSHA)
	if err != nil {
		t.Fatal(err)
	}
	o = observation(t, r, 5)
	if _, err := AdoptConfirm(r, "billing", "AC-5", DecisionDrop, o.Token()); err != nil {
		t.Fatal(err)
	}
	st, err := AdoptStatus(r)
	if err != nil || len(st.Areas) != 1 || st.Areas[0].State != "confirmed" {
		t.Fatalf("confirmed: %+v %v", st, err)
	}
	if err := AdoptPromote(r, "billing"); err != nil {
		t.Fatal(err)
	}
	text := memRead(t, mem, specRel("billing"))
	sp, err := parseSpec(text)
	if err != nil || sp.Version != "0.1.0" {
		t.Fatalf("spec: %+v %v", sp, err)
	}
	criteria, present, err := sp.Criteria()
	if err != nil || !present || len(criteria) != 2 || criteria[0].ID != 1 || criteria[1].ID != 4 {
		t.Fatalf("criteria: %+v %v", criteria, err)
	}
	if !strings.Contains(text, "observed at 1111111; not implemented yet.") {
		t.Fatalf("changelog: %s", text)
	}
	head := mustGit(t, mem, "rev-parse", "HEAD")
	if err := AdoptPromote(r, "billing"); err != nil {
		t.Fatal(err)
	}
	if mustGit(t, mem, "rev-parse", "HEAD") != head {
		t.Fatal("promotion was not idempotent")
	}
	approved, _, err := ApproveSpec(r, "billing")
	if err != nil || approved.Approval == nil || approved.Approval.MaxCriterion < 5 {
		t.Fatalf("approval: %+v %v", approved.Approval, err)
	}
	st, err = AdoptStatus(r)
	if err != nil || st.Areas[0].State != "promoted" {
		t.Fatalf("promoted: %+v %v", st, err)
	}
	for _, retired := range []int{2, 3, 5} {
		changed := strings.Replace(text, "version: 0.1.0", "version: 0.2.0", 1)
		changed = strings.Replace(changed, "## Changelog", fmt.Sprintf("- AC-%d: Given new input, when billed, then it succeeds.\n\n## Changelog", retired), 1)
		write(t, mem, specRel("billing"), changed)
		mustGit(t, mem, "add", "-A")
		mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "attempt retired number")
		if _, _, err := ApproveSpec(r, "billing"); err == nil || !strings.Contains(err.Error(), "retired") {
			t.Fatalf("reused AC-%d: %v", retired, err)
		}
	}
}

func TestAdoptPromotionConflictsAndKeepRequired(t *testing.T) {
	r, _ := newAreaProject(t)
	for id := 1; id <= 3; id++ {
		o := observation(t, r, id)
		if _, err := AdoptConfirm(r, "billing", fmt.Sprintf("AC-%d", id), DecisionDrop, o.Token()); err != nil {
			t.Fatal(err)
		}
	}
	if err := AdoptPromote(r, "billing"); err == nil || !strings.Contains(err.Error(), "at least one keep") {
		t.Fatalf("empty promotion: %v", err)
	}
	_, err := recordAreaObservations(r, "billing", nil, []observationInput{{Text: "Given an order, when billed, then it works.", Locations: []string{"src/billing/fee.go:1"}}}, obsSHA)
	if err != nil {
		t.Fatal(err)
	}
	o := observation(t, r, 4)
	if _, err := AdoptConfirm(r, "billing", "AC-4", DecisionKeep, o.Token()); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSpec(r, "other", "Other", []string{"src/"}); err != nil {
		t.Fatal(err)
	}
	if err := AdoptPromote(r, "billing"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("conflict: %v", err)
	}
}

func TestAdoptStatusUsesAcceptedInventoryBase(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	st, err := AdoptStatus(f.repo)
	if err != nil || st.Inventory != "none" {
		t.Fatalf("none: %+v %v", st, err)
	}
	f.inventory("")
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	f.setAgent(areaOK)
	f.inspectArea()
	write(t, f.dir, "later/new.txt", "later\n")
	f.commitCode("after inventory")
	st, err = AdoptStatus(f.repo)
	if err != nil || st.Base != base || st.Date == "" || st.Areas[0].State != "observed" {
		t.Fatalf("status: %+v %v", st, err)
	}
	groups := strings.Join(st.Uncovered, "\n")
	if !strings.Contains(groups, "src/users") || !strings.Contains(groups, "docs/guide.txt") || strings.Contains(groups, "src/billing") || strings.Contains(groups, "later") {
		t.Fatalf("groups: %s", groups)
	}
	if _, err := CreateSpec(f.repo, "users", "Users", []string{"src/users/"}); err != nil {
		t.Fatal(err)
	}
	st, err = AdoptStatus(f.repo)
	if err != nil || strings.Contains(strings.Join(st.Uncovered, "\n"), "src/users") {
		t.Fatalf("feature coverage: %+v %v", st, err)
	}
}

func TestAdoptStrictConfigAndDecodedSecrets(t *testing.T) {
	for _, text := range []string{"unknown: yes", "ladder: [missing]", "exclude: [../secret]", "exclude: [a/../../b]", "exclude: [a/./b]", "exclude: [/etc]", "exclude: ['C:/secret']", "exclude: ['a\\b']", "exclude: ['private/[']"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("adopt:\n  "+text+"\n"), &cfg); err == nil {
			if err := cfg.validateAdoptConfig(); err == nil {
				t.Errorf("accepted %q", text)
			}
		}
	}
	for _, p := range []string{"nested/.ENV.production", "Cert.PEM", "foo/TLS.KEY", "ID_ed25519", "a/Cert.PFX", "Credentials.txt", "Secrets.yaml", "private/x.txt"} {
		if !possibleAdoptSecret(p, []string{"private/**"}) {
			t.Errorf("not excluded: %s", p)
		}
	}
	for _, b := range []string{"commands: {}\n---\ncommands: {}", "a: &x value\nb: *x", "commands: !tag value", "commands: {test: echo hi, surprise: nope}", "commands:\n  test: \"ghp_" + strings.Repeat("\\x41", 36) + "\""} {
		if err := checkSuggested([]byte(b)); err == nil {
			t.Errorf("accepted unsafe YAML: %s", b)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("ladder: [a]\nprofiles:\n  a:\n    command: [echo]\nadopt:\n  exclude: [private/**, aeh/config.yaml]\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateAdoptConfig(); err != nil || len(cfg.adopt().Ladder) != 1 || cfg.adopt().Ladder[0] != "a" {
		t.Fatalf("defaults: %+v %v", cfg.adopt(), err)
	}
}

func TestAdoptResumesPendingInventoryBeforeAnyCommand(t *testing.T) {
	r := newMemoryProject(t)
	a, err := loadAdoptState(r, "")
	if err != nil {
		t.Fatal(err)
	}
	base := mustGit(t, r.Root, "rev-parse", "HEAD")
	info := &AdoptInspection{Base: base, Date: "2026-11-15", Profile: "agent"}
	a.state.Current, a.state.Inspection = info, info
	a.state.Suggested = []byte("commands:\n  test: echo suggested-marker\n")
	files := map[string][]byte{"wiki/system.md": []byte("# System\n\nInspected base " + base + ".\n")}
	a.state.Pending, err = newPendingImport(a.memDir, files, "inventory", "inventory")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after writing some, but not all, of the memory batch.
	write(t, a.memDir, "wiki/system.md", string(files["wiki/system.md"]))
	st, err := AdoptStatus(r)
	if err != nil || st.Inventory != "current" {
		t.Fatalf("resume: %+v %v", st, err)
	}
	if got := memRead(t, a.memDir, "wiki/system.md"); got != string(files["wiki/system.md"]) {
		t.Fatal("pending bytes changed")
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".aeh", "adopt", "config.suggested.yaml")); err != nil {
		t.Fatal(err)
	}
	head := mustGit(t, a.memDir, "rev-parse", "HEAD")
	if _, err := AdoptStatus(r); err != nil {
		t.Fatal(err)
	}
	if mustGit(t, a.memDir, "rev-parse", "HEAD") != head {
		t.Fatal("resume imported twice")
	}
	var state adoptState
	b, _ := os.ReadFile(a.path)
	if err := json.Unmarshal(b, &state); err != nil || state.Pending != nil {
		t.Fatalf("pending not cleared: %+v %v", state, err)
	}
}

func TestLoadAdoptAreaRedactsSessionErrors(t *testing.T) {
	r := newMemoryProject(t)
	secret := "ghp_" + strings.Repeat("T", 36)
	write(t, r.Root, ".aeh/adopt/billing/state.json", `{"version":1,"feature":"`+secret+`"}`)
	if _, err := LoadAdoptArea(r, "billing"); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("session error leaked: %v", err)
	}
}

func TestAdoptRejectsSecretsAndEscalatesWithSafeFeedback(t *testing.T) {
	endToEnd(t)
	secret := "ghp_" + strings.Repeat("A", 36)
	f := newAdoptFixture(t, "set -eu\n"+`printf '%s\n' "$1" | grep -q 'possible GitHub token'`+"\n"+inventoryOK)
	write(t, f.tmp, "bad.sh", adoptOutputDir+`printf '%s\n' '`+secret+`' > "$out/system.md"
echo 'INVENTORY: READY'
`)
	f.adoptConfig("adopt:\n  ladder: [bad, agent]\n")
	b, err := os.ReadFile(f.repo.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	config := string(b) + fmt.Sprintf("  bad:\n    command: [sh, %q, \"{{prompt}}\"]\n    timeout_minutes: 1\n", filepath.ToSlash(filepath.Join(f.tmp, "bad.sh")))
	write(t, f.dir, "aeh/config.yaml", config)
	st := f.inventory("")
	if len(st.Attempts) != 3 || st.Attempts[0].Profile != "bad" || st.Attempts[1].Profile != "bad" || st.Attempts[2].Profile != "agent" {
		t.Fatalf("ladder: %+v", st.Attempts)
	}
	for _, p := range []string{filepath.Join(f.dir, ".aeh", "adopt", "state.json"), f.repo.ReportPath("adopt"), filepath.Join(f.dir, ".aeh", "runs", "adopt", "a3", "adopt.prompt.md")} {
		b, err := os.ReadFile(p)
		if err != nil || strings.Contains(string(b), secret) {
			t.Fatalf("unsafe feedback in %s: %v", p, err)
		}
	}
}

func TestAdoptResumesPartialAreaImportFromStatus(t *testing.T) {
	r := newMemoryProject(t)
	a, err := loadAdoptSession(r, "billing")
	if err != nil {
		t.Fatal(err)
	}
	text := "Given an order, when billed, then a fee is shown."
	ident, err := replayObservations([]observationEvent{{Event: "observe", ID: 1, Text: text, TextSHA: observationDigest(text), CodeSHA: obsSHA, Locations: []string{"src/billing/fee.go:1"}, Date: "2026-11-15"}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := a.areaFiles(areaFront{Title: "Billing", Paths: []string{"src/billing/"}}, "# Billing\n", ident)
	if err != nil {
		t.Fatal(err)
	}
	a.state.Pending, err = newPendingImport(a.memDir, files, "adopt billing", "observe")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	write(t, a.memDir, areaRel("billing", "observations.log.yaml"), string(files[areaRel("billing", "observations.log.yaml")]))
	st, err := AdoptStatus(r)
	if err != nil || len(st.Areas) != 1 || st.Areas[0].State != "observed" {
		t.Fatalf("partial area recovery: %+v %v", st, err)
	}
	if err := memoryClean(a.memDir); err != nil {
		t.Fatal(err)
	}
	// Simulate the other crash boundary: the batch commit exists but pending is not cleared.
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	head := mustGit(t, a.memDir, "rev-parse", "HEAD")
	if _, err := AdoptStatus(r); err != nil {
		t.Fatal(err)
	}
	if mustGit(t, a.memDir, "rev-parse", "HEAD") != head {
		t.Fatal("already committed area imported twice")
	}
}

// Git Bash may emulate ln -s with a copy, and Windows may deny native symlinks.
// Exercise the same shell operation used by the build before relying on it.
func requireAdoptSymlinks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "target", "target\n")
	cmd := exec.Command("sh", "-c", "ln -s target link")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("shell cannot create symlinks: %v %s", err, out)
	}
	info, err := os.Lstat(filepath.Join(dir, "link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Skip("shell does not create native symlinks")
	}
}

func TestAdoptTrackedSymlinksAreExcluded(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	link := "src/billing/external.txt"
	outside := filepath.Join(f.tmp, "outside.txt")
	write(t, f.tmp, "outside.txt", "outside must stay private\n")
	if err := os.Symlink(outside, filepath.Join(f.dir, filepath.FromSlash(link))); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	base := f.commitCode("tracked symlink")
	if entry := mustGit(t, f.dir, "ls-tree", base, "--", link); !strings.HasPrefix(entry, "120000 blob ") {
		t.Skip("git does not track native symlinks")
	}
	check := `set -eu
[ ! -e src/billing/external.txt ]
[ ! -L src/billing/external.txt ]
printf '%s\n' "$1" | grep -F 'src/billing/external.txt' >/dev/null
`
	f.setAgent(check + inventoryOK)
	if st := f.inventory(""); st.Inspection.Excluded != 1 || len(st.Attempts) != 1 {
		t.Fatalf("inventory exclusions: %+v", st)
	}
	f.setAgent(check + areaOK)
	if st := f.inspectArea(); st.Inspection.Excluded != 1 || len(st.Attempts) != 1 {
		t.Fatalf("area exclusions: %+v", st)
	}
	if v, err := AdoptVerify(context.Background(), f.repo, "", nil); err != nil || !v.Passed {
		t.Fatalf("unchanged tracked link failed verification: %+v %v", v, err)
	}
	f.setAgent(strings.ReplaceAll(areaOK, "src/billing/fee.txt:1", link+":1"))
	st, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "", "", nil)
	if err != nil || st.Status != StatusBlocked || !strings.Contains(st.Attempts[0].Feedback, "missing as a file") {
		t.Fatalf("link location accepted: %+v %v", st, err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "outside must stay private\n" {
		t.Fatalf("link target changed: %q %v", b, err)
	}
}

func TestAdoptVerifySymlinks(t *testing.T) {
	endToEnd(t)
	requireAdoptSymlinks(t)
	f := newAdoptFixture(t, inventoryOK)
	for _, tc := range []struct {
		name, script string
		passed       bool
	}{
		{"ignored", "mkdir -p ignored\nln -s ../src ignored/source\necho build > ignored/build.txt\n", true},
		{"tracked file", "rm src/billing/fee.txt\nln -s ../users/user.txt src/billing/fee.txt\n", false},
		{"tracked directory", "rm -r src/billing\nln -s users src/billing\n", false},
		{"unignored", "ln -s src/source missing-link\n", false},
	} {
		write(t, f.dir, "tests/build.sh", "set -eu\n"+tc.script)
		f.commitCode(tc.name)
		v, err := AdoptVerify(context.Background(), f.repo, "", nil)
		if err != nil || v.Passed != tc.passed || len(v.Commands) == 0 || v.Commands[0].Exit != 0 {
			t.Fatalf("%s: %+v %v", tc.name, v, err)
		}
		if tc.passed && v.Commands[0].Integrity != "unchanged" || !tc.passed && v.Commands[0].Integrity == "unchanged" {
			t.Fatalf("%s integrity: %+v", tc.name, v)
		}
	}
}

func TestAdoptExcludesGitSymlinkPlaceholders(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	mustGit(t, f.dir, "config", "core.symlinks", "false")
	link := "src/billing/external.txt"
	write(t, f.dir, link, "../../outside.txt")
	blob := mustGit(t, f.dir, "hash-object", "-w", "--", link)
	mustGit(t, f.dir, "update-index", "--add", "--cacheinfo", "120000,"+blob+","+link)
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "symlink placeholder")
	f.setAgent(`set -eu
[ ! -e src/billing/external.txt ]
printf '%s\n' "$1" | grep -F 'src/billing/external.txt' >/dev/null
` + inventoryOK)
	if st := f.inventory(""); st.Inspection.Excluded != 1 || len(st.Attempts) != 1 {
		t.Fatalf("Git symlink placeholder not excluded: %+v", st)
	}
}

func TestInspectionSnapshotRecordsLinksWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, outside, "private.txt", "never inspect\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	files, err := inspectionFiles(dir)
	if err != nil || len(files) != 2 || files["link"] != "link:"+outside {
		t.Fatalf("snapshot followed link: %+v %v", files, err)
	}
	w := &inspectionWorktree{dir: dir, files: files}
	for _, target := range []string{dir, "missing-target"} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := w.integrity(true); err == nil || !strings.Contains(err.Error(), "changed an existing file") {
			t.Fatalf("changed link: %v", err)
		}
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := w.integrity(true); err == nil || !strings.Contains(err.Error(), "changed an existing file") {
		t.Fatalf("removed link: %v", err)
	}
}

func TestAdoptLocationLinesUsePinnedBlobs(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, areaOK)
	write(t, f.dir, "src/billing/empty.txt", "")
	write(t, f.dir, "src/billing/last.txt", "no final newline")
	base := f.commitCode("line bounds base")
	write(t, f.dir, "src/billing/fee.txt", "one\ntwo\nthree\nfour\nfive\n")
	f.commitCode("longer current file")
	for _, loc := range []string{"src/billing/fee.txt:99999", "src/billing/fee.txt:1-4", "src/billing/fee.txt:4-5", "src/billing/fee.txt:999999999999999999999999999999", "src/billing/empty.txt:1"} {
		f.setAgent(strings.ReplaceAll(areaOK, "src/billing/fee.txt:1", loc))
		st, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "", base, nil)
		if err != nil || st.Status != StatusBlocked || len(st.Attempts) != 2 {
			t.Fatalf("%s: %+v %v", loc, st, err)
		}
		for _, attempt := range st.Attempts {
			if !strings.Contains(attempt.Feedback, "line count at the pinned base") {
				t.Fatalf("bounds feedback: %+v", attempt)
			}
		}
	}
	// A shorter current checkout cannot invalidate the older blob's third line.
	write(t, f.dir, "src/billing/fee.txt", "short\n")
	f.commitCode("shorter current file")
	f.setAgent(strings.ReplaceAll(areaOK, "src/billing/fee.txt:1", "src/billing/last.txt:1"))
	st, err := AdoptInspectArea(context.Background(), f.repo, "billing", []string{"src/billing/"}, "", base, nil)
	if err != nil || st.Status != "accepted" {
		t.Fatalf("valid pinned lines including no final newline: %+v %v", st, err)
	}
}

func TestInspectionSnapshotRecordsJunctionWithoutFollowing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows reparse points")
	}
	dir, outside := t.TempDir(), t.TempDir()
	write(t, outside, "private.txt", "never inspect\n")
	link := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/C", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot create junction: %v %s", err, out)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	files, err := inspectionFiles(dir)
	if err != nil || len(files) != 2 || files["junction"] != "link:"+target {
		t.Fatalf("snapshot followed junction: %+v %v", files, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	w := &inspectionWorktree{dir: dir, files: files}
	if err := w.integrity(true); err == nil || !strings.Contains(err.Error(), "changed an existing file") {
		t.Fatalf("removed junction: %v", err)
	}
}

func TestAdoptPromotionChecksMemoryHEAD(t *testing.T) {
	r, mem := newAreaProject(t)
	for id := 1; id <= 3; id++ {
		o := observation(t, r, id)
		if _, err := AdoptConfirm(r, "billing", fmt.Sprintf("AC-%d", id), DecisionKeep, o.Token()); err != nil {
			t.Fatal(err)
		}
	}
	if err := AdoptPromote(r, "billing"); err != nil {
		t.Fatal(err)
	}
	original := memRead(t, mem, specRel("billing"))
	write(t, mem, specRel("billing"), strings.Replace(original, "title: Billing", "title: Changed", 1))
	mustGit(t, mem, "add", "-A")
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "edit promoted spec")
	head := mustGit(t, mem, "rev-parse", "HEAD")
	if err := AdoptPromote(r, "billing"); err == nil || !strings.Contains(err.Error(), "overlap existing feature") {
		t.Fatalf("edited spec silently accepted: %v", err)
	}
	if mustGit(t, mem, "rev-parse", "HEAD") != head {
		t.Fatal("conflict changed memory")
	}
	mustGit(t, mem, "rm", "--", specRel("billing"))
	mustGit(t, mem, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "remove promoted spec")
	head = mustGit(t, mem, "rev-parse", "HEAD")
	// The normal importer reports the earlier batch marker as a conflict. A missing
	// spec must reach that check rather than return a false idempotent success.
	if err := AdoptPromote(r, "billing"); err == nil || !strings.Contains(err.Error(), "carries this batch's marker") {
		t.Fatalf("removed spec silently accepted: %v", err)
	}
	if mustGit(t, mem, "rev-parse", "HEAD") != head {
		t.Fatal("removal conflict changed memory")
	}
}

func TestAdoptExactManifests(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest outputManifest
		files    map[string]string
	}{
		{"inventory extra", inventoryManifest(), map[string]string{"system.md": "# System\n", "unexpected.md": "extra\n"}},
		{"inventory missing", inventoryManifest(), map[string]string{"config.suggested.yaml": "commands: {}\n"}},
		{"inventory limit", inventoryManifest(), map[string]string{"system.md": strings.Repeat("x", 65537)}},
		{"suggested documents", inventoryManifest(), map[string]string{"system.md": "# System\n", "config.suggested.yaml": "commands: {}\n---\ncommands: {}\n"}},
		{"area extra", areaManifest(), map[string]string{"observations.yaml": "title: A\n", "system.md": "extra\n"}},
		{"area aliases", areaManifest(), map[string]string{"observations.yaml": "title: &x A\ndescription: *x\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "output")
			root, err := openOutputDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			for p, b := range tc.files {
				write(t, dir, p, b)
			}
			if _, err := freezeOutput(root, tc.manifest); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestAdoptCleansInterruptedInspection(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, inventoryOK)
	a, err := loadAdoptState(f.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	w, err := newInspectionWorktree(a.repo, base, nil, true, a.inspectionName(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	write(t, w.dir, "interrupted.txt", "unfinished agent write\n")
	if err := os.Mkdir(w.dir+"-output", 0700); err != nil {
		t.Fatal(err)
	}
	a.state.Attempts = []AdoptAttempt{{N: 1, Result: "running", AdoptInspection: AdoptInspection{Base: base, Profile: "agent"}}}
	a.state.RunStatus = StatusRunning
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	st := f.inventory("")
	if st.Attempts[0].N != 2 {
		t.Fatalf("interrupted identity reused: %+v", st)
	}
	for _, p := range []string{w.dir, w.dir + "-output", a.repo.WorktreePath(a.inspectionName(2))} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("inspection not removed: %s: %v", p, err)
		}
	}
}

func TestAdoptInventoryRetryFinishesSnapshotWithoutAgent(t *testing.T) {
	endToEnd(t)
	f := newAdoptFixture(t, "exit 99")
	a, err := loadAdoptState(f.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	base := mustGit(t, f.dir, "rev-parse", "HEAD")
	info := AdoptInspection{Base: base, Date: "2026-11-15", Profile: "agent", Snapshot: observationDigest("frozen")}
	a.state.Current, a.state.Inspection = &info, &info
	a.state.Attempts = []AdoptAttempt{{N: 1, Result: "failed", Feedback: "import interrupted", AdoptInspection: info}}
	files := map[string][]byte{"wiki/system.md": []byte("# Frozen system\n\nInspected at " + base + ".\n")}
	a.state.Pending, err = newPendingImport(a.memDir, files, "inventory", "inventory "+base)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	st := f.inventory("")
	if len(st.Attempts) != 1 || st.Attempts[0].Result != "accepted" || st.Attempts[0].Feedback != "" {
		t.Fatalf("recovery ran a new agent: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".aeh", "runs")); !os.IsNotExist(err) {
		t.Fatalf("agent invoked during import recovery: %v", err)
	}
	if f.memFile("wiki/system.md") != string(files["wiki/system.md"]) {
		t.Fatal("recovery changed frozen bytes")
	}
}
