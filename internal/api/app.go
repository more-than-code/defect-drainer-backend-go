// Package api registers the console HTTP contract on net/http ServeMux.
package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/env"
	"github.com/joe/defect-drainer-go/internal/httpx"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

const jsonBodyLimit = 1 << 20 // 1 MiB, Fastify default bodyLimit
const maxMultipartMem = 26 << 20
const maxFiles = 12
const maxFileBytes = 25 << 20

// App is a locked, opened control-plane instance.
type App struct {
	Roots   paths.Roots
	DB      *sql.DB
	Lock    *db.Lock
	Store   *store.Store
	Jobs    *jobs.Runner
	Batches *jobs.BatchRunner
	Handler http.Handler
}

// Options mirror TS buildApp({ defectsRoot, dataRoot, skipMigrate }).
type Options struct {
	DefectsRoot string
	DataRoot    string
	Cwd         string
	SkipMigrate bool
}

// Build resolves roots, flocks, opens the database, seeds, and returns the mux.
// SQLite is the default. DEFECT_DRAINER_DB_DRIVER=postgres opens Postgres and
// does not create defect-drainer.db or run DDL.
func Build(opts Options) (*App, error) {
	roots, err := paths.Resolve(paths.Options{
		DefectsRoot: opts.DefectsRoot,
		DataRoot:    opts.DataRoot,
		Cwd:         opts.Cwd,
	})
	if err != nil {
		return nil, err
	}
	lock, err := db.AcquireLock(roots.DataRoot)
	if err != nil {
		return nil, err
	}
	sqlDB, _, err := db.OpenConfigured(roots.DataRoot)
	if err != nil {
		lock.Close()
		return nil, err
	}
	if err := evidenceDirRequired(sqlDB, roots.DefectsRoot); err != nil {
		sqlDB.Close()
		lock.Close()
		return nil, err
	}
	if !opts.SkipMigrate {
		if err := migrateFilesystemIfNeeded(sqlDB, roots.DefectsRoot); err != nil {
			sqlDB.Close()
			lock.Close()
			return nil, err
		}
	}
	if err := store.EnsureSeededApps(sqlDB); err != nil {
		sqlDB.Close()
		lock.Close()
		return nil, err
	}
	st := store.NewStore(sqlDB, roots.DefectsRoot)
	a := &App{
		Roots:   roots,
		DB:      sqlDB,
		Lock:    lock,
		Store:   st,
		Jobs:    jobs.NewRunner(st, roots.DataRoot),
		Batches: jobs.NewBatchRunner(st, roots.DataRoot),
	}
	// Queued simulator jobs survived the restart. Start the oldest now that
	// the runner exists; the rest follow as each agent's loop ends.
	a.Batches.ResumeQueue(roots.DefectsRoot)
	a.Handler = a.mux()
	return a, nil
}

// Close releases the lock and DB.
func (a *App) Close() {
	if a == nil {
		return
	}
	if a.DB != nil {
		_ = a.DB.Close()
	}
	if a.Lock != nil {
		_ = a.Lock.Close()
	}
}

func (a *App) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", a.getHealth)
	m.HandleFunc("GET /api/apps", a.getApps)
	m.HandleFunc("POST /api/apps", a.postApp)
	m.HandleFunc("GET /api/apps/{id}", a.getApp)
	m.HandleFunc("PATCH /api/apps/{id}", a.patchApp)
	m.HandleFunc("DELETE /api/apps/{id}", a.deleteApp)
	m.HandleFunc("GET /api/test-plans", a.getTestPlans)
	m.HandleFunc("POST /api/test-plans", a.postTestPlan)
	m.HandleFunc("GET /api/test-plans/{id}/grid", a.getTestPlanGrid)
	m.HandleFunc("GET /api/test-plans/{id}/export", a.getTestPlanExport)
	m.HandleFunc("GET /api/test-plans/{id}", a.getTestPlan)
	m.HandleFunc("PATCH /api/test-plans/{id}", a.patchTestPlan)
	m.HandleFunc("DELETE /api/test-plans/{id}", a.deleteTestPlan)
	m.HandleFunc("GET /api/test-runs", a.getTestRuns)
	m.HandleFunc("POST /api/test-runs", a.postTestRun)
	m.HandleFunc("GET /api/test-runs/{id}", a.getTestRun)
	m.HandleFunc("DELETE /api/test-runs/{id}", a.deleteTestRun)
	m.HandleFunc("POST /api/test-runs/{id}/steps", a.postTestRunStep)
	m.HandleFunc("PATCH /api/test-runs/{id}/steps/{key}", a.patchTestRunStep)
	m.HandleFunc("POST /api/test-runs/{id}/steps/{key}/evidence", a.postTestRunStepEvidence)
	m.HandleFunc("POST /api/test-runs/{id}/steps/{key}/file-defect", a.fileTestRunStepDefect)
	m.HandleFunc("POST /api/test-runs/{id}/finish", a.finishTestRun)
	m.HandleFunc("POST /api/test-runs/{id}/abandon", a.abandonTestRun)
	m.HandleFunc("POST /api/test-runs/{id}/review", a.reviewTestRun)
	m.HandleFunc("POST /api/test-runs/{id}/save-as-plan", a.saveTestRunAsPlan)
	m.HandleFunc("GET /api/defects", a.getDefects)
	m.HandleFunc("GET /api/defects/{id}", a.getDefect)
	m.HandleFunc("PATCH /api/defects/{id}", a.patchDefect)
	m.HandleFunc("DELETE /api/defects/{id}", a.deleteDefect)
	m.HandleFunc("POST /api/defects/{id}/resolve", a.resolveDefect)
	m.HandleFunc("POST /api/defects/{id}/reopen", a.reopenDefect)
	m.HandleFunc("POST /api/defects/{id}/fix-evidence", a.fixEvidence)
	m.HandleFunc("GET /evidence/{id}/{file}", a.getEvidence)
	m.HandleFunc("GET /api/analytics", a.getAnalytics)
	m.HandleFunc("GET /api/search", a.getSearch)
	m.HandleFunc("GET /api/search/status", a.getSearchStatus)
	m.HandleFunc("POST /api/search/flush", a.postSearchFlush)
	m.HandleFunc("POST /api/search/reindex", a.postSearchReindex)
	m.HandleFunc("POST /api/intake", a.postIntakeMultipart)
	m.HandleFunc("POST /api/intake/json", a.postIntakeJSON)
	m.HandleFunc("GET /api/jobs", a.getJobs)
	m.HandleFunc("GET /api/jobs/{id}", a.getJob)
	m.HandleFunc("POST /api/jobs/{id}/complete", a.completeJob)
	m.HandleFunc("POST /api/jobs/{id}/cancel", a.cancelJob)
	m.HandleFunc("GET /api/batches", a.getBatches)
	m.HandleFunc("POST /api/batches", a.postBatch)
	m.HandleFunc("GET /api/batches/{id}", a.getBatch)
	m.HandleFunc("GET /api/batch-jobs/{id}", a.getBatchJob)
	m.HandleFunc("GET /api/batch-jobs/{id}/diff/{repo}", a.getBatchJobDiff)
	m.HandleFunc("DELETE /api/batch-jobs/{id}", a.deleteBatchJob)
	m.HandleFunc("GET /api/batch-jobs/{id}/delete-preview", a.deletePreview)
	m.HandleFunc("POST /api/batch-jobs/{id}/stop", a.stopBatchJob)
	m.HandleFunc("POST /api/batch-jobs/{id}/rerun", a.rerunBatchJob)
	m.HandleFunc("POST /api/batch-jobs/{id}/resume", a.resumeBatchJob)
	m.HandleFunc("GET /api/batch-jobs/{id}/ship-preview", a.shipPreview)
	m.HandleFunc("POST /api/batch-jobs/{id}/create-prs", a.createPRs)
	m.HandleFunc("POST /api/batch-jobs/{id}/refresh-prs", a.refreshPRs)
	m.HandleFunc("POST /api/git/branches", a.gitBranches)
	m.HandleFunc("POST /api/git/choose-folder", a.gitChooseFolder)
	return m
}

func resolveNormalizeMode() string {
	if env.EnvDrainerFlag("NORMALIZE_GROK") {
		return "grok"
	}
	if env.EnvDrainerFlag("NORMALIZE_MANUAL") {
		return "manual"
	}
	if env.EnvDrainerFlag("NORMALIZE_LOCAL") {
		return "local"
	}
	m := strings.ToLower(env.EnvDrainer("NORMALIZE_MODE"))
	if m == "" {
		m = "local"
	}
	if m == "local" || m == "manual" || m == "grok" {
		return m
	}
	return "local"
}

func searchPhase() string {
	if strings.TrimSpace(env.EnvDrainer("OPENSEARCH_URL")) != "" {
		return "phase-b"
	}
	return "phase-a"
}

func (a *App) getHealth(w http.ResponseWriter, _ *http.Request) {
	open, _ := a.Store.List(struct {
		Status string
		Bucket string
		AppID  string
	}{Bucket: "open"})
	resolved, _ := a.Store.List(struct {
		Status string
		Bucket string
		AppID  string
	}{Bucket: "resolved"})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"service":       "defect-drainer",
		"open":          len(open),
		"resolved":      len(resolved),
		"defaultAppId":  store.GetDefaultAppID(a.DB),
		"normalizeMode": resolveNormalizeMode(),
		"ssot":          ssotName(a.DB),
		"analytics":     "phase-a",
		"search":        searchPhase(),
	})
}

func (a *App) getApps(w http.ResponseWriter, _ *http.Request) {
	apps, err := store.ListApps(a.DB)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"apps":         apps,
		"defaultAppId": store.GetDefaultAppID(a.DB),
	})
}

func (a *App) getApp(w http.ResponseWriter, r *http.Request) {
	app, err := store.GetApp(a.DB, r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if app == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": app})
}

func (a *App) getDefects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket := q.Get("bucket")
	if bucket != "open" && bucket != "resolved" && bucket != "all" {
		bucket = "open"
	}
	list, err := a.Store.List(struct {
		Status string
		Bucket string
		AppID  string
	}{
		Status: q.Get("status"),
		Bucket: bucket,
		AppID:  strings.TrimSpace(q.Get("app_id")),
	})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defects": list})
}

func (a *App) getDefect(w http.ResponseWriter, r *http.Request) {
	rec, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defect": rec})
}

func (a *App) getEvidence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	file := r.PathValue("file")
	if !store.IsEvidenceOwnerID(id) || strings.Contains(file, "..") || strings.Contains(file, "/") || strings.Contains(file, `\`) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid path")
		return
	}
	ev := paths.EvidenceDir(a.Roots.DefectsRoot)
	abs := filepath.Join(ev, id, file)
	root, err := filepath.Abs(ev)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	ext := strings.ToLower(filepath.Ext(file))
	typ := "application/octet-stream"
	switch ext {
	case ".png":
		typ = "image/png"
	case ".jpg", ".jpeg":
		typ = "image/jpeg"
	case ".webp":
		typ = "image/webp"
	case ".gif":
		typ = "image/gif"
	}
	w.Header().Set("Content-Type", typ)
	http.ServeFile(w, r, abs)
}

// evidenceDirRequired refuses to start when a defect or test-step row references
// evidence files and those files are not in {DEFECTS_ROOT}/evidence. It does not
// create anything. A directory with none of the referenced files (empty, or nested
// one level too deep) is not enough. The refusal text stays the same either way.
func evidenceDirRequired(sqlDB *sql.DB, defectsRoot string) error {
	n, refs, err := evidenceRefs(sqlDB)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	dir := paths.EvidenceDir(defectsRoot)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("evidence directory %s does not exist, but defects reference evidence files; see docs/operating.md", dir)
	}
	for _, ref := range refs {
		if rel, ok := ref.(string); ok && referencedEvidenceFile(defectsRoot, rel) {
			return nil
		}
	}
	return fmt.Errorf("evidence directory %s exists, but none of the referenced evidence files were found there; see docs/operating.md", dir)
}

// evidenceRefScanLimit is how many referenced evidence entries the startup
// guard checks on disk.
const evidenceRefScanLimit = 50

// evidenceRefs returns how many defect and test-step rows have a non-empty
// evidence array, and the first evidenceRefScanLimit array elements in the
// order the guard checks them: defect evidence, then defect fix evidence, then
// test-step evidence. The JSON is decoded in Go because json_array_length and
// json_each do not accept TEXT on Postgres. Only strict JSON is supported (the
// app writes these columns with encoding/json). Malformed JSON is an error, and
// a non-array value references nothing. Only JSON strings are checked as paths:
// null, number, bool, array and object elements are skipped but still use up
// one of the evidenceRefScanLimit slots, as they did in the old SQL.
func evidenceRefs(sqlDB *sql.DB) (int, []any, error) {
	n, defects, err := scanEvidenceArrays(sqlDB, `SELECT evidence_json, fix_evidence_json FROM defects`, 2)
	if err != nil {
		return 0, nil, err
	}
	m, steps, err := scanEvidenceArrays(sqlDB, `SELECT evidence_json FROM test_step_results`, 1)
	if err != nil {
		return 0, nil, err
	}
	refs := append(append(defects[0], defects[1]...), steps[0]...)
	if len(refs) > evidenceRefScanLimit {
		refs = refs[:evidenceRefScanLimit]
	}
	return n + m, refs, nil
}

// scanEvidenceArrays runs query, whose cols columns are evidence JSON texts, and
// returns how many rows had at least one non-empty array plus one element list
// per column. Each list keeps at most evidenceRefScanLimit elements: the guard
// checks only the first evidenceRefScanLimit of the concatenated lists, so no
// column can contribute more than that.
func scanEvidenceArrays(sqlDB *sql.DB, query string, cols int) (int, [][]any, error) {
	rows, err := sqlDB.Query(query)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	raws := make([]string, cols)
	dest := make([]any, cols)
	for i := range raws {
		dest[i] = &raws[i]
	}
	lists := make([][]any, cols)
	n := 0
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return 0, nil, err
		}
		nonEmpty := false
		for i, raw := range raws {
			v, err := db.DecodeStrictJSON(raw)
			if err != nil {
				return 0, nil, fmt.Errorf("evidence JSON: %w", err)
			}
			arr, _ := v.([]any)
			nonEmpty = nonEmpty || len(arr) > 0
			if room := evidenceRefScanLimit - len(lists[i]); room > 0 {
				lists[i] = append(lists[i], arr[:min(room, len(arr))]...)
			}
		}
		if nonEmpty {
			n++
		}
	}
	return n, lists, rows.Err()
}

// referencedEvidenceFile reports whether rel is a regular file under
// {DEFECTS_ROOT}/evidence. Confinement matches inventoryEvidencePaths: clean
// the joined path and reject anything that escapes that directory. The path
// is not passed through EvalSymlinks.
func referencedEvidenceFile(defectsRoot, rel string) bool {
	if rel == "" || strings.ContainsAny(rel, "\x00\n\r`") {
		return false
	}
	norm := strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	cleanRoot := filepath.Clean(paths.EvidenceDir(defectsRoot))
	cleanJoin := filepath.Clean(filepath.Join(defectsRoot, filepath.FromSlash(norm)))
	r, err := filepath.Rel(cleanRoot, cleanJoin)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return false
	}
	st, err := os.Stat(cleanJoin)
	return err == nil && st.Mode().IsRegular()
}

// migrateFilesystemIfNeeded is a no-op until tables are empty and legacy FS exists.
// Full markdown import lives with the write path; empty-table seed still runs after this.
func migrateFilesystemIfNeeded(_ *sql.DB, _ string) error { return nil }

// ssotName is the /health "ssot" value: "sqlite" (unchanged default) or "postgres".
func ssotName(sqlDB *sql.DB) string {
	if db.IsPostgres(sqlDB) {
		return "postgres"
	}
	return "sqlite"
}
