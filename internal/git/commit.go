package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ShipPreview is what the Merge dialog shows before anything is written.
type ShipPreview struct {
	Message string            `json:"message"`
	Repos   []ShipPreviewRepo `json:"repos"`
}

// ShipPreviewRepo is one worktree's uncommitted paths and how far its branch is.
type ShipPreviewRepo struct {
	Repo         string   `json:"repo"`
	Branch       string   `json:"branch"`
	BaseBranch   string   `json:"baseBranch,omitempty"`
	BaseSource   string   `json:"baseSource,omitempty"`
	Files        []string `json:"files"`
	CommitsAhead int      `json:"commitsAhead"`
}

// ShipRequest is the operator's confirm. Commit is set only after they accept
// the file list. GitHub bases are never committed by this flag.
type ShipRequest struct {
	Commit  bool
	Message string
}

// PreviewShip lists uncommitted, non-ignored files in each local worktree.
// message is the suggested commit text; the file list is returned separately.
func PreviewShip(bindings []WorktreeBinding, message string) (ShipPreview, error) {
	out := ShipPreview{Message: strings.TrimSpace(message), Repos: []ShipPreviewRepo{}}
	for _, b := range bindings {
		source, ref, branch := bindingBase(b)
		repo := ShipPreviewRepo{
			Repo: b.Repo, Branch: b.Branch, BaseBranch: branch, BaseSource: source, Files: []string{},
		}
		if source == "local" && IsGitRepo(b.WorktreeAbs) {
			files, err := dirtyFiles(b.WorktreeAbs)
			if err != nil {
				return ShipPreview{}, fmt.Errorf("%s: %w", b.Repo, err)
			}
			repo.Files = files
			repo.CommitsAhead = countCommits(b.WorktreeAbs, ref)
			if repo.CommitsAhead == 0 {
				repo.CommitsAhead = countCommits(b.WorktreeAbs, branch)
			}
		}
		out.Repos = append(out.Repos, repo)
	}
	return out, nil
}

// commitLocalChanges commits the worktree's uncommitted files onto its job branch.
// No dirty files is success. Ignored paths are not included.
func commitLocalChanges(b WorktreeBinding, message string) error {
	files, err := dirtyFiles(b.WorktreeAbs)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return fmt.Errorf("empty commit message")
	}
	if len(message) > 2000 {
		return fmt.Errorf("commit message is too long")
	}
	head, err := gitCOut(b.WorktreeAbs, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("cannot read worktree HEAD")
	}
	if head != b.Branch {
		return fmt.Errorf("worktree is on %s, not %s", head, b.Branch)
	}
	args := append([]string{"add", "--"}, files...)
	if err := gitC(b.WorktreeAbs, args...); err != nil {
		return err
	}
	if err := gitC(b.WorktreeAbs, "commit", "-m", message); err != nil {
		return err
	}
	return nil
}

func dirtyFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain", "-z", "--untracked-files=all")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(string(out))
}

func parsePorcelainZ(out string) ([]string, error) {
	if out == "" {
		return nil, nil
	}
	parts := bytes.Split([]byte(out), []byte{0})
	var files []string
	for i := 0; i < len(parts); {
		rec := parts[i]
		i++
		if len(rec) == 0 {
			continue
		}
		if len(rec) < 4 {
			return nil, fmt.Errorf("unrecognized git status entry")
		}
		xy := rec[:2]
		path := string(rec[3:])
		if xy[0] == 'R' || xy[0] == 'C' {
			if i >= len(parts) || len(parts[i]) == 0 {
				return nil, fmt.Errorf("rename status missing new path")
			}
			path = string(parts[i])
			i++
		}
		if !safeWorktreePath(path) {
			return nil, fmt.Errorf("refusing to commit %q", path)
		}
		files = append(files, path)
	}
	return files, nil
}

func safeWorktreePath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") {
		return false
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return false
	}
	return !strings.Contains(p, "\x00")
}
