package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/store"
)

func TestFixJobDoesNotInheritTestPassword(t *testing.T) {
	const secret = "S3cr3t-Value-42"
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", secret)
	t.Setenv("QA_PASSWORD", secret)
	st, data, defects := testStore(t)
	if _, err := st.DB.Exec(`UPDATE apps SET test_accounts_json = ? WHERE id = ?`,
		`[{"label":"QA","username":"ada","environment":"local","secret_ref":"env:QA_PASSWORD"}]`,
		store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	br := NewBatchRunner(st, data)
	defID := "DEF-20260925-fixenv-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_fixenv", "BATCH-20260925-fe", defID)
	proof := filepath.Join(t.TempDir(), "proof")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\n"+
		"if [ -z \"$DD_TEST_ACCOUNT_PASSWORD\" ]; then echo DD=UNSET; else echo DD=SET; fi > '"+proof+"'\n"+
		"if [ -z \"$QA_PASSWORD\" ]; then echo QA=UNSET; else echo QA=SET; fi >> '"+proof+"'\n"+
		"exit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	got, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DD=UNSET\nQA=UNSET\n" {
		t.Fatalf("fix job child saw a test secret:\n%s", got)
	}
	raw, err := os.ReadFile(filepath.Join(br.handoffDir(job.ID), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("secret leaked into job.json")
	}
}

func TestHarvestTestRunFiles(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "harvest", Title: "Harvest",
		Steps: []store.PlanStepIn{
			{Key: "login", Action: "sign in", Expected: "home"},
			{Key: "later", Action: "later", Expected: "ok"},
			{Key: "bad", Action: "bad", Expected: "ok"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(st.DB, store.CreateRunInput{
		AppID: store.SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_harvest_run", Kind: "test_run", Status: "running", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.secrets[job.ID] = "S3cr3t-Value-42"
	br.mu.Unlock()
	handoff := br.handoffDir(job.ID)
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(handoff, "evidence", "login"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(handoff, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("RUN.md", "summary\nNOT-RUN: later bench was down\nNOT-RUN: login should lose\n")
	write("results/login.md", "STATUS: fail\nsaw S3cr3t-Value-42\n")
	write("results/bad.md", "STATUS: nope\nignored\n")
	write("results/nope.md", "STATUS: pass\nunknown\n")
	write("results/x-poke.md", "STATUS: blocked\nACTION: poked around\nEXPECTED: nothing\nfound a drawer\n")
	write("evidence/login/shot.png", "png")
	br.harvestTestRun(job, handoff, true)

	got, err := store.GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "awaiting_review" {
		t.Fatalf("status %s", got.Status)
	}
	by := map[string]store.RunStep{}
	for _, s := range got.Steps {
		by[s.Key] = s
	}
	if by["login"].Status != "fail" || strings.Contains(by["login"].Actual, "S3cr3t-Value-42") {
		t.Fatalf("login %#v", by["login"])
	}
	if !strings.Contains(by["login"].Actual, "[redacted]") {
		t.Fatalf("login actual not redacted: %q", by["login"].Actual)
	}
	if len(by["login"].Evidence) != 1 {
		t.Fatalf("login evidence %#v", by["login"].Evidence)
	}
	if by["later"].Status != "blocked" || by["later"].Actual != "Not run: bench was down" {
		t.Fatalf("later %#v", by["later"])
	}
	if by["bad"].Status != "pending" {
		t.Fatalf("invalid STATUS should leave the step pending: %#v", by["bad"])
	}
	if _, ok := by["nope"]; ok {
		t.Fatal("unknown key created a step")
	}
	ex := by["x-poke"]
	if !ex.Exploratory || ex.Status != "blocked" || ex.Action != "poked around" || ex.Expected != "nothing" || !strings.Contains(ex.Actual, "drawer") {
		t.Fatalf("exploratory %#v", ex)
	}
	log := strings.Join(br.GetJob(job.ID).Log, "\n")
	if !strings.Contains(log, "results/bad.md") || !strings.Contains(log, "results/nope.md") {
		t.Fatalf("warn log missing: %s", log)
	}
	if strings.Contains(log, "S3cr3t-Value-42") {
		t.Fatal("secret leaked into the job log")
	}
}

func TestHarvestStaleScreenshotBecomesUnverified(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "stale", Title: "Stale",
		Steps: []store.PlanStepIn{{Key: "login", Action: "sign in", Expected: "home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(st.DB, store.CreateRunInput{
		AppID: store.SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_stale", Kind: "test_run", Status: "running", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.mu.Unlock()
	handoff := br.handoffDir(job.ID)
	img := filepath.Join(handoff, "evidence", "login", "old.png")
	if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(img, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !birthtimeAvailable(t, img) {
		t.Skip("birthtime unavailable")
	}
	job.CreatedAt = time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	time.Sleep(2 * time.Second)
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "results", "login.md"), []byte("STATUS: pass\nhome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "RUN.md"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestTestRun(job, handoff, true)
	got, err := store.GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "awaiting_review" {
		t.Fatalf("status %s", got.Status)
	}
	if len(got.Steps) != 1 || got.Steps[0].Status != "unverified" || len(got.Steps[0].Evidence) != 0 {
		t.Fatalf("step %#v", got.Steps)
	}
}
