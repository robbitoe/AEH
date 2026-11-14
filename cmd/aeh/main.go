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
	"syscall"

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
  aeh do "<change>" [flags]         level 1: gates and route-based reviews, no sealed tests
        --paths a/,b/               restrict the change to these paths
        --profile <name>            start with this profile
        --route trivial|change|new   declare the route instead of model triage
        --feature <f> --change patch|minor|major --note "..."
                                    the feature this change modifies; recorded in memory
  aeh answer <ID> "<text>"          answer a blocked outcome so it can run again

Memory (local, private: .aeh/memory/ of the main checkout, never published by aeh)
  aeh memory init                   create the memory repository
  aeh memory new <feature> --title "..." --paths a/**,b/**
                                    add a feature spec at version 0.1.0
  aeh memory approve <feature>      approve the spec as it reads now (required by route new)
  aeh memory status                 features, versions, approvals and pending records
  aeh memory lint                   check memory and its privacy settings
  aeh memory record <ID>            retry a pending memory record of an accepted outcome

Tracking
  aeh status                        progress of every outcome
  aeh version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
		route := fs.String("route", "", "trivial, change or new (otherwise triage)")
		feature := fs.String("feature", "", "feature this change modifies")
		change := fs.String("change", "", "patch, minor or major")
		note := fs.String("note", "", "changelog line")
		if err := fs.Parse(reorder(args[1:])); err != nil {
			return 2, err
		}
		text := strings.Join(fs.Args(), " ")
		link := aeh.MemoryLink{Feature: *feature, Change: *change, Note: *note}
		st, err := aeh.Do(ctx, repo, text, splitList(*paths), *profile, link, os.Stdout, *route)
		return exitCode(st), err

	case "memory":
		return memoryCommand(repo, args[1:])

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

func memoryCommand(repo aeh.Repo, args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: aeh memory init|new|approve|status|lint|record")
	}
	switch args[0] {
	case "init":
		created, err := aeh.InitMemory(repo)
		if err != nil {
			return 1, err
		}
		dir, _ := aeh.MemoryDir(repo)
		if created {
			fmt.Println("created memory in", dir)
		} else {
			fmt.Println("memory already exists in", dir)
		}
		return 0, nil

	case "new":
		fs := flag.NewFlagSet("memory new", flag.ContinueOnError)
		title := fs.String("title", "", "feature title")
		paths := fs.String("paths", "", "comma-separated paths the feature owns")
		if err := fs.Parse(reorder(args[1:])); err != nil {
			return 2, err
		}
		if fs.NArg() != 1 {
			return 2, errors.New(`usage: aeh memory new <feature> --title "..." --paths a/**,b/**`)
		}
		s, err := aeh.CreateSpec(repo, fs.Arg(0), *title, splitList(*paths))
		if err != nil {
			return 1, err
		}
		fmt.Printf("created %s %s; describe it in wiki/features/%s/spec.md and commit it in the memory repository\n", s.Feature, s.Version, s.Feature)
		return 0, nil

	case "approve":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh memory approve <feature>")
		}
		s, changed, err := aeh.ApproveSpec(repo, args[1])
		if err != nil {
			return 1, err
		}
		if changed {
			fmt.Printf("approved %s %s; editing its title, paths or text voids the approval\n", s.Feature, s.Version)
		} else {
			fmt.Printf("%s %s is already approved as it reads now\n", s.Feature, s.Version)
		}
		return 0, nil

	case "status":
		st, err := aeh.ReadMemoryStatus(repo)
		if err != nil {
			return 1, err
		}
		if !st.Present {
			fmt.Println("no memory yet; create it with: aeh memory init")
			return 0, nil
		}
		fmt.Println("memory:", st.Dir)
		if st.Dirty != "" {
			fmt.Println("uncommitted changes (aeh will not record until they are committed):")
			fmt.Println("  " + strings.ReplaceAll(st.Dirty, "\n", "\n  "))
		}
		if len(st.Specs) == 0 {
			fmt.Println("no features yet")
		}
		for _, s := range st.Specs {
			last := aeh.LastAccepted(s)
			if last == "" {
				last = "not implemented yet"
			}
			approval := "not approved"
			switch {
			case s.Approved():
				approval = "approved " + s.Approval.Date
			case s.Approval != nil:
				approval = "edited after approval of " + s.Approval.Version
			}
			fmt.Printf("%-24s %-9s %-28s %s\n", s.Feature, s.Version, approval, last)
		}
		for _, p := range st.Pending {
			fmt.Printf("pending record: %s -> %s (%s); retry with: aeh memory record %s\n", p.OutcomeID, p.Feature, p.Change, p.OutcomeID)
		}
		return 0, nil

	case "lint":
		errs, warnings, err := aeh.LintMemory(repo)
		if err != nil {
			return 1, err
		}
		for _, w := range warnings {
			fmt.Println("warning:", w)
		}
		for _, e := range errs {
			fmt.Println("error:", e)
		}
		if len(errs) > 0 {
			return 1, fmt.Errorf("memory has %d error(s)", len(errs))
		}
		fmt.Println("memory ok")
		return 0, nil

	case "record":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh memory record <ID>")
		}
		v, c, err := aeh.RecordPending(repo, args[1])
		if err != nil {
			return 1, err
		}
		fmt.Printf("recorded %s as version %s (memory commit %s)\n", args[1], v, c[:12])
		return 0, nil
	}
	return 2, fmt.Errorf("unknown memory command %q; run `aeh help`", args[0])
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
