package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func execCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	return cmd
}

func TestWorktreeRepoNameStaysInsideRoot(t *testing.T) {
	data := t.TempDir()
	sentinel := filepath.Join(data, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(data, "batch-jobs", "bjob_x", "worktrees")
	primary := t.TempDir()
	init := func(args ...string) {
		t.Helper()
		cmd := execCommand(t, primary, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	init("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	init("add", "README")
	init("commit", "-m", "init")
	for _, name := range []string{"../../..", `a/../../b`} {
		_, err := CreateDetachedWorktrees(root, map[string]DetachRequest{
			name: {PrimaryAbs: primary, Source: "local", Branch: "main"},
		})
		if err == nil {
			t.Fatalf("repo %q was accepted", name)
		}
		_, err = CreateBatchWorktrees("BATCH-x", root, map[string]WorktreeRequest{
			name: {PrimaryAbs: primary, Source: "local", Branch: "main"},
		})
		if err == nil {
			t.Fatalf("batch repo %q was accepted", name)
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("sentinel gone after repo %q: %v", name, err)
		}
	}
}
