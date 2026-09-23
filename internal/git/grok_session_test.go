package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeGrokSession creates a session dir the way Grok Build 1.0.40 lays it
// out, with chat_history.jsonl stamped at mtime.
func writeGrokSession(t *testing.T, cwd, id string, mtime time.Time) {
	t.Helper()
	root, err := GrokSessionsDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(dir, "chat_history.jsonl")
	if err := os.WriteFile(hist, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hist, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// The directory name was read off disk for a real job on 2026-09-23.
func TestGrokSessionsDirMatchesGrokEscaping(t *testing.T) {
	t.Setenv("HOME", "/home/op")
	got, err := GrokSessionsDir("/Users/joe/workspace/defect-drainer/backend-go/.data/batch-jobs/bjob_muck130x_a5ad08c9")
	if err != nil {
		t.Fatal(err)
	}
	want := "/home/op/.grok/sessions/%2FUsers%2Fjoe%2Fworkspace%2Fdefect-drainer%2Fbackend-go%2F.data%2Fbatch-jobs%2Fbjob_muck130x_a5ad08c9"
	if got != want {
		t.Fatalf("sessions dir\n got %s\nwant %s", got, want)
	}
}

func TestLatestGrokSessionPicksNewestWithHistory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	older := "01a0c8be-7e0e-7c71-92ad-cd85bf99e86f"
	newer := "01a0c8be-7e0e-7c71-92ad-cd85bf99e870"
	now := time.Now()
	writeGrokSession(t, cwd, newer, now.Add(-time.Hour))
	writeGrokSession(t, cwd, older, now)

	root, _ := GrokSessionsDir(cwd)
	// Neither counts: no history, and a name grok could read as a flag/title.
	if err := os.MkdirAll(filepath.Join(root, "01a0c8be-7e0e-7c71-92ad-cd85bf99e871"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "--fork-session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "--fork-session", "chat_history.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, ok := LatestGrokSession(cwd)
	if !ok || id != older {
		t.Fatalf("got %q ok=%v, want the most recently written session %q", id, ok, older)
	}
}

func TestLatestGrokSessionNoneFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if id, ok := LatestGrokSession(t.TempDir()); ok || id != "" {
		t.Fatalf("missing sessions root must be not-found, got %q", id)
	}
}
