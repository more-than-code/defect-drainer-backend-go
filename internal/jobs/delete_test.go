package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// AC1 through DeleteJob: a job's worktree is unregistered from the product
// repo, its merged branch deleted and its handoff removed, and the report says
// so. Before this, Delete left the registration and branch behind.
func TestDeleteJobCleansProductRepo(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	primary := t.TempDir()
	gitRun(t, primary, "init", "-q", "-b", "main")
	commitFile(t, primary, "README")

	const id = "bjob_delclean"
	bindings, err := git.CreateBatchWorktrees("BATCH-20260924-dc", filepath.Join(br.handoffDir(id), "worktrees"), map[string]git.WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	br.jobs[id] = &Job{ID: id, Kind: "batch", Status: "completed", Mode: "grok",
		BatchID: "BATCH-20260924-dc", AppID: store.SeededTutoredWebappAppID, Worktrees: bindings}

	preview, err := br.DeletePreview(id)
	if err != nil || len(preview) != 1 || preview[0].BranchAction != "delete" {
		t.Fatalf("preview %+v %v", preview, err)
	}
	cleanup, err := br.DeleteJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) != 1 || cleanup[0].BranchAction != "delete" || cleanup[0].Error != "" {
		t.Fatalf("cleanup %+v", cleanup)
	}
	list := gitRun(t, primary, "worktree", "list", "--porcelain")
	if strings.Contains(list, "bjob_delclean") {
		t.Fatalf("worktree still registered:\n%s", list)
	}
	if branches := gitRun(t, primary, "branch", "--list", "defect-drainer/*"); branches != "" {
		t.Fatalf("job branch left behind: %q", branches)
	}
	if _, err := os.Stat(br.handoffDir(id)); !os.IsNotExist(err) {
		t.Fatalf("handoff still there: %v", err)
	}
	if _, err := br.DeletePreview(id); err == nil {
		t.Fatal("preview of a deleted job must be not-found")
	}
}

// Review finding (critical): job.json is agent-writable, and a BaseRef set to
// the branch's own tip made any branch count as merged. Bases now come from
// the app's saved settings, so the forged value is ignored and the unmerged
// commit survives.
func TestDeleteJobIgnoresForgedBaseRef(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	primary := t.TempDir()
	gitRun(t, primary, "init", "-q", "-b", "main")
	commitFile(t, primary, "README")
	const id = "bjob_delforged"
	bindings, err := git.CreateBatchWorktrees("BATCH-20260924-df", filepath.Join(br.handoffDir(id), "worktrees"), map[string]git.WorktreeRequest{
		"demo": {PrimaryAbs: primary, Source: "local", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tip := commitFile(t, bindings[0].WorktreeAbs, "agent-fix.txt")
	bindings[0].BaseRef, bindings[0].BaseBranch = tip, tip
	br.jobs[id] = &Job{ID: id, Kind: "batch", Status: "completed", Mode: "grok",
		BatchID: "BATCH-20260924-df", AppID: store.SeededTutoredWebappAppID, Worktrees: bindings}

	cleanup, err := br.DeleteJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup[0].BranchAction != "keep" || cleanup[0].Unmerged != 1 {
		t.Fatalf("forged base let an unmerged branch go: %+v", cleanup[0])
	}
	if branches := gitRun(t, primary, "branch", "--list", "defect-drainer/*"); !strings.Contains(branches, "BATCH-20260924-df") {
		t.Fatalf("unmerged branch deleted: %q", branches)
	}
}
