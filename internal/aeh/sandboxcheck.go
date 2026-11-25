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
			// Inside double quotes a backslash may escape the closing quote, and $ or `
			// substitute: not followed, so stop.
			if c == '"' && strings.ContainsAny(body, "`$\\") {
				return stop()
			}
			cur.WriteString(body)
			inWord, quoted = true, true
			i += j + 1
		case c == '\\' && i+1 < len(line) && line[i+1] == '\n':
			i++ // a line continuation joins lines; it is not part of any word
		case c == '\\':
			if i+1 < len(line) {
				cur.WriteByte(line[i+1])
				i++
			}
			inWord, quoted = true, true
		case c == '`' || (c == '$' && i+1 < len(line) && line[i+1] == '('):
			return stop() // substitution
		case c == '#' && !inWord:
			// A comment runs to the end of its line; the next line is new commands.
			if j := strings.IndexByte(line[i:], '\n'); j >= 0 {
				i += j - 1
			} else {
				i = len(line)
			}
		case c == '<' && i+1 < len(line) && line[i+1] == '<':
			return stop() // a here-document: its body is not commands
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
for p in "$@"; do command -v "$p" >/dev/null 2>&1 || echo "MISSING-PROGRAM: $p"; done
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
	fmt.Fprintf(w, "preflight user 10001, read-only root, no network, exact file modes\n")
	// Program detection is a heuristic over shell text, so a missing program is a warning:
	// it must never refuse a command that works.
	for _, line := range strings.Split(text, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "MISSING-PROGRAM: "); ok {
			fmt.Fprintf(w, "warning   %s was not found in the image; if a command runs it, add it to the image\n", p)
		}
	}
	for _, c := range [][2]string{{"build", cfg.Commands.Build}, {"test", cfg.Commands.Test}, {"acceptance", cfg.Commands.Acceptance}} {
		if name := c[0]; gitWord.MatchString(c[1]) {
			fmt.Fprintf(w, "warning   commands.%s calls git; sandboxed gates see no Git repository\n", name)
		}
	}
	if s.agents() {
		if err := checkAgentImage(ctx, s, cfg, w); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "sandbox ready")
	return nil
}

// checkAgentImage runs one throwaway agent container: the agent image runs as user 10001
// with the agent network, and has the program of every profile's command.
func checkAgentImage(ctx context.Context, s *sandbox, cfg Config, w io.Writer) error {
	fmt.Fprintf(w, "agents    %s\n          %s\n", s.cfg.AgentImage, s.agent.ID)
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	programs := map[string][]string{}
	var argv []string
	for _, name := range names {
		if c := cfg.Profiles[name].Command; len(c) > 0 {
			if programs[c[0]] == nil {
				argv = append(argv, c[0])
			}
			programs[c[0]] = append(programs[c[0]], name)
		}
	}
	script := `test "$(id -u)" = 10001 || { echo "not user 10001"; exit 1; }
for p; do command -v "$p" >/dev/null 2>&1 || echo "MISSING-PROGRAM: $p"; done
echo AGENT-OK`
	var out bytes.Buffer
	res, err := s.run(ctx, sandboxLaunch{Kind: "preflight", Argv: append([]string{"sh", "-c", script, "preflight"}, argv...),
		Workdir: "/tmp", Timeout: 2 * time.Minute, Output: &out, Agent: true}, nil)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(out.String())
	if res.Class != ClassProcess || res.ExitCode != 0 || !strings.HasSuffix(text, "AGENT-OK") {
		return fmt.Errorf("agent preflight failed (%s, exit %d %s):\n%s", res.Class, res.ExitCode, res.Reason, tail(text, 2000))
	}
	fmt.Fprintf(w, "preflight agent image: user 10001, read-only root\n")
	for _, line := range strings.Split(text, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "MISSING-PROGRAM: "); ok {
			fmt.Fprintf(w, "warning   %s (profiles %s) was not found in the agent image\n", p, strings.Join(programs[p], ", "))
		}
	}
	for _, name := range names {
		paths := cfg.Profiles[name].sandboxSession()
		if len(paths) == 0 {
			continue
		}
		store, err := sessionStore(s.root(), name, false)
		var files map[string][]byte
		if err == nil {
			files, err = loadSession(store, paths)
		}
		if err != nil {
			fmt.Fprintf(w, "warning   profile %s: %v\n", name, err)
		} else if len(files) == 0 {
			fmt.Fprintf(w, "warning   profile %s has no saved session; run: aeh sandbox login %s\n", name, name)
		}
	}
	return nil
}
