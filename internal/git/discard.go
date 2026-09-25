package git

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// WorktreeDiscard is what deleting a job does (PlanDiscard) or did
// (DiscardWorktrees) to one product repo.
type WorktreeDiscard struct {
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	// Present is true when the worktree is still a usable checkout.
	Present bool `json:"present"`
	// Uncommitted lists up to maxDiscardList paths that deleting discards.
	Uncommitted      []string `json:"uncommitted,omitempty"`
	UncommittedCount int      `json:"uncommittedCount"`
	Branch           string   `json:"branch"`
	// Unmerged counts branch commits not reachable from the base, the
	// primary's HEAD, or the branch's upstream. -1 when it cannot be told.
	Unmerged int `json:"unmerged"`
	// BranchAction is "delete", "keep" or "none" (no such branch).
	BranchAction string `json:"branchAction"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

const maxDiscardList = 50

// jobBranch is the only branch shape a job creates. Job bindings are read
// back from job.json, which the agent can write, so nothing else is ever
// deleted — a tampered "main" stays.
var jobBranch = regexp.MustCompile(`^defect-drainer/[A-Za-z0-9._-]+$`)

// strictlyInside reports whether p is below root (never root itself).
func strictlyInside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// insideRoot reports whether a binding's worktree may be removed: strictly
// inside the job's handoff both as written and, when it exists, with every
// symlink resolved. The same reason as jobBranch — bindings are agent-written
// — and the handoff is the agent's cwd, so a swapped-in symlink (say, the
// worktrees/ dir pointing elsewhere) must not carry a RemoveAll outside it.
// Delete runs after the agent is dead, so the path cannot change under us.
func insideRoot(root, p string) bool {
	if root == "" || p == "" || !strictlyInside(filepath.Clean(root), filepath.Clean(p)) {
		return false
	}
	if _, err := os.Lstat(p); err != nil {
		return true // nothing on disk to follow
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realP, err := filepath.EvalSymlinks(p)
	return err == nil && strictlyInside(realRoot, realP)
}

// registeredIn reports whether primary lists worktree among its worktrees.
// That registration lives in primary's own .git, which the agent cannot write,
// so it is what ties an agent-written PrimaryAbs to this job: without it, no
// git command runs in that repo.
func registeredIn(primary, worktree string) bool {
	out, err := gitCOut(primary, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	want := map[string]bool{filepath.Clean(worktree): true}
	if real, err := filepath.EvalSymlinks(worktree); err == nil {
		want[real] = true
	}
	for _, line := range strings.Split(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			if want[filepath.Clean(path)] {
				return true
			}
			if real, err := filepath.EvalSymlinks(path); err == nil && want[real] {
				return true
			}
		}
	}
	return false
}

func refExists(dir, ref string) bool {
	return ref != "" && gitC(dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}") == nil
}

// unmergedCommits counts the branch's commits that no safe ref already holds:
// BaseBranch and BaseRef, the primary's HEAD, and the branch's own upstream (a
// pushed branch loses nothing locally). Callers must set BaseBranch/BaseRef
// from the server's saved settings, never from job.json: an agent-written
// BaseRef equal to the branch tip would make any branch look merged.
func unmergedCommits(b WorktreeBinding) int {
	branchRef := "refs/heads/" + b.Branch
	var keep []string
	for _, ref := range []string{
		b.BaseBranch,
		b.BaseRef,
		"HEAD",
		// @{upstream} resolves only on the short name (refs/heads/x@{u} does
		// not); b.Branch is already checked against jobBranch.
		b.Branch + "@{upstream}",
	} {
		if refExists(b.PrimaryAbs, ref) {
			keep = append(keep, ref)
		}
	}
	args := append([]string{"rev-list", "--count", branchRef, "--not"}, keep...)
	out, err := gitCOut(b.PrimaryAbs, args...)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

func planOne(b WorktreeBinding, root string) WorktreeDiscard {
	d := WorktreeDiscard{Repo: b.Repo, Worktree: b.WorktreeAbs, Branch: b.Branch, Unmerged: -1, BranchAction: "none"}
	if !insideRoot(root, b.WorktreeAbs) {
		d.Error = "worktree path is outside this job's handoff; left alone"
		return d
	}
	if st, err := os.Stat(b.WorktreeAbs); err == nil && st.IsDir() && IsGitRepo(b.WorktreeAbs) {
		d.Present = true
		if files, err := dirtyFiles(b.WorktreeAbs); err == nil {
			d.UncommittedCount = len(files)
			if len(files) > maxDiscardList {
				files = files[:maxDiscardList]
			}
			d.Uncommitted = files
		}
	}
	if !IsGitRepo(b.PrimaryAbs) {
		d.Error = "product repo not found: " + b.PrimaryAbs
		return d
	}
	if !registeredIn(b.PrimaryAbs, b.WorktreeAbs) {
		d.Reason = "not registered in " + b.PrimaryAbs + "; repo left alone"
		return d
	}
	if strings.TrimSpace(b.Branch) == "" {
		d.Reason = "detached worktree; no branch to delete"
		return d
	}
	if !jobBranch.MatchString(b.Branch) {
		d.Reason = "not a job branch; left alone"
		return d
	}
	if !refExists(b.PrimaryAbs, "refs/heads/"+b.Branch) {
		d.Reason = "branch already gone"
		return d
	}
	d.Unmerged = unmergedCommits(b)
	switch {
	case d.Unmerged == 0:
		d.BranchAction = "delete"
		d.Reason = "merged"
	case d.Unmerged > 0:
		d.BranchAction = "keep"
		d.Reason = strconv.Itoa(d.Unmerged) + " unmerged commit(s)"
	default:
		d.BranchAction = "keep"
		d.Reason = "could not tell whether it is merged"
	}
	return d
}

// PlanDiscard reports, without changing anything, what DiscardWorktrees would
// do to each binding. root is the job's handoff directory.
func PlanDiscard(bindings []WorktreeBinding, root string) []WorktreeDiscard {
	out := make([]WorktreeDiscard, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, planOne(b, root))
	}
	return out
}

// DiscardWorktrees removes each job worktree and its registration, and deletes
// the job branch only when it is merged at that moment. It never stops at an
// error; each repo reports its own. Uncommitted edits are discarded — callers
// show PlanDiscard first.
func DiscardWorktrees(bindings []WorktreeBinding, root string) []WorktreeDiscard {
	out := make([]WorktreeDiscard, 0, len(bindings))
	for _, b := range bindings {
		d := planOne(b, root)
		if !insideRoot(root, b.WorktreeAbs) {
			out = append(out, d) // never touched
			continue
		}
		if !IsGitRepo(b.PrimaryAbs) || !registeredIn(b.PrimaryAbs, b.WorktreeAbs) {
			// No repo, or not provably this worktree's repo: run no git there.
			// The handoff removal takes the files.
			out = append(out, d)
			continue
		}
		if d.Present {
			if err := gitC(b.PrimaryAbs, "worktree", "remove", "--force", b.WorktreeAbs); err != nil {
				_ = os.RemoveAll(b.WorktreeAbs)
			}
		} else {
			_ = os.RemoveAll(b.WorktreeAbs)
		}
		if err := gitC(b.PrimaryAbs, "worktree", "prune"); err != nil {
			d.Error = "worktree prune: " + err.Error()
		}
		if d.BranchAction == "delete" {
			// Re-check: the plan may be stale, and -D must never drop work.
			if n := unmergedCommits(b); n != 0 {
				d.BranchAction, d.Unmerged = "keep", n
				d.Reason = "unmerged commits appeared since the preview"
			} else if err := gitC(b.PrimaryAbs, "branch", "-D", b.Branch); err != nil {
				d.BranchAction = "keep"
				d.Error = "branch delete: " + err.Error()
			}
		}
		out = append(out, d)
	}
	return out
}
