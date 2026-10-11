package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/api"
	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestBuildRefusesMissingEvidenceDirAndReleasesLock(t *testing.T) {
	cases := []struct {
		name     string
		evidence []string
		fix      []string
	}{
		{"evidence_json", []string{"evidence/DEF-20260924-ev-abcd/01.png"}, nil},
		{"fix_evidence_json", nil, []string{"evidence/DEF-20260924-ev-abcd/fix-01.png"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defects := t.TempDir()
			data := t.TempDir()
			cwd := t.TempDir()
			seedDefect(t, defects, data, cwd, tc.evidence, tc.fix)
			if err := os.RemoveAll(paths.EvidenceDir(defects)); err != nil {
				t.Fatal(err)
			}

			opts := api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true}
			first, err := api.Build(opts)
			if err == nil {
				first.Close()
				t.Fatal("expected missing evidence dir error")
			}
			assertEvidenceDirError(t, err, paths.EvidenceDir(defects))

			second, err := api.Build(opts)
			if err == nil {
				second.Close()
				t.Fatal("second build expected missing evidence dir error")
			}
			assertEvidenceDirError(t, err, paths.EvidenceDir(defects))
			if _, statErr := os.Stat(paths.EvidenceDir(defects)); !os.IsNotExist(statErr) {
				t.Fatalf("build created evidence dir: %v", statErr)
			}
		})
	}
}

func TestBuildRefusesEmptyEvidenceDir(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	seedDefect(t, defects, data, cwd, []string{"evidence/DEF-20260924-ev-abcd/01.png"}, nil)

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err == nil {
		app.Close()
		t.Fatal("expected refusal for an empty evidence dir")
	}
	assertNoReferencedEvidence(t, err, paths.EvidenceDir(defects))
	if _, statErr := os.Stat(filepath.Join(paths.EvidenceDir(defects), "DEF-20260924-ev-abcd", "01.png")); !os.IsNotExist(statErr) {
		t.Fatalf("build created the referenced file: %v", statErr)
	}
}

func TestBuildRefusesNestedEvidenceTree(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	const id = "DEF-20260924-ev-abcd"
	seedDefect(t, defects, data, cwd, []string{"evidence/" + id + "/01.png"}, nil)
	nested := filepath.Join(paths.EvidenceDir(defects), "evidence", id)
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err == nil {
		app.Close()
		t.Fatal("expected refusal when the only file is nested under evidence/evidence")
	}
	assertNoReferencedEvidence(t, err, paths.EvidenceDir(defects))
	if _, statErr := os.Stat(filepath.Join(paths.EvidenceDir(defects), id, "01.png")); !os.IsNotExist(statErr) {
		t.Fatalf("build created the flat file: %v", statErr)
	}
}

func TestBuildStartsWhenReferencedEvidenceFileExists(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	const id = "DEF-20260924-ev-abcd"
	seedDefect(t, defects, data, cwd, []string{"evidence/" + id + "/01.png"}, nil)
	dir := filepath.Join(paths.EvidenceDir(defects), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
}

func TestBuildAllowsEvidenceDirWhenPresent(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	const id = "DEF-20260924-ev-abcd"
	seedDefect(t, defects, data, cwd, []string{"evidence/" + id + "/01.png"}, nil)
	// An empty directory is not enough (E1). The referenced file has to be there.
	dir := filepath.Join(paths.EvidenceDir(defects), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
}

func TestBuildRefusesMissingStepEvidence(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	const runID = "TRUN-20260924-abcdef01"
	seedStepEvidence(t, defects, data, cwd, "evidence/"+runID+"/s-login-01.png")

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err == nil {
		app.Close()
		t.Fatal("expected refusal when a step references evidence that is missing")
	}
	assertNoReferencedEvidence(t, err, paths.EvidenceDir(defects))
}

func TestBuildStartsWhenStepEvidenceExists(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	cwd := t.TempDir()
	const runID = "TRUN-20260924-abcdef01"
	seedStepEvidence(t, defects, data, cwd, "evidence/"+runID+"/s-login-01.png")
	dir := filepath.Join(paths.EvidenceDir(defects), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s-login-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
}

func seedStepEvidence(t *testing.T, defects, data, cwd, rel string) {
	t.Helper()
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "TRUN-20260924-abcdef01"
	now := "2026-09-24T00:00:00.000Z"
	_, err = app.DB.Exec(`INSERT INTO test_runs (
		id, app_id, title, executor, status, started_at, created_at, updated_at
	) VALUES (?, ?, 't', 'operator', 'in_progress', ?, ?, ?)`,
		runID, store.SeededTutoredWebappAppID, now, now, now)
	if err != nil {
		app.Close()
		t.Fatal(err)
	}
	_, err = app.DB.Exec(`INSERT INTO test_step_results (
		run_id, step_key, position, action, evidence_json, updated_at
	) VALUES (?, 'login', 1, 'Sign in', ?, ?)`, runID, `["`+rel+`"]`, now)
	app.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBuildAllowsEmptyDBWithoutEvidenceDir(t *testing.T) {
	defects := t.TempDir()
	data := t.TempDir()
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: t.TempDir(), SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	if err := os.RemoveAll(paths.EvidenceDir(defects)); err != nil {
		t.Fatal(err)
	}
	app, err = api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: t.TempDir(), SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	app.Close()

	defects = t.TempDir()
	data = t.TempDir()
	cwd := t.TempDir()
	seedDefect(t, defects, data, cwd, nil, nil)
	if err := os.RemoveAll(paths.EvidenceDir(defects)); err != nil {
		t.Fatal(err)
	}
	app, err = api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
}

func seedDefect(t *testing.T, defects, data, cwd string, evidence, fix []string) {
	t.Helper()
	app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.Store.Write(store.DefectRecord{
		ID:          "DEF-20260924-ev-abcd",
		AppID:       store.SeededTutoredWebappAppID,
		Title:       "t",
		Evidence:    evidence,
		FixEvidence: fix,
	})
	app.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoReferencedEvidence(t *testing.T, err error, dir string) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, dir) || !strings.Contains(msg, "none of the referenced evidence files were found there") || !strings.Contains(msg, "docs/operating.md") {
		t.Fatalf("error must name the dir, say no referenced file was found, and point at docs/operating.md, got %q", msg)
	}
	if strings.Contains(msg, "locked") {
		t.Fatalf("expected evidence error, got lock error %q", msg)
	}
}

func assertEvidenceDirError(t *testing.T, err error, dir string) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, dir) || !strings.Contains(msg, "docs/operating.md") {
		t.Fatalf("error must name the evidence dir and docs/operating.md, got %q", msg)
	}
	if strings.Contains(msg, "locked") {
		t.Fatalf("expected evidence error, got lock error %q", msg)
	}
}

// TestBuildEvidenceJSONShapes pins how the startup guard reads evidence_json:
// non-array JSON references nothing, and malformed JSON refuses to start.
func TestBuildEvidenceJSONShapes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"object", `{"a":"evidence/x.png"}`, ""},
		{"null", `null`, ""},
		{"nested-empty", `[[]]`, "none of the referenced evidence files were found there"},
		{"malformed", `[`, "malformed JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defects := t.TempDir()
			data := t.TempDir()
			cwd := t.TempDir()
			seedDefect(t, defects, data, cwd, nil, nil)
			app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = app.DB.Exec(`UPDATE defects SET evidence_json = ?`, tc.raw)
			app.Close()
			if err != nil {
				t.Fatal(err)
			}
			app, err = api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				app.Close()
				return
			}
			if err == nil {
				app.Close()
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %q, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildEvidenceSampleLimit pins the guard's 50-entry sample: defect
// evidence first, then defect fix evidence, then test-step evidence.
func TestBuildEvidenceSampleLimit(t *testing.T) {
	const id = "DEF-20260924-ev-abcd"
	missing := func(n int) []string {
		var out []string
		for i := 1; i <= n; i++ {
			out = append(out, fmt.Sprintf("evidence/%s/missing-%02d.png", id, i))
		}
		return out
	}
	realPath := "evidence/" + id + "/real.png"
	cases := []struct {
		name     string
		evidence []string
		fix      []string
		wantOK   bool
	}{
		{"real file 51st is outside the sample", append(missing(50), realPath), nil, false},
		{"real fix file after 49 missing is inside the sample", missing(49), []string{realPath}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defects := t.TempDir()
			data := t.TempDir()
			cwd := t.TempDir()
			seedDefect(t, defects, data, cwd, tc.evidence, tc.fix)
			dir := filepath.Join(paths.EvidenceDir(defects), id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "real.png"), []byte("png"), 0o644); err != nil {
				t.Fatal(err)
			}
			app, err := api.Build(api.Options{DefectsRoot: defects, DataRoot: data, Cwd: cwd, SkipMigrate: true})
			if tc.wantOK {
				if err != nil {
					t.Fatal(err)
				}
				app.Close()
				return
			}
			if err == nil {
				app.Close()
				t.Fatal("expected refusal")
			}
			assertNoReferencedEvidence(t, err, paths.EvidenceDir(defects))
		})
	}
}
