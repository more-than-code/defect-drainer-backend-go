package store

import (
	"database/sql"
	"strconv"
	"strings"
)

// GridPlan is the plan header on GET /api/test-plans/{id}/grid.
type GridPlan struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

// GridRun is one reviewed run column, newest started_at first.
type GridRun struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	PlanVersion *int     `json:"plan_version"`
	StartedAt   string   `json:"started_at"`
	ReviewedAt  *string  `json:"reviewed_at"`
	ReviewedBy  string   `json:"reviewed_by"`
	Refs        []RunRef `json:"refs"`
}

// GridDefect is a defect linked to a plan step from any run of the plan.
type GridDefect struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	RunID  string `json:"run_id"`
}

// GridCell is one step's result on one shown run. A missing snapshot is null.
type GridCell struct {
	RunID    string  `json:"run_id"`
	Status   string  `json:"status"`
	Review   string  `json:"review"`
	DefectID *string `json:"defect_id"`
}

// GridStep is one current plan step and its cells, aligned with Grid.Runs.
type GridStep struct {
	Key         string       `json:"key"`
	Position    int          `json:"position"`
	Section     string       `json:"section"`
	Action      string       `json:"action"`
	NeedsRetest bool         `json:"needs_retest"`
	Defects     []GridDefect `json:"defects"`
	Cells       []*GridCell  `json:"cells"`
}

// TestPlanGrid is the retest grid. NeedsRetest is computed over every reviewed
// run of the plan, not only the runs included in the response.
type TestPlanGrid struct {
	Plan  GridPlan   `json:"plan"`
	Runs  []GridRun  `json:"runs"`
	Steps []GridStep `json:"steps"`
}

type gridHit struct {
	status   string
	review   string
	defectID *string
}

type linkedDefect struct {
	GridDefect
	stepKey   string
	updatedAt string
}

// PlanGrid returns the current steps of a plan against its last N reviewed runs.
// runsRaw empty means 10. Values outside 1..50 are rejected.
func PlanGrid(sqlDB *sql.DB, planID, runsRaw string) (TestPlanGrid, error) {
	limit, err := gridRunLimit(runsRaw)
	if err != nil {
		return TestPlanGrid{}, err
	}
	if !IsSafePlanID(planID) {
		return TestPlanGrid{}, notFound("test plan not found")
	}
	var grid TestPlanGrid
	err = withTx(sqlDB, func(tx *sql.Tx) error {
		plan, err := loadPlan(tx, planID, true)
		if err != nil {
			return err
		}
		reviewed, err := loadReviewedRuns(tx, plan.ID)
		if err != nil {
			return err
		}
		hits, err := loadPlanStepHits(tx, plan.ID)
		if err != nil {
			return err
		}
		linked, err := loadLinkedDefects(tx, plan.ID)
		if err != nil {
			return err
		}
		shown := reviewed
		if len(shown) > limit {
			shown = shown[:limit]
		}
		if shown == nil {
			shown = []GridRun{}
		}
		steps := make([]GridStep, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			defects := []GridDefect{}
			for _, d := range linked {
				if d.stepKey == step.Key {
					defects = append(defects, d.GridDefect)
				}
			}
			cells := make([]*GridCell, len(shown))
			for i, run := range shown {
				hit, ok := hits[run.ID][step.Key]
				if !ok {
					continue
				}
				cells[i] = &GridCell{RunID: run.ID, Status: hit.status, Review: hit.review, DefectID: hit.defectID}
			}
			steps = append(steps, GridStep{
				Key: step.Key, Position: step.Position, Section: step.Section, Action: step.Action,
				NeedsRetest: stepNeedsRetest(step.Key, linked, reviewed, hits),
				Defects:     defects, Cells: cells,
			})
		}
		grid = TestPlanGrid{
			Plan:  GridPlan{ID: plan.ID, Key: plan.Key, Title: plan.Title, Version: plan.Version},
			Runs:  shown,
			Steps: steps,
		}
		return nil
	})
	return grid, err
}

func gridRunLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 10, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 50 {
		return 0, badRequest("runs must be between 1 and 50")
	}
	return n, nil
}

func loadReviewedRuns(q rower, planID string) ([]GridRun, error) {
	rows, err := q.Query(`SELECT id, title, plan_version, started_at, reviewed_at, reviewed_by, refs_json
		FROM test_runs WHERE plan_id = ? AND status = 'reviewed'
		ORDER BY started_at DESC, id DESC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GridRun{}
	for rows.Next() {
		var r GridRun
		var planVersion sql.NullInt64
		var reviewedAt sql.NullString
		var refsJSON string
		if err := rows.Scan(&r.ID, &r.Title, &planVersion, &r.StartedAt, &reviewedAt, &r.ReviewedBy, &refsJSON); err != nil {
			return nil, err
		}
		r.PlanVersion = nullIntPtr(planVersion)
		r.ReviewedAt = nullStringPtr(reviewedAt)
		r.Refs = parseRunRefs(refsJSON)
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadPlanStepHits(q rower, planID string) (map[string]map[string]gridHit, error) {
	rows, err := q.Query(`SELECT s.run_id, s.step_key, s.status, s.review, s.defect_id
		FROM test_step_results s
		JOIN test_runs r ON r.id = s.run_id
		WHERE r.plan_id = ? AND r.status = 'reviewed'`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]gridHit{}
	for rows.Next() {
		var runID, key, status, review string
		var defect sql.NullString
		if err := rows.Scan(&runID, &key, &status, &review, &defect); err != nil {
			return nil, err
		}
		if out[runID] == nil {
			out[runID] = map[string]gridHit{}
		}
		out[runID][key] = gridHit{status: status, review: review, defectID: nullStringPtr(defect)}
	}
	return out, rows.Err()
}

func loadLinkedDefects(q rower, planID string) ([]linkedDefect, error) {
	rows, err := q.Query(`SELECT s.step_key, d.id, d.status, s.run_id, d.updated_at
		FROM test_step_results s
		JOIN test_runs r ON r.id = s.run_id
		JOIN defects d ON d.id = s.defect_id
		WHERE r.plan_id = ? AND s.defect_id IS NOT NULL AND s.defect_id != ''
		ORDER BY d.created_at ASC, d.id ASC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []linkedDefect{}
	for rows.Next() {
		var d linkedDefect
		if err := rows.Scan(&d.stepKey, &d.ID, &d.Status, &d.RunID, &d.updatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// stepNeedsRetest is true when a resolved linked defect has no later reviewed
// run whose accepted result on this step is pass or unverified. Resolution
// time is the defect's updated_at. A run must start strictly after that time.
// An empty timestamp does not count as cleared.
func stepNeedsRetest(stepKey string, linked []linkedDefect, reviewed []GridRun, hits map[string]map[string]gridHit) bool {
	for _, d := range linked {
		if d.stepKey != stepKey || d.Status != "resolved" {
			continue
		}
		if !retestCleared(stepKey, d.updatedAt, reviewed, hits) {
			return true
		}
	}
	return false
}

func retestCleared(stepKey, resolvedAt string, reviewed []GridRun, hits map[string]map[string]gridHit) bool {
	if strings.TrimSpace(resolvedAt) == "" {
		return false
	}
	for _, run := range reviewed {
		if strings.TrimSpace(run.StartedAt) == "" || run.StartedAt <= resolvedAt {
			continue
		}
		hit, ok := hits[run.ID][stepKey]
		if !ok {
			continue
		}
		if hit.review == "accepted" && (hit.status == "pass" || hit.status == "unverified") {
			return true
		}
	}
	return false
}
