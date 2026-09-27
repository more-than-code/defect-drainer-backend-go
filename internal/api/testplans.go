package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/httpx"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/store"
)

func writeOpErr(w http.ResponseWriter, err error) {
	var op *store.OpError
	if errors.As(err, &op) {
		httpx.WriteError(w, op.Status, op.Msg)
		return
	}
	code := http.StatusBadRequest
	if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "unknown") {
		code = http.StatusNotFound
	}
	httpx.WriteError(w, code, err.Error())
}

func decodeObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := httpx.DecodeJSON(r, jsonBodyLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

func parsePlanSteps(raw any) ([]store.PlanStepIn, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, &store.OpError{Status: http.StatusBadRequest, Msg: "steps must be an array"}
	}
	out := make([]store.PlanStepIn, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &store.OpError{Status: http.StatusBadRequest, Msg: "steps must be objects"}
		}
		out = append(out, store.PlanStepIn{
			Key: asString(m["key"]), Section: asString(m["section"]),
			Action: asString(m["action"]), Expected: asString(m["expected"]),
		})
	}
	return out, nil
}

func parseEnv(raw any) (map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, &store.OpError{Status: http.StatusBadRequest, Msg: "env must be a JSON object"}
	}
	return m, nil
}

func parseRefs(raw any) ([]store.RunRef, error) {
	if raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, &store.OpError{Status: http.StatusBadRequest, Msg: "refs must be an array"}
	}
	out := make([]store.RunRef, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, &store.OpError{Status: http.StatusBadRequest, Msg: "refs must be objects"}
		}
		out = append(out, store.RunRef{Repo: asString(m["repo"]), Ref: asString(m["ref"]), SHA: asString(m["sha"])})
	}
	return out, nil
}

func (a *App) getTestPlans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plans, err := store.ListPlans(a.DB, strings.TrimSpace(q.Get("app_id")), q.Get("include_archived") == "1")
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func (a *App) postTestPlan(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	rawSteps, hasSteps := body["steps"]
	if !hasSteps {
		httpx.WriteError(w, http.StatusBadRequest, "steps is required")
		return
	}
	steps, err := parsePlanSteps(rawSteps)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	env, err := parseEnv(body["env"])
	if err != nil {
		writeOpErr(w, err)
		return
	}
	res, err := store.UpsertPlan(a.DB, store.UpsertPlanInput{
		AppID: asString(body["app_id"]), Key: asString(body["key"]), Title: asString(body["title"]),
		Purpose: asString(body["purpose"]), Env: env, CreatedBy: asString(body["created_by"]), Steps: steps,
	})
	if err != nil {
		writeOpErr(w, err)
		return
	}
	code := http.StatusOK
	if res.Created {
		code = http.StatusCreated
	}
	httpx.WriteJSON(w, code, map[string]any{"plan": res.Plan, "created": res.Created, "changed": res.Changed})
}

func (a *App) getTestPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := store.GetPlan(a.DB, r.PathValue("id"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"plan": plan})
}

func (a *App) patchTestPlan(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	in := store.PatchPlanInput{Has: map[string]bool{}}
	for k := range body {
		in.Has[k] = true
	}
	if in.Has["title"] {
		in.Title = asString(body["title"])
	}
	if in.Has["purpose"] {
		in.Purpose = asString(body["purpose"])
	}
	if in.Has["env"] {
		env, err := parseEnv(body["env"])
		if err != nil {
			writeOpErr(w, err)
			return
		}
		in.Env = env
	}
	if in.Has["steps"] {
		steps, err := parsePlanSteps(body["steps"])
		if err != nil {
			writeOpErr(w, err)
			return
		}
		in.Steps = steps
	}
	if in.Has["archived"] {
		b, ok := body["archived"].(bool)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "archived must be a bool")
			return
		}
		in.Archived = b
	}
	plan, err := store.PatchPlan(a.DB, r.PathValue("id"), in)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"plan": plan})
}

func (a *App) deleteTestPlan(w http.ResponseWriter, r *http.Request) {
	archived, deleted, err := store.DeletePlan(a.DB, r.PathValue("id"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	if archived {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"archived": true})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (a *App) getTestRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runs, err := store.ListRuns(a.DB, strings.TrimSpace(q.Get("app_id")), strings.TrimSpace(q.Get("plan_id")))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (a *App) postTestRun(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	env, err := parseEnv(body["env"])
	if err != nil {
		writeOpErr(w, err)
		return
	}
	refs, err := parseRefs(body["refs"])
	if err != nil {
		writeOpErr(w, err)
		return
	}
	if asString(body["execute"]) == "agent" {
		a.postAgentTestRun(w, body, env, refs)
		return
	}
	run, err := store.CreateRun(a.DB, store.CreateRunInput{
		AppID: asString(body["app_id"]), PlanID: asString(body["plan_id"]), Title: asString(body["title"]),
		Execute: asString(body["execute"]), Executor: asString(body["executor"]), ExecutedBy: asString(body["executed_by"]),
		AccountLabel: asString(body["account_label"]), Env: env, Refs: refs,
	})
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func (a *App) getTestRun(w http.ResponseWriter, r *http.Request) {
	run, err := store.GetRun(a.DB, r.PathValue("id"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) deleteTestRun(w http.ResponseWriter, r *http.Request) {
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
	if err := store.DeleteRun(a.Store, r.PathValue("id"), r.URL.Query().Get("evidence") == "1"); err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *App) postTestRunStep(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
	run, err := store.AppendRunStep(a.DB, r.PathValue("id"), store.PlanStepIn{
		Key: asString(body["key"]), Section: asString(body["section"]),
		Action: asString(body["action"]), Expected: asString(body["expected"]),
	})
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"run": run})
}

func (a *App) patchTestRunStep(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	var status, actual *string
	if _, ok := body["status"]; ok {
		s := asString(body["status"])
		status = &s
	}
	if _, ok := body["actual"]; ok {
		s := asString(body["actual"])
		actual = &s
	}
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
	run, err := store.PatchRunStep(a.DB, r.PathValue("id"), r.PathValue("key"), status, actual)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) postTestRunStepEvidence(w http.ResponseWriter, r *http.Request) {
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
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
	run, err := store.AddRunStepEvidence(a.Store, r.PathValue("id"), r.PathValue("key"), files)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) finishTestRun(w http.ResponseWriter, r *http.Request) {
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
	run, err := store.FinishRun(a.DB, r.PathValue("id"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) abandonTestRun(w http.ResponseWriter, r *http.Request) {
	if err := a.liveRunConflict(r.PathValue("id")); err != nil {
		writeOpErr(w, err)
		return
	}
	run, err := store.AbandonRun(a.DB, r.PathValue("id"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) reviewTestRun(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	var steps []store.ReviewStepIn
	if raw, ok := body["steps"]; ok && raw != nil {
		arr, ok := raw.([]any)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "steps must be an array")
			return
		}
		for _, item := range arr {
			m, ok := item.(map[string]any)
			if !ok {
				httpx.WriteError(w, http.StatusBadRequest, "steps must be objects")
				return
			}
			steps = append(steps, store.ReviewStepIn{
				Key: asString(m["key"]), Review: asString(m["review"]), Note: asString(m["note"]),
			})
		}
	}
	run, err := store.ReviewRun(a.DB, r.PathValue("id"), store.ReviewInput{
		Reviewer: asString(body["reviewer"]), Note: asString(body["note"]), Steps: steps,
	})
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (a *App) fileTestRunStepDefect(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	in := store.FileDefectInput{
		Title: asString(body["title"]), Severity: asString(body["severity"]),
		Reporter: asString(body["reporter"]), Note: asString(body["note"]),
	}
	if raw, exists := body["repos"]; exists && raw != nil {
		in.HasRepos = true
		arr, ok := raw.([]any)
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "repos must be an array")
			return
		}
		for _, item := range arr {
			in.Repos = append(in.Repos, asString(item))
		}
	}
	def, run, err := store.FileRunStepDefect(a.Store, r.PathValue("id"), r.PathValue("key"), in)
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"defect": def, "run": run})
}

func (a *App) getTestPlanGrid(w http.ResponseWriter, r *http.Request) {
	grid, err := store.PlanGrid(a.DB, r.PathValue("id"), r.URL.Query().Get("runs"))
	if err != nil {
		writeOpErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"grid": grid})
}

func (a *App) getTestPlanExport(w http.ResponseWriter, r *http.Request) {
	plan, err := store.GetPlan(a.DB, r.PathValue("id"))
	if err != nil {
		var op *store.OpError
		if !errors.As(err, &op) {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeOpErr(w, err)
		return
	}
	app, err := store.GetApp(a.DB, plan.AppID)
	// GetApp reports a missing row as (nil, nil). Treat that as a failed load
	// so the export does not claim the plan has no test account.
	if err != nil || app == nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	body := jobs.RenderPlanExport(plan, app.TestAccounts, time.Now())
	name := fmt.Sprintf("%s-v%d.test-plan.md", plan.Key, plan.Version)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func (a *App) saveTestRunAsPlan(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeObject(w, r)
	if !ok {
		return
	}
	res, err := store.SaveRunAsPlan(a.DB, r.PathValue("id"), store.SaveAsPlanInput{
		PlanID: asString(body["plan_id"]), Key: asString(body["key"]), Title: asString(body["title"]),
	})
	if err != nil {
		writeOpErr(w, err)
		return
	}
	code := http.StatusOK
	if res.Created {
		code = http.StatusCreated
	}
	httpx.WriteJSON(w, code, map[string]any{"plan": res.Plan, "created": res.Created, "changed": res.Changed})
}
