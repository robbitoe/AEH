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
	"strconv"
	"strings"
	"syscall"

	"github.com/robbitoe/aeh/internal/aeh"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "1.0.0"

var usage = `aeh — Autonomous Engineering Harness ` + version + `

Setup
  aeh init                          create aeh/config.yaml and an example outcome
  aeh notify test                   send a test event to the configured webhook

Discovery (from an idea to a draft book of specs in memory)
  aeh discover new <slug> "<idea>"  start a discovery session
  aeh discover run <slug>           run one round: the agent asks questions or writes the draft book
  aeh discover show <slug>          phase, review, questions and owner actions
  aeh discover answer <slug> <QID> "<text>"
                                    answer a question (again to revise it)
  aeh discover skip <slug> <QID>    skip an optional question
  aeh discover review <slug>        run the advisory book review again when it has no verdict
  aeh discover promote <slug> <feature>
                                    create an unapproved feature spec from a committed draft,
                                    or the next version of a feature from its pending revision
  aeh discover discard <slug> <feature>
                                    discard the pending revision (spec.next.md) of a feature
  aeh discover accept <slug> vision|data
                                    accept an advisory document (existing documents use .next.md)

Adoption (an existing system, area by area)
  aeh adopt inventory [--base <rev>] inspect a pinned base and propose system documentation
  aeh adopt accept system           accept the pending system inventory
  aeh adopt area <feature> --paths src/billing/,... [--title "..."] [--base <rev>]
                                     inspect an area and record observations
  aeh adopt verify [--base <rev>]    verify only the commands in aeh/config.yaml
  aeh adopt promote <feature>       create an unapproved spec from keep observations
  aeh adopt seal <feature>          characterization tests of an approved adopted feature
  aeh adopt status                  inventory, area states and uncovered path groups
  aeh adopt show <feature>          observations of an area, with their state and decision token
  aeh adopt confirm <feature> AC-<n> keep|bug|drop --seen <token>
                                    decide on an observation as you saw it

Definition
  aeh tests approve <ID>            seal the outcome's acceptance tests
  aeh plan <feature>                write, check, review and seal acceptance tests and one task
                                    for the approved spec's new criteria, on branch aeh/<ID>

Execution
  aeh run <ID>                      run an outcome until it is accepted or blocked
  aeh do "<change>" [flags]         level 1: gates and route-based reviews, no sealed tests
        --paths a/,b/               restrict the change to these paths
        --profile <name>            start with this profile
        --route trivial|change|new   declare the route instead of model triage
        --feature <f> --change patch|minor|major --note "..."
                                    the feature this change modifies; recorded in memory
  aeh answer <ID> "<text>"          answer a blocked outcome so it can run again
  aeh abandon <ID>                  abandon unfinished work, preserving its branch and evidence

Memory (local, private: .aeh/memory/ of the main checkout, never published by aeh)
  aeh memory init                   create the memory repository
  aeh memory new <feature> --title "..." --paths a/**,b/**
                                    add a feature spec at version 0.1.0
  aeh memory approve <feature> [--clarify AC-<n>,...|none] [--relocate <from>=<to>[,...]]
                                    approve the exact spec; queue it while unfinished work remains
  aeh memory status                 features, pending work, queued approvals and blockers
  aeh memory lint                   check memory and its privacy settings
  aeh memory record <ID>            retry a pending memory record of an accepted outcome

Sandbox (optional: gates and agents in containers)
  aeh sandbox check                 check the engine, the image and the container limits
  aeh sandbox login <profile>       log a profile's CLI in inside the agent image and keep its session

Tracking
  aeh status                        progress of every outcome
  aeh version                       print the version
  aeh help                          this list
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

	case "discover":
		return discoverCommand(ctx, repo, args[1:])

	case "adopt":
		return adoptCommand(ctx, repo, args[1:])

	case "plan":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh plan <feature>")
		}
		st, err := aeh.PlanFeature(ctx, repo, args[1], os.Stdout)
		if err != nil {
			return 1, err
		}
		if st.Status == aeh.StatusBlocked {
			return 3, nil
		}
		return 0, nil

	case "abandon":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh abandon <ID>")
		}
		if err := aeh.Abandon(repo, args[1]); err != nil {
			return 1, err
		}
		fmt.Printf("%s abandoned; branch and evidence kept\n", args[1])
		return 0, nil

	case "answer":
		if len(args) < 3 {
			return 2, errors.New(`usage: aeh answer <ID> "<text>"`)
		}
		if err := aeh.Answer(repo, args[1], strings.Join(args[2:], " ")); err != nil {
			return 1, err
		}
		fmt.Printf("answer recorded; run it again with: aeh run %s (or aeh plan <feature>, or aeh adopt seal <feature>, for a blocked plan)\n", args[1])
		return 0, nil

	case "status":
		return 0, printStatus(repo)

	case "sandbox":
		badArgs := errors.New("usage: aeh sandbox check | aeh sandbox login <profile>")
		if len(args) > 1 && args[1] != "check" {
			if len(args) != 3 || args[1] != "login" {
				return 2, badArgs
			}
			if err := aeh.SandboxLogin(ctx, repo, args[2], os.Stdin, os.Stdout, os.Stderr); err != nil {
				return 1, err
			}
			return 0, nil
		}
		if len(args) != 2 || args[1] != "check" {
			return 2, badArgs
		}
		if err := aeh.SandboxCheck(ctx, repo, os.Stdout); err != nil {
			return 1, err
		}
		return 0, nil

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
		usage := errors.New("usage: aeh memory approve <feature> [--clarify AC-<n>,...|none] [--relocate <from>=<to>[,...]]")
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return 2, usage
		}
		fs := flag.NewFlagSet("approve", flag.ContinueOnError)
		clarify := fs.String("clarify", "", "changed criteria approved as wording-only clarifications (AC-<n>,... or none)")
		relocate := fs.String("relocate", "", "owner-declared literal path moves (<from>=<to>[,...])")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
			return 2, usage
		}
		var opt aeh.ApproveOptions
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "clarify" {
				opt.SetClarify = true
			}
			if f.Name == "relocate" {
				opt.SetRelocate = true
			}
		})
		if opt.SetRelocate {
			var err error
			if opt.Relocations, err = aeh.ParseRelocations(*relocate); err != nil {
				return 2, err
			}
		}
		if opt.SetClarify && *clarify != "none" {
			for _, item := range strings.Split(*clarify, ",") {
				n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(item), "AC-"))
				if err != nil || n < 1 || !strings.HasPrefix(strings.TrimSpace(item), "AC-") {
					return 2, fmt.Errorf("--clarify: %q is not AC-<n>", item)
				}
				opt.Clarify = append(opt.Clarify, n)
			}
		}
		s, changed, err := aeh.ApproveSpecWith(repo, args[1], opt)
		if err != nil {
			return 1, err
		}
		if s.Queued != nil {
			if len(s.Queued.Blockers) == 0 {
				// It replaced an earlier queued approval; aeh memory status (or the next run, plan or
				// record) activates it.
				fmt.Printf("approved %s %s, queued without blockers; run aeh memory status to activate it (version in force until then: %s)\n", s.Feature, s.Version, s.Queued.CurrentVersion)
			} else {
				fmt.Printf("approved %s %s, queued; version in force remains %s\n", s.Feature, s.Version, s.Queued.CurrentVersion)
			}
			for _, b := range s.Queued.Blockers {
				fmt.Printf("  blocker: %s (%s)\n", b.ID, b.Reason)
			}
		} else if changed {
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
		for _, diagnostic := range st.Errors {
			fmt.Println("error:", diagnostic)
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
			if session, ok := st.Revisions[s.Feature]; ok {
				fmt.Printf("  pending revision: spec.next.md from discovery %s; promote or discard it with aeh discover\n", session)
			}
			fmt.Print(pendingMemoryText(s.Feature, st.Work, st.Queues))
		}
		for _, q := range st.Queues {
			found := false
			for _, s := range st.Specs {
				found = found || s.Feature == q.Feature
			}
			if !found {
				fmt.Printf("%s: spec missing\n", q.Feature)
				fmt.Print(pendingMemoryText(q.Feature, st.Work, st.Queues))
			}
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

func pendingMemoryText(feature string, work []aeh.PendingWork, queues []aeh.QueuedApproval) string {
	var text strings.Builder
	for _, w := range work {
		if w.Feature == feature {
			fmt.Fprintf(&text, "  pending work: %s (%s)\n", w.ID, w.Reason)
		}
	}
	for _, q := range queues {
		if q.Feature != feature {
			continue
		}
		fmt.Fprintf(&text, "  queued approval: %s (in force: %s)\n", q.Approval.Version, q.CurrentVersion)
		if q.ActivationWaiting != "" {
			fmt.Fprintf(&text, "    activation waiting: %s\n", q.ActivationWaiting)
		}
		for _, b := range q.Blockers {
			fmt.Fprintf(&text, "    blocker: %s (%s)\n", b.ID, b.Reason)
		}
	}
	return text.String()
}

func adoptCommand(ctx context.Context, repo aeh.Repo, args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: aeh adopt inventory|accept|area|verify|promote|seal|status|show|confirm")
	}
	switch args[0] {
	case "inventory", "verify", "area":
		fs := flag.NewFlagSet("adopt "+args[0], flag.ContinueOnError)
		base := fs.String("base", "", "commit to inspect (default: main checkout HEAD)")
		paths := fs.String("paths", "", "comma-separated fixed area paths")
		title := fs.String("title", "", "title of a new area")
		if err := fs.Parse(reorder(args[1:])); err != nil {
			return 2, err
		}
		if args[0] == "verify" {
			if fs.NArg() != 0 || *paths != "" || *title != "" {
				return 2, errors.New("usage: aeh adopt verify [--base <rev>]")
			}
			v, err := aeh.AdoptVerify(ctx, repo, *base, os.Stdout)
			if err != nil {
				return 1, err
			}
			fmt.Println("Verification evidence: .aeh/adopt/verify.json")
			if !v.Passed {
				return 1, nil
			}
			return 0, nil
		}
		var st aeh.AdoptRun
		var err error
		if args[0] == "inventory" {
			if fs.NArg() != 0 || *paths != "" || *title != "" {
				return 2, errors.New("usage: aeh adopt inventory [--base <rev>]")
			}
			st, err = aeh.AdoptInventory(ctx, repo, *base, os.Stdout)
		} else {
			if fs.NArg() != 1 || *paths == "" {
				return 2, errors.New("usage: aeh adopt area <feature> --paths src/billing/,... [--title ...] [--base <rev>]")
			}
			st, err = aeh.AdoptInspectArea(ctx, repo, fs.Arg(0), splitList(*paths), *title, *base, os.Stdout)
		}
		if err != nil {
			return 1, err
		}
		if st.Status == aeh.StatusBlocked {
			fmt.Println("Adoption blocked: adjust the profile or prompt, then run the command again to retry.")
			return 3, nil
		}
		fmt.Println("Adoption recorded; report: .aeh/reports/adopt.md")
		return 0, nil
	case "accept":
		if len(args) != 2 || args[1] != "system" {
			return 2, errors.New("usage: aeh adopt accept system")
		}
		if err := aeh.AdoptAcceptSystem(repo); err != nil {
			return 1, err
		}
		fmt.Println("Current system inventory accepted in memory.")
		return 0, nil
	case "seal":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh adopt seal <feature>")
		}
		st, err := aeh.AdoptSeal(ctx, repo, args[1], os.Stdout)
		if err != nil {
			return 1, err
		}
		if st.Status == aeh.StatusBlocked {
			return 3, nil
		}
		return 0, nil
	case "promote":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh adopt promote <feature>")
		}
		if err := aeh.AdoptPromote(repo, args[1]); err != nil {
			return 1, err
		}
		fmt.Printf("Feature %s promoted; approve with: aeh memory approve %s\n", args[1], args[1])
		return 0, nil
	case "status":
		if len(args) != 1 {
			return 2, errors.New("usage: aeh adopt status")
		}
		st, err := aeh.AdoptStatus(repo)
		if err != nil {
			return 1, err
		}
		fmt.Printf("Inventory: %s; base %s; date %s\n", st.Inventory, st.Base, st.Date)
		for _, area := range st.Areas {
			fmt.Printf("%s: %s (%s)\n", area.Feature, area.State, strings.Join(area.Paths, ", "))
		}
		fmt.Println("Uncovered groups at the accepted inventory base:")
		for _, group := range st.Uncovered {
			fmt.Println("  " + group)
		}
		return 0, nil
	}
	if len(args) < 2 {
		return 2, errors.New("usage: aeh adopt show|confirm <feature> ...")
	}
	feature := args[1]
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh adopt show <feature>")
		}
		area, err := aeh.LoadAdoptArea(repo, feature)
		if err != nil {
			return 1, err
		}
		fmt.Printf("area %s: %s\n  paths: %s\n", area.Feature, area.Title, strings.Join(area.Paths, ", "))
		for _, o := range area.Observations {
			state := o.Status
			if o.Decision != "" {
				state += ", " + o.Decision
			}
			fmt.Printf("  AC-%d (%s) %s\n      observed at %s in %s\n", o.ID, state, o.Text, o.CodeSHA[:7], strings.Join(o.Locations, ", "))
			if o.Status == aeh.ObservationActive {
				fmt.Printf("      decide: aeh adopt confirm %s AC-%d keep|bug|drop --seen %s\n", area.Feature, o.ID, o.Token())
			}
		}
		return 0, nil
	case "confirm":
		fs := flag.NewFlagSet("adopt confirm", flag.ContinueOnError)
		seen := fs.String("seen", "", "the token aeh adopt show printed for the observation")
		if err := fs.Parse(reorder(args[2:])); err != nil {
			return 2, err
		}
		if fs.NArg() != 2 || *seen == "" {
			return 2, errors.New("usage: aeh adopt confirm <feature> AC-<n> keep|bug|drop --seen <token>")
		}
		o, err := aeh.AdoptConfirm(repo, feature, fs.Arg(0), fs.Arg(1), *seen)
		if err != nil {
			return 1, err
		}
		fmt.Printf("AC-%d is %s (%s)\n", o.ID, o.Status, o.Decision)
		return 0, nil
	}
	return 2, fmt.Errorf("unknown adopt command %q", args[0])
}

func discoverCommand(ctx context.Context, repo aeh.Repo, args []string) (int, error) {
	if len(args) < 2 {
		return 2, errors.New("usage: aeh discover new|run|show|answer|skip|review|promote|discard|accept <slug> ...")
	}
	slug := args[1]
	switch args[0] {
	case "new":
		if len(args) < 3 {
			return 2, errors.New(`usage: aeh discover new <slug> "<idea>"`)
		}
		if _, err := aeh.DiscoverNew(repo, slug, strings.Join(args[2:], " ")); err != nil {
			return 1, err
		}
		fmt.Printf("discovery %s started; run its first round with: aeh discover run %s\n", slug, slug)
		return 0, nil
	case "run":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh discover run <slug>")
		}
		st, err := aeh.DiscoverRun(ctx, repo, slug, os.Stdout)
		if err != nil {
			return 1, err
		}
		if st.Status == aeh.StatusBlocked {
			fmt.Println(st.Question)
			return 3, nil
		}
		printDiscovery(st, false)
		return 0, nil
	case "show":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh discover show <slug>")
		}
		st, err := aeh.DiscoverShow(repo, slug)
		if err != nil {
			return 1, err
		}
		printDiscovery(st, true)
		return 0, nil
	case "answer":
		if len(args) < 4 {
			return 2, errors.New(`usage: aeh discover answer <slug> <QID> "<text>"`)
		}
		st, err := aeh.DiscoverAnswer(repo, slug, args[2], strings.Join(args[3:], " "))
		if err != nil {
			return 1, err
		}
		fmt.Printf("answer to %s recorded; phase %s\n", args[2], st.Phase)
		return 0, nil
	case "skip":
		if len(args) != 3 {
			return 2, errors.New("usage: aeh discover skip <slug> <QID>")
		}
		st, err := aeh.DiscoverSkip(repo, slug, args[2])
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s skipped; phase %s\n", args[2], st.Phase)
		return 0, nil
	case "review":
		if len(args) != 2 {
			return 2, errors.New("usage: aeh discover review <slug>")
		}
		st, err := aeh.DiscoverReview(ctx, repo, slug, os.Stdout)
		if err != nil {
			return 1, err
		}
		printDiscovery(st, false)
		return 0, nil
	case "promote", "accept":
		if len(args) != 3 {
			return 2, fmt.Errorf("usage: aeh discover %s <slug> <feature|vision|data>", args[0])
		}
		var st aeh.DiscoveryState
		var err error
		actions := &st.Acceptances
		if args[0] == "promote" {
			st, err = aeh.DiscoverPromote(repo, slug, args[2])
			actions = &st.Promotions
		} else {
			st, err = aeh.DiscoverAccept(repo, slug, args[2])
		}
		if err != nil {
			return 1, err
		}
		// One line for this action (the latest of its name); aeh discover show prints the rest.
		for i := len(*actions) - 1; i >= 0; i-- {
			if a := (*actions)[i]; a.Name == args[2] {
				fmt.Printf("%sed %s to %s (%s)\n", strings.TrimSuffix(args[0], "e"), a.Name, a.Target, a.Date)
				break
			}
		}
		if args[0] == "promote" {
			fmt.Printf("approve it when it reads right: aeh memory approve %s\n", args[2])
		}
		return 0, nil
	case "discard":
		if len(args) != 3 {
			return 2, errors.New("usage: aeh discover discard <slug> <feature>")
		}
		if _, err := aeh.DiscoverDiscard(repo, slug, args[2]); err != nil {
			return 1, err
		}
		fmt.Printf("discarded the pending revision of %s\n", args[2])
		return 0, nil
	}
	return 2, fmt.Errorf("unknown discover command %q", args[0])
}

// printDiscovery shows a session. Questions and answers passed the display policy when they
// were recorded.
func printDiscovery(st aeh.DiscoveryState, all bool) {
	fmt.Printf("discovery %s: phase %s, %d round(s)\n", st.Slug, st.Phase, len(st.Rounds))
	if st.Status == aeh.StatusBlocked {
		fmt.Println("  blocked: " + strings.ReplaceAll(firstLines(st.Question, 4), "\n", "\n           "))
	}
	if p := st.Book; p != nil {
		fmt.Printf("pending book: round %d, author attempt %d\n", p.Round, p.Attempt)
		if v := p.Review; v != nil {
			fmt.Printf("  review: Reviewer %s (%s); %s; book SHA-256 %s\n", v.Profile, v.Provider, v.Status, v.Book)
		}
	}
	for i, r := range st.Rounds {
		if !all && i != len(st.Rounds)-1 {
			continue
		}
		if r.Kind == "book" {
			fmt.Printf("round %d: draft book with %d file(s) in memory under wiki/discovery/%s/drafts/\n", r.N, len(r.Files), st.Slug)
			if v := r.Review; v != nil {
				validity := "current"
				if v.Stale {
					validity = "stale"
				}
				fmt.Printf("  review: Reviewer %s (%s); %s; %s; book SHA-256 %s\n", v.Profile, v.Provider, v.Status, validity, v.Book)
			}
		}
		fmt.Printf("round %d questions:\n", r.N)
		for _, q := range r.Questions {
			kind := "optional"
			if q.Blocking {
				kind = "blocking"
			}
			state := "open"
			switch {
			case len(q.Answers) > 0:
				state = "answered"
			case q.Skipped:
				state = "skipped"
			}
			fmt.Printf("  %s (%s, %s) %s\n      why: %s\n", q.ID, kind, state, q.Text, q.Why)
			if q.Attribution != "" {
				fmt.Printf("      %s\n", q.Attribution)
			}
			if len(q.Options) > 0 {
				fmt.Printf("      options: %s\n", strings.Join(q.Options, " | "))
			}
			if len(q.Answers) > 0 {
				fmt.Printf("      answer: %s\n", strings.ReplaceAll(firstLines(q.Answers[len(q.Answers)-1].Text, 3), "\n", "\n              "))
			}
		}
	}
	for _, a := range st.Promotions {
		if a.Revision {
			fmt.Printf("promoted the revision of %s to %s (%s)\n", a.Name, a.Target, a.Date)
			continue
		}
		fmt.Printf("promoted %s to %s (%s)\n", a.Name, a.Target, a.Date)
	}
	for _, a := range st.Discards {
		fmt.Printf("discarded the revision of %s (%s)\n", a.Name, a.Date)
	}
	for _, a := range st.Acceptances {
		fmt.Printf("accepted %s to %s (%s)\n", a.Name, a.Target, a.Date)
	}
	switch st.Phase {
	case aeh.DiscoveryAwaiting:
		fmt.Printf("answer with: aeh discover answer %s <QID> \"...\"\n", st.Slug)
	case aeh.DiscoveryReady:
		fmt.Printf("next round: aeh discover run %s\n", st.Slug)
	}
}

func printStatus(repo aeh.Repo) error {
	if err := aeh.RecoverLiveWork(repo); err != nil {
		return err
	}
	states, err := aeh.ListStates(repo)
	if err != nil {
		return err
	}
	sessions, err := aeh.ListDiscoveries(repo)
	if err != nil {
		return err
	}
	for _, d := range sessions {
		fmt.Printf("%-22s discovery %-7s phase %-16s rounds %d\n", d.Slug, d.Status, d.Phase, len(d.Rounds))
	}
	plans, err := aeh.ListPlanStates(repo)
	if err != nil {
		return err
	}
	for _, p := range plans {
		fmt.Printf("%-22s plan %-9s phase %-10s %s %s\n", p.ID, p.Status, p.Phase, p.Feature, p.SpecVersion)
		if p.Status == aeh.StatusBlocked {
			fmt.Println("  question: " + strings.ReplaceAll(firstLines(p.Question, 4), "\n", "\n            "))
		}
	}
	if len(states) == 0 {
		if len(plans) == 0 && len(sessions) == 0 {
			fmt.Println("no outcomes have run yet")
		}
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
		if s.Cursor != nil {
			fmt.Println("  " + s.Cursor.Summary())
		}
		for _, task := range s.Tasks {
			for _, a := range task.Attempts {
				if a.SpecQuestion != nil && s.Cursor == nil {
					fmt.Println("  " + a.SpecQuestion.Summary())
				}
				if a.ResumedFromCursor {
					fmt.Printf("  task %s attempt %d resumed from cursor\n", task.ID, a.N)
				}
			}
		}
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
