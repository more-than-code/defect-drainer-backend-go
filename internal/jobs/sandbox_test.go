package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSimulatorSandboxProfileStaysInHandoff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handoff := t.TempDir()
	got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "dd-simulator" {
		t.Fatalf("profile %q", got.Profile)
	}
	path := filepath.Join(handoff, ".grok", "sandbox.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "[profiles.dd-simulator]") {
		t.Fatalf("missing profile: %s", s)
	}
	if !strings.Contains(s, `extends = "workspace"`) {
		t.Fatalf("missing extends: %s", s)
	}
	if !strings.Contains(s, "CoreSimulator/Devices") {
		t.Fatalf("missing devices grant: %s", s)
	}
	global := filepath.Join(home, ".grok", "sandbox.toml")
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Fatalf("~/.grok/sandbox.toml must be untouched, stat=%v", err)
	}
}
