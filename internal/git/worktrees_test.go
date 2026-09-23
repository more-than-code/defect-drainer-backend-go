package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	bindings, err := CreateBatchWorktrees("BATCH-20260817-test1", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
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
	_, err := CreateBatchWorktrees("BATCH-x", t.TempDir(), map[string]WorktreeRequest{})
	if err == nil {
		t.Fatal("expected empty map to fail")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README")
	gitRun(t, dir, "commit", "-m", "init")
}

func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-m", msg)
	rev, err := gitCOut(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestWorktreeUsesConfiguredLocalBranch(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	mainRev, err := gitCOut(primary, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, primary, "checkout", "-b", "feature")
	featureRev := commitFile(t, primary, "feature.txt", "f\n", "feature")
	gitRun(t, primary, "checkout", "main")
	gitRun(t, primary, "update-ref", "refs/remotes/origin/feature", mainRev)

	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-local", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "feature"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	if bindings[0].BaseSource != "local" || bindings[0].BaseBranch != "feature" || bindings[0].BaseRef != "feature" {
		t.Fatalf("base %+v", bindings[0])
	}
	head, err := gitCOut(bindings[0].WorktreeAbs, "rev-parse", "HEAD")
	if err != nil || head != featureRev {
		t.Fatalf("HEAD %s want %s (%v)", head, featureRev, err)
	}
}

func TestWorktreeUsesOriginBranchNotLocalMain(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	gitRun(t, primary, "checkout", "-b", "feature")
	featureRev := commitFile(t, primary, "feature.txt", "f\n", "feature")
	gitRun(t, primary, "checkout", "main")
	gitRun(t, primary, "update-ref", "refs/remotes/origin/feature", featureRev)

	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-origin", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "origin", Remote: "origin", Branch: "feature"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	if bindings[0].BaseRef != "origin/feature" || bindings[0].BaseSource != "origin" {
		t.Fatalf("base %+v", bindings[0])
	}
	head, err := gitCOut(bindings[0].WorktreeAbs, "rev-parse", "HEAD")
	if err != nil || head != featureRev {
		t.Fatalf("HEAD %s want %s", head, featureRev)
	}
}

func TestMissingBaseBranchFails(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	_, err := CreateBatchWorktrees("BATCH-miss", t.TempDir(), map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "no-such"},
	})
	if err == nil || !strings.Contains(err.Error(), "no-such") {
		t.Fatalf("got %v", err)
	}
}

func TestCommitLocalChangesThenMerge(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	if err := os.WriteFile(filepath.Join(primary, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, primary, "add", ".gitignore")
	gitRun(t, primary, "commit", "-m", "ignore build")
	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-commit", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	wt := bindings[0].WorktreeAbs
	if err := os.WriteFile(filepath.Join(wt, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "build", "noise"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewShip(bindings, "Dialog text uses the error style.\n\nTransparent Material on the dialog builders.")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Repos) != 1 || len(preview.Repos[0].Files) != 1 || preview.Repos[0].Files[0] != "fix.txt" {
		t.Fatalf("preview %+v", preview)
	}
	if strings.Contains(preview.Message, "fix.txt") || !strings.Contains(preview.Message, "Dialog text uses the error style.") {
		t.Fatalf("message %q", preview.Message)
	}
	if _, err := parsePorcelainZ("?? ../secret\x00"); err == nil {
		t.Fatal("expected escape path to be refused")
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	shipped := CreatePRsForWorktrees(bindings, "BATCH-commit", "Fix dialog text", ShipRequest{
		Commit: true, Message: preview.Message,
	})
	if len(shipped) != 1 || shipped[0].Status != "merged" {
		t.Fatalf("ship %+v", shipped)
	}
	body, err := os.ReadFile(filepath.Join(primary, "fix.txt"))
	if err != nil || string(body) != "fix\n" {
		t.Fatalf("primary fix.txt %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(primary, "build", "noise")); !os.IsNotExist(err) {
		t.Fatalf("ignored file landed in primary: %v", err)
	}
}

func TestLocalMergeIntoCleanBase(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-merge", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	want := commitFile(t, bindings[0].WorktreeAbs, "fix.txt", "fix\n", "fix")
	got := mergeIntoLocalBase(bindings[0], bindings[0].BaseRef, bindings[0].BaseBranch)
	if got.Status != "merged" || got.Kind != "merge" || got.GhState != "merged" {
		t.Fatalf("merge %+v", got)
	}
	head, err := gitCOut(primary, "rev-parse", "HEAD")
	if err != nil || head != want {
		t.Fatalf("primary HEAD %s want %s", head, want)
	}
}

func TestLocalMergeRefusesDirtyPrimary(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	before, _ := gitCOut(primary, "rev-parse", "HEAD")
	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-dirty", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	commitFile(t, bindings[0].WorktreeAbs, "fix.txt", "fix\n", "fix")
	if err := os.WriteFile(filepath.Join(primary, "dirty.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := mergeIntoLocalBase(bindings[0], bindings[0].BaseRef, bindings[0].BaseBranch)
	if got.Status != "failed" || !strings.Contains(got.Error, "dirty") {
		t.Fatalf("merge %+v", got)
	}
	head, _ := gitCOut(primary, "rev-parse", "HEAD")
	if head != before {
		t.Fatalf("primary moved %s -> %s", before, head)
	}
}

func TestLocalMergeLeavesOtherCheckout(t *testing.T) {
	primary := t.TempDir()
	initRepo(t, primary)
	root := t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-side", root, map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveBatchWorktrees(bindings) })
	want := commitFile(t, bindings[0].WorktreeAbs, "fix.txt", "fix\n", "fix")
	gitRun(t, primary, "checkout", "-b", "side")
	got := mergeIntoLocalBase(bindings[0], bindings[0].BaseRef, bindings[0].BaseBranch)
	if got.Status != "merged" {
		t.Fatalf("merge %+v", got)
	}
	headName, err := gitCOut(primary, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || headName != "side" {
		t.Fatalf("primary HEAD branch %s %v", headName, err)
	}
	mainRev, err := gitCOut(primary, "rev-parse", "main")
	if err != nil || mainRev != want {
		t.Fatalf("main %s want %s", mainRev, want)
	}
}
