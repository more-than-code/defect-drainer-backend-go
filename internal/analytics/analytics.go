// Package analytics ports getAnalyticsSummary, prompt_use, job_summary, and FTS search.
package analytics

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
)

// AggRow is {key, count}.
type AggRow struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// PromptTop is a prompt_use aggregate row.
type PromptTop struct {
	PromptKey string `json:"prompt_key"`
	Uses      int    `json:"uses"`
	Success   int    `json:"success"`
	Fail      int    `json:"fail"`
	Aborted   int    `json:"aborted"`
	Unknown   int    `json:"unknown"`
	LastUsed  string `json:"last_used"`
}

// Summary is AnalyticsSummary.
type Summary struct {
	AppID   *string `json:"app_id"`
	Defects struct {
		Total      int      `json:"total"`
		BySeverity []AggRow `json:"by_severity"`
		ByArea     []AggRow `json:"by_area"`
		ByStatus   []AggRow `json:"by_status"`
		BySource   []AggRow `json:"by_source"`
		ByBucket   []AggRow `json:"by_bucket"`
		BySurface  []AggRow `json:"by_surface"`
	} `json:"defects"`
	Prompts struct {
		Total int         `json:"total"`
		Top   []PromptTop `json:"top"`
	} `json:"prompts"`
	Jobs struct {
		Total         int      `json:"total"`
		ByStatus      []AggRow `json:"by_status"`
		Completed     int      `json:"completed"`
		Failed        int      `json:"failed"`
		SuccessRate   *float64 `json:"success_rate"`
		PRMergedTotal int      `json:"pr_merged_total"`
	} `json:"jobs"`
}

func countOne(sqlDB *sql.DB, q string, args ...any) int {
	var c int
	_ = sqlDB.QueryRow(q, args...).Scan(&c)
	return c
}

func groupCount(sqlDB *sql.DB, q string, args ...any) []AggRow {
	rows, err := sqlDB.Query(q, args...)
	if err != nil {
		return []AggRow{}
	}
	defer rows.Close()
	out := []AggRow{}
	for rows.Next() {
		var key sql.NullString
		var c int
		if err := rows.Scan(&key, &c); err != nil {
			continue
		}
		k := key.String
		if !key.Valid || k == "" {
			k = "(empty)"
		}
		out = append(out, AggRow{Key: k, Count: c})
	}
	return out
}

// GetSummary is field-for-field getAnalyticsSummary.
func GetSummary(sqlDB *sql.DB, appID string) Summary {
	appID = strings.TrimSpace(appID)
	var appPtr *string
	appClause := ""
	var appArgs []any
	if appID != "" {
		appPtr = &appID
		appClause = "WHERE app_id = ?"
		appArgs = []any{appID}
	}
	var s Summary
	s.AppID = appPtr
	s.Defects.Total = countOne(sqlDB, `SELECT COUNT(*) FROM defects `+appClause, appArgs...)
	by := func(col string) []AggRow {
		return groupCount(sqlDB,
			`SELECT `+col+` AS key, COUNT(*) AS count FROM defects `+appClause+` GROUP BY `+col+` ORDER BY count DESC LIMIT 30`,
			appArgs...)
	}
	s.Defects.BySeverity = by("severity")
	s.Defects.ByArea = by("area")
	s.Defects.ByStatus = by("status")
	s.Defects.BySource = by("source")
	s.Defects.ByBucket = by("bucket")
	s.Defects.BySurface = by("surface")

	s.Prompts.Total = countOne(sqlDB, `SELECT COUNT(*) FROM prompt_use `+appClause, appArgs...)
	s.Prompts.Top = []PromptTop{}
	rows, err := sqlDB.Query(`SELECT prompt_key,
        COUNT(*) AS uses,
        SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END) AS success,
        SUM(CASE WHEN outcome = 'fail' THEN 1 ELSE 0 END) AS fail,
        SUM(CASE WHEN outcome = 'aborted' THEN 1 ELSE 0 END) AS aborted,
        SUM(CASE WHEN outcome = 'unknown' OR outcome IS NULL OR outcome = '' THEN 1 ELSE 0 END) AS unknown,
        MAX(created_at) AS last_used
      FROM prompt_use
      `+appClause+`
      GROUP BY prompt_key
      ORDER BY uses DESC
      LIMIT 25`, appArgs...)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var t PromptTop
			if err := rows.Scan(&t.PromptKey, &t.Uses, &t.Success, &t.Fail, &t.Aborted, &t.Unknown, &t.LastUsed); err == nil {
				s.Prompts.Top = append(s.Prompts.Top, t)
			}
		}
	}

	s.Jobs.Total = countOne(sqlDB, `SELECT COUNT(*) FROM job_summary `+appClause, appArgs...)
	s.Jobs.ByStatus = groupCount(sqlDB,
		`SELECT status AS key, COUNT(*) AS count FROM job_summary `+appClause+` GROUP BY status ORDER BY count DESC`,
		appArgs...)
	if appID != "" {
		s.Jobs.Completed = countOne(sqlDB, `SELECT COUNT(*) FROM job_summary WHERE app_id = ? AND status = ?`, appID, "completed")
		s.Jobs.Failed = countOne(sqlDB, `SELECT COUNT(*) FROM job_summary WHERE app_id = ? AND status = ?`, appID, "failed")
		s.Jobs.PRMergedTotal = countOne(sqlDB, `SELECT COALESCE(SUM(pr_merged),0) FROM job_summary WHERE app_id = ?`, appID)
	} else {
		s.Jobs.Completed = countOne(sqlDB, `SELECT COUNT(*) FROM job_summary WHERE status = ?`, "completed")
		s.Jobs.Failed = countOne(sqlDB, `SELECT COUNT(*) FROM job_summary WHERE status = ?`, "failed")
		s.Jobs.PRMergedTotal = countOne(sqlDB, `SELECT COALESCE(SUM(pr_merged),0) FROM job_summary`)
	}
	denom := s.Jobs.Completed + s.Jobs.Failed
	if denom > 0 {
		rate := float64(s.Jobs.Completed) / float64(denom)
		s.Jobs.SuccessRate = &rate
	}
	return s
}

// DefectHit is a Phase A search row.
type DefectHit struct {
	ID       string `json:"id"`
	AppID    string `json:"app_id"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Area     string `json:"area"`
	Bucket   string `json:"bucket"`
	Snippet  string `json:"snippet,omitempty"`
}

// SearchDefects is FTS / LIKE / recent.
func SearchDefects(sqlDB *sql.DB, q, appID string, limit int) (hits []DefectHit, mode string) {
	if limit < 1 {
		limit = 40
	}
	if limit > 100 {
		limit = 100
	}
	appID = strings.TrimSpace(appID)
	q = strings.TrimSpace(q)
	hits = []DefectHit{}
	if q == "" {
		var rows *sql.Rows
		var err error
		if appID != "" {
			rows, err = sqlDB.Query(`SELECT id, app_id, title, summary, severity, status, area, bucket
             FROM defects WHERE app_id = ? ORDER BY updated_at DESC LIMIT ?`, appID, limit)
		} else {
			rows, err = sqlDB.Query(`SELECT id, app_id, title, summary, severity, status, area, bucket
             FROM defects ORDER BY updated_at DESC LIMIT ?`, limit)
		}
		if err != nil {
			return hits, "recent"
		}
		defer rows.Close()
		for rows.Next() {
			var h DefectHit
			if err := rows.Scan(&h.ID, &h.AppID, &h.Title, &h.Summary, &h.Severity, &h.Status, &h.Area, &h.Bucket); err == nil {
				hits = append(hits, h)
			}
		}
		return hits, "recent"
	}
	tokens := strings.Fields(strings.NewReplacer(`"`, " ", `'`, " ").Replace(q))
	if len(tokens) == 0 {
		return SearchDefects(sqlDB, "", appID, limit)
	}
	if len(tokens) > 12 {
		tokens = tokens[:12]
	}
	var parts []string
	for _, t := range tokens {
		t = strings.ReplaceAll(t, `"`, "")
		if t != "" {
			parts = append(parts, `"`+t+`"*`)
		}
	}
	ftsQuery := strings.Join(parts, " ")
	var rows *sql.Rows
	var err error
	if appID != "" {
		rows, err = sqlDB.Query(`SELECT d.id, d.app_id, d.title, d.summary, d.severity, d.status, d.area, d.bucket,
               snippet(defects_fts, 2, '[', ']', '…', 12) AS snippet
         FROM defects_fts
         JOIN defects d ON d.id = defects_fts.id
         WHERE defects_fts MATCH ? AND d.app_id = ?
         ORDER BY rank
         LIMIT ?`, ftsQuery, appID, limit)
	} else {
		rows, err = sqlDB.Query(`SELECT d.id, d.app_id, d.title, d.summary, d.severity, d.status, d.area, d.bucket,
               snippet(defects_fts, 2, '[', ']', '…', 12) AS snippet
         FROM defects_fts
         JOIN defects d ON d.id = defects_fts.id
         WHERE defects_fts MATCH ?
         ORDER BY rank
         LIMIT ?`, ftsQuery, limit)
	}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var h DefectHit
			var snip sql.NullString
			if err := rows.Scan(&h.ID, &h.AppID, &h.Title, &h.Summary, &h.Severity, &h.Status, &h.Area, &h.Bucket, &snip); err == nil {
				h.Snippet = snip.String
				hits = append(hits, h)
			}
		}
		return hits, "fts"
	}
	like := "%" + q
	if len(like) > 82 {
		like = "%" + q[:80] + "%"
	} else {
		like = "%" + q + "%"
	}
	if appID != "" {
		rows, err = sqlDB.Query(`SELECT id, app_id, title, summary, severity, status, area, bucket
             FROM defects
             WHERE app_id = ?
               AND (title LIKE ? OR summary LIKE ? OR body LIKE ?)
             ORDER BY updated_at DESC LIMIT ?`, appID, like, like, like, limit)
	} else {
		rows, err = sqlDB.Query(`SELECT id, app_id, title, summary, severity, status, area, bucket
             FROM defects
             WHERE title LIKE ? OR summary LIKE ? OR body LIKE ?
             ORDER BY updated_at DESC LIMIT ?`, like, like, like, limit)
	}
	if err != nil {
		return hits, "like"
	}
	defer rows.Close()
	for rows.Next() {
		var h DefectHit
		if err := rows.Scan(&h.ID, &h.AppID, &h.Title, &h.Summary, &h.Severity, &h.Status, &h.Area, &h.Bucket); err == nil {
			hits = append(hits, h)
		}
	}
	return hits, "like"
}

// RecordPromptUse inserts a prompt_use row.
func RecordPromptUse(sqlDB *sql.DB, promptKey, version, jobID, batchID, appID, outcome, runner, body string, defectIDs []string) string {
	id := "pu_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "_" + randHex(3)
	ts := db.NowIso()
	if promptKey == "" {
		promptKey = "unknown"
	}
	if version == "" {
		version = "1"
	}
	if outcome == "" {
		outcome = "unknown"
	}
	_, _ = sqlDB.Exec(`INSERT INTO prompt_use (
      id, prompt_key, prompt_version, job_id, batch_id, app_id,
      defect_ids_json, outcome, runner, body_text, created_at, updated_at
    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, trunc(promptKey, 200), trunc(version, 40), nullIfEmpty(jobID), nullIfEmpty(batchID), appID,
		db.JSONArray(defectIDs), outcome, trunc(runner, 80), trunc(body, 2000), ts, ts,
	)
	return id
}

// SetPromptUseOutcomeForJob updates all rows for a job.
func SetPromptUseOutcomeForJob(sqlDB *sql.DB, jobID, outcome string) {
	if jobID == "" {
		return
	}
	_, _ = sqlDB.Exec(`UPDATE prompt_use SET outcome = ?, updated_at = ? WHERE job_id = ?`, outcome, db.NowIso(), jobID)
}

// UpsertJobSummary writes job_summary.
func UpsertJobSummary(sqlDB *sql.DB, jobID, batchID, appID, status, mode, errMsg, createdAt string, defectCount, prCreated, prMerged, prOpen int) {
	updated := db.NowIso()
	var completed any
	if status == "completed" || status == "failed" || status == "cancelled" || status == "manual" {
		completed = updated
	}
	var errVal any
	if errMsg != "" {
		errVal = trunc(errMsg, 500)
	}
	_, _ = sqlDB.Exec(`INSERT INTO job_summary (
      job_id, batch_id, app_id, status, mode, defect_count,
      pr_created, pr_merged, pr_open, error, created_at, updated_at, completed_at
    ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(job_id) DO UPDATE SET
      batch_id=excluded.batch_id,
      app_id=excluded.app_id,
      status=excluded.status,
      mode=excluded.mode,
      defect_count=excluded.defect_count,
      pr_created=excluded.pr_created,
      pr_merged=excluded.pr_merged,
      pr_open=excluded.pr_open,
      error=excluded.error,
      updated_at=excluded.updated_at,
      completed_at=COALESCE(job_summary.completed_at, excluded.completed_at)`,
		jobID, batchID, appID, status, mode, defectCount,
		prCreated, prMerged, prOpen, errVal, createdAt, updated, completed,
	)
}

// DeleteJobSummary removes a row.
func DeleteJobSummary(sqlDB *sql.DB, jobID string) {
	_, _ = sqlDB.Exec(`DELETE FROM job_summary WHERE job_id = ?`, jobID)
}

func JobStatusToPromptOutcome(status string) string {
	switch status {
	case "completed", "manual":
		return "success"
	case "failed":
		return "fail"
	case "cancelled":
		return "aborted"
	default:
		return "unknown"
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CountOutbox returns search_outbox depth.
func CountOutbox(sqlDB *sql.DB) int {
	return countOne(sqlDB, `SELECT COUNT(*) FROM search_outbox`)
}
