package main

import (
	"strings"
	"testing"

	"github.com/robbitoe/aeh/internal/aeh"
)

func TestMemoryApproveRelocationFlagValidation(t *testing.T) {
	for _, value := range []string{"", "src", "src=", "src/**=lib/", "src/=src/lib/", "src=lib,lib=next"} {
		code, err := memoryCommand(aeh.Repo{}, []string{"approve", "words", "--relocate", value})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "relocat") {
			t.Fatalf("--relocate %q: %d %v", value, code, err)
		}
	}
	if !strings.Contains(usage, "[--relocate <from>=<to>[,...]]") {
		t.Fatal("relocation flag missing from aeh help")
	}
}
