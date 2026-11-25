package aeh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// caseRulesRepo commits .gitignore files given by path, including paths that differ only
// in case or by an NTFS short name: distinct on Linux, one directory on some hosts.
func caseRulesRepo(t *testing.T, rules map[string]string) (*ignoreChecker, string) {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "core.ignorecase", "false")
	for p, body := range rules {
		blob := mustGitInput(t, dir, body, "hash-object", "-w", "--stdin")
		mustGit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "rules")
	commit := mustGit(t, dir, "rev-parse", "HEAD")
	e, err := newCommitExport(dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	return mustIgnore(t, dir, commit, e), dir
}

func mustGitInput(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	out, err := gitInput(dir, nil, stdin, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// expectIgnored checks the batch and the session answers against want.
func expectIgnored(t *testing.T, c *ignoreChecker, want map[string]bool) {
	t.Helper()
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	got, err := c.ignored(paths)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for p, w := range want {
		one, err := s.ignored(p)
		if err != nil {
			t.Fatal(err)
		}
		if got[p] != w || one != w {
			t.Errorf("%s: batch %v, session %v, want %v", p, got[p], one, w)
		}
	}
}

// Each rule applies only under its exact directory, whatever the host folds together:
// no rule file exists on the host, so no host alias can reach one.
func TestIgnoreCheckerKeepsCaseDistinctRules(t *testing.T) {
	c, _ := caseRulesRepo(t, map[string]string{"A/.gitignore": "x\n", "a/sub/.gitignore": "y\n",
		"LONGDIRECTORYNAME/.gitignore": "z\n", "LONGDI~1/sub/.gitignore": "w\n", ".gitignore": ".GITIGNORE\n"})
	expectIgnored(t, c, map[string]bool{
		"A/x": true, "a/x": false, "a/sub/y": true, "A/sub/y": false,
		"LONGDIRECTORYNAME/z": true, "LONGDI~1/z": false, "LONGDI~1/sub/w": true, "LONGDIRECTORYNAME/sub/w": false,
		".GITIGNORE": true, "sub/.GITIGNORE": true, "longdirectoryname./z": false, "LONGDIRECTORYNAME/gen/z": true,
	})
}

// The translated rules answer as Git does with the real .gitignore files on a checkout.
func TestIgnoreCheckerMatchesGitOnARealCheckout(t *testing.T) {
	rules := map[string]string{
		".gitignore":         "*.log\n!keep.log\n/root-only\nbuild/\ndocs/*.tmp\n**/deep\n# comment\n\ntrail \n",
		"src/.gitignore":     "gen/\n/local\n!important.log\nsub/x\n*.o\n",
		"src/a/.gitignore":   "!*.o\nonly-here\n",
		"we[ird]/.gitignore": "z\n",
		"#hash/.gitignore":   "h\n",
		"crlf/.gitignore":    "gen/\r\n*.secret\r\n/top\r\n",
		"bom/.gitignore":     "\xef\xbb\xbf*.secret\n#c\n",
	}
	c, dir := caseRulesRepo(t, rules)
	mustGit(t, dir, "-c", "core.autocrlf=false", "checkout", "-q", "-f", "HEAD") // the commit's bytes, as in the volume
	paths := []string{"a.log", "keep.log", "src/keep.log", "src/important.log", "src/x/important.log", "root-only", "src/root-only",
		"build/", "src/build/", "build/x", "docs/a.tmp", "docs/sub/a.tmp", "src/docs/a.tmp", "deep", "src/a/deep", "src/gen/",
		"gen/", "src/gen/x", "src/local", "src/a/local", "local", "src/sub/x", "sub/x", "src/a/sub/x", "src/m.o", "src/a/m.o",
		"src/a/b/m.o", "m.o", "src/a/only-here", "src/only-here", "we[ird]/z", "weid/z", "wei/z", "src/a/b/only-here",
		"#hash/h", "#hash/x/h", "h", "trail", "trail ",
		"crlf/deep/gen/output", "crlf/gen/x", "crlf/a.secret", "crlf/x/a.secret", "crlf/top", "crlf/x/top",
		"bom/a.secret", "bom/x/b.secret", "a.secret"}
	// One process per path: with --stdin, Git keeps the per-directory rules of the previous
	// path and can answer a later one with them (src/gen/ then gen/).
	want := map[string]bool{}
	n := 0
	for _, p := range paths {
		real := exec.Command("git", "check-ignore", "--no-index", "-q", "--", p)
		real.Dir = dir
		real.Env = append(commandEnv(nil), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		err := real.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			want[p] = true
			n++
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			want[p] = false
		default:
			t.Fatalf("reference %s: %v", p, err)
		}
	}
	if n < 10 {
		t.Fatalf("the reference checkout ignored only %d paths", n)
	}
	expectIgnored(t, c, want)
}
