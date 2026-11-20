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
// preflight can check that the image has them. It is a heuristic: words with quotes,
// variables or paths are left to the shell.
func commandPrograms(lines ...string) []string {
	split := regexp.MustCompile(`&&|\|\||[;|()\n]`)
	plain := regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	assign := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	seen := map[string]bool{}
	for _, line := range lines {
		for _, part := range split.Split(line, -1) {
			for _, word := range strings.Fields(part) {
				if assign.MatchString(word) {
					continue
				}
				if plain.MatchString(word) {
					seen[word] = true
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
