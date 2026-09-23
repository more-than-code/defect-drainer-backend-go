package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// WorktreeBinding is one isolated batch worktree.
// BaseRef is the commit-ish the job branch was cut from. BaseBranch is the
// name PRs and local merges target. BaseSource is "local" or "origin".
type WorktreeBinding struct {
	Repo        string `json:"repo"`
	PrimaryAbs  string `json:"primaryAbs"`
	WorktreeAbs string `json:"worktreeAbs"`
	Branch      string `json:"branch"`
	RepoURL     string `json:"repoUrl,omitempty"`
	BaseRef     string `json:"baseRef,omitempty"`
	BaseBranch  string `json:"baseBranch,omitempty"`
	BaseSource  string `json:"baseSource,omitempty"`
}

// WorktreeRequest is one product repo to branch for a batch.
// Source "local" starts at the local branch. Source "origin" starts at
// <Remote>/<Branch>, then the local branch if that remote ref is absent.
type WorktreeRequest struct {
	PrimaryAbs string
	Source     string
	Remote     string
	Branch     string
}

func gitC(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitCOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// IsGitRepo is rev-parse --is-inside-work-tree.
func IsGitRepo(dir string) bool {
	return gitC(dir, "rev-parse", "--is-inside-work-tree") == nil
}

// CreateBatchWorktrees fail-closed. Branch defect-drainer/<BATCH-id>, cut from
// each repo's configured base. A missing base fails the batch; it does not
// fall back to main or HEAD.
func CreateBatchWorktrees(batchID, worktreesRoot string, repos map[string]WorktreeRequest) ([]WorktreeBinding, error) {
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
	branch := "defect-drainer/" + batchID
	var bindings []WorktreeBinding
	for _, repo := range names {
		req := repos[repo]
		primaryAbs, err := filepath.Abs(req.PrimaryAbs)
		if err != nil {
			RemoveBatchWorktrees(bindings)
			return nil, err
		}
		if st, err := os.Stat(primaryAbs); err != nil || !st.IsDir() {
			RemoveBatchWorktrees(bindings)
			return nil, fmt.Errorf("primary checkout missing for %s: %s", repo, primaryAbs)
		}
		if !IsGitRepo(primaryAbs) {
			RemoveBatchWorktrees(bindings)
			return nil, fmt.Errorf("not a git repo: %s", primaryAbs)
		}
		source := normalizeBaseSource(req.Source)
		if source == "origin" {
			FetchBaseRef(primaryAbs, req.Remote, req.Branch)
		}
		ref, baseBranch, err := ResolveJobBase(primaryAbs, source, req.Remote, req.Branch)
		if err != nil {
			RemoveBatchWorktrees(bindings)
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		worktreeAbs := filepath.Join(worktreesRoot, repo)
		if _, err := os.Stat(worktreeAbs); err == nil {
			_ = gitC(primaryAbs, "worktree", "remove", "--force", worktreeAbs)
			_ = os.RemoveAll(worktreeAbs)
			_ = gitC(primaryAbs, "worktree", "prune")
		}
		_ = gitC(primaryAbs, "branch", "-D", branch)
		if err := gitC(primaryAbs, "worktree", "add", "-b", branch, worktreeAbs, ref); err != nil {
			RemoveBatchWorktrees(bindings)
			return nil, fmt.Errorf("git worktree add failed for %s (base=%s, branch=%s): %v", repo, ref, branch, err)
		}
		bindings = append(bindings, WorktreeBinding{
			Repo: repo, PrimaryAbs: primaryAbs, WorktreeAbs: worktreeAbs, Branch: branch,
			BaseRef: ref, BaseBranch: baseBranch, BaseSource: source,
		})
	}
	return bindings, nil
}

// RemoveBatchWorktrees is best-effort cleanup.
func RemoveBatchWorktrees(bindings []WorktreeBinding) {
	for _, b := range bindings {
		_ = gitC(b.PrimaryAbs, "worktree", "remove", "--force", b.WorktreeAbs)
		_ = os.RemoveAll(b.WorktreeAbs)
		_ = gitC(b.PrimaryAbs, "worktree", "prune")
		_ = gitC(b.PrimaryAbs, "branch", "-D", b.Branch)
	}
}

// HasCommitsVsBase is true when worktree HEAD is not the same as base.
func HasCommitsVsBase(worktreeAbs, baseRef string) bool {
	out, err := gitCOut(worktreeAbs, "rev-list", "--count", baseRef+"..HEAD")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != "0"
}

// CreatePullRequest runs `gh pr create`.
func CreatePullRequest(worktreeAbs, title, body, base string) (url string, err error) {
	cmd := exec.Command("gh", "pr", "create", "--title", title, "--body", body, "--base", base)
	cmd.Dir = worktreeAbs
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RefreshPR runs `gh pr view --json`.
func RefreshPR(worktreeAbs string) (state, mergedAt, url string, err error) {
	cmd := exec.Command("gh", "pr", "view", "--json", "state,mergedAt,url")
	cmd.Dir = worktreeAbs
	out, err := cmd.Output()
	if err != nil {
		return "", "", "", fmt.Errorf("gh pr view failed")
	}
	s := string(out)
	// tiny extract without extra deps
	state = jsonField(s, "state")
	mergedAt = jsonField(s, "mergedAt")
	url = jsonField(s, "url")
	return state, mergedAt, url, nil
}

func jsonField(raw, key string) string {
	// "key":"value" or "key": null
	needle := `"` + key + `":`
	i := strings.Index(raw, needle)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(raw[i+len(needle):])
	if strings.HasPrefix(rest, "null") {
		return ""
	}
	if strings.HasPrefix(rest, `"`) {
		rest = rest[1:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return ""
		}
		return rest[:j]
	}
	return ""
}

// ResolveGrokBin matches TS order.
func ResolveGrokBin() string {
	for _, k := range []string{"GROK_BUILD_BIN", "DEFECT_DRAINER_GROK_BIN", "SKETCH_FORGE_GROK_BIN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	for _, p := range []string{"/opt/homebrew/bin/grok", "/usr/local/bin/grok"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("grok"); err == nil {
		return p
	}
	return ""
}
