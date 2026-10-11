package api

import (
	"net/http"
	"path/filepath"

	gitpkg "github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/httpx"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/store"
)

func (a *App) postAgentTestRun(w http.ResponseWriter, body map[string]any, env map[string]any, refs []store.RunRef) {
	in := store.CreateRunInput{
		AppID: asString(body["app_id"]), PlanID: asString(body["plan_id"]), Title: asString(body["title"]),
		Execute: "agent", Executor: asString(body["executor"]), ExecutedBy: asString(body["executed_by"]),
		AccountLabel: asString(body["account_label"]), Env: env, Refs: refs,
	}
	previewEnv, previewRefs, err := store.PreviewAgentRun(a.DB, in)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	if err := jobs.CheckAccountSecret(previewEnv); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	appRec, err := store.GetApp(a.DB, in.AppID)
	if err != nil || appRec == nil {
		httpx.WriteError(w, http.StatusBadRequest, "app not found")
		return
	}
	reqs := testWorktreeRequests(appRec, previewRefs)
	if len(reqs) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "no product repos resolved for worktrees — set defect.repos and/or app.repos + workspace_root")
		return
	}
	run, err := store.CreateAgentRun(a.DB, in, "")
	if err != nil {
		writeOpErr(w, err)
		return
	}
	job, err := a.Batches.CreateTestJob(run.AppID, run.ID)
	if err != nil {
		_ = store.AbandonAgentRun(a.DB, run.ID)
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := store.SetRunJob(a.DB, run.ID, job.ID); err != nil {
		a.Batches.FailTestSetup(job, err.Error())
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	root := filepath.Join(a.Roots.DataRoot, "batch-jobs", job.ID, "worktrees")
	resolved, err := gitpkg.CreateDetachedWorktrees(root, detachRequests(reqs))
	if err != nil || len(resolved) == 0 {
		msg := "no worktrees — refusing Grok spawn"
		if err != nil {
			msg = err.Error()
		}
		a.Batches.FailTestSetup(job, msg)
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	wts := make([]gitpkg.WorktreeBinding, 0, len(resolved))
	runRefs := make([]store.RunRef, 0, len(resolved))
	for _, r := range resolved {
		wts = append(wts, r.Binding)
		runRefs = append(runRefs, store.RunRef{Repo: r.Binding.Repo, Ref: r.Ref, SHA: r.SHA})
	}
	a.Batches.SetWorktrees(job.ID, wts)
	if err := store.SetRunRefs(a.DB, run.ID, runRefs); err != nil {
		a.Batches.FailTestSetup(job, err.Error())
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.Batches.StartSpawn(job.ID, a.Roots.DefectsRoot); err != nil {
		live := a.Batches.GetJob(job.ID)
		if live == nil {
			live = job
		}
		a.Batches.FailTestSetup(live, err.Error())
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated := a.Batches.GetJob(job.ID)
	if updated == nil {
		updated = job
	}
	fresh, err := store.GetRun(a.DB, run.ID)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"run": fresh, "job": a.batchJobView(updated)})
}

func testWorktreeRequests(app *store.AppRecord, refs []store.RunRef) map[string]gitpkg.WorktreeRequest {
	all := worktreeRequests(app)
	if len(refs) == 0 {
		return all
	}
	out := map[string]gitpkg.WorktreeRequest{}
	for _, r := range refs {
		req, ok := all[r.Repo]
		if !ok {
			continue
		}
		req.Ref = r.Ref
		out[r.Repo] = req
	}
	return out
}

func detachRequests(in map[string]gitpkg.WorktreeRequest) map[string]gitpkg.DetachRequest {
	out := make(map[string]gitpkg.DetachRequest, len(in))
	for name, req := range in {
		out[name] = gitpkg.DetachRequest{
			PrimaryAbs: req.PrimaryAbs, Source: req.Source, Remote: req.Remote,
			Branch: req.Branch, Ref: req.Ref,
		}
	}
	return out
}

// liveRunConflict is the 409 returned while a dd-job is queued or running.
func (a *App) liveRunConflict(runID string) error {
	if a == nil || a.Batches == nil || a.DB == nil {
		return nil
	}
	run, err := store.GetRun(a.DB, runID)
	if err != nil || run.Executor != "dd-job" || run.JobID == nil || *run.JobID == "" {
		return nil
	}
	j := a.Batches.GetJob(*run.JobID)
	if j == nil {
		return nil
	}
	if j.Status == "queued" || j.Status == "running" {
		return &store.OpError{Status: http.StatusConflict, Msg: "run is being executed by job " + j.ID}
	}
	return nil
}
