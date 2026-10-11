package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/store"
)

func TestHarvestRedactsSecretBeforeNewlineAndHeaders(t *testing.T) {
	const secret = "SEC\r\nRET-99"
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "redact-raw", Title: "Redact",
		Steps: []store.PlanStepIn{{Key: "later", Action: "later", Expected: "ok"}},
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
		ID: "bjob_rawredact", Kind: "test_run", Status: "running", Mode: "grok",
		AppID: store.SeededTutoredWebappAppID, TestRunID: run.ID,
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	}
	br.mu.Lock()
	br.jobs[job.ID] = job
	br.secrets[job.ID] = secret
	br.mu.Unlock()
	handoff := br.handoffDir(job.ID)
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "STATUS: blocked\r\nACTION: do " + secret + "\r\nEXPECTED: see " + secret + "\r\nactual " + secret + "\r\n"
	if err := os.WriteFile(filepath.Join(handoff, "results", "x-poke.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "RUN.md"), []byte("NOT-RUN: later "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestTestRun(job, handoff, false)
	got, err := store.GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Steps {
		blob := s.Action + "\n" + s.Expected + "\n" + s.Actual
		if strings.Contains(blob, "SEC") || strings.Contains(blob, "RET-99") {
			t.Fatalf("secret leaked into step %s action=%q expected=%q actual=%q", s.Key, s.Action, s.Expected, s.Actual)
		}
	}
}

func TestCheckAccountSecretRejectsMarkerSubstring(t *testing.T) {
	t.Setenv("QA_PASSWORD", "redacted")
	err := CheckAccountSecret(map[string]any{
		"account": map[string]any{"secret_ref": "env:QA_PASSWORD"},
	})
	if err == nil || err.Error() != "test account secret env QA_PASSWORD cannot be redacted safely" {
		t.Fatalf("got %v", err)
	}
}

func TestCheckAccountSecretRejectsMarkerSuffix(t *testing.T) {
	t.Setenv("QA_PASSWORD", "acted]ZZ")
	err := CheckAccountSecret(map[string]any{
		"account": map[string]any{"secret_ref": "env:QA_PASSWORD"},
	})
	if err == nil || err.Error() != "test account secret env QA_PASSWORD cannot be redacted safely" {
		t.Fatalf("got %v", err)
	}
}
