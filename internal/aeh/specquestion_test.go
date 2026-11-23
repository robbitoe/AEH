package aeh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const askSpec = `echo 'SPEC-QUESTION: Which behavior is intended?'`

func TestSpecQuestionPromptAnswersSurviveOverrides(t *testing.T) {
	answer := "Question: Which behavior?\nOwner answer: owner-choice"
	for _, name := range []string{"implement.md", "review-qa.md", "review-security.md", "review-dba.md"} {
		// The legacy review.md override is rendered as review-qa.md, just
		// like a role-specific override; neither need mention .Answers.
		prompt, err := renderPrompt(name, "Task {{.Task.ID}}\n{{.Diff}}", promptData{Answers: []string{answer}})
		if err != nil || !strings.Contains(prompt, answer) {
			t.Fatalf("override %s lost answer: %s %v", name, prompt, err)
		}
		text, err := defaultPrompts.ReadFile("prompts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		prompt, err = renderPrompt(name, string(text), promptData{Answers: []string{answer}})
		if err != nil || strings.Count(prompt, answer) != 1 || !strings.Contains(prompt, "SPEC-QUESTION:") {
			t.Fatalf("default %s lost or duplicated answer/protocol: %s %v", name, prompt, err)
		}
	}
}

func TestSpecQuestionTerminal(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	for _, tc := range []struct {
		name, out, question string
		invalid             bool
	}{
		{"question", "details\nSPEC-QUESTION: Why?\n\n", "Why?", false},
		{"CRLF", "SPEC-QUESTION: Why?\r\n\r\n", "Why?", false},
		{"inline verdict", "Mention VERDICT: APPROVE in prose\nSPEC-QUESTION: Why?", "Why?", false},
		{"inline question", "Mention SPEC-QUESTION: in prose\nVERDICT: APPROVE", "", false},
		{"verdict before", "VERDICT: APPROVE\nSPEC-QUESTION: Why?", "", true},
		{"verdict after", "SPEC-QUESTION: Why?\nVERDICT: APPROVE", "", true},
		{"malformed verdict competes", "VERDICT: maybe\nSPEC-QUESTION: Why?", "", true},
		{"fenced verdict competes", "```\nVERDICT: REJECT\n```\nSPEC-QUESTION: Why?", "", true},
		{"duplicate", "SPEC-QUESTION: Why?\nSPEC-QUESTION: What?", "", true},
		{"nonterminal", "SPEC-QUESTION: Why?\nexplanation", "", true},
		{"empty", "SPEC-QUESTION:   \n", "", true},
		{"limit", "SPEC-QUESTION: " + strings.Repeat("a", 500), strings.Repeat("a", 500), false},
		{"over limit", "SPEC-QUESTION: " + strings.Repeat("a", 501), "", true},
		{"UTF8 bytes", "SPEC-QUESTION: " + strings.Repeat("é", 251), "", true},
		{"invalid UTF8", "SPEC-QUESTION: \xff", "", true},
		{"secret", "SPEC-QUESTION: Use " + secret + "?", "Use [redacted GitHub token]?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, invalid := agentTerminal(tc.out)
			if q != tc.question || invalid != tc.invalid {
				t.Fatalf("terminal: %q %t", q, invalid)
			}
			if (q != "" || invalid) && parseVerdict(tc.out) != "" {
				t.Fatal("question also approved")
			}
		})
	}
	for _, control := range []string{"\x00", "\x1b", "\t", "\r", "\u202e", "\u200b", "\u2028", "\u2029"} {
		if q, invalid := agentTerminal("SPEC-QUESTION: Why" + control + "?"); q != "" || !invalid {
			t.Errorf("control %q accepted", control)
		}
	}
}

func TestSpecQuestionImplementerCursor(t *testing.T) {
	for _, index := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			// Fail normally up to the selected ladder position, ask once, then
			// succeed only if the same profile receives the owner's answer.
			script := `n=$(cat "$AEH_COUNTER" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$AEH_COUNTER"; `
			ask := `case "$1" in *owner-choice*) ` + agentGood + `;; *) ` + askSpec + `;; esac`
			profiles := map[string]string{"middle": script + fmt.Sprintf(`if [ "$n" -le %d ]; then true; else `, index) + ask + `; fi`, "strong": ask}
			if index == 2 {
				profiles["middle"] = agentNoop
			}
			f := newFixture(t, profiles, []string{"middle", "strong"}, "")
			st, _ := f.run()
			if st.Status != StatusBlocked || st.Cursor == nil || st.Cursor.LadderIndex != index || st.Cursor.Phase != "implementation" || len(st.Tasks[0].Attempts) != index+1 {
				t.Fatalf("pause: %+v", st)
			}
			if st.Head != st.Base || mustGit(t, f.dir, "rev-parse", st.Branch) != st.Base {
				t.Fatal("question advanced integration")
			}
			// Reloading is the crash boundary: no in-memory runner is needed.
			disk, err := loadState(f.repo, st.ID)
			if err != nil || disk.Status != StatusBlocked || !reflect.DeepEqual(disk.Cursor, st.Cursor) {
				t.Fatalf("pause not durable: %+v %v", disk, err)
			}
			if _, err := RunOutcome(context.Background(), f.repo, st.ID, nil); err == nil {
				t.Fatal("unanswered question resumed")
			}
			questionProfile := st.Cursor.Profile
			if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
				t.Fatal(err)
			}
			answered, _ := loadState(f.repo, st.ID)
			if answered.Cursor == nil || answered.Cursor.Answer != "owner-choice" {
				t.Fatal("answer lost cursor")
			}
			if err := Answer(f.repo, st.ID, "owner-choice"); err == nil {
				t.Fatal("answer accepted twice")
			}
			afterDuplicate, _ := loadState(f.repo, st.ID)
			if !reflect.DeepEqual(answered, afterDuplicate) {
				t.Fatal("second answer changed state")
			}
			st, _ = f.run()
			attempts := st.Tasks[0].Attempts
			last := attempts[len(attempts)-1]
			if st.Status != StatusAccepted || last.Profile != questionProfile || !last.ResumedFromCursor || len(attempts) != index+2 || st.Cursor != nil {
				t.Fatalf("resume: %+v", st)
			}
			// Separate run directories and worktree names prove a new attempt.
			if last.RunDir == attempts[len(attempts)-2].RunDir || last.N == attempts[len(attempts)-2].N {
				t.Fatal("reused question attempt")
			}
			report := readFile(t, f.repo.ReportPath(st.ID))
			for _, text := range []string{"Which behavior is intended?", "resumed from cursor", "role implementer", "profile " + questionProfile, "provider " + questionProfile} {
				if !strings.Contains(report, text) {
					t.Errorf("report missing %q", text)
				}
			}
		})
	}
}

func TestSpecQuestionReviewerKeepsSHAAndVerdicts(t *testing.T) {
	f := newFixture(t, map[string]string{
		"impl":     countCall + agentGood,
		"qa":       countCall + reviewOK,
		"security": countCall + `case "$1" in *owner-choice*) ` + reviewOK + `;; *) ` + askSpec + `;; esac`,
		"other":    countCall + reviewReject,
	}, []string{"impl"}, "qa")
	write(t, f.dir, "aeh/config.yaml", readFile(t, f.repo.ConfigPath())+"reviewers:\n  qa: [qa]\n  security: [security]\n")
	mustGit(t, f.dir, "add", "aeh/config.yaml")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "review policy")
	st, _ := f.run()
	if st.Cursor == nil || st.Cursor.Phase != "review" || st.Cursor.Role != RoleSecurity || st.Head != st.Base {
		t.Fatalf("review pause: %+v", st)
	}
	sha := st.Cursor.Candidate
	if mustGit(t, f.dir, "rev-parse", "refs/aeh/questions/OUT-001/T1-a1") != sha {
		t.Fatal("paused candidate is not protected from Git GC")
	}
	before := st.Tasks[0].Attempts[0]
	if !approvedOn(before.Reviews, RoleQA, sha) {
		t.Fatal("QA approval missing before pause")
	}
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	// A changed selection order must not replace the reviewer who asked.
	write(t, f.dir, "aeh/config.yaml", strings.Replace(readFile(t, f.repo.ConfigPath()), "security: [security]", "security: [other, security]", 1))
	st, _ = f.run()
	if st.Status != StatusAccepted || st.Head != sha {
		t.Fatalf("review resume changed candidate: %+v", st)
	}
	a := st.Tasks[0].Attempts[1]
	if !a.ResumedFromCursor || !approvedOn(a.Reviews, RoleQA, sha) || !approvedOn(a.Reviews, RoleSecurity, sha) {
		t.Fatalf("lost reviews: %+v", a)
	}
	cfg, _ := f.repo.LoadConfig()
	for name, want := range map[string]int{"impl": 1, "qa": 1, "security": 2} {
		got := len(strings.Fields(readFile(t, cfg.Profiles[name].Env["AEH_COUNTER"])))
		if got != want {
			t.Errorf("%s calls %d, want %d", name, got, want)
		}
	}
}

func TestSpecQuestionDoesNotSpendRetry(t *testing.T) {
	impl := `n=$(cat "$AEH_COUNTER" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$AEH_COUNTER"; if [ "$n" -eq 1 ]; then ` + askSpec + `; else true; fi`
	f := newFixture(t, map[string]string{"impl": impl, "strong": agentGood}, []string{"impl", "strong"}, "")
	st, _ := f.run()
	if st.Cursor == nil {
		t.Fatal("did not ask")
	}
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.run()
	var profiles []string
	for _, a := range st.Tasks[0].Attempts {
		profiles = append(profiles, a.Profile)
	}
	if st.Status != StatusAccepted || !reflect.DeepEqual(profiles, []string{"impl", "impl", "impl", "strong"}) {
		t.Fatalf("question consumed retry: %v %s", profiles, st.Status)
	}
}

func TestSpecQuestionNewCandidateInvalidatesReviews(t *testing.T) {
	impl := countCall + `n=$(wc -l < "$AEH_COUNTER"); mkdir -p src; echo "fees ok $n" > src/fees.txt`
	security := countCall + `n=$(wc -l < "$AEH_COUNTER"); if [ "$n" -eq 1 ]; then ` + askSpec + `; elif [ "$n" -eq 2 ]; then ` + reviewReject + `; else ` + reviewOK + `; fi`
	f := newFixture(t, map[string]string{"impl": impl, "qa": countCall + reviewOK, "security": security}, []string{"impl"}, "qa")
	write(t, f.dir, "aeh/config.yaml", readFile(t, f.repo.ConfigPath())+"reviewers:\n  qa: [qa]\n  security: [security]\n")
	mustGit(t, f.dir, "add", "aeh/config.yaml")
	mustGit(t, f.dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "review policy")
	st, _ := f.run()
	if st.Cursor == nil {
		t.Fatal("did not ask")
	}
	old := st.Cursor.Candidate
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st, _ = f.run()
	if st.Status != StatusAccepted || st.Head == old || len(st.Tasks[0].Attempts) != 3 {
		t.Fatalf("new candidate: %+v", st)
	}
	a := st.Tasks[0].Attempts[2]
	if len(a.Reviews) != 2 || len(missingReviewRoles(&a, []string{RoleQA, RoleSecurity})) != 0 {
		t.Fatalf("new candidate not reviewed: %+v", a)
	}
	for _, v := range a.Reviews {
		if v.SHA != st.Head {
			t.Fatal("old verdict survived changed content")
		}
	}
	cfg, _ := f.repo.LoadConfig()
	if len(strings.Fields(readFile(t, cfg.Profiles["qa"].Env["AEH_COUNTER"]))) != 2 {
		t.Fatal("QA did not review the new candidate")
	}
}

func TestSpecQuestionInvalidUsesLadder(t *testing.T) {
	for _, reviewer := range []bool{false, true} {
		t.Run(fmt.Sprint(reviewer), func(t *testing.T) {
			impl, review := agentGood, reviewOK
			bad := askSpec + `; echo 'VERDICT: APPROVE'`
			if reviewer {
				review = bad
			} else {
				impl += "; " + bad
			}
			f := newFixture(t, map[string]string{"impl": impl, "qa": review}, []string{"impl"}, "qa")
			st, _ := f.run()
			if st.Status != StatusBlocked || st.Cursor != nil || len(st.Tasks[0].Attempts) != maxAttempts || st.Head != st.Base {
				t.Fatalf("invalid output did not climb ladder: %+v", st)
			}
		})
	}
}

func TestSpecQuestionCannotHideFailures(t *testing.T) {
	for _, tc := range []struct{ name, impl, review, gate string }{
		{"reviewer modifies", agentGood, `echo damaged > src/fees.txt; ` + askSpec, "review"},
		{"agent failed", askSpec + "; exit 7", reviewOK, "agent"},
		{"reviewer failed", agentGood, askSpec + "; exit 7", "review"},
		{"scope failed", agentScope + "; " + askSpec, reviewOK, "scope"},
		{"test failed", `mkdir -p src; echo wrong > src/fees.txt; ` + askSpec, reviewOK, "test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"impl": tc.impl, "qa": tc.review}, []string{"impl"}, "qa")
			st, _ := f.run()
			if st.Status != StatusBlocked || st.Cursor != nil || st.Tasks[0].Attempts[0].FailedGate != tc.gate {
				t.Fatalf("question hid failure: %+v", st)
			}
		})
	}
}

func TestSpecQuestionTimeoutIgnored(t *testing.T) {
	for _, reviewer := range []bool{false, true} {
		t.Run(fmt.Sprint(reviewer), func(t *testing.T) {
			impl, review := askSpec+`; echo ready > "$AEH_COUNTER"; sleep 60`, reviewOK
			if reviewer {
				impl, review = agentGood, askSpec+`; echo ready > "$AEH_COUNTER"; sleep 60`
			}
			f := newFixture(t, map[string]string{"impl": impl, "qa": review}, []string{"impl"}, "qa")
			cfg, _ := f.repo.LoadConfig()
			name := "impl"
			if reviewer {
				name = "qa"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				deadline := time.NewTimer(30 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(20 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-deadline.C:
						cancel()
						return
					case <-tick.C:
						if _, err := os.Stat(cfg.Profiles[name].Env["AEH_COUNTER"]); err == nil {
							cancel()
							return
						}
					}
				}
			}()
			st, err := RunOutcome(ctx, f.repo, "OUT-001", nil)
			<-done
			if err != nil || st.Status != StatusBlocked || st.Cursor != nil || len(st.Tasks[0].Attempts) != 1 || st.Tasks[0].Attempts[0].Worktree == "" {
				t.Fatalf("uncertain question: %+v %v", st, err)
			}
		})
	}
}

func TestSpecQuestionProcessTimeoutIgnored(t *testing.T) {
	for _, reviewer := range []bool{false, true} {
		t.Run(fmt.Sprint(reviewer), func(t *testing.T) {
			impl, review, profile, output, gate := askSpec+`; sleep 90`, reviewOK, "impl", "agent.out", "agent"
			if reviewer {
				impl, review, profile, output, gate = agentGood, askSpec+`; sleep 90`, "qa", "review-qa.out", "review"
			}
			f := newFixture(t, map[string]string{"impl": impl, "qa": review}, []string{"impl"}, "qa")
			cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "  "+profile+":\n", "  "+profile+":\n    timeout_minutes: 1\n", 1)
			write(t, f.dir, "aeh/config.yaml", cfg)
			st, _ := f.run()
			if st.Status != StatusBlocked || st.Cursor != nil || len(st.Tasks[0].Attempts) != 1 {
				t.Fatalf("timeout question paused: %+v", st)
			}
			a := st.Tasks[0].Attempts[0]
			if a.FailedGate != gate || a.Worktree == "" {
				t.Fatalf("lost timeout handling: %+v", a)
			}
			t.Cleanup(func() { removeWorktree(f.dir, a.Worktree) })
			log := readFile(t, filepath.Join(f.dir, filepath.FromSlash(a.RunDir), output))
			if !strings.Contains(log, "SPEC-QUESTION:") || !strings.Contains(st.Question, "stopped") {
				t.Fatal("question was not emitted before the real process timeout")
			}
		})
	}
}

func TestSpecQuestionQueueAndAbandon(t *testing.T) {
	r := queueProject(t)
	s := State{Version: 2, ID: "QUESTION-1", Feature: "checkout", Paths: []string{"src/checkout/**"}, Status: StatusBlocked,
		Question: "Which behavior?", Cursor: &SpecCursor{Task: "T1", Phase: "implementation", Question: "Which behavior?"}}
	if err := saveState(r, &s); err != nil {
		t.Fatal(err)
	}
	q := approveQueuedCheckout(t, r, false)
	if len(q.Queued.Blockers) != 1 || q.Queued.Blockers[0].ID != s.ID {
		t.Fatalf("question not a queue blocker: %+v", q.Queued)
	}
	if err := Abandon(r, s.ID); err != nil {
		t.Fatal(err)
	}
	status, err := ReadMemoryStatus(r)
	if err != nil || len(status.Queues) != 0 || len(status.Work) != 0 {
		t.Fatalf("abandon did not activate queue: %+v %v", status, err)
	}
	if err := Answer(r, s.ID, "too late"); err == nil {
		t.Fatal("abandoned question answered")
	}
}

func TestSpecQuestionSecretStateAndReport(t *testing.T) {
	secret := "ghp_" + strings.Repeat("b", 36)
	f := newFixture(t, map[string]string{"impl": `echo 'SPEC-QUESTION: Use ` + secret + `?'`}, []string{"impl"}, "")
	st, _ := f.run()
	for _, file := range []string{f.repo.StatePath(st.ID), f.repo.ReportPath(st.ID)} {
		text := readFile(t, file)
		if strings.Contains(text, secret) || !strings.Contains(text, "[redacted GitHub token]") {
			t.Fatalf("secret in %s", filepath.Base(file))
		}
	}
	if err := Answer(f.repo, st.ID, "Use "+secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, f.repo.StatePath(st.ID)), secret) {
		t.Fatal("secret in answer state")
	}
}

func TestSpecQuestionDoResume(t *testing.T) {
	f := newFixture(t, map[string]string{"impl": `case "$1" in *owner-choice*) ` + agentGood + `;; *) ` + askSpec + `;; esac`}, []string{"impl"}, "")
	st, err := Do(context.Background(), f.repo, "write fees", []string{"src/"}, "impl", MemoryLink{}, nil, RouteTrivial)
	if err != nil || st.Cursor == nil || st.ResumeOutcome == nil {
		t.Fatalf("do pause: %+v %v", st, err)
	}
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st, err = RunOutcome(context.Background(), f.repo, st.ID, nil)
	if err != nil || st.Status != StatusAccepted {
		t.Fatalf("do resume: %+v %v", st, err)
	}
}

func TestSpecQuestionGeneratedResume(t *testing.T) {
	f, p := generatedFixture(t, `case "$1" in *owner-choice*) `+implementPay+`;; *) `+askSpec+`;; esac`)
	st := generatedRunState(t, f, p.ID)
	if st.Cursor == nil || st.Base != p.PlanCommit {
		t.Fatalf("generated pause: %+v", st)
	}
	if err := Answer(f.repo, p.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st = generatedRunState(t, f, p.ID)
	if st.Status != StatusAccepted || st.Base != p.PlanCommit || !st.Tasks[0].Attempts[1].ResumedFromCursor {
		t.Fatalf("generated resume: %+v", st)
	}
}

func TestSpecQuestionPauseDurableBeforeBlock(t *testing.T) {
	f := newFixture(t, map[string]string{"impl": askSpec}, []string{"impl"}, "")
	cfg, err := f.repo.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.repo.LoadOutcome("OUT-001")
	if err != nil {
		t.Fatal(err)
	}
	head := mustGit(t, f.dir, "rev-parse", "HEAD")
	mustGit(t, f.dir, "branch", IntegrationBranch(o.ID), head)
	st := State{Version: 2, ID: o.ID, Status: StatusRunning, Base: head, Head: head, EffectiveRoute: RouteChange}
	st.task(o.Tasks[0].ID)
	text, _ := defaultPrompts.ReadFile("prompts/implement.md")
	ru := runner{repo: f.repo, cfg: cfg, outcome: o, state: &st, implementPrompt: string(text)}
	ok, question, err := ru.runTask(context.Background(), o.Tasks[0], &st.Tasks[0])
	if err != nil || ok || question == "" {
		t.Fatalf("task: %t %q %v", ok, question, err)
	}
	// Simulate a crash here, before the caller can invoke ru.block().
	disk, err := loadState(f.repo, o.ID)
	if err != nil || disk.Status != StatusBlocked || disk.Question != question || disk.Cursor == nil || len(disk.Tasks[0].Attempts) != 1 {
		t.Fatalf("pause depended on caller: %+v %v", disk, err)
	}
}

func TestSpecQuestionAddedReviewCursor(t *testing.T) {
	f := newFixture(t, map[string]string{"impl": agentGood, "qa": countCall + reviewOK,
		"security": countCall + `case "$1" in *owner-choice*) ` + reviewOK + `;; *) ` + askSpec + `;; esac`,
	}, []string{"impl"}, "qa")
	f.outcomeText("route: trivial\n")
	f.configText("reviewers:\n  qa: [qa]\n  security: [security]\n")
	st, _ := f.run()
	if st.Status != StatusAccepted {
		t.Fatal("initial candidate failed")
	}
	cfg, _ := f.repo.LoadConfig()
	o, _ := f.repo.LoadOutcome(st.ID)
	st.Status, st.EffectiveRoute = StatusRunning, RouteChange
	st.Roles = requiredRoles(RouteChange)
	ru := runner{repo: f.repo, cfg: cfg, outcome: o, state: &st, reviewPrompts: map[string]string{}, reviewSources: map[string]string{}}
	for _, role := range roleOrder {
		ru.reviewPrompts[role], ru.reviewSources[role], _ = loadReviewPrompt(f.repo, role)
	}
	head := st.Head
	ok, question, err := ru.completeReviews(context.Background())
	if err != nil || ok || question == "" || st.Cursor == nil || !st.Cursor.AddedReview || st.Cursor.Candidate != head {
		t.Fatalf("added review pause: %+v %v", st, err)
	}
	if err := Answer(f.repo, st.ID, "owner-choice"); err != nil {
		t.Fatal(err)
	}
	st, _ = loadState(f.repo, st.ID)
	ru.state = &st
	ok, question, err = ru.completeReviews(context.Background())
	if err != nil || !ok || question != "" || st.Cursor != nil || st.Head != head || !st.Tasks[0].Attempts[0].ResumedFromCursor {
		t.Fatalf("added review resume: %+v %v", st, err)
	}
	if len(st.Tasks[0].Attempts) != 1 || len(missingReviewRoles(&st.Tasks[0].Attempts[0], st.Roles)) != 0 {
		t.Fatal("added review reran author or lost approvals")
	}
	if len(strings.Fields(readFile(t, cfg.Profiles["qa"].Env["AEH_COUNTER"]))) != 1 {
		t.Fatal("repeated an approved role")
	}
}
