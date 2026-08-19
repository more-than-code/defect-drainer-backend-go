package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestJobIDFallbackAndAlwaysWritesJobId(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{
  "id": "bjob_legacy",
  "status": "queued",
  "kind": "batch"
}`)
	if err := os.WriteFile(filepath.Join(dir, "job.json"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	j := readJob(filepath.Join(dir, "job.json"))
	if j == nil || j.ID != "bjob_legacy" {
		t.Fatalf("expected legacy id to load as jobId, got %+v", j)
	}
	j.Status = "completed"
	if err := writeJob(dir, j); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["jobId"] != "bjob_legacy" {
		t.Fatalf("jobId=%v", m["jobId"])
	}
	if _, ok := m["id"]; ok {
		t.Fatalf("legacy id key must not be rewritten: %s", raw)
	}
}

func TestJobExtraUnknownFieldsSurviveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	verification := map[string]any{
		"ran": true,
		"ok":  false,
		"results": []any{
			map[string]any{"repo": "web", "exit_code": float64(1)},
		},
	}
	src := map[string]any{
		"jobId":        "bjob_extra",
		"status":       "running",
		"kind":         "batch",
		"verification": verification,
		"baseline":     map[string]any{"ran": true, "ok": true},
		"diffHygiene":  map[string]any{"noisy": true, "repos": []any{}},
		"batchPath":    "sqlite:batches/BATCH-20260819-x",
		"repo_urls":    []any{"https://github.com/ex/repo"},
	}
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	j := readJob(filepath.Join(dir, "job.json"))
	if j == nil {
		t.Fatal("readJob nil")
	}
	j.Status = "completed"
	if err := writeJob(dir, j); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "completed" {
		t.Fatalf("status %v", got["status"])
	}
	for _, key := range []string{"batchPath", "repo_urls"} {
		if !reflect.DeepEqual(got[key], src[key]) {
			t.Fatalf("%s not value-identical:\nwant %#v\ngot  %#v", key, src[key], got[key])
		}
	}
	v, _ := got["verification"].(map[string]any)
	if v["ran"] != true {
		t.Fatalf("first-class verification lost: %#v", got["verification"])
	}
	bmap, _ := got["baseline"].(map[string]any)
	if bmap["ran"] != true {
		t.Fatalf("first-class baseline lost: %#v", got["baseline"])
	}
	dmap, _ := got["diffHygiene"].(map[string]any)
	if dmap["noisy"] != true {
		t.Fatalf("first-class diffHygiene lost: %#v", got["diffHygiene"])
	}
}

func TestJobLogCap(t *testing.T) {
	b := &BatchRunner{dataRoot: t.TempDir(), jobs: map[string]*Job{}}
	j := &Job{ID: "bjob_log"}
	b.jobs[j.ID] = j
	for i := 0; i < 4010; i++ {
		b.appendLog(j, fmt.Sprintf("line-%d", i), "info", "DefectDrainer")
	}
	if len(j.Log) != 4000 {
		t.Fatalf("len=%d want 4000", len(j.Log))
	}
	if j.Log[0] != "[info] [DefectDrainer] line-10" {
		t.Fatalf("first %q", j.Log[0])
	}
	if j.Log[3999] != "[info] [DefectDrainer] line-4009" {
		t.Fatalf("last %q", j.Log[3999])
	}
	raw, err := os.ReadFile(filepath.Join(b.dataRoot, "batch-jobs", j.ID, "job.json"))
	if err != nil {
		t.Fatalf("persisted job.json: %v", err)
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Log) != 4000 {
		t.Fatalf("persisted len=%d", len(got.Log))
	}
	if got.Log[0] != "[info] [DefectDrainer] line-10" || got.Log[3999] != "[info] [DefectDrainer] line-4009" {
		t.Fatalf("persisted first/last %q %q", got.Log[0], got.Log[3999])
	}
	if got.UpdatedAt == "" {
		t.Fatal("updatedAt empty on persisted job.json")
	}
}

func testStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	data := t.TempDir()
	defects := t.TempDir()
	sqlDB, err := db.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := store.EnsureSeededApps(sqlDB); err != nil {
		t.Fatal(err)
	}
	return store.NewStore(sqlDB, defects), data, defects
}

func TestHarvestResolvesWithImagesLeavesOpenWithout(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	fixed := "DEF-20260819-harvest-img-abcd"
	open := "DEF-20260819-harvest-none-ef01"
	for _, id := range []string{fixed, open} {
		if _, err := st.Write(store.DefectRecord{
			ID: id, AppID: store.SeededTutoredWebappAppID, Title: id,
			Severity: "P2", Status: "in_progress", Area: "other", Client: "web",
			Body: "## Acceptance\n\n- [ ] ship it\n", Bucket: "open",
			Summary: "s", Reported: "2026-08-19",
		}); err != nil {
			t.Fatal(err)
		}
	}
	job := &Job{
		ID: "bjob_harvest", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-h", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{fixed, open},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	imgDir := filepath.Join(handoff, "fix-evidence", fixed)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, ".hidden.png"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "notes.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestFixEvidence(job, handoff, nil)

	got, err := st.Get(fixed)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("fixed defect status %+v %v", got, err)
	}
	if len(got.FixEvidence) != 1 {
		t.Fatalf("FixEvidence should be exactly the one saved png, got %#v", got.FixEvidence)
	}
	if !strings.HasSuffix(got.FixEvidence[0], "fix-01.png") {
		t.Fatalf("expected fix-01.png, got %#v", got.FixEvidence)
	}
	still, err := st.Get(open)
	if err != nil || still == nil || still.Status != "in_progress" {
		t.Fatalf("open defect should stay in_progress, got %+v %v", still, err)
	}
	warn := false
	for _, line := range job.Log {
		if strings.Contains(line, "harvest "+open+": no fix-evidence/ or fix-notes/ — left open") {
			warn = true
		}
	}
	if !warn {
		t.Fatalf("missing left-open warning: %#v", job.Log)
	}

	noteMD := "DEF-20260819-harvest-notemd-1111"
	noteDir := "DEF-20260819-harvest-notesdir-2222"
	for _, id := range []string{noteMD, noteDir} {
		if _, err := st.Write(store.DefectRecord{
			ID: id, AppID: store.SeededTutoredWebappAppID, Title: id,
			Severity: "P2", Status: "in_progress", Area: "other", Client: "web",
			Body: "## Acceptance\n\n- [ ] ship it\n", Bucket: "open",
			Summary: "s", Reported: "2026-08-19",
		}); err != nil {
			t.Fatal(err)
		}
	}
	job2 := &Job{
		ID: "bjob_harvest_notes", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-n", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{noteMD, noteDir},
	}
	br.jobs[job2.ID] = job2
	handoff2 := filepath.Join(data, "batch-jobs", job2.ID)
	if err := os.MkdirAll(filepath.Join(handoff2, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff2, "fix-notes", noteMD+".md"), []byte("notes only"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(handoff2, "fix-evidence", noteDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff2, "fix-evidence", noteDir, "NOTES.md"), []byte("dir notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestFixEvidence(job2, handoff2, nil)
	for _, id := range []string{noteMD, noteDir} {
		got, err := st.Get(id)
		if err != nil || got == nil || got.Status != "in_progress" {
			t.Fatalf("notes-only %s should stay in_progress, got %+v %v", id, got, err)
		}
		found := false
		for _, line := range job2.Log {
			if strings.Contains(line, "harvest "+id+": notes only (no images) — still needs screenshots") {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing notes-only log for %s: %#v", id, job2.Log)
		}
	}
}

func TestBuildBatchFixBriefIncludesWorktreesAcceptanceContracts(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte("# repo rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := BuildBatchFixBrief(BatchFixBriefInput{
		Batch: BatchRecord{
			ID: "BATCH-1", AppID: "app_abc", Goal: "fix the button",
			Path: batchSQLitePath("BATCH-1"),
		},
		DefectsRoot: "/inv",
		Worktrees: []git.WorktreeBinding{{
			Repo: "demo", PrimaryAbs: "/primary/demo", WorktreeAbs: wt, Branch: "defect-drainer/BATCH-1",
		}},
		Defects: []store.DefectRecord{{
			ID: "DEF-1", Title: "t", Severity: "P1", Status: "in_progress",
			Summary: "sum", Body: "## Acceptance\n\n- [ ] one\n- [ ] two\n## Evidence\n\n- skip\n",
			Evidence: []string{"evidence/DEF-1/01.png"},
		}},
	})
	for _, want := range []string{
		"EDIT HERE",
		"DO NOT EDIT",
		"/primary/demo",
		wt,
		"acceptance criteria",
		"- [ ] one",
		"- [ ] two",
		filepath.Join(wt, "AGENTS.md"),
		"They bind you as much as this brief; where stricter, they win.",
		"evidence/DEF-1/01.png",
		"DEF-1",
		"sum",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "- [ ] skip") {
		t.Fatal("acceptance parser leaked the next section")
	}
}

// A sandboxed agent can name a path it cannot read and let the unsandboxed
// control plane read it on its behalf. Harvest must refuse every such link:
// a symlinked image, a symlinked note, and a symlinked evidence directory.
func TestHarvestRefusesSymlinkedEvidence(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	img := "DEF-20260819-symlink-img-0001"
	note := "DEF-20260819-symlink-note-0002"
	dir := "DEF-20260819-symlink-dir-0003"
	ids := []string{img, note, dir}
	for _, id := range ids {
		if _, err := st.Write(store.DefectRecord{
			ID: id, AppID: store.SeededTutoredWebappAppID, Title: id,
			Severity: "P2", Status: "in_progress", Area: "other", Client: "web",
			Body: "## Acceptance\n\n- [ ] ship it\n", Bucket: "open",
			Summary: "s", Reported: "2026-08-19",
		}); err != nil {
			t.Fatal(err)
		}
	}
	job := &Job{
		ID: "bjob_symlink", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-s", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: ids,
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)

	// 1. an image entry that is a symlink to a host secret
	imgDir := filepath.Join(handoff, "fix-evidence", img)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(imgDir, "fix-01.png")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// 2. a note file that is a symlink to the same secret
	notesRoot := filepath.Join(handoff, "fix-notes")
	if err := os.MkdirAll(notesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(notesRoot, note+".md")); err != nil {
		t.Fatal(err)
	}
	// 3. the whole per-defect evidence directory replaced by a symlink
	if err := os.Symlink(secretDir, filepath.Join(handoff, "fix-evidence", dir)); err != nil {
		t.Fatal(err)
	}

	br.harvestFixEvidence(job, handoff, nil)

	for _, id := range ids {
		got, err := st.Get(id)
		if err != nil || got == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("%s resolved through a symlink: status=%s", id, got.Status)
		}
		if len(got.FixEvidence) != 0 {
			t.Fatalf("%s harvested through a symlink: %v", id, got.FixEvidence)
		}
		if strings.Contains(got.Resolution, "PRIVATE KEY") {
			t.Fatalf("%s leaked secret bytes into resolution", id)
		}
	}
	// The evidence tree must not contain the secret under any name.
	_ = filepath.Walk(filepath.Join(data, "evidence"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("secret bytes copied into evidence at %s", p)
		}
		return nil
	})
}

// The DB is capped at one connection and harvest ends by taking b.mu to log, so
// any query issued while holding b.mu can deadlock against it. This pins the
// invariant: while CreatePRs is blocked on SQL, the mutex must be free.
func TestCreatePRsDoesNotHoldMutexAcrossSQL(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)

	gh := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_BIN", gh)

	job := &Job{
		ID: "bjob_lockorder", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-l", AppID: store.SeededTutoredWebappAppID,
		Worktrees: []git.WorktreeBinding{{
			Repo: "app", WorktreeAbs: t.TempDir(), Branch: "fix/x", PrimaryAbs: t.TempDir(),
		}},
	}
	br.jobs[job.ID] = job

	// Occupy the single connection so the next query blocks.
	tx, err := st.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback()
		}
	}
	defer release()

	created := make(chan struct{})
	go func() {
		_, _ = br.CreatePRs(job.ID)
		close(created)
	}()

	// Let CreatePRs get as far as its (blocked) query.
	time.Sleep(200 * time.Millisecond)

	free := make(chan struct{})
	go func() {
		br.mu.Lock()
		br.mu.Unlock() //nolint:staticcheck // probing lock availability only
		close(free)
	}()

	select {
	case <-free:
	case <-time.After(3 * time.Second):
		t.Fatal("b.mu held while CreatePRs waits on SQL — lock-order inversion with harvest")
	}

	release()
	select {
	case <-created:
	case <-time.After(10 * time.Second):
		t.Fatal("CreatePRs did not finish after the connection was released")
	}
}

func TestPRUnknownFieldsSurviveHydrateAndLogAppend(t *testing.T) {
	data := t.TempDir()
	jobID := "bjob_pr_extra"
	jobDir := filepath.Join(data, "batch-jobs", jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := map[string]any{
		"jobId":  jobID,
		"status": "completed",
		"kind":   "batch",
		"prs": []any{
			map[string]any{
				"repo":     "demo",
				"branch":   "fix/x",
				"ghNumber": float64(7),
				"mystery":  "keep-me",
			},
		},
	}
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	br := &BatchRunner{dataRoot: data, jobs: map[string]*Job{}}
	j := readJob(filepath.Join(jobDir, "job.json"))
	if j == nil {
		t.Fatal("readJob nil")
	}
	br.jobs[j.ID] = j
	br.appendLog(j, "hello", "info", "DefectDrainer")
	raw, err := os.ReadFile(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	prs, ok := got["prs"].([]any)
	if !ok || len(prs) == 0 {
		t.Fatalf("prs missing: %s", raw)
	}
	row, _ := prs[0].(map[string]any)
	if row["mystery"] != "keep-me" {
		t.Fatalf("unknown prs[] key dropped: %s", raw)
	}
	if row["branch"] != "fix/x" {
		t.Fatalf("branch lost: %s", raw)
	}
}

func TestGetJobDoesNotRaceWithHarvest(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	id := "DEF-20260819-race-img-aaaa"
	if _, err := st.Write(store.DefectRecord{
		ID: id, AppID: store.SeededTutoredWebappAppID, Title: id,
		Severity: "P2", Status: "in_progress", Area: "other", Client: "web",
		Body: "## Acceptance\n\n- [ ] ship it\n", Bucket: "open",
		Summary: "s", Reported: "2026-08-19",
	}); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: "bjob_race", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-r", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{id}, Extra: map[string]any{"batchPath": "sqlite:batches/x"},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	imgDir := filepath.Join(handoff, "fix-evidence", id)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		br.harvestFixEvidence(job, handoff, nil)
		for i := 0; i < 80; i++ {
			br.appendLog(job, fmt.Sprintf("extra-%d", i), "info", "DefectDrainer")
		}
	}()
	for {
		got := br.GetJob(job.ID)
		if got == nil {
			t.Fatal("GetJob nil")
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file never appeared: %s", path)
}

func runningJob(t *testing.T, br *BatchRunner, id, batchID, defID string) *Job {
	t.Helper()
	job := &Job{
		ID: id, Kind: "batch", Status: "running",
		BatchID: batchID, AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{defID},
		Worktrees: []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}},
	}
	br.jobs[job.ID] = job
	return job
}

func writeInProgressDefect(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.Write(store.DefectRecord{
		ID: id, AppID: store.SeededTutoredWebappAppID, Title: id,
		Severity: "P2", Status: "in_progress", Area: "other", Client: "web",
		Body: "## Acceptance\n\n- [ ] ship it\n", Bucket: "open",
		Summary: "s", Reported: "2026-08-19",
	}); err != nil {
		t.Fatal(err)
	}
}

// A hardlink to a host secret is a regular file and passes the symlink
// checks. Harvest must still refuse it on both the image and note paths.
func TestHarvestRefusesHardlinkedEvidence(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	img := "DEF-20260819-hardlink-img-0001"
	note := "DEF-20260819-hardlink-note-0002"
	ids := []string{img, note}
	for _, id := range ids {
		writeInProgressDefect(t, st, id)
	}
	job := &Job{
		ID: "bjob_hardlink", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-hl", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: ids,
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)

	imgDir := filepath.Join(handoff, "fix-evidence", img)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(imgDir, "fix-01.png")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	notesRoot := filepath.Join(handoff, "fix-notes")
	if err := os.MkdirAll(notesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(notesRoot, note+".md")); err != nil {
		t.Fatal(err)
	}

	br.harvestFixEvidence(job, handoff, nil)

	for _, id := range ids {
		got, err := st.Get(id)
		if err != nil || got == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("%s resolved through a hardlink: status=%s", id, got.Status)
		}
		if len(got.FixEvidence) != 0 {
			t.Fatalf("%s harvested through a hardlink: %v", id, got.FixEvidence)
		}
		if strings.Contains(got.Resolution, "PRIVATE KEY") {
			t.Fatalf("%s leaked secret bytes into resolution", id)
		}
	}
	_ = filepath.Walk(filepath.Join(data, "evidence"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("secret bytes copied into evidence at %s", p)
		}
		return nil
	})
}

func TestConcurrentJobJSONAppends(t *testing.T) {
	b := &BatchRunner{dataRoot: t.TempDir(), jobs: map[string]*Job{}}
	j := &Job{ID: "bjob_conc", Status: "running"}
	b.jobs[j.ID] = j
	const n = 80
	var appenders sync.WaitGroup
	appenders.Add(2)
	go func() {
		defer appenders.Done()
		for i := 0; i < n; i++ {
			b.appendLog(j, fmt.Sprintf("a-%d", i), "info", "DefectDrainer")
		}
	}()
	go func() {
		defer appenders.Done()
		for i := 0; i < n; i++ {
			b.appendLog(j, fmt.Sprintf("b-%d", i), "info", "DefectDrainer")
		}
	}()
	stopGet := make(chan struct{})
	var getter sync.WaitGroup
	getter.Add(1)
	go func() {
		defer getter.Done()
		for {
			select {
			case <-stopGet:
				return
			default:
				got := b.GetJob(j.ID)
				if got == nil {
					t.Error("GetJob nil")
					return
				}
				if _, err := json.Marshal(got); err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
			}
		}
	}()
	appenders.Wait()
	// Read job.json *before* a later persist can heal a lost update.
	raw, err := os.ReadFile(filepath.Join(b.dataRoot, "batch-jobs", j.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("job.json unparseable: %v\n%s", err, raw)
	}
	seen := map[string]bool{}
	for _, line := range got.Log {
		seen[line] = true
	}
	for i := 0; i < n; i++ {
		a := fmt.Sprintf("[info] [DefectDrainer] a-%d", i)
		bb := fmt.Sprintf("[info] [DefectDrainer] b-%d", i)
		if !seen[a] || !seen[bb] {
			t.Fatalf("missing line a-%d or b-%d (log len %d)", i, i, len(got.Log))
		}
	}
	b.mu.Lock()
	b.jobs[j.ID].Status = "cancelled"
	b.mu.Unlock()
	b.persistLive(j.ID)
	close(stopGet)
	getter.Wait()
	raw, err = os.ReadFile(filepath.Join(b.dataRoot, "batch-jobs", j.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("job.json unparseable after status write: %v", err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("status=%q want cancelled (last write)", got.Status)
	}
}

func TestStopDuringStartupStaysCancelled(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260819-stopstart-aaaa"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_stopstart", "BATCH-20260819-ss", defID)

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\necho spawned > \""+mark+"\"\nwhile true; do sleep 0.05; done\n")
	t.Setenv("GROK_BUILD_BIN", grok)

	entered := make(chan struct{})
	release := make(chan struct{})
	startSpawnHook = func() { close(entered); <-release }
	t.Cleanup(func() { startSpawnHook = nil })

	errc := make(chan error, 1)
	go func() { errc <- br.StartSpawn(job.ID, defects) }()
	<-entered
	stopped, err := br.Stop(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatalf("StartSpawn: %v", err)
	}
	if stopped == nil || stopped.Status != "cancelled" {
		t.Fatalf("stop returned %+v", stopped)
	}
	got := br.GetJob(job.ID)
	if got == nil || got.Status != "cancelled" {
		t.Fatalf("job status %+v", got)
	}
	br.mu.Lock()
	_, hasProc := br.procs[job.ID]
	br.mu.Unlock()
	if hasProc {
		t.Fatal("process handle installed after cancel")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("fake agent started after cancel")
	}
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status != "open" {
		t.Fatalf("defect should be reopened, got %+v %v", rec, err)
	}
}

func TestDeleteJobWhileRunning(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260819-delrun-bbbb"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_delrun", "BATCH-20260819-dr", defID)

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\necho spawned > \""+mark+"\"\nwhile true; do sleep 0.05; done\n")
	t.Setenv("GROK_BUILD_BIN", grok)

	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitFile(t, mark, 5*time.Second)
	if err := br.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("completion goroutine did not exit")
	}
	jobJSON := filepath.Join(data, "batch-jobs", job.ID, "job.json")
	if _, err := os.Stat(jobJSON); !os.IsNotExist(err) {
		t.Fatalf("job.json still present after delete: %v", err)
	}
	if br.GetJob(job.ID) != nil {
		t.Fatal("deleted job still in map")
	}
}

func TestDeleteJobDuringHarvest(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260819-delharv-cccc"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_delharv", "BATCH-20260819-dh", defID)

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, `#!/bin/sh
mkdir -p "fix-evidence/`+defID+`"
printf 'png' > "fix-evidence/`+defID+`/fix-01.png"
echo spawned > "`+mark+`"
exit 0
`)
	t.Setenv("GROK_BUILD_BIN", grok)

	entered := make(chan struct{})
	release := make(chan struct{})
	harvestStartHook = func() { close(entered); <-release }
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() {
		harvestStartHook = nil
		spawnExitHook = nil
	})

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := br.DeleteJob(job.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("completion goroutine did not exit")
	}
	jobJSON := filepath.Join(data, "batch-jobs", job.ID, "job.json")
	if _, err := os.Stat(jobJSON); !os.IsNotExist(err) {
		t.Fatalf("job.json recreated after delete-during-harvest: %v", err)
	}
	if br.GetJob(job.ID) != nil {
		t.Fatal("deleted job still in map")
	}
}

func TestAgentOutputByteCapKeepsJobJSONSane(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	job := runningJob(t, br, "bjob_cap", "BATCH-20260819-cap", "DEF-20260819-cap-dddd")
	job.DefectIDs = nil

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, `#!/bin/sh
dd if=/dev/zero bs=1024 count=1024 2>/dev/null | tr '\0' 'x'
echo spawned > "`+mark+`"
exit 0
`)
	t.Setenv("GROK_BUILD_BIN", grok)

	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not finish")
	}
	raw, err := os.ReadFile(filepath.Join(data, "batch-jobs", job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 2<<20 {
		t.Fatalf("job.json too large: %d", len(raw))
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("job.json: %v", err)
	}
	sawGrok := false
	for _, line := range got.Log {
		if strings.Contains(line, "[Grok]") {
			sawGrok = true
			if len(line) > 24_000+64 {
				t.Fatalf("unbounded grok line: %d bytes", len(line))
			}
		}
	}
	if !sawGrok {
		t.Fatalf("expected capped grok line in log, got %d lines", len(got.Log))
	}
}

func TestRefreshPRsKeepsUnknownFields(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	gh := filepath.Join(t.TempDir(), "fake-gh")
	writeExec(t, gh, `#!/bin/sh
case "$*" in
  *pr\ view*) echo '{"state":"MERGED","mergedAt":"2026-08-17T12:00:00Z","number":7,"url":"https://github.com/example/demo/pull/7"}'; exit 0 ;;
esac
exit 1
`)
	t.Setenv("GH_BIN", gh)
	job := &Job{
		ID: "bjob_pr_refresh", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-prf", AppID: store.SeededTutoredWebappAppID,
		PRs: []PR{{
			Repo: "demo", Branch: "fix/x",
			URL:    "https://github.com/example/demo/pull/7",
			Number: 7, Status: "created",
			Extra: map[string]any{"mystery": "keep-me"},
		}},
	}
	br.jobs[job.ID] = job
	got, err := br.RefreshPRs(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.PRs) == 0 || got.PRs[0].Extra["mystery"] != "keep-me" {
		t.Fatalf("extra dropped on refresh: %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(data, "batch-jobs", job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	prs, _ := disk["prs"].([]any)
	if len(prs) == 0 {
		t.Fatalf("prs missing: %s", raw)
	}
	row, _ := prs[0].(map[string]any)
	if row["mystery"] != "keep-me" {
		t.Fatalf("unknown prs[] key dropped on refresh: %s", raw)
	}
}

func TestUpdatedAtOnWireMatchesDisk(t *testing.T) {
	b := &BatchRunner{dataRoot: t.TempDir(), jobs: map[string]*Job{}}
	j := &Job{ID: "bjob_upd", Status: "running", UpdatedAt: "2000-01-01T00:00:00.000Z"}
	b.jobs[j.ID] = j
	b.appendLog(j, "hello", "info", "DefectDrainer")
	live := b.GetJob(j.ID)
	raw, err := os.ReadFile(filepath.Join(b.dataRoot, "batch-jobs", j.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk Job
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if live == nil || live.UpdatedAt == "" || live.UpdatedAt != disk.UpdatedAt {
		t.Fatalf("live updatedAt %q disk %q", live.UpdatedAt, disk.UpdatedAt)
	}
	if live.UpdatedAt == "2000-01-01T00:00:00.000Z" {
		t.Fatal("updatedAt not stamped on live job")
	}
}

func birthtimeAvailable(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	sys, _ := st.Sys().(*syscall.Stat_t)
	_, ok := fileBirthTime(f, sys)
	return ok
}

func assertHarvestRefusedSecret(t *testing.T, st *store.Store, data string, ids []string) {
	t.Helper()
	for _, id := range ids {
		got, err := st.Get(id)
		if err != nil || got == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("%s resolved through a pre-existing inode: status=%s", id, got.Status)
		}
		if len(got.FixEvidence) != 0 {
			t.Fatalf("%s harvested through a pre-existing inode: %v", id, got.FixEvidence)
		}
		if strings.Contains(got.Resolution, "PRIVATE KEY") {
			t.Fatalf("%s leaked secret bytes into resolution", id)
		}
	}
	_ = filepath.Walk(filepath.Join(data, "evidence"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), "PRIVATE KEY") {
			t.Fatalf("secret bytes copied into evidence at %s", p)
		}
		return nil
	})
}

func setupHarvestSecretJob(t *testing.T, st *store.Store, br *BatchRunner, jobID, batchID string, ids []string) (*Job, string, string) {
	t.Helper()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !birthtimeAvailable(t, secret) {
		t.Skip("birthtime unavailable on this platform/filesystem — H1 fails open")
	}
	for _, id := range ids {
		writeInProgressDefect(t, st, id)
	}
	job := &Job{
		ID: jobID, Kind: "batch", Status: "completed",
		BatchID: batchID, AppID: store.SeededTutoredWebappAppID,
		DefectIDs: ids,
		CreatedAt: time.Now().UTC().Add(2 * time.Second).Format("2006-01-02T15:04:05.000Z"),
	}
	br.jobs[job.ID] = job
	return job, filepath.Join(br.dataRoot, "batch-jobs", job.ID), secret
}

func TestHarvestRefusesRenamedPreexistingFile(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	img := "DEF-20260819-rename-img-0001"
	note := "DEF-20260819-rename-note-0002"
	job, handoff, secret := setupHarvestSecretJob(t, st, br, "bjob_rename", "BATCH-20260819-rn", []string{img, note})

	imgDir := filepath.Join(handoff, "fix-evidence", img)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(secret, filepath.Join(imgDir, "fix-01.png")); err != nil {
		t.Fatal(err)
	}
	notesRoot := filepath.Join(handoff, "fix-notes")
	if err := os.MkdirAll(notesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	noteSecret := filepath.Join(t.TempDir(), "note_secret")
	if err := os.WriteFile(noteSecret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(noteSecret, filepath.Join(notesRoot, note+".md")); err != nil {
		t.Fatal(err)
	}

	br.harvestFixEvidence(job, handoff, nil)
	assertHarvestRefusedSecret(t, st, data, []string{img, note})
}

func TestHarvestRefusesLinkUnlinkEvidence(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	img := "DEF-20260819-linkun-img-0001"
	note := "DEF-20260819-linkun-note-0002"
	job, handoff, secret := setupHarvestSecretJob(t, st, br, "bjob_linkun", "BATCH-20260819-lu", []string{img, note})

	imgDir := filepath.Join(handoff, "fix-evidence", img)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destImg := filepath.Join(imgDir, "fix-01.png")
	if err := os.Link(secret, destImg); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	notesRoot := filepath.Join(handoff, "fix-notes")
	if err := os.MkdirAll(notesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	noteSecret := filepath.Join(t.TempDir(), "note_secret")
	if err := os.WriteFile(noteSecret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	destNote := filepath.Join(notesRoot, note+".md")
	if err := os.Link(noteSecret, destNote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(noteSecret); err != nil {
		t.Fatal(err)
	}

	br.harvestFixEvidence(job, handoff, nil)
	assertHarvestRefusedSecret(t, st, data, []string{img, note})
}

func TestHarvestSkipsOversizedFile(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	img := "DEF-20260819-oversize-img-0001"
	note := "DEF-20260819-oversize-note-0002"
	writeInProgressDefect(t, st, img)
	writeInProgressDefect(t, st, note)
	job := &Job{
		ID: "bjob_oversize", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-os", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{img, note},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	imgDir := filepath.Join(handoff, "fix-evidence", img)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(imgDir, "fix-01.png")
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(harvestFileLimit + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	notesRoot := filepath.Join(handoff, "fix-notes")
	if err := os.MkdirAll(notesRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	nf, err := os.Create(filepath.Join(notesRoot, note+".md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := nf.Truncate(harvestFileLimit + 1); err != nil {
		t.Fatal(err)
	}
	if err := nf.Close(); err != nil {
		t.Fatal(err)
	}

	br.harvestFixEvidence(job, handoff, nil)

	gotImg, err := st.Get(img)
	if err != nil || gotImg == nil {
		t.Fatalf("img: %v", err)
	}
	if len(gotImg.FixEvidence) != 0 {
		t.Fatalf("oversized image persisted: %v", gotImg.FixEvidence)
	}
	gotNote, err := st.Get(note)
	if err != nil || gotNote == nil {
		t.Fatalf("note: %v", err)
	}
	if gotNote.Resolution != "" {
		t.Fatalf("oversized note persisted as resolution: %q", gotNote.Resolution)
	}
	live := br.GetJob(job.ID)
	if live == nil {
		t.Fatal("job gone")
	}
	logged := strings.Join(live.Log, "\n")
	if !strings.Contains(logged, "skip fix-01.png") {
		t.Fatalf("oversized image not logged: %s", logged)
	}
	if !strings.Contains(logged, "skip "+note+".md") {
		t.Fatalf("oversized note not logged: %s", logged)
	}
	_ = filepath.Walk(filepath.Join(data, "evidence"), func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if fi.Size() > harvestFileLimit {
			t.Fatalf("oversized bytes landed in evidence: %s size=%d", p, fi.Size())
		}
		return nil
	})
}

func TestHydrateDropsInvalidPRURL(t *testing.T) {
	data := t.TempDir()
	jobID := "bjob_pr_badurl"
	jobDir := filepath.Join(data, "batch-jobs", jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := map[string]any{
		"jobId":  jobID,
		"status": "completed",
		"kind":   "batch",
		"prs": []any{
			map[string]any{"repo": "demo", "url": "-evil", "ghNumber": float64(1)},
			map[string]any{"repo": "demo", "url": "http://example.com/pull/2", "ghNumber": float64(2)},
			map[string]any{"repo": "demo", "url": "https://github.com/example/demo/pull/3", "ghNumber": float64(3)},
		},
	}
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	j := readJob(filepath.Join(jobDir, "job.json"))
	if j == nil || len(j.PRs) != 3 {
		t.Fatalf("readJob %+v", j)
	}
	if j.PRs[0].URL != "" {
		t.Fatalf("leading-dash URL survived hydrate: %q", j.PRs[0].URL)
	}
	if j.PRs[1].URL != "" {
		t.Fatalf("http URL survived hydrate: %q", j.PRs[1].URL)
	}
	if j.PRs[2].URL != "https://github.com/example/demo/pull/3" {
		t.Fatalf("https URL dropped: %q", j.PRs[2].URL)
	}
}

func TestDeleteJobDuringSpawnBrief(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260819-delbrief-aaaa"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_delbrief", "BATCH-20260819-db", defID)

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\necho spawned > \""+mark+"\"\nwhile true; do sleep 0.05; done\n")
	t.Setenv("GROK_BUILD_BIN", grok)

	entered := make(chan struct{})
	release := make(chan struct{})
	startSpawnHook = func() { close(entered); <-release }
	t.Cleanup(func() { startSpawnHook = nil })

	errc := make(chan error, 1)
	go func() { errc <- br.StartSpawn(job.ID, defects) }()
	<-entered
	del := make(chan error, 1)
	go func() { del <- br.DeleteJob(job.ID) }()
	deadline := time.Now().Add(3 * time.Second)
	for br.GetJob(job.ID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("DeleteJob did not drop the map entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-del; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("StartSpawn: %v", err)
	}
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatalf("orphan handoff tree after delete-during-brief: %v", err)
	}
	if br.GetJob(job.ID) != nil {
		t.Fatal("deleted job still in map")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("fake agent started after delete-during-brief")
	}
}

func TestDeleteJobDuringSpawnAfterStart(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260819-delafter-bbbb"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_delafter", "BATCH-20260819-da", defID)

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\necho spawned > \""+mark+"\"\nwhile true; do sleep 0.05; done\n")
	t.Setenv("GROK_BUILD_BIN", grok)

	entered := make(chan struct{})
	release := make(chan struct{})
	startSpawnAfterStartHook = func() { close(entered); <-release }
	t.Cleanup(func() { startSpawnAfterStartHook = nil })

	errc := make(chan error, 1)
	go func() { errc <- br.StartSpawn(job.ID, defects) }()
	<-entered
	del := make(chan error, 1)
	go func() { del <- br.DeleteJob(job.ID) }()
	deadline := time.Now().Add(3 * time.Second)
	for br.GetJob(job.ID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("DeleteJob did not drop the map entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	if err := <-del; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("StartSpawn: %v", err)
	}
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Fatalf("orphan handoff tree after delete-after-start: %v", err)
	}
	if br.GetJob(job.ID) != nil {
		t.Fatal("deleted job still in map")
	}
	br.mu.Lock()
	_, hasProc := br.procs[job.ID]
	br.mu.Unlock()
	if hasProc {
		t.Fatal("untracked child left in procs after delete-after-start")
	}
}

func TestDeleteJobDropsPersistSlot(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	j := &Job{ID: "bjob_persistleak", Status: "running"}
	br.jobs[j.ID] = j
	if err := br.persistLive(j.ID); err != nil {
		t.Fatal(err)
	}
	br.persistMu.Lock()
	_, before := br.persist[j.ID]
	br.persistMu.Unlock()
	if !before {
		t.Fatal("expected persist slot after persistLive")
	}
	if err := br.DeleteJob(j.ID); err != nil {
		t.Fatal(err)
	}
	br.persistMu.Lock()
	_, after := br.persist[j.ID]
	br.persistMu.Unlock()
	if after {
		t.Fatal("persist slot leaked after DeleteJob")
	}
}

func TestPersistLiveReturnsWriteError(t *testing.T) {
	b := &BatchRunner{dataRoot: t.TempDir(), jobs: map[string]*Job{}}
	j := &Job{ID: "bjob_failpersist", Status: "running"}
	b.jobs[j.ID] = j
	jobsDir := filepath.Join(b.dataRoot, "batch-jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, j.ID), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.persistLive(j.ID); err == nil {
		t.Fatal("persistLive swallowed write error")
	}
}

func TestHarvestFailingVerdictDoesNotResolveEvenWithPNG(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	id := "DEF-20260819-verdict-png-0001"
	writeInProgressDefect(t, st, id)
	job := &Job{
		ID: "bjob_verdict", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-v", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{id},
		Baseline: &VerificationRun{
			Ran: true,
			Results: []VerifyResult{
				{Repo: "demo", Command: "true", Ok: true, ExitCode: intPtr(0)},
			},
		},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	imgDir := filepath.Join(handoff, "fix-evidence", id)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &VerificationRun{
		Ran: true,
		Results: []VerifyResult{
			{Repo: "demo", Command: "true", Ok: false, ExitCode: intPtr(1)},
		},
	}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{Verification: run})
	got, err := st.Get(id)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("failing verdict must not resolve, got %+v %v", got, err)
	}
	if len(got.FixEvidence) == 0 {
		t.Fatal("evidence should still be imported")
	}
	found := false
	for _, line := range job.Log {
		if strings.Contains(line, "NOT resolved") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing NOT resolved log: %#v", job.Log)
	}
}

func TestHarvestNilVerificationStillResolvesOnPNG(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	id := "DEF-20260819-nilver-png-0001"
	writeInProgressDefect(t, st, id)
	job := &Job{
		ID: "bjob_nilver", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260819-nv", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{id},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	imgDir := filepath.Join(handoff, "fix-evidence", id)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestFixEvidence(job, handoff, nil)
	got, err := st.Get(id)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("nil verification should still resolve on PNG, got %+v %v", got, err)
	}
}
