// Package jobs persists normalize + batch job.json files (TS JSON-on-disk).
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/joe/defect-drainer-go/internal/env"
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
	TestRunID    string                `json:"testRunId,omitempty"`
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
	AgentRun     *AgentRun             `json:"agentRun,omitempty"`
	Review       *ReviewRun            `json:"review,omitempty"`
	DiffBase     map[string]string     `json:"diffBase,omitempty"`     // worktree HEADs at first spawn; restarts measure hygiene from here
	RunStartedAt string                `json:"runStartedAt,omitempty"` // latest Re-run/Resume; harvest imports only images newer than this
	Harvests     []HarvestRun          `json:"harvests,omitempty"`     // one entry per harvest, oldest first; last 20 kept
	QueuedAt     string                `json:"queuedAt,omitempty"`     // when this job parked for the simulator lease (RFC3339Nano, UTC)
	QueuedMode   string                `json:"queuedMode,omitempty"`   // fresh, rerun, or resume — replayed when the lease is handed over
	SimQueue     *SimQueue             `json:"simQueue,omitempty"`     // computed on API reads (GetJobView/ListView), never persisted
	Log          []string              `json:"log,omitempty"`
	Source       string                `json:"source,omitempty"`
	DefectPath   string                `json:"defectPath,omitempty"`
	Severity     string                `json:"severity,omitempty"`
	Client       string                `json:"client,omitempty"`
	Surface      string                `json:"surface,omitempty"`
	Resumable    *RestartCheck         `json:"resumable,omitempty"`  // computed on API reads (GetJobView/ListView), never persisted
	Rerunnable   *RestartCheck         `json:"rerunnable,omitempty"` // computed on API reads (GetJobView/ListView), never persisted
	Extra        map[string]any        `json:"-"`
}

// jobOwnedJSON is every key the Job struct serializes (plus legacy "id").
var jobOwnedJSON = map[string]bool{
	"jobId": true, "id": true,
	"kind": true, "status": true, "mode": true, "app_id": true,
	"defectId": true, "batchId": true, "testRunId": true, "defect_ids": true, "repos": true,
	"error": true, "handoffPath": true, "createdAt": true, "updatedAt": true,
	"comment": true, "reporter": true, "prs": true, "worktrees": true,
	"verification": true, "baseline": true, "diffHygiene": true,
	"agentRun": true, "review": true, "diffBase": true, "runStartedAt": true, "harvests": true, "queuedAt": true, "queuedMode": true, "simQueue": true, "resumable": true, "rerunnable": true,
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
		TestRunID    string                `json:"testRunId,omitempty"`
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
		AgentRun     *AgentRun             `json:"agentRun,omitempty"`
		Review       *ReviewRun            `json:"review,omitempty"`
		DiffBase     map[string]string     `json:"diffBase,omitempty"`
		RunStartedAt string                `json:"runStartedAt,omitempty"`
		Harvests     []HarvestRun          `json:"harvests,omitempty"`
		QueuedAt     string                `json:"queuedAt,omitempty"`
		QueuedMode   string                `json:"queuedMode,omitempty"`
		SimQueue     *SimQueue             `json:"simQueue,omitempty"`
		Log          []string              `json:"log,omitempty"`
		Source       string                `json:"source,omitempty"`
		DefectPath   string                `json:"defectPath,omitempty"`
		Severity     string                `json:"severity,omitempty"`
		Client       string                `json:"client,omitempty"`
		Surface      string                `json:"surface,omitempty"`
		Resumable    *RestartCheck         `json:"resumable,omitempty"`
		Rerunnable   *RestartCheck         `json:"rerunnable,omitempty"`
	}
	b, err := json.Marshal(wire{
		ID: j.ID, Kind: j.Kind, Status: j.Status, Mode: j.Mode, AppID: j.AppID,
		DefectID: j.DefectID, BatchID: j.BatchID, TestRunID: j.TestRunID, DefectIDs: j.DefectIDs,
		Repos: j.Repos, Error: j.Error, HandoffPath: j.HandoffPath,
		CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, Comment: j.Comment,
		Reporter: j.Reporter, PRs: j.PRs, Worktrees: j.Worktrees,
		Verification: j.Verification, Baseline: j.Baseline, DiffHygiene: j.DiffHygiene,
		AgentRun: j.AgentRun, Review: j.Review, DiffBase: j.DiffBase, RunStartedAt: j.RunStartedAt,
		Harvests: j.Harvests,
		QueuedAt: j.QueuedAt, QueuedMode: j.QueuedMode, SimQueue: j.SimQueue,
		Resumable: j.Resumable, Rerunnable: j.Rerunnable,
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
	Kind      string         `json:"kind,omitempty"`
	Extra     map[string]any `json:"-"`
}

var prOwnedJSON = map[string]bool{
	"repo": true, "url": true, "ghNumber": true, "number": true,
	"status": true, "ghState": true, "mergedAt": true, "checkedAt": true,
	"base": true, "branch": true, "error": true, "commits": true, "ghError": true,
	"kind": true,
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
		Kind      string `json:"kind,omitempty"`
	}
	b, err := json.Marshal(wire{
		Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
		GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt,
		Base: p.Base, Branch: p.Branch, Error: p.Error, Commits: p.Commits,
		GhError: p.GhError, Kind: p.Kind,
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
		Kind: p.Kind,
	}
}

func prToResult(p PR) git.PRResult {
	return git.PRResult{
		Repo: p.Repo, URL: p.URL, Number: p.Number, Status: p.Status,
		GhState: p.GhState, MergedAt: p.MergedAt, CheckedAt: p.CheckedAt, Base: p.Base,
		Branch: p.Branch, Error: p.Error, Commits: p.Commits, GhError: p.GhError,
		Kind: p.Kind,
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
	if j.AgentRun != nil {
		a := *j.AgentRun
		if j.AgentRun.StopReasons != nil {
			a.StopReasons = append([]string(nil), j.AgentRun.StopReasons...)
		}
		if j.AgentRun.Missing != nil {
			a.Missing = append([]string(nil), j.AgentRun.Missing...)
		}
		c.AgentRun = &a
	}
	if j.Review != nil {
		c.Review = cloneReview(j.Review)
	}
	if j.DiffBase != nil {
		c.DiffBase = make(map[string]string, len(j.DiffBase))
		for k, v := range j.DiffBase {
			c.DiffBase[k] = v
		}
	}
	if j.DiffHygiene != nil {
		d := *j.DiffHygiene
		if j.DiffHygiene.Repos != nil {
			d.Repos = append([]RepoDiffHygiene(nil), j.DiffHygiene.Repos...)
		}
		c.DiffHygiene = &d
	}
	if j.SimQueue != nil {
		s := *j.SimQueue
		c.SimQueue = &s
	}
	if j.Harvests != nil {
		c.Harvests = make([]HarvestRun, len(j.Harvests))
		for i := range j.Harvests {
			c.Harvests[i] = cloneHarvestRun(j.Harvests[i])
		}
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
	// group is set for a reviewer. Stop and Delete kill its process group.
	// The implementer stays a normal process.
	group bool
}

// kill stops a live child. A reviewer is a process-group leader; SIGKILL the
// group so git and shell grandchildren die with it.
func (p *spawnedProc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if p.group {
		git.KillReviewProcess(p.cmd)
		return
	}
	_ = p.cmd.Process.Kill()
}

func (p *spawnedProc) reap() error {
	if p == nil {
		return nil
	}
	p.waitOnce.Do(func() {
		var pid int
		if p.cmd != nil && p.cmd.Process != nil {
			pid = p.cmd.Process.Pid
		}
		if p.cmd != nil {
			p.waitErr = p.cmd.Wait()
			// A background child keeps the stdout pipe open, so Wait reports
			// ErrWaitDelay after the leader has already exited 0. That is not
			// an implementer failure; the group kill below reaps the child.
			if errors.Is(p.waitErr, exec.ErrWaitDelay) && p.cmd.ProcessState != nil && p.cmd.ProcessState.Success() {
				p.waitErr = nil
			}
		}
		// The reviewer's post-reap kill is suppressed (R12). The implementer's
		// descendants are the ones that must not outlive this Wait.
		if !p.group && pid > 0 {
			git.KillImplementerGroup(pid)
		}
	})
	return p.waitErr
}

// beginReview opens the DeleteJob gate for this job. Call it before the
// manifest capture. The returned func closes the gate and is safe to call
// twice; defer it so the after-review compare has finished on every return path.
func (b *BatchRunner) beginReview(jobID string) func() {
	ch := make(chan struct{})
	b.mu.Lock()
	if b.reviewing == nil {
		b.reviewing = map[string]chan struct{}{}
	}
	b.reviewing[jobID] = ch
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			if b.reviewing[jobID] == ch {
				delete(b.reviewing, jobID)
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// BatchRunner persists batches + batch-jobs.
type BatchRunner struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	persist   map[string]*sync.Mutex
	starting  map[string]chan struct{}
	// looping stays open for the whole driver-loop goroutine, not just the
	// first Start. Between reaping one iteration and starting the next,
	// procs[jobID] is empty, so Stop's signal is a no-op and DeleteJob would
	// otherwise RemoveAll the handoff out from under the next child.
	looping map[string]chan struct{}
	// reviewing is open from just before the manifest capture until the
	// after-review compare has finished. DeleteJob waits on it after killing
	// the child. It is not looping: holding looping through review would
	// deadlock a delete that lands during harvest, which runs after this
	// channel is closed.
	reviewing map[string]chan struct{}
	// driving spans the whole driver goroutine, verify and harvest included.
	// Stop can cancel a job mid-harvest; until this clears, a Restart would
	// share the handoff with a goroutine that still writes the job and, on
	// exit, cancels whatever verify context is current.
	driving      map[string]bool
	verifyCancel map[string]*verifyCancelSlot
	store        *store.Store
	dataRoot     string
	jobs         map[string]*Job
	procs        map[string]*spawnedProc
	// simHolder is the job that owns the one host simulator lease. simQueue
	// is the FIFO of AllowSimulatorWrites jobs waiting for it. defectsRoot is
	// the defects directory a handover passes to startSpawn; ResumeQueue and
	// startSpawn set it. It is not read back from job.json.
	simHolder   string
	simQueue    []simWaiter
	defectsRoot string
	// secrets holds a test-account password for the life of that job's agent
	// loop. It is never persisted.
	secrets map[string]string
}

// NewBatchRunner hydrates batch-jobs. A running job is failed as interrupted
// (its agent died with the process). A queued job stays queued; call
// ResumeQueue to start the oldest one.
func NewBatchRunner(st *store.Store, dataRoot string) *BatchRunner {
	b := &BatchRunner{
		store: st, dataRoot: dataRoot,
		jobs:         map[string]*Job{},
		procs:        map[string]*spawnedProc{},
		persist:      map[string]*sync.Mutex{},
		verifyCancel: map[string]*verifyCancelSlot{},
		secrets:      map[string]string{},
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
	var queued []*Job
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		j := readJob(filepath.Join(root, e.Name(), "job.json"))
		if j == nil {
			continue
		}
		// simQueue is computed on read. Drop a copy an older write left behind
		// so the next persist does not store it. Harvest caps are structural
		// only: no evidence-dir reads while hydrating.
		j.SimQueue = nil
		j.Harvests = capHarvestRuns(j.Harvests, nil)
		if j.Status == "running" {
			j.Status = "failed"
			j.Error = "interrupted by backend restart"
			j.Log = append(j.Log, "[warn] [DefectDrainer] marked failed on hydrate (Grok process no longer live)")
			_ = writeJob(filepath.Join(root, e.Name()), j)
		}
		b.jobs[j.ID] = j
		if j.Status == "queued" {
			queued = append(queued, j)
		}
	}
	b.rebuildSimQueue(queued)
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

// finishTerminal writes status into job.json and only then publishes it on
// the live job. logLine, when set, is part of that same write and is not
// visible until the write returns. A client that observes the terminal
// status is not racing a later rename of job.json. A cancelled or deleted
// job is left as it is.
func (b *BatchRunner) finishTerminal(id, status, logLine string) {
	if id == "" || status == "" {
		return
	}
	slot := b.persistSlot(id)
	slot.Lock()
	defer slot.Unlock()
	b.mu.Lock()
	live := b.jobs[id]
	if live == nil || jobGoneOrCancelled(live) {
		b.mu.Unlock()
		return
	}
	snap := cloneJob(live)
	if logLine != "" {
		b.appendLogLocked(snap, logLine, "info", "DefectDrainer")
	}
	b.mu.Unlock()
	snap.Status = status
	snap.UpdatedAt = db.NowIso()
	if err := writeJobSnapshot(filepath.Join(paths.BatchJobsDir(b.dataRoot), snap.ID), snap); err != nil {
		fmt.Fprintf(os.Stderr, "defect-drainer: persist job %s: %v\n", id, err)
	}
	b.mu.Lock()
	live = b.jobs[id]
	if live != nil && !jobGoneOrCancelled(live) {
		live.Status = status
		live.UpdatedAt = snap.UpdatedAt
		live.Log = snap.Log
	}
	b.mu.Unlock()
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

// DeleteJob stops the child (signal + reap), removes the job's worktrees from
// each product repo (git.DiscardWorktrees: registration pruned, branch deleted
// only if merged), then removes the job dir. It returns that per-repo report.
// The map entry is dropped first so a later appendLog/persistLive cannot
// resurrect job.json. If StartSpawn is still in the startup window,
// wait for it to install or abandon the child before RemoveAll.
func (b *BatchRunner) DeleteJob(id string) ([]git.WorktreeDiscard, error) {
	b.mu.Lock()
	var wts []git.WorktreeBinding
	var appID string
	if j := b.jobs[id]; j != nil {
		wts = append(wts, j.Worktrees...)
		appID = j.AppID
	}
	b.dequeueSimLocked(id)
	slot := b.procs[id]
	var startDone, loopDone, reviewDone chan struct{}
	if b.starting != nil {
		startDone = b.starting[id]
	}
	if b.looping != nil {
		loopDone = b.looping[id]
	}
	if b.reviewing != nil {
		reviewDone = b.reviewing[id]
	}
	delete(b.jobs, id)
	delete(b.procs, id)
	var cancel context.CancelFunc
	if slot := b.verifyCancel[id]; slot != nil {
		cancel = slot.cancel
	}
	delete(b.verifyCancel, id)
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if slot != nil {
		slot.kill()
	}
	if slot != nil {
		_ = slot.reap()
	}
	if startDone != nil {
		<-startDone
	}
	// The loop goroutine may be between iterations with no process handle; wait
	// for it to observe the deleted job and return before removing the dir.
	if loopDone != nil {
		<-loopDone
	}
	// After the child is dead, wait until the after-review compare has
	// finished. Discarding the worktree first races that read.
	if reviewDone != nil {
		<-reviewDone
	}
	if b.store != nil {
		analytics.DeleteJobSummary(b.store.DB, id)
	}
	// The agent is dead and no loop can start another, so the worktrees can
	// go: unregistered from each product repo, branch deleted only if merged.
	cleanup := git.DiscardWorktrees(b.trustedBases(appID, wts), b.handoffDir(id))
	ps := b.persistSlot(id)
	ps.Lock()
	err := os.RemoveAll(b.handoffDir(id))
	ps.Unlock()
	b.persistMu.Lock()
	delete(b.persist, id)
	b.persistMu.Unlock()
	return cleanup, err
}

// DeletePreview reports what DeleteJob would do to each worktree — the
// uncommitted files it discards and whether each branch goes — without
// changing anything.
func (b *BatchRunner) DeletePreview(id string) ([]git.WorktreeDiscard, error) {
	j := b.GetJob(id)
	if j == nil {
		return nil, fmt.Errorf("job not found: %s", id)
	}
	return git.PlanDiscard(b.trustedBases(j.AppID, j.Worktrees), b.handoffDir(id)), nil
}

// trustedBases replaces each binding's BaseBranch/BaseRef with the base the
// app's saved settings (SQLite) give that repo, resolved the way batch
// creation does. job.json is agent-writable, and git.DiscardWorktrees treats
// these refs as proof a branch is merged: a BaseRef set to the branch's own
// tip would make unmerged work deletable. A repo the settings no longer list
// keeps no base, so only HEAD and the branch's upstream count.
func (b *BatchRunner) trustedBases(appID string, wts []git.WorktreeBinding) []git.WorktreeBinding {
	var app *store.AppRecord
	if b.store != nil && appID != "" {
		app, _ = store.GetApp(b.store.DB, appID)
	}
	out := make([]git.WorktreeBinding, len(wts))
	for i, w := range wts {
		w.BaseBranch, w.BaseRef = "", ""
		if app != nil {
			for _, e := range app.RepoEntries {
				if e.Name != w.Repo {
					continue
				}
				branch := e.BaseBranch
				if branch == "" {
					branch = store.ParseGitRefName(app.BaseBranch, "main")
				}
				w.BaseBranch, w.BaseRef = branch, branch
				if e.BaseSource != "local" {
					remote := strings.TrimPrefix(store.ParseGitRefName(app.BaseRemote, "origin"), "origin/")
					w.BaseRef = remote + "/" + branch
				}
				break
			}
		}
		out[i] = w
	}
	return out
}

// verifyCancelSlot is one startSpawn invocation's verify context. token is
// pointer identity: a queued call's defer must not cancel the dispatch that
// has since replaced the slot. context.CancelFunc itself is not comparable.
type verifyCancelSlot struct {
	token  *int
	cancel context.CancelFunc
}

func (b *BatchRunner) setVerifyCancel(id string, token *int, cancel context.CancelFunc) {
	b.mu.Lock()
	if b.verifyCancel == nil {
		b.verifyCancel = map[string]*verifyCancelSlot{}
	}
	b.verifyCancel[id] = &verifyCancelSlot{token: token, cancel: cancel}
	b.mu.Unlock()
}

// callVerifyCancel cancels and drops the slot only when token is still the
// one installed. A newer invocation's context is left alone.
func (b *BatchRunner) callVerifyCancel(id string, token *int) {
	b.mu.Lock()
	slot := b.verifyCancel[id]
	if slot == nil || slot.token != token {
		b.mu.Unlock()
		return
	}
	delete(b.verifyCancel, id)
	cancel := slot.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// dropOwnVerifyCancel cancels this invocation's context. If a dispatch has
// already replaced the slot, the map entry stays; only our own func is cancelled.
func (b *BatchRunner) dropOwnVerifyCancel(id string, token *int, cancel context.CancelFunc) {
	b.mu.Lock()
	if slot := b.verifyCancel[id]; slot != nil && slot.token == token {
		delete(b.verifyCancel, id)
	}
	b.mu.Unlock()
	if cancel != nil {
		cancel()
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
	if b.store != nil && batchID != "" {
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
	if b.secrets != nil {
		if secret := b.secrets[j.ID]; secret != "" {
			line = strings.ReplaceAll(line, secret, "[redacted]")
		}
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

// HarvestOpts gates resolve on the post-fix verification run and, when a
// review was required, on that review. Nil opts cannot resolve.
type HarvestOpts struct {
	Verification *VerificationRun
	// Review is the independent pass. Nil, or Required == false, does not
	// gate resolve (failed, cancelled, or skipped). Required && !OK blocks
	// resolve after evidence and notes are imported.
	Review *ReviewRun
	// AllowResolve must be set explicitly for a defect to resolve. The zero
	// value denies: a failed, cancelled or capped run harvests its evidence and
	// notes but resolves nothing, so nil opts can never resolve by accident.
	AllowResolve bool
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

// importCutoff is the stricter cutoff for importing fix images: the start of
// the latest restart, else the job's creation. The driver loop keeps
// harvestCutoff so artifacts a resumed session already wrote still count
// toward completion; only the import, and so resolve, is per run.
func importCutoff(j *Job) time.Time {
	if j != nil && j.RunStartedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, j.RunStartedAt); err == nil {
			return t
		}
	}
	return harvestCutoff(j)
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
	allowResolve := false
	if opts != nil {
		verification = opts.Verification
		allowResolve = opts.AllowResolve
	}
	resolveDenyReason := "this run may not resolve (failed, cancelled, or incomplete artifacts)"
	fixRoot := filepath.Join(handoff, "fix-evidence")
	notesRoot := filepath.Join(handoff, "fix-notes")
	imported := 0
	resolved := 0
	cutoff := harvestCutoff(job)
	imgCutoff := importCutoff(job)
	recorded := []HarvestDefect{}
	unfixed, notesUnreadable := notesForHarvest(handoff, job.DefectIDs, cutoff)

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
				// Shared with artifactsComplete so the driver loop can never
				// stop on a file this harvest would then skip. DirEntry.IsDir
				// follows the link, so a symlinked directory would read as a
				// file here; readRegularFile is the real gate.
				if !harvestableImageName(name, e.IsDir()) {
					continue
				}
				data, tooLarge, ok := readRegularFile(handoff, filepath.Join(imgDir, name), imgCutoff)
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
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNoEvidence})
			continue
		}
		cur, err := b.store.Get(defectID)
		if err != nil || cur == nil {
			reason := "defect not found"
			if err != nil {
				reason = err.Error()
			}
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestError, Reason: reason})
			continue
		}
		var saved []string
		if len(files) > 0 {
			added, err := b.store.SaveEvidence(defectID, files, "fix")
			if err != nil {
				b.appendLog(job, "harvest "+defectID+" failed: "+err.Error(), "error", "DefectDrainer")
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestError, Reason: err.Error()})
				continue
			}
			saved = added
			cur.FixEvidence = append(cur.FixEvidence, added...)
			written, err := b.store.Write(*cur)
			if err != nil {
				b.appendLog(job, "harvest "+defectID+" failed: "+err.Error(), "error", "DefectDrainer")
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestError, Reason: err.Error(), Images: saved})
				continue
			}
			cur = &written
			imported += len(files)
			b.appendLog(job, fmt.Sprintf("harvest %s: %d fix image(s) → evidence/", defectID, len(files)), "info", "DefectDrainer")
		}

		// Block on what THIS job broke or could not run — not on red that
		// was already there before it started (see judgeVerification).
		verdict := judgeVerification(verification, job.Baseline)
		if verification != nil && verification.Ran && !verdict.Ok {
			why := summarizeVerdict(verdict)
			b.appendLog(job, "harvest "+defectID+": NOT resolved — "+why, "warn", "DefectDrainer")
			if len(files) == 0 {
				recorded = append(recorded, HarvestDefect{
					DefectID: defectID, Outcome: harvestNotesOnly,
					Reason: "verification: " + why + "; note not saved",
				})
			} else {
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestRegression, Reason: why, Images: saved})
			}
			continue
		}

		// Resolve on evidence imported by THIS run. Historical fix_evidence on
		// the defect is not proof that this job fixed anything — accepting it
		// is how a job with notes and a screenshot from a previous run used to
		// resolve a defect it never touched.
		// A required review that is not ok still keeps the images and the note,
		// and resolves nothing. suggested_fix is not a command.
		// UNFIXED is a full line in NOTES.md. DEF-1 must not decline DEF-10;
		// notesMarkers matches the line exactly. A NOTES.md that exists but
		// cannot be read is not an empty marker list: nothing in the job
		// resolves, because a decline inside it would have been invisible.
		if len(files) > 0 && allowResolve && notesUnreadable {
			if note != "" {
				_, _ = b.store.Update(defectID, store.DefectRecord{Resolution: note}, map[string]bool{"resolution": true})
			}
			why := "decline file could not be read"
			b.appendLog(job, "harvest "+defectID+": NOT resolved — "+why, "warn", "DefectDrainer")
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNotResolved, Reason: why, Images: saved})
			continue
		}
		if len(files) > 0 && allowResolve && unfixed[defectID] {
			if note != "" {
				_, _ = b.store.Update(defectID, store.DefectRecord{Resolution: note}, map[string]bool{"resolution": true})
			}
			why := "declined (UNFIXED)"
			b.appendLog(job, "harvest "+defectID+": NOT resolved — "+why, "warn", "DefectDrainer")
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNotResolved, Reason: why, Images: saved})
			continue
		}
		var reviewWhy string
		if opts != nil {
			reviewWhy = reviewBlockReason(opts.Review)
		}
		if len(files) > 0 && allowResolve && reviewWhy != "" {
			if note != "" {
				_, _ = b.store.Update(defectID, store.DefectRecord{Resolution: note}, map[string]bool{"resolution": true})
			}
			b.appendLog(job, "harvest "+defectID+": NOT resolved — "+reviewWhy, "warn", "DefectDrainer")
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestReviewBlocked, Reason: reviewWhy, Images: saved})
			continue
		}
		if len(files) > 0 && allowResolve {
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
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestError, Reason: err.Error(), Images: saved})
				continue
			}
			resolved++
			b.appendLog(job, "harvest "+defectID+": resolved with fix_evidence", "info", "DefectDrainer")
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestResolved, Images: saved})
		} else if note != "" {
			_, _ = b.store.Update(defectID, store.DefectRecord{Resolution: note}, map[string]bool{"resolution": true})
			if len(files) > 0 {
				b.appendLog(job, "harvest "+defectID+": fix image(s) imported but NOT resolved — "+resolveDenyReason, "warn", "DefectDrainer")
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNotResolved, Reason: resolveDenyReason, Images: saved})
			} else {
				b.appendLog(job, "harvest "+defectID+": notes only (no images) — still needs screenshots", "warn", "DefectDrainer")
				recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNotesOnly})
			}
		} else if len(files) > 0 {
			// No note, so the warn line above is not emitted. The images were
			// still imported and the run may not resolve.
			recorded = append(recorded, HarvestDefect{DefectID: defectID, Outcome: harvestNotResolved, Reason: resolveDenyReason, Images: saved})
		}
	}
	b.appendLog(job, fmt.Sprintf("harvest done: %d image(s), %d/%d resolved", imported, resolved, len(job.DefectIDs)), "info", "DefectDrainer")
	b.recordHarvest(job.ID, recorded)
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
	if job.isTestRun() {
		_ = b.writeTestHandoff(job, filepath.Join(paths.BatchJobsDir(b.dataRoot), job.ID), job.Error)
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
	ee, ok := err.(*exec.ExitError)
	if !ok {
		// A child that never started has no exit status; its reason is the
		// only useful thing to show (e.g. a refused sandbox profile).
		return err.Error()
	}
	if ee.ProcessState != nil {
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
// loopBeforeNextHook runs in the driver loop after an iteration is reaped and
// the decision to continue is made, but before the next child starts — the gap
// where no process handle exists. Tests use it to make Stop/Delete land there
// deterministically instead of racing a sleep.
// spawnExitHook runs when the completion goroutine returns.
var (
	startSpawnHook           func()
	startSpawnAfterStartHook func()
	harvestStartHook         func()
	loopBeforeNextHook       func()
	spawnExitHook            func()
	// reviewLensStartedHook runs on the review goroutine after that lens's
	// child is in procs and before Wait. DeleteJob reaps the child, so a hook
	// that calls it has to do so from another goroutine.
	reviewLensStartedHook func(lens string)
	// reviewLensFinishedHook runs after that lens has been written to job.json.
	reviewLensFinishedHook func(lens string)
	// simLeaseQueuedHook runs after a job is queued and before that startSpawn
	// returns, so a test can promote the waiter before the caller's defer.
	simLeaseQueuedHook func()
	// simLeaseBeforeResumeProbeHook runs before a dispatched Resume looks up
	// its grok session.
	simLeaseBeforeResumeProbeHook func()
)

func jobGoneOrCancelled(j *Job) bool {
	return j == nil || j.Status == "cancelled"
}

func abandonStartedAgent(cmd *exec.Cmd, flush func()) {
	if cmd != nil && cmd.Process != nil {
		pid := cmd.Process.Pid
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		git.KillImplementerGroup(pid)
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
	return b.startSpawn(jobID, defectsRoot, spawnFresh, "")
}

// startSpawn is StartSpawn with a mode. A fresh spawn measures baseline and
// diff base; rerun and resume reuse the first run's — the worktree now holds
// the agent's edits, so re-measuring would compare the agent against itself —
// and add to the stored agentRun. resumeSession, when set, is the grok
// session iteration 1 continues (-r).
//
// An app with AllowSimulatorWrites takes the one host simulator lease here.
// If another job holds it, this job is persisted as queued and the call
// returns nil. The lease moves on when the holder's agent loop ends, or
// sooner when the holder never launches.
func (b *BatchRunner) startSpawn(jobID, defectsRoot string, mode spawnMode, resumeSession string) error {
	// These returns are before the launch defer. A handover may already have
	// named this job the holder; give the lease up if we are not going to run.
	bail := func(err error) error {
		b.mu.Lock()
		held := b.simHolder == jobID
		b.mu.Unlock()
		if held {
			b.releaseSimLease(jobID)
		}
		return err
	}
	b.mu.Lock()
	if defectsRoot != "" {
		b.defectsRoot = defectsRoot
	}
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return bail(fmt.Errorf("job not found: %s", jobID))
	}
	if jobGoneOrCancelled(j) {
		b.mu.Unlock()
		return bail(nil)
	}
	if len(j.Worktrees) == 0 {
		b.mu.Unlock()
		return bail(fmt.Errorf("refusing to spawn agent batch fix without worktrees"))
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	handoff := filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID)
	batchID := j.BatchID
	appID := j.AppID
	jobCopy := *j
	strat := strategyFor(&jobCopy)
	label := batchID
	if jobCopy.isTestRun() {
		label = jobLabel(&jobCopy)
	}
	done := make(chan struct{})
	if b.starting == nil {
		b.starting = map[string]chan struct{}{}
	}
	b.starting[jobID] = done
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	token := new(int)
	b.setVerifyCancel(jobID, token, cancel)
	launched := false
	// queuedHere: this call parked the job. A handover may promote it before
	// the defer runs; releasing or cancelling here would steal that dispatch.
	queuedHere := false
	defer func() {
		if !launched {
			b.clearSecret(jobID)
			if queuedHere {
				b.dropOwnVerifyCancel(jobID, token, cancel)
			} else {
				b.callVerifyCancel(jobID, token)
				b.releaseSimLease(jobID)
			}
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

	if err := strat.writeHandoff(b, &jobCopy, defectsRoot, handoff); err != nil {
		return err
	}
	b.mu.Lock()
	if jobGoneOrCancelled(b.jobs[jobID]) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	if b.store == nil || b.store.DB == nil {
		return fmt.Errorf("read app %s: store is not open", appID)
	}
	app, err := store.GetApp(b.store.DB, appID)
	if err != nil {
		// A failed read must not look like "flag clear" and skip the lease.
		return fmt.Errorf("read app %s: %w", appID, err)
	}
	sandbox := "strict"
	toolchainKind := "none"
	simWrites := false
	maxTurns := 0
	var verifyCommands []store.VerifyCommand
	var repoEntries []store.AppRepoEntry
	if app != nil {
		sandbox = store.ParseGrokSandbox(app.GrokSandbox)
		toolchainKind = store.ParseAgentToolchain(app.AgentToolchain)
		simWrites = app.AllowSimulatorWrites
		maxTurns = store.NormalizeAgentMaxTurns(app.AgentMaxTurns)
		verifyCommands = store.NormalizeVerifyCommands(app.VerifyCommands)
		repoEntries = app.RepoEntries
	}
	strat.prepare(handoff)
	turnsNote := "default"
	if maxTurns > 0 {
		turnsNote = strconv.Itoa(maxTurns)
	}
	b.appendLog(&jobCopy, fmt.Sprintf("app %s grok_sandbox=%s agent_toolchain=%s agent_max_turns=%s", appID, sandbox, toolchainKind, turnsNote), "info", "DefectDrainer")

	// Fail closed: a store error is not "no secret names". Baseline and the
	// toolchain probe use this list, so it is resolved before either runs.
	stripNames, stripErr := b.secretEnvNames()
	if stripErr != nil {
		return stripErr
	}

	// One lease for the whole host, shared by every app that writes to the
	// booted simulator. Apps without the flag never wait.
	if simWrites && !b.holdOrQueueSim(jobID, mode, defectsRoot) {
		queuedHere = true
		if simLeaseQueuedHook != nil {
			simLeaseQueuedHook()
		}
		return nil
	}

	spawnLog := func(line, level string) {
		b.appendLog(&Job{ID: jobID}, line, level, "DefectDrainer")
	}
	provisioned := NO_TOOLCHAIN
	if toolchainKind == "flutter" {
		provisioned = provisionFlutterToolchain(handoff, wts, spawnLog, stripNames)
	}

	var sandboxProfile string
	var profileBody []byte
	var profileNotes []string
	if simWrites {
		profile, err := writeSimulatorSandboxProfile(handoff, sandbox, label)
		if err != nil {
			spawnLog("sandbox: job profile failed — skipped ("+err.Error()+")", "warn")
		} else {
			sandboxProfile = profile.Profile
			profileBody = profile.Body
			profileNotes = profile.Notes
			spawnLog("sandbox: job profile '"+profile.Profile+"' extends "+sandbox+" + Simulator device writes", "info")
		}
	}

	diffBaseByRepo := map[string]string{}
	for _, w := range wts {
		if sha := jobCopy.DiffBase[w.Repo]; sha != "" {
			diffBaseByRepo[w.Repo] = sha
		} else {
			diffBaseByRepo[w.Repo] = worktreeHead(w.WorktreeAbs)
		}
	}
	realByRepo := snapshotWorktreeReals(wts)
	gitDirByRepo := snapshotGitDirs(wts)

	var baseline *VerificationRun
	if strat.verifies() {
		if mode != spawnFresh {
			baseline = jobCopy.Baseline
			if baseline != nil {
				spawnLog("baseline: reusing the first run's (measured before any agent edits)", "info")
			} else if len(verifyCommands) > 0 {
				spawnLog("baseline: none stored — skipped; verify judges without one, so every red command blocks", "warn")
			}
		} else if len(verifyCommands) > 0 {
			run := runVerification(ctx, verifyCommands, wts, filepath.Join(handoff, "baseline"), repoEntries, provisioned.VerifyEnv, realByRepo, func(line, level string) {
				spawnLog("baseline "+line, level)
			}, stripNames)
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
	acc := &agentRunAcc{}
	// Re-run and Resume add to the job's record. The caps below still count
	// this run's iterations only, so every restart gets a fresh budget.
	priorIters, resumes := 0, 0
	if mode != spawnFresh && jobCopy.AgentRun != nil {
		acc.seed(jobCopy.AgentRun)
		priorIters, resumes = jobCopy.AgentRun.Iterations, jobCopy.AgentRun.Resumes
	}
	if mode == spawnResume {
		resumes++
	}
	runRecord := func(iter int, stoppedBy string, missing []string) *AgentRun {
		run := acc.snapshot(priorIters+iter, stoppedBy, missing)
		run.Resumes = resumes
		return run
	}
	// Every iteration uses identical argv except for -c + the continue prompt,
	// so the sandbox and turn budget cannot drift between them.
	var secret string
	startIter := func(resume bool, session string) (*exec.Cmd, func(), error) {
		// The previous iteration ran with this handoff as its writable cwd,
		// and grok re-reads the profile on every start.
		if sandboxProfile != "" {
			if err := checkSimulatorSandboxProfile(handoff, profileBody); err != nil {
				return nil, nil, err
			}
		}
		names, nerr := b.secretEnvNames()
		if nerr != nil {
			return nil, nil, nerr
		}
		extra := map[string]string{}
		for k, v := range provisioned.Env {
			extra[k] = v
		}
		opts := &git.CodingAgentOpts{
			ExtraEnv:       extra,
			SandboxProfile: sandboxProfile,
			ToolchainNotes: notes,
			VerifyCommands: verifySpecs,
			AlreadyFailing: alreadyFailingLines(baseline),
			MaxTurns:       maxTurns,
			Continue:       resume,
			ResumeSession:  session,
			StripEnv:       names,
			OnEnd: func(e git.AgentEnd) {
				acc.end(e.StopReason, e.SessionID, e.NumTurns, e.TotalCostUSD)
			},
			OnMaxTurns: acc.maxTurnsReached,
		}
		if prompt, cont := strat.prompts(b, &jobCopy, handoff, defectsRoot, sandbox, wts, opts); prompt != "" || cont != "" {
			opts.Prompt = prompt
			opts.ContinuePrompt = cont
		}
		if !strat.verifies() {
			opts.VerifyCommands = nil
			opts.AlreadyFailing = nil
		}
		if secret != "" {
			opts.Secret = secret
			opts.ExtraEnv["DD_TEST_ACCOUNT_PASSWORD"] = secret
		}
		return git.StartCodingAgent(handoff, label, defectsRoot, wts, sandbox, onStdout, onStderr, opts)
	}
	var serr error
	secret, serr = b.readTestSecret(&jobCopy)
	if serr != nil {
		return bail(Conflict(serr.Error()))
	}
	if secret != "" {
		b.setSecret(jobID, secret)
	}
	// Running hits disk before the child exists. A kill after this rename
	// hydrates as interrupted; a kill before it leaves queued and no process.
	b.mu.Lock()
	live := b.jobs[jobID]
	if jobGoneOrCancelled(live) {
		b.mu.Unlock()
		return nil
	}
	live.Status = "running"
	live.Error = ""
	if live.DiffBase == nil {
		live.DiffBase = diffBaseByRepo
	}
	spawnLine := fmt.Sprintf(
		"spawning batch fix: %s — %s sandbox=%s cwd=%s (%d worktree(s))",
		bin, batchID, sandbox, handoff, len(wts),
	)
	if jobCopy.isTestRun() {
		spawnLine = fmt.Sprintf(
			"spawning test run: %s — %s sandbox=%s cwd=%s (%d worktree(s))",
			bin, label, sandbox, handoff, len(wts),
		)
	}
	b.appendLogLocked(live, spawnLine, "info", "DefectDrainer")
	for _, w := range wts {
		if w.Branch != "" {
			b.appendLogLocked(live, fmt.Sprintf("worktree %s → %s (%s)", w.Repo, w.WorktreeAbs, w.Branch), "info", "DefectDrainer")
		} else {
			b.appendLogLocked(live, fmt.Sprintf("worktree %s → %s (detached %s)", w.Repo, w.WorktreeAbs, diffBaseByRepo[w.Repo]), "info", "DefectDrainer")
		}
	}
	b.mu.Unlock()
	if err := b.persistLive(jobID); err != nil {
		return err
	}
	if err := b.markTestRunStarted(jobID); err != nil {
		return err
	}
	b.mu.Lock()
	if jobGoneOrCancelled(b.jobs[jobID]) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	cmd, flush, err := startIter(false, resumeSession)
	if err != nil {
		return err
	}
	// Previous-run hygiene stays until this child exists. A sim-lease queue
	// returns before startIter, and a failed start must not wipe it.
	if jobCopy.isTestRun() {
		b.mu.Lock()
		if live := b.jobs[jobID]; live != nil && !jobGoneOrCancelled(live) {
			live.Verification = nil
			live.DiffHygiene = nil
		}
		b.mu.Unlock()
		_ = b.persistLive(jobID)
	}
	// The wall clock starts here — at iter-1 Start, not at StartSpawn. Baseline
	// is 20 min per command on its own clock and must not eat the agent budget.
	loopStart := time.Now()
	maxIters := parsePositiveInt(env.EnvDrainer("AGENT_MAX_ITERS"), defaultAgentMaxIters)
	maxWall := time.Duration(parsePositiveInt(env.EnvDrainer("AGENT_MAX_WALL_SEC"), defaultAgentMaxWallSec)) * time.Second
	if startSpawnAfterStartHook != nil {
		startSpawnAfterStartHook()
	}
	b.mu.Lock()
	live = b.jobs[jobID]
	if jobGoneOrCancelled(live) {
		b.mu.Unlock()
		abandonStartedAgent(cmd, flush)
		return nil
	}
	proc := &spawnedProc{cmd: cmd}
	b.procs[jobID] = proc
	b.mu.Unlock()
	launched = true
	loopDone := make(chan struct{})
	b.mu.Lock()
	if b.looping == nil {
		b.looping = map[string]chan struct{}{}
	}
	b.looping[jobID] = loopDone
	if b.driving == nil {
		b.driving = map[string]bool{}
	}
	b.driving[jobID] = true
	b.mu.Unlock()
	go func() {
		// endLoop releases DeleteJob as soon as no further child can start. It
		// deliberately does NOT span verify and harvest: the gap this guards is
		// between reaping one iteration and starting the next, and holding it
		// through verify would block DeleteJob behind commands that are allowed
		// 20 minutes each — and deadlock against a harvest that waits on the
		// deleting goroutine.
		var closeLoopOnce sync.Once
		endLoop := func() {
			closeLoopOnce.Do(func() {
				b.mu.Lock()
				if b.looping[jobID] == loopDone {
					delete(b.looping, jobID)
				}
				b.mu.Unlock()
				close(loopDone)
				// Verify and harvest stay on this goroutine. The next simulator
				// job may start while they are still running.
				b.releaseSimLease(jobID)
			})
		}
		defer func() {
			endLoop()
			b.clearSecret(jobID)
			// Cancel before releasing driving: once it clears, a Restart may
			// install the next run's cancel under the same id.
			b.callVerifyCancel(jobID, token)
			b.mu.Lock()
			delete(b.driving, jobID)
			b.mu.Unlock()
			if spawnExitHook != nil {
				spawnExitHook()
			}
		}()

		// Driver loop: completion is the artifacts BRIEF.md demands, not the
		// child's exit code (K6). A non-zero exit carrying max_turns_reached is
		// turn exhaustion and resumes; anything else non-zero is a real failure.
		curProc, curFlush := proc, flush
		iter := 1
		stoppedBy := ""
		var missing []string
		var exitErr error
		cutoff := harvestCutoff(&jobCopy)
		for {
			exitErr = curProc.reap()
			if curFlush != nil {
				curFlush()
			}
			sawMaxTurns := acc.takeMaxTurns()
			b.mu.Lock()
			delete(b.procs, jobID)
			gone := jobGoneOrCancelled(b.jobs[jobID])
			b.mu.Unlock()
			if gone {
				return
			}

			complete, miss := strat.complete(b, handoff, &jobCopy, sandbox, cutoff)
			missing = miss
			stoppedBy = nextLoopStop(iter, maxIters, time.Since(loopStart), maxWall, exitErr, sawMaxTurns, complete)

			// Persist after every iteration so a killed loop still leaves its
			// cost and stop reasons on disk.
			b.mu.Lock()
			if live := b.jobs[jobID]; live != nil && !jobGoneOrCancelled(live) {
				live.AgentRun = runRecord(iter, stoppedBy, missing)
				b.appendLogLocked(live, loopIterationLine(priorIters+iter, exitErr, sawMaxTurns, complete, stoppedBy, missing), loopLevel(stoppedBy), "DefectDrainer")
			}
			b.mu.Unlock()
			b.persistLive(jobID)
			if stoppedBy != "" {
				break
			}

			iter++
			if loopBeforeNextHook != nil {
				loopBeforeNextHook()
			}
			b.mu.Lock()
			gone = jobGoneOrCancelled(b.jobs[jobID])
			b.mu.Unlock()
			if gone {
				return
			}
			nextCmd, nextFlush, nerr := startIter(true, "")
			if nerr != nil {
				exitErr = nerr
				stoppedBy = StoppedByExit
				b.mu.Lock()
				if live := b.jobs[jobID]; live != nil && !jobGoneOrCancelled(live) {
					live.AgentRun = runRecord(iter, stoppedBy, missing)
					b.appendLogLocked(live, "agent iteration "+strconv.Itoa(priorIters+iter)+" failed to start: "+nerr.Error(), "error", "DefectDrainer")
				}
				b.mu.Unlock()
				b.persistLive(jobID)
				break
			}
			b.mu.Lock()
			if jobGoneOrCancelled(b.jobs[jobID]) {
				b.mu.Unlock()
				abandonStartedAgent(nextCmd, nextFlush)
				return
			}
			curProc = &spawnedProc{cmd: nextCmd}
			b.procs[jobID] = curProc
			curFlush = nextFlush
			b.mu.Unlock()
		}

		// The loop is over; no further child will start, so a concurrent
		// DeleteJob may proceed while verify and harvest run.
		endLoop()

		failed := stoppedBy == StoppedByExit
		b.mu.Lock()
		live := b.jobs[jobID]
		if jobGoneOrCancelled(live) {
			b.mu.Unlock()
			return
		}
		if failed {
			// The error text is visible while the job is still running.
			// Status stays running until finishTerminal, after afterLoop's
			// last write: a client that sees failed is not racing job.json.
			live.Error = formatGrokExit(exitErr)
			b.appendLogLocked(live, live.Error, "error", "DefectDrainer")
		} else {
			live.Error = ""
		}
		b.mu.Unlock()
		b.persistLive(jobID)
		strat.afterLoop(b, loopFinish{
			jobID: jobID, handoff: handoff, failed: failed, ctx: ctx,
			wts: wts, diffBase: diffBaseByRepo, verifyCmds: verifyCommands,
			repoEntries: repoEntries, verifyEnv: provisioned.VerifyEnv,
			stripEnv: stripNames, realByRepo: realByRepo, gitDirByRepo: gitDirByRepo, baseline: baseline, log: spawnLog,
		})
		if failed {
			b.finishTerminal(jobID, "failed", "")
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
	// A queued job has no agent. Drop it before anyone can hand it the lease.
	// A running holder keeps the lease until its loop ends.
	b.dequeueSimLocked(jobID)
	slot := b.procs[jobID]
	j.Status = "cancelled"
	j.Error = "stopped by operator"
	b.appendLogLocked(j, "stop requested by operator (web UI)", "warn", "DefectDrainer")
	if slot == nil {
		b.appendLogLocked(j, "no live process handle (may still be starting)", "warn", "DefectDrainer")
	}
	var cancel context.CancelFunc
	if slot := b.verifyCancel[jobID]; slot != nil {
		cancel = slot.cancel
	}
	delete(b.verifyCancel, jobID)
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.persistLive(jobID)
	if slot != nil && slot.group {
		slot.kill()
	} else if slot != nil && slot.cmd != nil && slot.cmd.Process != nil {
		_ = slot.cmd.Process.Signal(os.Interrupt)
		go func() {
			time.Sleep(3 * time.Second)
			_ = slot.cmd.Process.Kill()
		}()
	}
	if !j.isTestRun() {
		b.SyncBatchAndDefects(j, "cancelled")
	}
	return b.GetJob(jobID), nil
}

// ShipPreview lists uncommitted files a local Merge would commit.
func (b *BatchRunner) ShipPreview(jobID string) (git.ShipPreview, error) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return git.ShipPreview{}, fmt.Errorf("job not found: %s", jobID)
	}
	if j.isTestRun() {
		b.mu.Unlock()
		return git.ShipPreview{}, Conflict("test run jobs do not ship")
	}
	wts := append([]git.WorktreeBinding(nil), j.Worktrees...)
	batchID := j.BatchID
	b.mu.Unlock()
	var titles []string
	if rec, err := b.GetBatch(batchID); err == nil && rec != nil && b.store != nil {
		for _, id := range rec.DefectIDs {
			d, derr := b.store.Get(id)
			if derr != nil || d == nil {
				continue
			}
			titles = append(titles, d.Title)
		}
	}
	message := suggestShipMessage(titles, b.handoffDir(jobID))
	if message == "" {
		message = "Fix defects for " + batchID
	}
	return git.PreviewShip(wts, message)
}

// CreatePRs runs gh pr create or a local merge for each worktree and writes job.prs[].
// ship.Commit commits uncommitted files on local worktrees first.
func (b *BatchRunner) CreatePRs(jobID string, ship git.ShipRequest) (*Job, error) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	if j.isTestRun() {
		b.mu.Unlock()
		return nil, Conflict("test run jobs do not ship")
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
	b.appendLog(j, fmt.Sprintf("Create PR / merge: %d worktree(s)…", len(wts)), "info", "DefectDrainer")
	raw := git.CreatePRsForWorktrees(wts, j.BatchID, title, ship)
	prs := make([]PR, 0, len(raw))
	for _, p := range raw {
		prs = append(prs, prFromResult(p))
	}
	updated := b.UpdatePRs(jobID, prs)
	landed, failed, skipped := 0, 0, 0
	for _, p := range prs {
		switch p.Status {
		case "created", "existing", "merged":
			landed++
		case "failed":
			failed++
		default:
			skipped++
		}
	}
	if updated != nil {
		b.appendLog(updated, fmt.Sprintf("Create PR / merge done: %d landed, %d failed, %d skipped", landed, failed, skipped), "info", "DefectDrainer")
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
	if j.isTestRun() {
		b.mu.Unlock()
		return nil, Conflict("test run jobs do not ship")
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
		if p.URL != "" || p.Status == "created" || p.Status == "existing" || p.Status == "merged" || p.Kind == "merge" {
			return true
		}
	}
	return false
}
