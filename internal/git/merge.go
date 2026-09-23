package git

import (
	"os"
	"strings"
)

// mergeIntoLocalBase merges the job branch into the configured local branch.
// The primary working tree is updated only when it is already on that branch
// and clean. Otherwise the merge runs in a temporary worktree so the
// operator's checkout stays where it is. A dirty checkout of the base is refused.
func mergeIntoLocalBase(b WorktreeBinding, ref, branch string) PRResult {
	entry := PRResult{
		Repo: b.Repo, Branch: b.Branch, Base: branch, Kind: "merge", Status: "failed",
	}
	if !validRefToken(branch) || !validRefToken(b.Branch) {
		entry.Error = "invalid branch name"
		return entry
	}
	if st, err := os.Stat(b.WorktreeAbs); err != nil || !st.IsDir() || !IsGitRepo(b.WorktreeAbs) {
		entry.Error = "worktree missing"
		return entry
	}
	commits := countCommits(b.WorktreeAbs, ref)
	if commits == 0 {
		commits = countCommits(b.WorktreeAbs, branch)
	}
	entry.Commits = commits
	if commits == 0 {
		entry.Status = "skipped"
		entry.Error = "no commits ahead of base"
		return entry
	}
	head, err := gitCOut(b.PrimaryAbs, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		entry.Error = "cannot read primary HEAD"
		return entry
	}
	dir := b.PrimaryAbs
	if head == branch {
		dirty, derr := gitCOut(b.PrimaryAbs, "status", "--porcelain")
		if derr != nil {
			entry.Error = "cannot read primary status"
			return entry
		}
		if strings.TrimSpace(dirty) != "" {
			entry.Error = "primary checkout of " + branch + " is dirty; commit or stash before merging"
			return entry
		}
	} else {
		tmp, terr := os.MkdirTemp("", "dd-merge-")
		if terr != nil {
			entry.Error = terr.Error()
			return entry
		}
		_ = os.RemoveAll(tmp)
		if err := gitC(b.PrimaryAbs, "worktree", "add", tmp, branch); err != nil {
			_ = os.RemoveAll(tmp)
			entry.Error = clipGit(err.Error())
			return entry
		}
		defer func() {
			_ = gitC(b.PrimaryAbs, "worktree", "remove", "--force", tmp)
			_ = os.RemoveAll(tmp)
		}()
		dir = tmp
	}
	if err := gitC(dir, "merge", "--no-edit", "--", b.Branch); err != nil {
		_ = gitC(dir, "merge", "--abort")
		entry.Error = clipGit(err.Error())
		return entry
	}
	entry.Status = "merged"
	entry.GhState = "merged"
	entry.MergedAt = nowIso()
	entry.CheckedAt = entry.MergedAt
	return entry
}

func confirmLocalMerge(entry *PRResult, worktrees []WorktreeBinding) {
	entry.Kind = "merge"
	entry.CheckedAt = nowIso()
	var primary string
	for _, w := range worktrees {
		if w.Repo == entry.Repo && IsGitRepo(w.PrimaryAbs) {
			primary = w.PrimaryAbs
			break
		}
	}
	if primary == "" || !validRefToken(entry.Branch) || !validRefToken(entry.Base) {
		entry.GhError = "cannot confirm local merge"
		return
	}
	if gitOk(primary, "merge-base", "--is-ancestor", "--end-of-options", entry.Branch, entry.Base) {
		entry.GhState = "merged"
		entry.GhError = ""
		return
	}
	entry.GhError = "job branch is not contained in " + entry.Base
}

func clipGit(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
