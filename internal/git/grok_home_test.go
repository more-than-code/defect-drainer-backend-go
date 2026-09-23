package git

import (
	"path/filepath"
	"testing"
)

func TestGrokHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("GROK_HOME", "")
	if got, err := GrokHome(); err != nil || got != filepath.Join(home, ".grok") {
		t.Fatalf("default: got %q, %v", got, err)
	}

	alt := t.TempDir()
	t.Setenv("GROK_HOME", alt)
	if got, err := GrokHome(); err != nil || got != alt {
		t.Fatalf("GROK_HOME: got %q, %v", got, err)
	}

	// A relative home would resolve against whatever cwd reads it — never
	// the file grok actually uses.
	t.Setenv("GROK_HOME", "rel/grok")
	if got, err := GrokHome(); err == nil {
		t.Fatalf("relative GROK_HOME must error, got %q", got)
	}
	t.Setenv("GROK_HOME", "")
	t.Setenv("HOME", "")
	if got, err := GrokHome(); err == nil {
		t.Fatalf("empty HOME must error, got %q", got)
	}
}
