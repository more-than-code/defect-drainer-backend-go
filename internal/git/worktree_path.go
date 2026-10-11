package git

import (
	"fmt"
	"path/filepath"
	"strings"
)

// safeRepoSegment is one path segment. Names that could escape the worktrees
// root ("..", "a/../../b", separators) are rejected before any filesystem call.
func safeRepoSegment(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// worktreeDir is root/repo, and only when that path is strictly inside root.
func worktreeDir(root, repo string) (string, error) {
	if !safeRepoSegment(repo) {
		return "", fmt.Errorf("%s: invalid repo name", repo)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	joined := filepath.Join(absRoot, repo)
	if filepath.Base(joined) != repo || !strictlyInside(absRoot, joined) {
		return "", fmt.Errorf("%s: worktree path escapes the handoff", repo)
	}
	return joined, nil
}

// confinedWorktree reports whether a binding's worktree is a single safe
// segment under its parent. RemoveBatchWorktrees refuses anything else.
func confinedWorktree(b WorktreeBinding) bool {
	if !safeRepoSegment(b.Repo) || b.WorktreeAbs == "" {
		return false
	}
	clean := filepath.Clean(b.WorktreeAbs)
	if filepath.Base(clean) != b.Repo {
		return false
	}
	return strictlyInside(filepath.Dir(clean), clean)
}
