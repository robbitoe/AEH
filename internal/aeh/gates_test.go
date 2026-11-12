package aeh

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchAny(t *testing.T) {
	cases := []struct {
		file     string
		patterns []string
		want     bool
	}{
		{"src/Fees/Service.cs", []string{"src/Fees/"}, true},
		{"src/Fees/Service.cs", []string{"src/Fees"}, true},
		{"src/Fees/deep/x.cs", []string{"src/Fees/**"}, true},
		{"src/FeesOther/x.cs", []string{"src/Fees"}, false},
		{"src/Pay/x.cs", []string{"src/Fees/"}, false},
		{"README.md", []string{"*.md"}, true},
		{"docs/a.md", []string{"*.md"}, false},
		{"anything", []string{"**"}, true},
		{"src\\win.cs", []string{"src\\"}, false}, // git always reports forward slashes
	}
	for _, c := range cases {
		if got := matchAny(c.file, c.patterns); got != c.want {
			t.Errorf("matchAny(%q, %v) = %v, want %v", c.file, c.patterns, got, c.want)
		}
	}
}

func TestGateScope(t *testing.T) {
	protected := []string{"aeh/", "tests/acceptance/"}
	ok := gateScope([]fileChange{{"src/a.go", 10}}, protected, []string{"src/"}, 100)
	if !ok.Passed {
		t.Fatalf("expected pass, got %s", ok.Output)
	}
	for name, changes := range map[string][]fileChange{
		"empty":     nil,
		"protected": {{"tests/acceptance/check.sh", 1}},
		"outside":   {{"lib/b.go", 1}},
		"too large": {{"src/a.go", 101}},
	} {
		if g := gateScope(changes, protected, []string{"src/"}, 100); g.Passed {
			t.Errorf("%s: expected failure", name)
		}
	}
}

func TestHashPathsDetectsChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "tests/a.txt", "one")
	write(t, dir, "tests/sub/b.txt", "two")
	h1, files, err := hashPaths(dir, []string{"tests/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "tests/a.txt" {
		t.Fatalf("unexpected files %+v", files)
	}
	write(t, dir, "tests/sub/b.txt", "changed")
	h2, _, _ := hashPaths(dir, []string{"tests/"})
	if h1 == h2 {
		t.Fatal("hash did not change after editing a sealed file")
	}
	if _, _, err := hashPaths(dir, []string{"missing/"}); err == nil {
		t.Fatal("expected error for a missing path")
	}
}

func TestParseVerdict(t *testing.T) {
	cases := map[string]string{
		"looks fine\nVERDICT: APPROVE":                         "APPROVE",
		"VERDICT: APPROVE or VERDICT: REJECT\nVERDICT: REJECT": "REJECT",
		`{"result":"ok\nVERDICT: APPROVE"}`:                    "APPROVE",
		"no verdict here":                                      "",
	}
	for in, want := range cases {
		if got := parseVerdict(in); got != want {
			t.Errorf("parseVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscalate(t *testing.T) {
	c := Config{Ladder: []string{"local", "medio", "fuerte"}}
	if got := c.escalate("local"); got != "medio" {
		t.Errorf("got %s", got)
	}
	if got := c.escalate("fuerte"); got != "fuerte" {
		t.Errorf("top of ladder should stay, got %s", got)
	}
	if got := c.escalate("other"); got != "other" {
		t.Errorf("profile off the ladder should stay, got %s", got)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
