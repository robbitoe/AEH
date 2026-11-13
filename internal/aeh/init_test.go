package aeh

import (
	"strings"
	"testing"
)

// The template must load, and no profile may pass the prompt as an argument:
// on Windows the CLIs are .cmd shims and cmd.exe cuts a multi-line argument.
func TestInitTemplateLoadsAndPipesPrompts(t *testing.T) {
	r := Repo{Root: t.TempDir()}
	if _, err := Init(r); err != nil {
		t.Fatal(err)
	}
	cfg, err := r.LoadConfig()
	if err != nil {
		t.Fatalf("template config does not load: %v", err)
	}
	for _, name := range append(append([]string{}, cfg.Ladder...), cfg.Reviewer) {
		if err := cfg.checkProfile(name); err != nil {
			t.Fatal(err)
		}
	}
	for name, p := range cfg.Profiles {
		if !p.Stdin {
			t.Errorf("profile %s does not read the prompt from stdin", name)
		}
		for _, arg := range p.Command {
			if strings.Contains(arg, "{{prompt}}") {
				t.Errorf("profile %s passes the prompt as an argument", name)
			}
		}
	}
}
