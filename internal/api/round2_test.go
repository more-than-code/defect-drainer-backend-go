package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestRound2EmptyStepsAndSecretEnv(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID

	emptyPlan := doJSON(t, h, "POST", "/api/test-plans", map[string]any{
		"app_id": appID, "key": "empty", "title": "Empty", "steps": []any{},
	})
	if emptyPlan.Code != 201 {
		t.Fatalf("empty plan %d %s", emptyPlan.Code, emptyPlan.Body)
	}
	emptyID := asMap(t, decodeBody(t, emptyPlan)["plan"])["id"].(string)
	gotPlan := doJSON(t, h, "GET", "/api/test-plans/"+emptyID, nil)
	plan := asMap(t, decodeBody(t, gotPlan)["plan"])
	steps, ok := plan["steps"].([]any)
	if !ok || steps == nil {
		t.Fatalf("zero-step plan steps = %#v", plan["steps"])
	}
	if len(steps) != 0 {
		t.Fatalf("steps %+v", steps)
	}

	listed := doJSON(t, h, "GET", "/api/test-plans?app_id="+appID, nil)
	for _, item := range asSlice(t, decodeBody(t, listed)["plans"]) {
		m := asMap(t, item)
		if _, has := m["steps"]; has {
			t.Fatalf("plan list included steps: %+v", m)
		}
		if m["step_count"] == nil {
			t.Fatal("list missing step_count")
		}
	}

	bare := doJSON(t, h, "POST", "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual", "title": "No steps",
	})
	if bare.Code != 201 {
		t.Fatalf("planless %d %s", bare.Code, bare.Body)
	}
	bareRun := asMap(t, decodeBody(t, bare)["run"])
	bareID := bareRun["id"].(string)
	if s, ok := bareRun["steps"].([]any); !ok || s == nil {
		t.Fatalf("create steps = %#v", bareRun["steps"])
	}
	again := doJSON(t, h, "GET", "/api/test-runs/"+bareID, nil)
	if s, ok := asMap(t, decodeBody(t, again)["run"])["steps"].([]any); !ok || s == nil {
		t.Fatalf("get steps = %#v", decodeBody(t, again))
	}
	runs := doJSON(t, h, "GET", "/api/test-runs?app_id="+appID, nil)
	for _, item := range asSlice(t, decodeBody(t, runs)["runs"]) {
		m := asMap(t, item)
		if _, has := m["steps"]; has {
			t.Fatalf("run list included steps: %+v", m)
		}
		if m["progress"] == nil {
			t.Fatal("list missing progress")
		}
	}

	secretMsg := "env keys must not hold secrets (password/secret/token); use a test account secret_ref"
	for _, tc := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"plan password", "POST", "/api/test-plans", map[string]any{
			"app_id": appID, "key": "pw", "title": "Pw", "steps": []any{},
			"env": map[string]any{"password": "hunter2"},
		}},
		{"plan nested token", "POST", "/api/test-plans", map[string]any{
			"app_id": appID, "key": "tk", "title": "Tk", "steps": []any{},
			"env": map[string]any{"auth": map[string]any{"token": "abc"}},
		}},
		{"run password", "POST", "/api/test-runs", map[string]any{
			"app_id": appID, "execute": "manual", "env": map[string]any{"password": "hunter2"},
		}},
		{"run nested token", "POST", "/api/test-runs", map[string]any{
			"app_id": appID, "execute": "manual",
			"env": map[string]any{"auth": map[string]any{"token": "abc"}},
		}},
	} {
		rec := doJSON(t, h, tc.method, tc.path, tc.body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), secretMsg) {
			t.Fatalf("%s %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	patched := doJSON(t, h, "PATCH", "/api/test-plans/"+emptyID, map[string]any{
		"env": map[string]any{"password": "hunter2"},
	})
	if patched.Code != 400 || !strings.Contains(patched.Body.String(), secretMsg) {
		t.Fatalf("patch %d %s", patched.Code, patched.Body)
	}
	okRef := doJSON(t, h, "POST", "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual",
		"env": map[string]any{"account": map[string]any{"secret_ref": "env:ADMIN_PASSWORD", "label": "A"}},
	})
	if okRef.Code != 201 {
		t.Fatalf("secret_ref %d %s", okRef.Code, okRef.Body)
	}
	acc := asMap(t, asMap(t, asMap(t, decodeBody(t, okRef)["run"])["env"])["account"])
	if acc["secret_ref"] != "env:ADMIN_PASSWORD" {
		t.Fatalf("account %+v", acc)
	}
}

func TestRound2EvidenceTxAndErrors(t *testing.T) {
	app := testApp(t)
	h := app.Handler
	appID := store.SeededTutoredWebappAppID
	runRec := doJSON(t, h, "POST", "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual", "title": "Shots",
	})
	if runRec.Code != 201 {
		t.Fatal(runRec.Body)
	}
	runID := asMap(t, decodeBody(t, runRec)["run"])["id"].(string)
	if rec := doJSON(t, h, "POST", "/api/test-runs/"+runID+"/steps", map[string]any{"action": "Look"}); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	first := uploadStepEvidence(t, h, runID, "x-look", "a.png", []byte("one"))
	second := uploadStepEvidence(t, h, runID, "x-look", "b.png", []byte("two"))
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("uploads %d %s / %d %s", first.Code, first.Body, second.Code, second.Body)
	}
	var evidence []any
	for _, s := range asSlice(t, asMap(t, decodeBody(t, second)["run"])["steps"]) {
		m := asMap(t, s)
		if m["key"] == "x-look" {
			evidence = asSlice(t, m["evidence"])
		}
	}
	if len(evidence) != 2 || !strings.Contains(evidence[0].(string), "s-x-look-01.png") || !strings.Contains(evidence[1].(string), "s-x-look-02.png") {
		t.Fatalf("evidence %+v", evidence)
	}
	dir := filepath.Join(paths.EvidenceDir(app.Roots.DefectsRoot), runID)
	for _, name := range []string{"s-x-look-01.png", "s-x-look-02.png"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if rec := doJSON(t, h, "POST", "/api/test-runs/"+runID+"/finish", nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	late := uploadStepEvidence(t, h, runID, "x-look", "c.png", []byte("three"))
	if late.Code != 409 {
		t.Fatalf("upload after finish %d %s", late.Code, late.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "s-x-look-03.png")); !os.IsNotExist(err) {
		t.Fatalf("finished run stored a file: %v", err)
	}

	// A regular file where the run directory should be.
	blocked := doJSON(t, h, "POST", "/api/test-runs", map[string]any{
		"app_id": appID, "execute": "manual", "title": "Blocked",
	})
	blockedID := asMap(t, decodeBody(t, blocked)["run"])["id"].(string)
	if rec := doJSON(t, h, "POST", "/api/test-runs/"+blockedID+"/steps", map[string]any{"action": "Look"}); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	if err := os.MkdirAll(paths.EvidenceDir(app.Roots.DefectsRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	occupied := filepath.Join(paths.EvidenceDir(app.Roots.DefectsRoot), blockedID)
	if err := os.WriteFile(occupied, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := uploadStepEvidence(t, h, blockedID, "x-look", "a.png", []byte("png"))
	if up.Code != 500 || !strings.Contains(up.Body.String(), "could not store evidence") {
		t.Fatalf("occupied %d %s", up.Code, up.Body)
	}
	if strings.Contains(up.Body.String(), app.Roots.DefectsRoot) {
		t.Fatalf("path leaked: %s", up.Body)
	}

	// RemoveAll failure must not delete the run, and must not echo a path.
	stuck := filepath.Join(dir, "s-x-look-01.png")
	if err := exec.Command("chflags", "uchg", stuck).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", stuck).Run() })
	del := doJSON(t, h, "DELETE", "/api/test-runs/"+runID+"?evidence=1", nil)
	if del.Code != 500 || !strings.Contains(del.Body.String(), "could not remove run evidence") {
		t.Fatalf("wipe %d %s", del.Code, del.Body)
	}
	if strings.Contains(del.Body.String(), app.Roots.DefectsRoot) {
		t.Fatalf("wipe path leaked: %s", del.Body)
	}
	if got := doJSON(t, h, "GET", "/api/test-runs/"+runID, nil); got.Code != 200 {
		t.Fatalf("run deleted despite wipe failure %d %s", got.Code, got.Body)
	}
}
