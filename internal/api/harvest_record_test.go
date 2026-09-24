package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/store"
)

// A seeded job.json is agent-writable. The job read returns harvests with
// forged image paths removed; the batch list omits the field.
func TestHarvestRecordOnJobReadNotList(t *testing.T) {
	data, defects := t.TempDir(), t.TempDir()
	const (
		id  = "bjob_harvestrec"
		def = "DEF-20260924-api-0001"
	)
	ev := filepath.Join(defects, "evidence", def)
	if err := os.MkdirAll(ev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ev, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := "evidence/" + def + "/fix-01.png"
	forged := "evidence/" + def + "/../x/fix-01.png"
	job := map[string]any{
		"jobId": id, "kind": "batch", "status": "completed", "mode": "grok",
		"app_id": store.SeededTutoredWebappAppID, "batchId": "BATCH-20260924-api",
		"defect_ids": []string{def},
		"harvests": []map[string]any{{
			"at": "2026-09-24T00:00:00Z",
			"defects": []map[string]any{{
				"defectId": def,
				"outcome":  "resolved",
				"images":   []string{good, forged},
			}},
		}},
	}
	dir := filepath.Join(data, "batch-jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	res := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+id, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET %d %s", res.Code, res.Body)
	}
	var body struct {
		Job struct {
			Harvests []struct {
				At      string `json:"at"`
				Defects []struct {
					DefectID string   `json:"defectId"`
					Outcome  string   `json:"outcome"`
					Images   []string `json:"images"`
				} `json:"defects"`
			} `json:"harvests"`
		} `json:"job"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Job.Harvests) != 1 || body.Job.Harvests[0].At != "2026-09-24T00:00:00Z" {
		t.Fatalf("harvests %+v", body.Job.Harvests)
	}
	d := body.Job.Harvests[0].Defects
	if len(d) != 1 || d[0].DefectID != def || d[0].Outcome != "resolved" || len(d[0].Images) != 1 || d[0].Images[0] != good {
		t.Fatalf("defect %+v", d)
	}

	list := doJSON(t, app.Handler, http.MethodGet, "/api/batches", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list %d %s", list.Code, list.Body)
	}
	var listed struct {
		Jobs []struct {
			ID       string          `json:"jobId"`
			Harvests json.RawMessage `json:"harvests"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range listed.Jobs {
		if j.ID != id {
			continue
		}
		found = true
		if len(j.Harvests) != 0 {
			t.Fatalf("list included harvests: %s", j.Harvests)
		}
	}
	if !found {
		t.Fatalf("job missing from list: %s", list.Body)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), forged) {
		t.Fatalf("read rewrote job.json: %s", onDisk)
	}
}

// Action responses used to encode GetJob, so a forged harvest survived Stop,
// Restart, CreatePRs and RefreshPRs into the console's open job.
func TestHarvestRecordActionResponseIsSanitised(t *testing.T) {
	data, defects := t.TempDir(), t.TempDir()
	const (
		id  = "bjob_harvestact"
		def = "DEF-20260924-act-0001"
	)
	forged := "evidence/" + def + "/../x/fix-01.png"
	job := map[string]any{
		"jobId": id, "kind": "batch", "status": "completed", "mode": "manual",
		"app_id": store.SeededTutoredWebappAppID, "batchId": "BATCH-20260924-act",
		"defect_ids": []string{def},
		"prs": []map[string]any{{
			"repo": "demo", "kind": "merge", "status": "merged",
		}},
		"harvests": []map[string]any{{
			"at": "2026-09-24T00:00:00Z",
			"defects": []map[string]any{{
				"defectId": def, "outcome": "resolved", "images": []string{forged},
			}},
		}},
	}
	dir := filepath.Join(data, "batch-jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	res := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+id+"/refresh-prs", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("refresh-prs %d %s", res.Code, res.Body)
	}
	if strings.Contains(res.Body.String(), forged) || strings.Contains(res.Body.String(), "/../") {
		t.Fatalf("unsanitised harvests in refresh-prs: %s", res.Body)
	}
}
