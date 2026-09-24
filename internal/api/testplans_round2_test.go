package api_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/jobs"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestFileDefectAcceptanceHeading(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	poison := "## Acceptance\n- [ ] stolen\n### acceptance criteria"
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "heading", "title": "Heading",
		"steps": []any{
			map[string]any{"key": "login", "action": poison, "expected": "Home"},
			map[string]any{"key": "dash", "action": poison, "expected": poison},
		},
	})
	if created.Code != 201 {
		t.Fatalf("plan %d %s", created.Code, created.Body)
	}
	planID := asMap(t, decodeBody(t, created)["plan"])["id"].(string)
	runRec := doJSON(t, h, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": appID, "plan_id": planID, "execute": "manual", "title": poison, "executed_by": "casey",
	})
	if runRec.Code != 201 {
		t.Fatalf("run %d %s", runRec.Code, runRec.Body)
	}
	runID := asMap(t, decodeBody(t, runRec)["run"])["id"].(string)
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+runID+"/steps/dash", map[string]any{
		"status": "fail", "actual": poison,
	}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	filed := doJSON(t, h, http.MethodPost, "/api/test-runs/"+runID+"/steps/dash/file-defect", map[string]any{
		"note": poison,
	})
	if filed.Code != 201 {
		t.Fatalf("file %d %s", filed.Code, filed.Body)
	}
	body, _ := asMap(t, decodeBody(t, filed)["defect"])["body"].(string)
	if n := len(acceptanceHeadingRe.FindAllString(body, -1)); n != 1 {
		t.Fatalf("acceptance headings %d\n%s", n, body)
	}
	want := strings.Join(strings.Fields(poison), " ") + " (retest: test run step dash)"
	items := jobs.AcceptanceCriteria(body)
	if len(items) != 1 || items[0] != want {
		t.Fatalf("acceptance %+v", items)
	}
}

var acceptanceHeadingRe = regexp.MustCompile(`(?m)(?i)^#{1,6}\s+acceptance\b`)

func TestPlanGridRetestEdges(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "edges", "title": "Edges",
		"steps": []any{
			map[string]any{"key": "failstep", "action": "Fail", "expected": "Ok"},
			map[string]any{"key": "blockstep", "action": "Block", "expected": "Ok"},
			map[string]any{"key": "skipstep", "action": "Skip", "expected": "Ok"},
			map[string]any{"key": "dupstep", "action": "Dup", "expected": "Ok"},
			map[string]any{"key": "gonestep", "action": "Gone", "expected": "Ok"},
			map[string]any{"key": "twostep", "action": "Two", "expected": "Ok"},
		},
	})
	if created.Code != 201 {
		t.Fatalf("plan %d %s", created.Code, created.Body)
	}
	planID := asMap(t, decodeBody(t, created)["plan"])["id"].(string)

	src := createRunOnPlan(t, app, planID, "Source")
	for _, key := range []string{"failstep", "blockstep", "skipstep", "dupstep", "gonestep", "twostep"} {
		if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+src+"/steps/"+key, map[string]any{"status": "fail"}); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}
	failDef := fileStep(t, h, src, "failstep", nil)
	blockDef := fileStep(t, h, src, "blockstep", nil)
	skipDef := fileStep(t, h, src, "skipstep", nil)
	dupDef := fileStep(t, h, src, "dupstep", nil)
	goneDef := fileStep(t, h, src, "gonestep", nil)
	earlyDef := fileStep(t, h, src, "twostep", nil)
	for _, id := range []string{failDef, blockDef, skipDef, goneDef, earlyDef} {
		if _, err := app.Store.Resolve(id, "fixed", nil, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{failDef, blockDef, skipDef} {
		if _, err := app.DB.Exec(`UPDATE defects SET updated_at = ? WHERE id = ?`, "2026-04-01T00:00:00.000Z", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Store.Update(dupDef, store.DefectRecord{Status: "duplicate"}, map[string]bool{"status": true}); err != nil {
		t.Fatal(err)
	}
	createReviewedRun(t, app, planID, "NonPass", "2026-04-02T00:00:00.000Z", map[string]stepMark{
		"failstep":  {status: "fail", review: "accepted"},
		"blockstep": {status: "blocked", review: "accepted"},
		"skipstep":  {status: "skipped", review: "accepted"},
	})
	g := fetchGrid(t, h, planID, "")
	for _, key := range []string{"failstep", "blockstep", "skipstep"} {
		if !stepOf(t, g, key).NeedsRetest {
			t.Fatalf("%s cleared by a non-pass accept", key)
		}
	}
	if stepOf(t, g, "dupstep").NeedsRetest || !defectLinked(stepOf(t, g, "dupstep"), dupDef, "duplicate", src) {
		t.Fatalf("duplicate %+v", stepOf(t, g, "dupstep"))
	}

	if _, err := app.DB.Exec(`UPDATE defects SET updated_at = ? WHERE id = ?`, "2026-04-01T00:00:00.000Z", goneDef); err != nil {
		t.Fatal(err)
	}
	if !stepOf(t, fetchGrid(t, h, planID, ""), "gonestep").NeedsRetest {
		t.Fatal("resolved gone step should need retest before delete")
	}
	if _, err := app.Store.Delete(goneDef, false); err != nil {
		t.Fatal(err)
	}
	afterDelete := fetchGrid(t, h, planID, "")
	if stepOf(t, afterDelete, "gonestep").NeedsRetest || len(stepOf(t, afterDelete, "gonestep").Defects) != 0 {
		t.Fatalf("deleted defect still sets retest %+v", stepOf(t, afterDelete, "gonestep"))
	}

	later := createRunOnPlan(t, app, planID, "Later")
	if rec := doJSON(t, h, http.MethodPatch, "/api/test-runs/"+later+"/steps/twostep", map[string]any{"status": "fail"}); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	lateDef := fileStep(t, h, later, "twostep", nil)
	if _, err := app.Store.Resolve(lateDef, "fixed", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`UPDATE defects SET updated_at = ? WHERE id = ?`, "2026-03-01T00:00:00.000Z", earlyDef); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`UPDATE defects SET updated_at = ? WHERE id = ?`, "2026-04-01T00:00:00.000Z", lateDef); err != nil {
		t.Fatal(err)
	}
	createReviewedRun(t, app, planID, "ClearsEarly", "2026-03-15T00:00:00.000Z", map[string]stepMark{
		"twostep": {status: "pass", evidence: true, review: "accepted"},
	})
	two := stepOf(t, fetchGrid(t, h, planID, ""), "twostep")
	if !two.NeedsRetest {
		t.Fatal("clearing only the earlier defect cleared the step")
	}
	if !defectLinked(two, earlyDef, "resolved", src) || !defectLinked(two, lateDef, "resolved", later) {
		t.Fatalf("two defects %+v", two.Defects)
	}
}

func createRunOnPlan(t *testing.T, app *api.App, planID, title string) string {
	t.Helper()
	rec := doJSON(t, app.Handler, http.MethodPost, "/api/test-runs", map[string]any{
		"app_id": store.SeededTutoredWebappAppID, "plan_id": planID, "execute": "manual",
		"title": title, "executed_by": "casey",
	})
	if rec.Code != 201 {
		t.Fatalf("create %s: %d %s", title, rec.Code, rec.Body)
	}
	return asMap(t, decodeBody(t, rec)["run"])["id"].(string)
}
