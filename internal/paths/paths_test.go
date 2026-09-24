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
	// Default DEFECTS_ROOT is the data dir, not the umbrella parent.
	if got.DefectsRoot != data {
		t.Fatalf("DefectsRoot=%q want %q", got.DefectsRoot, data)
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
	// No evidence/ dir is required to recognise the umbrella, and the default
	// defects root is the data dir.
	if got.DefectsRoot != data || got.DataRoot != data {
		t.Fatalf("got %+v want DefectsRoot=DataRoot=%s", got, data)
	}
	if got.ModuleRoot != "" {
		t.Fatalf("expected no module, got %q", got.ModuleRoot)
	}
}

func TestResolveDataEnvWithLegacyEvidenceRefuses(t *testing.T) {
	clearRootEnv(t)
	root, mod, _ := moduleWithDB(t)
	data := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv("DEFECT_DRAINER_DATA", data)
	legacy := filepath.Join(root, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(data, "evidence")

	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected guard (a) error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mv") || !strings.Contains(msg, legacy) || !strings.Contains(msg, dest) {
		t.Fatalf("error must contain mv and both paths, got %q", msg)
	}
	if !dirExists(legacy) {
		t.Fatal("legacy evidence was removed")
	}
	if _, statErr := os.Stat(data); !os.IsNotExist(statErr) {
		t.Fatalf("must not create the data dir: %v", statErr)
	}
}

func TestResolveDataEnvDefaultsDefectsRoot(t *testing.T) {
	clearRootEnv(t)
	root := t.TempDir()
	mod := filepath.Join(root, "backend-go")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	data := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv("DEFECT_DRAINER_DATA", data)

	got, err := Resolve(Options{Cwd: mod})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != data || got.DataRoot != data {
		t.Fatalf("got %+v want both %s", got, data)
	}
	if _, statErr := os.Stat(data); !os.IsNotExist(statErr) {
		t.Fatalf("must not create the data dir: %v", statErr)
	}
}

func TestResolveLegacyEvidenceRefusesDefault(t *testing.T) {
	clearRootEnv(t)
	root, mod, data := moduleWithDB(t)
	legacy := filepath.Join(root, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(data, "evidence")

	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected legacy evidence error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mv") || !strings.Contains(msg, legacy) || !strings.Contains(msg, dest) {
		t.Fatalf("error must contain mv and both paths, got %q", msg)
	}
	if !dirExists(legacy) {
		t.Fatal("legacy evidence was moved or removed")
	}
	if dirExists(dest) || fileExists(dest) {
		t.Fatal("resolve created the destination evidence path")
	}
}

func TestResolveLegacyEvidenceFromUmbrellaCwd(t *testing.T) {
	clearRootEnv(t)
	root := t.TempDir()
	data := filepath.Join(root, "backend-go", ".data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(data, "evidence")

	_, err := Resolve(Options{Cwd: root})
	if err == nil {
		t.Fatal("expected legacy evidence error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mv") || !strings.Contains(msg, legacy) || !strings.Contains(msg, dest) {
		t.Fatalf("error must contain mv and both paths, got %q", msg)
	}
	if !dirExists(legacy) || dirExists(dest) {
		t.Fatalf("layout changed: legacy=%v dest=%v", dirExists(legacy), dirExists(dest))
	}
}

func TestResolveQuotesCommandsWhenPathHasSpace(t *testing.T) {
	clearRootEnv(t)
	base := t.TempDir()
	root := filepath.Join(base, "umbrella dir")
	mod := filepath.Join(root, "backend-go")
	data := filepath.Join(mod, ".data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "evidence")
	dest := filepath.Join(data, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected mv error")
	}
	wantMv := "mv '" + legacy + "' '" + dest + "'"
	if !strings.Contains(err.Error(), wantMv) {
		t.Fatalf("mv command must single-quote both paths, got %q", err.Error())
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected rmdir error")
	}
	wantRm := "rmdir '" + legacy + "'"
	if !strings.Contains(err.Error(), wantRm) {
		t.Fatalf("rmdir command must single-quote the path, got %q", err.Error())
	}
}

func TestResolveQuotesApostropheInPath(t *testing.T) {
	clearRootEnv(t)
	base := t.TempDir()
	root := filepath.Join(base, "umbrella'o")
	mod := filepath.Join(root, "backend-go")
	data := filepath.Join(mod, ".data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "evidence")
	dest := filepath.Join(data, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected mv error")
	}
	want := "mv '" + strings.ReplaceAll(legacy, "'", `'\''`) + "' '" + strings.ReplaceAll(dest, "'", `'\''`) + "'"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("apostrophe must be escaped as '\\'', got %q", err.Error())
	}
}

func TestResolveLegacyEmptySaysRmdir(t *testing.T) {
	clearRootEnv(t)
	root, mod, data := moduleWithDB(t)
	legacy := filepath.Join(root, "evidence")
	dest := filepath.Join(data, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected rmdir error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "rmdir") || !strings.Contains(msg, legacy) {
		t.Fatalf("error must contain rmdir and the legacy path, got %q", msg)
	}
	if !dirExists(legacy) || !dirExists(dest) {
		t.Fatal("resolve removed or created an evidence directory")
	}
}

func TestResolveLegacyEvidenceAsksToMerge(t *testing.T) {
	clearRootEnv(t)
	root, mod, data := moduleWithDB(t)
	legacy := filepath.Join(root, "evidence")
	dest := filepath.Join(data, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(Options{Cwd: mod})
	if err == nil {
		t.Fatal("expected merge error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "merge") || !strings.Contains(msg, legacy) || !strings.Contains(msg, dest) {
		t.Fatalf("error must say to merge and name both paths, got %q", msg)
	}
	if !dirExists(legacy) || !dirExists(dest) {
		t.Fatal("resolve changed the two evidence directories")
	}
}

func TestResolveExplicitDefectsRootSkipsLegacyEvidence(t *testing.T) {
	root, mod, data := moduleWithDB(t)
	legacy := filepath.Join(root, "evidence")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	clearRootEnv(t)
	explicit := filepath.Join(root, "explicit-inv")
	t.Setenv("DEFECTS_ROOT", explicit)
	got, err := Resolve(Options{Cwd: mod})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != explicit || got.DataRoot != data {
		t.Fatalf("env: got %+v", got)
	}

	clearRootEnv(t)
	optRoot := filepath.Join(root, "opt-inv")
	got, err = Resolve(Options{Cwd: mod, DefectsRoot: optRoot})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefectsRoot != optRoot || got.DataRoot != data {
		t.Fatalf("opts: got %+v", got)
	}
	if !dirExists(legacy) {
		t.Fatal("explicit defects root still moved legacy evidence")
	}
	if _, statErr := os.Stat(explicit); !os.IsNotExist(statErr) {
		t.Fatalf("created explicit defects root: %v", statErr)
	}
}

func moduleWithDB(t *testing.T) (root, mod, data string) {
	t.Helper()
	root = t.TempDir()
	mod = filepath.Join(root, "backend-go")
	data = filepath.Join(mod, ".data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoMod(t, mod)
	if err := os.WriteFile(filepath.Join(data, dbFileName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, mod, data
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
