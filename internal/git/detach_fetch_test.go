package git

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmptyOriginRefFetchesStaleTracking(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	primary := filepath.Join(root, "primary")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := execCommand(t, dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run(root, "init", "--bare", "-b", "main", bare)
	run(root, "init", "-b", "main", primary)
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(primary, "add", "README")
	run(primary, "commit", "-m", "old")
	oldSHA := gitSHA(t, primary, "HEAD")
	run(primary, "remote", "add", "origin", bare)
	run(primary, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(primary, "add", "README")
	run(primary, "commit", "-m", "new")
	newSHA := gitSHA(t, primary, "HEAD")
	run(primary, "push", "origin", "main")
	run(primary, "update-ref", "refs/remotes/origin/main", oldSHA)
	if gitSHA(t, primary, "refs/remotes/origin/main") != oldSHA {
		t.Fatal("remote-tracking ref was not rewound")
	}
	got, err := CreateDetachedWorktrees(filepath.Join(root, "worktrees"), map[string]DetachRequest{
		"demo": {PrimaryAbs: primary, Source: "origin", Remote: "origin", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA != newSHA {
		t.Fatalf("recorded %+v, want %s", got, newSHA)
	}
	if got[0].Ref != "main" {
		t.Fatalf("recorded ref %q", got[0].Ref)
	}
}

func gitSHA(t *testing.T, dir, rev string) string {
	t.Helper()
	cmd := execCommand(t, dir, "rev-parse", rev)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:40])
}
