package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/paths"
)

// RunRef is a repo revision recorded on a test run.
type RunRef struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha,omitempty"`
}

// RunProgress counts step statuses. Total is the step count.
type RunProgress struct {
	Total      int `json:"total"`
	Pending    int `json:"pending"`
	Pass       int `json:"pass"`
	Fail       int `json:"fail"`
	Blocked    int `json:"blocked"`
	Skipped    int `json:"skipped"`
	Unverified int `json:"unverified"`
}

// RunStep is one snapshotted or exploratory step on a run.
type RunStep struct {
	Key         string   `json:"key"`
	Position    int      `json:"position"`
	Section     string   `json:"section"`
	Action      string   `json:"action"`
	Expected    string   `json:"expected"`
	Exploratory bool     `json:"exploratory"`
	Status      string   `json:"status"`
	Actual      string   `json:"actual"`
	Evidence    []string `json:"evidence"`
	DefectID    *string  `json:"defect_id"`
	Review      string   `json:"review"`
	ReviewNote  string   `json:"review_note"`
	UpdatedAt   string   `json:"updated_at"`
}

// TestRun is the run JSON object. List responses omit Steps and keep Progress.
type TestRun struct {
	ID          string         `json:"id"`
	AppID       string         `json:"app_id"`
	PlanID      *string        `json:"plan_id"`
	PlanVersion *int           `json:"plan_version"`
	Title       string         `json:"title"`
	Executor    string         `json:"executor"`
	ExecutedBy  string         `json:"executed_by"`
	JobID       *string        `json:"job_id"`
	Env         map[string]any `json:"env"`
	Refs        []RunRef       `json:"refs"`
	Status      string         `json:"status"`
	ReviewedBy  string         `json:"reviewed_by"`
	ReviewedAt  *string        `json:"reviewed_at"`
	ReviewNote  string         `json:"review_note"`
	StartedAt   string         `json:"started_at"`
	FinishedAt  *string        `json:"finished_at"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	Progress    RunProgress    `json:"progress"`
	Steps       []RunStep      `json:"steps,omitempty"`
	// omitSteps selects the list shape, which has progress and no steps key.
	omitSteps bool
}

// MarshalJSON emits steps on a detail run, including an empty list, and omits
// them on a list row.
func (r TestRun) MarshalJSON() ([]byte, error) {
	env := r.Env
	if env == nil {
		env = map[string]any{}
	}
	refs := r.Refs
	if refs == nil {
		refs = []RunRef{}
	}
	head := struct {
		ID          string         `json:"id"`
		AppID       string         `json:"app_id"`
		PlanID      *string        `json:"plan_id"`
		PlanVersion *int           `json:"plan_version"`
		Title       string         `json:"title"`
		Executor    string         `json:"executor"`
		ExecutedBy  string         `json:"executed_by"`
		JobID       *string        `json:"job_id"`
		Env         map[string]any `json:"env"`
		Refs        []RunRef       `json:"refs"`
		Status      string         `json:"status"`
		ReviewedBy  string         `json:"reviewed_by"`
		ReviewedAt  *string        `json:"reviewed_at"`
		ReviewNote  string         `json:"review_note"`
		StartedAt   string         `json:"started_at"`
		FinishedAt  *string        `json:"finished_at"`
		CreatedAt   string         `json:"created_at"`
		UpdatedAt   string         `json:"updated_at"`
		Progress    RunProgress    `json:"progress"`
	}{
		ID: r.ID, AppID: r.AppID, PlanID: r.PlanID, PlanVersion: r.PlanVersion,
		Title: r.Title, Executor: r.Executor, ExecutedBy: r.ExecutedBy, JobID: r.JobID,
		Env: env, Refs: refs, Status: r.Status, ReviewedBy: r.ReviewedBy, ReviewedAt: r.ReviewedAt,
		ReviewNote: r.ReviewNote, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Progress: r.Progress,
	}
	if r.omitSteps {
		return json.Marshal(head)
	}
	steps := r.Steps
	if steps == nil {
		steps = []RunStep{}
	}
	return json.Marshal(struct {
		ID          string         `json:"id"`
		AppID       string         `json:"app_id"`
		PlanID      *string        `json:"plan_id"`
		PlanVersion *int           `json:"plan_version"`
		Title       string         `json:"title"`
		Executor    string         `json:"executor"`
		ExecutedBy  string         `json:"executed_by"`
		JobID       *string        `json:"job_id"`
		Env         map[string]any `json:"env"`
		Refs        []RunRef       `json:"refs"`
		Status      string         `json:"status"`
		ReviewedBy  string         `json:"reviewed_by"`
		ReviewedAt  *string        `json:"reviewed_at"`
		ReviewNote  string         `json:"review_note"`
		StartedAt   string         `json:"started_at"`
		FinishedAt  *string        `json:"finished_at"`
		CreatedAt   string         `json:"created_at"`
		UpdatedAt   string         `json:"updated_at"`
		Progress    RunProgress    `json:"progress"`
		Steps       []RunStep      `json:"steps"`
	}{
		ID: head.ID, AppID: head.AppID, PlanID: head.PlanID, PlanVersion: head.PlanVersion,
		Title: head.Title, Executor: head.Executor, ExecutedBy: head.ExecutedBy, JobID: head.JobID,
		Env: head.Env, Refs: head.Refs, Status: head.Status, ReviewedBy: head.ReviewedBy,
		ReviewedAt: head.ReviewedAt, ReviewNote: head.ReviewNote, StartedAt: head.StartedAt,
		FinishedAt: head.FinishedAt, CreatedAt: head.CreatedAt, UpdatedAt: head.UpdatedAt,
		Progress: head.Progress, Steps: steps,
	})
}

// CreateRunInput is POST /api/test-runs. Execute must be "manual".
type CreateRunInput struct {
	AppID        string
	PlanID       string
	Title        string
	Execute      string
	Executor     string
	ExecutedBy   string
	AccountLabel string
	Env          map[string]any
	Refs         []RunRef
}

// ReviewStepIn is one per-step review.
type ReviewStepIn struct {
	Key    string
	Review string
	Note   string
}

// ReviewInput is POST /api/test-runs/{id}/review.
type ReviewInput struct {
	Reviewer string
	Note     string
	Steps    []ReviewStepIn
}

// SaveAsPlanInput replaces PlanID's steps, or creates a plan from Key and Title.
type SaveAsPlanInput struct {
	PlanID string
	Key    string
	Title  string
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

const runCols = `id, app_id, plan_id, plan_version, title, executor, executed_by, job_id, env_json, refs_json, status, reviewed_by, reviewed_at, review_note, started_at, finished_at, created_at, updated_at`

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	s := n.String
	return &s
}

func nullIntPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func parseRunRefs(raw string) []RunRef {
	if strings.TrimSpace(raw) == "" {
		return []RunRef{}
	}
	var refs []RunRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil || refs == nil {
		return []RunRef{}
	}
	return refs
}

func scanRun(sc interface{ Scan(...any) error }) (TestRun, error) {
	var r TestRun
	var planID, jobID, reviewedAt, finishedAt sql.NullString
	var planVersion sql.NullInt64
	var envJSON, refsJSON string
	if err := sc.Scan(
		&r.ID, &r.AppID, &planID, &planVersion, &r.Title, &r.Executor, &r.ExecutedBy, &jobID,
		&envJSON, &refsJSON, &r.Status, &r.ReviewedBy, &reviewedAt, &r.ReviewNote,
		&r.StartedAt, &finishedAt, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return TestRun{}, err
	}
	r.PlanID = nullStringPtr(planID)
	r.PlanVersion = nullIntPtr(planVersion)
	r.JobID = nullStringPtr(jobID)
	r.ReviewedAt = nullStringPtr(reviewedAt)
	r.FinishedAt = nullStringPtr(finishedAt)
	r.Env = unmarshalEnv(envJSON)
	r.Refs = parseRunRefs(refsJSON)
	return r, nil
}

func loadRunSteps(q rower, runID string) ([]RunStep, error) {
	rows, err := q.Query(`SELECT step_key, position, section, action, expected, exploratory, status, actual, evidence_json, defect_id, review, review_note, updated_at
		FROM test_step_results WHERE run_id = ? ORDER BY position ASC, step_key ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunStep{}
	for rows.Next() {
		var s RunStep
		var expl int
		var evJSON string
		var defect sql.NullString
		if err := rows.Scan(&s.Key, &s.Position, &s.Section, &s.Action, &s.Expected, &expl, &s.Status, &s.Actual, &evJSON, &defect, &s.Review, &s.ReviewNote, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Exploratory = expl != 0
		s.Evidence = db.ParseJSONArray(evJSON)
		if s.Evidence == nil {
			s.Evidence = []string{}
		}
		s.DefectID = nullStringPtr(defect)
		out = append(out, s)
	}
	return out, rows.Err()
}

func progressFrom(steps []RunStep) RunProgress {
	var p RunProgress
	p.Total = len(steps)
	for _, s := range steps {
		switch s.Status {
		case "pending":
			p.Pending++
		case "pass":
			p.Pass++
		case "fail":
			p.Fail++
		case "blocked":
			p.Blocked++
		case "skipped":
			p.Skipped++
		case "unverified":
			p.Unverified++
		}
	}
	return p
}

func loadRun(q rower, id string, withSteps bool) (TestRun, error) {
	if !IsSafeRunID(id) {
		return TestRun{}, notFound("test run not found")
	}
	r, err := scanRun(q.QueryRow(`SELECT `+runCols+` FROM test_runs WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return TestRun{}, notFound("test run not found")
	}
	if err != nil {
		return TestRun{}, err
	}
	steps, err := loadRunSteps(q, id)
	if err != nil {
		return TestRun{}, err
	}
	r.Progress = progressFrom(steps)
	if withSteps {
		r.Steps = steps
	}
	return r, nil
}

func findTestAccount(accounts []TestAccount, label string) (TestAccount, bool) {
	want := strings.TrimSpace(label)
	for _, a := range accounts {
		if strings.EqualFold(a.Label, want) {
			return a, true
		}
	}
	return TestAccount{}, false
}

func normalizeRefs(app *AppRecord, in []RunRef) ([]RunRef, error) {
	names := map[string]bool{}
	for _, n := range app.Repos {
		if n != "" {
			names[n] = true
		}
	}
	for _, e := range app.RepoEntries {
		if e.Name != "" {
			names[e.Name] = true
		}
	}
	if in == nil {
		return []RunRef{}, nil
	}
	out := make([]RunRef, 0, len(in))
	for _, r := range in {
		repo := strings.TrimSpace(r.Repo)
		if !names[repo] {
			return nil, badRequest("unknown repo")
		}
		ref := strings.TrimSpace(r.Ref)
		if ref == "" || ParseGitRefName(ref, "") != ref {
			return nil, badRequest("invalid ref")
		}
		sha := strings.TrimSpace(r.SHA)
		if sha != "" && !shaRe.MatchString(sha) {
			return nil, badRequest("invalid sha")
		}
		out = append(out, RunRef{Repo: repo, Ref: ref, SHA: sha})
	}
	return out, nil
}

func exploratoryKey(action string, used map[string]bool) (string, error) {
	base := "x-" + Slugify(action, 40)
	if validKey(base) && !used[base] {
		return base, nil
	}
	for n := 2; n < 1000; n++ {
		k := fmt.Sprintf("%s-%d", base, n)
		if validKey(k) && !used[k] {
			return k, nil
		}
	}
	return "", badRequest("could not allocate step key")
}

func clientStepStatus(status string) error {
	switch status {
	case "pending", "pass", "fail", "blocked", "skipped":
		return nil
	case "unverified":
		return badRequest("cannot set status unverified")
	default:
		return badRequest("invalid status")
	}
}

// CreateRun snapshots plan steps (or starts empty) and leaves the run in_progress.
func CreateRun(sqlDB *sql.DB, in CreateRunInput) (TestRun, error) {
	switch in.Execute {
	case "agent":
		return TestRun{}, badRequest("execute=agent is not available yet")
	case "manual":
	default:
		return TestRun{}, badRequest(`execute must be "manual"`)
	}
	executor := strings.TrimSpace(in.Executor)
	if executor == "" {
		executor = "operator"
	}
	switch executor {
	case "operator", "agent-session":
	case "dd-job":
		return TestRun{}, badRequest("executor=dd-job is not available yet")
	default:
		return TestRun{}, badRequest("executor must be operator or agent-session")
	}
	executedBy, err := trimLimit("executed_by", in.ExecutedBy, maxActorLen, false)
	if err != nil {
		return TestRun{}, err
	}
	var out TestRun
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		// Accounts and repo names are read in this transaction, not from a
		// snapshot taken before it.
		app, err := requireApp(tx, in.AppID)
		if err != nil {
			return err
		}
		var plan *TestPlan
		if strings.TrimSpace(in.PlanID) != "" {
			if !IsSafePlanID(in.PlanID) {
				return notFound("test plan not found")
			}
			p, err := loadPlan(tx, in.PlanID, true)
			if err != nil {
				return err
			}
			if p.AppID != app.ID {
				return badRequest("plan does not belong to app")
			}
			if p.Archived {
				return badRequest("plan is archived")
			}
			plan = &p
		}
		title := strings.TrimSpace(in.Title)
		if title == "" {
			if plan != nil {
				title = plan.Title
			} else {
				title = "Exploratory run"
			}
		}
		if len(title) > maxTitleLen {
			return badRequest("title too long")
		}
		env := map[string]any{}
		if plan != nil {
			for k, v := range plan.Env {
				env[k] = v
			}
		}
		for k, v := range in.Env {
			env[k] = v
		}
		if strings.TrimSpace(in.AccountLabel) != "" {
			acc, ok := findTestAccount(app.TestAccounts, in.AccountLabel)
			if !ok {
				return badRequest("unknown account_label")
			}
			env["account"] = map[string]any{
				"label": acc.Label, "username": acc.Username,
				"environment": acc.Environment, "secret_ref": acc.SecretRef,
			}
		}
		envJSON, err := marshalEnv(env)
		if err != nil {
			return err
		}
		refs, err := normalizeRefs(app, in.Refs)
		if err != nil {
			return err
		}
		refsJSON, err := json.Marshal(refs)
		if err != nil {
			return err
		}
		id := MakeRunID(time.Now())
		ts := db.NowIso()
		var planID any
		var planVersion any
		if plan != nil {
			planID = plan.ID
			planVersion = plan.Version
		}
		if _, err := tx.Exec(`INSERT INTO test_runs (
			id, app_id, plan_id, plan_version, title, executor, executed_by, job_id,
			env_json, refs_json, status, reviewed_by, reviewed_at, review_note,
			started_at, finished_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, 'in_progress', '', NULL, '', ?, NULL, ?, ?)`,
			id, app.ID, planID, planVersion, title, executor, executedBy, envJSON, string(refsJSON), ts, ts, ts); err != nil {
			return err
		}
		if plan != nil {
			if _, err := tx.Exec(`INSERT INTO test_step_results (
				run_id, step_key, position, section, action, expected, exploratory,
				status, actual, evidence_json, defect_id, review, review_note, updated_at
			) SELECT ?, key, position, section, action, expected, 0, 'pending', '', '[]', NULL, '', '', ?
			  FROM test_plan_steps WHERE plan_id = ?`, id, ts, plan.ID); err != nil {
				return err
			}
		}
		out, err = loadRun(tx, id, true)
		return err
	})
	return out, err
}

// GetRun returns one run and its steps, both read in one transaction.
func GetRun(sqlDB *sql.DB, id string) (TestRun, error) {
	var out TestRun
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		var err error
		out, err = loadRun(tx, id, true)
		return err
	})
	return out, err
}

// ListRuns returns runs newest created_at first. The header and each run's
// steps are read in one transaction. Steps are omitted; Progress is set.
func ListRuns(sqlDB *sql.DB, appID, planID string) ([]TestRun, error) {
	appFilter := ""
	if strings.TrimSpace(appID) != "" {
		appFilter = CanonicalizeAppID(appID)
	}
	planFilter := strings.TrimSpace(planID)
	var out []TestRun
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT `+runCols+` FROM test_runs
			WHERE (? = '' OR app_id = ?) AND (? = '' OR plan_id = ?)
			ORDER BY created_at DESC, id DESC`, appFilter, appFilter, planFilter, planFilter)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []TestRun{}
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for i := range out {
			steps, err := loadRunSteps(tx, out[i].ID)
			if err != nil {
				return err
			}
			out[i].Progress = progressFrom(steps)
			out[i].omitSteps = true
		}
		return nil
	})
	return out, err
}

// DeleteRun removes the run. When wipeEvidence is set, the evidence directory
// is removed in the same transaction; a filesystem failure rolls the delete back.
func DeleteRun(s *Store, id string, wipeEvidence bool) error {
	if !IsSafeRunID(id) {
		return notFound("test run not found")
	}
	return withTx(s.DB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM test_runs WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("test run not found")
		}
		if !wipeEvidence {
			return nil
		}
		if err := os.RemoveAll(filepath.Join(paths.EvidenceDir(s.DefectsRoot), id)); err != nil {
			return &OpError{Status: 500, Msg: "could not remove run evidence"}
		}
		return nil
	})
}

func requireInProgress(q rower, runID string) (TestRun, error) {
	run, err := loadRun(q, runID, true)
	if err != nil {
		return TestRun{}, err
	}
	if run.Status != "in_progress" {
		return TestRun{}, conflictErr("run is not in_progress")
	}
	return run, nil
}

// AppendRunStep adds an exploratory step at the end. Only while the run is in_progress.
func AppendRunStep(sqlDB *sql.DB, runID string, in PlanStepIn) (TestRun, error) {
	action, err := trimLimit("action", in.Action, maxTextLen, true)
	if err != nil {
		return TestRun{}, err
	}
	expected, err := trimLimit("expected", in.Expected, maxTextLen, false)
	if err != nil {
		return TestRun{}, err
	}
	section := strings.TrimSpace(in.Section)
	var out TestRun
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := requireInProgress(tx, runID)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		pos := 1
		for _, s := range run.Steps {
			used[s.Key] = true
			if s.Position >= pos {
				pos = s.Position + 1
			}
		}
		key := strings.TrimSpace(in.Key)
		if key != "" {
			if !validKey(key) {
				return badRequest("invalid step key")
			}
			if used[key] {
				return badRequest("step key already used")
			}
		} else {
			key, err = exploratoryKey(action, used)
			if err != nil {
				return err
			}
		}
		ts := db.NowIso()
		if _, err := tx.Exec(`INSERT INTO test_step_results (
			run_id, step_key, position, section, action, expected, exploratory,
			status, actual, evidence_json, defect_id, review, review_note, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 1, 'pending', '', '[]', NULL, '', '', ?)`,
			runID, key, pos, section, action, expected, ts); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE test_runs SET updated_at = ? WHERE id = ?`, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	return out, err
}

// PatchRunStep sets status and/or actual. unverified is rejected. Only while in_progress.
func PatchRunStep(sqlDB *sql.DB, runID, key string, status, actual *string) (TestRun, error) {
	if status != nil {
		if err := clientStepStatus(*status); err != nil {
			return TestRun{}, err
		}
	}
	if actual != nil {
		v, err := trimLimit("actual", *actual, maxTextLen, false)
		if err != nil {
			return TestRun{}, err
		}
		actual = &v
	}
	var out TestRun
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := requireInProgress(tx, runID)
		if err != nil {
			return err
		}
		var cur *RunStep
		for i := range run.Steps {
			if run.Steps[i].Key == key {
				cur = &run.Steps[i]
				break
			}
		}
		if cur == nil {
			return notFound("step not found")
		}
		nextStatus := cur.Status
		nextActual := cur.Actual
		if status != nil {
			nextStatus = *status
		}
		if actual != nil {
			nextActual = *actual
		}
		ts := db.NowIso()
		if _, err := tx.Exec(`UPDATE test_step_results SET status = ?, actual = ?, updated_at = ? WHERE run_id = ? AND step_key = ?`,
			nextStatus, nextActual, ts, runID, key); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE test_runs SET updated_at = ? WHERE id = ?`, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	return out, err
}

// AddRunStepEvidence writes s-<key>-NN.ext under the run's evidence directory and appends the paths.
// The status check, step read, file write and evidence UPDATE share one transaction.
// A run that is not in_progress writes nothing. Files written before a failed
// commit are removed.
func AddRunStepEvidence(s *Store, runID, key string, files []struct {
	Filename string
	Data     []byte
}) (TestRun, error) {
	if !IsEvidenceOwnerID(runID) || !IsSafeRunID(runID) {
		return TestRun{}, notFound("test run not found")
	}
	var out TestRun
	var written []string
	err := withTx(s.DB, func(tx *sql.Tx) error {
		run, err := requireInProgress(tx, runID)
		if err != nil {
			return err
		}
		var cur *RunStep
		for i := range run.Steps {
			if run.Steps[i].Key == key {
				cur = &run.Steps[i]
				break
			}
		}
		if cur == nil {
			return notFound("step not found")
		}
		if !validKey(key) {
			return badRequest("invalid step key")
		}
		written, err = s.writeNumberedEvidence(runID, "s-"+key+"-", files)
		if err != nil {
			return err
		}
		if len(written) == 0 {
			out = run
			return nil
		}
		ev := append(append([]string{}, cur.Evidence...), written...)
		ts := db.NowIso()
		if _, err := tx.Exec(`UPDATE test_step_results SET evidence_json = ?, updated_at = ? WHERE run_id = ? AND step_key = ?`,
			db.JSONArray(ev), ts, runID, key); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE test_runs SET updated_at = ? WHERE id = ?`, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	if err != nil {
		s.removeEvidenceFiles(written)
		var pe *os.PathError
		if errors.As(err, &pe) {
			return TestRun{}, &OpError{Status: 500, Msg: "could not store evidence"}
		}
		return TestRun{}, err
	}
	return out, nil
}

func (s *Store) removeEvidenceFiles(rels []string) {
	for _, rel := range rels {
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		_ = os.Remove(filepath.Join(s.DefectsRoot, filepath.FromSlash(rel)))
	}
}

// FinishRun moves in_progress to awaiting_review and converts pass-without-evidence to unverified.
func FinishRun(sqlDB *sql.DB, runID string) (TestRun, error) {
	var out TestRun
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, false)
		if err != nil {
			return err
		}
		if run.Status != "in_progress" {
			return conflictErr("run is not in_progress")
		}
		ts := db.NowIso()
		if _, err := tx.Exec(`UPDATE test_step_results SET status = 'unverified', updated_at = ?
			WHERE run_id = ? AND status = 'pass' AND json_array_length(evidence_json) = 0`, ts, runID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE test_runs SET status = 'awaiting_review', finished_at = ?, updated_at = ? WHERE id = ?`,
			ts, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	return out, err
}

// AbandonRun moves in_progress or awaiting_review to abandoned.
func AbandonRun(sqlDB *sql.DB, runID string) (TestRun, error) {
	var out TestRun
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, false)
		if err != nil {
			return err
		}
		if run.Status != "in_progress" && run.Status != "awaiting_review" {
			return conflictErr("run cannot be abandoned")
		}
		ts := db.NowIso()
		if _, err := tx.Exec(`UPDATE test_runs SET status = 'abandoned', updated_at = ? WHERE id = ?`, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	return out, err
}

// ReviewRun records per-step reviews and marks the run reviewed. Only from awaiting_review.
func ReviewRun(sqlDB *sql.DB, runID string, in ReviewInput) (TestRun, error) {
	reviewer, err := trimLimit("reviewer", in.Reviewer, maxActorLen, true)
	if err != nil {
		return TestRun{}, err
	}
	note, err := trimLimit("note", in.Note, maxTextLen, false)
	if err != nil {
		return TestRun{}, err
	}
	var out TestRun
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, true)
		if err != nil {
			return err
		}
		if run.Status != "awaiting_review" {
			return conflictErr("run is not awaiting_review")
		}
		byKey := map[string]RunStep{}
		for _, s := range run.Steps {
			byKey[s.Key] = s
		}
		seen := map[string]bool{}
		ts := db.NowIso()
		for _, s := range in.Steps {
			key := strings.TrimSpace(s.Key)
			if seen[key] {
				return badRequest("duplicate step key")
			}
			seen[key] = true
			cur, ok := byKey[key]
			if !ok {
				return badRequest("unknown step key")
			}
			if cur.Status == "pending" {
				return badRequest("cannot review a pending step")
			}
			if s.Review != "accepted" && s.Review != "disputed" {
				return badRequest("invalid review")
			}
			stepNote, err := trimLimit("note", s.Note, maxTextLen, false)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE test_step_results SET review = ?, review_note = ?, updated_at = ? WHERE run_id = ? AND step_key = ?`,
				s.Review, stepNote, ts, runID, key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE test_runs SET status = 'reviewed', reviewed_by = ?, reviewed_at = ?, review_note = ?, updated_at = ? WHERE id = ?`,
			reviewer, ts, note, ts, runID); err != nil {
			return err
		}
		out, err = loadRun(tx, runID, true)
		return err
	})
	return out, err
}

// SaveRunAsPlan creates a plan from the run or replaces an existing plan's steps.
// Exploratory flags are dropped. Abandoned runs are refused.
func SaveRunAsPlan(sqlDB *sql.DB, runID string, in SaveAsPlanInput) (PlanResult, error) {
	hasPlan := strings.TrimSpace(in.PlanID) != ""
	hasKey := strings.TrimSpace(in.Key) != ""
	if hasPlan && hasKey {
		return PlanResult{}, badRequest("provide plan_id or key, not both")
	}
	if !hasPlan && !hasKey {
		return PlanResult{}, badRequest("plan_id or key is required")
	}
	var result PlanResult
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, true)
		if err != nil {
			return err
		}
		if run.Status == "abandoned" {
			return conflictErr("abandoned run cannot be saved as a plan")
		}
		raw := make([]PlanStepIn, len(run.Steps))
		for i, s := range run.Steps {
			raw[i] = PlanStepIn{Key: s.Key, Section: s.Section, Action: s.Action, Expected: s.Expected}
		}
		steps, err := normalizePlanSteps(raw)
		if err != nil {
			return err
		}
		if hasPlan {
			return replacePlanFromRun(tx, run, strings.TrimSpace(in.PlanID), steps, &result)
		}
		return createPlanFromRun(tx, run, in.Key, in.Title, steps, &result)
	})
	return result, err
}

func createPlanFromRun(tx *sql.Tx, run TestRun, key, title string, steps []PlanStep, result *PlanResult) error {
	k, err := normalizeKey("key", key)
	if err != nil {
		return err
	}
	t, err := trimLimit("title", title, maxTitleLen, true)
	if err != nil {
		return err
	}
	existing, err := findPlanByKey(tx, run.AppID, k)
	if err != nil {
		return err
	}
	if existing != nil {
		return conflictErr("plan key already exists")
	}
	id := MakePlanID(t, time.Now())
	ts := db.NowIso()
	if _, err := tx.Exec(`INSERT INTO test_plans (
		id, app_id, key, title, purpose, env_json, version, archived, created_by, created_at, updated_at
	) VALUES (?, ?, ?, ?, '', '{}', 1, 0, '', ?, ?)`, id, run.AppID, k, t, ts, ts); err != nil {
		return err
	}
	if err := replacePlanSteps(tx, id, steps); err != nil {
		return err
	}
	plan, err := loadPlan(tx, id, true)
	if err != nil {
		return err
	}
	*result = PlanResult{Plan: plan, Created: true, Changed: true}
	return nil
}

func replacePlanFromRun(tx *sql.Tx, run TestRun, planID string, steps []PlanStep, result *PlanResult) error {
	if !IsSafePlanID(planID) {
		return notFound("test plan not found")
	}
	plan, err := loadPlan(tx, planID, true)
	if err != nil {
		return err
	}
	if plan.AppID != run.AppID {
		return badRequest("plan does not belong to app")
	}
	if plan.Archived {
		return badRequest("plan is archived")
	}
	if sameSteps(plan.Steps, steps) {
		*result = PlanResult{Plan: plan, Created: false, Changed: false}
		return nil
	}
	ts := db.NowIso()
	if _, err := tx.Exec(`UPDATE test_plans SET version = ?, updated_at = ? WHERE id = ?`, plan.Version+1, ts, plan.ID); err != nil {
		return err
	}
	if err := replacePlanSteps(tx, plan.ID, steps); err != nil {
		return err
	}
	updated, err := loadPlan(tx, plan.ID, true)
	if err != nil {
		return err
	}
	*result = PlanResult{Plan: updated, Created: false, Changed: true}
	return nil
}
