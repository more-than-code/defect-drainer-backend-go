package api_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/store"
)

func gitEnv(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "ERR " + err.Error() + " " + string(out)
	}
	return strings.TrimSpace(string(out))
}

func twoCommitRepo(t *testing.T) (dir, oldSHA, newSHA string) {
	t.Helper()
	dir = t.TempDir()
	initGitRepo(t, dir)
	oldSHA = gitEnv(dir, "rev-parse", "HEAD")
	if strings.HasPrefix(oldSHA, "ERR") || len(oldSHA) != 40 {
		t.Fatal(oldSHA)
	}
	if gitEnv(dir, "branch", "old") != "" && strings.HasPrefix(gitEnv(dir, "rev-parse", "old"), "ERR") {
		t.Fatal("branch old")
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := gitEnv(dir, "add", "README"); strings.HasPrefix(out, "ERR") {
		t.Fatal(out)
	}
	if out := gitEnv(dir, "commit", "-m", "two"); strings.HasPrefix(out, "ERR") {
		t.Fatal(out)
	}
	newSHA = gitEnv(dir, "rev-parse", "HEAD")
	if newSHA == oldSHA || len(newSHA) != 40 {
		t.Fatalf("shas old %s new %s", oldSHA, newSHA)
	}
	return dir, oldSHA, newSHA
}

func patchDemo(t *testing.T, h http.Handler, appID, primary string, extra map[string]any) {
	t.Helper()
	body := map[string]any{
		"repo_entries": []map[string]any{
			{"name": "demo", "url": primary, "base_source": "local", "base_branch": "main"},
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := doJSON(t, h, http.MethodPatch, "/api/apps/"+appID, body)
	if rec.Code != 200 {
		t.Fatalf("patch app %d %s", rec.Code, rec.Body)
	}
}

func postPlan(t *testing.T, h http.Handler, appID string) string {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "smoke", "title": "Smoke",
		"steps": []any{map[string]any{"key": "login", "action": "sign in", "expected": "home"}},
	})
	if rec.Code != 201 && rec.Code != 200 {
		t.Fatalf("plan %d %s", rec.Code, rec.Body)
	}
	return asMap(t, decodeBody(t, rec)["plan"])["id"].(string)
}

func waitJob(t *testing.T, h http.Handler, id string, accept ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		rec := doJSON(t, h, http.MethodGet, "/api/batch-jobs/"+id, nil)
		if rec.Code != 200 {
			t.Fatalf("job get %d %s", rec.Code, rec.Body)
		}
		job := asMap(t, decodeBody(t, rec)["job"])
		last, _ = job["status"].(string)
		for _, want := range accept {
			if last == want {
				return job
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s stuck at %s", id, last)
	return nil
}

func countRuns(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM test_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func dumpSQLite(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	var b strings.Builder
	for _, name := range names {
		if strings.ContainsAny(name, "\"';- ") {
			continue
		}
		rs, err := db.Query("SELECT * FROM " + name)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rs.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s\t%v\n", name, vals)
		}
		rs.Close()
	}
	return b.String()
}

func TestAgentTestRunRefHarvestAndNoShip(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, oldSHA, newSHA := twoCommitRepo(t)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	var batchesBefore int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&batchesBefore); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, `#!/bin/sh
set -e
wt=$(find worktrees -mindepth 1 -maxdepth 1 -type d | head -1)
echo x >> "$wt/README"
mkdir -p results evidence/login
printf 'STATUS: pass\nhome\n' > results/login.md
printf 'png' > evidence/login/shot.png
printf 'done\n' > RUN.md
exit 0
`)
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
		"refs": []any{map[string]any{"repo": "demo", "ref": "old"}},
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	run := asMap(t, body["run"])
	job := asMap(t, body["job"])
	if job["kind"] != "test_run" || job["testRunId"] != run["id"] || job["batchId"] != nil {
		t.Fatalf("job identity %#v", job)
	}
	if run["executor"] != "dd-job" || run["executed_by"] != "dd-job" {
		t.Fatalf("run actors %#v", run)
	}
	refs, _ := run["refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("refs %#v", run["refs"])
	}
	ref := asMap(t, refs[0])
	if ref["sha"] != oldSHA || ref["ref"] != "old" {
		t.Fatalf("recorded ref %#v want sha %s", ref, oldSHA)
	}
	wts, _ := job["worktrees"].([]any)
	if len(wts) != 1 {
		t.Fatalf("worktrees %#v", job["worktrees"])
	}
	wt := asMap(t, wts[0])["worktreeAbs"].(string)
	if gitEnv(wt, "rev-parse", "HEAD") != oldSHA {
		t.Fatalf("worktree HEAD %s", gitEnv(wt, "rev-parse", "HEAD"))
	}
	if gitEnv(wt, "rev-parse", "--abbrev-ref", "HEAD") != "HEAD" {
		t.Fatalf("not detached: %s", gitEnv(wt, "rev-parse", "--abbrev-ref", "HEAD"))
	}
	if gitEnv(primary, "rev-parse", "HEAD") != newSHA {
		t.Fatal("primary HEAD moved")
	}
	if branches := gitEnv(primary, "branch", "--list", "defect-drainer/*"); branches != "" {
		t.Fatalf("created a branch: %s", branches)
	}
	jobID := job["jobId"].(string)
	waitJob(t, app.Handler, jobID, "completed")
	got := doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+run["id"].(string), nil)
	finalRun := asMap(t, decodeBody(t, got)["run"])
	if finalRun["status"] != "awaiting_review" {
		t.Fatalf("run status %#v", finalRun["status"])
	}
	steps := finalRun["steps"].([]any)
	step := asMap(t, steps[0])
	if step["status"] != "pass" || len(step["evidence"].([]any)) != 1 {
		t.Fatalf("step %#v", step)
	}
	jobBody := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
	if !strings.Contains(jobBody.Body.String(), "worktree changes are never shipped from a test job") {
		t.Fatal("missing diff warning")
	}
	prs := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+jobID+"/create-prs", map[string]any{})
	if prs.Code != 409 || !strings.Contains(prs.Body.String(), "test run jobs do not ship") {
		t.Fatalf("create-prs %d %s", prs.Code, prs.Body)
	}
	preview := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID+"/ship-preview", nil)
	if preview.Code != 409 {
		t.Fatalf("ship-preview %d %s", preview.Code, preview.Body)
	}
	var batchesAfter int
	if err := app.DB.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&batchesAfter); err != nil {
		t.Fatal(err)
	}
	if batchesAfter != batchesBefore {
		t.Fatalf("batches %d → %d", batchesBefore, batchesAfter)
	}
	del := doJSON(t, app.Handler, http.MethodDelete, "/api/batch-jobs/"+jobID, nil)
	if del.Code != 200 {
		t.Fatalf("delete %d %s", del.Code, del.Body)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still present: %v", err)
	}
}

func TestAgentTestRunSimulatorQueue(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	release := filepath.Join(t.TempDir(), "release")
	mark := filepath.Join(t.TempDir(), "mark")
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, map[string]any{"allow_simulator_writes": true})
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("x"), 0o644) })
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\necho $$ > '"+mark+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.05; done\nmkdir -p results\nprintf 'done\\n' > RUN.md\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	first := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "title": "holder",
	})
	if first.Code != 201 {
		t.Fatalf("holder %d %s", first.Code, first.Body)
	}
	holderID := asMap(t, decodeBody(t, first)["job"])["jobId"].(string)
	waitJob(t, app.Handler, holderID, "running")
	second := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "title": "waiter",
	})
	if second.Code != 201 {
		t.Fatalf("waiter %d %s", second.Code, second.Body)
	}
	waiter := asMap(t, decodeBody(t, second)["job"])
	if waiter["status"] != "queued" {
		t.Fatalf("waiter status %#v", waiter["status"])
	}
	run := asMap(t, decodeBody(t, second)["run"])
	if run["status"] != "queued" {
		t.Fatalf("waiter run %#v", run["status"])
	}
	_ = os.WriteFile(release, []byte("x"), 0o644)
	waiterID := waiter["jobId"].(string)
	for _, id := range []string{holderID, waiterID} {
		stop := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+id+"/stop", nil)
		if stop.Code != 200 && stop.Code != 400 {
			t.Fatalf("stop %s %d %s", id, stop.Code, stop.Body)
		}
	}
	waitJob(t, app.Handler, holderID, "cancelled", "completed", "failed")
	waitJob(t, app.Handler, waiterID, "cancelled", "completed", "failed", "queued")
	time.Sleep(200 * time.Millisecond)
}

func TestAgentTestRunSecretRedacted(t *testing.T) {
	const secret = "S3cr3t-Value-42"
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QA_PASSWORD", secret)
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", "host-password-should-not-pass")
	primary, _, _ := twoCommitRepo(t)
	proof := filepath.Join(t.TempDir(), "proof")
	t.Setenv("DD_PROOF", proof)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, map[string]any{
		"test_accounts": []any{map[string]any{
			"label": "QA", "username": "ada", "environment": "local", "secret_ref": "env:QA_PASSWORD",
		}},
	})
	planID := postPlan(t, app.Handler, appID)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fake\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(`package main
import ("os"; "path/filepath")
func main() {
  v := os.Getenv("DD_TEST_ACCOUNT_PASSWORD")
  _ = os.WriteFile(os.Getenv("DD_PROOF"), []byte(v), 0o600)
  if v == "" { os.Exit(1) }
  mid := len(v) / 2
  _, _ = os.Stdout.Write([]byte(v[:mid]))
  _, _ = os.Stdout.Write([]byte(v[mid:] + "\n"))
  _, _ = os.Stderr.Write([]byte(v[:mid]))
  _, _ = os.Stderr.Write([]byte(v[mid:] + "\n"))
  cwd, _ := os.Getwd()
  _ = os.MkdirAll(filepath.Join(cwd, "results"), 0o755)
  _ = os.MkdirAll(filepath.Join(cwd, "evidence", "login"), 0o755)
  _ = os.WriteFile(filepath.Join(cwd, "results", "login.md"), []byte("STATUS: pass\n"+v+"\n"), 0o644)
  _ = os.WriteFile(filepath.Join(cwd, "evidence", "login", "shot.png"), []byte("png"), 0o644)
  _ = os.WriteFile(filepath.Join(cwd, "RUN.md"), []byte("done\n"), 0o644)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-grok")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake agent: %s", out)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent", "account_label": "QA",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	runID := asMap(t, body["run"])["id"].(string)
	jobID := asMap(t, body["job"])["jobId"].(string)
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("secret leaked in create response")
	}
	waitJob(t, app.Handler, jobID, "completed")
	gotProof, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotProof) != secret {
		t.Fatalf("child password mismatch len %d", len(gotProof))
	}
	handoff := filepath.Join(app.Roots.DataRoot, "batch-jobs", jobID)
	for _, name := range []string{"job.json", "BRIEF.md", "PROCESS.md", "agent.log"} {
		raw, err := os.ReadFile(filepath.Join(handoff, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret leaked in %s", name)
		}
	}
	brief, _ := os.ReadFile(filepath.Join(handoff, "BRIEF.md"))
	if strings.Contains(string(brief), "secret_ref") || !strings.Contains(string(brief), "the password is in $DD_TEST_ACCOUNT_PASSWORD") {
		t.Fatal("brief account section is wrong")
	}
	if !strings.Contains(string(brief), "Label: QA") || !strings.Contains(string(brief), "Username: ada") {
		t.Fatal("brief missing label or username")
	}
	jobRes := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
	runRes := doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil)
	if strings.Contains(jobRes.Body.String(), secret) {
		t.Fatal("secret leaked in job API")
	}
	if strings.Contains(runRes.Body.String(), secret) {
		t.Fatal("secret leaked in run API")
	}
	if strings.Contains(dumpSQLite(t, app.DB), secret) {
		t.Fatal("secret leaked into sqlite")
	}
	run := asMap(t, decodeBody(t, runRes)["run"])
	if run["status"] != "awaiting_review" {
		t.Fatalf("status %#v", run["status"])
	}
}

func TestAgentTestRunSecretRefused(t *testing.T) {
	primary, _, _ := twoCommitRepo(t)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, map[string]any{
		"test_accounts": []any{map[string]any{
			"label": "QA", "username": "ada", "environment": "local", "secret_ref": "env:QA_PASSWORD",
		}},
	})
	t.Setenv("QA_PASSWORD", "")
	before := countRuns(t, app.DB)
	unset := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "account_label": "QA",
	})
	if unset.Code != 400 || !strings.Contains(unset.Body.String(), "test account secret env QA_PASSWORD is not set") {
		t.Fatalf("unset %d %s", unset.Code, unset.Body)
	}
	if countRuns(t, app.DB) != before {
		t.Fatal("unset secret created a run")
	}
	if ents, _ := os.ReadDir(filepath.Join(app.Roots.DataRoot, "batch-jobs")); len(ents) != 0 {
		t.Fatalf("unset secret created a job: %d", len(ents))
	}
	t.Setenv("QA_PASSWORD", "short")
	short := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent", "account_label": "QA",
	})
	if short.Code != 400 || !strings.Contains(short.Body.String(), "test account secret env QA_PASSWORD is too short to redact safely") {
		t.Fatalf("short %d %s", short.Code, short.Body)
	}
	if countRuns(t, app.DB) != before {
		t.Fatal("short secret created a run")
	}
}

func TestAgentTestRunLiveGuardAndStop(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	mark := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("x"), 0o644) })
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\nmkdir -p results evidence/login\nprintf 'STATUS: pass\\nhome\\n' > results/login.md\nprintf 'png' > evidence/login/shot.png\nprintf 'done\\n' > RUN.md\necho ready > '"+mark+"'\nwhile [ ! -f '"+release+"' ]; do sleep 0.05; done\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	runID := asMap(t, decodeBody(t, rec)["run"])["id"].(string)
	jobID := asMap(t, decodeBody(t, rec)["job"])["jobId"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	patch := doJSON(t, app.Handler, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{"status": "fail"})
	if patch.Code != 409 || !strings.Contains(patch.Body.String(), "run is being executed by job "+jobID) {
		t.Fatalf("live patch %d %s", patch.Code, patch.Body)
	}
	stop := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+jobID+"/stop", nil)
	if stop.Code != 200 {
		t.Fatalf("stop %d %s", stop.Code, stop.Body)
	}
	waitJob(t, app.Handler, jobID, "cancelled")
	got := doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil)
	run := asMap(t, decodeBody(t, got)["run"])
	step := asMap(t, run["steps"].([]any)[0])
	if step["status"] != "pending" {
		t.Fatalf("stopped job harvested: %#v", step)
	}
	if run["status"] != "in_progress" {
		t.Fatalf("stop left run %#v", run["status"])
	}
	again := doJSON(t, app.Handler, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{"status": "fail"})
	if again.Code != 200 {
		t.Fatalf("patch after stop %d %s", again.Code, again.Body)
	}
}

func TestAgentTestRunFailedHarvestStaysOpen(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\nmkdir -p results\nprintf 'STATUS: fail\\nbroke\\n' > results/login.md\nprintf 'done\\n' > RUN.md\nexit 1\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	runID := asMap(t, decodeBody(t, rec)["run"])["id"].(string)
	jobID := asMap(t, decodeBody(t, rec)["job"])["jobId"].(string)
	var step map[string]any
	var run map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got := doJSON(t, app.Handler, http.MethodGet, "/api/test-runs/"+runID, nil)
		run = asMap(t, decodeBody(t, got)["run"])
		step = asMap(t, run["steps"].([]any)[0])
		if step["actual"] == "broke" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if step["status"] != "fail" || step["actual"] != "broke" {
		t.Fatalf("harvest %#v", step)
	}
	if run["status"] != "in_progress" {
		t.Fatalf("failed job finished the run: %#v", run["status"])
	}
	waitJob(t, app.Handler, jobID, "failed")
}

func TestAgentTestRunBadRefAbandons(t *testing.T) {
	primary, _, _ := twoCommitRepo(t)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "agent",
		"refs": []any{map[string]any{"repo": "demo", "ref": "does-not-exist"}},
	})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "demo: ref does-not-exist not found") {
		t.Fatalf("bad ref %d %s", rec.Code, rec.Body)
	}
	var status, jobID string
	if err := app.DB.QueryRow(`SELECT status, IFNULL(job_id, '') FROM test_runs`).Scan(&status, &jobID); err != nil {
		t.Fatal(err)
	}
	if status != "abandoned" || jobID == "" {
		t.Fatalf("run %s job %s", status, jobID)
	}
	raw, err := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", jobID, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status": "failed"`) {
		t.Fatalf("job not failed: %s", raw)
	}
	brief, err := os.ReadFile(filepath.Join(app.Roots.DataRoot, "batch-jobs", jobID, "BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "Spawn did not start") {
		t.Fatal("brief does not explain the failure")
	}
}

func TestAgentTestRunReviewedRestart(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	primary, _, _ := twoCommitRepo(t)
	app := testApp(t)
	appID := store.SeededTutoredWebappAppID
	patchDemo(t, app.Handler, appID, primary, nil)
	planID := postPlan(t, app.Handler, appID)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\nmkdir -p results evidence/login\nprintf 'STATUS: pass\\nhome\\n' > results/login.md\nprintf 'png' > evidence/login/shot.png\nprintf 'done\\n' > RUN.md\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	})
	if rec.Code != 201 {
		t.Fatalf("create %d %s", rec.Code, rec.Body)
	}
	runID := asMap(t, decodeBody(t, rec)["run"])["id"].(string)
	jobID := asMap(t, decodeBody(t, rec)["job"])["jobId"].(string)
	waitJob(t, app.Handler, jobID, "completed")
	review := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs/"+runID+"/review", map[string]any{
		"reviewer": "ada",
		"steps":    []any{map[string]any{"key": "login", "review": "accepted"}},
	})
	if review.Code != 200 {
		t.Fatalf("review %d %s", review.Code, review.Body)
	}
	// completed is set before the driver clears, so Re-run can still see a live agent.
	waitRerunnableReason(t, app.Handler, jobID, "test run is reviewed")
	rerun := doJSON(t, app.Handler, http.MethodPost, "/api/batch-jobs/"+jobID+"/rerun", nil)
	if rerun.Code != 409 || !strings.Contains(rerun.Body.String(), "test run is reviewed") {
		t.Fatalf("rerun %d %s", rerun.Code, rerun.Body)
	}
	view := doJSON(t, app.Handler, http.MethodGet, "/api/batch-jobs/"+jobID, nil)
	if !strings.Contains(view.Body.String(), "test run is reviewed") {
		t.Fatal("resumable/rerunnable missing the run reason")
	}
}
