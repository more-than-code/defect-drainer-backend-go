// Package api registers the console HTTP contract on net/http ServeMux.
package api

import (
	"database/sql"
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

// Build resolves roots, flocks, opens SQLite, seeds, and returns the mux.
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
	sqlDB, err := db.Open(roots.DataRoot)
	if err != nil {
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
	m.HandleFunc("DELETE /api/batch-jobs/{id}", a.deleteBatchJob)
	m.HandleFunc("POST /api/batch-jobs/{id}/stop", a.stopBatchJob)
	m.HandleFunc("POST /api/batch-jobs/{id}/rerun", a.rerunBatchJob)
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
		"ssot":          "sqlite",
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
	if !store.IsSafeID(id) || strings.Contains(file, "..") || strings.Contains(file, "/") || strings.Contains(file, `\`) {
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

// migrateFilesystemIfNeeded is a no-op until tables are empty and legacy FS exists.
// Full markdown import lives with the write path; empty-table seed still runs after this.
func migrateFilesystemIfNeeded(_ *sql.DB, _ string) error { return nil }
