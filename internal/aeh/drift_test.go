package aeh

import (
	"reflect"
	"strings"
	"testing"
)

func TestGateDrift(t *testing.T) {
	memory := MemorySnapshot{Present: true, Specs: []Spec{
		{Feature: "fees", Paths: []string{"src/fees/**"}},
		{Feature: "billing", Paths: []string{"src/billing/**"}},
	}}
	tests := []struct {
		name, declared  string
		paths           []string
		passed, skipped bool
		output, warning string
	}{
		{name: "undeclared", paths: []string{"src/fees/calc.go"}, output: `touched feature "fees"`},
		{name: "wrong declaration", declared: "billing", paths: []string{"src/fees/calc.go"}, output: `touched feature "fees"`},
		{name: "two features", declared: "fees", paths: []string{"src/fees/calc.go", "src/billing/pay.go"}, output: "split into separate outcomes"},
		{name: "declared untouched", declared: "fees", paths: []string{"README.md"}, passed: true, warning: `declared feature "fees" is not touched`},
		{name: "unowned", paths: []string{"README.md"}, passed: true, warning: "files belong to no feature: README.md"},
		{name: "owned and unowned", declared: "fees", paths: []string{"README.md", "src/fees/calc.go"}, passed: true, warning: "files belong to no feature: README.md"},
		{name: "rename across features", declared: "fees", paths: []string{"src/fees/old.go", "src/billing/new.go"}, output: "split into separate outcomes"},
		{name: "deleted feature file", paths: []string{"src/fees/deleted.go"}, output: "fees: src/fees/deleted.go"},
		{name: "declared deletion", declared: "fees", paths: []string{"src/fees/deleted.go"}, passed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gateDrift(tt.paths, memory, tt.declared)
			if g.Name != "drift" || g.Passed != tt.passed || g.Skipped != tt.skipped || !strings.Contains(g.Output, tt.output) {
				t.Fatalf("unexpected gate result: %+v", g)
			}
			if tt.warning != "" && !strings.Contains(strings.Join(g.Warnings, "\n"), tt.warning) {
				t.Fatalf("missing warning %q: %+v", tt.warning, g)
			}
		})
	}
}

func TestGateDriftOverlappingPatternsAndOrdering(t *testing.T) {
	memory := MemorySnapshot{Present: true, Specs: []Spec{
		{Feature: "fees", Paths: []string{"src/fees/**"}},
		{Feature: "billing", Paths: []string{"src/**"}},
	}}
	paths := []string{"unowned/z", "src/fees/z.go", "src/fees/a.go", "unowned/a"}
	g := gateDrift(paths, memory, "fees")
	if g.Passed || !strings.Contains(g.Output, "split into separate outcomes") ||
		!strings.Contains(g.Output, "billing: src/fees/a.go, src/fees/z.go\nfees: src/fees/a.go, src/fees/z.go") {
		t.Fatalf("overlap must touch both features in sorted order: %+v", g)
	}
	memory.Specs[0], memory.Specs[1] = memory.Specs[1], memory.Specs[0]
	other := gateDrift([]string{"unowned/a", "src/fees/a.go", "unowned/z", "src/fees/z.go", "src/fees/z.go"}, memory, "fees")
	if !reflect.DeepEqual(g, other) {
		t.Fatalf("result depends on input ordering or duplicates: %+v != %+v", g, other)
	}
}

func TestGateDriftWithoutMemory(t *testing.T) {
	g := gateDrift([]string{"src/fees.go"}, MemorySnapshot{}, "")
	if !g.Passed || !g.Skipped || !strings.Contains(g.Output, "no product memory") || len(g.Warnings) != 0 {
		t.Fatalf("expected drift to skip: %+v", g)
	}
}
