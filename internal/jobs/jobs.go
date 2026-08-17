// Package jobs persists normalize + batch job.json files (TS JSON-on-disk).
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joe/defect-drainer-go/internal/analytics"
	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

// Job is a normalize or batch job record.
type Job struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind,omitempty"`
	Status      string         `json:"status"`
	Mode        string         `json:"mode,omitempty"`
	AppID       string         `json:"app_id,omitempty"`
	DefectID    string         `json:"defectId,omitempty"`
	BatchID     string         `json:"batchId,omitempty"`
	DefectIDs   []string       `json:"defect_ids,omitempty"`
	Repos       []string       `json:"repos,omitempty"`
	Error       string         `json:"error,omitempty"`
	HandoffPath string         `json:"handoffPath,omitempty"`
	CreatedAt   string         `json:"createdAt,omitempty"`
	UpdatedAt   string         `json:"updatedAt,omitempty"`
	Comment     string         `json:"comment,omitempty"`
	Reporter    string               `json:"reporter,omitempty"`
	PRs         []PR                 `json:"prs,omitempty"`
	Worktrees   []git.WorktreeBinding `json:"worktrees,omitempty"`
	Extra       map[string]any       `json:"-"`
}

// PR is a gh PR row on a batch job.
type PR struct {
	Repo      string `json:"repo,omitempty"`
	URL       string `json:"url,omitempty"`
	Number    int    `json:"number,omitempty"`
	Status    string `json:"status,omitempty"`
	GhState   string `json:"ghState,omitempty"`
	MergedAt  string `json:"mergedAt,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
	Base      string `json:"base,omitempty"`
}

// Runner holds in-memory normalize jobs + disk hydrate.
type Runner struct {
	mu       sync.Mutex
	store    *store.Store
	dataRoot string
	jobs     map[string]*Job
}

// NewRunner hydrates {DATA}/jobs (normalize) as stored — does not fail them.
func NewRunner(st *store.Store, dataRoot string) *Runner {
	r := &Runner{store: st, dataRoot: dataRoot, jobs: map[string]*Job{}}
	_ = os.MkdirAll(paths.JobsDir(dataRoot), 0o755)
	r.hydrateNormalize()
	return r
}

func (r *Runner) hydrateNormalize() {
	root := paths.JobsDir(r.dataRoot)
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if j := readJob(filepath.Join(root, e.Name(), "job.json")); j != nil {
				r.jobs[j.ID] = j
			}
		}
	}
}

func readJob(p string) *Job {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil
	}
	return &j
}

func writeJob(dir string, j *Job) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	j.UpdatedAt = db.NowIso()
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "job.json"), b, 0o644)
}

func newJobID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + strconv.FormatInt(time.Now().UnixMilli(), 36) + "_" + hex.EncodeToString(b[:])
}

// IntakeInput is local normalize input.
type IntakeInput struct {
	Comment  string
	Severity string
	Client   string
	Surface  string
	Area     string
	Source   string
	Reporter string
	AppID    string
	Mode     string
	Repos    []string
	Files    []struct {
		Filename string
		Data     []byte
	}
}

// EnqueueLocal writes handoff + defect and returns a completed job (202).
func (r *Runner) EnqueueLocal(in IntakeInput) (*Job, error) {
	if strings.TrimSpace(in.Comment) == "" && len(in.Files) == 0 {
		return nil, fmt.Errorf("comment or at least one image is required")
	}
	mode := in.Mode
	if mode != "local" && mode != "manual" && mode != "grok" {
		mode = "local"
	}
	if mode == "grok" {
		mode = "local" // Phase 1 / fallback
	}
	appID, err := store.ResolveAppID(r.store.DB, in.AppID)
	if err != nil {
		return nil, err
	}
	id := newJobID("job_")
	defectID := store.MakeDefectID(in.Comment, time.Now())
	ts := db.NowIso()
	dir := filepath.Join(paths.JobsDir(r.dataRoot), id)
	j := &Job{
		ID:          id,
		Kind:        "normalize",
		Status:      "completed",
		Mode:        mode,
		AppID:       appID,
		DefectID:    defectID,
		Repos:       in.Repos,
		HandoffPath: "jobs/" + id,
		CreatedAt:   ts,
		UpdatedAt:   ts,
		Comment:     in.Comment,
		Reporter:    in.Reporter,
	}
	if mode == "manual" {
		j.Status = "waiting_external"
	}
	if err := writeJob(dir, j); err != nil {
		return nil, err
	}
	if mode != "manual" {
		sev := in.Severity
		if sev == "" {
			sev = "P2"
		}
		client := in.Client
		if client == "" {
			app, _ := store.GetApp(r.store.DB, appID)
			client = store.ClientHintForApp(app)
		}
		source := in.Source
		if source == "" {
			if len(in.Files) > 0 {
				source = "screenshot+comment"
			} else {
				source = "comment-only"
			}
		}
		area := in.Area
		if area == "" {
			area = "other"
		}
		var evPaths []string
		if len(in.Files) > 0 {
			files := make([]struct {
				Filename string
				Data     []byte
			}, len(in.Files))
			copy(files, in.Files)
			evPaths, err = r.store.SaveEvidence(defectID, files, "report")
			if err != nil {
				return nil, err
			}
		}
		title := in.Comment
		if len(title) > 80 {
			title = title[:80]
		}
		_, err = r.store.Write(store.DefectRecord{
			ID:         defectID,
			AppID:      appID,
			Title:      title,
			Severity:   sev,
			Status:     "open",
			Area:       area,
			Client:     client,
			Surface:    in.Surface,
			Repos:      in.Repos,
			Labels:     []string{},
			Related:    []string{},
			Source:     source,
			Reporter:   in.Reporter, // omitted → ""
			KeyFiles:   []string{},
			Evidence:   evPaths,
			FixEvidence: []string{},
			Reported:   db.TodayUTC(),
			Summary:    in.Comment,
			Body:       in.Comment,
			Bucket:     "open",
		})
		if err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	r.jobs[id] = j
	r.mu.Unlock()
	return j, nil
}

// ListNormalize returns in-memory + hydrated jobs.
func (r *Runner) ListNormalize() []*Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	return out
}

// Get returns a normalize job by id (no SAFE_JOB_ID gate).
func (r *Runner) Get(id string) *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id]
}

// Complete marks waiting_external completed.
func (r *Runner) Complete(id string) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	j.Status = "completed"
	_ = writeJob(filepath.Join(paths.JobsDir(r.dataRoot), id), j)
	return j, nil
}

// Cancel marks cancelled.
func (r *Runner) Cancel(id string) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	j.Status = "cancelled"
	_ = writeJob(filepath.Join(paths.JobsDir(r.dataRoot), id), j)
	return j, nil
}

// BatchRecord is a batches table row.
type BatchRecord struct {
	ID        string   `json:"id"`
	AppID     string   `json:"app_id"`
	Title     string   `json:"title"`
	Goal      string   `json:"goal"`
	Status    string   `json:"status"`
	DefectIDs []string `json:"defect_ids"`
	Mode      string   `json:"mode,omitempty"`
	Created   string   `json:"created"`
	UpdatedAt string   `json:"updated_at"`
}

// BatchRunner persists batches + batch-jobs.
type BatchRunner struct {
	mu       sync.Mutex
	store    *store.Store
	dataRoot string
	jobs     map[string]*Job
	procs    map[string]*exec.Cmd
}

// NewBatchRunner hydrates batch-jobs; running/queued → failed + interrupted.
func NewBatchRunner(st *store.Store, dataRoot string) *BatchRunner {
	b := &BatchRunner{store: st, dataRoot: dataRoot, jobs: map[string]*Job{}, procs: map[string]*exec.Cmd{}}
	_ = os.MkdirAll(paths.BatchJobsDir(dataRoot), 0o755)
	b.hydrate()
	return b
}

func (b *BatchRunner) hydrate() {
	root := paths.BatchJobsDir(b.dataRoot)
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		j := readJob(filepath.Join(root, e.Name(), "job.json"))
		if j == nil {
			continue
		}
		if j.Status == "running" || j.Status == "queued" {
			j.Status = "failed"
			j.Error = "interrupted by backend restart"
			_ = writeJob(filepath.Join(root, e.Name()), j)
		}
		b.jobs[j.ID] = j
	}
}

func makeBatchID(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	y, m, d := now.Date()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("BATCH-%04d%02d%02d-%s", y, int(m), d, hex.EncodeToString(b[:]))
}

// CreateBatch applies the Phase-1 decision table (or worker path when spawn=true).
func (b *BatchRunner) CreateBatch(appID, title, goal, mode string, defectIDs []string, startFix, spawnWorker bool) (BatchRecord, *Job, error) {
	id, err := store.ResolveAppID(b.store.DB, appID)
	if err != nil {
		return BatchRecord{}, nil, err
	}
	if title == "" {
		title = "Batch"
	}
	if mode != "manual" && mode != "grok" {
		if startFix {
			mode = "grok"
		} else {
			mode = "manual"
		}
	}
	bid := makeBatchID(time.Now())
	ts := db.NowIso()
	batchStatus := "planned"
	jobStatus := "manual"
	jobErr := ""
	if startFix && mode == "grok" && !spawnWorker {
		batchStatus = "failed"
		jobStatus = "failed"
		jobErr = "coding agent worker not implemented"
	}
	if spawnWorker && startFix && mode == "grok" {
		batchStatus = "in_progress"
		jobStatus = "running"
	}
	_, err = b.store.DB.Exec(`INSERT INTO batches (
      id, app_id, title, goal, status, defect_ids_json, mode, created, updated_at
    ) VALUES (?,?,?,?,?,?,?,?,?)`,
		bid, id, title, goal, batchStatus, db.JSONArray(defectIDs), mode, ts, ts,
	)
	if err != nil {
		return BatchRecord{}, nil, err
	}
	jobID := newJobID("bjob_")
	j := &Job{
		ID:          jobID,
		Kind:        "batch",
		Status:      jobStatus,
		Mode:        mode,
		AppID:       id,
		BatchID:     bid,
		DefectIDs:   defectIDs,
		Error:       jobErr,
		HandoffPath: "batch-jobs/" + jobID,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}
	if err := writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID), j); err != nil {
		return BatchRecord{}, nil, err
	}
	promptKey := "batch_fix.manual.v1"
	if mode == "grok" {
		promptKey = "batch_fix.v1"
	}
	analytics.RecordPromptUse(b.store.DB, promptKey, "1", jobID, bid, id, analytics.JobStatusToPromptOutcome(jobStatus), "", "", defectIDs)
	analytics.UpsertJobSummary(b.store.DB, jobID, bid, id, jobStatus, mode, jobErr, ts, len(defectIDs), 0, 0, 0)
	b.mu.Lock()
	b.jobs[jobID] = j
	b.mu.Unlock()
	rec := BatchRecord{
		ID: bid, AppID: id, Title: title, Goal: goal, Status: batchStatus,
		DefectIDs: defectIDs, Mode: mode, Created: ts, UpdatedAt: ts,
	}
	return rec, j, nil
}

// GetBatch loads a batches row.
func (b *BatchRunner) GetBatch(id string) (*BatchRecord, error) {
	var rec BatchRecord
	var idsJSON string
	var mode sqlNull
	err := b.store.DB.QueryRow(`SELECT id, app_id, title, goal, status, defect_ids_json, mode, created, updated_at FROM batches WHERE id = ?`, id).
		Scan(&rec.ID, &rec.AppID, &rec.Title, &rec.Goal, &rec.Status, &idsJSON, &mode, &rec.Created, &rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	rec.DefectIDs = db.ParseJSONArray(idsJSON)
	rec.Mode = mode.s
	return &rec, nil
}

type sqlNull struct{ s string }

func (n *sqlNull) Scan(src any) error {
	if src == nil {
		n.s = ""
		return nil
	}
	switch v := src.(type) {
	case string:
		n.s = v
	case []byte:
		n.s = string(v)
	}
	return nil
}

// ListBatches returns all batches.
func (b *BatchRunner) ListBatches() ([]BatchRecord, error) {
	rows, err := b.store.DB.Query(`SELECT id, app_id, title, goal, status, defect_ids_json, mode, created, updated_at FROM batches ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchRecord
	for rows.Next() {
		var rec BatchRecord
		var idsJSON string
		var mode sqlNull
		if err := rows.Scan(&rec.ID, &rec.AppID, &rec.Title, &rec.Goal, &rec.Status, &idsJSON, &mode, &rec.Created, &rec.UpdatedAt); err != nil {
			return nil, err
		}
		rec.DefectIDs = db.ParseJSONArray(idsJSON)
		rec.Mode = mode.s
		out = append(out, rec)
	}
	if out == nil {
		out = []BatchRecord{}
	}
	return out, rows.Err()
}

// GetJob returns a batch job.
func (b *BatchRunner) GetJob(id string) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.jobs[id]
}

// DeleteJob removes a batch-job dir (SAFE_JOB_ID gated by handler).
func (b *BatchRunner) DeleteJob(id string) error {
	b.mu.Lock()
	delete(b.jobs, id)
	b.mu.Unlock()
	analytics.DeleteJobSummary(b.store.DB, id)
	return os.RemoveAll(filepath.Join(paths.BatchJobsDir(b.dataRoot), id))
}

// MarkFailed updates job+batch.
func (b *BatchRunner) MarkFailed(jobID, batchID, msg string) {
	b.mu.Lock()
	if j := b.jobs[jobID]; j != nil {
		j.Status = "failed"
		j.Error = msg
		_ = writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID), j)
	}
	b.mu.Unlock()
	_, _ = b.store.DB.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ?`, "failed", db.NowIso(), batchID)
}

// FlipInProgress marks open/triaged defects in_progress (TS create() start path).
func (b *BatchRunner) FlipInProgress(ids []string) {
	for _, id := range ids {
		rec, err := b.store.Get(id)
		if err != nil || rec == nil {
			continue
		}
		if rec.Status == "open" || rec.Status == "triaged" {
			rec.Status = "in_progress"
			_, _ = b.store.Write(*rec)
		}
	}
}

// SyncBatchAndDefects aligns batch + defects with the job outcome.
func (b *BatchRunner) SyncBatchAndDefects(job *Job, batchStatus string) {
	if job == nil {
		return
	}
	_, _ = b.store.DB.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ?`, batchStatus, db.NowIso(), job.BatchID)
	if batchStatus != "failed" && batchStatus != "cancelled" {
		return
	}
	for _, id := range job.DefectIDs {
		rec, err := b.store.Get(id)
		if err != nil || rec == nil {
			continue
		}
		if rec.Status == "in_progress" && len(rec.FixEvidence) == 0 {
			rec.Status = "open"
			rec.Bucket = "open"
			_, _ = b.store.Write(*rec)
		}
	}
}

// SetWorktrees persists bindings on the job (create-prs/refresh-prs need them).
func (b *BatchRunner) SetWorktrees(id string, wts []git.WorktreeBinding) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	j := b.jobs[id]
	if j == nil {
		return nil
	}
	j.Worktrees = wts
	_ = writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), id), j)
	return j
}

// UpdatePRs stores prs[] on the job.
func (b *BatchRunner) UpdatePRs(id string, prs []PR) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	j := b.jobs[id]
	if j == nil {
		return nil
	}
	j.PRs = prs
	_ = writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), id), j)
	return j
}

// StartSpawn execs the resolved coding-agent bin. Empty worktrees refuse spawn.
func (b *BatchRunner) StartSpawn(jobID, defectsRoot string) error {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return fmt.Errorf("job not found: %s", jobID)
	}
	if len(j.Worktrees) == 0 {
		b.mu.Unlock()
		return fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	handoff := filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID)
	b.mu.Unlock()

	cmd, err := git.StartCodingAgent(handoff, j.BatchID, defectsRoot, wts)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.procs[jobID] = cmd
	if live := b.jobs[jobID]; live != nil {
		live.Status = "running"
		live.Error = ""
		_ = writeJob(handoff, live)
	}
	b.mu.Unlock()
	go func() {
		err := cmd.Wait()
		b.mu.Lock()
		delete(b.procs, jobID)
		live := b.jobs[jobID]
		if live == nil || live.Status == "cancelled" {
			b.mu.Unlock()
			return
		}
		if err != nil {
			live.Status = "failed"
			live.Error = err.Error()
		} else {
			live.Status = "completed"
			live.Error = ""
		}
		_ = writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID), live)
		outcome := live.Status
		b.mu.Unlock()
		if outcome == "failed" {
			b.SyncBatchAndDefects(live, "failed")
		} else {
			b.SyncBatchAndDefects(live, "complete")
		}
	}()
	return nil
}

// Stop kills a live spawn (SIGTERM then SIGKILL).
func (b *BatchRunner) Stop(jobID string) (*Job, error) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	if j.Status != "running" && j.Status != "queued" {
		b.mu.Unlock()
		return nil, fmt.Errorf("cannot stop job in status %s (only running/queued)", j.Status)
	}
	cmd := b.procs[jobID]
	j.Status = "cancelled"
	j.Error = "stopped by operator"
	_ = writeJob(filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID), j)
	b.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		go func() {
			time.Sleep(3 * time.Second)
			_ = cmd.Process.Kill()
		}()
	}
	b.SyncBatchAndDefects(j, "cancelled")
	return j, nil
}

// CreatePRs runs gh pr create for each worktree and writes job.prs[].
func (b *BatchRunner) CreatePRs(jobID string) (*Job, error) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	if j.Status == "running" || j.Status == "queued" {
		b.mu.Unlock()
		return nil, fmt.Errorf("job is still running — stop it before creating PRs")
	}
	if len(j.Worktrees) == 0 {
		b.mu.Unlock()
		return nil, fmt.Errorf("no worktrees on job — cannot create PRs")
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	title := "defect-drainer " + j.BatchID
	if rec, err := b.GetBatch(j.BatchID); err == nil && rec != nil && rec.Title != "" {
		title = rec.Title
	}
	b.mu.Unlock()
	raw := git.CreatePRsForWorktrees(wts, j.BatchID, title)
	prs := make([]PR, 0, len(raw))
	for _, p := range raw {
		prs = append(prs, PR{
			Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
			GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		})
	}
	return b.UpdatePRs(jobID, prs), nil
}

// RefreshPRs runs gh pr view/list and writes job.prs[].
func (b *BatchRunner) RefreshPRs(jobID string) (*Job, error) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	if len(j.PRs) == 0 {
		b.mu.Unlock()
		return nil, fmt.Errorf("no PRs on job — create PRs first")
	}
	if !hasTrackable(j.PRs) {
		b.mu.Unlock()
		return nil, fmt.Errorf("no trackable PRs (all skipped or failed without URL)")
	}
	in := make([]git.PRResult, 0, len(j.PRs))
	for _, p := range j.PRs {
		in = append(in, git.PRResult{
			Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
			GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		})
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	b.mu.Unlock()
	raw := git.RefreshPRStatuses(in, wts)
	prs := make([]PR, 0, len(raw))
	for _, p := range raw {
		prs = append(prs, PR{
			Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
			GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		})
	}
	return b.UpdatePRs(jobID, prs), nil
}

func hasTrackable(prs []PR) bool {
	for _, p := range prs {
		if p.URL != "" || p.Status == "created" || p.Status == "existing" {
			return true
		}
	}
	return false
}
