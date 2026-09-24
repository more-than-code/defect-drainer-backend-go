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
	"github.com/joe/defect-drainer-go/internal/store"
)

// AC9: GET /api/batch-jobs/{id} reports the computed simulator queue for a
// queued job, and that object is not stored in job.json.
func TestSimLeaseQueueOnGet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GROK_HOME", t.TempDir())
	data, defects := t.TempDir(), t.TempDir()
	release := filepath.Join(t.TempDir(), "release")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\nwhile [ ! -f '"+release+"' ]; do sleep 0.05; done\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", grok)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) })

	seedQueued := func(id, queuedAt string, wt string) {
		t.Helper()
		dir := filepath.Join(data, "batch-jobs", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		job := map[string]any{
			"jobId": id, "kind": "batch", "status": "queued", "mode": "grok",
			"app_id": store.SeededTutoredWebappAppID, "batchId": "BATCH-20260924-" + id,
			"queuedAt": queuedAt, "queuedMode": "fresh",
			"worktrees": []map[string]string{{
				"repo": "demo", "worktreeAbs": wt, "primaryAbs": t.TempDir(), "branch": "fix/x",
			}},
		}
		b, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Later name, earlier timestamp: ResumeQueue must follow queuedAt.
	seedQueued("bjob_zholder", "2026-09-01T00:00:00.000000000Z", t.TempDir())
	seedQueued("bjob_awaiter", "2026-09-02T00:00:00.000000000Z", t.TempDir())

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	// The holder has to reach its agent before GET, otherwise a fast failure
	// would hand the lease to the waiter and the queued view would be gone.
	deadline := time.Now().Add(15 * time.Second)
	for {
		holder := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_zholder", nil)
		if strings.Contains(holder.Body.String(), `"status":"running"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not start: %s", holder.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	res := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_awaiter", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET %d %s", res.Code, res.Body)
	}
	var body struct {
		Job struct {
			Status   string `json:"status"`
			SimQueue *struct {
				Position int    `json:"position"`
				Holder   string `json:"holder"`
			} `json:"simQueue"`
		} `json:"job"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Job.Status != "queued" || body.Job.SimQueue == nil || body.Job.SimQueue.Position != 1 || body.Job.SimQueue.Holder != "bjob_zholder" {
		t.Fatalf("queued view: %s", res.Body)
	}
	raw, err := os.ReadFile(filepath.Join(data, "batch-jobs", "bjob_awaiter", "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "simQueue") {
		t.Fatalf("simQueue was persisted: %s", raw)
	}
	holder := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_zholder", nil)
	if strings.Contains(holder.Body.String(), `"simQueue"`) {
		t.Fatalf("holder must not carry simQueue once it is no longer waiting: %s", holder.Body)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// "batch complete" is the last line persisted. Waiting for it on disk
	// means the driver goroutine has finished writing the handoff.
	deadline = time.Now().Add(20 * time.Second)
	for _, id := range []string{"bjob_zholder", "bjob_awaiter"} {
		for {
			raw, _ := os.ReadFile(filepath.Join(data, "batch-jobs", id, "job.json"))
			if strings.Contains(string(raw), "batch complete") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not finish: %s", id, raw)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
