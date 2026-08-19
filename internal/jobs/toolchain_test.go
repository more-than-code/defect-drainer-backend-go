package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/git"
)

func writeFvmrc(t *testing.T, dir, pin string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".fvmrc"), []byte(`{"flutter":"`+pin+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionFlutterNoPinSkips(t *testing.T) {
	t.Setenv("FVM_HOME", t.TempDir())
	handoff := t.TempDir()
	wt := t.TempDir()
	var logs []string
	got := provisionFlutterToolchain(handoff, []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt}}, func(line, _ string) {
		logs = append(logs, line)
	})
	if got.Ok {
		t.Fatalf("no pin must skip, got %+v", got)
	}
	if len(got.Env) != 0 || len(got.VerifyEnv) != 0 {
		t.Fatalf("env %#v verify %#v", got.Env, got.VerifyEnv)
	}
	found := false
	for _, l := range logs {
		if l == "toolchain=flutter but no worktree pins one (.fvmrc) — skipped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing skip log: %#v", logs)
	}
	if _, err := os.Stat(filepath.Join(handoff, ".tooling")); err == nil {
		t.Fatal("skipped provision must not create .tooling")
	}
}

func TestProvisionFlutterConflictingPinsSkips(t *testing.T) {
	t.Setenv("FVM_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	writeFvmrc(t, a, "3.24.0")
	writeFvmrc(t, b, "3.29.0")
	got := provisionFlutterToolchain(t.TempDir(), []git.WorktreeBinding{
		{Repo: "one", WorktreeAbs: a},
		{Repo: "two", WorktreeAbs: b},
	}, func(string, string) {})
	if got.Ok {
		t.Fatal("conflicting pins must skip")
	}
}

func TestProvisionFlutterMissingSourceSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FVM_HOME", home)
	wt := t.TempDir()
	writeFvmrc(t, wt, "3.24.0")
	var logs []string
	got := provisionFlutterToolchain(t.TempDir(), []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt}}, func(line, _ string) {
		logs = append(logs, line)
	})
	if got.Ok {
		t.Fatal("missing source must skip")
	}
	want := filepath.Join(home, "versions", "3.24.0")
	found := false
	for _, l := range logs {
		if strings.Contains(l, want) && strings.Contains(l, "skipped") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-source log: %#v", logs)
	}
}
