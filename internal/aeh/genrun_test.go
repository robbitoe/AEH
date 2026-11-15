package aeh

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const implementPay = `mkdir -p src; echo paid > src/pay.txt`

func generatedFixture(t *testing.T, script string) (*planFixture, PlanState) {
	t.Helper()
	f := newPlanFixture(t, authorOK, countCall+reviewOK)
	f.setScript("impl", countCall+script)
	f.writeConfig("plan:\n  ladder: [author]\n")
	cfg := strings.Replace(readFile(t, f.repo.ConfigPath()), "ladder: [author]", "ladder: [impl]", 1)
	cfg += "  impl:\n    provider: implementer\n    command: [sh, " + yamlQuote(filepath.ToSlash(filepath.Join(f.tmp, "impl.sh"))) + ", '{{prompt}}']\n    env:\n      AEH_COUNTER: " + yamlQuote(filepath.ToSlash(filepath.Join(f.tmp, "impl.count"))) + "\n"
	write(t, f.dir, "aeh/config.yaml", cfg)
	f.commitAll("configure independent implementation")
	p := f.plan()
	if p.Phase != PlanLogged {
		t.Fatalf("plan failed: %+v", p)
	}
	return f, p
}

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func generatedRunState(t *testing.T, f *planFixture, id string) State {
	t.Helper()
	var log strings.Builder
	st, err := RunOutcome(context.Background(), f.repo, id, &log)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, log.String())
	}
	return st
}

func TestGeneratedRunAcceptsAndRecordsImplementation(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted || st.Base != p.PlanCommit || st.InitialRoute != p.Route || !reflect.DeepEqual(st.Roles, p.Roles) {
		t.Fatalf("generated run: %+v", st)
	}
	im, err := readImplementations(filepath.Join(memDir(t, f.repo), filepath.FromSlash(implementedRel("pay"))))
	if err != nil || len(im) != 1 || im[0].Outcome != planID || im[0].Approval != p.SpecDigest || im[0].Code != st.Head ||
		len(im[0].Criteria) != 1 || !reflect.DeepEqual(im[0].Criteria[0].Tests, []string{planDir + "/pay.sh"}) {
		t.Fatalf("implementation: %+v %v", im, err)
	}
	for _, v := range st.Tasks[0].Attempts[0].Reviews {
		if v.SHA != st.Head || v.Profile != "qa" || v.Verdict != "APPROVE" {
			t.Fatalf("run review: %+v", v)
		}
	}
	report := readFile(t, f.repo.ReportPath(planID))
	for _, text := range []string{"## Plan", p.PlanCommit, "Test author: `author`", "sealed by runner", "Plan checks:", "Role: **security**", "Provider: `qa`", "Verdict: **APPROVE**"} {
		if !strings.Contains(report, text) {
			t.Fatalf("report missing %q:\n%s", text, report)
		}
	}
	gates := readGateResults(f.dir, st.FinalRun)
	if gates[len(gates)-1].Name != "acceptance" || !gates[len(gates)-1].Passed {
		t.Fatalf("final acceptance suite not run: %+v", gates)
	}
}

func TestGeneratedRunRejectsSealedTestChangesAndResumes(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	for _, script := range []string{
		`echo 'exit 0' > ` + planDir + `/pay.sh`,
		`rm ` + planDir + `/pay.sh`,
		`echo 'exit 0' > ` + planDir + `/extra.sh`,
	} {
		f.setScript("impl", countCall+implementPay+"; "+script)
		st := generatedRunState(t, f, planID)
		if st.Status != StatusBlocked || st.Head != p.PlanCommit {
			t.Fatalf("tampering accepted: %+v", st)
		}
		for _, a := range st.Tasks[0].Attempts {
			if a.FailedGate != "hash" {
				t.Fatalf("wrong failure: %+v", a)
			}
		}
		if err := Answer(f.repo, planID, "Retry with unchanged tests"); err != nil {
			t.Fatal(err)
		}
	}
	f.setScript("impl", countCall+implementPay)
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted || st.Base != p.PlanCommit {
		t.Fatalf("resume lost provenance: %+v", st)
	}
}

func TestGeneratedRunPreflightConflicts(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	if _, err := ApproveTests(f.repo, planID); err == nil || !strings.Contains(err.Error(), "generated") {
		t.Fatalf("manual approval allowed: %v", err)
	}
	for _, file := range []string{"aeh/outcomes/" + planID + ".yaml", "aeh/seals/" + planID + ".json"} {
		blob, _ := gitBlob(f.dir, p.PlanCommit, file)
		write(t, f.dir, file, blob+"\n")
		if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "shadowing file") {
			t.Fatalf("shadow %s accepted: %v", file, err)
		}
		// Identical copies are allowed and never establish provenance.
		write(t, f.dir, file, blob)
	}
	original := readFile(t, f.repo.ConfigPath())
	for _, cfg := range []string{
		strings.Replace(original, "ladder: [impl]", "ladder: [impl, author]", 1),
		strings.Replace(strings.Replace(original, "independence: profile", "independence: provider", 1), "provider: implementer", "provider: author", 1),
	} {
		write(t, f.dir, "aeh/config.yaml", cfg)
		if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "independent from test author") {
			t.Fatalf("dependent implementer accepted: %v", err)
		}
	}
	write(t, f.dir, "aeh/config.yaml", original)
	mustGit(t, f.dir, "update-ref", "refs/heads/"+IntegrationBranch(planID), p.Base, p.PlanCommit)
	if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "must point at plan commit") {
		t.Fatalf("moved plan branch accepted: %v", err)
	}
	mustGit(t, f.dir, "update-ref", "refs/heads/"+IntegrationBranch(planID), p.PlanCommit, p.Base)
	p.Phase = PlanPublished
	if err := savePlanState(f.repo, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := RunOutcome(context.Background(), f.repo, planID, nil); err == nil || !strings.Contains(err.Error(), "is not finished; run aeh plan pay") {
		t.Fatalf("unfinished plan accepted: %v", err)
	}
	if f.count("impl") != 0 {
		t.Fatal("preflight ran an implementer")
	}
}

func TestGeneratedRunStaleSpecRequiresRestart(t *testing.T) {
	f, _ := generatedFixture(t, implementPay)
	editSpec(t, f.repo, "pay", "Pays the cart.", "Updated payment behavior.")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	st := generatedRunState(t, f, planID)
	if st.Status != StatusBlocked || !st.RestartRequired || st.Question != "spec changed since plan; approve it and run aeh plan again" || f.count("impl") != 0 {
		t.Fatalf("stale spec accepted: %+v", st)
	}
}

func TestGeneratedRunPendingImplementationRetry(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	dir := memDir(t, f.repo)
	f.setScript("impl", implementPay+`; printf '\nOwner notes\n' >> `+yamlQuote(filepath.ToSlash(filepath.Join(dir, "wiki", "log.md"))))
	st := generatedRunState(t, f, planID)
	if st.Status != StatusAccepted || !strings.Contains(st.MemoryError, "uncommitted changes") {
		t.Fatalf("expected pending memory record: %+v", st)
	}
	pending, exists, err := LoadPending(f.repo, planID)
	if err != nil || !exists || pending.Implementation == nil || pending.Implementation.Approval != p.SpecDigest || pending.Implementation.Code != st.Head {
		t.Fatalf("pending implementation lost: %+v %v %v", pending, exists, err)
	}
	mustGit(t, dir, append(memoryGitIdentity, "commit", "-qam", "owner notes")...)
	if version, _, err := RecordPending(f.repo, planID); err != nil || version != "0.2.0" {
		t.Fatalf("pending implementation retry: %s %v", version, err)
	}
	im, err := readImplementations(filepath.Join(dir, filepath.FromSlash(implementedRel("pay"))))
	if err != nil || len(im) != 1 || !reflect.DeepEqual(im[0].Criteria, pending.Implementation.Criteria) {
		t.Fatalf("retried implementation: %+v %v", im, err)
	}
}

func TestGeneratedRunRejectsTamperedPinnedArtifacts(t *testing.T) {
	f, p := generatedFixture(t, implementPay)
	cfg, err := f.repo.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"seal", "outcome", "files"} {
		wt := filepath.Join(f.tmp, "tamper-"+kind)
		if err := addWorktreeDetached(f.dir, wt, p.PlanCommit); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "seal":
			b, _ := gitBlob(f.dir, p.PlanCommit, "aeh/seals/"+planID+".json")
			var s Seal
			if err := json.Unmarshal([]byte(b), &s); err != nil {
				t.Fatal(err)
			}
			s.Files[0].SHA256 = strings.Repeat("0", 64)
			if err := writeJSON(filepath.Join(wt, "aeh", "seals", planID+".json"), s); err != nil {
				t.Fatal(err)
			}
		case "outcome":
			b, _ := gitBlob(f.dir, p.PlanCommit, "aeh/outcomes/"+planID+".yaml")
			write(t, wt, "aeh/outcomes/"+planID+".yaml", strings.Replace(b, "feature: pay", "feature: other", 1))
		case "files":
			write(t, wt, planDir+"/pay.sh", "exit 0\n")
		}
		candidate, err := candidateCommit(wt, p.PlanCommit, "tamper")
		removeWorktree(f.dir, wt)
		if err != nil {
			t.Fatal(err)
		}
		fake := p
		fake.PlanCommit = candidate
		if _, _, err := loadGeneratedRun(f.repo, cfg, fake); err == nil {
			t.Fatalf("tampered pinned %s accepted", kind)
		}
	}
}

func TestGeneratedRunProtectsHistoricalCriteria(t *testing.T) {
	f, _ := generatedFixture(t, implementPay)
	first := generatedRunState(t, f, planID)
	if first.Status != StatusAccepted {
		t.Fatalf("initial run: %+v", first)
	}
	mustGit(t, f.dir, "merge", "--ff-only", IntegrationBranch(planID))
	editSpec(t, f.repo, "pay", "then src/pay.txt says paid.\n", "then src/pay.txt says paid.\n- AC-2: Given a payment, when it completes, then src/receipt.txt says paid.\n")
	if _, _, err := ApproveSpec(f.repo, "pay"); err != nil {
		t.Fatal(err)
	}
	const nextID = "pay-v0_2_0"
	author := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(authorOK, planID, nextID), "AC-1:", "AC-2:"), "src/pay.txt", "src/receipt.txt")
	f.setScript("author", author)
	p := f.plan()
	if p.ID != nextID || p.Phase != PlanLogged || len(p.Delta.Unchanged) != 1 {
		t.Fatalf("incremental plan: %+v", p)
	}
	f.setScript("impl", countCall+`echo paid > src/receipt.txt; echo 'exit 0' > `+planDir+`/pay.sh`)
	st := generatedRunState(t, f, nextID)
	if st.Status != StatusBlocked || st.Head != p.PlanCommit || st.Tasks[0].Attempts[0].FailedGate != "hash" {
		t.Fatalf("historical tamper accepted: %+v", st)
	}
	if err := Answer(f.repo, nextID, "Keep historical tests"); err != nil {
		t.Fatal(err)
	}
	f.setScript("impl", countCall+`echo paid > src/receipt.txt`)
	st = generatedRunState(t, f, nextID)
	if st.Status != StatusAccepted {
		t.Fatalf("incremental run: %+v", st)
	}
	im, err := readImplementations(filepath.Join(memDir(t, f.repo), filepath.FromSlash(implementedRel("pay"))))
	if err != nil || len(im) != 2 || len(im[1].Criteria) != 2 ||
		!reflect.DeepEqual(im[1].Criteria[0].Tests, im[0].Criteria[0].Tests) ||
		!reflect.DeepEqual(im[1].Criteria[1].Tests, []string{"tests/acceptance/" + nextID + "/pay.sh"}) {
		t.Fatalf("incremental implementation: %+v %v", im, err)
	}
}

func TestPlanCollisionFoldsExtensions(t *testing.T) {
	for _, file := range []string{"aeh/outcomes/PAY-v0_1_0.YAML", "aeh/outcomes/pay-v0_1_0.YmL", "aeh/seals/pay-v0_1_0.JSON", ".aeh/state/pay-v0_1_0.PLAN.JSON"} {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, file, "occupied")
			p := planner{repo: Repo{Root: dir}}
			if err := p.checkCollisions(planID); err == nil || !strings.Contains(err.Error(), "already used") {
				t.Fatalf("case collision missed: %v", err)
			}
		})
	}
}
