package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestFileDefectFromStep(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	acc := doJSON(t, h, http.MethodPatch, "/api/apps/"+appID, map[string]any{
		"test_accounts": []any{map[string]any{
			"label": "QA", "username": "qa-user", "environment": "account-env-hidden",
			"secret_ref": "super-secret-ref-value",
		}},
	})
	if acc.Code != 200 {
		t.Fatalf("account %d %s", acc.Code, acc.Body)
	}

	action := "Open the dashboard " + strings.Repeat("x", 70)
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "file", "title": "File",
		"steps": []any{
			map[string]any{"key": "login", "section": "auth", "action": "Sign in", "expected": "Home"},
			map[string]any{"key": "dash", "action": action, "expected": "Home is shown"},
			map[string]any{"key": "sync", "action": "Wait for sync"},
		},
	})
	if created.Code != 201 {
		t.Fatalf("plan %d %s", created.Code, created.Body)
	}
	plan := asMap(t, decodeBody(t, created)["plan"])
	planID := plan["id"].(string)
	planVersion := int(plan["version"].(float64))

	runRec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Nightly smoke",
		"executed_by": "casey", "account_label": "QA",
		"env": map[string]any{
			"target": "ios-target", "device": "pixel-device",
			"api_base": "https://api.example.test", "flavor": "debug-flavor",
			"leak_key": "leak-value-xyz",
		},
		"refs": []any{map[string]any{"repo": "ttd-webapp", "ref": "main", "sha": "abc1234"}},
	})
	if runRec.Code != 201 {
		t.Fatalf("run %d %s", runRec.Code, runRec.Body)
	}
	runID := asMap(t, decodeBody(t, runRec)["run"])["id"].(string)
	for _, s := range runStepMaps(t, h, runID) {
		if s["defect_status"] != nil || s["defect_id"] != nil {
			t.Fatalf("fresh step %+v", s)
		}
	}

	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/login", map[string]any{"status": "pass"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	passFile := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/login/file-defect", map[string]any{})
	if passFile.Code != 400 {
		t.Fatalf("pass step %d %s", passFile.Code, passFile.Body)
	}

	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/dash", map[string]any{
		"status": "fail", "actual": "Blank chart",
	}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"title": strings.Repeat("t", 201),
	}); rec.Code != 400 {
		t.Fatalf("long title %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"severity": "P4",
	}); rec.Code != 400 {
		t.Fatalf("severity %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"reporter": strings.Repeat("r", 121),
	}); rec.Code != 400 {
		t.Fatalf("reporter %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"note": strings.Repeat("n", 4001),
	}); rec.Code != 400 {
		t.Fatalf("note %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"repos": []any{"not-a-repo"},
	}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unknown repo") {
		t.Fatalf("repo %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/missing/file-defect", map[string]any{}); rec.Code != 404 {
		t.Fatalf("missing step %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/TRUN-missing/steps/dash/file-defect", map[string]any{}); rec.Code != 404 {
		t.Fatalf("missing run %d %s", rec.Code, rec.Body)
	}

	png := []byte("png-one")
	jpg := []byte("jpg-two")
	if rec := uploadStepEvidence(t, h, runID, "dash", "one.png", png); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if rec := uploadStepEvidence(t, h, runID, "dash", "two.jpg", jpg); rec.Code != 200 {
		t.Fatal(rec.Body)
	}

	filed := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"note": "Saw a blank chart",
	})
	if filed.Code != 201 {
		t.Fatalf("file %d %s", filed.Code, filed.Body)
	}
	body := decodeBody(t, filed)
	def := asMap(t, body["defect"])
	defID := def["id"].(string)
	if !store.IsSafeID(defID) {
		t.Fatal(defID)
	}
	if def["app_id"] != appID || def["status"] != "open" || def["bucket"] != "open" || def["area"] != "other" || def["client"] != "web" {
		t.Fatalf("defect identity %+v", def)
	}
	if def["source"] != "test-run" || def["severity"] != "P2" || def["reporter"] != "casey" || def["surface"] != "" {
		t.Fatalf("defect meta %+v", def)
	}
	if def["reported"] != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("reported %v", def["reported"])
	}
	titleWant := string([]rune(action)[:80])
	if def["title"] != titleWant {
		t.Fatalf("title %q", def["title"])
	}
	if def["summary"] != "Test step failed: "+action {
		t.Fatalf("summary %q", def["summary"])
	}
	repos := asSlice(t, def["repos"])
	if len(repos) != 1 || repos[0] != "ttd-webapp" {
		t.Fatalf("repos %+v", repos)
	}
	if len(asSlice(t, def["labels"])) != 0 || len(asSlice(t, def["fix_evidence"])) != 0 {
		t.Fatalf("empty slices %+v", def)
	}
	related := asSlice(t, def["related"])
	if len(related) != 1 || related[0] != runID+"/steps/dash" {
		t.Fatalf("related %+v", related)
	}
	ev := asSlice(t, def["evidence"])
	if len(ev) != 2 {
		t.Fatalf("evidence %+v", ev)
	}
	defBody, _ := def["body"].(string)
	heads := markdownH2(defBody)
	wantHeads := []string{
		"Reporter notes", "Screenshot observations", "Repro", "Expected",
		"Actual", "Notes", "Acceptance", "Evidence",
	}
	if strings.Join(heads, "|") != strings.Join(wantHeads, "|") {
		t.Fatalf("sections %v", heads)
	}
	if !strings.Contains(defBody, "Saw a blank chart") {
		t.Fatal("note missing")
	}
	filedLine := "Filed from test run " + runID + " (Nightly smoke), step 2 dash."
	planLine := "Plan " + planID + " version " + strconv.Itoa(planVersion) + "."
	if !strings.Contains(defBody, filedLine) || !strings.Contains(defBody, planLine) {
		t.Fatalf("provenance missing:\n%s", defBody)
	}
	if !strings.Contains(defBody, "1. Sign in\n2. "+action) {
		t.Fatalf("repro missing:\n%s", defBody)
	}
	if strings.Contains(sectionText(defBody, "Repro"), "Wait for sync") {
		t.Fatal("repro included a later step")
	}
	if !strings.Contains(sectionText(defBody, "Expected"), "Home is shown") {
		t.Fatal("expected")
	}
	if strings.TrimSpace(sectionText(defBody, "Actual")) != "> Blank chart" {
		t.Fatalf("actual %q", sectionText(defBody, "Actual"))
	}
	for _, line := range []string{
		"- target: ios-target",
		"- device: pixel-device",
		"- api_base: https://api.example.test",
		"- flavor: debug-flavor",
		"- account: QA (qa-user)",
		"- ttd-webapp@main (abc1234)",
	} {
		if !strings.Contains(defBody, line) {
			t.Fatalf("notes missing %q\n%s", line, defBody)
		}
	}
	for _, hidden := range []string{"secret_ref", "super-secret-ref-value", "leak_key", "leak-value-xyz", "account-env-hidden"} {
		if strings.Contains(defBody, hidden) {
			t.Fatalf("body contains %q\n%s", hidden, defBody)
		}
	}
	items := jobs.AcceptanceCriteria(defBody)
	if len(items) != 1 || items[0] != "Home is shown (retest: test run step dash)" {
		t.Fatalf("acceptance %+v", items)
	}
	for i, rel := range []string{ev[0].(string), ev[1].(string)} {
		want := [][]byte{png, jpg}[i]
		if !bytesEqual(t, readRel(t, app.Roots.DefectsRoot, rel), want) {
			t.Fatalf("copied %s", rel)
		}
	}
	runSteps := asSlice(t, asMap(t, body["run"])["steps"])
	var dash map[string]any
	for _, raw := range runSteps {
		s := asMap(t, raw)
		if s["key"] == "dash" {
			dash = s
		} else if s["defect_status"] != nil {
			t.Fatalf("unlinked status %+v", s)
		}
	}
	if dash["defect_id"] != defID || dash["defect_status"] != "open" {
		t.Fatalf("linked step %+v", dash)
	}
	stepEv := asSlice(t, dash["evidence"])
	if len(stepEv) != 2 || !bytesEqual(t, readRel(t, app.Roots.DefectsRoot, stepEv[0].(string)), png) {
		t.Fatal("run evidence bytes")
	}

	again := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{})
	if again.Code != 409 || !strings.Contains(again.Body.String(), "step already has defect "+defID) {
		t.Fatalf("second %d %s", again.Code, again.Body)
	}

	if statusOf(t, h, runID, "dash") != "open" {
		t.Fatal("detail status")
	}
	if _, err := app.Store.Resolve(defID, "fixed in test", nil, true); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, h, runID, "dash") != "resolved" {
		t.Fatal("resolved status")
	}
	deleted, err := app.Store.Delete(defID, false)
	if err != nil || !deleted {
		t.Fatalf("delete defect %v %v", deleted, err)
	}
	if statusOf(t, h, runID, "dash") != "" {
		t.Fatal("status after delete")
	}
	for _, s := range runStepMaps(t, h, runID) {
		if s["key"] == "dash" && s["defect_id"] != defID {
			t.Fatalf("link cleared %+v", s)
		}
		if s["key"] == "dash" && s["defect_status"] != nil {
			t.Fatalf("status not null %+v", s["defect_status"])
		}
	}
	for _, rel := range []string{ev[0].(string), ev[1].(string)} {
		if !bytesEqual(t, readRel(t, app.Roots.DefectsRoot, rel), map[string][]byte{ev[0].(string): png, ev[1].(string): jpg}[rel]) {
			t.Fatal("defect evidence lost before run delete")
		}
	}
	runDir := filepath.Join(app.Roots.DefectsRoot, "evidence", runID)
	del := doJSON(t, h, http.MethodDelete, "/api/test-runs/"+runID+"?evidence=1", nil)
	if del.Code != 200 {
		t.Fatalf("delete run %d %s", del.Code, del.Body)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run evidence remains: %v", err)
	}
	if !bytesEqual(t, readRel(t, app.Roots.DefectsRoot, ev[0].(string)), png) || !bytesEqual(t, readRel(t, app.Roots.DefectsRoot, ev[1].(string)), jpg) {
		t.Fatal("defect evidence lost with the run")
	}

	left := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Left",
	})
	leftID := asMap(t, decodeBody(t, left)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+leftID+"/steps/dash", map[string]any{"status": "fail"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/abandon", nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	abandoned := doJSON(t, h, http.MethodPost, "/api/test-runs/"+leftID+"/steps/dash/file-defect", map[string]any{})
	if abandoned.Code != 409 {
		t.Fatalf("abandoned %d %s", abandoned.Code, abandoned.Body)
	}

	blockedRun := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Blocked", "executed_by": "casey",
	})
	blockedID := asMap(t, decodeBody(t, blockedRun)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+blockedID+"/steps/sync", map[string]any{"status": "blocked"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	blocked := doJSON(t, h, http.MethodPost, "/api/test-runs/"+blockedID+"/steps/sync/file-defect", map[string]any{
		"title": "Sync stuck", "severity": "P1", "reporter": "pat",
	})
	if blocked.Code != 201 {
		t.Fatalf("blocked %d %s", blocked.Code, blocked.Body)
	}
	bdef := asMap(t, decodeBody(t, blocked)["defect"])
	if bdef["title"] != "Sync stuck" || bdef["severity"] != "P1" || bdef["reporter"] != "pat" || bdef["summary"] != "Test step blocked: Wait for sync" {
		t.Fatalf("blocked defect %+v", bdef)
	}
	brepos := asSlice(t, bdef["repos"])
	if len(brepos) != 2 || brepos[0] != "ttd-webapp" || brepos[1] != "ttd-backend" {
		t.Fatalf("default repos %+v", brepos)
	}
	bbody, _ := bdef["body"].(string)
	if strings.TrimSpace(sectionText(bbody, "Actual")) != "> (not recorded)" {
		t.Fatalf("actual %q", sectionText(bbody, "Actual"))
	}
	bitems := jobs.AcceptanceCriteria(bbody)
	if len(bitems) != 1 || bitems[0] != `Step "Wait for sync" passes on retest` {
		t.Fatalf("blocked acceptance %+v", bitems)
	}
	if !strings.Contains(bbody, "step 3 sync.") || !strings.Contains(bbody, planID) {
		t.Fatalf("blocked provenance %s", bbody)
	}
}

func TestPlanGridBounds(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	if rec := doJSON(t, h, http.MethodGet, "/api/test-plans/TPLAN-missing/grid", nil); rec.Code != 404 {
		t.Fatalf("missing %d %s", rec.Code, rec.Body)
	}
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "bounds", "title": "Bounds",
		"steps": []any{map[string]any{"key": "only", "action": "Only", "expected": "Ok"}},
	})
	planID := asMap(t, decodeBody(t, created)["plan"])["id"].(string)
	for _, q := range []string{"runs=0", "runs=51", "runs=nope", "runs=-1"} {
		rec := doJSON(t, h, http.MethodGet, "/api/test-plans/"+planID+"/grid?"+q, nil)
		if rec.Code != 400 {
			t.Fatalf("%s %d %s", q, rec.Code, rec.Body)
		}
	}
	for i := 1; i <= 11; i++ {
		createReviewedRun(t, app, planID, "R", formatDay(i), map[string]stepMark{
			"only": {status: "fail", review: "accepted"},
		})
	}
	def := fetchGrid(t, h, planID, "")
	if len(def.Runs) != 10 || def.Runs[0].StartedAt != "2026-01-11T00:00:00.000Z" || def.Runs[9].StartedAt != "2026-01-02T00:00:00.000Z" {
		t.Fatalf("default %+v", startedOf(def))
	}
	blank := fetchGrid(t, h, planID, "runs=")
	if len(blank.Runs) != 10 {
		t.Fatalf("empty runs param %d", len(blank.Runs))
	}
	one := fetchGrid(t, h, planID, "runs=1")
	if len(one.Runs) != 1 || one.Runs[0].StartedAt != "2026-01-11T00:00:00.000Z" {
		t.Fatalf("runs=1 %+v", startedOf(one))
	}
	all := fetchGrid(t, h, planID, "runs=11")
	if len(all.Runs) != 11 || all.Runs[10].StartedAt != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("runs=11 %+v", startedOf(all))
	}
	cap50 := fetchGrid(t, h, planID, "runs=50")
	if len(cap50.Runs) != 11 {
		t.Fatalf("runs=50 %d", len(cap50.Runs))
	}
}

func TestPlanGridNeedsRetest(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	const resolvedAt = "2026-06-15T00:00:00.000Z"
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "regress", "title": "Regress",
		"steps": []any{
			map[string]any{"key": "login", "section": "auth", "action": "Sign in", "expected": "Home"},
			map[string]any{"key": "home", "action": "Open home", "expected": "Feed"},
		},
	})
	if created.Code != 201 {
		t.Fatalf("plan %d %s", created.Code, created.Body)
	}
	planID := asMap(t, decodeBody(t, created)["plan"])["id"].(string)

	trap := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Trap",
	})
	trapID := asMap(t, decodeBody(t, trap)["run"])["id"].(string)
	await := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Await",
	})
	awaitID := asMap(t, decodeBody(t, await)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+awaitID+"/finish", nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	abandon := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": "Abandon",
	})
	abandonID := asMap(t, decodeBody(t, abandon)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+abandonID+"/abandon", nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}

	fID := createReviewedRun(t, app, planID, "Filed", "2026-06-10T00:00:00.000Z", map[string]stepMark{
		"login": {status: "fail", review: "accepted"},
		"home":  {status: "fail", review: "accepted"},
	})
	loginDef := fileStep(t, h, fID, "login", nil)
	homeDef := fileStep(t, h, fID, "home", nil)
	openGrid := fetchGrid(t, h, planID, "")
	if stepOf(t, openGrid, "login").NeedsRetest || stepOf(t, openGrid, "home").NeedsRetest {
		t.Fatal("open defect needs retest")
	}
	if !defectLinked(stepOf(t, openGrid, "login"), loginDef, "open", fID) {
		t.Fatalf("open defects %+v", stepOf(t, openGrid, "login").Defects)
	}

	for _, id := range []string{loginDef, homeDef} {
		if _, err := app.Store.Resolve(id, "fixed", nil, true); err != nil {
			t.Fatal(err)
		}
		if _, err := app.DB.Exec(`UPDATE defects SET updated_at = ? WHERE id = ?`, resolvedAt, id); err != nil {
			t.Fatal(err)
		}
	}
	if !stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("resolved should need retest")
	}

	if _, err := app.DB.Exec(`UPDATE test_step_results SET status = 'pass', review = 'accepted' WHERE run_id = ? AND step_key = 'login'`, trapID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`UPDATE test_runs SET started_at = ? WHERE id = ?`, "2026-06-16T00:00:00.000Z", trapID); err != nil {
		t.Fatal(err)
	}
	if !stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("in-progress run cleared retest")
	}

	beforeID := createReviewedRun(t, app, planID, "Before", "2026-06-14T00:00:00.000Z", map[string]stepMark{
		"login": {status: "pass", evidence: true, review: "accepted"},
	})
	if !stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("run started before resolution cleared retest")
	}
	equalID := createReviewedRun(t, app, planID, "Equal", resolvedAt, map[string]stepMark{
		"login": {status: "pass", evidence: true, review: "accepted"},
	})
	if !stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("run started at resolution cleared retest")
	}
	disputeID := createReviewedRun(t, app, planID, "Dispute", "2026-06-20T00:00:00.000Z", map[string]stepMark{
		"login": {status: "pass", evidence: true, review: "disputed"},
	})
	if !stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("disputed pass cleared retest")
	}
	clearID := createReviewedRun(t, app, planID, "Clear", "2026-06-21T00:00:00.000Z", map[string]stepMark{
		"login": {status: "pass", evidence: true, review: "accepted"},
	})
	cleared := fetchGrid(t, h, planID, "")
	if stepOf(t, cleared, "login").NeedsRetest {
		t.Fatal("accepted pass did not clear retest")
	}
	if !stepOf(t, cleared, "home").NeedsRetest {
		t.Fatal("home cleared by a login pass")
	}
	newestID := createReviewedRun(t, app, planID, "Newest", "2026-06-25T00:00:00.000Z", map[string]stepMark{
		"login": {status: "fail", review: "accepted"},
	})
	if stepOf(t, fetchGrid(t, h, planID, ""), "login").NeedsRetest {
		t.Fatal("later fail reopened retest")
	}
	window := fetchGrid(t, h, planID, "runs=1")
	if len(window.Runs) != 1 || window.Runs[0].ID != newestID {
		t.Fatalf("window %+v", window.Runs)
	}
	if stepOf(t, window, "login").NeedsRetest {
		t.Fatal("needs_retest ignored reviewed runs outside the window")
	}
	if !defectLinked(stepOf(t, window, "login"), loginDef, "resolved", fID) {
		t.Fatalf("defects outside window %+v", stepOf(t, window, "login").Defects)
	}
	if len(stepOf(t, window, "login").Cells) != 1 || stepOf(t, window, "login").Cells[0] == nil || stepOf(t, window, "login").Cells[0].Status != "fail" {
		t.Fatal("window cell")
	}

	unverifiedID := createReviewedRun(t, app, planID, "Unverified", "2026-07-02T00:00:00.000Z", map[string]stepMark{
		"home": {status: "pass", review: "accepted"},
	})
	afterUnverified := fetchGrid(t, h, planID, "")
	if stepOf(t, afterUnverified, "home").NeedsRetest {
		t.Fatal("accepted unverified did not clear retest")
	}
	homeIdx := runIndex(afterUnverified, unverifiedID)
	homeCell := stepOf(t, afterUnverified, "home").Cells[homeIdx]
	if homeCell == nil || homeCell.Status != "unverified" || homeCell.Review != "accepted" {
		t.Fatalf("unverified cell %+v", homeCell)
	}

	patched := doJSON(t, h, http.MethodPatch, "/api/test-plans/"+planID, map[string]any{
		"steps": []any{
			map[string]any{"key": "login", "section": "auth", "action": "Sign in", "expected": "Home"},
			map[string]any{"key": "home", "action": "Open home", "expected": "Feed"},
			map[string]any{"key": "extra", "section": "more", "action": "Extra step", "expected": "Ok"},
		},
	})
	if patched.Code != 200 || asMap(t, decodeBody(t, patched)["plan"])["version"].(float64) != 2 {
		t.Fatalf("patch %d %s", patched.Code, patched.Body)
	}
	extraID := createReviewedRun(t, app, planID, "Extra", "2026-08-01T00:00:00.000Z", map[string]stepMark{
		"extra": {status: "fail", review: "accepted"},
	})
	extraDef := fileStep(t, h, extraID, "extra", nil)
	if _, err := app.Store.Update(extraDef, store.DefectRecord{Status: "wontfix"}, map[string]bool{"status": true}); err != nil {
		t.Fatal(err)
	}
	g := fetchGrid(t, h, planID, "")
	assertAligned(t, g)
	if g.Plan.ID != planID || g.Plan.Key != "regress" || g.Plan.Title != "Regress" || g.Plan.Version != 2 {
		t.Fatalf("plan %+v", g.Plan)
	}
	if stepOf(t, g, "extra").NeedsRetest {
		t.Fatal("wontfix set needs_retest")
	}
	if !defectLinked(stepOf(t, g, "extra"), extraDef, "wontfix", extraID) {
		t.Fatalf("wontfix defects %+v", stepOf(t, g, "extra").Defects)
	}
	seen := map[string]bool{}
	var prev string
	for i, r := range g.Runs {
		seen[r.ID] = true
		if r.ReviewedBy != "ada" || r.ReviewedAt == nil || *r.ReviewedAt == "" {
			t.Fatalf("review meta %+v", r)
		}
		if i > 0 && r.StartedAt > prev {
			t.Fatalf("order %s then %s", prev, r.StartedAt)
		}
		prev = r.StartedAt
		if r.Refs == nil {
			t.Fatalf("nil refs %+v", r)
		}
	}
	for _, hidden := range []string{trapID, awaitID, abandonID} {
		if seen[hidden] {
			t.Fatalf("non-reviewed run %s included", hidden)
		}
	}
	for _, id := range []string{fID, beforeID, equalID, disputeID, clearID, newestID, unverifiedID, extraID} {
		if !seen[id] {
			t.Fatalf("reviewed run %s missing", id)
		}
	}
	if pv := g.Runs[runIndex(g, fID)].PlanVersion; pv == nil || *pv != 1 {
		t.Fatal("old plan_version")
	}
	if pv := g.Runs[runIndex(g, extraID)].PlanVersion; pv == nil || *pv != 2 {
		t.Fatal("new plan_version")
	}
	extraStep := stepOf(t, g, "extra")
	if extraStep.Position != 3 || extraStep.Section != "more" || extraStep.Action != "Extra step" {
		t.Fatalf("extra step %+v", extraStep)
	}
	if extraStep.Cells[runIndex(g, beforeID)] != nil || extraStep.Cells[runIndex(g, fID)] != nil {
		t.Fatal("run without the step should be a null cell")
	}
	extraCell := extraStep.Cells[runIndex(g, extraID)]
	if extraCell == nil || extraCell.RunID != extraID || extraCell.Status != "fail" || extraCell.Review != "accepted" || extraCell.DefectID == nil || *extraCell.DefectID != extraDef {
		t.Fatalf("extra cell %+v", extraCell)
	}
	loginCell := stepOf(t, g, "login").Cells[runIndex(g, fID)]
	if loginCell == nil || loginCell.DefectID == nil || *loginCell.DefectID != loginDef || loginCell.Status != "fail" {
		t.Fatalf("filed cell %+v", loginCell)
	}
	disputeCell := stepOf(t, g, "login").Cells[runIndex(g, disputeID)]
	if disputeCell == nil || disputeCell.Status != "pass" || disputeCell.Review != "disputed" || disputeCell.DefectID != nil {
		t.Fatalf("dispute cell %+v", disputeCell)
	}
	if stepOf(t, g, "login").Section != "auth" || stepOf(t, g, "login").Position != 1 {
		t.Fatal("login step shape")
	}
}

type stepMark struct {
	status   string
	evidence bool
	review   string
}

type gridCell struct {
	RunID    string  `json:"run_id"`
	Status   string  `json:"status"`
	Review   string  `json:"review"`
	DefectID *string `json:"defect_id"`
}

type gridDefect struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	RunID  string `json:"run_id"`
}

type gridStep struct {
	Key         string       `json:"key"`
	Position    int          `json:"position"`
	Section     string       `json:"section"`
	Action      string       `json:"action"`
	NeedsRetest bool         `json:"needs_retest"`
	Defects     []gridDefect `json:"defects"`
	Cells       []*gridCell  `json:"cells"`
}

type gridRun struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	PlanVersion *int           `json:"plan_version"`
	StartedAt   string         `json:"started_at"`
	ReviewedAt  *string        `json:"reviewed_at"`
	ReviewedBy  string         `json:"reviewed_by"`
	Refs        []store.RunRef `json:"refs"`
}

type planGrid struct {
	Plan struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Title   string `json:"title"`
		Version int    `json:"version"`
	} `json:"plan"`
	Runs  []gridRun  `json:"runs"`
	Steps []gridStep `json:"steps"`
}

func createReviewedRun(t *testing.T, app *api.App, planID, title, started string, marks map[string]stepMark) string {
	t.Helper()
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": store.SeededTutoredWebappAppID, "plan_id": planID, "execute": "manual",
		"title": title, "executed_by": "casey",
	})
	if rec.Code != 201 {
		t.Fatalf("create %s: %d %s", title, rec.Code, rec.Body)
	}
	runID := asMap(t, decodeBody(t, rec)["run"])["id"].(string)
	var reviews []any
	for key, mark := range marks {
		if rec := doJSON(t, app.Handler, http.MethodPatch, "/api/test-runs/"+runID+"/steps/"+key, map[string]any{"status": mark.status}); rec.Code != 200 {
			t.Fatalf("patch %s %s: %d %s", title, key, rec.Code, rec.Body)
		}
		if mark.evidence {
			if rec := uploadStepEvidence(t, app.Handler, runID, key, key+".png", []byte("ev-"+key)); rec.Code != 200 {
				t.Fatalf("evidence %s %s: %d %s", title, key, rec.Code, rec.Body)
			}
		}
		reviews = append(reviews, map[string]any{"key": key, "review": mark.review})
	}
	if rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs/"+runID+"/finish", nil); rec.Code != 200 {
		t.Fatalf("finish %s: %d %s", title, rec.Code, rec.Body)
	}
	if rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs/"+runID+"/review", map[string]any{
		"reviewer": "ada", "steps": reviews,
	}); rec.Code != 200 {
		t.Fatalf("review %s: %d %s", title, rec.Code, rec.Body)
	}
	if started != "" {
		if _, err := app.DB.Exec(`UPDATE test_runs SET started_at = ? WHERE id = ?`, started, runID); err != nil {
			t.Fatal(err)
		}
	}
	return runID
}

func fileStep(t *testing.T, h http.Handler, runID, key string, body any) string {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	rec := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/"+key+"/file-defect", body)
	if rec.Code != 201 {
		t.Fatalf("file %s: %d %s", key, rec.Code, rec.Body)
	}
	return asMap(t, decodeBody(t, rec)["defect"])["id"].(string)
}

func fetchGrid(t *testing.T, h http.Handler, planID, rawQuery string) planGrid {
	t.Helper()
	path := "/api/test-plans/" + planID + "/grid"
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	rec := doJSON(t, h, http.MethodGet, path, nil)
	if rec.Code != 200 {
		t.Fatalf("grid %s: %d %s", rawQuery, rec.Code, rec.Body)
	}
	var wrap struct {
		Grid planGrid `json:"grid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wrap); err != nil {
		t.Fatal(err)
	}
	return wrap.Grid
}

func stepOf(t *testing.T, g planGrid, key string) gridStep {
	t.Helper()
	for _, s := range g.Steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no step %s", key)
	return gridStep{}
}

func runIndex(g planGrid, id string) int {
	for i, r := range g.Runs {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func defectLinked(step gridStep, id, status, runID string) bool {
	for _, d := range step.Defects {
		if d.ID == id && d.Status == status && d.RunID == runID {
			return true
		}
	}
	return false
}

func assertAligned(t *testing.T, g planGrid) {
	t.Helper()
	for _, s := range g.Steps {
		if len(s.Cells) != len(g.Runs) {
			t.Fatalf("step %s cells %d runs %d", s.Key, len(s.Cells), len(g.Runs))
		}
	}
}

func startedOf(g planGrid) []string {
	out := make([]string, len(g.Runs))
	for i, r := range g.Runs {
		out[i] = r.StartedAt
	}
	return out
}

func formatDay(day int) string {
	return "2026-01-" + itoa(day) + "T00:00:00.000Z"
}

func itoa(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func runStepMaps(t *testing.T, h http.Handler, runID string) []map[string]any {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/api/test-runs/"+runID, nil)
	if rec.Code != 200 {
		t.Fatalf("get run %d %s", rec.Code, rec.Body)
	}
	raw := asSlice(t, asMap(t, decodeBody(t, rec)["run"])["steps"])
	out := make([]map[string]any, len(raw))
	for i, s := range raw {
		out[i] = asMap(t, s)
	}
	return out
}

func statusOf(t *testing.T, h http.Handler, runID, key string) string {
	t.Helper()
	for _, s := range runStepMaps(t, h, runID) {
		if s["key"] == key {
			if s["defect_status"] == nil {
				return ""
			}
			return s["defect_status"].(string)
		}
	}
	t.Fatalf("missing step %s", key)
	return ""
}

func markdownH2(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "## ")))
		}
	}
	return out
}

func sectionText(body, name string) string {
	parts := strings.Split(body, "\n## ")
	for _, p := range parts {
		p = strings.TrimPrefix(p, "## ")
		line, rest, _ := strings.Cut(p, "\n")
		if strings.TrimSpace(line) == name {
			if i := strings.Index(rest, "\n## "); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	return ""
}

func readRel(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bytesEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
