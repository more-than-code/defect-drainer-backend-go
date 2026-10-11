package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joe/defect-drainer-go/internal/env"
	"github.com/joe/defect-drainer-go/internal/git"
)

// Review is the independent pass between diff hygiene and harvest. Critical
// and high findings block resolve. The model does not get to set ok.

const (
	defaultReviewMaxTurns   = 8
	defaultReviewMaxWallSec = 900
	reviewMaxFindings       = 100
	reviewFieldMax          = 2000
	reviewLocationMax       = 200

	// reviewSocketError is the lens error when read-only refuses to start
	// because the runtime socket is a symlink. The phrases matched in stderr
	// are grok's own (probed 2026-09-26).
	reviewSocketError = "read-only sandbox refused to start (runtime socket is a symlink, e.g. /var/run/docker.sock)"

	reviewAgentPrompt = `Read REVIEW.md in this directory and follow it exactly.
You are an independent reviewer. You did not author this change.
Do not edit any file and do not implement fixes.
Your final answer is the JSON object REVIEW.md describes.`
)

// ReviewFinding is one row of a dd.review.v1 payload. suggested_fix is text
// for a human; nothing in this process executes it.
type ReviewFinding struct {
	Severity     string `json:"severity"`
	Lens         string `json:"lens"`
	DefectID     string `json:"defect_id,omitempty"`
	Location     string `json:"location,omitempty"`
	Finding      string `json:"finding"`
	Reasoning    string `json:"reasoning,omitempty"`
	SuggestedFix string `json:"suggested_fix,omitempty"`
}

// ReviewLens is one fresh reviewer process.
type ReviewLens struct {
	Lens      string          `json:"lens"`
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Turns     int             `json:"turns,omitempty"`
	CostUSD   float64         `json:"costUsd,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Findings  []ReviewFinding `json:"findings"`
}

// ReviewRun is job.review. Required is false when the pass was skipped;
// harvest gates resolve only when Required && !OK.
type ReviewRun struct {
	OK            bool         `json:"ok"`
	Required      bool         `json:"required"`
	SkippedReason string       `json:"skippedReason,omitempty"`
	Error         string       `json:"error,omitempty"`
	Lenses        []ReviewLens `json:"lenses,omitempty"`
	TotalCostUSD  float64      `json:"totalCostUsd,omitempty"`
}

var (
	reviewSeverities = map[string]bool{
		"critical": true, "high": true, "medium": true, "low": true,
	}
	// docs is a legal row lens but not a default spawn. A row whose lens is
	// not the lens this process was spawned for still fails validation.
	reviewKnownLenses = map[string]bool{
		"spec": true, "security": true, "code": true, "tests": true, "docs": true, "ui": true,
	}
)

func cloneReview(r *ReviewRun) *ReviewRun {
	if r == nil {
		return nil
	}
	c := *r
	if r.Lenses != nil {
		c.Lenses = make([]ReviewLens, len(r.Lenses))
		for i, l := range r.Lenses {
			c.Lenses[i] = l
			if l.Findings != nil {
				c.Lenses[i].Findings = append([]ReviewFinding(nil), l.Findings...)
			}
		}
	}
	return &c
}

// reviewLenses is the spawn list. ui is added only when a worktree holds a
// UI-looking file; see pathLooksLikeUI.
func reviewLenses(wts []git.WorktreeBinding) []string {
	lenses := []string{"spec", "security", "code", "tests"}
	if uiLensWanted(wts) {
		lenses = append(lenses, "ui")
	}
	return lenses
}

// pathLooksLikeUI reports whether rel, relative to a worktree, looks like a
// UI file. It matches when a directory segment is lib or src and some segment
// (directory or file) contains "widget" or "component", case-insensitive.
// A file sitting directly under lib/ with neither word in its name does not
// match. Symlinks are not followed by the walker.
func pathLooksLikeUI(rel string) bool {
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return false
	}
	inTree := false
	widgety := false
	for i, p := range parts {
		pl := strings.ToLower(p)
		if (pl == "lib" || pl == "src") && i < len(parts)-1 {
			inTree = true
		}
		if strings.Contains(pl, "widget") || strings.Contains(pl, "component") {
			widgety = true
		}
	}
	return inTree && widgety
}

func skipUIDir(name string) bool {
	switch name {
	case ".git", "node_modules", "build", ".dart_tool", "dist", ".next", "vendor", ".grok":
		return true
	default:
		return false
	}
}

func uiLensWanted(wts []git.WorktreeBinding) bool {
	for _, w := range wts {
		if w.WorktreeAbs == "" || samePath(w.WorktreeAbs, w.PrimaryAbs) {
			continue
		}
		found := false
		_ = filepath.WalkDir(w.WorktreeAbs, func(p string, d os.DirEntry, err error) error {
			if err != nil || found {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if p != w.WorktreeAbs && skipUIDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(w.WorktreeAbs, p)
			if rerr != nil {
				return nil
			}
			if pathLooksLikeUI(rel) {
				found = true
				return filepath.SkipDir
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ar, err1 := filepath.EvalSymlinks(a)
	br, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(ar) == filepath.Clean(br)
}

// worktreeHasDiff is true when the worktree is a checkout and it is not
// provably identical to base, including uncommitted and untracked files.
// A missing base or a git error counts as a diff so a review is not skipped
// on a command we could not run. A directory that is not a checkout is not
// a diff.
func worktreeHasDiff(wt, base, gitDir string) bool {
	// Fail closed. A missing checkout, a missing recorded git dir, or a git
	// error is not proof of "no diff".
	if wt == "" || gitDir == "" {
		return true
	}
	if _, err := os.Lstat(filepath.Join(wt, ".git")); err != nil {
		return true
	}
	if strings.TrimSpace(base) == "" {
		return true
	}
	// GIT_DIR is the pre-spawn directory, so a rewritten .git file is ignored
	// even if the still-matches check raced.
	out, err := reviewGit(wt, gitDir, "diff", "--name-only", base)
	if err != nil || strings.TrimSpace(out) != "" {
		return true
	}
	st, err := reviewGit(wt, gitDir, "status", "--porcelain")
	if err != nil || strings.TrimSpace(st) != "" {
		return true
	}
	return false
}

func (fin loopFinish) reviewInputs() (diff bool) {
	for _, w := range fin.wts {
		if samePath(w.WorktreeAbs, w.PrimaryAbs) {
			continue
		}
		// A swapped directory or a rewritten .git file is not a place to run
		// git just to decide whether there is a diff.
		if !worktreeStillReal(w.WorktreeAbs, fin.realByRepo[w.Repo]) || !gitDirStill(w.WorktreeAbs, fin.gitDirByRepo[w.Repo]) {
			return true
		}
		if worktreeHasDiff(w.WorktreeAbs, fin.diffBase[w.Repo], fin.gitDirByRepo[w.Repo]) {
			return true
		}
	}
	return false
}

func buildReviewMD(lens, brief string, wts []git.WorktreeBinding, diffBase map[string]string, defectIDs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Review (%s)\n\n", lens)
	b.WriteString("## Role\n\n")
	b.WriteString("You are an independent reviewer. You did not author this change.\n")
	b.WriteString("Do not implement fixes. Do not edit product code, tests, or BRIEF.md.\n")
	b.WriteString("Do not write a findings file. This process is read-only; the only result\n")
	b.WriteString("that counts is the JSON object in your final answer.\n\n")
	b.WriteString("Review by reading only: `git diff`, `git status`, `git log`, `git show`, and reading files.\n")
	b.WriteString("Do not create repositories, write files, run scripts, set git config, or reproduce an exploit;\n")
	b.WriteString("describe it in a finding instead. A command the harness refuses ends your review and blocks the fix.\n\n")
	fmt.Fprintf(&b, "## Lens\n\nThis session reviews **%s** only. ", lens)
	b.WriteString("Every finding's `lens` must be exactly that value. ")
	b.WriteString("Do not report findings that belong to another lens.\n\n")
	b.WriteString(lensBlurb(lens))
	b.WriteString("\n\n## Facts\n\n")
	fmt.Fprintf(&b, "Acceptance criteria are in `%s` (read-only).\n\n", brief)
	if len(defectIDs) == 0 {
		b.WriteString("Defect ids: (none)\n\n")
	} else {
		b.WriteString("Defect ids:\n")
		for _, id := range defectIDs {
			fmt.Fprintf(&b, "- %s\n", id)
		}
		b.WriteString("\n")
	}
	b.WriteString("Worktrees (diff base is the commit recorded at spawn; HEAD is not the whole fix — include uncommitted changes):\n")
	listed := 0
	for _, w := range wts {
		if w.WorktreeAbs == "" || samePath(w.WorktreeAbs, w.PrimaryAbs) {
			continue
		}
		listed++
		base := ""
		if diffBase != nil {
			base = diffBase[w.Repo]
		}
		fmt.Fprintf(&b, "- %s: %s\n", w.Repo, w.WorktreeAbs)
		if base != "" {
			fmt.Fprintf(&b, "  - `git -C %s diff %s`\n", shellQuote(w.WorktreeAbs), shellQuote(base))
		} else {
			fmt.Fprintf(&b, "  - `git -C %s diff`\n", shellQuote(w.WorktreeAbs))
		}
		fmt.Fprintf(&b, "  - `git -C %s status`\n", shellQuote(w.WorktreeAbs))
	}
	if listed == 0 {
		b.WriteString("- (none — review BRIEF.md and fix-notes only)\n")
	}
	b.WriteString("\n## Output\n\n")
	b.WriteString("Reply with one JSON object, schema `dd.review.v1`. ")
	b.WriteString("Do not set `ok`; it is ignored. Unknown fields are ignored.\n\n")
	b.WriteString("```json\n")
	fmt.Fprintf(&b, "{\"findings\":[{\"severity\":\"high\",\"lens\":%q,\"defect_id\":\"\",\"location\":\"\",\"finding\":\"\",\"reasoning\":\"\",\"suggested_fix\":\"\"}]}\n", lens)
	b.WriteString("```\n\n")
	b.WriteString("`severity` is `critical`, `high`, `medium`, or `low`. ")
	b.WriteString("`finding` is required. `defect_id` is empty or one of the ids above. ")
	b.WriteString("`location` is a short path (no backticks or line breaks). ")
	b.WriteString("`suggested_fix` is advice for a human, not a command.\n\n")
	b.WriteString("## Honesty\n\n")
	b.WriteString("Empty findings must be `\"findings\": []`. Do not invent locations.\n")
	return b.String()
}

func lensBlurb(lens string) string {
	switch lens {
	case "spec":
		return "Spec conformance: does the diff and the fix-notes meet every acceptance criterion in BRIEF.md?"
	case "security":
		return "Security: secrets, injection, authentication, authorization, and unsafe paths."
	case "code":
		return "Code quality: the change should be clear, handle errors, and stay inside the defect."
	case "tests":
		return "Tests: the bug should be reproduced and the fix covered, or the gap explained."
	case "ui":
		return "UI: widgets and components — portability, accessibility, and obvious layout bugs."
	default:
		return ""
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lastJSONObject returns the last complete top-level JSON object in s.
// Prose and objects that are not valid JSON are ignored.
func lastJSONObject(s string) (string, bool) {
	var last string
	depth := 0
	start := -1
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				cand := s[start : i+1]
				if json.Valid([]byte(cand)) {
					last = strings.Clone(cand)
				}
				start = -1
			}
		}
	}
	return last, last != ""
}

func parseLensText(lens, text string, tooLarge bool, defectIDs []string) ReviewLens {
	out := ReviewLens{Lens: lens, Findings: []ReviewFinding{}}
	if tooLarge || len(text) > harvestFileLimit {
		out.Error = lens + ": too large"
		return out
	}
	obj, ok := lastJSONObject(text)
	if !ok {
		out.Error = lens + ": missing"
		return out
	}
	if len(obj) > harvestFileLimit {
		out.Error = lens + ": too large"
		return out
	}
	ids := make(map[string]bool, len(defectIDs))
	for _, id := range defectIDs {
		ids[id] = true
	}
	findings, err := validateFindings(lens, obj, ids)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Findings = findings
	out.OK = true
	for _, f := range findings {
		if f.Severity == "critical" || f.Severity == "high" {
			out.OK = false
			break
		}
	}
	return out
}

func validateFindings(lens, obj string, ids map[string]bool) ([]ReviewFinding, error) {
	var envelope struct {
		Findings json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal([]byte(obj), &envelope); err != nil || len(envelope.Findings) == 0 || string(envelope.Findings) == "null" {
		return nil, fmt.Errorf("%s: findings", lens)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(envelope.Findings, &rows); err != nil {
		return nil, fmt.Errorf("%s: findings", lens)
	}
	if len(rows) > reviewMaxFindings {
		return nil, fmt.Errorf("%s: too many findings", lens)
	}
	out := make([]ReviewFinding, 0, len(rows))
	for i, raw := range rows {
		f, err := validateFinding(lens, i, raw, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func validateFinding(lens string, i int, raw json.RawMessage, ids map[string]bool) (ReviewFinding, error) {
	rule := func(name string) error {
		return fmt.Errorf("%s: findings[%d]: %s", lens, i, name)
	}
	if len(raw) == 0 || raw[0] != '{' {
		return ReviewFinding{}, rule("finding")
	}
	var f ReviewFinding
	if err := json.Unmarshal(raw, &f); err != nil {
		return ReviewFinding{}, rule("finding")
	}
	if !reviewSeverities[f.Severity] {
		return ReviewFinding{}, rule("severity")
	}
	if !reviewKnownLenses[f.Lens] || f.Lens != lens {
		return ReviewFinding{}, rule("lens")
	}
	if strings.TrimSpace(f.Finding) == "" || len(f.Finding) > reviewFieldMax {
		return ReviewFinding{}, rule("finding")
	}
	if len(f.Reasoning) > reviewFieldMax {
		return ReviewFinding{}, rule("reasoning")
	}
	if len(f.SuggestedFix) > reviewFieldMax {
		return ReviewFinding{}, rule("suggested_fix")
	}
	if len(f.DefectID) > reviewFieldMax || (f.DefectID != "" && !ids[f.DefectID]) {
		return ReviewFinding{}, rule("defect_id")
	}
	if len(f.Location) > reviewLocationMax || badLocation(f.Location) {
		return ReviewFinding{}, rule("location")
	}
	return f, nil
}

func badLocation(s string) bool {
	return strings.ContainsAny(s, "`\r\n") || strings.ContainsRune(s, 0)
}

func (rev *ReviewRun) finish() {
	rev.OK = len(rev.Lenses) > 0
	rev.TotalCostUSD = 0
	var errs []string
	for _, l := range rev.Lenses {
		rev.TotalCostUSD += l.CostUSD
		if !l.OK {
			rev.OK = false
		}
		switch {
		case l.Error == "deadline":
			errs = append(errs, l.Lens+": deadline")
		case l.Error != "":
			errs = append(errs, l.Error)
		}
	}
	rev.Error = truncateUTF8(strings.Join(errs, "; "), reviewFieldMax)
}

// reviewBlockReason is set when harvest must not resolve because a required
// review is not ok. The reason names the blocking finding count when there
// is one. Empty means the review gate does not apply.
func reviewBlockReason(rev *ReviewRun) string {
	if rev == nil || !rev.Required || rev.OK {
		return ""
	}
	n := 0
	for _, l := range rev.Lenses {
		for _, f := range l.Findings {
			if f.Severity == "critical" || f.Severity == "high" {
				n++
			}
		}
	}
	if n > 0 {
		return fmt.Sprintf("review blocked (%d critical/high)", n)
	}
	if rev.Error != "" {
		return "review blocked (" + rev.Error + ")"
	}
	return "review blocked"
}

func sandboxSocketError(text string) (string, bool) {
	if strings.Contains(text, "endpoint is a symlink") || strings.Contains(text, "Refusing to start") {
		return reviewSocketError, true
	}
	return "", false
}

// appendReviewOutput is the reviewer's stdout/stderr. Once Stop has
// published cancelled, a later rename of job.json races the client that
// deletes the job directory. Same rule as reviewLog.
func (b *BatchRunner) appendReviewOutput(id, line, level string) {
	b.mu.Lock()
	live := b.jobs[id]
	if live == nil || jobGoneOrCancelled(live) {
		b.mu.Unlock()
		return
	}
	b.appendLogLocked(live, line, level, "Grok")
	b.mu.Unlock()
	b.persistLive(id)
}

func (b *BatchRunner) reviewLog(id, line, level string) {
	b.mu.Lock()
	live := b.jobs[id]
	// Stop already published cancelled. A later persist races the client
	// that observed it and then deletes the job directory.
	if live == nil || jobGoneOrCancelled(live) {
		b.mu.Unlock()
		return
	}
	b.appendLogLocked(live, line, level, "DefectDrainer")
	b.mu.Unlock()
	b.persistLive(id)
}

func (b *BatchRunner) saveReview(jobID string, rev *ReviewRun) {
	if rev == nil || jobID == "" {
		return
	}
	b.mu.Lock()
	live := b.jobs[jobID]
	if live == nil || jobGoneOrCancelled(live) {
		b.mu.Unlock()
		return
	}
	live.Review = cloneReview(rev)
	b.mu.Unlock()
	_ = b.persistLive(jobID)
}

// runJobReview is the batch-fix pass. Test-run jobs never call it.
// stop is true when the job was cancelled or deleted and harvest must not run.
func (b *BatchRunner) runJobReview(fin loopFinish) (rev *ReviewRun, stop bool) {
	j := b.GetJob(fin.jobID)
	if j == nil || jobGoneOrCancelled(j) {
		return nil, true
	}
	if fin.failed {
		rev = &ReviewRun{SkippedReason: "implementer failed"}
		b.saveReview(fin.jobID, rev)
		return rev, false
	}
	// No diff: nothing in this job may resolve, notes and REVIEW=0 included.
	// A placeholder fix-notes file is not a change to the tree.
	if !fin.reviewInputs() {
		rev = &ReviewRun{
			Required:      true,
			SkippedReason: "no diff",
			Error:         "nothing to review",
		}
		b.reviewLog(fin.jobID, "review: skipped (no diff)", "info")
		b.saveReview(fin.jobID, rev)
		return rev, false
	}
	if env.EnvDrainer("REVIEW") == "0" {
		rev = &ReviewRun{SkippedReason: "REVIEW=0"}
		b.reviewLog(fin.jobID, "review: skipped (REVIEW=0)", "warn")
		b.saveReview(fin.jobID, rev)
		return rev, false
	}

	rev = &ReviewRun{Required: true}
	endReview := b.beginReview(fin.jobID)
	defer endReview()
	before, err := captureWorktreeManifests(fin.wts, fin.realByRepo, fin.gitDirByRepo)
	if err != nil {
		rev.Error = "manifest: " + err.Error()
		b.reviewLog(fin.jobID, "review: "+rev.Error, "error")
		b.saveReview(fin.jobID, rev)
		return rev, false
	}
	defer func() {
		// The server does not put the tree back. A difference means the
		// reviewer (or something else) wrote, and the fix must not resolve.
		if b.GetJob(fin.jobID) == nil {
			return
		}
		msg := manifestDiff(before, fin.wts, fin.realByRepo, fin.gitDirByRepo)
		if msg == "" {
			return
		}
		rev.OK = false
		if rev.Error != "" {
			rev.Error += "; " + msg
		} else {
			rev.Error = msg
		}
		b.reviewLog(fin.jobID, "review: "+msg, "error")
		b.saveReview(fin.jobID, rev)
	}()

	ids := append([]string(nil), j.DefectIDs...)
	brief := filepath.Join(fin.handoff, "BRIEF.md")
	maxTurns := parsePositiveInt(env.EnvDrainer("REVIEW_MAX_TURNS"), defaultReviewMaxTurns)
	wall := time.Duration(parsePositiveInt(env.EnvDrainer("REVIEW_MAX_WALL_SEC"), defaultReviewMaxWallSec)) * time.Second
	for _, lens := range reviewLenses(fin.wts) {
		if cur := b.GetJob(fin.jobID); cur == nil || jobGoneOrCancelled(cur) {
			return rev, true
		}
		ran, stopped := b.runReviewLens(fin, rev, lens, brief, ids, maxTurns, wall)
		if stopped {
			return rev, true
		}
		if ran {
			b.saveReview(fin.jobID, rev)
			if reviewLensFinishedHook != nil {
				reviewLensFinishedHook(lens)
			}
		}
	}
	rev.finish()
	line, level := reviewSummary(rev)
	b.reviewLog(fin.jobID, line, level)
	b.saveReview(fin.jobID, rev)
	return rev, false
}

func reviewSummary(rev *ReviewRun) (string, string) {
	if rev.OK {
		return "review: ok", "info"
	}
	crit, high, med := 0, 0, 0
	for _, l := range rev.Lenses {
		for _, f := range l.Findings {
			switch f.Severity {
			case "critical":
				crit++
			case "high":
				high++
			case "medium":
				med++
			}
		}
	}
	if crit+high > 0 {
		var parts []string
		if crit > 0 {
			parts = append(parts, fmt.Sprintf("%d critical", crit))
		}
		if high > 0 {
			parts = append(parts, fmt.Sprintf("%d high", high))
		}
		if med > 0 {
			parts = append(parts, fmt.Sprintf("%d medium", med))
		}
		return "review: " + strings.Join(parts, ", ") + " — blocking resolve", "warn"
	}
	if rev.Error != "" {
		return "review: blocking resolve (" + rev.Error + ")", "warn"
	}
	return "review: blocking resolve", "warn"
}

// runReviewLens spawns one lens. ran is false when the lens was not started
// because the job was already gone. stopped means do not start another lens.
func (b *BatchRunner) runReviewLens(fin loopFinish, rev *ReviewRun, lens, brief string, ids []string, maxTurns int, wall time.Duration) (ran, stopped bool) {
	if cur := b.GetJob(fin.jobID); cur == nil || jobGoneOrCancelled(cur) {
		return false, true
	}
	dir := filepath.Join(fin.handoff, "review", lens)
	row := ReviewLens{Lens: lens, Findings: []ReviewFinding{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		row.Error = lens + ": " + err.Error()
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	md := buildReviewMD(lens, brief, fin.wts, fin.diffBase, ids)
	if err := os.WriteFile(filepath.Join(dir, "REVIEW.md"), []byte(md), 0o644); err != nil {
		row.Error = lens + ": " + err.Error()
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	b.reviewLog(fin.jobID, "review: spawn "+lens, "info")

	var installed *spawnedProc
	res, err := git.StartReviewAgent(git.ReviewAgentOpts{
		Cwd:       dir,
		Prompt:    reviewAgentPrompt,
		MaxTurns:  maxTurns,
		TextLimit: harvestFileLimit,
		Wall:      wall,
		StripEnv:  fin.stripEnv,
		OnStdout: func(line string) {
			b.appendReviewOutput(fin.jobID, line, "info")
		},
		OnStderr: func(line string) {
			b.appendReviewOutput(fin.jobID, line, "warn")
		},
		Register: func(cmd *exec.Cmd) func() error {
			proc := &spawnedProc{cmd: cmd, group: true}
			b.mu.Lock()
			gone := b.jobs[fin.jobID] == nil || jobGoneOrCancelled(b.jobs[fin.jobID])
			if !gone {
				b.procs[fin.jobID] = proc
				installed = proc
			}
			b.mu.Unlock()
			if gone {
				git.KillReviewProcess(cmd)
				return proc.reap
			}
			if reviewLensStartedHook != nil {
				reviewLensStartedHook(lens)
			}
			return proc.reap
		},
	})
	if installed != nil {
		b.mu.Lock()
		if b.procs[fin.jobID] == installed {
			delete(b.procs, fin.jobID)
		}
		b.mu.Unlock()
	}
	if cur := b.GetJob(fin.jobID); cur == nil || jobGoneOrCancelled(cur) {
		return false, true
	}
	if err != nil {
		if msg, ok := sandboxSocketError(err.Error()); ok {
			row.Error = msg
			b.reviewLog(fin.jobID, "review: "+lens+" "+msg, "error")
		} else {
			row.Error = lens + ": " + err.Error()
			b.reviewLog(fin.jobID, "review: "+row.Error, "error")
		}
		rev.Lenses = append(rev.Lenses, row)
		return true, false
	}
	row.Turns = res.End.NumTurns
	row.CostUSD = res.End.TotalCostUSD
	row.SessionID = res.End.SessionID
	// A wall kill is deadline even if stderr also mentions the runtime socket.
	// The socket phrase is the cause only when the process exited on its own.
	if res.Deadline {
		row.Error = "deadline"
		b.reviewLog(fin.jobID, "review: "+lens+" deadline", "error")
	} else if msg, ok := sandboxSocketError(res.Stderr); ok {
		row.Error = msg
		b.reviewLog(fin.jobID, "review: "+lens+" "+msg, "error")
	} else if tool, denied := toolDenied(res.ToolEvents); denied {
		// The permission gate cancelled the command and ended the run.
		// That is not a missing payload: name the tool and keep the fix blocked.
		row.OK = false
		row.Error = "tool-denied: " + tool
		b.reviewLog(fin.jobID, "review: "+lens+": "+row.Error, "error")
	} else {
		parsed := parseLensText(lens, res.Text, res.TooLarge, ids)
		row.OK = parsed.OK
		row.Error = parsed.Error
		row.Findings = parsed.Findings
		if row.OK && !readEvidence(res.ToolEvents, fin.wts) {
			// Valid JSON is not proof the model saw the diff.
			row.OK = false
			row.Error = "no-read-evidence"
		}
		if row.Error != "" {
			msg := row.Error
			if !strings.HasPrefix(msg, lens) {
				msg = lens + ": " + msg
			}
			b.reviewLog(fin.jobID, "review: "+msg, "warn")
		}
	}
	rev.Lenses = append(rev.Lenses, row)
	return true, false
}

// manifestPathCap and manifestByteCap stop a huge or generated tree from
// turning the read-only check into an unbounded hash. The byte cap is a var
// so a test can lower it; production leaves it at 2 GiB.
const manifestPathCap = 200_000

var manifestByteCap int64 = 2 << 30

// errManifestByteCap is remapped to the capture error. Callers must not have
// read past the remaining budget when they return it.
var errManifestByteCap = errors.New("manifest byte cap")

// manifestOpen reads a regular file for the manifest hash. Tests substitute
// a counting reader.
var manifestOpen = func(name string) (io.ReadCloser, error) {
	return os.Open(name)
}

// reviewBeforeSnapshotHook runs at the start of the manifest capture. Tests
// check that implementer descendants are already gone.
var reviewBeforeSnapshotHook func()

type manifestFile struct {
	missing bool
	mode    os.FileMode
	size    int64
	hash    [32]byte
	hashed  bool
	link    string
}

type wtManifest struct {
	repo     string
	head     string
	symRef   string
	detached bool
	files    map[string]manifestFile
}

func captureWorktreeManifests(wts []git.WorktreeBinding, realByRepo, gitDirByRepo map[string]string) ([]wtManifest, error) {
	if reviewBeforeSnapshotHook != nil {
		reviewBeforeSnapshotHook()
	}
	var out []wtManifest
	for _, w := range wts {
		// A primary checkout is refused before any read. Every other binding
		// has to produce a manifest: skipping an empty path or a missing .git
		// would drop that repo from both sides and look like "no change".
		if samePath(w.WorktreeAbs, w.PrimaryAbs) {
			return nil, fmt.Errorf("refusing to read primary checkout %s", w.Repo)
		}
		if w.WorktreeAbs == "" {
			return nil, fmt.Errorf("worktree %s has no path", w.Repo)
		}
		gitFile := filepath.Join(w.WorktreeAbs, ".git")
		fi, err := os.Lstat(gitFile)
		if err != nil {
			return nil, fmt.Errorf("worktree %s .git: %w", w.Repo, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("worktree %s .git is a symlink", w.Repo)
		}
		if !worktreeStillReal(w.WorktreeAbs, realByRepo[w.Repo]) {
			return nil, fmt.Errorf("worktree %s is not the directory recorded before spawn", w.Repo)
		}
		if !gitDirStill(w.WorktreeAbs, gitDirByRepo[w.Repo]) {
			return nil, fmt.Errorf("worktree %s git dir does not match the directory recorded before spawn", w.Repo)
		}
		m, err := captureOne(w.Repo, w.WorktreeAbs, gitDirByRepo[w.Repo])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func captureOne(repo, wt, gitDir string) (wtManifest, error) {
	m := wtManifest{repo: repo, files: map[string]manifestFile{}}
	head, err := reviewGit(wt, gitDir, "rev-parse", "HEAD")
	if err != nil || head == "" {
		if err == nil {
			err = fmt.Errorf("empty HEAD")
		}
		return m, fmt.Errorf("%s: %w", repo, err)
	}
	m.head = head
	ref, refErr := reviewGit(wt, gitDir, "symbolic-ref", "-q", "HEAD")
	if refErr != nil {
		if !gitExit(refErr, 1) {
			return m, fmt.Errorf("%s: %w", repo, refErr)
		}
		m.detached = true
	} else {
		m.symRef = ref
	}
	listing, err := reviewGit(wt, gitDir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return m, fmt.Errorf("%s: %w", repo, err)
	}
	var paths []string
	for _, p := range bytes.Split([]byte(listing), []byte{0}) {
		if len(p) == 0 {
			continue
		}
		paths = append(paths, string(p))
	}
	if len(paths) > manifestPathCap {
		return m, fmt.Errorf("%s: listing has %d paths, over %d", repo, len(paths), manifestPathCap)
	}
	var total int64
	for _, p := range paths {
		rel, err := manifestRel(wt, p)
		if err != nil {
			return m, fmt.Errorf("%s: %w", repo, err)
		}
		budget := manifestByteCap - total
		ent, n, err := hashManifestPath(wt, rel, budget)
		if errors.Is(err, errManifestByteCap) {
			return m, fmt.Errorf("%s: regular-file bytes exceed %d", repo, manifestByteCap)
		}
		if err != nil {
			return m, fmt.Errorf("%s: %s: %w", repo, rel, err)
		}
		total += n
		m.files[rel] = ent
	}
	return m, nil
}

func manifestRel(wt, p string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(p))
	if clean == "." || clean == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes worktree: %s", p)
	}
	abs := filepath.Join(wt, clean)
	rel, err := filepath.Rel(wt, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes worktree: %s", p)
	}
	return filepath.ToSlash(rel), nil
}

func hashManifestPath(wt, rel string, budget int64) (manifestFile, int64, error) {
	abs := filepath.Join(wt, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return manifestFile{missing: true}, 0, nil
		}
		return manifestFile{}, 0, err
	}
	ent := manifestFile{mode: fi.Mode(), size: fi.Size()}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return manifestFile{}, 0, err
		}
		ent.link = target
		return ent, 0, nil
	case fi.Mode().IsRegular():
		// Size is known before the open. A file already over the remaining
		// budget must not be read: DeleteJob waits on this capture.
		if fi.Size() > budget {
			return manifestFile{}, 0, errManifestByteCap
		}
		f, err := manifestOpen(abs)
		if err != nil {
			return manifestFile{}, 0, err
		}
		defer f.Close()
		h := sha256.New()
		// budget+1 so a file that grows during the read is detected and the
		// rest is not consumed.
		n, err := io.Copy(h, io.LimitReader(f, budget+1))
		if err != nil {
			return manifestFile{}, 0, err
		}
		if n > budget {
			return manifestFile{}, 0, errManifestByteCap
		}
		ent.hashed = true
		ent.size = n
		copy(ent.hash[:], h.Sum(nil))
		return ent, n, nil
	default:
		return ent, 0, nil
	}
}

func manifestDiff(before []wtManifest, wts []git.WorktreeBinding, realByRepo, gitDirByRepo map[string]string) string {
	saved := reviewBeforeSnapshotHook
	reviewBeforeSnapshotHook = nil
	after, err := captureWorktreeManifests(wts, realByRepo, gitDirByRepo)
	reviewBeforeSnapshotHook = saved
	if err != nil {
		return "worktree changed during review: " + err.Error()
	}
	return diffManifestLists(before, after)
}

// diffManifestLists reports the first repo that is missing on one side or
// whose manifest differs. A repo that vanished from the second capture is a
// change even when capture itself returned no error.
func diffManifestLists(before, after []wtManifest) string {
	byRepo := map[string]wtManifest{}
	for _, m := range before {
		byRepo[m.repo] = m
	}
	for _, m := range after {
		prev, ok := byRepo[m.repo]
		if !ok {
			return fmt.Sprintf("worktree changed during review: %s: HEAD", m.repo)
		}
		if what := diffOneManifest(prev, m); what != "" {
			return fmt.Sprintf("worktree changed during review: %s: %s", m.repo, what)
		}
		delete(byRepo, m.repo)
	}
	for repo := range byRepo {
		return fmt.Sprintf("worktree changed during review: %s: HEAD", repo)
	}
	return ""
}

func diffOneManifest(a, b wtManifest) string {
	if a.head != b.head || a.detached != b.detached || a.symRef != b.symRef {
		return "HEAD"
	}
	keys := make([]string, 0, len(a.files)+len(b.files))
	seen := map[string]struct{}{}
	for k := range a.files {
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	for k := range b.files {
		if _, ok := seen[k]; ok {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a.files[k] != b.files[k] {
			return k
		}
	}
	return ""
}

func reviewGit(wt, gitDir string, args ...string) (string, error) {
	return execReviewGit(wt, gitDir, localFilterNames(wt, gitDir), args...)
}

func localFilterNames(wt, gitDir string) []string {
	out, err := execReviewGit(wt, gitDir, nil, "config", "--includes", "--get-regexp", "^filter\\.")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, _, _ := strings.Cut(line, " ")
		rest, ok := strings.CutPrefix(key, "filter.")
		if !ok {
			continue
		}
		dot := strings.LastIndex(rest, ".")
		if dot <= 0 {
			continue
		}
		name := rest[:dot]
		if !filterArgSafe(name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// filterArgSafe rejects names that cannot sit in a single -c argument.
// Dots are kept: filter.a.b.clean is driver a.b.
func filterArgSafe(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, " \t\r\n\x00=")
}

// execReviewGit is one server-side git command. hooksPath is /dev/null so a
// directory the implementer seeded under the handoff cannot run. Global and
// system config are blanked. filter.<id> from the pinned config, including
// include.path and config.worktree, is turned off so a worktree
// .gitattributes cannot select it. The id may contain dots (filter.a.b.clean
// is driver a.b) and is passed as one -c argument, not through a shell.
func execReviewGit(wt, gitDir string, filters []string, args ...string) (string, error) {
	gitArgs := []string{
		"-C", wt,
		"-c", "core.fsmonitor=",
		"-c", "core.autocrlf=false",
		"-c", "diff.external=",
		"-c", "core.hooksPath=/dev/null",
	}
	for _, name := range filters {
		gitArgs = append(gitArgs,
			"-c", "filter."+name+".clean=",
			"-c", "filter."+name+".smudge=",
			"-c", "filter."+name+".process=",
			"-c", "filter."+name+".required=false",
		)
	}
	gitArgs = append(gitArgs, "--no-pager")
	gitArgs = append(gitArgs, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Env = envWithoutGit()
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if gitDir != "" {
		cmd.Env = append(cmd.Env, "GIT_DIR="+gitDir, "GIT_WORK_TREE="+wt)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func envWithoutGit() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		key, _, ok := strings.Cut(e, "=")
		if ok && strings.HasPrefix(key, "GIT_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func gitExit(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// toolDenied reports a permission-gate refusal. Grok ends the run cancelled
// and puts "User cancelled the execution for tool `<name>`" in the
// tool_call_update content, which is an array, so the raw event is scanned.
func toolDenied(events [][]byte) (string, bool) {
	const phrase = "User cancelled the execution for tool"
	names := map[string]string{}
	for _, raw := range events {
		var ev struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if ev.Type == "tool_call" && ev.ToolCallID != "" && ev.ToolName != "" {
			names[ev.ToolCallID] = ev.ToolName
		}
		if ev.Type != "tool_call_update" || !bytes.Contains(raw, []byte(phrase)) {
			continue
		}
		name := ev.ToolName
		if name == "" {
			name = deniedToolName(raw)
		}
		if name == "" {
			name = names[ev.ToolCallID]
		}
		if name == "" {
			name = "unknown"
		}
		return name, true
	}
	return "", false
}

func deniedToolName(raw []byte) string {
	const mark = "User cancelled the execution for tool `"
	s := string(raw)
	i := strings.Index(s, mark)
	if i < 0 {
		return ""
	}
	rest := s[i+len(mark):]
	j := strings.IndexByte(rest, '`')
	if j <= 0 {
		return ""
	}
	return rest[:j]
}

// readEvidence reports whether the stream shows a completed git diff of one
// of this job's worktrees. read_file and grep do not count. Paths are matched
// both as recorded and after EvalSymlinks (/var and /private/var on macOS).
func readEvidence(events [][]byte, wts []git.WorktreeBinding) bool {
	want := map[string]struct{}{}
	for _, w := range wts {
		if w.WorktreeAbs == "" || samePath(w.WorktreeAbs, w.PrimaryAbs) {
			continue
		}
		want[w.WorktreeAbs] = struct{}{}
		if real, err := filepath.EvalSymlinks(w.WorktreeAbs); err == nil && real != "" {
			want[real] = struct{}{}
		}
	}
	if len(want) == 0 {
		return false
	}
	diffCall := map[string]bool{}
	completed := map[string]bool{}
	for _, raw := range events {
		var ev struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
			Status     string `json:"status"`
			RawInput   struct {
				Command string `json:"command"`
			} `json:"rawInput"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.ToolCallID == "" {
			continue
		}
		switch ev.Type {
		case "tool_call":
			if ev.ToolName != "run_terminal_command" {
				continue
			}
			cmd := ev.RawInput.Command
			if !strings.Contains(cmd, "git") || !strings.Contains(cmd, "diff") {
				continue
			}
			for p := range want {
				if strings.Contains(cmd, p) {
					diffCall[ev.ToolCallID] = true
					break
				}
			}
		case "tool_call_update":
			if ev.Status == "completed" {
				completed[ev.ToolCallID] = true
			}
		}
	}
	for id := range diffCall {
		if completed[id] {
			return true
		}
	}
	return false
}

// snapshotGitDirs records each worktree's git dir before the implementer
// runs. A later rewrite of the handoff's .git file must not retarget it.
func snapshotGitDirs(wts []git.WorktreeBinding) map[string]string {
	out := map[string]string{}
	for _, w := range wts {
		if dir, ok := trustedGitDir(w.WorktreeAbs); ok {
			out[w.Repo] = dir
		}
	}
	return out
}

func trustedGitDir(wt string) (string, bool) {
	if wt == "" {
		return "", false
	}
	reported, err := gitCAbs(wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", false
	}
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return "", false
	}
	if !filepath.IsAbs(reported) {
		abs, err := filepath.Abs(reported)
		if err != nil {
			return "", false
		}
		reported = abs
	}
	current, kind, ok := currentGitDir(wt)
	if !ok || !samePath(current, reported) {
		return "", false
	}
	if kind == "file" && !insideWorktreesAdmin(reported) {
		return "", false
	}
	return reported, true
}

func gitDirStill(wt, want string) bool {
	if wt == "" || want == "" {
		return false
	}
	current, _, ok := currentGitDir(wt)
	return ok && samePath(current, want)
}

// currentGitDir reads the worktree's .git entry without trusting a symlink.
// kind is "dir" for a plain repository and "file" for a linked worktree.
func currentGitDir(wt string) (dir, kind string, ok bool) {
	p := filepath.Join(wt, ".git")
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return "", "", false
	}
	if fi.IsDir() {
		return p, "dir", true
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(b))
	rest, found := strings.CutPrefix(line, "gitdir:")
	if !found {
		return "", "", false
	}
	target := strings.TrimSpace(rest)
	if target == "" {
		return "", "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(wt, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", "", false
	}
	return abs, "file", true
}

func insideWorktreesAdmin(p string) bool {
	return strings.Contains(filepath.ToSlash(filepath.Clean(p)), "/.git/worktrees/")
}
