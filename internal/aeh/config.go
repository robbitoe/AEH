package aeh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Profile describes how to launch one agent.
// Command arguments may contain {{prompt}} and {{prompt_file}} placeholders.
// Provider is a free name (openai, anthropic, lmstudio...) used to keep reviewers independent
// from authors. Transport is how the agent is reached; only "cli" exists today, "api" is
// reserved for a later stage.
type Profile struct {
	Provider       string            `yaml:"provider"`
	Transport      string            `yaml:"transport"`
	Command        []string          `yaml:"command"`
	Env            map[string]string `yaml:"env"`
	Stdin          bool              `yaml:"stdin"`
	TimeoutMinutes int               `yaml:"timeout_minutes"`
	QuotaExitCodes []int             `yaml:"quota_exit_codes"`
}

// Config is the project's constitution: commands, limits, protected paths and agent profiles.
type Config struct {
	Project  string `yaml:"project"`
	Commands struct {
		Build          string `yaml:"build"`
		Test           string `yaml:"test"`
		TimeoutMinutes int    `yaml:"timeout_minutes"`
	} `yaml:"commands"`
	PassEnv   []string `yaml:"pass_env"`
	Protected []string `yaml:"protected"`
	Limits    struct {
		MaxDiffLines int `yaml:"max_diff_lines"`
	} `yaml:"limits"`
	Ladder    []string       `yaml:"ladder"`
	Reviewer  string         `yaml:"reviewer"` // legacy: one reviewer for every role
	Reviewers ReviewerPolicy `yaml:"reviewers"`
	Triage    struct {
		Profile string       `yaml:"profile"`
		Rules   []TriageRule `yaml:"rules"`
	} `yaml:"triage"`
	Notify struct {
		Webhook string `yaml:"webhook"`
	} `yaml:"notify"`
	Profiles map[string]Profile `yaml:"profiles"`
}

// Criterion is one given/when/then scenario of an outcome.
type Criterion struct {
	Given string `yaml:"given"`
	When  string `yaml:"when"`
	Then  string `yaml:"then"`
}

// Task is one unit of work inside an outcome.
type Task struct {
	ID           string   `yaml:"id"`
	Goal         string   `yaml:"goal"`
	Paths        []string `yaml:"paths"`
	Start        string   `yaml:"start"`
	MaxDiffLines int      `yaml:"max_diff_lines"`
}

// Outcome is the contract: what an actor achieves, how it is verified, and how it is split.
type Outcome struct {
	ID          string      `yaml:"id"`
	Title       string      `yaml:"title"`
	Level       int         `yaml:"level"`
	Actor       string      `yaml:"actor"`
	Description string      `yaml:"description"`
	Feature     string      `yaml:"feature,omitempty"`
	Change      string      `yaml:"change,omitempty"`
	Changelog   string      `yaml:"changelog,omitempty"`
	Route       string      `yaml:"route,omitempty"`     // the owner's route; a floor that rules may raise
	Reviewers   []string    `yaml:"reviewers,omitempty"` // roles to add, e.g. [dba]
	Criteria    []Criterion `yaml:"criteria"`
	Acceptance  []string    `yaml:"acceptance"`
	Tasks       []Task      `yaml:"tasks"`
}

// LoadConfig reads and validates aeh/config.yaml.
func (r Repo) LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(r.ConfigPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, fmt.Errorf("no %s found: run `aeh init` first", rel(r.Root, r.ConfigPath()))
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if err := checkConfigYAML(b); err != nil {
		return c, err
	}
	return c, c.validate()
}

// checkConfigYAML rejects anchors, aliases and merge keys, and a reviewers key whose value is
// not a mapping. The config decides who reviews; it must read the same to the owner and to aeh.
// yaml.v3 does not call UnmarshalYAML for null, so "reviewers:", "reviewers: ~" and
// "reviewers: null" would otherwise leave the legacy reviewer in charge of every role.
func checkConfigYAML(b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			return fmt.Errorf("config: line %d: YAML anchors and aliases are not supported", n.Line)
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "<<" && n.Content[i].Tag == "!!merge" {
					return fmt.Errorf("config: line %d: YAML merge keys are not supported", n.Content[i].Line)
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
	if err := walk(&doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "reviewers" && m.Content[i+1].Kind != yaml.MappingNode {
			return errors.New("config: reviewers must be a mapping of independence and roles; remove the key to use the legacy reviewer")
		}
	}
	return nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Commands.Test) == "" {
		return errors.New("config: commands.test is required")
	}
	if c.Commands.TimeoutMinutes <= 0 {
		c.Commands.TimeoutMinutes = 20
	}
	if c.Limits.MaxDiffLines <= 0 {
		c.Limits.MaxDiffLines = 400
	}
	if len(c.Ladder) == 0 {
		return errors.New("config: ladder needs at least one profile")
	}
	onLadder := map[string]bool{}
	for _, name := range c.Ladder {
		if onLadder[name] {
			return fmt.Errorf("config: profile %q appears twice in the ladder", name)
		}
		onLadder[name] = true
		if err := c.checkProfile(name); err != nil {
			return err
		}
	}
	names := []string{}
	if c.Reviewer != "" {
		names = append(names, c.Reviewer)
	}
	for _, role := range roleOrder {
		names = append(names, c.Reviewers.list(role)...)
	}
	if c.Triage.Profile != "" {
		names = append(names, c.Triage.Profile)
	}
	for _, name := range names {
		if err := c.checkProfile(name); err != nil {
			return err
		}
	}
	switch c.Reviewers.Independence {
	case "":
		c.Reviewers.Independence = IndependenceProvider
	case IndependenceProvider, IndependenceProfile:
	default:
		return fmt.Errorf("config: reviewers.independence %q must be provider or profile", c.Reviewers.Independence)
	}
	seen := map[string]bool{}
	for i, rule := range c.Triage.Rules {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("config: triage rule %d (%s): %w", i+1, rule.Name, err)
		}
		if seen[rule.Name] {
			return fmt.Errorf("config: triage rule %q is defined twice", rule.Name)
		}
		seen[rule.Name] = true
	}
	for name := range c.Profiles {
		p := c.Profiles[name]
		switch p.Transport {
		case "":
			p.Transport = TransportCLI
		case TransportCLI:
		case TransportAPI:
			return fmt.Errorf("config: profile %q: transport api is not supported yet", name)
		default:
			return fmt.Errorf("config: profile %q: transport %q must be cli", name, p.Transport)
		}
		if !validProvider(p.Provider) {
			return fmt.Errorf("config: profile %q: provider must be one word of printable characters without spaces", name)
		}
		c.Profiles[name] = p
	}
	return nil
}

// validProvider accepts an empty provider (not declared) or one word of printable,
// non-space characters.
func validProvider(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Transports a profile can use. Validation lets only cli through, and runAgent launches it;
// an api transport would be one more branch there, with its own proposal.
const (
	TransportCLI = "cli"
	TransportAPI = "api" // reserved
)

func (c Config) checkProfile(name string) error {
	p, ok := c.Profiles[name]
	if !ok {
		return fmt.Errorf("config: profile %q is used but not defined", name)
	}
	if len(p.Command) == 0 || strings.TrimSpace(p.Command[0]) == "" {
		return fmt.Errorf("config: profile %q has no command", name)
	}
	for _, code := range p.QuotaExitCodes {
		if code <= 0 || code > 255 {
			return fmt.Errorf("config: profile %q has invalid quota exit code %d", name, code)
		}
	}
	return nil
}

// escalate returns the profile one step up the ladder, or the same profile at the top
// or when it is not on the ladder.
func (c Config) escalate(name string) string {
	for i, n := range c.Ladder {
		if n == name && i+1 < len(c.Ladder) {
			return c.Ladder[i+1]
		}
	}
	return name
}

// LoadOutcome reads and validates aeh/outcomes/<id>.yaml.
func (r Repo) LoadOutcome(id string) (Outcome, error) {
	var o Outcome
	if !validID(id) {
		return o, fmt.Errorf("invalid outcome ID %q", id)
	}
	b, err := os.ReadFile(r.OutcomePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return o, fmt.Errorf("outcome %s not found at %s", id, rel(r.Root, r.OutcomePath(id)))
		}
		return o, err
	}
	if err := yaml.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("parse outcome %s: %w", id, err)
	}
	if o.ID == "" {
		o.ID = id
	}
	if o.ID != id {
		return o, fmt.Errorf("outcome file %s declares id %q", id, o.ID)
	}
	return o, o.normalize()
}

func (o *Outcome) normalize() error {
	if err := (MemoryLink{Feature: o.Feature, Change: o.Change, Note: o.Changelog}).Validate(); err != nil {
		return fmt.Errorf("outcome %s: memory link: %w", o.ID, err)
	}
	if strings.TrimSpace(o.Title) == "" {
		return fmt.Errorf("outcome %s: title is required", o.ID)
	}
	if o.Level == 0 {
		o.Level = 1
		if len(o.Acceptance) > 0 {
			o.Level = 2
		}
	}
	if o.Level >= 2 && len(o.Acceptance) == 0 {
		return fmt.Errorf("outcome %s: level 2 needs acceptance test paths", o.ID)
	}
	if o.Route != "" && !validRoute(o.Route) {
		return fmt.Errorf("outcome %s: route %q must be trivial, change or new", o.ID, o.Route)
	}
	for _, role := range o.Reviewers {
		if !validRole(role) {
			return fmt.Errorf("outcome %s: reviewer role %q must be qa, security or dba", o.ID, role)
		}
	}
	if len(o.Tasks) == 0 {
		goal := o.Title
		if o.Description != "" {
			goal += "\n\n" + o.Description
		}
		o.Tasks = []Task{{ID: "T1", Goal: goal}}
	}
	seen := map[string]bool{}
	for i := range o.Tasks {
		t := &o.Tasks[i]
		if t.ID == "" {
			t.ID = fmt.Sprintf("T%d", i+1)
		}
		if !validID(t.ID) {
			return fmt.Errorf("outcome %s: invalid task ID %q", o.ID, t.ID)
		}
		if seen[t.ID] {
			return fmt.Errorf("outcome %s: duplicate task id %s", o.ID, t.ID)
		}
		seen[t.ID] = true
		if strings.TrimSpace(t.Goal) == "" {
			return fmt.Errorf("outcome %s: task %s has no goal", o.ID, t.ID)
		}
	}
	return nil
}

func validID(id string) bool {
	if id == "" || id[0] == '-' || id[0] == '_' {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}
