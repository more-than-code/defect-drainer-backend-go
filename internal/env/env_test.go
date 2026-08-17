package env

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvDrainerPrefersCanonicalThenLegacy(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_HOST", "")
	t.Setenv("DEFECT_CHANNEL_HOST", "")
	if EnvDrainer("HOST") != "" {
		t.Fatalf("empty env: got %q", EnvDrainer("HOST"))
	}

	t.Setenv("DEFECT_CHANNEL_HOST", "legacy")
	if got := EnvDrainer("HOST"); got != "legacy" {
		t.Fatalf("legacy: got %q", got)
	}

	t.Setenv("DEFECT_DRAINER_HOST", "canon")
	if got := EnvDrainer("HOST"); got != "canon" {
		t.Fatalf("canonical: got %q", got)
	}
}

func TestEnvDrainerFlag(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_NORMALIZE_LOCAL", "")
	if EnvDrainerFlag("NORMALIZE_LOCAL") {
		t.Fatal("unset should be false")
	}
	t.Setenv("DEFECT_DRAINER_NORMALIZE_LOCAL", "true")
	if EnvDrainerFlag("NORMALIZE_LOCAL") {
		t.Fatal("only exact 1 is true")
	}
	t.Setenv("DEFECT_DRAINER_NORMALIZE_LOCAL", "1")
	if !EnvDrainerFlag("NORMALIZE_LOCAL") {
		t.Fatal("1 should be true")
	}
}

func TestLoadEnvFileDoesNotOverrideAndSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	missing, err := LoadEnvFile(filepath.Join(dir, "nope.env"))
	if err != nil || missing != "" {
		t.Fatalf("missing: got %q %v", missing, err)
	}

	path := filepath.Join(dir, ".env")
	body := "# comment\nDEFECT_DRAINER_PORT=9999\nDEFECTS_ROOT=/from-file\nQUOTED=\"/quoted\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DEFECT_DRAINER_PORT", "8788")
	t.Setenv("DEFECTS_ROOT", "placeholder")
	t.Setenv("QUOTED", "placeholder")
	if err := os.Unsetenv("DEFECTS_ROOT"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("QUOTED"); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadEnvFile(path)
	if err != nil || loaded != path {
		t.Fatalf("load: %q %v", loaded, err)
	}
	if os.Getenv("DEFECT_DRAINER_PORT") != "8788" {
		t.Fatalf("must not override existing PORT, got %q", os.Getenv("DEFECT_DRAINER_PORT"))
	}
	if os.Getenv("DEFECTS_ROOT") != "/from-file" {
		t.Fatalf("DEFECTS_ROOT: got %q", os.Getenv("DEFECTS_ROOT"))
	}
	if os.Getenv("QUOTED") != "/quoted" {
		t.Fatalf("quoted: got %q", os.Getenv("QUOTED"))
	}
}
