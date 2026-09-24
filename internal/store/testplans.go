package store

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
)

// OpError is a failed plan or run operation. Status is the HTTP code the API returns.
type OpError struct {
	Status int
	Msg    string
}

func (e *OpError) Error() string { return e.Msg }

func badRequest(msg string) error { return &OpError{Status: 400, Msg: msg} }
func notFound(msg string) error   { return &OpError{Status: 404, Msg: msg} }
func conflictErr(msg string) error {
	return &OpError{Status: 409, Msg: msg}
}

const (
	maxTitleLen = 200
	maxTextLen  = 4000
	maxActorLen = 120
	maxEnvBytes = 8 << 10
	maxKeyLen   = 64
)

var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// PlanStep is one ordered step of a test plan.
type PlanStep struct {
	Key      string `json:"key"`
	Position int    `json:"position"`
	Section  string `json:"section"`
	Action   string `json:"action"`
	Expected string `json:"expected"`
}

// PlanStepIn is a step as submitted (position comes from array order).
type PlanStepIn struct {
	Key      string
	Section  string
	Action   string
	Expected string
}

// TestPlan is the plan JSON object. List responses set StepCount and omit Steps.
type TestPlan struct {
	ID        string         `json:"id"`
	AppID     string         `json:"app_id"`
	Key       string         `json:"key"`
	Title     string         `json:"title"`
	Purpose   string         `json:"purpose"`
	Env       map[string]any `json:"env"`
	Version   int            `json:"version"`
	Archived  bool           `json:"archived"`
	CreatedBy string         `json:"created_by"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Steps     []PlanStep     `json:"steps,omitempty"`
	StepCount *int           `json:"step_count,omitempty"`
	// omitSteps selects the list shape: step_count, and no steps key.
	omitSteps bool
}

// MarshalJSON emits steps on a detail plan, including an empty list, and omits
// them on a list row (step_count only).
func (p TestPlan) MarshalJSON() ([]byte, error) {
	env := p.Env
	if env == nil {
		env = map[string]any{}
	}
	if p.omitSteps {
		n := 0
		if p.StepCount != nil {
			n = *p.StepCount
		}
		return json.Marshal(struct {
			ID        string         `json:"id"`
			AppID     string         `json:"app_id"`
			Key       string         `json:"key"`
			Title     string         `json:"title"`
			Purpose   string         `json:"purpose"`
			Env       map[string]any `json:"env"`
			Version   int            `json:"version"`
			Archived  bool           `json:"archived"`
			CreatedBy string         `json:"created_by"`
			CreatedAt string         `json:"created_at"`
			UpdatedAt string         `json:"updated_at"`
			StepCount int            `json:"step_count"`
		}{
			ID: p.ID, AppID: p.AppID, Key: p.Key, Title: p.Title, Purpose: p.Purpose,
			Env: env, Version: p.Version, Archived: p.Archived, CreatedBy: p.CreatedBy,
			CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, StepCount: n,
		})
	}
	steps := p.Steps
	if steps == nil {
		steps = []PlanStep{}
	}
	return json.Marshal(struct {
		ID        string         `json:"id"`
		AppID     string         `json:"app_id"`
		Key       string         `json:"key"`
		Title     string         `json:"title"`
		Purpose   string         `json:"purpose"`
		Env       map[string]any `json:"env"`
		Version   int            `json:"version"`
		Archived  bool           `json:"archived"`
		CreatedBy string         `json:"created_by"`
		CreatedAt string         `json:"created_at"`
		UpdatedAt string         `json:"updated_at"`
		Steps     []PlanStep     `json:"steps"`
	}{
		ID: p.ID, AppID: p.AppID, Key: p.Key, Title: p.Title, Purpose: p.Purpose,
		Env: env, Version: p.Version, Archived: p.Archived, CreatedBy: p.CreatedBy,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Steps: steps,
	})
}

// PlanResult is an upsert or save-as-plan outcome.
type PlanResult struct {
	Plan    TestPlan
	Created bool
	Changed bool
}

// UpsertPlanInput is POST /api/test-plans.
type UpsertPlanInput struct {
	AppID     string
	Key       string
	Title     string
	Purpose   string
	Env       map[string]any
	CreatedBy string
	Steps     []PlanStepIn
}

// PatchPlanInput patches whichever Has keys are set. Steps, when set, replace the whole list.
type PatchPlanInput struct {
	Has      map[string]bool
	Title    string
	Purpose  string
	Env      map[string]any
	Archived bool
	Steps    []PlanStepIn
}

type rower interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

const planCols = `id, app_id, key, title, purpose, env_json, version, archived, created_by, created_at, updated_at`

func withTx(sqlDB *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func validKey(k string) bool {
	return keyRe.MatchString(k) && !strings.Contains(k, "..")
}

func trimLimit(name, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return "", badRequest(name + " is required")
	}
	if len(v) > max {
		return "", badRequest(name + " too long")
	}
	return v, nil
}

func normalizeKey(name, v string) (string, error) {
	k, err := trimLimit(name, v, maxKeyLen, true)
	if err != nil {
		return "", err
	}
	if !validKey(k) {
		return "", badRequest("invalid " + name)
	}
	return k, nil
}

var envSecretKeyRe = regexp.MustCompile(`(?i)(pass|secret|token)`)

const envSecretKeyMsg = "env keys must not hold secrets (password/secret/token); use a test account secret_ref"

func rejectSecretEnvKeys(v any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k != "secret_ref" && envSecretKeyRe.MatchString(k) {
				return badRequest(envSecretKeyMsg)
			}
			if err := rejectSecretEnvKeys(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := rejectSecretEnvKeys(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func marshalEnv(m map[string]any) (string, error) {
	if m == nil {
		m = map[string]any{}
	}
	if err := rejectSecretEnvKeys(m); err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", badRequest("env must be a JSON object")
	}
	if len(b) > maxEnvBytes {
		return "", badRequest("env exceeds 8 KiB")
	}
	return string(b), nil
}

func unmarshalEnv(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func normalizePlanSteps(in []PlanStepIn) ([]PlanStep, error) {
	seen := map[string]bool{}
	out := make([]PlanStep, 0, len(in))
	for i, s := range in {
		key, err := normalizeKey("step key", s.Key)
		if err != nil {
			return nil, err
		}
		if seen[key] {
			return nil, badRequest("duplicate step key")
		}
		seen[key] = true
		action, err := trimLimit("action", s.Action, maxTextLen, true)
		if err != nil {
			return nil, err
		}
		expected, err := trimLimit("expected", s.Expected, maxTextLen, false)
		if err != nil {
			return nil, err
		}
		out = append(out, PlanStep{
			Key: key, Position: i + 1, Section: strings.TrimSpace(s.Section),
			Action: action, Expected: expected,
		})
	}
	return out, nil
}

func sameSteps(a, b []PlanStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Section != b[i].Section || a[i].Action != b[i].Action || a[i].Expected != b[i].Expected {
			return false
		}
	}
	return true
}

func requireApp(q rower, appID string) (*AppRecord, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, badRequest("app_id is required")
	}
	app, err := getApp(q, appID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, notFound("unknown app_id")
	}
	return app, nil
}

func scanPlan(sc interface{ Scan(...any) error }) (TestPlan, error) {
	var p TestPlan
	var envJSON string
	var archived int
	if err := sc.Scan(&p.ID, &p.AppID, &p.Key, &p.Title, &p.Purpose, &envJSON, &p.Version, &archived, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return TestPlan{}, err
	}
	p.Archived = archived != 0
	p.Env = unmarshalEnv(envJSON)
	return p, nil
}

func loadPlanSteps(q rower, planID string) ([]PlanStep, error) {
	rows, err := q.Query(`SELECT key, position, section, action, expected FROM test_plan_steps WHERE plan_id = ? ORDER BY position ASC, key ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlanStep{}
	for rows.Next() {
		var s PlanStep
		if err := rows.Scan(&s.Key, &s.Position, &s.Section, &s.Action, &s.Expected); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func loadPlan(q rower, id string, withSteps bool) (TestPlan, error) {
	p, err := scanPlan(q.QueryRow(`SELECT `+planCols+` FROM test_plans WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return TestPlan{}, notFound("test plan not found")
	}
	if err != nil {
		return TestPlan{}, err
	}
	if withSteps {
		p.Steps, err = loadPlanSteps(q, p.ID)
		if err != nil {
			return TestPlan{}, err
		}
	}
	return p, nil
}

func findPlanByKey(q rower, appID, key string) (*TestPlan, error) {
	p, err := scanPlan(q.QueryRow(`SELECT `+planCols+` FROM test_plans WHERE app_id = ? AND key = ?`, appID, key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Steps, err = loadPlanSteps(q, p.ID)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func replacePlanSteps(q rower, planID string, steps []PlanStep) error {
	if _, err := q.Exec(`DELETE FROM test_plan_steps WHERE plan_id = ?`, planID); err != nil {
		return err
	}
	for _, s := range steps {
		if _, err := q.Exec(
			`INSERT INTO test_plan_steps (plan_id, key, position, section, action, expected) VALUES (?, ?, ?, ?, ?, ?)`,
			planID, s.Key, s.Position, s.Section, s.Action, s.Expected,
		); err != nil {
			return err
		}
	}
	return nil
}

// UpsertPlan inserts a plan or updates the one with the same (app_id, key).
// Version increments only when title, purpose, env, or steps change.
// Re-posting an archived plan's key clears archived without counting as a content change.
func UpsertPlan(sqlDB *sql.DB, in UpsertPlanInput) (PlanResult, error) {
	app, err := requireApp(sqlDB, in.AppID)
	if err != nil {
		return PlanResult{}, err
	}
	key, err := normalizeKey("key", in.Key)
	if err != nil {
		return PlanResult{}, err
	}
	title, err := trimLimit("title", in.Title, maxTitleLen, true)
	if err != nil {
		return PlanResult{}, err
	}
	purpose, err := trimLimit("purpose", in.Purpose, maxTextLen, false)
	if err != nil {
		return PlanResult{}, err
	}
	createdBy, err := trimLimit("created_by", in.CreatedBy, maxActorLen, false)
	if err != nil {
		return PlanResult{}, err
	}
	if in.Steps == nil {
		return PlanResult{}, badRequest("steps is required")
	}
	steps, err := normalizePlanSteps(in.Steps)
	if err != nil {
		return PlanResult{}, err
	}
	envJSON, err := marshalEnv(in.Env)
	if err != nil {
		return PlanResult{}, err
	}
	var result PlanResult
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		cur, err := findPlanByKey(tx, app.ID, key)
		if err != nil {
			return err
		}
		ts := db.NowIso()
		if cur == nil {
			id := MakePlanID(title, time.Now())
			if _, err := tx.Exec(`INSERT INTO test_plans (
				id, app_id, key, title, purpose, env_json, version, archived, created_by, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 1, 0, ?, ?, ?)`,
				id, app.ID, key, title, purpose, envJSON, createdBy, ts, ts); err != nil {
				return err
			}
			if err := replacePlanSteps(tx, id, steps); err != nil {
				return err
			}
			plan, err := loadPlan(tx, id, true)
			if err != nil {
				return err
			}
			result = PlanResult{Plan: plan, Created: true, Changed: true}
			return nil
		}
		curEnv, err := marshalEnv(cur.Env)
		if err != nil {
			return err
		}
		changed := cur.Title != title || cur.Purpose != purpose || curEnv != envJSON || !sameSteps(cur.Steps, steps)
		if !changed && !cur.Archived {
			result = PlanResult{Plan: *cur, Created: false, Changed: false}
			return nil
		}
		version := cur.Version
		if changed {
			version++
		}
		if _, err := tx.Exec(`UPDATE test_plans SET title = ?, purpose = ?, env_json = ?, version = ?, archived = 0, updated_at = ? WHERE id = ?`,
			title, purpose, envJSON, version, ts, cur.ID); err != nil {
			return err
		}
		if changed {
			if err := replacePlanSteps(tx, cur.ID, steps); err != nil {
				return err
			}
		}
		plan, err := loadPlan(tx, cur.ID, true)
		if err != nil {
			return err
		}
		result = PlanResult{Plan: plan, Created: false, Changed: changed}
		return nil
	})
	return result, err
}

// GetPlan returns one plan and its steps.
func GetPlan(sqlDB *sql.DB, id string) (TestPlan, error) {
	if !IsSafePlanID(id) {
		return TestPlan{}, notFound("test plan not found")
	}
	return loadPlan(sqlDB, id, true)
}

// ListPlans returns plans newest updated_at first. Archived plans are hidden unless includeArchived.
// Steps are omitted; StepCount is set.
func ListPlans(sqlDB *sql.DB, appID string, includeArchived bool) ([]TestPlan, error) {
	filter := ""
	if strings.TrimSpace(appID) != "" {
		filter = CanonicalizeAppID(appID)
	}
	inc := 0
	if includeArchived {
		inc = 1
	}
	rows, err := sqlDB.Query(`SELECT `+planCols+`, (SELECT COUNT(*) FROM test_plan_steps s WHERE s.plan_id = test_plans.id)
		FROM test_plans WHERE (? = '' OR app_id = ?) AND (? = 1 OR archived = 0)
		ORDER BY updated_at DESC, id DESC`, filter, filter, inc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TestPlan{}
	for rows.Next() {
		var envJSON string
		var archived, count int
		var p TestPlan
		if err := rows.Scan(&p.ID, &p.AppID, &p.Key, &p.Title, &p.Purpose, &envJSON, &p.Version, &archived, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &count); err != nil {
			return nil, err
		}
		p.Archived = archived != 0
		p.Env = unmarshalEnv(envJSON)
		n := count
		p.StepCount = &n
		p.omitSteps = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// PatchPlan applies a partial update. Version increments only when content changes, not when only archived changes.
func PatchPlan(sqlDB *sql.DB, id string, in PatchPlanInput) (TestPlan, error) {
	if !IsSafePlanID(id) {
		return TestPlan{}, notFound("test plan not found")
	}
	var out TestPlan
	err := withTx(sqlDB, func(tx *sql.Tx) error {
		cur, err := loadPlan(tx, id, true)
		if err != nil {
			return err
		}
		title, purpose := cur.Title, cur.Purpose
		env := cur.Env
		steps := cur.Steps
		archived := cur.Archived
		if in.Has["title"] {
			title, err = trimLimit("title", in.Title, maxTitleLen, true)
			if err != nil {
				return err
			}
		}
		if in.Has["purpose"] {
			purpose, err = trimLimit("purpose", in.Purpose, maxTextLen, false)
			if err != nil {
				return err
			}
		}
		if in.Has["env"] {
			if _, err = marshalEnv(in.Env); err != nil {
				return err
			}
			env = in.Env
			if env == nil {
				env = map[string]any{}
			}
		}
		if in.Has["steps"] {
			raw := in.Steps
			if raw == nil {
				raw = []PlanStepIn{}
			}
			steps, err = normalizePlanSteps(raw)
			if err != nil {
				return err
			}
		}
		if in.Has["archived"] {
			archived = in.Archived
		}
		curEnv, err := marshalEnv(cur.Env)
		if err != nil {
			return err
		}
		nextEnv, err := marshalEnv(env)
		if err != nil {
			return err
		}
		contentChanged := title != cur.Title || purpose != cur.Purpose || nextEnv != curEnv || !sameSteps(cur.Steps, steps)
		if !contentChanged && archived == cur.Archived {
			out = cur
			return nil
		}
		version := cur.Version
		if contentChanged {
			version++
		}
		arch := 0
		if archived {
			arch = 1
		}
		ts := db.NowIso()
		if _, err := tx.Exec(`UPDATE test_plans SET title = ?, purpose = ?, env_json = ?, version = ?, archived = ?, updated_at = ? WHERE id = ?`,
			title, purpose, nextEnv, version, arch, ts, id); err != nil {
			return err
		}
		if !sameSteps(cur.Steps, steps) {
			if err := replacePlanSteps(tx, id, steps); err != nil {
				return err
			}
		}
		out, err = loadPlan(tx, id, true)
		return err
	})
	return out, err
}

// DeletePlan archives the plan when any run references it, otherwise deletes it.
// archived result is true when the row was kept and marked archived.
func DeletePlan(sqlDB *sql.DB, id string) (archived bool, deleted bool, err error) {
	if !IsSafePlanID(id) {
		return false, false, notFound("test plan not found")
	}
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		if _, err := loadPlan(tx, id, false); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM test_runs WHERE plan_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err := tx.Exec(`UPDATE test_plans SET archived = 1, updated_at = ? WHERE id = ?`, db.NowIso(), id); err != nil {
				return err
			}
			archived = true
			return nil
		}
		if _, err := tx.Exec(`DELETE FROM test_plans WHERE id = ?`, id); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return archived, deleted, err
}
