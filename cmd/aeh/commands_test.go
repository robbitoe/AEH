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
	dispatched := dispatchedCommands(string(src))
	listed := map[string]bool{}
	for _, c := range usageCommands() {
		listed[c] = true
		if !dispatched[c] {
			t.Errorf("aeh help lists %q, which the CLI does not dispatch", c)
		}
	}
	for c := range dispatched {
		if !listed[c] {
			t.Errorf("the CLI dispatches %q, but aeh help does not list it", c)
		}
	}
}

// dispatchedCommands reads the command names main.go accepts: the cases of dispatch and of the
// memory, adopt and discover commands; a fixed sub-word a case checks inline
// (`args[1] != "approve"`) completes that case's name; and the words dispatch checks before
// its switch (`args[0] == "help"`).
func dispatchedCommands(src string) map[string]bool {
	groups := map[string]string{"dispatch": "aeh", "memoryCommand": "aeh memory", "adoptCommand": "aeh adopt", "discoverCommand": "aeh discover"}
	funcRe := regexp.MustCompile(`^func (\w+)\(`)
	wordRe := regexp.MustCompile(`"([a-z-]+)"`)
	subRe := regexp.MustCompile(`args\[1\] != "([a-z-]+)"`)
	topRe := regexp.MustCompile(`args\[0\] == "([a-z]+)"`)
	dispatched, completed := map[string]bool{}, map[string]bool{}
	prefix := ""
	var cases []string
	for _, line := range strings.Split(src, "\n") {
		if m := funcRe.FindStringSubmatch(line); m != nil {
			prefix, cases = groups[m[1]], nil
			continue
		}
		if prefix == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "case \"") {
			cases = nil
			for _, m := range wordRe.FindAllStringSubmatch(trimmed, -1) {
				cases = append(cases, prefix+" "+m[1])
				dispatched[prefix+" "+m[1]] = true
			}
			continue
		}
		if m := subRe.FindStringSubmatch(trimmed); m != nil && len(cases) > 0 {
			for _, c := range cases {
				completed[c] = true
				dispatched[c+" "+m[1]] = true
			}
		}
		if prefix == "aeh" && len(cases) == 0 {
			for _, m := range topRe.FindAllStringSubmatch(trimmed, -1) {
				dispatched["aeh "+m[1]] = true
			}
		}
	}
	for c := range completed {
		delete(dispatched, c)
	}
	for _, g := range groups {
		delete(dispatched, g) // "aeh memory" only hands over to memoryCommand
	}
	return dispatched
}

// The dispatch parser itself: inline sub-words and pre-switch words are exact, not prefixes.
func TestDispatchedCommandsParser(t *testing.T) {
	src := "func dispatch(ctx context.Context, args []string) (int, error) {\n" +
		"\tif args[0] == \"help\" || args[0] == \"version\" {\n\t}\n" +
		"\tswitch args[0] {\n\tcase \"notify\":\n\t\tif len(args) != 2 || args[1] != \"test\" {\n\t\t}\n\tcase \"run\":\n\t}\n}\n" +
		"func adoptCommand(ctx context.Context, repo aeh.Repo, args []string) (int, error) {\n" +
		"\tcase \"accept\":\n\t\tif len(args) != 2 || args[1] != \"system\" {\n\t\t}\n\tcase \"area\", \"seal\":\n}\n"
	got := sortedSet(dispatchedCommands(src))
	want := "aeh adopt accept system,aeh adopt area,aeh adopt seal,aeh help,aeh notify test,aeh run,aeh version"
	if strings.Join(got, ",") != want {
		t.Fatalf("parsed %v", got)
	}
}
