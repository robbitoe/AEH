package aeh

import "testing"

func TestCodexRepoCheckError(t *testing.T) {
	cases := []struct {
		cmd  []string
		want bool
	}{
		{[]string{"codex", "exec", "--sandbox", "read-only", "-"}, true},
		{[]string{`C:\tools\Codex.CMD`, "exec", "-"}, true},
		{[]string{"/usr/bin/codex.exe", "exec", "-"}, true},
		{[]string{"codex", "exec", "--skip-git-repo-check", "-"}, false},
		{[]string{"codex", "login"}, false},
		{[]string{"claude", "-p", "exec"}, false},
		{[]string{"codex"}, false},
		{nil, false},
	}
	for _, c := range cases {
		err := codexRepoCheckError("p", Profile{Command: c.cmd})
		if (err != nil) != c.want {
			t.Errorf("%q: error %v, want error %v", c.cmd, err, c.want)
		}
	}
}
