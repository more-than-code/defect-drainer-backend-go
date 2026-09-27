package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/store"
)

func TestTestPlanExport(t *testing.T) {
	t.Setenv("QA_PASSWORD", "hunter2-secret")
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID

	missing := doJSON(t, h, http.MethodGet, "/api/test-plans/TPLAN-missing/export", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing %d %s", missing.Code, missing.Body)
	}
	if ct := missing.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("missing content-type %q", ct)
	}
	if decodeBody(t, missing)["error"] == nil || decodeBody(t, missing)["error"] == "" {
		t.Fatalf("missing body %s", missing.Body)
	}

	accounts := doJSON(t, h, http.MethodPatch, "/api/apps/"+appID, map[string]any{
		"test_accounts": []any{map[string]any{
			"label": "Admin", "username": "ada", "environment": "staging", "secret_ref": "env:QA_PASSWORD",
		}},
	})
	if accounts.Code != http.StatusOK {
		t.Fatalf("accounts %d %s", accounts.Code, accounts.Body)
	}

	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "export-smoke", "title": "Export smoke",
		"purpose": "check export",
		"env":     map[string]any{"target": "web", "account_label": "admin"},
		"steps": []any{map[string]any{
			"key": "login", "section": "auth", "action": "Sign in", "expected": "Home",
		}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	plan := asMap(t, decodeBody(t, created)["plan"])
	planID := plan["id"].(string)
	if plan["version"].(float64) != 1 {
		t.Fatalf("version %+v", plan["version"])
	}

	rec := doJSON(t, h, http.MethodGet, "/api/test-plans/"+planID+"/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("content-type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="export-smoke-v1.test-plan.md"` {
		t.Fatalf("disposition %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control %q", got)
	}
	body := rec.Body.String()
	for _, needle := range []string{
		"format: dd-test-plan/1",
		"plan_id: " + planID,
		"archived: false",
		"- Label: Admin",
		"- Username: ada",
		"- Environment: staging",
		"`$QA_PASSWORD`",
		"### 1. `login` — auth",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("missing %q\n%s", needle, body)
		}
	}
	if strings.Contains(body, "hunter2-secret") {
		t.Fatal("password leaked")
	}

	archived := doJSON(t, h, http.MethodPatch, "/api/test-plans/"+planID, map[string]any{"archived": true})
	if archived.Code != http.StatusOK {
		t.Fatalf("archive %d %s", archived.Code, archived.Body)
	}
	again := doJSON(t, h, http.MethodGet, "/api/test-plans/"+planID+"/export", nil)
	if again.Code != http.StatusOK {
		t.Fatalf("archived export %d %s", again.Code, again.Body)
	}
	if !strings.Contains(again.Body.String(), "archived: true") {
		t.Fatalf("archived flag missing\n%s", again.Body)
	}
	if strings.Contains(again.Body.String(), "hunter2-secret") {
		t.Fatal("password leaked from archived export")
	}
	if again.Header().Get("Content-Disposition") != `attachment; filename="export-smoke-v1.test-plan.md"` {
		t.Fatalf("archived disposition %q", again.Header().Get("Content-Disposition"))
	}
}

func TestTestPlanExportClosedDB(t *testing.T) {
	app := testApp(t)
	h := app.Handler

	missing := doJSON(t, h, http.MethodGet, "/api/test-plans/TPLAN-missing/export", nil)
	if missing.Code != http.StatusNotFound || missing.Body.String() != "{\"error\":\"test plan not found\"}\n" {
		t.Fatalf("unknown id %d %q", missing.Code, missing.Body.String())
	}

	if err := app.DB.Close(); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, h, http.MethodGet, "/api/test-plans/TPLAN-missing/export", nil)
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "{\"error\":\"internal error\"}\n" {
		t.Fatalf("closed db %d %q", rec.Code, rec.Body.String())
	}
}

func TestTestPlanExportMissingApp(t *testing.T) {
	app := testApp(t)
	h := app.Handler

	createdApp := doJSON(t, h, http.MethodPost, "/api/apps", map[string]any{"name": "TempExport"})
	if createdApp.Code != http.StatusCreated {
		t.Fatalf("app %d %s", createdApp.Code, createdApp.Body)
	}
	appID := asMap(t, decodeBody(t, createdApp)["app"])["id"].(string)

	created := doJSON(t, h, http.MethodPost, "/api/test-plans", map[string]any{
		"app_id": appID, "key": "orphan", "title": "Orphan plan",
		"purpose": "should not leak",
		"steps": []any{map[string]any{
			"key": "step", "action": "Do the thing", "expected": "See it",
		}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("plan %d %s", created.Code, created.Body)
	}
	planID := asMap(t, decodeBody(t, created)["plan"])["id"].(string)

	// app.DB is the exported pool. Open sets MaxOpenConns(1), so this pragma
	// stays on the connection the handler then uses.
	if _, err := app.DB.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`DELETE FROM apps WHERE id = ?`, appID); err != nil {
		t.Fatal(err)
	}

	rec := doJSON(t, h, http.MethodGet, "/api/test-plans/"+planID+"/export", nil)
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "{\"error\":\"internal error\"}\n" {
		t.Fatalf("missing app %d %q", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"Orphan plan", "should not leak", "Do the thing", "See it", "dd-test-plan", planID} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("plan text %q in %q", leak, rec.Body.String())
		}
	}
}
