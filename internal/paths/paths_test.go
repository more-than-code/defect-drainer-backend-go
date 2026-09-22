package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearRootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEFECTS_ROOT", "")
	t.Setenv("DEFECT_DRAINER_DATA", "")
	t.Setenv("DEFECT_CHANNEL_DATA", "")
}

func writeGoMod(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+ModulePath+"\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFailClosedOutsideUmbrella(t *testing.T) {
	clearRootEnv(t)
	cwd := t.TempDir()
	_, err := Resolve(Options{Cwd: cwd})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "DEFECTS_ROOT") || !strings.Contains(msg, "DEFECT_DRAINER_DATA") {
		t.Fatalf("error must name both env vars, got %q", msg)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("must not create files under cwd, got %v", entries)
	}
}

func TestResolveExplicitOpts(t *testing.T) {
	clearRootEnv(t)
	got, err := Resolve(Options{
		Cwd:         t.TempDir(),
		DefectsRoot: "/inv",
		DataRoot:    "/rt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != "/inv" || got.DataRoot != "/rt" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveEnvWins(t *testing.T) {
	t.Setenv("DEFECTS_ROOT", "/from-env")
	t.Setenv("DEFECT_DRAINER_DATA", "/data-env")
	got, err := Resolve(Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != "/from-env" || got.DataRoot != "/data-env" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveModuleDataWhenModuleFound(t *testing.T) {
	clearRootEnv(t)
	root := t.TempDir()
	mod := filepath.Join(root, "backend-go")
	// Data lives inside the module since the TS tree was retired; it used to
	// be the sibling backend/.data.
	data := filepath.Join(mod, ".data")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(Options{Cwd: mod})
	if err != nil {
		t.Fatal(err)
	}
	if got.ModuleRoot != mod {
		t.Fatalf("ModuleRoot=%q", got.ModuleRoot)
	}
	if got.DefectsRoot != root {
		t.Fatalf("DefectsRoot=%q want %q", got.DefectsRoot, root)
	}
	if got.DataRoot != data {
		t.Fatalf("DataRoot=%q want %q", got.DataRoot, data)
	}
}

func TestResolveModuleWithoutDBDoesNotInventData(t *testing.T) {
	clearRootEnv(t)
	root := t.TempDir()
	mod := filepath.Join(root, "backend-go")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected fail-closed when the module db is missing")
	}
	if _, statErr := os.Stat(filepath.Join(mod, ".data")); !os.IsNotExist(statErr) {
		t.Fatalf("must not create module .data: %v", statErr)
	}
}

func TestResolveUmbrellaCwdHeuristic(t *testing.T) {
	clearRootEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "backend-go", ".data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(Options{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != root || got.DataRoot != data {
		t.Fatalf("got %+v", got)
	}
	if got.ModuleRoot != "" {
		t.Fatalf("expected no module, got %q", got.ModuleRoot)
	}
}

func TestHelperPaths(t *testing.T) {
	if EvidenceDir("/u") != filepath.Join("/u", "evidence") {
		t.Fatal("evidence")
	}
	if DbPath("/d") != filepath.Join("/d", dbFileName) {
		t.Fatal("db")
	}
	if LockPath("/d") != filepath.Join("/d", "defect-drainer.lock") {
		t.Fatal("lock")
	}
}
