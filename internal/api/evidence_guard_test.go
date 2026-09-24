package api_test

import (
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
