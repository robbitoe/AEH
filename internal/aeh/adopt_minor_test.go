package aeh

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A credential spelled with YAML escapes in a field of the wrong type is caught on the decoded
// scalar, before the type error could quote a truncated, unrecognizable part of it.
func TestAdoptEscapedSecretInTypedFieldNeverLeaks(t *testing.T) {
	escaped := `"ghp_` + strings.Repeat(`\x41`, 36) + `"`
	body := strings.Repeat("A", 8)
	for name, check := range map[string]func() error{
		"suggested": func() error { return checkSuggested([]byte("commands:\n  timeout_minutes: " + escaped + "\n")) },
		"observations": func() error {
			var tree yaml.Node
			if err := yaml.Unmarshal([]byte("title: x\nobservations:\n  - id: "+escaped+"\n"), &tree); err != nil {
				return err
			}
			return checkYAMLScalars("observations.yaml", &tree, nil)
		},
	} {
		err := check()
		if err == nil || !strings.Contains(err.Error(), "possible") || strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), body) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The inventory's run evidence and an area named "system" never share a directory.
func TestAdoptRunDirectoriesAreDistinct(t *testing.T) {
	inventory := (&adoptSession{}).runName()
	area := (&adoptSession{feature: "system"}).runName()
	if inventory == area || inventory != "adopt" || area != "adopt-system" {
		t.Fatalf("run directories: inventory %q, area %q", inventory, area)
	}
}
