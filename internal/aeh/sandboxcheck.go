package aeh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// commandPrograms lists the first word of every simple command in a shell line, so the
// preflight can check that the image has them. Separators inside quotes or after a
// backslash do not split commands. It is a heuristic: only plain words count, and a line
// it cannot follow (an unclosed quote, a $( or backquote substitution) yields nothing
// from that point on, so the check never refuses a valid command.
func commandPrograms(lines ...string) []string {
	plain := regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	assign := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	seen := map[string]bool{}
	for _, line := range lines {
		for _, words := range simpleCommands(line) {
			for _, word := range words {
				if assign.MatchString(word.text) && !word.quoted {
					continue
				}
				if plain.MatchString(word.text) && !word.quoted {
					seen[word.text] = true
				}
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

type shellWord struct {
	text   string
	quoted bool // any part of the word was quoted or escaped
}

// simpleCommands splits a shell line into the words of each simple command, honoring
// single and double quotes and backslashes. It stops at the first construct it does not
// follow (command substitution, an unclosed quote), returning the commands before it.
func simpleCommands(line string) [][]shellWord {
	var cmds [][]shellWord
	var words []shellWord
	var cur strings.Builder
	inWord, quoted := false, false
	endWord := func() {
		if inWord {
			words = append(words, shellWord{cur.String(), quoted})
		}
		cur.Reset()
		inWord, quoted = false, false
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		words = nil
	}
	// stop keeps the complete words of the current command and drops the partial one.
	stop := func() [][]shellWord {
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		return cmds
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'' || c == '"':
			j := strings.IndexByte(line[i+1:], c)
			if j < 0 {
				return stop() // unclosed quote
			}
			body := line[i+1 : i+1+j]
			if c == '"' && strings.ContainsAny(body, "`$") {
				return stop()
			}
			cur.WriteString(body)
			inWord, quoted = true, true
			i += j + 1
		case c == '\\':
			if i+1 < len(line) {
				cur.WriteByte(line[i+1])
				i++
			}
			inWord, quoted = true, true
		case c == '`' || (c == '$' && i+1 < len(line) && line[i+1] == '('):
			return stop() // substitution
		case c == ';' || c == '|' || c == '&' || c == '(' || c == ')' || c == '\n':
			endCmd()
		case c == ' ' || c == '\t':
			endWord()
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds
}

var gitWord = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])git([^A-Za-z0-9_.-]|$)`)

// preflightScript checks the container from inside: user, programs, read-only root,
// writable work view, read-only runner view, file modes and network interfaces.
const preflightScript = `fail() { echo "PREFLIGHT-FAIL: $*"; exit 1; }
[ "$(id -u)" = 10001 ] || fail "the container runs as user $(id -u), expected 10001"
for p in "$@"; do command -v "$p" >/dev/null 2>&1 || fail "program not found in the image: $p"; done
touch /aeh-probe 2>/dev/null && fail "the root filesystem is writable"
[ "$(cat /run/aeh/probe.txt)" = runner ] || fail "cannot read the runner view"
echo x > /run/aeh/x 2>/dev/null && fail "the runner view is writable"
[ "$(./probe.sh)" = executable ] || fail "an executable file (100755) did not run"
./plain.sh >/dev/null 2>&1 && fail "a non-executable file (100644) ran"
echo written > /work/out.txt || fail "cannot write the work view"
echo home > "$HOME/probe" || fail "cannot write HOME"
while IFS= read -r line; do
  case "$line" in *:*) n=${line%%:*}; n=$(echo $n); [ "$n" = lo ] || fail "network interface besides loopback: $n";; esac
done < /proc/net/dev
for v in HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
  eval "val=\${$v}"; [ -z "$val" ] || fail "proxy variable $v is set"
done
echo PREFLIGHT-OK`

// SandboxCheck verifies the sandbox configuration and engine without changing anything
// outside its own throwaway container and volumes.
func SandboxCheck(ctx context.Context, r Repo, w io.Writer) error {
	cfg, err := r.LoadConfig()
	if err != nil {
		return err
	}
	if cfg.Sandbox == nil {
		return errors.New("no sandbox section in aeh/config.yaml; gates and agents run on the host")
	}
	// Opening a flow recovers its interrupted launches, so two checks must not overlap:
	// one would remove the other's live containers.
	unlock, err := lockSandboxFlow(r, "check")
	if err != nil {
		return err
	}
	defer unlock()
	s, err := openSandbox(r, *cfg.Sandbox, "check")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "engine    %s (Docker %s, id %s)\n", s.engine.Endpoint, s.engine.Version, s.engine.ID)
	fmt.Fprintf(w, "image     %s\n          %s\n", s.cfg.Image, s.image.ID)
	fmt.Fprintf(w, "limits    memory %s, cpus %s\n", s.cfg.Memory, s.cfg.CPUs)
	programs := commandPrograms(cfg.Commands.Build, cfg.Commands.Test, strings.ReplaceAll(cfg.Commands.Acceptance, "{{manifest}}", "x"))
	work := treeFiles(map[string][]byte{
		"probe.sh": []byte("#!/bin/sh\necho executable\n"),
		"plain.sh": []byte("#!/bin/sh\necho plain\n"),
	}, map[string]bool{"probe.sh": true})
	runner := treeFiles(map[string][]byte{"probe.txt": []byte("runner")}, nil)
	var out bytes.Buffer
	var back map[string]treeEntry
	res, err := s.run(ctx, sandboxLaunch{
		Kind:    "preflight",
		Argv:    append([]string{"sh", "-c", preflightScript, "preflight"}, programs...),
		Volumes: []sandboxVolume{{Target: "/work", Fill: work}, {Target: "/run/aeh", ReadOnly: true, Fill: runner}},
		Workdir: "/work",
		Timeout: 2 * time.Minute,
		Output:  &out,
	}, func(container string) error {
		return s.readBack(ctx, container, "/work", func(r io.Reader) error {
			var err error
			back, err = readTree(r, "work", 1<<20)
			return err
		})
	})
	if err != nil {
		return err
	}
	text := strings.TrimSpace(out.String())
	if res.Class != ClassProcess || res.ExitCode != 0 || !strings.HasSuffix(text, "PREFLIGHT-OK") {
		return fmt.Errorf("preflight failed (%s, exit %d %s):\n%s", res.Class, res.ExitCode, res.Reason, tail(text, 2000))
	}
	if e, ok := back["out.txt"]; !ok || e.Size != int64(len("written\n")) {
		return errors.New("preflight: the work view was not read back with the file the container wrote")
	}
	fmt.Fprintf(w, "preflight user 10001, read-only root, no network, exact file modes, programs: %s\n", strings.Join(programs, " "))
	for _, c := range [][2]string{{"build", cfg.Commands.Build}, {"test", cfg.Commands.Test}, {"acceptance", cfg.Commands.Acceptance}} {
		if name := c[0]; gitWord.MatchString(c[1]) {
			fmt.Fprintf(w, "warning   commands.%s calls git; sandboxed gates see no Git repository\n", name)
		}
	}
	fmt.Fprintln(w, "sandbox ready")
	return nil
}
