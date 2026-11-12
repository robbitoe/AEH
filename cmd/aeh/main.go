// Command aeh is the Autonomous Engineering Harness: it turns an approved outcome
// into verified code on its own branch, using agent CLIs and deterministic gates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/robbitoe/aeh/internal/aeh"
)

const version = "0.1.0"

const usage = `aeh — Autonomous Engineering Harness ` + version + `

Setup
  aeh init                          create aeh/config.yaml and an example outcome
  aeh notify test                   send a test event to the configured webhook

Definition
  aeh tests approve <ID>            seal the outcome's acceptance tests

Execution
  aeh run <ID>                      run an outcome until it is accepted or blocked
  aeh do "<change>" [flags]         level 1: one agent, gates only, no sealed tests
        --paths a/,b/               restrict the change to these paths
        --profile <name>            start with this profile
  aeh answer <ID> "<text>"          answer a blocked outcome so it can run again

Tracking
  aeh status                        progress of every outcome
  aeh version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := dispatch(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "aeh:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func dispatch(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0, nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return 0, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	repo, err := aeh.OpenRepo(cwd)
	if err != nil {
		return 1, err
	}

	switch args[0] {
	case "init":
		created, err := aeh.Init(repo)
		for _, c := range created {
			fmt.Println("created", c)
		}
		if err == nil && len(created) == 0 {
			fmt.Println("already initialized")
		}
		return 0, err

	case "tests":
		if len(args) != 3 || args[1] != "approve" {
			return 2, errors.New("usage: aeh tests approve <ID>")
		}
		s, err := aeh.ApproveTests(repo, args[2])
		if err != nil {
			return 1, err
		}
		fmt.Printf("sealed %d files for %s (hash %s)\n", len(s.Files), s.Outcome, s.Hash[:12])
		for _, f := range s.Files {
			fmt.Println("  " + f.Path)
		}
		fmt.Println("commit the tests and aeh/seals/ before running")
		return 0, nil

	case "run":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh run <ID>")
		}
		st, err := aeh.RunOutcome(ctx, repo, args[1], os.Stdout)
		return exitCode(st), err

	case "do":
		fs := flag.NewFlagSet("do", flag.ContinueOnError)
		paths := fs.String("paths", "", "comma-separated paths the change may touch")
		profile := fs.String("profile", "", "profile to start with")
		if err := fs.Parse(reorder(args[1:])); err != nil {
			return 2, err
		}
		text := strings.Join(fs.Args(), " ")
		st, err := aeh.Do(ctx, repo, text, splitList(*paths), *profile, os.Stdout)
		return exitCode(st), err

	case "answer":
		if len(args) < 3 {
			return 2, errors.New(`usage: aeh answer <ID> "<text>"`)
		}
		if err := aeh.Answer(repo, args[1], strings.Join(args[2:], " ")); err != nil {
			return 1, err
		}
		fmt.Printf("answer recorded; run it again with: aeh run %s\n", args[1])
		return 0, nil

	case "status":
		return 0, printStatus(repo)

	case "notify":
		if len(args) != 2 || args[1] != "test" {
			return 2, errors.New("usage: aeh notify test")
		}
		if err := aeh.TestNotify(repo); err != nil {
			return 1, err
		}
		fmt.Println("test event sent")
		return 0, nil
	}
	return 2, fmt.Errorf("unknown command %q; run `aeh help`", args[0])
}

func printStatus(repo aeh.Repo) error {
	states, err := aeh.ListStates(repo)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("no outcomes have run yet")
		return nil
	}
	for _, s := range states {
		done, attempts := 0, 0
		for _, t := range s.Tasks {
			if t.Status == aeh.StatusDone {
				done++
			}
			attempts += len(t.Attempts)
		}
		fmt.Printf("%-22s %-9s tasks %d/%d  attempts %-3d %s\n", s.ID, s.Status, done, len(s.Tasks), attempts, s.Title)
		if s.Status == aeh.StatusBlocked {
			fmt.Println("  question: " + strings.ReplaceAll(firstLines(s.Question, 4), "\n", "\n            "))
		}
	}
	return nil
}

// exitCode: 0 accepted, 3 blocked, 1 anything else.
func exitCode(st aeh.State) int {
	switch st.Status {
	case aeh.StatusAccepted:
		return 0
	case aeh.StatusBlocked:
		return 3
	}
	return 1
}

// reorder moves flags before positional arguments so `aeh do "text" --paths x` works.
func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}
