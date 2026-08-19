package jobs

import (
	"fmt"
	"os/exec"
	"strings"
	"unicode"

	"github.com/joe/defect-drainer-go/internal/git"
)

// Measure how much of a fix job's diff is reformatting rather than change.
//
// A 2026-08-17 fix changed 523 lines across 82 hunks, of which 34 hunks and
// 146 lines were pure `dart format` reflow of code the fix never needed to
// touch — the real change was buried in noise, and nothing surfaced it.
//
// Advisory only: reformatting is messy, not wrong, so this never blocks a
// resolve. It is recorded on the job so a reviewer sees it before reading the
// diff.

// RepoDiffHygiene is one worktree's hygiene numbers. baseSha is required so
// GET /api/batch-jobs/{id}/diff/{repo} can recompute hunks on demand.
type RepoDiffHygiene struct {
	Repo                string  `json:"repo"`
	BaseSha             string  `json:"baseSha"`
	FilesChanged        int     `json:"filesChanged"`
	ChangedLines        int     `json:"changedLines"`
	EffectiveLines      int     `json:"effectiveLines"`
	FormattingOnlyLines int     `json:"formattingOnlyLines"`
	Hunks               int     `json:"hunks"`
	EffectiveHunks      int     `json:"effectiveHunks"`
	FormattingOnlyHunks int     `json:"formattingOnlyHunks"`
	FormattingRatio     float64 `json:"formattingRatio"`
	Error               string  `json:"error,omitempty"`
}

// DiffHygieneReport is job.diffHygiene.
type DiffHygieneReport struct {
	Repos []RepoDiffHygiene `json:"repos"`
	Noisy bool              `json:"noisy"`
}

const (
	diffMinLines   = 40
	diffNoisyRatio = 0.25
)

func gitCAbs(abs string, args ...string) (string, error) {
	gitArgs := []string{
		"-C", abs,
		"-c", "diff.external=",
		"-c", "core.fsmonitor=",
		"--no-pager",
	}
	if len(args) > 0 && args[0] == "diff" {
		args = append([]string{"diff", "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	cmd := exec.Command("git", append(gitArgs, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// worktreeHead is the current commit of a worktree, for use as the later diff base.
func worktreeHead(worktreeAbs string) string {
	out, err := gitCAbs(worktreeAbs, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// DiffHunk is one parsed unified-0 hunk. A hunk is reflow iff added and
// removed text are identical once all whitespace is stripped. Not `git diff -w`:
// `-w` compares line by line, so a formatter joining three lines into one still
// reads as three deletions and one addition.
type DiffHunk struct {
	File    string   `json:"file"`
	Header  string   `json:"header"`
	Removed []string `json:"removed"`
	Added   []string `json:"added"`
	Reflow  bool     `json:"reflow"`
}

func stripAllWS(ls []string) string {
	s := strings.Join(ls, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// ParseDiffHunks parses a `git diff --unified=0` payload. Tests feed a fixture
// here; they must not call `git diff -w`.
func ParseDiffHunks(diff string) []DiffHunk {
	var out []DiffHunk
	file := ""
	var cur *DiffHunk
	flush := func() {
		if cur != nil && (len(cur.Added) > 0 || len(cur.Removed) > 0) {
			cur.Reflow = stripAllWS(cur.Added) == stripAllWS(cur.Removed)
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			flush()
			cur = &DiffHunk{File: file, Header: line, Removed: []string{}, Added: []string{}}
			continue
		}
		// Header lines, not content — `--- a/x` would otherwise read as a deletion.
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") {
			if strings.HasPrefix(line, "+++ b/") {
				file = strings.TrimPrefix(line, "+++ b/")
			}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(line, "+") {
			cur.Added = append(cur.Added, line[1:])
		} else if strings.HasPrefix(line, "-") {
			cur.Removed = append(cur.Removed, line[1:])
		}
	}
	flush()
	return out
}

type diffAnalysis struct {
	hunks       int
	reflowHunks int
	changed     int
	reflowLines int
	files       map[string]struct{}
}

func analyseDiff(diff string) diffAnalysis {
	parsed := ParseDiffHunks(diff)
	a := diffAnalysis{files: map[string]struct{}{}}
	for _, h := range parsed {
		if h.File != "" {
			a.files[h.File] = struct{}{}
		}
		n := len(h.Added) + len(h.Removed)
		a.hunks++
		a.changed += n
		if h.Reflow {
			a.reflowHunks++
			a.reflowLines += n
		}
	}
	return a
}

// CollectHunks returns the hunks behind the hygiene numbers, for the console
// drill-down. Computed on demand from the worktree — diffs are not stored on
// the job. kind is "reflow" or "all".
func isGitSHA(s string) bool {
	if n := len(s); n < 4 || n > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func CollectHunks(worktreeAbs, baseSha, kind string, limit int) (hunks []DiffHunk, total int, truncated bool, err error) {
	if limit <= 0 {
		limit = 200
	}
	if !isGitSHA(baseSha) {
		return nil, 0, false, fmt.Errorf("invalid diff baseline")
	}
	diff, err := gitCAbs(worktreeAbs, "diff", "--unified=0", baseSha, "--")
	if err != nil {
		return nil, 0, false, err
	}
	all := ParseDiffHunks(diff)
	var picked []DiffHunk
	if kind == "all" {
		picked = all
	} else {
		for _, h := range all {
			if h.Reflow {
				picked = append(picked, h)
			}
		}
	}
	total = len(picked)
	truncated = total > limit
	if total > limit {
		picked = picked[:limit]
	}
	if picked == nil {
		picked = []DiffHunk{}
	}
	return picked, total, truncated, nil
}

func blankRepoHygiene(repo, baseSha string) RepoDiffHygiene {
	return RepoDiffHygiene{Repo: repo, BaseSha: baseSha}
}

type hygieneLog func(line string, level string)

func measureDiffHygiene(worktrees []git.WorktreeBinding, baseByRepo map[string]string, onLog hygieneLog) DiffHygieneReport {
	if onLog == nil {
		onLog = func(string, string) {}
	}
	repos := make([]RepoDiffHygiene, 0, len(worktrees))
	for _, w := range worktrees {
		baseSha := baseByRepo[w.Repo]
		blank := blankRepoHygiene(w.Repo, baseSha)
		if baseSha == "" || !isGitSHA(baseSha) {
			blank.Error = "no base commit recorded for this worktree"
			if baseSha != "" {
				blank.Error = "invalid diff baseline"
			}
			repos = append(repos, blank)
			continue
		}
		diff, err := gitCAbs(w.WorktreeAbs, "diff", "--unified=0", baseSha, "--")
		if err != nil {
			blank.Error = err.Error()
			repos = append(repos, blank)
			onLog("diff hygiene: "+w.Repo+" — could not measure ("+err.Error()+")", "warn")
			continue
		}
		a := analyseDiff(diff)
		ratio := 0.0
		if a.changed != 0 {
			ratio = float64(a.reflowLines) / float64(a.changed)
		}
		entry := RepoDiffHygiene{
			Repo:                w.Repo,
			BaseSha:             baseSha,
			FilesChanged:        len(a.files),
			ChangedLines:        a.changed,
			EffectiveLines:      a.changed - a.reflowLines,
			FormattingOnlyLines: a.reflowLines,
			Hunks:               a.hunks,
			EffectiveHunks:      a.hunks - a.reflowHunks,
			FormattingOnlyHunks: a.reflowHunks,
			FormattingRatio:     ratio,
		}
		repos = append(repos, entry)
		if entry.ChangedLines >= diffMinLines && entry.FormattingRatio >= diffNoisyRatio {
			onLog(fmt.Sprintf(
				"diff hygiene: %s — %d/%d changed lines are reformatting only (%d/%d hunks). Reformatting untouched code buries the real change.",
				w.Repo, entry.FormattingOnlyLines, entry.ChangedLines, entry.FormattingOnlyHunks, entry.Hunks,
			), "warn")
		} else if entry.ChangedLines != 0 {
			onLog(fmt.Sprintf(
				"diff hygiene: %s — %d line(s) across %d file(s), %d reformatting only",
				w.Repo, entry.ChangedLines, entry.FilesChanged, entry.FormattingOnlyLines,
			), "info")
		}
	}
	noisy := false
	for _, r := range repos {
		if r.ChangedLines >= diffMinLines && r.FormattingRatio >= diffNoisyRatio {
			noisy = true
			break
		}
	}
	return DiffHygieneReport{Repos: repos, Noisy: noisy}
}
