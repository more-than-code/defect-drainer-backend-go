package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/store"
)

func waitRerunnableOK(t *testing.T, h http.Handler, jobID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		rec := doJSON(t, h, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
		if rec.Code != 200 {
			t.Fatalf("job get %d %s", rec.Code, rec.Body)
		}
		job := asMap(t, decodeBody(t, rec)["job"])
		rr, _ := job["rerunnable"].(map[string]any)
		if rr != nil {
			if ok, _ := rr["ok"].(bool); ok {
				return
			}
			last, _ = rr["reason"].(string)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rerunnable not ok: %s", last)
}

func waitRerunnableReason(t *testing.T, h http.Handler, jobID, reason string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		rec := doJSON(t, h, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
		if rec.Code != 200 {
			t.Fatalf("job get %d %s", rec.Code, rec.Body)
		}
		job := asMap(t, decodeBody(t, rec)["job"])
		rr, _ := job["rerunnable"].(map[string]any)
		if rr != nil {
			last, _ = rr["reason"].(string)
			if last == reason {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rerunnable reason %q, want %q", last, reason)
}

func TestAgentTestRunRerunFromAwaitingReview(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "once")
	release := filepath.Join(dir, "release")
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("x"), 0o644) })
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\n"+
		"if [ -f '"+mark+"' ]; then\n"+
		"  while [ ! -f '"+release+"' ]; do sleep 0.05; done\n"+
		"  mkdir -p results\n"+
		"  printf 'STATUS: pass\\nhome\\n' > results/login.md\n"+
		"  printf 'done\\n' > RUN.md\n"+
		"  exit 0\n"+
		"fi\n"+
		"echo 1 > '"+mark+"'\n"+
		"mkdir -p results evidence/login\n"+
		"printf 'STATUS: pass\\nhome\\n' > results/login.md\n"+
		"printf 'png' > evidence/login/shot.png\n"+
		"printf 'done\\n' > RUN.md\n"+
		"exit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	runID := asMap(t, body["run"])["id"].(string)
	jobID := asMap(t, body["job"])["jobId"].(string)
	waitJob(t, app.Handler, jobID, "completed")
	waitRerunnableOK(t, app.Handler, jobID)
	got := doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil)
	if asMap(t, decodeBody(t, got)["run"])["status"] != "awaiting_review" {
		t.Fatalf("before rerun %#v", decodeBody(t, got)["run"])
	}
	rerun := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+jobID+"/rerun", nil)
	if rerun.Code != 200 {
		t.Fatalf("rerun %d %s", rerun.Code, rerun.Body)
	}
	deadline := time.Now().Add(15 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		job := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil))["job"])
		run := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil))["run"])
		if job["status"] == "running" && run["status"] == "in_progress" {
			saw = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !saw {
		t.Fatal("rerun did not show job running and run in_progress together")
	}
	if err := os.WriteFile(release, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitJob(t, app.Handler, jobID, "completed")
	done := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil))["run"])
	if done["status"] != "awaiting_review" {
		t.Fatalf("after rerun %#v", done["status"])
	}
}

func TestAgentTestRunRerunSkipsOldScreenshot(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	mark := filepath.Join(t.TempDir(), "once")
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\n"+
		"if [ -f '"+mark+"' ]; then\n"+
		"  mkdir -p results\n"+
		"  printf 'STATUS: pass\\nhome\\n' > results/login.md\n"+
		"  printf 'done\\n' > RUN.md\n"+
		"  exit 0\n"+
		"fi\n"+
		"echo 1 > '"+mark+"'\n"+
		"mkdir -p results evidence/login\n"+
		"printf 'STATUS: pass\\nhome\\n' > results/login.md\n"+
		"printf 'png' > evidence/login/shot.png\n"+
		"printf 'done\\n' > RUN.md\n"+
		"exit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	runID := asMap(t, body["run"])["id"].(string)
	jobID := asMap(t, body["job"])["jobId"].(string)
	waitJob(t, app.Handler, jobID, "completed")
	waitRerunnableOK(t, app.Handler, jobID)
	first := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil))["run"])
	step := asMap(t, first["steps"].([]any)[0])
	if first["status"] != "awaiting_review" || step["status"] != "pass" || len(step["evidence"].([]any)) != 1 {
		t.Fatalf("first harvest %#v step %#v", first["status"], step)
	}
	rerun := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+jobID+"/rerun", nil)
	if rerun.Code != 200 {
		t.Fatalf("rerun %d %s", rerun.Code, rerun.Body)
	}
	waitJob(t, app.Handler, jobID, "completed")
	done := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil))["run"])
	step = asMap(t, done["steps"].([]any)[0])
	if done["status"] != "awaiting_review" || step["status"] != "unverified" || len(step["evidence"].([]any)) != 1 {
		t.Fatalf("rerun harvest status=%v step=%#v", done["status"], step)
	}
}

func TestAgentTestRunLeaseHandoverStartsWaiter(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	primary, _, _ := twoCommitRepo(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "mark")
	holderRelease := filepath.Join(dir, "holder-release")
	waiterRelease := filepath.Join(dir, "waiter-release")
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, map[string]any{"allow_simulator_writes": true})
	t.Cleanup(func() {
		_ = os.WriteFile(holderRelease, []byte("x"), 0o644)
		_ = os.WriteFile(waiterRelease, []byte("x"), 0o644)
	})
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\n"+
		"echo $$ >> '"+mark+"'\n"+
		"n=$(wc -l < '"+mark+"' | tr -d ' ')\n"+
		"if [ \"$n\" -le 1 ]; then\n"+
		"  while [ ! -f '"+holderRelease+"' ]; do sleep 0.05; done\n"+
		"  exit 0\n"+
		"fi\n"+
		"while [ ! -f '"+waiterRelease+"' ]; do sleep 0.05; done\n"+
		"exit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	first := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "title": "holder",
	})
	if first.Code != 201 {
		t.Fatalf("holder %d %s", first.Code, first.Body)
	}
	holderID := asMap(t, decodeBody(t, first)["job"])["jobId"].(string)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("holder never wrote its mark")
	}
	second := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "title": "waiter",
	})
	if second.Code != 201 {
		t.Fatalf("waiter %d %s", second.Code, second.Body)
	}
	secondBody := decodeBody(t, second)
	waiterID := asMap(t, secondBody["job"])["jobId"].(string)
	waiterRun := asMap(t, secondBody["run"])["id"].(string)
	if asMap(t, secondBody["job"])["status"] != "queued" {
		t.Fatalf("waiter not queued: %#v", secondBody["job"])
	}
	if err := os.WriteFile(holderRelease, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	saw := false
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+waiterID, nil))["job"])
		run := asMap(t, decodeBody(t, doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+waiterRun, nil))["run"])
		if job["status"] == "running" && run["status"] == "in_progress" {
			saw = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !saw {
		t.Fatal("lease handover did not start the waiter")
	}
	_ = os.WriteFile(waiterRelease, []byte("x"), 0o644)
	for _, id := range []string{holderID, waiterID} {
		stop := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+id+"/stop", nil)
		if stop.Code != 200 && stop.Code != 400 {
			t.Fatalf("stop %s %d %s", id, stop.Code, stop.Body)
		}
	}
	waitJob(t, app.Handler, holderID, "cancelled", "completed", "failed")
	waitJob(t, app.Handler, waiterID, "cancelled", "completed", "failed")
	time.Sleep(200 * time.Millisecond)
}

func TestReservedSecretRefRejectedOnSave(t *testing.T) {
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	for _, name := range []string{"PATH", "DD_PASSWORD"} {
		rec := doJSON(t, app.Handler, http.MethodPatch, "/api/apps/"+appID, map[string]any{
			"test_accounts": []any{map[string]any{
				"label": "QA", "username": "ada", "environment": "local", "secret_ref": "env:" + name,
			}},
		})
		want := "secret_ref env:" + name + " is reserved; use a dedicated variable such as QA_PASSWORD"
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestAbandonQueuedRun(t *testing.T) {
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "title": "queued", "execute": "manual",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	runID := asMap(t, decodeBody(t, rec)["run"])["id"].(string)
	if _, err := app.DB.Exec(`UPDATE test_runs SET status = 'queued' WHERE id = ?`, runID); err != nil {
		t.Fatal(err)
	}
	ab := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs/"+runID+"/abandon", nil)
	if ab.Code != 200 {
		t.Fatalf("abandon %d %s", ab.Code, ab.Body)
	}
	if asMap(t, decodeBody(t, ab)["run"])["status"] != "abandoned" {
		t.Fatalf("status %#v", decodeBody(t, ab)["run"])
	}
}
