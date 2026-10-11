package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// discardFixture is a primary repo on main with one batch worktree cut from
// it under root (the job's handoff), as CreateBatchWorktrees makes them.
func discardFixture(t *testing.T) (primary, root string, b WorktreeBinding) {
	t.Helper()
	primary = t.TempDir()
	gitRun(t, primary, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(primary, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, primary, "add", "README")
	gitRun(t, primary, "commit", "-q", "-m", "init")
	root = t.TempDir()
	bindings, err := CreateBatchWorktrees("BATCH-20260924-discard", filepath.Join(root, "worktrees"), map[string]WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return primary, root, bindings[0]
}

func registered(t *testing.T, primary, worktree string) bool {
	t.Helper()
	out, err := gitCOut(primary, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(worktree)
	return strings.Contains(out, worktree) || (real != "" && strings.Contains(out, real))
}

func branchExists(primary, branch string) bool {
	return gitC(primary, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

func commitIn(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-q", "-m", name)
}

// AC1: a clean, merged job leaves no registration, branch or directory.
func TestDiscardMergedWorktree(t *testing.T) {
	primary, root, b := discardFixture(t)
	plan := PlanDiscard([]WorktreeBinding{b}, root)
	if plan[0].BranchAction != "delete" || !plan[0].Present || plan[0].UncommittedCount != 0 {
		t.Fatalf("plan %+v", plan[0])
	}
	if !registered(t, primary, b.WorktreeAbs) || !branchExists(primary, b.Branch) {
		t.Fatal("the preview must not change anything")
	}
	got := DiscardWorktrees([]WorktreeBinding{b}, root)
	if got[0].Error != "" || got[0].BranchAction != "delete" {
		t.Fatalf("report %+v", got[0])
	}
	if registered(t, primary, b.WorktreeAbs) {
		t.Fatal("worktree still registered")
	}
	if branchExists(primary, b.Branch) {
		t.Fatal("merged branch not deleted")
	}
	if _, err := os.Stat(b.WorktreeAbs); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still there: %v", err)
	}
}

// AC2: an unmerged commit keeps the branch; the worktree still goes.
func TestDiscardKeepsUnmergedBranch(t *testing.T) {
	primary, root, b := discardFixture(t)
	commitIn(t, b.WorktreeAbs, "fix.txt")
	got := DiscardWorktrees([]WorktreeBinding{b}, root)
	if got[0].BranchAction != "keep" || got[0].Unmerged != 1 {
		t.Fatalf("report %+v", got[0])
	}
	if !branchExists(primary, b.Branch) {
		t.Fatal("an unmerged branch was deleted")
	}
	if registered(t, primary, b.WorktreeAbs) {
		t.Fatal("worktree still registered")
	}
}

// AC3: a branch its upstream already holds is safe to delete locally.
func TestDiscardDeletesPushedBranch(t *testing.T) {
	primary, root, b := discardFixture(t)
	remote := t.TempDir()
	gitRun(t, remote, "init", "-q", "--bare")
	gitRun(t, primary, "remote", "add", "origin", remote)
	commitIn(t, b.WorktreeAbs, "fix.txt")
	gitRun(t, b.WorktreeAbs, "push", "-q", "-u", "origin", b.Branch)
	got := DiscardWorktrees([]WorktreeBinding{b}, root)
	if got[0].BranchAction != "delete" || got[0].Unmerged != 0 {
		t.Fatalf("report %+v", got[0])
	}
	if branchExists(primary, b.Branch) {
		t.Fatal("pushed branch not deleted")
	}
}

// AC4: the preview names uncommitted files; discarding removes them anyway.
func TestDiscardDirtyWorktree(t *testing.T) {
	primary, root, b := discardFixture(t)
	if err := os.WriteFile(filepath.Join(b.WorktreeAbs, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.WorktreeAbs, "README"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := PlanDiscard([]WorktreeBinding{b}, root)
	if plan[0].UncommittedCount != 2 || strings.Join(plan[0].Uncommitted, ",") != "README,wip.txt" {
		t.Fatalf("plan %+v", plan[0])
	}
	DiscardWorktrees([]WorktreeBinding{b}, root)
	if registered(t, primary, b.WorktreeAbs) {
		t.Fatal("dirty worktree still registered")
	}
	if _, err := os.Stat(b.WorktreeAbs); !os.IsNotExist(err) {
		t.Fatal("dirty worktree dir still there")
	}
}

// AC5: a vanished product repo is reported, not fatal.
func TestDiscardMissingPrimary(t *testing.T) {
	primary, root, b := discardFixture(t)
	if err := os.RemoveAll(primary); err != nil {
		t.Fatal(err)
	}
	got := DiscardWorktrees([]WorktreeBinding{b}, root)
	if !strings.HasPrefix(got[0].Error, "product repo not found") || got[0].BranchAction != "none" {
		t.Fatalf("report %+v", got[0])
	}
}

// Bindings come from agent-writable job.json: a worktree outside the handoff
// is never removed, and a non-job branch is never deleted.
func TestDiscardGuards(t *testing.T) {
	primary, root, b := discardFixture(t)
	outside := b
	outside.WorktreeAbs = primary
	got := DiscardWorktrees([]WorktreeBinding{outside}, root)
	if got[0].Error == "" {
		t.Fatalf("outside path accepted: %+v", got[0])
	}
	if _, err := os.Stat(filepath.Join(primary, "README")); err != nil {
		t.Fatal("the primary checkout was touched")
	}
	notJob := b
	notJob.Branch = "main"
	got = DiscardWorktrees([]WorktreeBinding{notJob}, root)
	if got[0].BranchAction != "none" || !branchExists(primary, "main") {
		t.Fatalf("main must never be deleted: %+v", got[0])
	}
}

// Review finding (high): the handoff is the agent's cwd, so it can swap the
// worktrees/ dir for a symlink to somewhere else. Delete must not follow it.
func TestDiscardRefusesSymlinkOutOfRoot(t *testing.T) {
	primary, root, b := discardFixture(t)
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "precious"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(victim, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	wtDir := filepath.Join(root, "worktrees")
	if err := os.RemoveAll(wtDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, wtDir); err != nil {
		t.Fatal(err)
	}
	got := DiscardWorktrees([]WorktreeBinding{b}, root)
	if got[0].Error == "" {
		t.Fatalf("symlinked path accepted: %+v", got[0])
	}
	if _, err := os.Stat(filepath.Join(victim, "demo")); err != nil {
		t.Fatal("followed the symlink and removed the target")
	}
	if !branchExists(primary, b.Branch) {
		t.Fatal("branch touched through a refused binding")
	}
	self := b
	self.WorktreeAbs = root
	if got := DiscardWorktrees([]WorktreeBinding{self}, root); got[0].Error == "" {
		t.Fatalf("the handoff root itself accepted: %+v", got[0])
	}
}

// Review finding (critical): an agent-written PrimaryAbs pointing at another
// repo must not get git run in it — no prune, no branch delete — because the
// worktree is not registered there.
func TestDiscardIgnoresForeignPrimary(t *testing.T) {
	_, root, b := discardFixture(t)
	other := t.TempDir()
	gitRun(t, other, "init", "-q", "-b", "main")
	commitIn(t, other, "README")
	gitRun(t, other, "branch", b.Branch) // same job-branch name, merged
	forged := b
	forged.PrimaryAbs = other
	plan := PlanDiscard([]WorktreeBinding{forged}, root)
	if plan[0].BranchAction != "none" || !strings.Contains(plan[0].Reason, "not registered") {
		t.Fatalf("plan %+v", plan[0])
	}
	DiscardWorktrees([]WorktreeBinding{forged}, root)
	if !branchExists(other, b.Branch) {
		t.Fatal("deleted a branch in a repo the worktree does not belong to")
	}
}
