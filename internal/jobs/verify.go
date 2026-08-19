package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// Re-run the app's verification commands after a fix job and gate resolution
// on the result.
//
// Why this exists: harvest used to resolve a defect because an image file was
// present, and the agent's own claims ("6 tests passed") were accepted as prose.
// Observed 2026-08-17 — a P1 was marked resolved off widget-test renders with
// the project's real gate never run.
//
// The commands are OPERATOR-authored (App Settings), never agent-authored: this
// runner is not sandboxed, so executing strings written by the coding agent
// would hand it unsandboxed execution on the host.

// VerifyResult is one operator-defined check DD re-ran. Field names match
// console BatchJobVerification / VerifyResult.
type VerifyResult struct {
	Repo          string  `json:"repo"`
	Command       string  `json:"command"`
	Cwd           string  `json:"cwd"`
	ExitCode      *int    `json:"exit_code"`
	Signal        *string `json:"signal"`
	Ok            bool    `json:"ok"`
	DurationMs    int64   `json:"duration_ms"`
	OutputTail    string  `json:"output_tail"`
	SkippedReason string  `json:"skipped_reason,omitempty"`
}

// VerificationRun is job.verification / job.baseline.
type VerificationRun struct {
	Ran        bool           `json:"ran"`
	Ok         bool           `json:"ok"`
	Results    []VerifyResult `json:"results"`
	StartedAt  string         `json:"startedAt"`
	FinishedAt string         `json:"finishedAt"`
}

const (
	verifyTimeout = 20 * time.Minute
	verifyTail    = 4000
	verifyMaxOut  = 32 * 1024 * 1024
)

// repoLeaf of a git URL or path: ".../ttd-webapp.git" -> "ttd-webapp".
func repoLeaf(url string) string {
	if url == "" {
		return ""
	}
	s := strings.TrimRight(url, "/")
	i := strings.LastIndex(s, "/")
	leaf := s
	if i >= 0 {
		leaf = s[i+1:]
	}
	if len(leaf) >= 4 && strings.EqualFold(leaf[len(leaf)-4:], ".git") {
		leaf = leaf[:len(leaf)-4]
	}
	return leaf
}

func addAlias(names map[string]struct{}, v string) {
	t := strings.ToLower(strings.TrimSpace(v))
	if t != "" {
		names[t] = struct{}{}
	}
}

// BindingAliases are names that legitimately refer to one worktree.
//
// A binding is named by the app's repo ENTRY name on the entries path
// ("webapp") but by the git URL LEAF on the repo_urls path ("ttd-webapp"), so a
// verify row configured with either must still match — otherwise the command is
// silently skipped, which counts as a failure and blocks every resolve.
//
// Cross-walk through the app's own entry->url mapping; never by substring.
func BindingAliases(b git.WorktreeBinding, entries []store.AppRepoEntry) map[string]struct{} {
	names := map[string]struct{}{}
	addAlias(names, b.Repo)
	addAlias(names, repoLeaf(b.RepoURL))
	addAlias(names, filepath.Base(b.WorktreeAbs))
	bindRepo := strings.ToLower(strings.TrimSpace(b.Repo))
	bindLeaf := strings.ToLower(repoLeaf(b.RepoURL))
	for _, e := range entries {
		leaf := repoLeaf(e.URL)
		leafLower := strings.ToLower(leaf)
		entryName := strings.ToLower(strings.TrimSpace(e.Name))
		if entryName == bindRepo || (leafLower != "" && leafLower == bindLeaf) || (leafLower != "" && leafLower == bindRepo) {
			addAlias(names, e.Name)
			addAlias(names, leaf)
		}
	}
	return names
}

func tailOutput(s string) string {
	t := strings.ReplaceAll(s, "\r\n", "\n")
	t = strings.TrimRight(t, " \t\n")
	if len(t) <= verifyTail {
		return t
	}
	return "…\n" + t[len(t)-verifyTail:]
}

type cappedBuffer struct {
	buf bytes.Buffer
	n   int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.n >= verifyMaxOut {
		return len(p), nil
	}
	left := verifyMaxOut - c.n
	if len(p) > left {
		_, _ = c.buf.Write(p[:left])
		c.n = verifyMaxOut
		return len(p), nil
	}
	n, err := c.buf.Write(p)
	c.n += n
	return len(p), err
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

func mergeVerifyEnv(extra map[string]string) []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, "CI=1")
	for k, v := range extra {
		if k == "" {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

type verifyLog func(line string, level string)

// runVerification runs every command in its worktree. Always returns a record
// — a command whose repo has no worktree is reported as skipped, not silently
// dropped.
func worktreeStillReal(abs, wantReal string) bool {
	if abs == "" || wantReal == "" {
		return false
	}
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	real, err := filepath.EvalSymlinks(abs)
	return err == nil && real == wantReal
}

func snapshotWorktreeReals(worktrees []git.WorktreeBinding) map[string]string {
	out := map[string]string{}
	for _, w := range worktrees {
		fi, err := os.Lstat(w.WorktreeAbs)
		if err != nil || !fi.Mode().IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		real, err := filepath.EvalSymlinks(w.WorktreeAbs)
		if err != nil || real == "" {
			continue
		}
		out[w.Repo] = real
	}
	return out
}

func runVerification(ctx context.Context, commands []store.VerifyCommand, worktrees []git.WorktreeBinding, handoffAbs string, repoEntries []store.AppRepoEntry, extraEnv map[string]string, realByRepo map[string]string, onLog verifyLog) VerificationRun {
	startedAt := db.NowIso()
	results := make([]VerifyResult, 0, len(commands))
	if onLog == nil {
		onLog = func(string, string) {}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	for _, entry := range commands {
		if ctx.Err() != nil {
			break
		}
		target := strings.ToLower(strings.TrimSpace(entry.Repo))
		var wt *git.WorktreeBinding
		for i := range worktrees {
			if _, ok := BindingAliases(worktrees[i], repoEntries)[target]; ok {
				wt = &worktrees[i]
				break
			}
		}
		wantReal := ""
		if wt != nil {
			wantReal = realByRepo[wt.Repo]
		}
		if wt == nil || !worktreeStillReal(wt.WorktreeAbs, wantReal) {
			reason := `no worktree for repo "` + entry.Repo + `" in this job (have: ` + worktreeRepoList(worktrees) + `)`
			cwd := ""
			if wt != nil {
				reason = "worktree path missing: " + wt.WorktreeAbs
				cwd = wt.WorktreeAbs
				if wantReal != "" && pathExists(wt.WorktreeAbs) {
					reason = "worktree is not the directory recorded before spawn: " + wt.WorktreeAbs
				}
			}
			onLog("verify: SKIP "+entry.Repo+" — "+reason, "warn")
			results = append(results, VerifyResult{
				Repo:          entry.Repo,
				Command:       entry.Command,
				Cwd:           cwd,
				Ok:            false,
				DurationMs:    0,
				OutputTail:    "",
				SkippedReason: reason,
			})
			continue
		}

		onLog("verify: "+entry.Repo+" $ "+entry.Command, "info")
		started := time.Now()
		cmdCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		cmd := exec.CommandContext(cmdCtx, "sh", "-c", entry.Command)
		cmd.Dir = wt.WorktreeAbs
		cmd.Env = mergeVerifyEnv(extraEnv)
		var out cappedBuffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		durationMs := time.Since(started).Milliseconds()

		var exitCode *int
		var signal *string
		if cmdCtx.Err() == context.DeadlineExceeded {
			signal = strPtr("SIGKILL")
		} else if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ProcessState != nil {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					signal = strPtr(unixSignalName(ws.Signal()))
				} else {
					exitCode = intPtr(ee.ExitCode())
				}
			} else {
				// Command could not be started; still a recorded failure.
				msg := err.Error()
				results = append(results, VerifyResult{
					Repo:       entry.Repo,
					Command:    entry.Command,
					Cwd:        wt.WorktreeAbs,
					Ok:         false,
					DurationMs: durationMs,
					OutputTail: tailOutput(out.buf.String() + "\n" + msg),
				})
				onLog(fmt.Sprintf("verify: FAIL %s (exit=killed, %.1fs)", entry.Repo, float64(durationMs)/1000), "warn")
				continue
			}
		} else {
			exitCode = intPtr(0)
		}
		ok := exitCode != nil && *exitCode == 0 && signal == nil
		results = append(results, VerifyResult{
			Repo:       entry.Repo,
			Command:    entry.Command,
			Cwd:        wt.WorktreeAbs,
			ExitCode:   exitCode,
			Signal:     signal,
			Ok:         ok,
			DurationMs: durationMs,
			OutputTail: tailOutput(out.buf.String()),
		})
		exitStr := "killed"
		if exitCode != nil {
			exitStr = fmt.Sprintf("%d", *exitCode)
		}
		sigBit := ""
		if signal != nil {
			sigBit = " signal=" + *signal
		}
		level := "info"
		word := "PASS"
		if !ok {
			level = "warn"
			word = "FAIL"
		}
		onLog(fmt.Sprintf("verify: %s %s (exit=%s%s, %.1fs)", word, entry.Repo, exitStr, sigBit, float64(durationMs)/1000), level)
	}

	run := VerificationRun{
		Ran:        len(results) > 0,
		Ok:         len(results) > 0,
		Results:    results,
		StartedAt:  startedAt,
		FinishedAt: db.NowIso(),
	}
	for _, r := range results {
		if !r.Ok {
			run.Ok = false
			break
		}
	}
	if len(results) == 0 {
		run.Ok = false
	}

	tryWriteVerifyJSON(handoffAbs, run)
	return run
}

func tryWriteVerifyJSON(handoffAbs string, run VerificationRun) {
	if err := os.MkdirAll(handoffAbs, 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(handoffAbs, "verify.json"), append(b, '\n'), 0o644)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func worktreeRepoList(worktrees []git.WorktreeBinding) string {
	if len(worktrees) == 0 {
		return "none"
	}
	names := make([]string, 0, len(worktrees))
	for _, w := range worktrees {
		names = append(names, w.Repo)
	}
	return strings.Join(names, ", ")
}

// How a post-fix result compares to the same command before the agent ran.
//
//   - pass         — green now
//   - regression   — was green at baseline, red now: this job broke it
//   - pre-existing — was red at baseline too: not this job's doing
//   - blocked      — could not run (bad repo name), or no baseline to compare to
//
// Only `regression` and `blocked` stop a defect resolving. Without this,
// long-standing red (e.g. `flutter analyze` exiting 1 on 5 old infos) would
// block every resolve and read as damage the agent did.
type VerifyVerdict string

const (
	VerdictPass        VerifyVerdict = "pass"
	VerdictRegression  VerifyVerdict = "regression"
	VerdictPreExisting VerifyVerdict = "pre-existing"
	VerdictBlocked     VerifyVerdict = "blocked"
)

func resultKey(r VerifyResult) string {
	return r.Repo + "\x00" + r.Command
}

func classifyResult(result VerifyResult, baseline *VerificationRun) VerifyVerdict {
	if result.Ok {
		return VerdictPass
	}
	// A command that could not run is a configuration fault, never excused by a
	// baseline that failed for the same reason.
	if result.SkippedReason != "" {
		return VerdictBlocked
	}
	if baseline == nil || !baseline.Ran {
		return VerdictBlocked
	}
	var before *VerifyResult
	key := resultKey(result)
	for i := range baseline.Results {
		if resultKey(baseline.Results[i]) == key {
			before = &baseline.Results[i]
			break
		}
	}
	if before == nil {
		return VerdictBlocked
	}
	if before.SkippedReason != "" {
		return VerdictBlocked
	}
	if before.Ok {
		return VerdictRegression
	}
	return VerdictPreExisting
}

type judgedRow struct {
	Result  VerifyResult
	Verdict VerifyVerdict
}

// VerificationVerdict is the harvest gate.
type VerificationVerdict struct {
	Ok          bool
	Verdicts    []judgedRow
	Regressions int
	PreExisting int
	Fixed       int
	Blocked     int
}

func judgeVerification(run, baseline *VerificationRun) VerificationVerdict {
	var rows []judgedRow
	if run != nil {
		for _, result := range run.Results {
			rows = append(rows, judgedRow{Result: result, Verdict: classifyResult(result, baseline)})
		}
	}
	regressions, blocked, preExisting := 0, 0, 0
	for _, v := range rows {
		switch v.Verdict {
		case VerdictRegression:
			regressions++
		case VerdictBlocked:
			blocked++
		case VerdictPreExisting:
			preExisting++
		}
	}
	fixed := 0
	if baseline != nil {
		for _, b := range baseline.Results {
			if b.Ok {
				continue
			}
			if run != nil {
				key := resultKey(b)
				for _, r := range run.Results {
					if resultKey(r) == key && r.Ok {
						fixed++
						break
					}
				}
			}
		}
	}
	return VerificationVerdict{
		Ok:          regressions == 0 && blocked == 0,
		Verdicts:    rows,
		Regressions: regressions,
		PreExisting: preExisting,
		Fixed:       fixed,
		Blocked:     blocked,
	}
}

func summarizeVerdict(v VerificationVerdict) string {
	if len(v.Verdicts) == 0 {
		return "no verification commands configured"
	}
	pass := 0
	for _, x := range v.Verdicts {
		if x.Verdict == VerdictPass {
			pass++
		}
	}
	bits := []string{fmt.Sprintf("%d/%d passed", pass, len(v.Verdicts))}
	if v.Regressions != 0 {
		bits = append(bits, fmt.Sprintf("%d regression(s)", v.Regressions))
	}
	blocked := 0
	for _, x := range v.Verdicts {
		if x.Verdict == VerdictBlocked {
			blocked++
		}
	}
	if blocked != 0 {
		bits = append(bits, fmt.Sprintf("%d could not run", blocked))
	}
	if v.PreExisting != 0 {
		bits = append(bits, fmt.Sprintf("%d already failing before this job", v.PreExisting))
	}
	if v.Fixed != 0 {
		bits = append(bits, fmt.Sprintf("%d newly fixed", v.Fixed))
	}
	return strings.Join(bits, ", ")
}

func summarizeVerification(run VerificationRun) string {
	if !run.Ran {
		return "no verification commands configured"
	}
	pass := 0
	var failed []string
	for _, r := range run.Results {
		if r.Ok {
			pass++
			continue
		}
		detail := r.SkippedReason
		if detail == "" {
			if r.ExitCode != nil {
				detail = fmt.Sprintf("exit %d", *r.ExitCode)
			} else {
				detail = "exit killed"
			}
		}
		failed = append(failed, r.Repo+": "+detail)
	}
	s := fmt.Sprintf("%d/%d verification command(s) passed", pass, len(run.Results))
	if len(failed) > 0 {
		s += " — " + strings.Join(failed, "; ")
	}
	return s
}

func alreadyFailingLines(run *VerificationRun) []string {
	if run == nil {
		return nil
	}
	var out []string
	for _, r := range run.Results {
		if !r.Ok {
			out = append(out, r.Repo+": "+r.Command)
		}
	}
	return out
}
