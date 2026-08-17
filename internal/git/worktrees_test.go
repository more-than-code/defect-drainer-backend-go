package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCreateBatchWorktreesFromLocalPrimary(t *testing.T) {
	primary := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = primary
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")

	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-20260817-test1", root, map[string]string{"demo": primary})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings %d", len(bindings))
	}
	if bindings[0].Branch != "defect-drainer/BATCH-20260817-test1" {
		t.Fatalf("branch %s", bindings[0].Branch)
	}
	head, err := gitCOut(bindings[0].WorktreeAbs, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || head != bindings[0].Branch {
		t.Fatalf("HEAD %q %v", head, err)
	}
	RemoveBatchWorktrees(bindings)
}

func TestEmptyWorktreesRefuse(t *testing.T) {
	_, err := CreateBatchWorktrees("BATCH-x", t.TempDir(), map[string]string{})
	if err == nil {
		t.Fatal("expected empty map to fail")
	}
}
