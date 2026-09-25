package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DetachRequest is one product repo checked out detached at a resolved commit.
// An empty Ref uses ResolveJobBase (the same base a fix job would cut from).
type DetachRequest struct {
	PrimaryAbs string
	Source     string
	Remote     string
	Branch     string
	Ref        string
}

// ResolvedDetach is the commit a detached worktree was created at.
type ResolvedDetach struct {
	Binding WorktreeBinding
	// Ref is the requested ref, or the base branch when none was requested.
	Ref string
	SHA string
}

// CreateDetachedWorktrees adds one detached worktree per repo at a resolved
// commit. No branch is created. A failure removes the worktrees already added.
func CreateDetachedWorktrees(worktreesRoot string, repos map[string]DetachRequest) ([]ResolvedDetach, error) {
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, r)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no product repos resolved for worktrees — set defect.repos and/or app.repos + workspace_root")
	}
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return nil, err
	}
	var made []WorktreeBinding
	var out []ResolvedDetach
	fail := func(err error) ([]ResolvedDetach, error) {
		RemoveBatchWorktrees(made)
		return nil, err
	}
	for _, repo := range names {
		req := repos[repo]
		if _, err := worktreeDir(worktreesRoot, repo); err != nil {
			return fail(err)
		}
		primaryAbs, err := filepath.Abs(req.PrimaryAbs)
		if err != nil {
			return fail(err)
		}
		if st, err := os.Stat(primaryAbs); err != nil || !st.IsDir() {
			return fail(fmt.Errorf("primary checkout missing for %s: %s", repo, primaryAbs))
		}
		if !IsGitRepo(primaryAbs) {
			return fail(fmt.Errorf("not a git repo: %s", primaryAbs))
		}
		resolved, recordedRef, sha, err := resolveDetachRef(primaryAbs, repo, req)
		if err != nil {
			return fail(err)
		}
		worktreeAbs, err := worktreeDir(worktreesRoot, repo)
		if err != nil {
			return fail(err)
		}
		if _, err := os.Stat(worktreeAbs); err == nil {
			_ = gitC(primaryAbs, "worktree", "remove", "--force", worktreeAbs)
			_ = os.RemoveAll(worktreeAbs)
			_ = gitC(primaryAbs, "worktree", "prune")
		}
		if err := gitC(primaryAbs, "worktree", "add", "--detach", worktreeAbs, sha); err != nil {
			return fail(fmt.Errorf("git worktree add failed for %s (commit=%s): %v", repo, sha, err))
		}
		source := normalizeBaseSource(req.Source)
		binding := WorktreeBinding{
			Repo: repo, PrimaryAbs: primaryAbs, WorktreeAbs: worktreeAbs,
			BaseRef: resolved, BaseBranch: recordedBase(req, recordedRef), BaseSource: source,
		}
		made = append(made, binding)
		out = append(out, ResolvedDetach{Binding: binding, Ref: recordedRef, SHA: sha})
	}
	return out, nil
}

// recordedBase is the branch name stored when the run did not name a ref.
// A requested ref keeps that ref; the base branch is still the settings branch.
func recordedBase(req DetachRequest, recordedRef string) string {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = "main"
	}
	if strings.TrimSpace(req.Ref) == "" {
		return recordedRef
	}
	return branch
}

// resolveDetachRef resolves req to a 40-character commit.
// No ref: ResolveJobBase, including its origin fetch.
// A ref must pass validRefToken. Origin tries <remote>/<ref> then <ref> after
// a best-effort fetch. Local tries <ref> only.
func resolveDetachRef(primary, repo string, req DetachRequest) (resolved, recordedRef, sha string, err error) {
	source := normalizeBaseSource(req.Source)
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		if source == "origin" {
			remote := strings.TrimSpace(req.Remote)
			if remote == "" {
				remote = "origin"
			}
			remote = strings.TrimPrefix(remote, "origin/")
			branch := strings.TrimSpace(req.Branch)
			if branch == "" {
				branch = "main"
			}
			FetchBaseRef(primary, remote, branch)
		}
		resolved, recordedRef, err = ResolveJobBase(primary, source, req.Remote, req.Branch)
		if err != nil {
			return "", "", "", fmt.Errorf("%s: %w", repo, err)
		}
		sha, err = revParseCommit(primary, resolved)
		if err != nil {
			return "", "", "", fmt.Errorf("%s: ref %s not found", repo, recordedRef)
		}
		return resolved, recordedRef, sha, nil
	}
	// Same allowlist as store.ParseGitRefName, before any git call.
	if !detachRefAllowed(ref) {
		return "", "", "", fmt.Errorf("%s: ref %s not found", repo, ref)
	}
	var candidates []string
	if source == "origin" {
		remote := strings.TrimSpace(req.Remote)
		if remote == "" {
			remote = "origin"
		}
		remote = strings.TrimPrefix(remote, "origin/")
		if !detachRefAllowed(remote) {
			return "", "", "", fmt.Errorf("%s: ref %s not found", repo, ref)
		}
		fetchDetachRef(primary, remote, ref)
		candidates = append(candidates, remote+"/"+ref)
	}
	candidates = append(candidates, ref)
	for _, cand := range candidates {
		sha, err = revParseCommit(primary, cand)
		if err == nil {
			return cand, ref, sha, nil
		}
	}
	return "", "", "", fmt.Errorf("%s: ref %s not found", repo, ref)
}

// detachRefAllowed matches store.ParseGitRefName's accept rule (fallback "").
var detachRefRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,120}$`)

func detachRefAllowed(ref string) bool {
	if !detachRefRe.MatchString(ref) {
		return false
	}
	return !strings.HasPrefix(ref, "-") && !strings.Contains(ref, "..") && !strings.HasSuffix(ref, "/")
}

// fetchDetachRef is the test-run fetch. It is not FetchBaseRef: fix jobs keep
// that command, and this one puts --end-of-options before the remote and ref.
func fetchDetachRef(primary, remote, ref string) {
	if remote == "" {
		remote = "origin"
	}
	_ = exec.Command("git", "-C", primary, "fetch", "--no-tags", "--end-of-options", remote, ref).Run()
}

func revParseCommit(dir, rev string) (string, error) {
	out, err := gitCOut(dir, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if len(out) != 40 {
		return "", fmt.Errorf("not a commit: %s", rev)
	}
	return out, nil
}
