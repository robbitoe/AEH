package aeh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile describes how to launch one agent CLI.
// Command arguments may contain {{prompt}} and {{prompt_file}} placeholders.
type Profile struct {
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
	Ladder   []string `yaml:"ladder"`
	Reviewer string   `yaml:"reviewer"`
	Notify   struct {
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
	return c, c.validate()
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
	for _, name := range c.Ladder {
		if err := c.checkProfile(name); err != nil {
			return err
		}
	}
	if c.Reviewer != "" {
		if err := c.checkProfile(c.Reviewer); err != nil {
			return err
		}
	}
	return nil
}

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
