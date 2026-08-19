package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joe/defect-drainer-go/internal/analytics"
	gitpkg "github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/httpx"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/search"
	"github.com/joe/defect-drainer-go/internal/store"
)

func (a *App) postApp(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := store.CreateApp(a.DB, appFromMap(body))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"app": rec})
}

func (a *App) patchApp(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	has := map[string]bool{}
	for k := range body {
		has[k] = true
	}
	rec, err := store.UpdateAppSettings(a.DB, r.PathValue("id"), appFromMap(body), has)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "unknown") {
			code = http.StatusNotFound
		}
		httpx.WriteError(w, code, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"app": rec})
}

func (a *App) deleteApp(w http.ResponseWriter, r *http.Request) {
	if err := store.DeleteApp(a.DB, r.PathValue("id")); err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "unknown") {
			code = http.StatusNotFound
		}
		httpx.WriteError(w, code, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func appFromMap(b map[string]any) store.AppRecord {
	var rec store.AppRecord
	rec.Name, _ = b["name"].(string)
	rec.Description, _ = b["description"].(string)
	rec.WorkspaceRoot, _ = b["workspace_root"].(string)
	rec.RepoURL, _ = b["repo_url"].(string)
	rec.GrokSandbox, _ = b["grok_sandbox"].(string)
	rec.AgentToolchain, _ = b["agent_toolchain"].(string)
	if v, ok := b["allow_simulator_writes"].(bool); ok {
		rec.AllowSimulatorWrites = v
	}
	if raw, ok := b["verify_commands"]; ok {
		rec.VerifyCommands = parseVerifyCommands(raw)
	}
	rec.BaseRemote, _ = b["base_remote"].(string)
	rec.BaseBranch, _ = b["base_branch"].(string)
	if v, ok := b["default"].(bool); ok {
		rec.Default = v
	}
	rec.Repos = stringSlice(b["repos"])
	rec.RepoURLs = stringSlice(b["repo_urls"])
	if raw, ok := b["repo_entries"]; ok {
		rec.RepoEntries = parseEntries(raw)
	}
	return rec
}

func parseVerifyCommands(raw any) []store.VerifyCommand {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := []store.VerifyCommand{}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		v := store.VerifyCommand{}
		v.Repo, _ = m["repo"].(string)
		v.Command, _ = m["command"].(string)
		out = append(out, v)
	}
	return store.NormalizeVerifyCommands(out)
}

func parseEntries(raw any) []store.AppRepoEntry {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []store.AppRepoEntry
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, store.AppRepoEntry{Name: s, URL: ""})
			continue
		}
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		e := store.AppRepoEntry{}
		e.Name, _ = m["name"].(string)
		e.URL, _ = m["url"].(string)
		e.BaseSource, _ = m["base_source"].(string)
		e.BaseBranch, _ = m["base_branch"].(string)
		out = append(out, e)
	}
	return out
}

func stringSlice(v any) []string {
	switch t := v.(type) {
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, strings.TrimSpace(asString(x)))
		}
		return out
	case []string:
		return t
	case string:
		return storeSplitRepos(t)
	default:
		return nil
	}
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		s := strings.Trim(string(b), `"`)
		if s == "null" {
			return ""
		}
		return s
	}
}

func storeSplitRepos(raw string) []string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return nil
	}
	if strings.HasPrefix(t, "[") {
		var v any
		if err := json.Unmarshal([]byte(t), &v); err == nil {
			if arr, ok := v.([]any); ok {
				var out []string
				for _, x := range arr {
					s := strings.TrimSpace(asString(x))
					if s != "" {
						out = append(out, s)
					}
				}
				return out
			}
		}
	}
	var out []string
	for _, p := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == '\n' }) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) patchDefect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.IsSafeID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	has := map[string]bool{}
	for k := range body {
		has[k] = true
	}
	patch := store.DefectRecord{
		Title:        asString(body["title"]),
		AppID:        asString(body["app_id"]),
		Severity:     asString(body["severity"]),
		Status:       asString(body["status"]),
		Area:         asString(body["area"]),
		Client:       asString(body["client"]),
		Surface:      asString(body["surface"]),
		Repos:        stringSlice(body["repos"]),
		Labels:       stringSlice(body["labels"]),
		Related:      stringSlice(body["related"]),
		Source:       asString(body["source"]),
		KeyFiles:     stringSlice(body["key_files"]),
		Evidence:     stringSlice(body["evidence"]),
		FixEvidence:  stringSlice(body["fix_evidence"]),
		Summary:      asString(body["summary"]),
		Body:         asString(body["body"]),
		Resolution:   asString(body["resolution"]),
		ResolvedDate: asString(body["resolved_date"]),
		DuplicateOf:  asString(body["duplicate_of"]),
	}
	rec, err := a.Store.Update(id, patch, has)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defect": rec})
}

func (a *App) deleteDefect(w http.ResponseWriter, r *http.Request) {
	wipe := r.URL.Query().Get("evidence") == "1"
	ok, err := a.Store.Delete(r.PathValue("id"), wipe)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) resolveDefect(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = httpx.DecodeJSON(r, jsonBodyLimit, &body)
	fix := stringSlice(body["fix_evidence"])
	rec, err := a.Store.Resolve(r.PathValue("id"), asString(body["resolution"]), fix, false)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defect": rec})
}

func (a *App) reopenDefect(w http.ResponseWriter, r *http.Request) {
	rec, err := a.Store.Reopen(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defect": rec})
}

func (a *App) fixEvidence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseMultipartForm(maxMultipartMem); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	var files []struct {
		Filename string
		Data     []byte
	}
	if r.MultipartForm != nil {
		n := 0
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				if n >= maxFiles {
					break
				}
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
				f.Close()
				if err != nil || len(data) == 0 || int64(len(data)) > maxFileBytes {
					continue
				}
				files = append(files, struct {
					Filename string
					Data     []byte
				}{fh.Filename, data})
				n++
			}
		}
	}
	paths, err := a.Store.SaveEvidence(id, files, "fix")
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, err := a.Store.Get(id)
	if err != nil || cur == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	cur.FixEvidence = append(cur.FixEvidence, paths...)
	saved, err := a.Store.Write(*cur)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"defect": saved, "files": paths})
}

func (a *App) getAnalytics(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"analytics": analytics.GetSummary(a.DB, appID)})
}

func (a *App) getSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 40
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	prefer := q.Get("prefer")
	if prefer != "fts" && prefer != "opensearch" {
		prefer = ""
	}
	res := search.MultiSearch(a.DB, q.Get("q"), strings.TrimSpace(q.Get("app_id")),
		strings.TrimSpace(q.Get("artifact_type")), strings.TrimSpace(q.Get("severity")),
		strings.TrimSpace(q.Get("area")), strings.TrimSpace(q.Get("status")), prefer, limit)
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (a *App) getSearchStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := search.GetConfig()
	reachable := false
	if cfg.Enabled {
		reachable = search.Ping(cfg.URL)
	}
	var url any
	if cfg.URL != "" {
		url = cfg.URL
	} else {
		url = nil
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled":   cfg.Enabled,
		"url":       url,
		"index":     cfg.Index,
		"reachable": reachable,
		"outbox":    analytics.CountOutbox(a.DB),
	})
}

func (a *App) postSearchFlush(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, search.FlushOutbox(analytics.CountOutbox(a.DB)))
}

func (a *App) postSearchReindex(w http.ResponseWriter, _ *http.Request) {
	res, err := search.ReindexAll(a.DB, a.Store)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (a *App) postIntakeJSON(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	in := jobs.IntakeInput{
		Comment:  asString(body["comment"]),
		Severity: asString(body["severity"]),
		Client:   asString(body["client"]),
		Surface:  asString(body["surface"]),
		Area:     asString(body["area"]),
		Source:   asString(body["source"]),
		Reporter: asString(body["reporter"]),
		AppID:    asString(body["app_id"]),
		Mode:     asString(body["mode"]),
		Repos:    stringSlice(body["repos"]),
	}
	job, err := a.Jobs.EnqueueLocal(in)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *App) postIntakeMultipart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxMultipartMem); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	val := func(k string) string {
		if r.MultipartForm == nil {
			return ""
		}
		if vs := r.MultipartForm.Value[k]; len(vs) > 0 {
			return vs[0]
		}
		return r.FormValue(k)
	}
	var repos []string
	if r.MultipartForm != nil {
		for _, v := range r.MultipartForm.Value["repos"] {
			repos = append(repos, storeSplitRepos(v)...)
		}
	}
	var files []struct {
		Filename string
		Data     []byte
	}
	if r.MultipartForm != nil {
		n := 0
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				if n >= maxFiles {
					break
				}
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, _ := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
				f.Close()
				if len(data) == 0 || int64(len(data)) > maxFileBytes {
					continue
				}
				files = append(files, struct {
					Filename string
					Data     []byte
				}{fh.Filename, data})
				n++
			}
		}
	}
	in := jobs.IntakeInput{
		Comment:  val("comment"),
		Severity: val("severity"),
		Client:   val("client"),
		Surface:  val("surface"),
		Area:     val("area"),
		Source:   val("source"),
		Reporter: val("reporter"),
		AppID:    val("app_id"),
		Mode:     val("mode"),
		Repos:    repos,
		Files:    files,
	}
	job, err := a.Jobs.EnqueueLocal(in)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (a *App) getJobs(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"jobs": a.Jobs.ListNormalize()})
}

func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	j := a.Jobs.Get(r.PathValue("id"))
	if j == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (a *App) completeJob(w http.ResponseWriter, r *http.Request) {
	j, err := a.Jobs.Complete(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (a *App) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, err := a.Jobs.Cancel(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (a *App) getBatches(w http.ResponseWriter, _ *http.Request) {
	list, err := a.Batches.ListBatches()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"batches": list, "jobs": a.Batches.List()})
}

func (a *App) postBatch(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	startFix, _ := body["start_fix"].(bool)
	mode := asString(body["mode"])
	spawn := startFix && (mode == "grok" || mode == "")
	rec, job, err := a.Batches.CreateBatch(
		asString(body["app_id"]),
		asString(body["title"]),
		asString(body["goal"]),
		mode,
		stringSlice(body["defect_ids"]),
		startFix,
		spawn,
	)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if spawn {
		a.Batches.FlipInProgress(job.DefectIDs)
		appRec, _ := store.GetApp(a.DB, rec.AppID)
		primaries := map[string]string{}
		if appRec != nil {
			for _, e := range appRec.RepoEntries {
				if e.URL != "" && (e.BaseSource == "local" || strings.HasPrefix(e.URL, "/")) {
					primaries[e.Name] = e.URL
				}
			}
		}
		if len(primaries) == 0 {
			msg := "no product repos resolved for worktrees — set defect.repos and/or app.repos + workspace_root"
			a.Batches.WriteFailClosedBrief(job, a.Roots.DefectsRoot)
			a.Batches.MarkFailed(job.ID, rec.ID, msg)
			a.Batches.SyncBatchAndDefects(a.Batches.GetJob(job.ID), "failed")
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		root := filepath.Join(a.Roots.DataRoot, "batch-jobs", job.ID, "worktrees")
		wts, err := gitpkg.CreateBatchWorktrees(rec.ID, root, primaries)
		if err != nil {
			a.Batches.WriteFailClosedBrief(job, a.Roots.DefectsRoot)
			a.Batches.MarkFailed(job.ID, rec.ID, err.Error())
			a.Batches.SyncBatchAndDefects(a.Batches.GetJob(job.ID), "failed")
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(wts) == 0 {
			msg := "no worktrees — refusing Grok spawn"
			a.Batches.WriteFailClosedBrief(job, a.Roots.DefectsRoot)
			a.Batches.MarkFailed(job.ID, rec.ID, msg)
			a.Batches.SyncBatchAndDefects(a.Batches.GetJob(job.ID), "failed")
			httpx.WriteError(w, http.StatusBadRequest, msg)
			return
		}
		a.Batches.SetWorktrees(job.ID, wts)
		if err := a.Batches.StartSpawn(job.ID, a.Roots.DefectsRoot); err != nil {
			a.Batches.MarkFailed(job.ID, rec.ID, err.Error())
			a.Batches.SyncBatchAndDefects(a.Batches.GetJob(job.ID), "failed")
			httpx.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if updated := a.Batches.GetJob(job.ID); updated != nil {
			job = updated
		}
		if b2, err := a.Batches.GetBatch(rec.ID); err == nil && b2 != nil {
			rec = *b2
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"batch": rec, "job": job})
}

func (a *App) getBatch(w http.ResponseWriter, r *http.Request) {
	rec, err := a.Batches.GetBatch(r.PathValue("id"))
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"batch": rec})
}

func (a *App) getBatchJob(w http.ResponseWriter, r *http.Request) {
	j := a.Batches.GetJob(r.PathValue("id"))
	if j == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (a *App) deleteBatchJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !store.IsSafeJobID(id) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := a.Batches.DeleteJob(id); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) stopBatchJob(w http.ResponseWriter, r *http.Request) {
	j, err := a.Batches.Stop(r.PathValue("id"))
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		httpx.WriteError(w, code, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j})
}

func (a *App) rerunBatchJob(w http.ResponseWriter, r *http.Request) {
	j := a.Batches.GetJob(r.PathValue("id"))
	if j == nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if err := a.Batches.StartSpawn(j.ID, a.Roots.DefectsRoot); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": a.Batches.GetJob(j.ID)})
}

func (a *App) createPRs(w http.ResponseWriter, r *http.Request) {
	j, err := a.Batches.CreatePRs(r.PathValue("id"))
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		httpx.WriteError(w, code, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j, "prs": j.PRs})
}

func (a *App) refreshPRs(w http.ResponseWriter, r *http.Request) {
	j, err := a.Batches.RefreshPRs(r.PathValue("id"))
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			code = http.StatusNotFound
		}
		httpx.WriteError(w, code, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"job": j, "prs": j.PRs})
}

func (a *App) gitBranches(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source   string `json:"source"`
		Location string `json:"location"`
	}
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := gitpkg.ListRepoBranches(store.ParseBaseSource(body.Source, "origin"), body.Location)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (a *App) gitChooseFolder(w http.ResponseWriter, _ *http.Request) {
	res, err := gitpkg.ChooseLocalFolder()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
