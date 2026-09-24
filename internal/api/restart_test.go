package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// seedStoppedJob writes a job.json the way a previous server left it, so
// Build hydrates it.
func seedStoppedJob(t *testing.T, data, id, mode, status string) {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{
		"jobId": id, "kind": "batch", "status": status, "mode": mode,
		"app_id": store.SeededTutoredWebappAppID, "batchId": "BATCH-20260923-api",
		"defect_ids": []string{"DEF-20260923-api-0001"},
		"worktrees":  []map[string]string{{"repo": "demo", "worktreeAbs": wt, "primaryAbs": t.TempDir(), "branch": "fix/x"}},
	}
	dir := filepath.Join(data, "batch-jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type restartJobJSON struct {
	Job struct {
		Status    string `json:"status"`
		Resumable *struct {
			Ok     bool   `json:"ok"`
			Reason string `json:"reason"`
		} `json:"resumable"`
		Rerunnable *struct {
			Ok bool `json:"ok"`
		} `json:"rerunnable"`
	} `json:"job"`
}

// AC7 + route mapping: reads expose the checks; resume/rerun answer
// 200 / 409 live / 400 with the rule's reason / 404.
func TestRestartRoutes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GROK_HOME", "")
	data, defects := t.TempDir(), t.TempDir()
	seedStoppedJob(t, data, "bjob_api", "grok", "failed")
	seedStoppedJob(t, data, "bjob_manual", "manual", "manual")
	seedStoppedJob(t, data, "bjob_nosession", "grok", "cancelled")
	root, err := git.GrokSessionsDir(filepath.Join(data, "batch-jobs", "bjob_api"))
	if err != nil {
		t.Fatal(err)
	}
	sess := filepath.Join(root, "01a0c8be-7e0e-7c71-92ad-cd85bf99e86f")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "chat_history.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "release")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\nwhile [ ! -f '"+release+"' ]; do sleep 0.05; done\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", grok)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	var got restartJobJSON
	res := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_api", nil)
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Job.Resumable == nil || !got.Job.Resumable.Ok || got.Job.Rerunnable == nil || !got.Job.Rerunnable.Ok {
		t.Fatalf("stopped job with a session should offer both: %s", res.Body)
	}
	if list := doJSON(t, app.Handler, http.MethodGet, "/api/batches", nil); !strings.Contains(list.Body.String(), `"resumable":{"ok":true}`) {
		t.Fatalf("list must carry the checks: %s", list.Body)
	}

	// AC7: the reason a read reports is the reason the action refuses with.
	// bjob_nosession has no grok session on disk.
	var blocked restartJobJSON
	res = doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_nosession", nil)
	if err := json.Unmarshal(res.Body.Bytes(), &blocked); err != nil || blocked.Job.Resumable == nil || blocked.Job.Resumable.Ok {
		t.Fatalf("job without a session must not be resumable: %s", res.Body)
	}
	refused := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_nosession/resume", nil)
	var refusedBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(refused.Body.Bytes(), &refusedBody)
	if refused.Code != http.StatusBadRequest || refusedBody.Error != blocked.Job.Resumable.Reason {
		t.Fatalf("POST refused with %d %q; GET reported %q", refused.Code, refusedBody.Error, blocked.Job.Resumable.Reason)
	}
	if bad := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/..%2F..%2Fetc/resume", nil); bad.Code == http.StatusOK {
		t.Fatalf("unsafe id accepted: %d", bad.Code)
	}

	manual := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_manual/resume", nil)
	if manual.Code != http.StatusBadRequest || !strings.Contains(manual.Body.String(), "not a coding-agent job") {
		t.Fatalf("manual resume: %d %s", manual.Code, manual.Body)
	}
	if nf := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_nope/resume", nil); nf.Code != http.StatusNotFound {
		t.Fatalf("unknown resume: %d %s", nf.Code, nf.Body)
	}

	started := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_api/resume", nil)
	if started.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", started.Code, started.Body)
	}
	if err := json.Unmarshal(started.Body.Bytes(), &got); err != nil || got.Job.Status != "running" {
		t.Fatalf("resume did not start the job: %s", started.Body)
	}
	for _, action := range []string{"rerun", "resume"} {
		again := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_api/"+action, nil)
		if again.Code != http.StatusConflict {
			t.Fatalf("%s on a live job: %d %s, want 409", action, again.Code, again.Body)
		}
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		res := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_api", nil)
		if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Job.Status != "running" && got.Job.Rerunnable != nil && got.Job.Rerunnable.Ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never settled: %s", res.Body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AC7 + the DELETE contract: the preview is read-only and maps 400/404;
// DELETE answers with the per-repo cleanup report.
func TestDeletePreviewAndCleanupRoutes(t *testing.T) {
	data, defects := t.TempDir(), t.TempDir()
	seedStoppedJob(t, data, "bjob_delapi", "grok", "failed")
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	if bad := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_bad!/delete-preview", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d %s", bad.Code, bad.Body)
	}
	if nf := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_nope/delete-preview", nil); nf.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d %s", nf.Code, nf.Body)
	}
	pre := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_delapi/delete-preview", nil)
	var preview struct {
		Repos []struct {
			Repo  string `json:"repo"`
			Error string `json:"error"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(pre.Body.Bytes(), &preview); err != nil || pre.Code != http.StatusOK || len(preview.Repos) != 1 {
		t.Fatalf("preview: %d %s", pre.Code, pre.Body)
	}
	// The seeded worktree lives outside the handoff, so the guard reports it.
	if !strings.Contains(preview.Repos[0].Error, "outside this job's handoff") {
		t.Fatalf("guard not reported: %s", pre.Body)
	}
	if still := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_delapi", nil); still.Code != http.StatusOK {
		t.Fatal("the preview must not delete anything")
	}

	del := doJSON(t, app.Handler, http.MethodDelete, "/api/batch-jobs/bjob_delapi", nil)
	var out struct {
		Ok      bool   `json:"ok"`
		JobID   string `json:"jobId"`
		Cleanup []struct {
			Repo string `json:"repo"`
		} `json:"cleanup"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &out); err != nil || del.Code != http.StatusOK || !out.Ok || out.JobID != "bjob_delapi" || len(out.Cleanup) != 1 {
		t.Fatalf("delete: %d %s", del.Code, del.Body)
	}
}
