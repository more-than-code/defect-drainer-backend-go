// Package db opens the shared SQLite SSOT (same SCHEMA as the TS backend).
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/joe/defect-drainer-go/internal/paths"
	_ "modernc.org/sqlite"
)

// SCHEMA matches backend/src/db.ts (including JS-style comments SQLite accepts).
const SCHEMA = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS apps (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT,
  workspace_root TEXT,
  repos_json TEXT NOT NULL DEFAULT '[]',
  repo_url TEXT,
  repo_urls_json TEXT NOT NULL DEFAULT '[]',
  /** Agent sandbox profile: strict | workspace (per App Settings) */
  grok_sandbox TEXT NOT NULL DEFAULT 'strict',
  /** Batch-fix base: worktrees branch from <base_remote>/<base_branch>; PRs target base_branch. */
  base_remote TEXT NOT NULL DEFAULT 'origin',
  base_branch TEXT NOT NULL DEFAULT 'main',
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS defects (
  id TEXT PRIMARY KEY,
  app_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  severity TEXT NOT NULL DEFAULT 'P2',
  status TEXT NOT NULL DEFAULT 'open',
  area TEXT NOT NULL DEFAULT 'other',
  client TEXT NOT NULL DEFAULT 'unknown',
  surface TEXT NOT NULL DEFAULT '',
  repos_json TEXT NOT NULL DEFAULT '[]',
  labels_json TEXT NOT NULL DEFAULT '[]',
  related_json TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL DEFAULT '',
  /** Who filed it: operator name or agent tool. source is HOW it was detected. */
  reporter TEXT NOT NULL DEFAULT '',
  key_files_json TEXT NOT NULL DEFAULT '[]',
  evidence_json TEXT NOT NULL DEFAULT '[]',
  fix_evidence_json TEXT NOT NULL DEFAULT '[]',
  reported TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  resolution TEXT,
  resolved_date TEXT,
  duplicate_of TEXT,
  bucket TEXT NOT NULL DEFAULT 'open',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (app_id) REFERENCES apps(id)
);

CREATE INDEX IF NOT EXISTS idx_defects_app ON defects(app_id);
CREATE INDEX IF NOT EXISTS idx_defects_bucket ON defects(bucket);
CREATE INDEX IF NOT EXISTS idx_defects_status ON defects(status);
CREATE INDEX IF NOT EXISTS idx_defects_app_bucket ON defects(app_id, bucket);

CREATE TABLE IF NOT EXISTS batches (
  id TEXT PRIMARY KEY,
  app_id TEXT NOT NULL,
  title TEXT NOT NULL,
  goal TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'planned',
  defect_ids_json TEXT NOT NULL DEFAULT '[]',
  mode TEXT,
  created TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (app_id) REFERENCES apps(id)
);

CREATE INDEX IF NOT EXISTS idx_batches_app ON batches(app_id);

CREATE INDEX IF NOT EXISTS idx_defects_severity ON defects(severity);
CREATE INDEX IF NOT EXISTS idx_defects_area ON defects(area);
CREATE INDEX IF NOT EXISTS idx_defects_source ON defects(source);

/** Phase A analytics: structured prompt/job events (not log scrapes). */
CREATE TABLE IF NOT EXISTS prompt_use (
  id TEXT PRIMARY KEY,
  prompt_key TEXT NOT NULL,
  prompt_version TEXT NOT NULL DEFAULT '1',
  job_id TEXT,
  batch_id TEXT,
  app_id TEXT NOT NULL DEFAULT '',
  defect_ids_json TEXT NOT NULL DEFAULT '[]',
  outcome TEXT NOT NULL DEFAULT 'unknown',
  runner TEXT NOT NULL DEFAULT '',
  body_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_prompt_use_app ON prompt_use(app_id);
CREATE INDEX IF NOT EXISTS idx_prompt_use_key ON prompt_use(prompt_key);
CREATE INDEX IF NOT EXISTS idx_prompt_use_job ON prompt_use(job_id);
CREATE INDEX IF NOT EXISTS idx_prompt_use_created ON prompt_use(created_at);

/** Phase A: denormalized batch job rows for SQL harness analytics. */
CREATE TABLE IF NOT EXISTS job_summary (
  job_id TEXT PRIMARY KEY,
  batch_id TEXT NOT NULL DEFAULT '',
  app_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT '',
  defect_count INTEGER NOT NULL DEFAULT 0,
  pr_created INTEGER NOT NULL DEFAULT 0,
  pr_merged INTEGER NOT NULL DEFAULT 0,
  pr_open INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  completed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_job_summary_app ON job_summary(app_id);
CREATE INDEX IF NOT EXISTS idx_job_summary_status ON job_summary(status);
CREATE INDEX IF NOT EXISTS idx_job_summary_created ON job_summary(created_at);

/** Phase B fail-soft: docs that failed to publish to OpenSearch. */
CREATE TABLE IF NOT EXISTS search_outbox (
  id TEXT PRIMARY KEY,
  doc_id TEXT NOT NULL,
  op TEXT NOT NULL DEFAULT 'index',
  body_json TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_search_outbox_updated ON search_outbox(updated_at);
`

// Open creates/opens {dataRoot}/defect-drainer.db, applies SCHEMA + migrations + FTS.
func Open(dataRoot string) (*sql.DB, error) {
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", paths.DbPath(dataRoot))
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		sqlDB.Close()
		return nil, err
	}
	// Go-only additive (not TS parity): short wait after flock instead of SQLITE_BUSY.
	if _, err := sqlDB.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(SCHEMA); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := migrateAppColumns(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := migrateDefectColumns(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := ensureDefectsFts(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return sqlDB, nil
}

func migrateAppColumns(sqlDB *sql.DB) error {
	names, err := tableColumns(sqlDB, "apps")
	if err != nil {
		return err
	}
	adds := []struct {
		name string
		ddl  string
	}{
		{"grok_sandbox", `ALTER TABLE apps ADD COLUMN grok_sandbox TEXT NOT NULL DEFAULT 'strict'`},
		{"base_remote", `ALTER TABLE apps ADD COLUMN base_remote TEXT NOT NULL DEFAULT 'origin'`},
		{"base_branch", `ALTER TABLE apps ADD COLUMN base_branch TEXT NOT NULL DEFAULT 'main'`},
	}
	for _, a := range adds {
		if names[a.name] {
			continue
		}
		if _, err := sqlDB.Exec(a.ddl); err != nil {
			return err
		}
	}
	return nil
}

func migrateDefectColumns(sqlDB *sql.DB) error {
	names, err := tableColumns(sqlDB, "defects")
	if err != nil {
		return err
	}
	if !names["reporter"] {
		if _, err := sqlDB.Exec(`ALTER TABLE defects ADD COLUMN reporter TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func ensureDefectsFts(sqlDB *sql.DB) error {
	if _, err := sqlDB.Exec(`
    CREATE VIRTUAL TABLE IF NOT EXISTS defects_fts USING fts5(
      id UNINDEXED,
      app_id UNINDEXED,
      title,
      summary,
      body,
      tokenize = 'porter unicode61'
    );
  `); err != nil {
		return err
	}
	var ftsCount, defCount int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM defects_fts`).Scan(&ftsCount); err != nil {
		return nil // FTS optional if extension missing
	}
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM defects`).Scan(&defCount); err != nil {
		return nil
	}
	if defCount == 0 || ftsCount != 0 {
		return nil
	}
	rows, err := sqlDB.Query(`SELECT id, app_id, title, summary, body FROM defects`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ins, err := sqlDB.Prepare(`INSERT INTO defects_fts (id, app_id, title, summary, body) VALUES (?,?,?,?,?)`)
	if err != nil {
		return nil
	}
	defer ins.Close()
	for rows.Next() {
		var id, appID, title, summary, body string
		if err := rows.Scan(&id, &appID, &title, &summary, &body); err != nil {
			continue
		}
		_, _ = ins.Exec(id, appID, title, summary, body)
	}
	return nil
}

func tableColumns(sqlDB *sql.DB, table string) (map[string]bool, error) {
	rows, err := sqlDB.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// NowIso is JS Date#toISOString() (UTC, millisecond precision).
func NowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// TodayUTC is UTC date-only YYYY-MM-DD.
func TodayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

// JSONArray stringifies a string array (TS jsonArray: v.map(String)).
func JSONArray(v []string) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ParseJSONArray parses a JSON string array; invalid/non-array → [].
func ParseJSONArray(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return []string{}
	}
	arr, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprint(x))
	}
	return out
}
