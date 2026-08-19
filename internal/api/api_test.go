package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

func testApp(t *testing.T) *api.App {
	t.Helper()
	defects := t.TempDir()
	data := t.TempDir()
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthAppsEvidenceReadPath(t *testing.T) {
	app := testApp(t)

	h := doJSON(t, app.Handler, http.MethodGet, "/health", nil)
	if h.Code != 200 {
		t.Fatalf("health %d %s", h.Code, h.Body)
	}
	var health map[string]any
	if err := json.Unmarshal(h.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != true {
		t.Fatalf("ok %+v", health)
	}
	if health["service"] != "defect-drainer" {
		t.Fatalf("service %+v", health)
	}

	apps := doJSON(t, app.Handler, http.MethodGet, "/api/apps", nil)
	if apps.Code != 200 {
		t.Fatalf("apps %d %s", apps.Code, apps.Body)
	}
	var ap struct {
		DefaultAppID string           `json:"defaultAppId"`
		Apps         []map[string]any `json:"apps"`
	}
	if err := json.Unmarshal(apps.Body.Bytes(), &ap); err != nil {
		t.Fatal(err)
	}
	if ap.DefaultAppID != store.SeededTutoredWebappAppID {
		t.Fatalf("defaultAppId %s", ap.DefaultAppID)
	}
	found := false
	for _, a := range ap.Apps {
		if a["id"] == store.SeededTutoredWebappAppID {
			found = true
			if a["grok_sandbox"] != "strict" {
				t.Fatalf("sandbox %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("seeded webapp missing")
	}

	one := doJSON(t, app.Handler, http.MethodGet, "/api/apps/"+store.SeededTutoredWebappAppID, nil)
	if one.Code != 200 {
		t.Fatalf("get app %d", one.Code)
	}

	list := doJSON(t, app.Handler, http.MethodGet, "/api/defects?bucket=open", nil)
	if list.Code != 200 {
		t.Fatalf("defects %d", list.Code)
	}

	bad := doJSON(t, app.Handler, http.MethodGet, "/evidence/not-an-id/01.png", nil)
	if bad.Code != 400 {
		t.Fatalf("bad evidence %d", bad.Code)
	}
	miss := doJSON(t, app.Handler, http.MethodGet, "/evidence/DEF-20260817-x-abcd/01.png", nil)
	if miss.Code != 404 {
		t.Fatalf("missing evidence %d", miss.Code)
	}

	st := doJSON(t, app.Handler, http.MethodGet, "/api/search/status", nil)
	if st.Code != 200 {
		t.Fatalf("search status %d", st.Code)
	}
	var ss map[string]any
	_ = json.Unmarshal(st.Body.Bytes(), &ss)
	if ss["enabled"] != false {
		t.Fatalf("opensearch should be disabled: %+v", ss)
	}
}

func TestSecondLockFails(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	a, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err == nil || !strings.Contains(err.Error(), data) {
		t.Fatalf("expected lock error naming data dir, got %v", err)
	}
}

func TestIntakeOmitReporterAndResolveGate(t *testing.T) {
	app := testApp(t)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/intake/json", map[string]any{
		"comment":  "Submit button does nothing on practice hub",
		"severity": "P1",
		"client":   "web",
		"surface":  "practice hub",
		"app_id":   store.SeededTutoredWebappAppID,
		"repos":    []string{"webapp"},
		"mode":     "local",
	})
	if rec.Code != 202 {
		t.Fatalf("intake %d %s", rec.Code, rec.Body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	jobMap, _ := envelope["job"].(map[string]any)
	if jobMap == nil {
		t.Fatalf("no job: %s", rec.Body)
	}
	if _, ok := jobMap["id"]; ok {
		t.Fatalf("job object must not emit id: %s", rec.Body)
	}
	if jobMap["jobId"] == nil || jobMap["jobId"] == "" {
		t.Fatalf("missing jobId: %s", rec.Body)
	}
	if jobMap["source"] != "comment-only" {
		t.Fatalf("source %v", jobMap["source"])
	}
	if jobMap["severity"] != "P1" || jobMap["client"] != "web" || jobMap["surface"] != "practice hub" {
		t.Fatalf("passthrough fields: %+v", jobMap)
	}
	if jobMap["defectPath"] == nil || jobMap["defectPath"] == "" {
		t.Fatalf("defectPath missing: %+v", jobMap)
	}
	var out struct {
		Job struct {
			Status   string `json:"status"`
			DefectID string `json:"defectId"`
			JobID    string `json:"jobId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Job.Status != "completed" {
		t.Fatalf("status %s", out.Job.Status)
	}
	listedJobs := doJSON(t, app.Handler, http.MethodGet, "/api/jobs", nil)
	if listedJobs.Code != 200 || !strings.Contains(listedJobs.Body.String(), `"jobId"`) {
		t.Fatalf("GET /api/jobs missing jobId: %d %s", listedJobs.Code, listedJobs.Body)
	}
	if strings.Contains(listedJobs.Body.String(), `"id":"`+out.Job.JobID+`"`) {
		t.Fatalf("GET /api/jobs still emits job id: %s", listedJobs.Body)
	}
	oneJob := doJSON(t, app.Handler, http.MethodGet, "/api/jobs/"+out.Job.JobID, nil)
	if oneJob.Code != 200 || !strings.Contains(oneJob.Body.String(), `"jobId"`) {
		t.Fatalf("GET /api/jobs/{id} %d %s", oneJob.Code, oneJob.Body)
	}

	got := doJSON(t, app.Handler, http.MethodGet, "/api/defects/"+out.Job.DefectID, nil)
	if got.Code != 200 {
		t.Fatalf("get defect %d %s", got.Code, got.Body)
	}
	var wrap struct {
		Defect store.DefectRecord `json:"defect"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &wrap); err != nil {
		t.Fatal(err)
	}
	if wrap.Defect.Reporter != "" {
		t.Fatalf("omitted reporter must be empty, got %q", wrap.Defect.Reporter)
	}
	if wrap.Defect.CreatedAt == "" || wrap.Defect.Path == "" {
		t.Fatalf("created_at/path missing: %+v", wrap.Defect)
	}

	bad := doJSON(t, app.Handler, http.MethodPost, "/api/defects/"+out.Job.DefectID+"/resolve", map[string]any{})
	if bad.Code != 400 || !strings.Contains(bad.Body.String(), "fix_evidence") {
		t.Fatalf("resolve without proof: %d %s", bad.Code, bad.Body)
	}

	ok := doJSON(t, app.Handler, http.MethodPost, "/api/defects/"+out.Job.DefectID+"/resolve", map[string]any{
		"fix_evidence": []string{"evidence/" + out.Job.DefectID + "/fix-01.png"},
	})
	if ok.Code != 200 {
		t.Fatalf("resolve with virtual proof %d %s", ok.Code, ok.Body)
	}
}

func TestHostileBaseBranchFallback(t *testing.T) {
	app := testApp(t)
	res := doJSON(t, app.Handler, http.MethodPatch, "/api/apps/"+store.SeededTutoredWebappAppID, map[string]any{
		"base_branch": "--upload-pack=evil",
	})
	if res.Code != 200 {
		t.Fatalf("patch %d %s", res.Code, res.Body)
	}
	var wrap struct {
		App store.AppRecord `json:"app"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &wrap)
	if wrap.App.BaseBranch != "main" {
		t.Fatalf("hostile branch stored %q", wrap.App.BaseBranch)
	}
}

func TestBatchPhase1DecisionTable(t *testing.T) {
	app := testApp(t)
	// need a defect id for the batch body
	in := doJSON(t, app.Handler, http.MethodPost, "/api/intake/json", map[string]any{
		"comment": "batch target",
		"app_id":  store.SeededTutoredWebappAppID,
		"mode":    "local",
	})
	var job struct {
		Job struct {
			DefectID string `json:"defectId"`
		} `json:"job"`
	}
	_ = json.Unmarshal(in.Body.Bytes(), &job)

	grok := doJSON(t, app.Handler, http.MethodPost, "/api/batches", map[string]any{
		"app_id":     store.SeededTutoredWebappAppID,
		"title":      "fix",
		"defect_ids": []string{job.Job.DefectID},
		"start_fix":  true,
		"mode":       "grok",
	})
	// After worker PRs: seeded apps have no local URLs → worktrees fail → 400.
	if grok.Code != 400 {
		t.Fatalf("batch grok %d %s", grok.Code, grok.Body)
	}
	if !strings.Contains(grok.Body.String(), "worktrees") && !strings.Contains(grok.Body.String(), "repos") {
		t.Fatalf("expected worktree failure, got %s", grok.Body)
	}
	d := doJSON(t, app.Handler, http.MethodGet, "/api/defects/"+job.Job.DefectID, nil)
	var wrap struct {
		Defect store.DefectRecord `json:"defect"`
	}
	_ = json.Unmarshal(d.Body.Bytes(), &wrap)
	if wrap.Defect.Status == "in_progress" {
		t.Fatal("phase1 must not flip defects to in_progress")
	}

	manual := doJSON(t, app.Handler, http.MethodPost, "/api/batches", map[string]any{
		"app_id":     store.SeededTutoredWebappAppID,
		"title":      "manual",
		"defect_ids": []string{job.Job.DefectID},
		"start_fix":  true,
		"mode":       "manual",
	})
	if manual.Code != 201 {
		t.Fatalf("manual %d %s", manual.Code, manual.Body)
	}
	var manualOut struct {
		Batch struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"batch"`
		Job map[string]any `json:"job"`
	}
	if err := json.Unmarshal(manual.Body.Bytes(), &manualOut); err != nil {
		t.Fatal(err)
	}
	wantPath := "sqlite:batches/" + manualOut.Batch.ID
	if manualOut.Batch.Path != wantPath {
		t.Fatalf("batch path %q want %q", manualOut.Batch.Path, wantPath)
	}
	if _, ok := manualOut.Job["id"]; ok {
		t.Fatalf("batch job must not emit id: %s", manual.Body)
	}
	if manualOut.Job["jobId"] == nil {
		t.Fatalf("batch job missing jobId: %s", manual.Body)
	}
	listed := doJSON(t, app.Handler, http.MethodGet, "/api/batches", nil)
	if !strings.Contains(listed.Body.String(), `"path":"`+wantPath+`"`) {
		t.Fatalf("list batches missing path: %s", listed.Body)
	}
	var listedWrap struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listedWrap); err != nil {
		t.Fatal(err)
	}
	if len(listedWrap.Jobs) == 0 {
		t.Fatalf("GET /api/batches omitted jobs: %s", listed.Body)
	}
	foundJob := false
	for _, j := range listedWrap.Jobs {
		if j["jobId"] == manualOut.Job["jobId"] {
			foundJob = true
			if _, ok := j["id"]; ok {
				t.Fatalf("list job must not emit id: %#v", j)
			}
		}
	}
	if !foundJob {
		t.Fatalf("created job missing from GET /api/batches jobs: %s", listed.Body)
	}
	assertJobLogShape(t, manualOut.Job["log"])
	gotJob := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+manualOut.Job["jobId"].(string), nil)
	if gotJob.Code != 200 {
		t.Fatalf("get job %d %s", gotJob.Code, gotJob.Body)
	}
	var jobWrap struct {
		Job map[string]any `json:"job"`
	}
	if err := json.Unmarshal(gotJob.Body.Bytes(), &jobWrap); err != nil {
		t.Fatal(err)
	}
	assertJobLogShape(t, jobWrap.Job["log"])

	// F10: fail-closed worktree setup still writes BRIEF.md
	var grokWrap struct {
		Jobs []struct {
			ID     string `json:"jobId"`
			Status string `json:"status"`
		} `json:"jobs"`
	}
	_ = json.Unmarshal(listed.Body.Bytes(), &grokWrap)
	// The grok 400 above created a failed job before we listed. Re-list after
	// that request — listed already includes it if hydrate picked it up.
	listedAfter := doJSON(t, app.Handler, http.MethodGet, "/api/batches", nil)
	_ = json.Unmarshal(listedAfter.Body.Bytes(), &grokWrap)
	foundBrief := false
	for _, j := range grokWrap.Jobs {
		if j.Status != "failed" {
			continue
		}
		if _, err := os.Stat(filepath.Join(app.Roots.DataRoot, "batch-jobs", j.ID, "BRIEF.md")); err == nil {
			foundBrief = true
			brief, _ := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", j.ID, "BRIEF.md"))
			if !strings.Contains(string(brief), "NONE — refuse to edit") {
				t.Fatalf("fail-closed BRIEF.md missing NONE stanza:\n%s", brief)
			}
		}
	}
	if !foundBrief {
		t.Fatal("fail-closed worktree setup left no BRIEF.md")
	}
	gotBatch := doJSON(t, app.Handler, http.MethodGet, "/api/batches/"+manualOut.Batch.ID, nil)
	if !strings.Contains(gotBatch.Body.String(), `"path":"`+wantPath+`"`) {
		t.Fatalf("get batch missing path: %s", gotBatch.Body)
	}

	stop := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/bjob_x/stop", nil)
	if stop.Code != 404 && stop.Code != 400 {
		t.Fatalf("stop %d %s", stop.Code, stop.Body)
	}
}

func TestAnalyticsAndFTS(t *testing.T) {
	app := testApp(t)
	_, err := app.Store.Write(store.DefectRecord{
		ID: "DEF-20260809-analytics-login-fail-a1b2", AppID: store.SeededTutoredWebappAppID,
		Title: "Login button no-op on iOS", Severity: "P1", Status: "open", Area: "auth",
		Client: "ios", Surface: "login", Repos: []string{"webapp"},
		Source: "web-ui", Reported: "2026-08-09", Summary: "Tapping login does nothing",
		Body: "Expected navigate home. Actual no-op.", Bucket: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := doJSON(t, app.Handler, http.MethodGet, "/api/analytics?app_id="+store.SeededTutoredWebappAppID, nil)
	if sum.Code != 200 {
		t.Fatalf("analytics %d %s", sum.Code, sum.Body)
	}
	var payload struct {
		Analytics struct {
			Jobs struct {
				SuccessRate *float64 `json:"success_rate"`
			} `json:"jobs"`
			Prompts struct {
				Top []struct {
					LastUsed string `json:"last_used"`
				} `json:"top"`
			} `json:"prompts"`
		} `json:"analytics"`
	}
	if err := json.Unmarshal(sum.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Analytics.Jobs.SuccessRate != nil {
		t.Fatalf("success_rate must be null when denom=0, got %v", *payload.Analytics.Jobs.SuccessRate)
	}

	sr := doJSON(t, app.Handler, http.MethodGet, "/api/search?q=login&app_id="+store.SeededTutoredWebappAppID, nil)
	if sr.Code != 200 {
		t.Fatalf("search %d %s", sr.Code, sr.Body)
	}
	var res struct {
		Mode string `json:"mode"`
		Hits []struct {
			ArtifactType string `json:"artifact_type"`
			ID           string `json:"id"`
		} `json:"hits"`
	}
	_ = json.Unmarshal(sr.Body.Bytes(), &res)
	if res.Mode != "fts" && res.Mode != "like" && res.Mode != "recent" {
		t.Fatalf("mode %s", res.Mode)
	}
	found := false
	for _, h := range res.Hits {
		if h.ID == "DEF-20260809-analytics-login-fail-a1b2" && h.ArtifactType == "defect_summary" {
			found = true
		}
	}
	if !found && res.Mode == "fts" {
		t.Fatalf("expected FTS hit: %s", sr.Body)
	}
}

func TestEvidencePrefixAndFile(t *testing.T) {
	app := testApp(t)
	id := "DEF-20260817-ev-abcd"
	_, err := app.Store.Write(store.DefectRecord{
		ID: id, AppID: store.SeededTutoredWebappAppID, Title: "e",
		Severity: "P2", Status: "open", Area: "other", Client: "web",
		Reported: "2026-08-17", Summary: "e", Body: "e", Bucket: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(app.Roots.DefectsRoot, "evidence", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, app.Handler, http.MethodGet, "/evidence/"+id+"/01.png", nil)
	if rec.Code != 200 {
		t.Fatalf("evidence %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type %s", ct)
	}
	slash := doJSON(t, app.Handler, http.MethodGet, "/evidence/"+id+"/..%2F01.png", nil)
	if slash.Code != 400 && slash.Code != 404 {
		t.Fatalf("traversal %d", slash.Code)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
}

func TestWorkerSpawnAndGhPRRoutes(t *testing.T) {
	app := testApp(t)
	primary := t.TempDir()
	initGitRepo(t, primary)
	if err := os.WriteFile(filepath.Join(primary, "AGENTS.md"), []byte("# repo contract\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addCommit := exec.Command("git", "add", "AGENTS.md")
	addCommit.Dir = primary
	if out, err := addCommit.CombinedOutput(); err != nil {
		t.Fatalf("add AGENTS.md: %s", out)
	}
	cm := exec.Command("git", "commit", "-m", "agents")
	cm.Dir = primary
	cm.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cm.CombinedOutput(); err != nil {
		t.Fatalf("commit AGENTS.md: %s", out)
	}

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	// The mark file is the test's "spawn happened" sentinel, so it must be written
	// LAST — otherwise the reader can beat the args file into existence.
	writeExec(t, grok, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+mark+".args\"\necho GROK_SANDBOX=$GROK_SANDBOX >> \""+mark+".args\"\necho spawned > \""+mark+"\"\nexit 0\n")
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	gh := filepath.Join(t.TempDir(), "fake-gh")
	writeExec(t, gh, `#!/bin/sh
echo "$@" >> "`+ghLog+`"
args="$*"
case "$args" in
  *pr\ create*) echo 'https://github.com/example/demo/pull/7'; exit 0 ;;
  *pr\ view*) echo '{"state":"MERGED","mergedAt":"2026-08-17T12:00:00Z","number":7,"url":"https://github.com/example/demo/pull/7"}'; exit 0 ;;
  *pr\ list*) echo '[]'; exit 0 ;;
esac
exit 1
`)
	t.Setenv("GROK_BUILD_BIN", grok)
	t.Setenv("GH_BIN", gh)

	patched := doJSON(t, app.Handler, http.MethodPatch, "/api/apps/"+store.SeededTutoredWebappAppID, map[string]any{
		"grok_sandbox": "workspace",
		"repo_entries": []map[string]any{
			{"name": "demo", "url": primary, "base_source": "local", "base_branch": "main"},
		},
	})
	if patched.Code != 200 {
		t.Fatalf("patch app %d %s", patched.Code, patched.Body)
	}

	in := doJSON(t, app.Handler, http.MethodPost, "/api/intake/json", map[string]any{
		"comment": "worker target",
		"app_id":  store.SeededTutoredWebappAppID,
		"mode":    "local",
	})
	var intake struct {
		Job struct {
			DefectID string `json:"defectId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(in.Body.Bytes(), &intake); err != nil {
		t.Fatal(err)
	}

	created := doJSON(t, app.Handler, http.MethodPost, "/api/batches", map[string]any{
		"app_id":     store.SeededTutoredWebappAppID,
		"title":      "worker batch",
		"defect_ids": []string{intake.Job.DefectID},
		"start_fix":  true,
		"mode":       "grok",
	})
	if created.Code != 201 {
		t.Fatalf("batch %d %s", created.Code, created.Body)
	}
	var batchOut struct {
		Batch struct {
			Status string `json:"status"`
		} `json:"batch"`
		Job struct {
			ID        string                `json:"jobId"`
			Status    string                `json:"status"`
			Worktrees []git.WorktreeBinding `json:"worktrees"`
		} `json:"job"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &batchOut); err != nil {
		t.Fatal(err)
	}
	if batchOut.Job.ID == "" {
		t.Fatalf("jobId missing on create: %s", created.Body)
	}
	if strings.Contains(created.Body.String(), `"id":"`+batchOut.Job.ID+`"`) &&
		!strings.Contains(created.Body.String(), `"jobId":"`+batchOut.Job.ID+`"`) {
		t.Fatalf("job emitted id not jobId: %s", created.Body)
	}
	if len(batchOut.Job.Worktrees) == 0 {
		t.Fatalf("expected worktrees on job: %s", created.Body)
	}
	briefBytes, err := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", batchOut.Job.ID, "BRIEF.md"))
	if err != nil {
		t.Fatalf("BRIEF.md not written before spawn: %v", err)
	}
	brief := string(briefBytes)
	for _, want := range []string{"EDIT HERE", "DO NOT EDIT", "worker target", "AGENTS.md", "They bind you as much as this brief"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("BRIEF.md missing %q\n%s", want, brief)
		}
	}
	d := doJSON(t, app.Handler, http.MethodGet, "/api/defects/"+intake.Job.DefectID, nil)
	var dw struct {
		Defect store.DefectRecord `json:"defect"`
	}
	_ = json.Unmarshal(d.Body.Bytes(), &dw)
	if dw.Defect.Status != "in_progress" {
		t.Fatalf("defect should be in_progress after worker start, got %s", dw.Defect.Status)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatal("coding agent bin was not exec'd")
	}
	argsBytes, _ := os.ReadFile(mark + ".args")
	args := string(argsBytes)
	if argvFlagValue(args, "--sandbox") != "workspace" {
		t.Fatalf("spawn --sandbox want workspace, got %q in:\n%s", argvFlagValue(args, "--sandbox"), args)
	}
	if !strings.Contains(args, "GROK_SANDBOX=workspace") {
		t.Fatalf("GROK_SANDBOX: %s", args)
	}

	// wait until spawn is no longer running so create-prs is allowed
	idleUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(idleUntil) {
		got := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+batchOut.Job.ID, nil)
		if !strings.Contains(got.Body.String(), `"status":"running"`) &&
			!strings.Contains(got.Body.String(), `"status":"queued"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	wt := batchOut.Job.Worktrees[0].WorktreeAbs
	if err := os.WriteFile(filepath.Join(wt, "fix.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "fix.txt")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add: %s", out)
	}
	cmd = exec.Command("git", "commit", "-m", "fix")
	cmd.Dir = wt
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %s", out)
	}

	prs := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+batchOut.Job.ID+"/create-prs", nil)
	if prs.Code != 200 {
		t.Fatalf("create-prs %d %s", prs.Code, prs.Body)
	}
	var prOut struct {
		PRs []struct {
			URL      string `json:"url"`
			Status   string `json:"status"`
			Repo     string `json:"repo"`
			Branch   string `json:"branch"`
			GhNumber int    `json:"ghNumber"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(prs.Body.Bytes(), &prOut); err != nil {
		t.Fatal(err)
	}
	if len(prOut.PRs) == 0 || prOut.PRs[0].URL != "https://github.com/example/demo/pull/7" || prOut.PRs[0].Status != "created" {
		t.Fatalf("prs not persisted from gh: %s", prs.Body)
	}
	if prOut.PRs[0].Branch == "" || prOut.PRs[0].GhNumber != 7 {
		t.Fatalf("create-prs missing branch/ghNumber: %s", prs.Body)
	}
	if strings.Contains(prs.Body.String(), `"number":`) && !strings.Contains(prs.Body.String(), `"ghNumber":`) {
		t.Fatalf("prs still serialize number not ghNumber: %s", prs.Body)
	}
	jobJSON, err := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", batchOut.Job.ID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		PRs []struct {
			Branch   string `json:"branch"`
			GhNumber int    `json:"ghNumber"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(jobJSON, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.PRs) == 0 || persisted.PRs[0].Branch == "" || persisted.PRs[0].GhNumber != 7 {
		t.Fatalf("persisted job.json missing branch/ghNumber: %s", jobJSON)
	}
	ghBytes, _ := os.ReadFile(ghLog)
	if !strings.Contains(string(ghBytes), "pr create") {
		t.Fatalf("fake gh was not invoked for create: %s", ghBytes)
	}

	ref := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+batchOut.Job.ID+"/refresh-prs", nil)
	if ref.Code != 200 {
		t.Fatalf("refresh-prs %d %s", ref.Code, ref.Body)
	}
	var refOut struct {
		PRs []struct {
			GhState  string `json:"ghState"`
			MergedAt string `json:"mergedAt"`
		} `json:"prs"`
	}
	if err := json.Unmarshal(ref.Body.Bytes(), &refOut); err != nil {
		t.Fatal(err)
	}
	if len(refOut.PRs) == 0 || refOut.PRs[0].GhState != "merged" || refOut.PRs[0].MergedAt == "" {
		t.Fatalf("refresh did not apply gh view: %s", ref.Body)
	}
}

func TestSpawnRefusesEmptyWorktrees(t *testing.T) {
	_, _, err := git.StartCodingAgent(t.TempDir(), "BATCH-x", t.TempDir(), nil, "", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "without worktrees") {
		t.Fatalf("got %v", err)
	}
}

var jobLogLineRe = regexp.MustCompile(`^\[(info|warn|error)\] \[(DefectDrainer|Grok)\] `)

func assertJobLogShape(t *testing.T, log any) {
	t.Helper()
	arr, ok := log.([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("log is not a non-empty array: %#v", log)
	}
	for _, e := range arr {
		s, _ := e.(string)
		if !jobLogLineRe.MatchString(s) {
			t.Fatalf("log line missing [level] [source] shape: %q", s)
		}
	}
}

func argvFlagValue(s, flag string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

func TestLockFileCreated(t *testing.T) {
	app := testApp(t)
	if _, err := os.Stat(filepath.Join(app.Roots.DataRoot, "defect-drainer.lock")); err != nil {
		t.Fatal(err)
	}
	_ = db.NowIso()
}

func intakeDefect(t *testing.T, app *api.App, comment string) string {
	t.Helper()
	in := doJSON(t, app.Handler, http.MethodPost, "/api/intake/json", map[string]any{
		"comment": comment,
		"app_id":  store.SeededTutoredWebappAppID,
		"mode":    "local",
	})
	var wrap struct {
		Job struct {
			DefectID string `json:"defectId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(in.Body.Bytes(), &wrap); err != nil {
		t.Fatal(err)
	}
	if wrap.Job.DefectID == "" {
		t.Fatalf("no defectId: %s", in.Body)
	}
	return wrap.Job.DefectID
}

func patchWorkerApp(t *testing.T, app *api.App, primary string) {
	t.Helper()
	patched := doJSON(t, app.Handler, http.MethodPatch, "/api/apps/"+store.SeededTutoredWebappAppID, map[string]any{
		"grok_sandbox": "workspace",
		"repo_entries": []map[string]any{
			{"name": "demo", "url": primary, "base_source": "local", "base_branch": "main"},
		},
	})
	if patched.Code != 200 {
		t.Fatalf("patch app %d %s", patched.Code, patched.Body)
	}
}

func waitJobStatus(t *testing.T, app *api.App, jobID string, timeout time.Duration, notThese ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		got := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
		last = got.Body.String()
		var wrap struct {
			Job map[string]any `json:"job"`
		}
		_ = json.Unmarshal(got.Body.Bytes(), &wrap)
		st, _ := wrap.Job["status"].(string)
		ok := st != ""
		for _, n := range notThese {
			if st == n {
				ok = false
			}
		}
		if ok {
			return wrap.Job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not leave %v: %s", jobID, notThese, last)
	return nil
}

func TestHarvestViaStartSpawnCompletion(t *testing.T) {
	app := testApp(t)
	primary := t.TempDir()
	initGitRepo(t, primary)
	fixedID := intakeDefect(t, app, "harvest target with evidence")
	openID := intakeDefect(t, app, "harvest target left open")

	mark := filepath.Join(t.TempDir(), "spawned")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, `#!/bin/sh
mkdir -p "fix-evidence/`+fixedID+`"
printf 'png' > "fix-evidence/`+fixedID+`/fix-01.png"
echo stdout-from-agent
echo stderr-from-agent >&2
printf '%s\n' "$@" > "`+mark+`.args"
echo GROK_SANDBOX=$GROK_SANDBOX >> "`+mark+`.args"
echo spawned > "`+mark+`"
exit 0
`)
	t.Setenv("GROK_BUILD_BIN", grok)
	patchWorkerApp(t, app, primary)

	created := doJSON(t, app.Handler, http.MethodPost, "/api/batches", map[string]any{
		"app_id":     store.SeededTutoredWebappAppID,
		"title":      "harvest e2e",
		"defect_ids": []string{fixedID, openID},
		"start_fix":  true,
		"mode":       "grok",
	})
	if created.Code != 201 {
		t.Fatalf("batch %d %s", created.Code, created.Body)
	}
	var batchOut struct {
		Job struct {
			ID string `json:"jobId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &batchOut); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var resolved store.DefectRecord
	for time.Now().Before(deadline) {
		d := doJSON(t, app.Handler, http.MethodGet, "/api/defects/"+fixedID, nil)
		var wrap struct {
			Defect store.DefectRecord `json:"defect"`
		}
		_ = json.Unmarshal(d.Body.Bytes(), &wrap)
		if wrap.Defect.Status == "resolved" {
			resolved = wrap.Defect
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resolved.Status != "resolved" {
		t.Fatalf("defect %s not resolved via StartSpawn harvest", fixedID)
	}
	if len(resolved.FixEvidence) == 0 {
		t.Fatal("resolved defect has no fix_evidence")
	}
	still := doJSON(t, app.Handler, http.MethodGet, "/api/defects/"+openID, nil)
	var openWrap struct {
		Defect store.DefectRecord `json:"defect"`
	}
	_ = json.Unmarshal(still.Body.Bytes(), &openWrap)
	if openWrap.Defect.Status != "in_progress" {
		t.Fatalf("empty-handoff defect should stay in_progress, got %s", openWrap.Defect.Status)
	}

	job := waitJobStatus(t, app, batchOut.Job.ID, 5*time.Second, "running", "queued")
	assertJobLogShape(t, job["log"])
	logLines, _ := job["log"].([]any)
	sawOut, sawErr := false, false
	for _, e := range logLines {
		s, _ := e.(string)
		if strings.Contains(s, "[info] [Grok] stdout-from-agent") {
			sawOut = true
		}
		if strings.Contains(s, "[warn] [Grok] stderr-from-agent") {
			sawErr = true
		}
	}
	if !sawOut || !sawErr {
		t.Fatalf("agent streams missing from job log: %#v", logLines)
	}
	agentLog, err := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", batchOut.Job.ID, "agent.log"))
	if err != nil {
		t.Fatalf("agent.log: %v", err)
	}
	if !strings.Contains(string(agentLog), "stdout-from-agent") {
		t.Fatalf("agent.log missing stdout: %s", agentLog)
	}
	// The completion goroutine keeps writing job.json after the defect resolves
	// (batch sync, then a final log line). Returning before it finishes races
	// t.TempDir cleanup, which fails the package with "directory not empty".
	waitJobLogContains(t, app, batchOut.Job.ID, 5*time.Second, "batch complete")
}

// waitJobLogContains polls a job until its log carries want, so a test can wait
// for the completion goroutine's last write instead of guessing at timing.
func waitJobLogContains(t *testing.T, app *api.App, jobID string, within time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		got := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
		if strings.Contains(got.Body.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s log never contained %q", jobID, want)
}

func TestFailedSpawnErrorText(t *testing.T) {
	app := testApp(t)
	primary := t.TempDir()
	initGitRepo(t, primary)
	defID := intakeDefect(t, app, "failing spawn")
	grok := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, grok, "#!/bin/sh\nexit 1\n")
	t.Setenv("GROK_BUILD_BIN", grok)
	patchWorkerApp(t, app, primary)

	created := doJSON(t, app.Handler, http.MethodPost, "/api/batches", map[string]any{
		"app_id":     store.SeededTutoredWebappAppID,
		"title":      "fail e2e",
		"defect_ids": []string{defID},
		"start_fix":  true,
		"mode":       "grok",
	})
	if created.Code != 201 {
		t.Fatalf("batch %d %s", created.Code, created.Body)
	}
	var batchOut struct {
		Job struct {
			ID string `json:"jobId"`
		} `json:"job"`
	}
	_ = json.Unmarshal(created.Body.Bytes(), &batchOut)
	job := waitJobStatus(t, app, batchOut.Job.ID, 5*time.Second, "running", "queued")
	errStr, _ := job["error"].(string)
	if !strings.HasPrefix(errStr, "Grok exited code=") || !strings.Contains(errStr, "signal=") {
		t.Fatalf("job.error want Grok exited code=… signal=…, got %q", errStr)
	}
	if !strings.Contains(errStr, "code=1") {
		t.Fatalf("job.error should report exit 1: %q", errStr)
	}
}

func writeBatchJobJSON(t *testing.T, data, id string, job map[string]any) {
	t.Helper()
	dir := filepath.Join(data, "batch-jobs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func TestBatchJobDiffEndpoint(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	wt := t.TempDir()
	initGitRepo(t, wt)
	if err := os.WriteFile(filepath.Join(wt, "a.dart"), []byte("foo\n  bar\nbaz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "b.dart"), []byte("oldToken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, wt, "add", "a.dart", "b.dart")
	gitOut(t, wt, "commit", "-m", "base")
	baseSha := gitOut(t, wt, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(wt, "a.dart"), []byte("foo bar baz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "b.dart"), []byte("newToken\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wtBinding := []map[string]any{{
		"repo": "demo", "worktreeAbs": wt, "primaryAbs": wt, "branch": "main",
	}}
	writeBatchJobJSON(t, data, "bjob_diffok", map[string]any{
		"jobId": "bjob_diffok", "status": "completed", "kind": "batch",
		"worktrees": wtBinding,
		"diffHygiene": map[string]any{
			"noisy": false,
			"repos": []map[string]any{{"repo": "demo", "baseSha": baseSha}},
		},
	})
	writeBatchJobJSON(t, data, "bjob_diff409", map[string]any{
		"jobId": "bjob_diff409", "status": "completed", "kind": "batch",
		"worktrees":   wtBinding,
		"diffHygiene": map[string]any{"noisy": false, "repos": []map[string]any{{"repo": "demo"}}},
	})
	writeBatchJobJSON(t, data, "bjob_diff410", map[string]any{
		"jobId": "bjob_diff410", "status": "completed", "kind": "batch",
		"worktrees": []map[string]any{{
			"repo": "demo", "worktreeAbs": filepath.Join(t.TempDir(), "gone"), "primaryAbs": wt, "branch": "main",
		}},
		"diffHygiene": map[string]any{
			"repos": []map[string]any{{"repo": "demo", "baseSha": baseSha}},
		},
	})
	writeBatchJobJSON(t, data, "bjob_diffnowt", map[string]any{
		"jobId": "bjob_diffnowt", "status": "completed", "kind": "batch",
		"worktrees": wtBinding,
		"diffHygiene": map[string]any{
			"repos": []map[string]any{{"repo": "demo", "baseSha": baseSha}},
		},
	})

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	notFound := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/no-such/diff/demo", nil)
	if notFound.Code != 404 || !strings.Contains(notFound.Body.String(), `"not found"`) {
		t.Fatalf("unknown job: %d %s", notFound.Code, notFound.Body)
	}
	noWT := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_diffnowt/diff/other", nil)
	if noWT.Code != 404 || !strings.Contains(noWT.Body.String(), `no worktree`) || !strings.Contains(noWT.Body.String(), "other") {
		t.Fatalf("no worktree: %d %s", noWT.Code, noWT.Body)
	}
	conflict := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_diff409/diff/demo", nil)
	if conflict.Code != 409 || !strings.Contains(conflict.Body.String(), "no diff baseline recorded") {
		t.Fatalf("409: %d %s", conflict.Code, conflict.Body)
	}
	gone := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_diff410/diff/demo", nil)
	if gone.Code != 410 || !strings.Contains(gone.Body.String(), "worktree is gone:") {
		t.Fatalf("410: %d %s", gone.Code, gone.Body)
	}

	okReflow := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_diffok/diff/demo", nil)
	if okReflow.Code != 200 {
		t.Fatalf("200 reflow: %d %s", okReflow.Code, okReflow.Body)
	}
	var reflowBody struct {
		Repo  string `json:"repo"`
		Kind  string `json:"kind"`
		Hunks []struct {
			File   string `json:"file"`
			Reflow bool   `json:"reflow"`
		} `json:"hunks"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(okReflow.Body.Bytes(), &reflowBody); err != nil {
		t.Fatal(err)
	}
	if reflowBody.Repo != "demo" || reflowBody.Kind != "reflow" {
		t.Fatalf("body %+v", reflowBody)
	}
	if reflowBody.Total < 1 {
		t.Fatalf("expected reflow hunks: %s", okReflow.Body)
	}
	for _, h := range reflowBody.Hunks {
		if !h.Reflow {
			t.Fatalf("kind=reflow returned a non-reflow hunk: %+v", h)
		}
	}

	okAll := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/bjob_diffok/diff/demo?kind=all", nil)
	if okAll.Code != 200 {
		t.Fatalf("200 all: %d %s", okAll.Code, okAll.Body)
	}
	var allBody struct {
		Kind  string `json:"kind"`
		Hunks []struct {
			Reflow bool `json:"reflow"`
		} `json:"hunks"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(okAll.Body.Bytes(), &allBody); err != nil {
		t.Fatal(err)
	}
	if allBody.Kind != "all" || allBody.Total < 2 {
		t.Fatalf("kind=all should include token + reflow hunks: %s", okAll.Body)
	}
	sawReflow, sawToken := false, false
	for _, h := range allBody.Hunks {
		if h.Reflow {
			sawReflow = true
		} else {
			sawToken = true
		}
	}
	if !sawReflow || !sawToken {
		t.Fatalf("kind=all missing both kinds: %s", okAll.Body)
	}
}
