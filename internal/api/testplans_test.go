package api_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/store"
)

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("json %s: %v", rec.Body, err)
	}
	return m
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("want object, got %#v", v)
	}
	return m
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("want array, got %#v", v)
	}
	return s
}

func TestTestPlanAndRunAPI(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	other := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{"name": "Other"})
	if other.Code != 201 {
		t.Fatalf("other app %d %s", other.Code, other.Body)
	}
	otherID := asMap(t, decodeBody(t, other)["app"])["id"].(string)

	if rec := doJSON(t, h, http.MethodGet, "/api/test-plans/TPLAN-missing", nil); rec.Code != 404 {
		t.Fatalf("missing plan %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": "app_0123456789abcdef", "key": "smoke", "title": "Smoke",
		"steps": []any{map[string]any{"key": "login", "action": "Sign in"}},
	}); rec.Code != 404 {
		t.Fatalf("unknown app %d %s", rec.Code, rec.Body)
	}

	steps := []any{
		map[string]any{"key": "login", "section": "auth", "action": "Sign in", "expected": "Home"},
		map[string]any{"key": "logout", "action": "Sign out", "expected": "Login"},
	}
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "smoke", "title": "Smoke", "purpose": "happy",
		"created_by": "ada", "env": map[string]any{"tier": "staging", "keep": true},
		"steps": steps,
	})
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	body := decodeBody(t, created)
	if body["created"] != true || body["changed"] != true {
		t.Fatalf("flags %+v", body)
	}
	plan := asMap(t, body["plan"])
	planID := plan["id"].(string)
	if plan["version"].(float64) != 1 || len(asSlice(t, plan["steps"])) != 2 {
		t.Fatalf("plan %+v", plan)
	}
	if asMap(t, asSlice(t, plan["steps"])[0])["position"].(float64) != 1 {
		t.Fatal("position")
	}

	again := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "smoke", "title": "Smoke", "purpose": "happy",
		"env": map[string]any{"keep": true, "tier": "staging"}, "steps": steps,
	})
	if again.Code != 200 {
		t.Fatalf("repost %d %s", again.Code, again.Body)
	}
	if decodeBody(t, again)["changed"] != false || decodeBody(t, again)["created"] != false {
		t.Fatalf("unchanged %+v", decodeBody(t, again))
	}

	reordered := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "smoke", "title": "Smoke", "purpose": "happy",
		"env":   map[string]any{"tier": "staging", "keep": true},
		"steps": []any{steps[1], steps[0]},
	})
	if reordered.Code != 200 || decodeBody(t, reordered)["changed"] != true {
		t.Fatalf("reorder %d %s", reordered.Code, reordered.Body)
	}
	if asMap(t, decodeBody(t, reordered)["plan"])["version"].(float64) != 2 {
		t.Fatal("version")
	}

	patched := doJSON(t, h, http.MethodPatch, "/api/test-plans/"+planID, map[string]any{"archived": true})
	if patched.Code != 200 {
		t.Fatalf("archive patch %d %s", patched.Code, patched.Body)
	}
	if asMap(t, decodeBody(t, patched)["plan"])["version"].(float64) != 2 {
		t.Fatal("archive bumped version")
	}
	if asMap(t, decodeBody(t, patched)["plan"])["archived"] != true {
		t.Fatal("not archived")
	}

	listed := doJSON(t, h, http.MethodGet, "/api/test-plans?app_id="+appID, nil)
	if listed.Code != 200 {
		t.Fatal(listed.Body)
	}
	for _, item := range asSlice(t, decodeBody(t, listed)["plans"]) {
		if asMap(t, item)["id"] == planID {
			t.Fatal("archived plan in default list")
		}
	}
	withArch := doJSON(t, h, http.MethodGet, "/api/test-plans?app_id="+appID+"&include_archived=1", nil)
	found := false
	for _, item := range asSlice(t, decodeBody(t, withArch)["plans"]) {
		m := asMap(t, item)
		if m["id"] == planID {
			found = true
			if m["step_count"].(float64) != 2 {
				t.Fatalf("step_count %+v", m)
			}
			if _, ok := m["steps"]; ok {
				t.Fatal("list included steps")
			}
		}
	}
	if !found {
		t.Fatal("archived plan missing from include_archived")
	}

	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual",
	}); rec.Code != 400 {
		t.Fatalf("archived plan run %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": otherID, "plan_id": planID, "execute": "manual",
	}); rec.Code != 400 {
		t.Fatalf("wrong app %d %s", rec.Code, rec.Body)
	}

	// Un-archive via re-post of the same content, then exercise runs.
	repost := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "smoke", "title": "Smoke", "purpose": "happy",
		"env":   map[string]any{"tier": "staging", "keep": true},
		"steps": []any{steps[1], steps[0]},
	})
	if repost.Code != 200 || asMap(t, decodeBody(t, repost)["plan"])["archived"] != false {
		t.Fatalf("unarchive %d %s", repost.Code, repost.Body)
	}
	if asMap(t, decodeBody(t, repost)["plan"])["version"].(float64) != 2 || decodeBody(t, repost)["changed"] != false {
		t.Fatalf("unarchive changed content %+v", decodeBody(t, repost))
	}

	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "agent",
	}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "execute=agent is not available yet") {
		t.Fatalf("agent %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "executor": "dd-job",
	}); rec.Code != 400 {
		t.Fatalf("dd-job %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": "TPLAN-missing", "execute": "manual",
	}); rec.Code != 404 {
		t.Fatalf("missing plan run %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodGet, "/api/test-runs/TRUN-missing", nil); rec.Code != 404 {
		t.Fatalf("missing run %d", rec.Code)
	}

	accounts := doJSON(t, h, http.MethodPatch, "/api/apps/"+appID, map[string]any{
		"test_accounts": []any{
			map[string]any{"label": "Admin", "username": "ada", "environment": "staging", "secret_ref": "env:ADMIN_PASSWORD"},
			map[string]any{"label": "Reader", "username": "bob", "environment": "local", "secret_ref": "op://vault/item"},
		},
	})
	if accounts.Code != 200 {
		t.Fatalf("accounts %d %s", accounts.Code, accounts.Body)
	}

	runRec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "executor": "agent-session",
		"executed_by": "ada", "account_label": "admin",
		"env":  map[string]any{"tier": "local"},
		"refs": []any{map[string]any{"repo": "ttd-webapp", "ref": "main", "sha": "abc1234"}},
	})
	if runRec.Code != 201 {
		t.Fatalf("run %d %s", runRec.Code, runRec.Body)
	}
	run := asMap(t, decodeBody(t, runRec)["run"])
	runID := run["id"].(string)
	if run["status"] != "in_progress" || run["plan_version"].(float64) != 2 || run["job_id"] != nil {
		t.Fatalf("run header %+v", run)
	}
	if run["executor"] != "agent-session" || run["title"] != "Smoke" {
		t.Fatalf("title/executor %+v", run)
	}
	env := asMap(t, run["env"])
	if env["tier"] != "local" || env["keep"] != true {
		t.Fatalf("merged env %+v", env)
	}
	acc := asMap(t, env["account"])
	if acc["label"] != "Admin" || acc["secret_ref"] != "env:ADMIN_PASSWORD" {
		t.Fatalf("account %+v", acc)
	}
	if len(asSlice(t, run["refs"])) != 1 || len(asSlice(t, run["steps"])) != 2 {
		t.Fatalf("refs/steps %+v", run)
	}
	if asMap(t, run["progress"])["total"].(float64) != 2 || asMap(t, run["progress"])["pending"].(float64) != 2 {
		t.Fatal(run["progress"])
	}
	if asMap(t, asSlice(t, run["steps"])[0])["defect_id"] != nil {
		t.Fatal("defect_id")
	}

	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual", "account_label": "nobody",
	}); rec.Code != 400 {
		t.Fatalf("unknown account %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual",
		"refs": []any{map[string]any{"repo": "not-a-repo", "ref": "main"}},
	}); rec.Code != 400 {
		t.Fatalf("bad repo %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual",
		"refs": []any{map[string]any{"repo": "ttd-webapp", "ref": "main", "sha": "GGGG"}},
	}); rec.Code != 400 {
		t.Fatalf("bad sha %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual",
		"refs": []any{map[string]any{"repo": "ttd-webapp", "ref": "../evil"}},
	}); rec.Code != 400 {
		t.Fatalf("bad ref %d %s", rec.Code, rec.Body)
	}

	// Editing the plan must not change the snapshotted run.
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-plans/"+planID, map[string]any{
		"title": "Smoke renamed",
	}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	got := doJSON(t, h, http.MethodGet, "/api/test-runs/"+runID, nil)
	if got.Code != 200 {
		t.Fatal(got.Body)
	}
	gotRun := asMap(t, decodeBody(t, got)["run"])
	if gotRun["title"] != "Smoke" || gotRun["plan_version"].(float64) != 2 {
		t.Fatalf("snapshot drifted %+v", gotRun)
	}
	if _, ok := gotRun["steps"]; !ok {
		t.Fatal("get run omitted steps")
	}

	runs := doJSON(t, h, http.MethodGet, "/api/test-runs?app_id="+appID+"&plan_id="+planID, nil)
	if runs.Code != 200 || len(asSlice(t, decodeBody(t, runs)["runs"])) != 1 {
		t.Fatalf("list runs %d %s", runs.Code, runs.Body)
	}
	if _, ok := asMap(t, asSlice(t, decodeBody(t, runs)["runs"])[0])["steps"]; ok {
		t.Fatal("run list included steps")
	}
	if asMap(t, asSlice(t, decodeBody(t, runs)["runs"])[0])["progress"] == nil {
		t.Fatal("list dropped progress")
	}

	appended := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps", map[string]any{
		"action": "Look around", "expected": "Notes", "section": "explore",
	})
	if appended.Code != 201 {
		t.Fatalf("append %d %s", appended.Code, appended.Body)
	}
	expl := asSlice(t, asMap(t, decodeBody(t, appended)["run"])["steps"])
	if asMap(t, expl[2])["key"] != "x-look-around" || asMap(t, expl[2])["exploratory"] != true {
		t.Fatalf("exploratory %+v", expl[2])
	}

	passed := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/logout", map[string]any{
		"status": "pass", "actual": "left",
	})
	if passed.Code != 200 {
		t.Fatalf("patch step %d %s", passed.Code, passed.Body)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{
		"status": "unverified",
	}); rec.Code != 400 {
		t.Fatalf("set unverified %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/nope", map[string]any{
		"status": "fail",
	}); rec.Code != 404 {
		t.Fatalf("unknown step %d %s", rec.Code, rec.Body)
	}

	upload := uploadStepEvidence(t, h, runID, "logout", "shot.png", []byte("\x89PNG"))
	if upload.Code != 200 {
		t.Fatalf("upload %d %s", upload.Code, upload.Body)
	}
	var evidencePath string
	for _, s := range asSlice(t, asMap(t, decodeBody(t, upload)["run"])["steps"]) {
		m := asMap(t, s)
		if m["key"] == "logout" {
			ev := asSlice(t, m["evidence"])
			if len(ev) != 1 {
				t.Fatalf("evidence %+v", m)
			}
			evidencePath = ev[0].(string)
		}
	}
	if !strings.Contains(evidencePath, "/s-logout-01.png") {
		t.Fatal(evidencePath)
	}
	file := evidencePath[strings.LastIndex(evidencePath, "/")+1:]
	gotFile := doJSON(t, h, http.MethodGet, "/evidence/"+runID+"/"+file, nil)
	if gotFile.Code != 200 {
		t.Fatalf("get evidence %d %s", gotFile.Code, gotFile.Body)
	}
	// Go's ServeMux redirects an unclean path (/../) with 307 before the handler.
	// Either that redirect or a 4xx is a refusal to serve the file.
	if rec := doJSON(t, h, http.MethodGet, "/evidence/"+runID+"/../x", nil); rec.Code == 200 {
		t.Fatalf("dotdot served the file")
	}
	if rec := doJSON(t, h, http.MethodGet, "/evidence/"+runID+"/s-..png", nil); rec.Code != 400 {
		t.Fatalf("dotdot segment %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodGet, "/evidence/BATCH-x/01.png", nil); rec.Code != 400 {
		t.Fatalf("batch evidence %d %s", rec.Code, rec.Body)
	}

	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/x-look-around", map[string]any{"status": "skipped"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{"status": "fail"}); rec.Code != 200 {
		t.Fatalf("fail login %d %s", rec.Code, rec.Body)
	}

	finished := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/finish", nil)
	if finished.Code != 200 {
		t.Fatalf("finish %d %s", finished.Code, finished.Body)
	}
	fin := asMap(t, decodeBody(t, finished)["run"])
	if fin["status"] != "awaiting_review" || fin["finished_at"] == nil {
		t.Fatalf("finished %+v", fin)
	}
	statusOf := map[string]string{}
	for _, s := range asSlice(t, fin["steps"]) {
		m := asMap(t, s)
		statusOf[m["key"].(string)] = m["status"].(string)
	}
	if statusOf["logout"] != "pass" || statusOf["login"] != "fail" || statusOf["x-look-around"] != "skipped" {
		t.Fatalf("statuses %+v", statusOf)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps", map[string]any{"action": "too late"}); rec.Code != 409 {
		t.Fatalf("append after finish %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{"actual": "x"}); rec.Code != 409 {
		t.Fatalf("patch after finish %d", rec.Code)
	}
	if rec := uploadStepEvidence(t, h, runID, "login", "late.png", []byte("png")); rec.Code != 409 {
		t.Fatalf("evidence after finish %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/finish", nil); rec.Code != 409 {
		t.Fatalf("second finish %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/review", map[string]any{
		"note": "early",
	}); rec.Code != 400 {
		t.Fatalf("missing reviewer %d %s", rec.Code, rec.Body)
	}

	reviewed := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/review", map[string]any{
		"reviewer": "ada", "note": "ship it",
		"steps": []any{
			map[string]any{"key": "logout", "review": "accepted"},
			map[string]any{"key": "login", "review": "disputed", "note": "flaky"},
			map[string]any{"key": "x-look-around", "review": "accepted"},
		},
	})
	if reviewed.Code != 200 {
		t.Fatalf("review %d %s", reviewed.Code, reviewed.Body)
	}
	rev := asMap(t, decodeBody(t, reviewed)["run"])
	if rev["status"] != "reviewed" || rev["reviewed_by"] != "ada" || rev["review_note"] != "ship it" {
		t.Fatalf("reviewed %+v", rev)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/abandon", nil); rec.Code != 409 {
		t.Fatalf("abandon reviewed %d", rec.Code)
	}

	saved := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/save-as-plan", map[string]any{
		"key": "from-run", "title": "From run",
	})
	if saved.Code != 201 || decodeBody(t, saved)["created"] != true {
		t.Fatalf("save-as-plan %d %s", saved.Code, saved.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/save-as-plan", map[string]any{
		"key": "from-run", "title": "Again",
	}); rec.Code != 409 {
		t.Fatalf("dup save %d %s", rec.Code, rec.Body)
	}
	replaced := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/save-as-plan", map[string]any{"plan_id": planID})
	if replaced.Code != 200 || decodeBody(t, replaced)["changed"] != true {
		t.Fatalf("replace %d %s", replaced.Code, replaced.Body)
	}

	// Second run: pass without evidence becomes unverified; review accepts it and status stays.
	barePlan := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "bare", "title": "Bare",
		"steps": []any{
			map[string]any{"key": "seen", "action": "Seen"},
			map[string]any{"key": "bare", "action": "Bare"},
			map[string]any{"key": "wait", "action": "Wait"},
		},
	})
	if barePlan.Code != 201 {
		t.Fatal(barePlan.Body)
	}
	bareID := asMap(t, decodeBody(t, barePlan)["plan"])["id"].(string)
	bareRun := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": bareID, "execute": "manual",
	})
	if bareRun.Code != 201 {
		t.Fatal(bareRun.Body)
	}
	bareRunID := asMap(t, decodeBody(t, bareRun)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+bareRunID+"/review", map[string]any{
		"reviewer": "ada", "steps": []any{},
	}); rec.Code != 409 {
		t.Fatalf("review in progress %d %s", rec.Code, rec.Body)
	}
	for _, keyStatus := range []struct{ key, status string }{{"seen", "pass"}, {"bare", "pass"}} {
		if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+bareRunID+"/steps/"+keyStatus.key, map[string]any{"status": keyStatus.status}); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}
	if rec := uploadStepEvidence(t, h, bareRunID, "seen", "ok.png", []byte("png")); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	bareDone := doJSON(t, h, http.MethodPost, "/api/test-runs/"+bareRunID+"/finish", nil)
	if bareDone.Code != 200 {
		t.Fatal(bareDone.Body)
	}
	bareStatus := map[string]string{}
	for _, s := range asSlice(t, asMap(t, decodeBody(t, bareDone)["run"])["steps"]) {
		m := asMap(t, s)
		bareStatus[m["key"].(string)] = m["status"].(string)
	}
	if bareStatus["seen"] != "pass" || bareStatus["bare"] != "unverified" || bareStatus["wait"] != "pending" {
		t.Fatalf("bare statuses %+v", bareStatus)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+bareRunID+"/review", map[string]any{
		"reviewer": "ada",
		"steps":    []any{map[string]any{"key": "wait", "review": "accepted"}},
	}); rec.Code != 400 {
		t.Fatalf("review pending %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+bareRunID+"/review", map[string]any{
		"reviewer": "ada",
		"steps":    []any{map[string]any{"key": "missing", "review": "accepted"}},
	}); rec.Code != 400 {
		t.Fatalf("review unknown %d %s", rec.Code, rec.Body)
	}
	accepted := doJSON(t, h, http.MethodPost, "/api/test-runs/"+bareRunID+"/review", map[string]any{
		"reviewer": "ada",
		"steps": []any{
			map[string]any{"key": "seen", "review": "accepted"},
			map[string]any{"key": "bare", "review": "accepted", "note": "no shot"},
		},
	})
	if accepted.Code != 200 {
		t.Fatalf("accept unverified %d %s", accepted.Code, accepted.Body)
	}
	for _, s := range asSlice(t, asMap(t, decodeBody(t, accepted)["run"])["steps"]) {
		m := asMap(t, s)
		if m["key"] == "bare" && (m["status"] != "unverified" || m["review"] != "accepted") {
			t.Fatalf("unverified review %+v", m)
		}
	}

	// Abandon from in_progress, and save-as-plan refused. Delete with evidence=1 removes the dir.
	left := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual", "title": "Left",
	})
	if left.Code != 201 {
		t.Fatal(left.Body)
	}
	leftID := asMap(t, decodeBody(t, left)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/steps", map[string]any{"action": "Poke"}); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	if rec := uploadStepEvidence(t, h, leftID, "x-poke", "a.png", []byte("png")); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	evDir := filepath.Join(app.Roots.DefectsRoot, "evidence", leftID)
	if _, err := os.Stat(evDir); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/abandon", nil); rec.Code != 200 {
		t.Fatalf("abandon %d %s", rec.Code, rec.Body)
	}
	if asMap(t, decodeBody(t, recBody(t, h, http.MethodGet, "/api/test-runs/"+leftID))["run"])["status"] != "abandoned" {
		t.Fatal("not abandoned")
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/save-as-plan", map[string]any{"key": "nope", "title": "No"}); rec.Code != 409 {
		t.Fatalf("save abandoned %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/finish", nil); rec.Code != 409 {
		t.Fatalf("finish abandoned %d", rec.Code)
	}
	del := doJSON(t, h, http.MethodDelete, "/api/test-runs/"+leftID+"?evidence=1", nil)
	if del.Code != 200 || decodeBody(t, del)["deleted"] != true {
		t.Fatalf("delete run %d %s", del.Code, del.Body)
	}
	if _, err := os.Stat(evDir); !os.IsNotExist(err) {
		t.Fatalf("evidence dir remains: %v", err)
	}
	if rec := doJSON(t, h, http.MethodDelete, "/api/test-runs/"+leftID, nil); rec.Code != 404 {
		t.Fatalf("delete missing %d", rec.Code)
	}

	// Plan with no runs is deleted; a referenced plan is archived.
	solo := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "solo", "title": "Solo",
		"steps": []any{map[string]any{"key": "only", "action": "Only"}},
	})
	soloID := asMap(t, decodeBody(t, solo)["plan"])["id"].(string)
	if rec := doJSON(t, h, http.MethodDelete, "/api/test-plans/"+soloID, nil); rec.Code != 200 || decodeBody(t, rec)["deleted"] != true {
		t.Fatalf("delete plan %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodDelete, "/api/test-plans/"+planID, nil); rec.Code != 200 || decodeBody(t, rec)["archived"] != true {
		t.Fatalf("archive delete %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodDelete, "/api/test-plans/TPLAN-missing", nil); rec.Code != 404 {
		t.Fatalf("delete missing plan %d", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-plans/TPLAN-missing", map[string]any{"title": "x"}); rec.Code != 404 {
		t.Fatalf("patch missing %d", rec.Code)
	}
}

func recBody(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, method, path, nil)
}

func uploadStepEvidence(t *testing.T, h http.Handler, runID, key, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/test-runs/"+runID+"/steps/"+key+"/evidence", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTestAccountsRejectSecrets(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	password := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{
		"name": "Pw",
		"test_accounts": []any{map[string]any{
			"label": "Admin", "username": "ada", "password": "hunter2",
		}},
	})
	if password.Code != 400 || !strings.Contains(password.Body.String(), "test_accounts only accept label, username, environment, secret_ref; passwords are never stored") {
		t.Fatalf("password %d %s", password.Code, password.Body)
	}
	badRef := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{
		"name": "Ref",
		"test_accounts": []any{map[string]any{
			"label": "Admin", "secret_ref": "env:bad-name",
		}},
	})
	if badRef.Code != 400 {
		t.Fatalf("env ref %d %s", badRef.Code, badRef.Body)
	}
	dup := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{
		"name": "Dup",
		"test_accounts": []any{
			map[string]any{"label": "Admin", "username": "a"},
			map[string]any{"label": "admin", "username": "b"},
		},
	})
	if dup.Code != 400 {
		t.Fatalf("dup %d %s", dup.Code, dup.Body)
	}
	createdApp := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{
		"name": "Ok",
		"test_accounts": []any{map[string]any{
			"label": "Admin", "username": "ada", "environment": "staging", "secret_ref": "env:ADMIN_PASSWORD",
		}},
	})
	if createdApp.Code != 201 {
		t.Fatalf("create %d %s", createdApp.Code, createdApp.Body)
	}
	id := asMap(t, decodeBody(t, createdApp)["app"])["id"].(string)
	got := doJSON(t, h, http.MethodGet, "/api/apps/"+id, nil)
	if got.Code != 200 {
		t.Fatal(got.Body)
	}
	list := asSlice(t, asMap(t, decodeBody(t, got)["app"])["test_accounts"])
	if len(list) != 1 || asMap(t, list[0])["secret_ref"] != "env:ADMIN_PASSWORD" || asMap(t, list[0])["label"] != "Admin" {
		t.Fatalf("round trip %+v", list)
	}
	seed := doJSON(t, h, http.MethodGet, "/api/apps/"+store.SeededTutoredWebappAppID, nil)
	accounts, ok := asMap(t, decodeBody(t, seed)["app"])["test_accounts"].([]any)
	if !ok || accounts == nil {
		t.Fatal("test_accounts null")
	}
}
