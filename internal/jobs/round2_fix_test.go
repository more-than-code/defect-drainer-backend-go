package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestVerifyCommandsDoNotSeeTestSecrets(t *testing.T) {
	const secret = "S3cr3t-Value-42"
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", secret)
	t.Setenv("QA_PASSWORD", secret)
	st, data, defects := testStore(t)
	commands, err := json.Marshal([]store.VerifyCommand{{
		Repo:    "demo",
		Command: `printf 'QA=%s\nDD=%s\n' "$QA_PASSWORD" "$DD_TEST_ACCOUNT_PASSWORD"`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE apps SET test_accounts_json = ?, verify_commands_json = ? WHERE id = ?`,
		`[{"label":"QA","username":"ada","environment":"local","secret_ref":"env:QA_PASSWORD"}]`,
		string(commands), store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	br := NewBatchRunner(st, data)
	defID := "DEF-20260925-verifyenv-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_verifyenv", "BATCH-20260925-ve", defID)
	job.Mode = "grok"
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	got := br.GetJob(job.ID)
	if got == nil || got.Baseline == nil || got.Verification == nil {
		t.Fatalf("baseline/verification missing: %+v", got)
	}
	for _, label := range []string{"baseline", "verification"} {
		var run *VerificationRun
		if label == "baseline" {
			run = got.Baseline
		} else {
			run = got.Verification
		}
		if len(run.Results) != 1 {
			t.Fatalf("%s results %+v", label, run.Results)
		}
		tail := run.Results[0].OutputTail
		if strings.Contains(tail, secret) {
			t.Fatalf("%s printed the secret: %q", label, tail)
		}
		if !strings.Contains(tail, "QA=") || !strings.Contains(tail, "DD=") {
			t.Fatalf("%s did not run: %q", label, tail)
		}
	}
}

func TestToolchainProbeDropsTestSecrets(t *testing.T) {
	const secret = "S3cr3t-Value-42"
	t.Setenv("QA_PASSWORD", secret)
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", secret)
	home := t.TempDir()
	t.Setenv("FVM_HOME", home)
	sdkBin := filepath.Join(home, "versions", "3.24.0", "bin")
	if err := os.MkdirAll(sdkBin, 0o755); err != nil {
		t.Fatal(err)
	}
	proof := filepath.Join(t.TempDir(), "proof")
	script := "#!/bin/sh\n" +
		"if [ -z \"$QA_PASSWORD\" ]; then echo QA=UNSET; else echo QA=$QA_PASSWORD; fi > '" + proof + "'\n" +
		"if [ -z \"$DD_TEST_ACCOUNT_PASSWORD\" ]; then echo DD=UNSET; else echo DD=$DD_TEST_ACCOUNT_PASSWORD; fi >> '" + proof + "'\n" +
		"echo Flutter\n"
	if err := os.WriteFile(filepath.Join(sdkBin, "flutter"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	writeFvmrc(t, wt, "3.24.0")
	got := provisionFlutterToolchain(t.TempDir(), []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt}}, func(string, string) {}, []string{"QA_PASSWORD"})
	if !got.Ok {
		t.Fatal("probe should succeed")
	}
	raw, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "QA=UNSET") || !strings.Contains(string(raw), "DD=UNSET") {
		t.Fatalf("probe env:\n%s", raw)
	}
}

func TestSecretEnvNamesFailsClosed(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	if err := st.DB.Close(); err != nil {
		t.Fatal(err)
	}
	names, err := br.secretEnvNames()
	if err == nil {
		t.Fatalf("closed database returned names %v", names)
	}
}

func TestReservedSecretRefRefusesSpawn(t *testing.T) {
	st, data, defects := testStore(t)
	if _, err := st.DB.Exec(`UPDATE apps SET test_accounts_json = ? WHERE id = ?`,
		`[{"label":"QA","username":"ada","environment":"local","secret_ref":"env:PATH"}]`,
		store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	br := NewBatchRunner(st, data)
	defID := "DEF-20260925-reserved-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_reserved", "BATCH-20260925-rs", defID)
	job.Mode = "grok"
	t.Setenv("GROK_BUILD_BIN", filepath.Join(t.TempDir(), "missing-grok"))
	err := br.StartSpawn(job.ID, defects)
	want := "secret_ref env:PATH is reserved; use a dedicated variable such as QA_PASSWORD"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v", err)
	}
}

func TestFailedTestRestartKeepsDiffHygiene(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	run, err := store.CreateRun(st.DB, store.CreateRunInput{
		AppID: store.SeededTutoredWebappAppID, Execute: "manual", Title: "keep",
	})
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_keep_hygiene", Kind: "test_run", Status: "completed", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		Worktrees: []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir()}},
		DiffHygiene: &DiffHygieneReport{Repos: []RepoDiffHygiene{{
			Repo: "demo", FilesChanged: 3, ChangedLines: 9,
		}}},
	}
	if _, err := st.DB.Exec(`UPDATE test_runs SET status = 'awaiting_review', job_id = ? WHERE id = ?`, job.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.mu.Unlock()
	t.Setenv("GROK_BUILD_BIN", filepath.Join(t.TempDir(), "missing-grok"))
	_, err = br.Restart(job.ID, defects, false)
	if err == nil {
		t.Fatal("missing agent should fail the restart")
	}
	got := br.GetJob(job.ID)
	if got == nil || got.Status != "failed" || got.DiffHygiene == nil || len(got.DiffHygiene.Repos) != 1 || got.DiffHygiene.Repos[0].FilesChanged != 3 {
		t.Fatalf("hygiene discarded: status=%v hygiene=%+v err=%v", got.Status, got.DiffHygiene, err)
	}
}

func TestRestartStoreConflictIsConflictError(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	run, err := store.CreateRun(st.DB, store.CreateRunInput{
		AppID: store.SeededTutoredWebappAppID, Execute: "manual", Title: "conflict",
	})
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_conflict", Kind: "test_run", Status: "completed", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		Worktrees: []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir()}},
	}
	if _, err := st.DB.Exec(`UPDATE test_runs SET status = 'awaiting_review', job_id = ? WHERE id = ?`, job.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.mu.Unlock()
	startSpawnHook = func() {
		_, _ = st.DB.Exec(`UPDATE test_runs SET status = 'reviewed' WHERE id = ?`, run.ID)
	}
	t.Cleanup(func() { startSpawnHook = nil })
	t.Setenv("GROK_BUILD_BIN", filepath.Join(t.TempDir(), "missing-grok"))
	_, err = br.Restart(job.ID, defects, false)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want conflict (HTTP 409), got %v", err)
	}
}

func TestHarvestImportsTwelveImagesInNameOrder(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "shots", Title: "Shots",
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
	if _, err := st.DB.Exec(`UPDATE test_runs SET status = 'in_progress' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_images", Kind: "test_run", Status: "running", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.mu.Unlock()
	handoff := br.handoffDir(job.ID)
	imgDir := filepath.Join(handoff, "evidence", "login")
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil || os.MkdirAll(imgDir, 0o755) != nil {
		t.Fatal(err)
	}
	names := make([]string, 13)
	bodies := map[string]string{}
	for i := 0; i < 13; i++ {
		name := string(rune('a'+i)) + ".png"
		names[i] = name
		body := "img-" + name
		bodies[name] = body
		if err := os.WriteFile(filepath.Join(imgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(handoff, "results", "login.md"), []byte("STATUS: pass\nhome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestTestRun(job, handoff, false)
	got, err := store.GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var step store.RunStep
	for _, s := range got.Steps {
		if s.Key == "login" {
			step = s
		}
	}
	if len(step.Evidence) != 12 {
		t.Fatalf("imported %d, want 12 in name order", len(step.Evidence))
	}
	for i, rel := range step.Evidence {
		raw, err := os.ReadFile(filepath.Join(defects, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != bodies[names[i]] {
			t.Fatalf("evidence %d = %q, want %s", i, raw, names[i])
		}
	}
}

func TestHarvestCutsActualOnRuneBoundary(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "runes", Title: "Runes",
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
	if _, err := st.DB.Exec(`UPDATE test_runs SET status = 'in_progress' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_runes", Kind: "test_run", Status: "running", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.mu.Unlock()
	handoff := br.handoffDir(job.ID)
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	actual := strings.Repeat("a", 3999) + "你" + "b"
	body := "STATUS: fail\n" + actual + "\n"
	if utf8.RuneCountInString(actual) != 4001 {
		t.Fatalf("fixture runes %d", utf8.RuneCountInString(actual))
	}
	if err := os.WriteFile(filepath.Join(handoff, "results", "login.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestTestRun(job, handoff, false)
	got, err := store.GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	for _, s := range got.Steps {
		if s.Key == "login" {
			stored = s.Actual
		}
	}
	if !utf8.ValidString(stored) || utf8.RuneCountInString(stored) != 4000 || !strings.HasSuffix(stored, "你") {
		t.Fatalf("cut actual runes=%d valid=%v suffix=%q", utf8.RuneCountInString(stored), utf8.ValidString(stored), tail(stored))
	}
}

func tail(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
}
