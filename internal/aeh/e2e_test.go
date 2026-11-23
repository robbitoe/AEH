package aeh

import (
	"sync"
	"testing"
)

// endToEndTests remembers which tests already asked to run in parallel: a test that builds
// two fixtures must call t.Parallel only once.
var endToEndTests sync.Map

// endToEnd marks a test that builds real repositories, worktrees and agent processes. With
// -short it is skipped, for a fast inner loop; otherwise it runs in parallel with the other
// end-to-end tests. Each fixture lives in its own temporary directory and its worktrees are
// keyed by that directory, so they share nothing. Parallel tests start only after every
// serial test has finished, so tests that swap package variables (memoryNow,
// memoryWriteFile) or the environment (t.Setenv) never overlap with them.
func endToEnd(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end test; run without -short")
	}
	if _, seen := endToEndTests.LoadOrStore(t, true); !seen {
		t.Parallel()
	}
}
