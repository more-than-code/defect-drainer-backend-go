package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/paths"
)

// DefectRecord is the console defect JSON object.
type DefectRecord struct {
	ID           string   `json:"id"`
	AppID        string   `json:"app_id"`
	Title        string   `json:"title"`
	Severity     string   `json:"severity"`
	Status       string   `json:"status"`
	Area         string   `json:"area"`
	Client       string   `json:"client"`
	Surface      string   `json:"surface"`
	Repos        []string `json:"repos"`
	Labels       []string `json:"labels"`
	Related      []string `json:"related"`
	Source       string   `json:"source"`
	Reporter     string   `json:"reporter"`
	KeyFiles     []string `json:"key_files"`
	Evidence     []string `json:"evidence"`
	FixEvidence  []string `json:"fix_evidence"`
	Reported     string   `json:"reported"`
	Summary      string   `json:"summary"`
	ResolvedDate string   `json:"resolved_date,omitempty"`
	Resolution   string   `json:"resolution,omitempty"`
	DuplicateOf  string   `json:"duplicate_of,omitempty"`
	Body         string   `json:"body"`
	Bucket       string   `json:"bucket"`
	CreatedAt    string   `json:"created_at"`
	Path         string   `json:"path"`
}

// Store is the defect + evidence root.
type Store struct {
	DB          *sql.DB
	DefectsRoot string
}

// NewStore ensures evidence/ exists.
func NewStore(sqlDB *sql.DB, defectsRoot string) *Store {
	_ = os.MkdirAll(paths.EvidenceDir(defectsRoot), 0o755)
	return &Store{DB: sqlDB, DefectsRoot: defectsRoot}
}

func rowToDefect(
	id, appID, title, severity, status, area, client, surface string,
	reposJSON, labelsJSON, relatedJSON string,
	source, reporter string,
	keyFilesJSON, evidenceJSON, fixJSON string,
	reported, summary, body string,
	resolution, resolvedDate, duplicateOf sql.NullString,
	bucket, createdAt string,
) DefectRecord {
	bkt := "open"
	if bucket == "resolved" {
		bkt = "resolved"
	}
	rec := DefectRecord{
		ID:          id,
		AppID:       CanonicalizeAppID(appID),
		Title:       title,
		Severity:    severity,
		Status:      status,
		Area:        area,
		Client:      client,
		Surface:     surface,
		Repos:       db.ParseJSONArray(reposJSON),
		Labels:      db.ParseJSONArray(labelsJSON),
		Related:     db.ParseJSONArray(relatedJSON),
		Source:      source,
		Reporter:    reporter,
		KeyFiles:    db.ParseJSONArray(keyFilesJSON),
		Evidence:    db.ParseJSONArray(evidenceJSON),
		FixEvidence: db.ParseJSONArray(fixJSON),
		Reported:    reported,
		Summary:     summary,
		Body:        body,
		Bucket:      bkt,
		CreatedAt:   createdAt,
		Path:        bkt + "/" + id + ".md",
	}
	if rec.Reporter == "" {
		rec.Reporter = ""
	}
	if resolution.Valid && resolution.String != "" {
		rec.Resolution = resolution.String
	}
	if resolvedDate.Valid && resolvedDate.String != "" {
		rec.ResolvedDate = resolvedDate.String
	}
	if duplicateOf.Valid && duplicateOf.String != "" {
		rec.DuplicateOf = duplicateOf.String
	}
	return rec
}

func scanDefect(scanner interface{ Scan(dest ...any) error }) (DefectRecord, error) {
	var (
		id, appID, title, severity, status, area, client, surface string
		reposJSON, labelsJSON, relatedJSON                        string
		source, reporter                                          string
		keyFilesJSON, evidenceJSON, fixJSON                       string
		reported, summary, body                                   string
		resolution, resolvedDate, duplicateOf                     sql.NullString
		bucket, createdAt                                         string
	)
	err := scanner.Scan(
		&id, &appID, &title, &severity, &status, &area, &client, &surface,
		&reposJSON, &labelsJSON, &relatedJSON, &source, &reporter,
		&keyFilesJSON, &evidenceJSON, &fixJSON,
		&reported, &summary, &body, &resolution, &resolvedDate, &duplicateOf,
		&bucket, &createdAt,
	)
	if err != nil {
		return DefectRecord{}, err
	}
	return rowToDefect(
		id, appID, title, severity, status, area, client, surface,
		reposJSON, labelsJSON, relatedJSON, source, reporter,
		keyFilesJSON, evidenceJSON, fixJSON,
		reported, summary, body, resolution, resolvedDate, duplicateOf,
		bucket, createdAt,
	), nil
}

const defectSelect = `id, app_id, title, severity, status, area, client, surface,
  repos_json, labels_json, related_json, source, reporter, key_files_json,
  evidence_json, fix_evidence_json, reported, summary, body, resolution,
  resolved_date, duplicate_of, bucket, created_at`

// List filters defects. Default caller supplies bucket.
func (s *Store) List(filter struct {
	Status string
	Bucket string
	AppID  string
}) ([]DefectRecord, error) {
	var clauses []string
	var params []any
	if filter.Bucket == "open" || filter.Bucket == "resolved" {
		clauses = append(clauses, "bucket = ?")
		params = append(params, filter.Bucket)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		params = append(params, filter.Status)
	}
	if filter.AppID != "" {
		clauses = append(clauses, "app_id = ?")
		params = append(params, CanonicalizeAppID(filter.AppID))
	}
	q := `SELECT ` + defectSelect + ` FROM defects`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY reported DESC, id ASC"
	rows, err := s.DB.Query(q, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DefectRecord{}
	for rows.Next() {
		rec, err := scanDefect(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Get returns a defect or nil. Unsafe ids are nil (404), not 400.
func (s *Store) Get(id string) (*DefectRecord, error) {
	if !IsSafeID(id) {
		return nil, nil
	}
	row := s.DB.QueryRow(`SELECT `+defectSelect+` FROM defects WHERE id = ?`, id)
	rec, err := scanDefect(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// Write inserts or updates a defect (created_at preserved on update).
func (s *Store) Write(rec DefectRecord) (DefectRecord, error) {
	if !IsSafeID(rec.ID) {
		return DefectRecord{}, fmt.Errorf("invalid defect id: %s", rec.ID)
	}
	bucket := rec.Bucket
	if bucket == "" {
		if rec.Status == "resolved" || rec.Status == "wontfix" {
			bucket = "resolved"
		} else {
			bucket = "open"
		}
	}
	appID := CanonicalizeAppID(rec.AppID)
	if appID == "" {
		appID = SeededTutoredWebappAppID
	}
	ts := db.NowIso()
	existing, err := s.Get(rec.ID)
	if err != nil {
		return DefectRecord{}, err
	}
	severity := strings.TrimSpace(rec.Severity)
	if severity == "" {
		severity = "P2"
	}
	status := strings.TrimSpace(rec.Status)
	if status == "" {
		status = "open"
	}
	area := strings.TrimSpace(rec.Area)
	if area == "" {
		area = "other"
	}
	client := strings.TrimSpace(rec.Client)
	if client == "" {
		client = "unknown"
	}
	surface := strings.TrimSpace(rec.Surface)
	source := strings.TrimSpace(rec.Source)
	if source == "" {
		source = "unknown"
	}
	reporter := strings.TrimSpace(rec.Reporter)
	title := strings.TrimSpace(rec.Title)
	if title == "" {
		title = "Untitled defect"
	}
	var resolution, resolvedDate, dup any
	if rec.Resolution != "" {
		resolution = rec.Resolution
	}
	if rec.ResolvedDate != "" {
		resolvedDate = rec.ResolvedDate
	}
	if rec.DuplicateOf != "" {
		dup = rec.DuplicateOf
	}
	if existing != nil {
		_, err = s.DB.Exec(`UPDATE defects SET
            app_id=?, title=?, severity=?, status=?, area=?, client=?, surface=?,
            repos_json=?, labels_json=?, related_json=?, source=?, reporter=?, key_files_json=?,
            evidence_json=?, fix_evidence_json=?, reported=?, summary=?, body=?,
            resolution=?, resolved_date=?, duplicate_of=?, bucket=?, updated_at=?
           WHERE id=?`,
			appID, title, severity, status, area, client, surface,
			db.JSONArray(rec.Repos), db.JSONArray(rec.Labels), db.JSONArray(rec.Related),
			source, reporter, db.JSONArray(rec.KeyFiles),
			db.JSONArray(rec.Evidence), db.JSONArray(rec.FixEvidence),
			rec.Reported, rec.Summary, rec.Body,
			resolution, resolvedDate, dup, bucket, ts, rec.ID,
		)
	} else {
		_, err = s.DB.Exec(`INSERT INTO defects (
            id, app_id, title, severity, status, area, client, surface,
            repos_json, labels_json, related_json, source, reporter, key_files_json,
            evidence_json, fix_evidence_json, reported, summary, body,
            resolution, resolved_date, duplicate_of, bucket, created_at, updated_at
          ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			rec.ID, appID, title, severity, status, area, client, surface,
			db.JSONArray(rec.Repos), db.JSONArray(rec.Labels), db.JSONArray(rec.Related),
			source, reporter, db.JSONArray(rec.KeyFiles),
			db.JSONArray(rec.Evidence), db.JSONArray(rec.FixEvidence),
			rec.Reported, rec.Summary, rec.Body,
			resolution, resolvedDate, dup, bucket, ts, ts,
		)
	}
	if err != nil {
		return DefectRecord{}, err
	}
	saved, err := s.Get(rec.ID)
	if err != nil || saved == nil {
		return DefectRecord{}, fmt.Errorf("write defect failed")
	}
	_ = SyncDefectFTS(s.DB, *saved)
	return *saved, nil
}

// Update patches a defect. Reporter is not in the PATCH allowlist (TS).
func (s *Store) Update(id string, patch DefectRecord, has map[string]bool) (DefectRecord, error) {
	cur, err := s.Get(id)
	if err != nil {
		return DefectRecord{}, err
	}
	if cur == nil {
		return DefectRecord{}, fmt.Errorf("defect not found: %s", id)
	}
	next := *cur
	if has["title"] {
		next.Title = patch.Title
	}
	if has["app_id"] {
		next.AppID = patch.AppID
	}
	if has["severity"] {
		next.Severity = patch.Severity
	}
	if has["status"] {
		next.Status = patch.Status
	}
	if has["area"] {
		next.Area = patch.Area
	}
	if has["client"] {
		next.Client = patch.Client
	}
	if has["surface"] {
		next.Surface = patch.Surface
	}
	if has["repos"] {
		next.Repos = patch.Repos
	}
	if has["labels"] {
		next.Labels = patch.Labels
	}
	if has["related"] {
		next.Related = patch.Related
	}
	if has["source"] {
		next.Source = patch.Source
	}
	if has["key_files"] {
		next.KeyFiles = patch.KeyFiles
	}
	if has["evidence"] {
		next.Evidence = patch.Evidence
	}
	if has["fix_evidence"] {
		next.FixEvidence = patch.FixEvidence
	}
	if has["summary"] {
		next.Summary = patch.Summary
	}
	if has["body"] {
		next.Body = patch.Body
	}
	if has["resolution"] {
		next.Resolution = patch.Resolution
	}
	if has["resolved_date"] {
		next.ResolvedDate = patch.ResolvedDate
	}
	if has["duplicate_of"] {
		next.DuplicateOf = patch.DuplicateOf
	}
	if patch.Status == "resolved" || patch.Status == "wontfix" {
		next.Bucket = "resolved"
		if next.ResolvedDate == "" {
			next.ResolvedDate = db.TodayUTC()
		}
	} else if patch.Status == "open" || patch.Status == "triaged" || patch.Status == "in_progress" {
		next.Bucket = "open"
	}
	return s.Write(next)
}

// Resolve requires nonempty fix_evidence unless skip.
func (s *Store) Resolve(id, resolution string, fix []string, skip bool) (DefectRecord, error) {
	cur, err := s.Get(id)
	if err != nil {
		return DefectRecord{}, err
	}
	if cur == nil {
		return DefectRecord{}, fmt.Errorf("defect not found: %s", id)
	}
	if fix == nil {
		fix = cur.FixEvidence
	}
	if !skip && len(fix) == 0 {
		return DefectRecord{}, fmt.Errorf("fix_evidence required: attach post-fix screenshot(s) before resolve")
	}
	res := resolution
	if res == "" {
		res = cur.Resolution
	}
	if res == "" {
		res = "resolved"
	}
	return s.Update(id, DefectRecord{
		Status:       "resolved",
		Resolution:   res,
		ResolvedDate: db.TodayUTC(),
		FixEvidence:  fix,
	}, map[string]bool{"status": true, "resolution": true, "resolved_date": true, "fix_evidence": true})
}

// Reopen clears resolution.
func (s *Store) Reopen(id string) (DefectRecord, error) {
	cur, err := s.Get(id)
	if err != nil {
		return DefectRecord{}, err
	}
	if cur == nil {
		return DefectRecord{}, fmt.Errorf("defect not found: %s", id)
	}
	cur.Status = "open"
	cur.Bucket = "open"
	cur.Resolution = ""
	cur.ResolvedDate = ""
	return s.Write(*cur)
}

// Delete removes the row; optionally wipes evidence/.
func (s *Store) Delete(id string, wipeEvidence bool) (bool, error) {
	cur, err := s.Get(id)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return false, nil
	}
	if _, err := s.DB.Exec(`DELETE FROM defects WHERE id = ?`, id); err != nil {
		return false, err
	}
	_, _ = s.DB.Exec(`DELETE FROM defects_fts WHERE id = ?`, id)
	if wipeEvidence {
		_ = os.RemoveAll(filepath.Join(paths.EvidenceDir(s.DefectsRoot), id))
	}
	return true, nil
}

// SaveEvidence writes 01.png / fix-01.png style files.
func (s *Store) SaveEvidence(id string, files []struct{ Filename string; Data []byte }, kind string) ([]string, error) {
	if !IsSafeID(id) {
		return nil, fmt.Errorf("invalid defect id: %s", id)
	}
	if kind == "" {
		kind = "report"
	}
	dir := filepath.Join(paths.EvidenceDir(s.DefectsRoot), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, _ := os.ReadDir(dir)
	used := map[string]bool{}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			used[e.Name()] = true
		}
	}
	prefix := ""
	if kind == "fix" {
		prefix = "fix-"
	}
	var out []string
	n := 1
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f.Filename))
		if ext == "" {
			ext = ".png"
		}
		switch ext {
		case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".heic":
		default:
			ext = ".png"
		}
		name := fmt.Sprintf("%s%02d%s", prefix, n, ext)
		for used[name] {
			n++
			name = fmt.Sprintf("%s%02d%s", prefix, n, ext)
		}
		if err := os.WriteFile(filepath.Join(dir, name), f.Data, 0o644); err != nil {
			return nil, err
		}
		used[name] = true
		out = append(out, fmt.Sprintf("evidence/%s/%s", id, name))
		n++
	}
	return out, nil
}

// SyncDefectFTS updates the FTS row; errors are swallowed by callers like TS.
func SyncDefectFTS(sqlDB *sql.DB, rec DefectRecord) error {
	_, err := sqlDB.Exec(`DELETE FROM defects_fts WHERE id = ?`, rec.ID)
	if err != nil {
		return err
	}
	_, err = sqlDB.Exec(
		`INSERT INTO defects_fts (id, app_id, title, summary, body) VALUES (?,?,?,?,?)`,
		rec.ID, rec.AppID, rec.Title, rec.Summary, rec.Body,
	)
	return err
}
