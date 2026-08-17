// Package git ports parseGitRepoUrl, listRepoBranches, chooseLocalFolder, and later worktrees/gh.
package git

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ParsedRepo is a sanitized git remote or local path.
type ParsedRepo struct {
	Scheme   string
	Host     string
	Path     string
	Name     string
	Location string
	Source   string // origin | local
}

// ParseGitRepoURL allowlists schemes and rejects .., newlines, NUL.
func ParseGitRepoURL(raw string) (ParsedRepo, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ParsedRepo{}, fmt.Errorf("repo URL is empty")
	}
	if len(s) > 2048 {
		return ParsedRepo{}, fmt.Errorf("repo URL too long")
	}
	if strings.Contains(s, "\n") || strings.Contains(s, "\r") || strings.Contains(s, "\x00") {
		return ParsedRepo{}, fmt.Errorf("invalid repo URL")
	}
	if strings.Contains(s, "..") {
		return ParsedRepo{}, fmt.Errorf("repo URL must not contain ..")
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "file://") {
		p := strings.TrimPrefix(s, "file://")
		if !strings.HasPrefix(p, "/") {
			return ParsedRepo{}, fmt.Errorf("invalid repo URL")
		}
		return ParsedRepo{Scheme: "file", Path: p, Name: leafName(p), Location: p, Source: "local"}, nil
	}
	if strings.HasPrefix(s, "git@") {
		// git@host:path
		rest := strings.TrimPrefix(s, "git@")
		host, path, ok := strings.Cut(rest, ":")
		if !ok || host == "" || path == "" {
			return ParsedRepo{}, fmt.Errorf("invalid repo URL")
		}
		if !allowHost(host) {
			return ParsedRepo{}, fmt.Errorf("invalid repo URL")
		}
		return ParsedRepo{Scheme: "ssh", Host: host, Path: path, Name: leafName(path), Location: s, Source: "origin"}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return ParsedRepo{}, fmt.Errorf("invalid repo URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" && scheme != "ssh" && scheme != "git" {
		return ParsedRepo{}, fmt.Errorf("invalid repo URL")
	}
	if u.Host == "" || !allowHost(u.Host) {
		return ParsedRepo{}, fmt.Errorf("invalid repo URL")
	}
	return ParsedRepo{
		Scheme:   scheme,
		Host:     u.Host,
		Path:     u.Path,
		Name:     leafName(u.Path),
		Location: s,
		Source:   "origin",
	}, nil
}

func allowHost(host string) bool {
	h := strings.ToLower(host)
	if strings.Contains(h, "/") || strings.Contains(h, "\\") {
		return false
	}
	return h != ""
}

func leafName(p string) string {
	p = strings.TrimRight(p, "/")
	parts := strings.Split(p, "/")
	leaf := "repo"
	if len(parts) > 0 && parts[len(parts)-1] != "" {
		leaf = parts[len(parts)-1]
	}
	leaf = strings.TrimSuffix(leaf, ".git")
	if leaf == "" {
		return "repo"
	}
	return leaf
}

// ListRepoBranches runs git ls-remote or local branch listing. Never a shell.
func ListRepoBranches(source, location string) (map[string]any, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, fmt.Errorf("location is required")
	}
	src := strings.ToLower(strings.TrimSpace(source))
	if src == "github" {
		src = "origin"
	}
	if src != "local" && src != "origin" {
		src = "origin"
	}
	if src == "origin" {
		if _, err := ParseGitRepoURL(location); err != nil {
			return nil, err
		}
		cmd := exec.Command("git", "ls-remote", "--heads", location)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-remote failed")
		}
		return map[string]any{"branches": parseLsRemote(string(out)), "source": src, "location": location}, nil
	}
	// local checkout
	if !strings.HasPrefix(location, "/") {
		return nil, fmt.Errorf("local location must be an absolute path")
	}
	if _, err := os.Stat(location); err != nil {
		return nil, fmt.Errorf("local checkout not found")
	}
	cmd := exec.Command("git", "-C", location, "branch", "--format=%(refname:short)")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git branch failed")
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	if branches == nil {
		branches = []string{}
	}
	return map[string]any{"branches": branches, "source": src, "location": location}, nil
}

func parseLsRemote(out string) []string {
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ref := fields[len(fields)-1]
		const prefix = "refs/heads/"
		if strings.HasPrefix(ref, prefix) {
			branches = append(branches, strings.TrimPrefix(ref, prefix))
		}
	}
	if branches == nil {
		branches = []string{}
	}
	return branches
}

// ChooseLocalFolder is macOS osascript only.
func ChooseLocalFolder() (map[string]any, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("Finder folder picker is only available on macOS")
	}
	cmd := exec.Command("osascript", "-e", `POSIX path of (choose folder)`)
	out, err := cmd.Output()
	if err != nil {
		return map[string]any{"cancelled": true}, nil
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return map[string]any{"cancelled": true}, nil
	}
	return map[string]any{"path": p}, nil
}

// ResolveBaseRef prefers remote/branch, then local branch, then master (only if branch==main), then HEAD.
func ResolveBaseRef(primaryAbs, remote, branch string) (ref, br, from string) {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	// never origin/origin/main
	remote = strings.TrimPrefix(remote, "origin/")
	cand := remote + "/" + branch
	if gitOk(primaryAbs, "rev-parse", "--verify", cand) {
		return cand, branch, "remote"
	}
	if gitOk(primaryAbs, "rev-parse", "--verify", branch) {
		return branch, branch, "local"
	}
	if branch == "main" && gitOk(primaryAbs, "rev-parse", "--verify", "master") {
		return "master", "master", "master"
	}
	return "HEAD", branch, "HEAD"
}

func gitOk(dir string, args ...string) bool {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run() == nil
}

// FetchBaseRef is best-effort fetch of the remote-tracking ref only.
func FetchBaseRef(primaryAbs, remote, branch string) {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	_ = exec.Command("git", "-C", primaryAbs, "fetch", "--no-tags", remote, branch).Run()
}
