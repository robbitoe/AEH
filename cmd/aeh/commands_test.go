package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// usageCommands are the command names `aeh help` lists, such as "aeh discover review":
// the words of each line before its first argument, flag or choice.
func usageCommands() []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(usage, "\n") {
		if !strings.HasPrefix(line, "  aeh ") {
			continue
		}
		var name []string
		for _, w := range strings.Fields(line) {
			if strings.ContainsAny(w[:1], "<[\"-") || strings.Contains(w, "|") {
				break
			}
			name = append(name, w)
			if len(name) > 1 && len(strings.Fields(line)) > len(name) && strings.HasPrefix(strings.TrimPrefix(line, "  "+strings.Join(name, " ")), "   ") {
				break // the description starts after a run of spaces
			}
		}
		seen[strings.Join(name, " ")] = true
	}
	return sortedSet(seen)
}

func sortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every command of `aeh help` has its section in docs/commands.md, and every section names a
// command that exists: the reference never misses one.
func TestCommandReferenceIsComplete(t *testing.T) {
	b, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^### `(aeh[^`]*)`$").FindAllStringSubmatch(string(b), -1) {
		documented[m[1]] = true
	}
	listed := map[string]bool{}
	for _, c := range usageCommands() {
		listed[c] = true
		if !documented[c] {
			t.Errorf("%q is in aeh help but has no section in docs/commands.md", c)
		}
	}
	for c := range documented {
		if !listed[c] {
			t.Errorf("docs/commands.md documents %q, which aeh help does not list", c)
		}
	}
	if len(listed) < 30 {
		t.Fatalf("parsed only %d commands from the usage text: %v", len(listed), usageCommands())
	}
}

// Every command the CLI dispatches is listed in `aeh help`, and every listed command is
// dispatched: a command added for a special case cannot stay undocumented.
func TestUsageListsEveryDispatchedCommand(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{"dispatch": "aeh", "memoryCommand": "aeh memory", "adoptCommand": "aeh adopt", "discoverCommand": "aeh discover"}
	funcRe := regexp.MustCompile(`^func (\w+)\(`)
	caseRe := regexp.MustCompile(`"([a-z-]+)"`)
	dispatched := map[string]bool{"aeh help": true, "aeh version": true}
	prefix := ""
	for _, line := range strings.Split(string(src), "\n") {
		if m := funcRe.FindStringSubmatch(line); m != nil {
			prefix = groups[m[1]]
			continue
		}
		trimmed := strings.TrimSpace(line)
		if prefix == "" || !strings.HasPrefix(trimmed, "case \"") {
			continue
		}
		for _, m := range caseRe.FindAllStringSubmatch(trimmed, -1) {
			dispatched[prefix+" "+m[1]] = true
		}
	}
	listed := usageCommands()
	for name := range dispatched {
		found := false
		for _, c := range listed {
			if c == name || strings.HasPrefix(c, name+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("the CLI dispatches %q, but aeh help does not list it", name)
		}
	}
	for _, c := range listed {
		words := strings.Fields(c)
		key := strings.Join(words[:min(2, len(words))], " ")
		if groups := words[1]; (groups == "memory" || groups == "adopt" || groups == "discover") && len(words) > 2 {
			key = strings.Join(words[:3], " ")
		}
		if !dispatched[key] {
			t.Errorf("aeh help lists %q, which the CLI does not dispatch (%q)", c, key)
		}
	}
}
