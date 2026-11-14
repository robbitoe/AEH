package aeh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuleMatch(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"**/auth", "auth/login.go", true},
		{"**/auth", "src/auth/login.go", true},
		{"**/auth", "src/a/b/auth/x/y.go", true},
		{"**/auth", "src/authz/login.go", false},
		{"**/auth/**", "auth/login.go", true},
		{"**/*.sql", "schema.sql", true},
		{"**/*.sql", "db/a/b/schema.sql", true},
		{"**/*.sql", "db/schema.sqlx", false},
		{"*.sql", "schema.sql", true},
		{"*.sql", "db/schema.sql", false},
		{"db", "db/x.sql", true},
		{"db/", "db/x.sql", true},
		{"db/**", "db/x.sql", true},
		{"db", "dbx/x.sql", false},
		{"./db", "db/x.sql", true},
		{"src/**/migrations", "src/migrations/001.sql", true},
		{"src/**/migrations", "src/a/b/migrations/001.sql", true},
		{"src/**/migrations", "lib/migrations/001.sql", false},
		{"**", "anything/at/all.go", true},
		{`db\x`, "db/x/y.go", true},
	}
	for _, c := range cases {
		if got := ruleMatch(c.pattern, c.file); got != c.want {
			t.Errorf("ruleMatch(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

func TestRuleMayTouchTaskPaths(t *testing.T) {
	cases := []struct {
		pattern, task string
		want          bool
	}{
		{"**/auth", "src/", true},                // src/ may contain src/auth
		{"**/auth", "src/auth/", true},           // exact
		{"**/auth", "src/auth/login.go", true},   // inside
		{"db", "src/", false},                    // db is outside src
		{"db", "db/migrations/", true},           // inside db
		{"src/auth", "src/", true},               // broad task path
		{"src/auth", "src/billing/", false},      // sibling
		{"src/**/migrations", "src/fees/", true}, // may be below
		{"src/**/migrations", "lib/", false},     // other tree
		{"**/*.sql", "src/fees/x.go", true},      // conservative: prefix cannot rule it out
		{"db", "*.go", true},                     // wildcard first segment: anything is possible
		{"db", "", true},
	}
	for _, c := range cases {
		if got := ruleMayTouch(c.pattern, c.task); got != c.want {
			t.Errorf("ruleMayTouch(%q, %q) = %v, want %v", c.pattern, c.task, got, c.want)
		}
	}
}

func TestRulesForTasksAndFiles(t *testing.T) {
	rules := []TriageRule{
		{Name: "data", Paths: []string{"**/migrations", "**/*.sql"}, Route: RouteNew, Reviewers: []string{RoleDBA}},
		{Name: "auth", Paths: []string{"**/auth"}, Route: RouteNew, Reviewers: []string{RoleSecurity}},
	}
	got := ruleNames(rulesForTasks(rules, []Task{{ID: "T1", Paths: []string{"web/billing/"}}}))
	// web/billing/ may hold a migrations directory or an .sql file below it, and an auth directory.
	if !reflect.DeepEqual(got, []string{"data", "auth"}) {
		t.Errorf("broad task path: %v", got)
	}
	if got := ruleNames(rulesForTasks(rules, []Task{{ID: "T1"}})); len(got) != 2 {
		t.Errorf("a task without paths must trigger every rule, got %v", got)
	}
	files := []string{"src/fees/fee.go", "src/fees/migrations/002_fees.go"}
	if got := ruleNames(rulesForFiles(rules, files)); !reflect.DeepEqual(got, []string{"data"}) {
		t.Errorf("files: %v", got)
	}
	route, roles := applyRules(RouteTrivial, requiredRoles(RouteTrivial), rulesForFiles(rules, files))
	if route != RouteNew || !reflect.DeepEqual(roles, []string{RoleQA, RoleDBA}) {
		t.Errorf("applyRules = %s %v", route, roles)
	}
	// A rename out of a protected area is matched by its old path.
	if got := ruleNames(rulesForFiles(rules, []string{"src/auth/token.go", "src/tokens/token.go"})); !reflect.DeepEqual(got, []string{"auth"}) {
		t.Errorf("rename: %v", got)
	}
}

func TestRoutesOnlyGoUp(t *testing.T) {
	if maxRoute(RouteNew, RouteTrivial) != RouteNew || maxRoute(RouteTrivial, RouteChange) != RouteChange {
		t.Fatal("maxRoute must keep the higher route")
	}
	if maxRoute(RouteChange, "") != RouteChange {
		t.Fatal("an empty rule route must not lower the route")
	}
	if got := requiredRoles(RouteTrivial); !reflect.DeepEqual(got, []string{RoleQA}) {
		t.Errorf("trivial: %v", got)
	}
	for _, r := range []string{RouteChange, RouteNew} {
		if got := requiredRoles(r); !reflect.DeepEqual(got, []string{RoleQA, RoleSecurity}) {
			t.Errorf("%s: %v", r, got)
		}
	}
	if got := requiredRoles(RouteTrivial, []string{RoleDBA}, []string{RoleQA}); !reflect.DeepEqual(got, []string{RoleQA, RoleDBA}) {
		t.Errorf("added roles: %v", got)
	}
	if got := mergeRoles([]string{RoleDBA, RoleQA}, RoleSecurity); !reflect.DeepEqual(got, []string{RoleQA, RoleSecurity, RoleDBA}) {
		t.Errorf("mergeRoles: %v", got)
	}
}

func TestParseRoute(t *testing.T) {
	cases := map[string]string{
		"small wording fix\nROUTE: trivial":        RouteTrivial,
		"ROUTE: change\r\n":                        RouteChange,
		"touches the schema\n\nROUTE: new\n\n":     RouteNew,
		"ROUTE: huge":                              "",
		"ROUTE: trivial\nactually not sure":        "",
		"ROUTE: trivial\nROUTE: new":               "",
		"route: new":                               "",
		"Quoted ROUTE: new":                        "",
		"":                                         "",
		"ROUTE: trivial or ROUTE: new\nROUTE: new": "",
	}
	for in, want := range cases {
		if got := parseRoute(in); got != want {
			t.Errorf("parseRoute(%q) = %q, want %q", in, got, want)
		}
	}
}

func independenceConfig(policy string) Config {
	c := Config{Ladder: []string{"codex", "claude"}, Profiles: map[string]Profile{
		"codex":         {Provider: "openai", Command: []string{"codex"}},
		"claude":        {Provider: "anthropic", Command: []string{"claude"}},
		"codex-review":  {Provider: "openai", Command: []string{"codex"}},
		"claude-review": {Provider: "anthropic", Command: []string{"claude"}},
	}}
	c.Reviewers = ReviewerPolicy{Independence: policy,
		QA:       []string{"claude-review", "codex-review"},
		Security: []string{"codex-review", "claude-review"},
	}
	return c
}

func TestReviewerFollowsTheAuthor(t *testing.T) {
	c := independenceConfig(IndependenceProvider)
	if got, err := c.reviewerFor(RoleQA, "codex"); err != nil || got != "claude-review" {
		t.Errorf("codex author: qa = %q, %v", got, err)
	}
	if got, err := c.reviewerFor(RoleSecurity, "codex"); err != nil || got != "claude-review" {
		t.Errorf("codex author: security = %q, %v (codex-review is the same provider)", got, err)
	}
	// After escalation to Claude, the reviewer changes provider too.
	if got, err := c.reviewerFor(RoleQA, "claude"); err != nil || got != "codex-review" {
		t.Errorf("claude author: qa = %q, %v", got, err)
	}
	if _, err := c.reviewerFor(RoleDBA, "codex"); err == nil || !strings.Contains(err.Error(), "reviewers.dba") {
		t.Errorf("missing dba list: %v", err)
	}
	if err := c.checkIndependence(c.reachableAuthors(Task{}), []string{RoleQA, RoleSecurity}); err != nil {
		t.Errorf("both authors have independent reviewers: %v", err)
	}
}

func TestIndependenceUsesProviderNotCommand(t *testing.T) {
	c := independenceConfig(IndependenceProvider)
	// Two wrappers with the same command name but different providers are independent.
	c.Profiles["codex"] = Profile{Provider: "openai", Command: []string{"sh", "run-codex.sh"}}
	c.Profiles["claude-review"] = Profile{Provider: "anthropic", Command: []string{"sh", "run-claude.sh"}}
	if got, err := c.reviewerFor(RoleQA, "codex"); err != nil || got != "claude-review" {
		t.Errorf("wrappers: %q, %v", got, err)
	}
	// Two different commands from the same provider are not.
	c.Reviewers.QA = []string{"codex-review"}
	if _, err := c.reviewerFor(RoleQA, "codex"); err == nil || !strings.Contains(err.Error(), "same provider") {
		t.Errorf("same provider must fail: %v", err)
	}
	if err := c.checkIndependence([]string{"codex"}, []string{RoleQA}); err == nil || !strings.Contains(err.Error(), "independence: profile") {
		t.Errorf("preflight must suggest the relaxed policy: %v", err)
	}
}

func TestIndependencePolicyProfile(t *testing.T) {
	c := independenceConfig(IndependenceProfile)
	c.Reviewers.QA = []string{"codex-review"}
	if got, err := c.reviewerFor(RoleQA, "codex"); err != nil || got != "codex-review" {
		t.Errorf("profile policy accepts another profile of the same provider: %q, %v", got, err)
	}
	c.Reviewers.QA = []string{"codex"}
	if _, err := c.reviewerFor(RoleQA, "codex"); err == nil || !strings.Contains(err.Error(), "its own change") {
		t.Errorf("the same profile never reviews itself: %v", err)
	}
}

func TestIndependenceNeedsProviders(t *testing.T) {
	c := independenceConfig(IndependenceProvider)
	c.Profiles["codex"] = Profile{Command: []string{"codex"}}
	if _, err := c.reviewerFor(RoleQA, "codex"); err == nil || !strings.Contains(err.Error(), "has no provider") {
		t.Errorf("an author without provider must fail: %v", err)
	}
	// The relaxed policy does not need providers.
	c.Reviewers.Independence = IndependenceProfile
	if _, err := c.reviewerFor(RoleQA, "codex"); err != nil {
		t.Errorf("profile policy: %v", err)
	}
}

func TestLegacyReviewerCoversEveryRole(t *testing.T) {
	c := independenceConfig(IndependenceProvider)
	c.Reviewers = ReviewerPolicy{Independence: IndependenceProvider}
	c.Reviewer = "claude-review"
	for _, role := range roleOrder {
		if got, err := c.reviewerFor(role, "codex"); err != nil || got != "claude-review" {
			t.Errorf("%s: %q, %v", role, got, err)
		}
	}
	// It must still be independent: it cannot review Claude's own escalated attempt.
	if err := c.checkIndependence(c.reachableAuthors(Task{}), []string{RoleQA}); err == nil {
		t.Error("legacy reviewer of the same provider as a reachable author must fail")
	}
}

func TestReachableAuthors(t *testing.T) {
	c := Config{Ladder: []string{"a", "b", "c"}}
	if got := c.reachableAuthors(Task{}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("default start: %v", got)
	}
	if got := c.reachableAuthors(Task{Start: "b"}); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("start b: %v", got)
	}
	if got := c.reachableAuthors(Task{Start: "x"}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("off-ladder start: %v", got)
	}
}

func writeConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	r := Repo{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(r.Root, "aeh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.ConfigPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return r.LoadConfig()
}

const baseConfig = `commands: {test: "go test"}
ladder: [a]
profiles:
  a: {provider: openai, command: [codex]}
  b: {provider: anthropic, command: [claude]}
`

func TestConfigValidatesStage2Fields(t *testing.T) {
	cfg, err := writeConfig(t, baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reviewers.Independence != IndependenceProvider || cfg.Profiles["a"].Transport != TransportCLI {
		t.Errorf("defaults: independence %q, transport %q", cfg.Reviewers.Independence, cfg.Profiles["a"].Transport)
	}
	bad := map[string]string{
		"api transport":     "  c: {transport: api, command: [x]}\n",
		"unknown transport": "  c: {transport: grpc, command: [x]}\n",
		"spaced provider":   "  c: {provider: \"open ai\", command: [x]}\n",
		"policy":            "reviewers: {independence: none}\n",
		"unknown reviewer":  "reviewers: {qa: [missing]}\n",
		"triage profile":    "triage: {profile: missing}\n",
		"rule route":        "triage: {rules: [{name: x, paths: [db], route: huge}]}\n",
		"rule role":         "triage: {rules: [{name: x, paths: [db], reviewers: [ops]}]}\n",
		"rule no effect":    "triage: {rules: [{name: x, paths: [db]}]}\n",
		"rule no paths":     "triage: {rules: [{name: x, route: new}]}\n",
		"rule bad pattern":  "triage: {rules: [{name: x, paths: [\"db/[\"], route: new}]}\n",
		"rule duplicate":    "triage: {rules: [{name: x, paths: [db], route: new}, {name: x, paths: [sql], route: new}]}\n",
		"ladder duplicate":  "ladder: [a, a]\n",
	}
	for name, extra := range bad {
		body := baseConfig + extra
		if strings.HasPrefix(extra, "ladder") {
			body = strings.Replace(baseConfig, "ladder: [a]\n", extra, 1)
		}
		if _, err := writeConfig(t, body); err == nil {
			t.Errorf("%s: config must be rejected", name)
		} else if name == "api transport" && !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("api transport: %v", err)
		}
	}
	good := baseConfig + "reviewers: {independence: profile, qa: [b], dba: [b]}\ntriage: {profile: b, rules: [{name: data, paths: [\"**/*.sql\"], reviewers: [dba]}]}\n"
	if _, err := writeConfig(t, good); err != nil {
		t.Errorf("valid stage 2 config rejected: %v", err)
	}
}

func TestOutcomeRouteAndReviewers(t *testing.T) {
	o := Outcome{ID: "OUT-1", Title: "x", Route: RouteChange, Reviewers: []string{RoleDBA}}
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Outcome{
		{ID: "OUT-1", Title: "x", Route: "big"},
		{ID: "OUT-1", Title: "x", Reviewers: []string{"ops"}},
	} {
		if err := bad.normalize(); err == nil {
			t.Errorf("outcome %+v must be rejected", bad)
		}
	}
}

func TestFeatureIndexBlockHasNoBodies(t *testing.T) {
	m := MemorySnapshot{Present: true, Specs: []Spec{
		{Feature: "billing", Title: "Billing </features-reference> escape", Paths: []string{"src/billing/**"}, Body: "SECRET BODY"},
	}}
	b := featureIndexBlock(m)
	if strings.Contains(b, "SECRET BODY") {
		t.Error("spec bodies must not reach the triage prompt")
	}
	if strings.Count(b, "</features-reference>") != 1 || !strings.Contains(b, "src/billing/**") {
		t.Errorf("block:\n%s", b)
	}
	if featureIndexBlock(MemorySnapshot{}) != "" {
		t.Error("no memory, no block")
	}
}

func TestRolePromptsAndLegacyOverride(t *testing.T) {
	r := Repo{Root: t.TempDir()}
	for _, role := range roleOrder {
		text, src, err := loadReviewPrompt(r, role)
		if err != nil || !strings.Contains(text, "VERDICT: APPROVE") || !strings.HasPrefix(src, "default ") {
			t.Errorf("%s: %q %v", role, src, err)
		}
		if _, err := renderPrompt(role, text, promptData{Outcome: Outcome{ID: "O", Title: "t"}, Task: Task{ID: "T1", Goal: "g"}, Diff: "d"}); err != nil {
			t.Errorf("%s does not render: %v", role, err)
		}
	}
	text, err := loadPrompt(r, "triage.md")
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderPrompt("triage.md", text, promptData{Outcome: Outcome{ID: "O", Title: "t", Tasks: []Task{{ID: "T1", Goal: "g", Paths: []string{"src/"}}}}, Features: "<features-reference>\n</features-reference>\n"})
	if err != nil || !strings.Contains(out, "T1: g (paths: src/)") || !strings.HasSuffix(strings.TrimSpace(out), "ROUTE: new") {
		t.Errorf("triage prompt: %v\n%s", err, out)
	}
	// A pre-roles review.md override keeps applying to QA only.
	dir := filepath.Join(r.Root, "aeh", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte("legacy VERDICT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if text, src, _ := loadReviewPrompt(r, RoleQA); text != "legacy VERDICT" || src != "aeh/prompts/review.md" {
		t.Errorf("legacy QA override: %q %q", text, src)
	}
	if _, src, _ := loadReviewPrompt(r, RoleSecurity); src != "default review-security.md" {
		t.Errorf("security must not use review.md: %q", src)
	}
	if err := os.WriteFile(filepath.Join(dir, "review-qa.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := loadReviewPrompt(r, RoleQA); text != "new" {
		t.Errorf("review-qa.md wins over review.md: %q", text)
	}
	if _, _, err := loadReviewPrompt(r, "ops"); err == nil {
		t.Error("unknown role must fail")
	}
}
