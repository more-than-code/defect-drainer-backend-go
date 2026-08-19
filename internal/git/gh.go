package git

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PRResult is one row written onto job.prs[].
type PRResult struct {
	Repo      string `json:"repo,omitempty"`
	URL       string `json:"url,omitempty"`
	Number    int    `json:"number,omitempty"`
	Status    string `json:"status,omitempty"`
	GhState   string `json:"ghState,omitempty"`
	MergedAt  string `json:"mergedAt,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
	Base      string `json:"base,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Error     string `json:"error,omitempty"`
	Commits   int    `json:"commits,omitempty"`
	GhError   string `json:"ghError,omitempty"`
}

// GhBin is GH_BIN or "gh" (tests inject a fake at GH_BIN).
func GhBin() string {
	if v := os.Getenv("GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

func ghCmd(cwd string, args ...string) *exec.Cmd {
	cmd := exec.Command(GhBin(), args...)
	cmd.Dir = cwd
	return cmd
}

func nowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// AcceptPRURL returns s when it is a usable `gh pr view` target: https
// with a non-empty host, and not a leading-dash flag injection. Empty
// otherwise. Hydrated prs[].url is untrusted — call this on load too.
func AcceptPRURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return s
}

func createdPRURL(raw []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(raw)))
	if len(fields) == 0 {
		return ""
	}
	return AcceptPRURL(fields[len(fields)-1])
}

func mapGhState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open":
		return "open"
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return ""
	}
}

// CreatePRsForWorktrees pushes (best-effort) and runs `gh pr create` per binding.
func CreatePRsForWorktrees(bindings []WorktreeBinding, batchID, title string) []PRResult {
	var out []PRResult
	for _, b := range bindings {
		_, branch, _ := ResolveBaseRef(b.PrimaryAbs, "origin", "main")
		if branch == "" {
			branch = "main"
		}
		entry := PRResult{Repo: b.Repo, Branch: b.Branch, Base: branch, Status: "failed"}
		if st, err := os.Stat(b.WorktreeAbs); err != nil || !st.IsDir() || !IsGitRepo(b.WorktreeAbs) {
			entry.Error = "worktree missing"
			out = append(out, entry)
			continue
		}
		ref, _, _ := ResolveBaseRef(b.PrimaryAbs, "origin", "main")
		commits := countCommits(b.WorktreeAbs, ref)
		if commits == 0 {
			commits = countCommits(b.WorktreeAbs, branch)
		}
		entry.Commits = commits
		if commits == 0 && !HasCommitsVsBase(b.WorktreeAbs, ref) && !HasCommitsVsBase(b.WorktreeAbs, branch) {
			entry.Status = "skipped"
			entry.Error = "no commits ahead of base"
			out = append(out, entry)
			continue
		}
		_ = gitC(b.WorktreeAbs, "push", "-u", "origin", "HEAD:"+b.Branch) // best-effort
		// existing?
		if raw, err := ghCmd(b.WorktreeAbs, "pr", "list", "--head", b.Branch, "--base", branch, "--json", "url,number,state", "--limit", "1").Output(); err == nil {
			var list []struct {
				URL    string `json:"url"`
				Number int    `json:"number"`
				State  string `json:"state"`
			}
			if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
				u := AcceptPRURL(list[0].URL)
				if u != "" || list[0].Number != 0 {
					entry.Status = "existing"
					entry.URL = u
					entry.Number = list[0].Number
					entry.GhState = mapGhState(list[0].State)
					if entry.GhState == "" {
						entry.GhState = "open"
					}
					entry.CheckedAt = nowIso()
					out = append(out, entry)
					continue
				}
			}
		}
		raw, err := ghCmd(b.WorktreeAbs, "pr", "create", "--base", branch, "--head", b.Branch, "--title", title, "--body", "Batch "+batchID).CombinedOutput()
		if err != nil {
			entry.Error = strings.TrimSpace(string(raw))
			if entry.Error == "" {
				entry.Error = err.Error()
			}
			if len(entry.Error) > 500 {
				entry.Error = entry.Error[:500]
			}
			out = append(out, entry)
			continue
		}
		url := createdPRURL(raw)
		if url == "" {
			entry.Error = "gh pr create returned a non-https URL"
			out = append(out, entry)
			continue
		}
		entry.Status = "created"
		entry.URL = url
		if entry.Number == 0 {
			entry.Number = prNumberFromURL(url)
		}
		entry.GhState = "open"
		entry.CheckedAt = nowIso()
		out = append(out, entry)
	}
	return out
}

// RefreshPRStatuses runs `gh pr view` (or list) and writes ghState/mergedAt/checkedAt.
func RefreshPRStatuses(prs []PRResult, worktrees []WorktreeBinding) []PRResult {
	byRepo := map[string]WorktreeBinding{}
	for _, w := range worktrees {
		byRepo[w.Repo] = w
	}
	out := make([]PRResult, 0, len(prs))
	for _, prev := range prs {
		entry := prev
		entry.URL = AcceptPRURL(entry.URL)
		trackable := entry.URL != "" || entry.Number != 0 || entry.Status == "created" || entry.Status == "existing"
		if !trackable {
			out = append(out, entry)
			continue
		}
		cwd := ""
		if w, ok := byRepo[entry.Repo]; ok {
			if IsGitRepo(w.WorktreeAbs) {
				cwd = w.WorktreeAbs
			} else if IsGitRepo(w.PrimaryAbs) {
				cwd = w.PrimaryAbs
			}
		}
		var raw []byte
		var err error
		if entry.Number != 0 {
			raw, err = ghCmd(cwd, "pr", "view", strconv.Itoa(entry.Number), "--json", "state,mergedAt,number,url").Output()
		} else if u := AcceptPRURL(entry.URL); u != "" {
			raw, err = ghCmd(cwd, "pr", "view", u, "--json", "state,mergedAt,number,url").Output()
		} else if entry.Branch != "" {
			raw, err = ghCmd(cwd, "pr", "list", "--head", entry.Branch, "--base", orMain(entry.Base), "--state", "all", "--json", "state,mergedAt,number,url", "--limit", "1").Output()
		} else {
			out = append(out, entry)
			continue
		}
		if err != nil {
			msg := err.Error()
			if len(msg) > 500 {
				msg = msg[:500]
			}
			entry.GhError = msg
			entry.CheckedAt = nowIso()
			out = append(out, entry)
			continue
		}
		var view struct {
			State    string `json:"state"`
			MergedAt string `json:"mergedAt"`
			Number   int    `json:"number"`
			URL      string `json:"url"`
		}
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "[") {
			var list []struct {
				State    string `json:"state"`
				MergedAt string `json:"mergedAt"`
				Number   int    `json:"number"`
				URL      string `json:"url"`
			}
			if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
				view.State, view.MergedAt, view.Number, view.URL = list[0].State, list[0].MergedAt, list[0].Number, list[0].URL
			} else if entry.Branch != "" && entry.URL == "" {
				entry.GhError = "no PR found for branch"
				entry.CheckedAt = nowIso()
				out = append(out, entry)
				continue
			}
		} else {
			_ = json.Unmarshal(raw, &view)
		}
		if st := mapGhState(view.State); st != "" {
			entry.GhState = st
		}
		entry.MergedAt = view.MergedAt
		if u := AcceptPRURL(view.URL); u != "" {
			entry.URL = u
		} else if entry.URL != "" && AcceptPRURL(entry.URL) == "" {
			entry.URL = ""
		}
		if view.Number != 0 {
			entry.Number = view.Number
		}
		entry.CheckedAt = nowIso()
		entry.GhError = ""
		out = append(out, entry)
	}
	return out
}

func orMain(s string) string {
	if s == "" {
		return "main"
	}
	return s
}

func countCommits(worktreeAbs, baseRef string) int {
	if baseRef == "" {
		return 0
	}
	out, err := gitCOut(worktreeAbs, "rev-list", "--count", baseRef+"..HEAD")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func prNumberFromURL(url string) int {
	url = strings.TrimRight(url, "/")
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return ParsePRNumber(url[i+1:])
	}
	return 0
}

// ParsePRNumber is a tiny helper for tests.
func ParsePRNumber(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
