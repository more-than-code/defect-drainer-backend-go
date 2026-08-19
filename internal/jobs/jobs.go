// Package jobs persists normalize + batch job.json files (TS JSON-on-disk).
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joe/defect-drainer-go/internal/analytics"
	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

// Job is a normalize or batch job record.
type Job struct {
	ID           string                `json:"jobId"`
	Kind         string                `json:"kind,omitempty"`
	Status       string                `json:"status"`
	Mode         string                `json:"mode,omitempty"`
	AppID        string                `json:"app_id,omitempty"`
	DefectID     string                `json:"defectId,omitempty"`
	BatchID      string                `json:"batchId,omitempty"`
	DefectIDs    []string              `json:"defect_ids,omitempty"`
	Repos        []string              `json:"repos,omitempty"`
	Error        string                `json:"error,omitempty"`
	HandoffPath  string                `json:"handoffPath,omitempty"`
	CreatedAt    string                `json:"createdAt,omitempty"`
	UpdatedAt    string                `json:"updatedAt,omitempty"`
	Comment      string                `json:"comment,omitempty"`
	Reporter     string                `json:"reporter,omitempty"`
	PRs          []PR                  `json:"prs,omitempty"`
	Worktrees    []git.WorktreeBinding `json:"worktrees,omitempty"`
	Verification *VerificationRun      `json:"verification,omitempty"`
	Baseline     *VerificationRun      `json:"baseline,omitempty"`
	DiffHygiene  *DiffHygieneReport    `json:"diffHygiene,omitempty"`
	Log          []string              `json:"log,omitempty"`
	Source       string                `json:"source,omitempty"`
	DefectPath   string                `json:"defectPath,omitempty"`
	Severity     string                `json:"severity,omitempty"`
	Client       string                `json:"client,omitempty"`
	Surface      string                `json:"surface,omitempty"`
	Extra        map[string]any        `json:"-"`
}

// jobOwnedJSON is every key the Job struct serializes (plus legacy "id").
var jobOwnedJSON = map[string]bool{
	"jobId": true, "id": true,
	"kind": true, "status": true, "mode": true, "app_id": true,
	"defectId": true, "batchId": true, "defect_ids": true, "repos": true,
	"error": true, "handoffPath": true, "createdAt": true, "updatedAt": true,
	"comment": true, "reporter": true, "prs": true, "worktrees": true,
	"verification": true, "baseline": true, "diffHygiene": true,
	"log": true, "source": true, "defectPath": true,
	"severity": true, "client": true, "surface": true,
}

func rawString(raw map[string]any, key string) (string, bool) {
	v, ok := raw[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// UnmarshalJSON accepts jobId (current) or id (older Go builds) and stashes
// unknown keys in Extra so a later write is lossless.
func (j *Job) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := rawString(raw, "jobId"); !ok {
		if id, ok := rawString(raw, "id"); ok {
			raw["jobId"] = id
		}
	}
	type wire Job
	reb, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var w wire
	if err := json.Unmarshal(reb, &w); err != nil {
		return err
	}
	*j = Job(w)
	extra := map[string]any{}
	for k, v := range raw {
		if !jobOwnedJSON[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		j.Extra = extra
	}
	return nil
}

// MarshalJSON always writes jobId (never id). Struct fields win over Extra.
func (j Job) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID           string                `json:"jobId"`
		Kind         string                `json:"kind,omitempty"`
		Status       string                `json:"status"`
		Mode         string                `json:"mode,omitempty"`
		AppID        string                `json:"app_id,omitempty"`
		DefectID     string                `json:"defectId,omitempty"`
		BatchID      string                `json:"batchId,omitempty"`
		DefectIDs    []string              `json:"defect_ids,omitempty"`
		Repos        []string              `json:"repos,omitempty"`
		Error        string                `json:"error,omitempty"`
		HandoffPath  string                `json:"handoffPath,omitempty"`
		CreatedAt    string                `json:"createdAt,omitempty"`
		UpdatedAt    string                `json:"updatedAt,omitempty"`
		Comment      string                `json:"comment,omitempty"`
		Reporter     string                `json:"reporter,omitempty"`
		PRs          []PR                  `json:"prs,omitempty"`
		Worktrees    []git.WorktreeBinding `json:"worktrees,omitempty"`
		Verification *VerificationRun      `json:"verification,omitempty"`
		Baseline     *VerificationRun      `json:"baseline,omitempty"`
		DiffHygiene  *DiffHygieneReport    `json:"diffHygiene,omitempty"`
		Log          []string              `json:"log,omitempty"`
		Source       string                `json:"source,omitempty"`
		DefectPath   string                `json:"defectPath,omitempty"`
		Severity     string                `json:"severity,omitempty"`
		Client       string                `json:"client,omitempty"`
		Surface      string                `json:"surface,omitempty"`
	}
	b, err := json.Marshal(wire{
		ID: j.ID, Kind: j.Kind, Status: j.Status, Mode: j.Mode, AppID: j.AppID,
		DefectID: j.DefectID, BatchID: j.BatchID, DefectIDs: j.DefectIDs,
		Repos: j.Repos, Error: j.Error, HandoffPath: j.HandoffPath,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, Comment: j.Comment,
		Reporter: j.Reporter, PRs: j.PRs, Worktrees: j.Worktrees,
		Verification: j.Verification, Baseline: j.Baseline, DiffHygiene: j.DiffHygiene,
		Log: j.Log, Source: j.Source, DefectPath: j.DefectPath, Severity: j.Severity,
		Client: j.Client, Surface: j.Surface,
	})
	if err != nil {
		return nil, err
	}
	if len(j.Extra) == 0 {
		return b, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range j.Extra {
		if jobOwnedJSON[k] {
			continue
		}
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// PR is a gh PR row on a batch job. Field names match console BatchJobPr.
type PR struct {
	Repo      string         `json:"repo,omitempty"`
	URL       string         `json:"url,omitempty"`
	Number    int            `json:"ghNumber,omitempty"`
	Status    string         `json:"status,omitempty"`
	GhState   string         `json:"ghState,omitempty"`
	MergedAt  string         `json:"mergedAt,omitempty"`
	CheckedAt string         `json:"checkedAt,omitempty"`
	Base      string         `json:"base,omitempty"`
	Branch    string         `json:"branch,omitempty"`
	Error     string         `json:"error,omitempty"`
	Commits   int            `json:"commits,omitempty"`
	GhError   string         `json:"ghError,omitempty"`
	Extra     map[string]any `json:"-"`
}

var prOwnedJSON = map[string]bool{
	"repo": true, "url": true, "ghNumber": true, "number": true,
	"status": true, "ghState": true, "mergedAt": true, "checkedAt": true,
	"base": true, "branch": true, "error": true, "commits": true, "ghError": true,
}

// UnmarshalJSON accepts ghNumber (console) or number (older Go) and stashes
// unknown per-element keys so a later write is lossless.
func (p *PR) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["ghNumber"]; !ok {
		if n, ok := raw["number"]; ok {
			raw["ghNumber"] = n
		}
	}
	type wire PR
	reb, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var w wire
	if err := json.Unmarshal(reb, &w); err != nil {
		return err
	}
	*p = PR(w)
	extra := map[string]any{}
	for k, v := range raw {
		if !prOwnedJSON[k] {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		p.Extra = extra
	}
	p.URL = git.AcceptPRURL(p.URL)
	return nil
}

// MarshalJSON always writes ghNumber (never number). Struct fields win over Extra.
func (p PR) MarshalJSON() ([]byte, error) {
	type wire struct {
		Repo      string `json:"repo,omitempty"`
		URL       string `json:"url,omitempty"`
		Number    int    `json:"ghNumber,omitempty"`
		Status    string `json:"status,omitempty"`
		GhState   string `json:"ghState,omitempty"`
		MergedAt  string `json:"mergedAt,omitempty"`
		CheckedAt string `json:"checkedAt,omitempty"`
		Base      string `json:"base,omitempty"`
		Branch    string `json:"branch,omitempty"`
		Error     string `json:"error,omitempty"`
		Commits   int    `json:"commits,omitempty"`
		GhError   string `json:"ghError,omitempty"`
	}
	b, err := json.Marshal(wire{
		Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
		GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt,
		Base: p.Base, Branch: p.Branch, Error: p.Error, Commits: p.Commits,
		GhError: p.GhError,
	})
	if err != nil {
		return nil, err
	}
	if len(p.Extra) == 0 {
		return b, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range p.Extra {
		if prOwnedJSON[k] {
			continue
		}
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
	return json.Marshal(m)
}

func prFromResult(p git.PRResult) PR {
	return PR{
		Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
		GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		Branch: p.Branch, Error: p.Error, Commits: p.Commits, GhError: p.GhError,
	}
}

func prToResult(p PR) git.PRResult {
	return git.PRResult{
		Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
		GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		Branch: p.Branch, Error: p.Error, Commits: p.Commits, GhError: p.GhError,
	}
}

// prevPRExtra carries PR.Extra across a refresh. Match the previous row by
// index first, then repo+branch. git.PRResult stays typed-only.
func prevPRExtra(prev []PR, i int, next PR) map[string]any {
	if i >= 0 && i < len(prev) {
		return cloneMap(prev[i].Extra)
	}
	for _, p := range prev {
		if p.Repo == next.Repo && p.Branch == next.Branch {
			return cloneMap(p.Extra)
		}
	}
	return nil
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return map[string]any{}
	}
	return out
}

func clonePR(p PR) PR {
	c := p
	if p.Extra != nil {
		c.Extra = cloneMap(p.Extra)
	}
	return c
}

func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	c := *j
	if j.DefectIDs != nil {
		c.DefectIDs = append([]string(nil), j.DefectIDs...)
	}
	if j.Repos != nil {
		c.Repos = append([]string(nil), j.Repos...)
	}
	if j.Log != nil {
		c.Log = append([]string(nil), j.Log...)
	}
	if j.PRs != nil {
		c.PRs = make([]PR, len(j.PRs))
		for i, p := range j.PRs {
			c.PRs[i] = clonePR(p)
		}
	}
	if j.Worktrees != nil {
		c.Worktrees = append([]git.WorktreeBinding(nil), j.Worktrees...)
	}
	if j.Verification != nil {
		v := *j.Verification
		if j.Verification.Results != nil {
			v.Results = append([]VerifyResult(nil), j.Verification.Results...)
		}
		c.Verification = &v
	}
	if j.Baseline != nil {
		v := *j.Baseline
		if j.Baseline.Results != nil {
			v.Results = append([]VerifyResult(nil), j.Baseline.Results...)
		}
		c.Baseline = &v
	}
	if j.DiffHygiene != nil {
		d := *j.DiffHygiene
		if j.DiffHygiene.Repos != nil {
			d.Repos = append([]RepoDiffHygiene(nil), j.DiffHygiene.Repos...)
		}
		c.DiffHygiene = &d
	}
	if j.Extra != nil {
		c.Extra = cloneMap(j.Extra)
	}
	return &c
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
	if j == nil {
		return fmt.Errorf("nil job")
	}
	j.UpdatedAt = db.NowIso()
	return writeJobSnapshot(dir, j)
}

// writeJobSnapshot writes j as-is (no UpdatedAt stamp) via temp+rename so a
// crash or overlapping writer cannot leave a truncated job.json.
func writeJobSnapshot(dir string, j *Job) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "job.json"), b)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".job-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	// Sync the parent so a crash cannot lose the directory entry for the
	// renamed SSOT file. Ignore errors — some platforms cannot fsync a dir.
	if dirf, err := os.Open(dir); err == nil {
		_ = dirf.Sync()
		_ = dirf.Close()
	}
	return nil
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
		Source:      source,
		Severity:    sev,
		Client:      client,
		Surface:     in.Surface,
	}
	if mode == "manual" {
		j.Status = "waiting_external"
	}
	if err := writeJob(dir, j); err != nil {
		return nil, err
	}
	if mode != "manual" {
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
		saved, err := r.store.Write(store.DefectRecord{
			ID:          defectID,
			AppID:       appID,
			Title:       title,
			Severity:    sev,
			Status:      "open",
			Area:        area,
			Client:      client,
			Surface:     in.Surface,
			Repos:       in.Repos,
			Labels:      []string{},
			Related:     []string{},
			Source:      source,
			Reporter:    in.Reporter, // omitted → ""
			KeyFiles:    []string{},
			Evidence:    evPaths,
			FixEvidence: []string{},
			Reported:    db.TodayUTC(),
			Summary:     in.Comment,
			Body:        in.Comment,
			Bucket:      "open",
		})
		if err != nil {
			return nil, err
		}
		j.DefectPath = saved.Path
		_ = writeJob(dir, j)
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
	Path      string   `json:"path"`
}

// spawnedProc is one live child. Wait is once-only so DeleteJob and the
// completion goroutine can both reap without racing exec.Cmd.Wait.
type spawnedProc struct {
	cmd      *exec.Cmd
	waitOnce sync.Once
	waitErr  error
}

func (p *spawnedProc) reap() error {
	if p == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		if p.cmd != nil {
			p.waitErr = p.cmd.Wait()
		}
	})
	return p.waitErr
}

// BatchRunner persists batches + batch-jobs.
type BatchRunner struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	persist   map[string]*sync.Mutex
	starting     map[string]chan struct{}
	verifyCancel map[string]context.CancelFunc
	store        *store.Store
	dataRoot     string
	jobs         map[string]*Job
	procs        map[string]*spawnedProc
}

// NewBatchRunner hydrates batch-jobs; running/queued → failed + interrupted.
func NewBatchRunner(st *store.Store, dataRoot string) *BatchRunner {
	b := &BatchRunner{
		store: st, dataRoot: dataRoot,
		jobs:         map[string]*Job{},
		procs:        map[string]*spawnedProc{},
		persist:      map[string]*sync.Mutex{},
		verifyCancel: map[string]context.CancelFunc{},
	}
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
			j.Log = append(j.Log, "[warn] [DefectDrainer] marked failed on hydrate (Grok process no longer live)")
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
		Log: []string{
			"[info] [DefectDrainer] batch manifest → " + batchSQLitePath(bid),
			"[info] [DefectDrainer] handoff → " + filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID),
		},
		Extra: map[string]any{"batchPath": batchSQLitePath(bid)},
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
		Path: batchSQLitePath(bid),
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
	rec.Path = batchSQLitePath(rec.ID)
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
		rec.Path = batchSQLitePath(rec.ID)
		out = append(out, rec)
	}
	if out == nil {
		out = []BatchRecord{}
	}
	return out, rows.Err()
}

// GetJob returns a deep copy of a batch job so HTTP encode cannot race
// with harvest / appendLog mutating the live map entry.
func (b *BatchRunner) GetJob(id string) *Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneJob(b.jobs[id])
}

// List returns in-memory batch jobs sorted by createdAt descending (TS list()).
func (b *BatchRunner) List() []*Job {
	b.mu.Lock()
	out := make([]*Job, 0, len(b.jobs))
	for _, j := range b.jobs {
		out = append(out, cloneJob(j))
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, k int) bool {
		return out[i].CreatedAt > out[k].CreatedAt
	})
	return out
}

func (b *BatchRunner) persistSlot(id string) *sync.Mutex {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()
	if b.persist == nil {
		b.persist = map[string]*sync.Mutex{}
	}
	m := b.persist[id]
	if m == nil {
		m = &sync.Mutex{}
		b.persist[id] = m
	}
	return m
}

// persistLive is the one persist path per job: serialize writers, re-clone
// the live map entry immediately before writing, stamp UpdatedAt on the live
// job under b.mu (so GetJob matches job.json), then write off the mutex.
// A missing map entry is a successful delete — do not recreate the file.
// A write error is returned and logged; callers must not treat a failed
// persist as success.
func (b *BatchRunner) persistLive(id string) error {
	if id == "" {
		return nil
	}
	slot := b.persistSlot(id)
	slot.Lock()
	defer slot.Unlock()
	b.mu.Lock()
	live := b.jobs[id]
	if live == nil {
		b.mu.Unlock()
		return nil
	}
	live.UpdatedAt = db.NowIso()
	snap := cloneJob(live)
	b.mu.Unlock()
	err := writeJobSnapshot(filepath.Join(paths.BatchJobsDir(b.dataRoot), snap.ID), snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "defect-drainer: persist job %s: %v\n", id, err)
	}
	return err
}

// DeleteJob stops the child (signal + reap) then removes the job dir.
// The map entry is dropped first so a later appendLog/persistLive cannot
// resurrect job.json. If StartSpawn is still in the startup window,
// wait for it to install or abandon the child before RemoveAll.
func (b *BatchRunner) DeleteJob(id string) error {
	b.mu.Lock()
	slot := b.procs[id]
	var startDone chan struct{}
	if b.starting != nil {
		startDone = b.starting[id]
	}
	delete(b.jobs, id)
	delete(b.procs, id)
	c := b.verifyCancel[id]
	delete(b.verifyCancel, id)
	b.mu.Unlock()
	if c != nil {
		c()
	}
	if slot != nil && slot.cmd != nil && slot.cmd.Process != nil {
		_ = slot.cmd.Process.Kill()
	}
	if slot != nil {
		_ = slot.reap()
	}
	if startDone != nil {
		<-startDone
	}
	if b.store != nil {
		analytics.DeleteJobSummary(b.store.DB, id)
	}
	ps := b.persistSlot(id)
	ps.Lock()
	err := os.RemoveAll(filepath.Join(paths.BatchJobsDir(b.dataRoot), id))
	ps.Unlock()
	b.persistMu.Lock()
	delete(b.persist, id)
	b.persistMu.Unlock()
	return err
}

func (b *BatchRunner) setVerifyCancel(id string, cancel context.CancelFunc) {
	b.mu.Lock()
	if b.verifyCancel == nil {
		b.verifyCancel = map[string]context.CancelFunc{}
	}
	b.verifyCancel[id] = cancel
	b.mu.Unlock()
}

func (b *BatchRunner) callVerifyCancel(id string) {
	b.mu.Lock()
	c := b.verifyCancel[id]
	delete(b.verifyCancel, id)
	b.mu.Unlock()
	if c != nil {
		c()
	}
}

// MarkFailed updates job+batch. Mutate under the lock, persist off it, SQL last.
func (b *BatchRunner) MarkFailed(jobID, batchID, msg string) {
	b.mu.Lock()
	if j := b.jobs[jobID]; j != nil {
		j.Status = "failed"
		j.Error = msg
	}
	b.mu.Unlock()
	b.persistLive(jobID)
	if b.store != nil {
		_, _ = b.store.DB.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ?`, "failed", db.NowIso(), batchID)
	}
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

const jobLogCap = 4000

// appendLogLocked requires b.mu held. In-memory append only — never writes.
// Format matches TS batchJob.log: `[level] [source] line`. Caps at a trailing 4000.
func (b *BatchRunner) appendLogLocked(j *Job, line, level, source string) {
	if j == nil {
		return
	}
	if level == "" {
		level = "info"
	}
	if source == "" {
		source = "DefectDrainer"
	}
	j.Log = append(j.Log, fmt.Sprintf("[%s] [%s] %s", level, source, line))
	if len(j.Log) > jobLogCap {
		j.Log = j.Log[len(j.Log)-jobLogCap:]
	}
}

// appendLog takes the mutex, appends in memory, then persists the live job.
// If the map entry is gone (deleted), it returns without writing — the stub
// Job{ID} the stream callback holds must not recreate job.json.
func (b *BatchRunner) appendLog(j *Job, line, level, source string) {
	if j == nil {
		return
	}
	b.mu.Lock()
	live := b.jobs[j.ID]
	if live == nil {
		b.mu.Unlock()
		return
	}
	b.appendLogLocked(live, line, level, source)
	b.mu.Unlock()
	b.persistLive(j.ID)
}

// HarvestOpts gates resolve on the post-fix verification run. Nil / ran==false
// keeps evidence-only resolve (failed-agent harvest and jobs with no commands).
type HarvestOpts struct {
	Verification *VerificationRun
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true,
	".webp": true, ".gif": true, ".heic": true,
}

// harvestFileLimit matches api.maxMultipartMem: a harvested file is stored
// and served like an upload, so it cannot be larger than an upload.
const harvestFileLimit = 26 << 20

func harvestCutoff(j *Job) time.Time {
	if j == nil || j.CreatedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, j.CreatedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

// readRegularFile reads p only when p is a regular file reached without
// traversing a symlink. The agent that populates the handoff directory is
// sandboxed precisely so it cannot read host secrets; harvest is not. Following
// a link the agent planted would make this process read the secret on its
// behalf, so every harvest read is confined to real files under root.
//
// tooLarge is true when the file exceeds harvestFileLimit; callers must skip
// and log rather than persist a truncated prefix. A mismatched birthtime
// (inode created before notBefore) is a silent skip — same as Nlink/Dev.
func readRegularFile(root, p string, notBefore time.Time) (data []byte, tooLarge, ok bool) {
	if !underRoot(root, p) {
		return nil, false, false
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, false
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()
	// Re-check after the open so a swap between Lstat and open cannot win.
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false, false
	}
	sys, okSys := st.Sys().(*syscall.Stat_t)
	if !okSys || sys.Nlink > 1 {
		// A hardlink is a regular file and passes O_NOFOLLOW; it still shares
		// the inode of a host secret the agent could not read. Refuse it.
		return nil, false, false
	}
	dirInfo, err := os.Stat(root)
	if err != nil {
		return nil, false, false
	}
	dirSys, okDir := dirInfo.Sys().(*syscall.Stat_t)
	if !okDir || sys.Dev != dirSys.Dev {
		return nil, false, false
	}
	if !notBefore.IsZero() {
		if born, hasBtime := fileBirthTime(f, sys); hasBtime && born.Before(notBefore) {
			return nil, false, false
		}
	}
	if st.Size() > harvestFileLimit {
		return nil, true, false
	}
	b, err := io.ReadAll(io.LimitReader(f, harvestFileLimit+1))
	if err != nil {
		return nil, false, false
	}
	if int64(len(b)) > harvestFileLimit {
		return nil, true, false
	}
	return b, false, true
}

// underRoot reports whether p resolves to a location inside root, with every
// symlink on the way already resolved. A missing path is judged on its cleaned
// form so callers can probe candidates that do not exist.
func underRoot(root, p string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	target := p
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		target = resolved
	} else {
		target = filepath.Clean(p)
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isRealDir reports whether p is a directory itself, not a symlink to one.
func isRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

func readFirstNote(root string, notBefore time.Time, cands ...string) (note string, oversized []string) {
	for _, p := range cands {
		b, tooLarge, ok := readRegularFile(root, p, notBefore)
		if tooLarge {
			oversized = append(oversized, p)
			continue
		}
		if !ok {
			continue
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, oversized
		}
	}
	return "", oversized
}

// harvestFixEvidence ports backend/src/jobs/batchJob.ts harvestFixEvidence.
func (b *BatchRunner) harvestFixEvidence(job *Job, handoff string, opts *HarvestOpts) {
	if job == nil {
		return
	}
	if harvestStartHook != nil {
		harvestStartHook()
	}
	var verification *VerificationRun
	if opts != nil {
		verification = opts.Verification
	}
	fixRoot := filepath.Join(handoff, "fix-evidence")
	notesRoot := filepath.Join(handoff, "fix-notes")
	imported := 0
	resolved := 0
	cutoff := harvestCutoff(job)

	for _, defectID := range job.DefectIDs {
		if !store.IsSafeID(defectID) {
			continue
		}
		imgDir := filepath.Join(fixRoot, defectID)
		var files []struct {
			Filename string
			Data     []byte
		}
		if ents, err := os.ReadDir(imgDir); err == nil && isRealDir(imgDir) {
			for _, e := range ents {
				name := e.Name()
				// DirEntry.IsDir follows the link, so a symlinked directory
				// would read as a file here; readRegularFile is the real gate.
				if strings.HasPrefix(name, ".") || e.IsDir() {
					continue
				}
				ext := strings.ToLower(filepath.Ext(name))
				if !imageExts[ext] {
					continue
				}
				data, tooLarge, ok := readRegularFile(handoff, filepath.Join(imgDir, name), cutoff)
				if tooLarge {
					b.appendLog(job, fmt.Sprintf("harvest %s: skip %s (exceeds %d bytes)", defectID, name, harvestFileLimit), "warn", "DefectDrainer")
					continue
				}
				if !ok {
					continue
				}
				files = append(files, struct {
					Filename string
					Data     []byte
				}{name, data})
			}
		}
		note, oversizedNotes := readFirstNote(
			handoff,
			cutoff,
			filepath.Join(notesRoot, defectID+".md"),
			filepath.Join(notesRoot, defectID+".txt"),
			filepath.Join(fixRoot, defectID, "NOTES.md"),
			filepath.Join(fixRoot, defectID, "resolution.md"),
		)
		for _, p := range oversizedNotes {
			b.appendLog(job, fmt.Sprintf("harvest %s: skip %s (exceeds %d bytes)", defectID, filepath.Base(p), harvestFileLimit), "warn", "DefectDrainer")
		}
		if len(files) == 0 && note == "" {
			b.appendLog(job, "harvest "+defectID+": no fix-evidence/ or fix-notes/ — left open", "warn", "DefectDrainer")
			continue
		}
		cur, err := b.store.Get(defectID)
		if err != nil || cur == nil {
			continue
		}
		if len(files) > 0 {
			added, err := b.store.SaveEvidence(defectID, files, "fix")
			if err != nil {
				b.appendLog(job, "harvest "+defectID+" failed: "+err.Error(), "error", "DefectDrainer")
				continue
			}
			cur.FixEvidence = append(cur.FixEvidence, added...)
			saved, err := b.store.Write(*cur)
			if err != nil {
				b.appendLog(job, "harvest "+defectID+" failed: "+err.Error(), "error", "DefectDrainer")
				continue
			}
			cur = &saved
			imported += len(files)
			b.appendLog(job, fmt.Sprintf("harvest %s: %d fix image(s) → evidence/", defectID, len(files)), "info", "DefectDrainer")
		}

		// Block on what THIS job broke or could not run — not on red that
		// was already there before it started (see judgeVerification).
		verdict := judgeVerification(verification, job.Baseline)
		if verification != nil && verification.Ran && !verdict.Ok {
			b.appendLog(job, "harvest "+defectID+": NOT resolved — "+summarizeVerdict(verdict), "warn", "DefectDrainer")
			continue
		}

		if len(cur.FixEvidence) > 0 {
			res := note
			if res == "" {
				res = cur.Resolution
			}
			if res == "" {
				res = "fixed in batch " + job.BatchID + " (Grok + fix evidence"
				if verification != nil && verification.Ran {
					res += "; " + summarizeVerdict(judgeVerification(verification, job.Baseline))
				}
				res += ")"
			}
			if _, err := b.store.Resolve(defectID, res, cur.FixEvidence, false); err != nil {
				b.appendLog(job, "harvest "+defectID+" failed: "+err.Error(), "error", "DefectDrainer")
				continue
			}
			resolved++
			b.appendLog(job, "harvest "+defectID+": resolved with fix_evidence", "info", "DefectDrainer")
		} else if note != "" {
			_, _ = b.store.Update(defectID, store.DefectRecord{Resolution: note}, map[string]bool{"resolution": true})
			b.appendLog(job, "harvest "+defectID+": notes only (no images) — still needs screenshots", "warn", "DefectDrainer")
		}
	}
	b.appendLog(job, fmt.Sprintf("harvest done: %d image(s), %d/%d resolved", imported, resolved, len(job.DefectIDs)), "info", "DefectDrainer")
}

// SyncBatchAndDefects aligns batch + defects with the job outcome.
// complete: leave harvested/resolved defects; others stay in_progress.
// failed/cancelled: reopen in_progress defects that have no fix_evidence.
func (b *BatchRunner) SyncBatchAndDefects(job *Job, batchStatus string) {
	if job == nil {
		return
	}
	_, _ = b.store.DB.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ?`, batchStatus, db.NowIso(), job.BatchID)
	if batchStatus == "complete" {
		b.appendLog(job, "batch "+job.BatchID+" → "+batchStatus, "info", "DefectDrainer")
		return
	}
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
			b.appendLog(job, "defect "+id+" → open ("+batchStatus+")", "info", "DefectDrainer")
		}
	}
	b.appendLog(job, "batch "+job.BatchID+" → "+batchStatus, "info", "DefectDrainer")
}

// SetWorktrees persists bindings on the job (create-prs/refresh-prs need them).
func (b *BatchRunner) SetWorktrees(id string, wts []git.WorktreeBinding) *Job {
	b.mu.Lock()
	j := b.jobs[id]
	if j == nil {
		b.mu.Unlock()
		return nil
	}
	j.Worktrees = wts
	b.mu.Unlock()
	b.persistLive(id)
	return b.GetJob(id)
}

// UpdatePRs stores prs[] on the job.
func (b *BatchRunner) UpdatePRs(id string, prs []PR) *Job {
	b.mu.Lock()
	j := b.jobs[id]
	if j == nil {
		b.mu.Unlock()
		return nil
	}
	j.PRs = prs
	b.mu.Unlock()
	b.persistLive(id)
	return b.GetJob(id)
}

func (b *BatchRunner) writeSpawnBrief(j *Job, defectsRoot, handoff string) error {
	if j == nil || b.GetJob(j.ID) == nil {
		return nil
	}
	rec, err := b.GetBatch(j.BatchID)
	if err != nil || rec == nil {
		rec = &BatchRecord{ID: j.BatchID, AppID: j.AppID, Path: batchSQLitePath(j.BatchID)}
	}
	var defects []store.DefectRecord
	for _, id := range j.DefectIDs {
		d, err := b.store.Get(id)
		if err != nil || d == nil {
			continue
		}
		defects = append(defects, *d)
	}
	sandbox := "strict"
	if app, err := store.GetApp(b.store.DB, j.AppID); err == nil && app != nil {
		sandbox = store.ParseGrokSandbox(app.GrokSandbox)
	}
	return WriteHandoffBrief(handoff, BatchFixBriefInput{
		Batch:       *rec,
		Defects:     defects,
		DefectsRoot: defectsRoot,
		GrokSandbox: sandbox,
		Worktrees:   j.Worktrees,
	})
}

// WriteFailClosedBrief writes BRIEF.md with an empty worktree list (the
// "NONE — refuse to edit" stanza) so fail-closed worktree setup still leaves
// something an operator can inspect.
func (b *BatchRunner) WriteFailClosedBrief(job *Job, defectsRoot string) {
	if job == nil {
		return
	}
	handoff := filepath.Join(paths.BatchJobsDir(b.dataRoot), job.ID)
	cp := *job
	cp.Worktrees = nil
	_ = b.writeSpawnBrief(&cp, defectsRoot, handoff)
}

func formatGrokExit(err error) string {
	code, sig := "null", "null"
	if err == nil {
		return fmt.Sprintf("Grok exited code=%s signal=%s", code, sig)
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ProcessState != nil {
		code = strconv.Itoa(ee.ExitCode())
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig = unixSignalName(ws.Signal())
		}
	}
	return fmt.Sprintf("Grok exited code=%s signal=%s", code, sig)
}

func unixSignalName(sig os.Signal) string {
	if sig == nil {
		return "null"
	}
	// syscall.Signal.String() is "terminated" / "interrupt"; TS uses SIGTERM / SIGINT.
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return sig.String()
	}
}

// Test-only hooks. Nil in production.
// startSpawnHook runs after StartSpawn releases b.mu and before the brief write
// so a test can cancel during the startup window.
// startSpawnAfterStartHook runs after cmd.Start returns and before the relock
// that installs procs[id] — a live child exists but is still untracked.
// harvestStartHook runs at the top of harvestFixEvidence so a test can delete
// while harvest still holds the live *Job pointer.
// spawnExitHook runs when the completion goroutine returns.
var (
	startSpawnHook           func()
	startSpawnAfterStartHook func()
	harvestStartHook         func()
	spawnExitHook            func()
)

func jobGoneOrCancelled(j *Job) bool {
	return j == nil || j.Status == "cancelled"
}

func abandonStartedAgent(cmd *exec.Cmd, flush func()) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	if flush != nil {
		flush()
	}
}

// StartSpawn execs the resolved coding-agent bin. Empty worktrees refuse spawn.
// Status==cancelled is the startup cancel flag: Stop sets it while procs[id]
// is still nil; this function honours it after the brief write and after Start
// and will not install the handle or overwrite the status.
func (b *BatchRunner) StartSpawn(jobID, defectsRoot string) error {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return fmt.Errorf("job not found: %s", jobID)
	}
	if jobGoneOrCancelled(j) {
		b.mu.Unlock()
		return nil
	}
	if len(j.Worktrees) == 0 {
		b.mu.Unlock()
		return fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	handoff := filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID)
	batchID := j.BatchID
	appID := j.AppID
	jobCopy := *j
	done := make(chan struct{})
	if b.starting == nil {
		b.starting = map[string]chan struct{}{}
	}
	b.starting[jobID] = done
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	b.setVerifyCancel(jobID, cancel)
	launched := false
	defer func() {
		if !launched {
			b.callVerifyCancel(jobID)
		}
		b.mu.Lock()
		if b.starting[jobID] == done {
			delete(b.starting, jobID)
		}
		b.mu.Unlock()
		close(done)
	}()

	if startSpawnHook != nil {
		startSpawnHook()
	}

	if err := b.writeSpawnBrief(&jobCopy, defectsRoot, handoff); err != nil {
		return err
	}
	b.mu.Lock()
	if jobGoneOrCancelled(b.jobs[jobID]) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	app, _ := store.GetApp(b.store.DB, appID)
	sandbox := "strict"
	toolchainKind := "none"
	simWrites := false
	var verifyCommands []store.VerifyCommand
	var repoEntries []store.AppRepoEntry
	if app != nil {
		sandbox = store.ParseGrokSandbox(app.GrokSandbox)
		toolchainKind = store.ParseAgentToolchain(app.AgentToolchain)
		simWrites = app.AllowSimulatorWrites
		verifyCommands = store.NormalizeVerifyCommands(app.VerifyCommands)
		repoEntries = app.RepoEntries
	}
	_ = os.MkdirAll(filepath.Join(handoff, "fix-evidence"), 0o755)
	_ = os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755)
	b.appendLog(&jobCopy, fmt.Sprintf("app %s grok_sandbox=%s agent_toolchain=%s", appID, sandbox, toolchainKind), "info", "DefectDrainer")

	spawnLog := func(line, level string) {
		b.appendLog(&Job{ID: jobID}, line, level, "DefectDrainer")
	}
	provisioned := NO_TOOLCHAIN
	if toolchainKind == "flutter" {
		provisioned = provisionFlutterToolchain(handoff, wts, spawnLog)
	}

	var sandboxProfile string
	var profileNotes []string
	if simWrites {
		profile, err := writeSimulatorSandboxProfile(handoff, sandbox, batchID)
		if err != nil {
			spawnLog("sandbox: job profile failed — skipped ("+err.Error()+")", "warn")
		} else {
			sandboxProfile = profile.Profile
			profileNotes = profile.Notes
			spawnLog("sandbox: job profile '"+profile.Profile+"' extends "+sandbox+" + Simulator device writes", "info")
		}
	}

	diffBaseByRepo := map[string]string{}
	for _, w := range wts {
		diffBaseByRepo[w.Repo] = worktreeHead(w.WorktreeAbs)
	}
	realByRepo := snapshotWorktreeReals(wts)

	var baseline *VerificationRun
	if len(verifyCommands) > 0 {
		run := runVerification(ctx, verifyCommands, wts, filepath.Join(handoff, "baseline"), repoEntries, provisioned.VerifyEnv, realByRepo, func(line, level string) {
			spawnLog("baseline "+line, level)
		})
		baseline = &run
		if run.Ran {
			b.mu.Lock()
			if live := b.jobs[jobID]; live != nil && !jobGoneOrCancelled(live) {
				live.Baseline = baseline
				b.appendLogLocked(live, "baseline: "+summarizeVerification(run), "info", "DefectDrainer")
			}
			b.mu.Unlock()
			b.persistLive(jobID)
		}
	}

	b.mu.Lock()
	if jobGoneOrCancelled(b.jobs[jobID]) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	bin := git.ResolveGrokBin()
	onStdout := func(line string) { b.appendLog(&Job{ID: jobID}, line, "info", "Grok") }
	onStderr := func(line string) { b.appendLog(&Job{ID: jobID}, line, "warn", "Grok") }
	var verifySpecs []git.VerifySpec
	for _, v := range verifyCommands {
		verifySpecs = append(verifySpecs, git.VerifySpec{Repo: v.Repo, Command: v.Command})
	}
	notes := append(append([]string{}, provisioned.Notes...), profileNotes...)
	cmd, flush, err := git.StartCodingAgent(handoff, batchID, defectsRoot, wts, sandbox, onStdout, onStderr, &git.CodingAgentOpts{
		ExtraEnv:       provisioned.Env,
		SandboxProfile: sandboxProfile,
		ToolchainNotes: notes,
		VerifyCommands: verifySpecs,
		AlreadyFailing: alreadyFailingLines(baseline),
	})
	if err != nil {
		return err
	}
	if startSpawnAfterStartHook != nil {
		startSpawnAfterStartHook()
	}
	b.mu.Lock()
	live := b.jobs[jobID]
	if jobGoneOrCancelled(live) {
		b.mu.Unlock()
		abandonStartedAgent(cmd, flush)
		return nil
	}
	proc := &spawnedProc{cmd: cmd}
	b.procs[jobID] = proc
	live.Status = "running"
	live.Error = ""
	b.appendLogLocked(live, fmt.Sprintf(
		"spawning batch fix: %s — %s sandbox=%s cwd=%s (%d worktree(s))",
		bin, batchID, sandbox, handoff, len(wts),
	), "info", "DefectDrainer")
	for _, w := range wts {
		b.appendLogLocked(live, fmt.Sprintf("worktree %s → %s (%s)", w.Repo, w.WorktreeAbs, w.Branch), "info", "DefectDrainer")
	}
	b.mu.Unlock()
	b.persistLive(jobID)
	launched = true
	go func() {
		defer func() {
			if spawnExitHook != nil {
				spawnExitHook()
			}
			b.callVerifyCancel(jobID)
		}()
		err := proc.reap()
		if flush != nil {
			flush()
		}
		b.mu.Lock()
		delete(b.procs, jobID)
		live := b.jobs[jobID]
		if jobGoneOrCancelled(live) {
			b.mu.Unlock()
			return
		}
		failed := err != nil
		if failed {
			live.Status = "failed"
			live.Error = formatGrokExit(err)
			b.appendLogLocked(live, live.Error, "error", "DefectDrainer")
		} else {
			live.Error = ""
		}
		b.mu.Unlock()
		b.persistLive(jobID)
		if b.GetJob(jobID) == nil {
			return
		}
		var harvestOpts *HarvestOpts
		if !failed {
			run := runVerification(ctx, verifyCommands, wts, handoff, repoEntries, provisioned.VerifyEnv, realByRepo, spawnLog)
			hygiene := measureDiffHygiene(wts, diffBaseByRepo, spawnLog)
			verdict := judgeVerification(&run, baseline)
			level := "info"
			if !verdict.Ok {
				level = "warn"
			}
			b.mu.Lock()
			if live = b.jobs[jobID]; live != nil && !jobGoneOrCancelled(live) {
				cp := run
				live.Verification = &cp
				h := hygiene
				live.DiffHygiene = &h
				if baseline != nil {
					live.Baseline = baseline
				}
				b.appendLogLocked(live, "verify: "+summarizeVerdict(verdict), level, "DefectDrainer")
				b.appendLogLocked(live, "Grok process exited OK — harvesting fix-evidence…", "info", "DefectDrainer")
			}
			b.mu.Unlock()
			b.persistLive(jobID)
			harvestOpts = &HarvestOpts{Verification: &run}
		}
		if j := b.GetJob(jobID); j == nil || jobGoneOrCancelled(j) {
			return
		}
		b.harvestFixEvidence(b.GetJob(jobID), handoff, harvestOpts)
		live = b.GetJob(jobID)
		if live == nil || jobGoneOrCancelled(live) {
			return
		}
		if failed {
			b.SyncBatchAndDefects(live, "failed")
			return
		}
		b.mu.Lock()
		if cur := b.jobs[jobID]; cur != nil && !jobGoneOrCancelled(cur) {
			cur.Status = "completed"
		}
		b.mu.Unlock()
		b.persistLive(jobID)
		live = b.GetJob(jobID)
		if live == nil || jobGoneOrCancelled(live) {
			return
		}
		b.SyncBatchAndDefects(live, "complete")
		b.appendLog(live, "batch complete — review worktrees; merge when satisfied", "info", "DefectDrainer")
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
	slot := b.procs[jobID]
	j.Status = "cancelled"
	j.Error = "stopped by operator"
	b.appendLogLocked(j, "stop requested by operator (web UI)", "warn", "DefectDrainer")
	if slot == nil {
		b.appendLogLocked(j, "no live process handle (may still be starting)", "warn", "DefectDrainer")
	}
	c := b.verifyCancel[jobID]
	delete(b.verifyCancel, jobID)
	b.mu.Unlock()
	if c != nil {
		c()
	}
	b.persistLive(jobID)
	if slot != nil && slot.cmd != nil && slot.cmd.Process != nil {
		_ = slot.cmd.Process.Signal(os.Interrupt)
		go func() {
			time.Sleep(3 * time.Second)
			_ = slot.cmd.Process.Kill()
		}()
	}
	b.SyncBatchAndDefects(j, "cancelled")
	return b.GetJob(jobID), nil
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
	batchID := j.BatchID
	b.mu.Unlock()

	// SQL happens off the mutex on purpose. The DB is capped at one connection,
	// so a query issued while holding b.mu can block behind harvest's inserts —
	// and harvest finishes by taking b.mu to log. Lock, then query, is a
	// deadlock; copy what we need, unlock, then query.
	title := "defect-drainer " + batchID
	if rec, err := b.GetBatch(batchID); err == nil && rec != nil && rec.Title != "" {
		title = rec.Title
	}
	b.appendLog(j, fmt.Sprintf("Create PR: %d worktree(s)…", len(wts)), "info", "DefectDrainer")
	raw := git.CreatePRsForWorktrees(wts, j.BatchID, title)
	prs := make([]PR, 0, len(raw))
	for _, p := range raw {
		prs = append(prs, prFromResult(p))
	}
	updated := b.UpdatePRs(jobID, prs)
	created, failed, skipped := 0, 0, 0
	for _, p := range prs {
		switch p.Status {
		case "created", "existing":
			created++
		case "failed":
			failed++
		default:
			skipped++
		}
	}
	if updated != nil {
		b.appendLog(updated, fmt.Sprintf("Create PR done: %d PR(s), %d failed, %d skipped", created, failed, skipped), "info", "DefectDrainer")
	}
	return b.GetJob(jobID), nil
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
	prev := make([]PR, len(j.PRs))
	in := make([]git.PRResult, 0, len(j.PRs))
	for i, p := range j.PRs {
		prev[i] = clonePR(p)
		in = append(in, prToResult(p))
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	b.mu.Unlock()
	b.appendLog(j, fmt.Sprintf("Refresh PRs: %d row(s)…", len(in)), "info", "DefectDrainer")
	raw := git.RefreshPRStatuses(in, wts)
	prs := make([]PR, 0, len(raw))
	for i, p := range raw {
		pr := prFromResult(p)
		pr.Extra = prevPRExtra(prev, i, pr)
		prs = append(prs, pr)
	}
	updated := b.UpdatePRs(jobID, prs)
	merged, openN, closed, errs := 0, 0, 0, 0
	for _, p := range prs {
		switch p.GhState {
		case "merged":
			merged++
		case "open":
			openN++
		case "closed":
			closed++
		}
		if p.GhError != "" {
			errs++
		}
	}
	if updated != nil {
		line := fmt.Sprintf("Refresh PRs done: %d open, %d merged, %d closed", openN, merged, closed)
		if errs > 0 {
			line += fmt.Sprintf(", %d error(s)", errs)
		}
		b.appendLog(updated, line, "info", "DefectDrainer")
	}
	return b.GetJob(jobID), nil
}

func hasTrackable(prs []PR) bool {
	for _, p := range prs {
		if p.URL != "" || p.Status == "created" || p.Status == "existing" {
			return true
		}
	}
	return false
}
