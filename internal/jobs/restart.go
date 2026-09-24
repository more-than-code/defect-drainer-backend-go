package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/paths"
)

// RestartCheck says whether an operator may Re-run or Resume a job and, when
// not, why. The console renders Reason as-is, so these rules live only here.
type RestartCheck struct {
	Ok     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// ErrJobLive rejects a restart while an agent for the job is running, starting,
// or still being reaped after Stop. Two agents on one handoff would share a
// cwd, a worktree and — through callVerifyCancel — each other's cancel.
var ErrJobLive = errors.New("an agent is still running for this job — stop it first")

// spawnMode picks what a spawn carries over from the job's earlier runs.
type spawnMode int

const (
	// spawnFresh is the first spawn: it measures baseline and diff base.
	spawnFresh spawnMode = iota
	// spawnRerun starts a new agent session on the existing worktrees.
	spawnRerun
	// spawnResume continues the job's last agent session.
	spawnResume
)

// restartFacts is what the rules need from outside the job record.
type restartFacts struct {
	live            bool
	missingWorktree string // first repo whose worktree is gone; "" when all present
	sessionFound    bool
}

func blockedBy(reason string) *RestartCheck { return &RestartCheck{Reason: reason} }

// rerunCheck gates a fresh agent session on the job's existing worktrees.
func rerunCheck(j *Job, f restartFacts) *RestartCheck {
	switch {
	case j.Mode != "grok":
		return blockedBy("not a coding-agent job")
	case f.live:
		return blockedBy(ErrJobLive.Error())
	case len(j.Worktrees) == 0:
		return blockedBy("job has no worktrees")
	case f.missingWorktree != "":
		return blockedBy("worktree missing: " + f.missingWorktree)
	}
	for _, p := range j.PRs {
		// A branch that already landed would get a second PR from Create PR.
		if p.GhState == "merged" || p.GhState == "closed" || p.MergedAt != "" {
			return blockedBy("PR merged/closed — file a new batch")
		}
	}
	return &RestartCheck{Ok: true}
}

// resumeCheck adds what continuing a session needs: a run that stopped short
// of its artifacts, and a session on disk to continue.
func resumeCheck(j *Job, f restartFacts) *RestartCheck {
	if c := rerunCheck(j, f); !c.Ok {
		return c
	}
	switch j.Status {
	case "failed", "cancelled":
		// Stop, backend restart, crash, and pre-loop turn caps all land here.
	case "completed":
		if j.AgentRun == nil || (j.AgentRun.StoppedBy != StoppedByIterations && j.AgentRun.StoppedBy != StoppedByWall) {
			return blockedBy("agent finished its run — use Re-run for another pass")
		}
	default:
		return blockedBy("job status " + j.Status + " cannot be resumed")
	}
	if !f.sessionFound {
		return blockedBy("no grok session found for this job")
	}
	return &RestartCheck{Ok: true}
}

// missingWorktree returns the first repo whose worktree directory is gone or
// is no longer a checkout. Lstat only: this runs on every job list.
func missingWorktree(wts []git.WorktreeBinding) string {
	for _, w := range wts {
		if !isRealDir(w.WorktreeAbs) {
			return w.Repo
		}
		if _, err := os.Lstat(filepath.Join(w.WorktreeAbs, ".git")); err != nil {
			return w.Repo
		}
	}
	return ""
}

func (b *BatchRunner) handoffDir(jobID string) string {
	return filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID)
}

// liveLocked requires b.mu. "running" spans CreateBatch or Restart through
// harvest; the maps cover a Stopped job whose child is still being reaped or
// whose driver goroutine is still in verify/harvest.
func (b *BatchRunner) liveLocked(j *Job) bool {
	if j.Status == "running" || j.Status == "queued" {
		return true
	}
	return b.starting[j.ID] != nil || b.looping[j.ID] != nil || b.procs[j.ID] != nil || b.driving[j.ID]
}

func (b *BatchRunner) isLive(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	j := b.jobs[id]
	return j != nil && b.liveLocked(j)
}

// fillRestartChecks sets the computed checks on a clone for an API read.
func (b *BatchRunner) fillRestartChecks(c *Job, live bool) {
	_, found := git.LatestGrokSession(b.handoffDir(c.ID))
	f := restartFacts{live: live, missingWorktree: missingWorktree(c.Worktrees), sessionFound: found}
	c.Rerunnable = rerunCheck(c, f)
	c.Resumable = resumeCheck(c, f)
}

// GetJobView is GetJob plus Resumable/Rerunnable and, for a queued job, the
// simulator-lease wait spot. For API reads only. The driver loop calls GetJob
// constantly and must not pay for the disk probes.
func (b *BatchRunner) GetJobView(id string) *Job {
	c := b.GetJob(id)
	if c != nil {
		b.fillRestartChecks(c, b.isLive(id))
		b.fillSimQueue(c)
	}
	return c
}

// ListView is List plus Resumable/Rerunnable and the simulator-lease wait
// spot, for API reads only.
func (b *BatchRunner) ListView() []*Job {
	out := b.List()
	for _, c := range out {
		b.fillRestartChecks(c, b.isLive(c.ID))
		b.fillSimQueue(c)
	}
	return out
}

// Restart re-runs (a new agent session) or resumes (the last session) a job
// that has stopped. The check and the flip to "running" happen under one lock,
// so of two concurrent requests exactly one starts an agent; the other gets
// ErrJobLive.
func (b *BatchRunner) Restart(jobID, defectsRoot string, resume bool) (*Job, error) {
	c := b.GetJob(jobID)
	if c == nil {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	// Disk probes stay outside the lock; only liveness must be read under it.
	session, found := git.LatestGrokSession(b.handoffDir(jobID))
	f := restartFacts{missingWorktree: missingWorktree(c.Worktrees), sessionFound: found}

	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("job not found: %s", jobID)
	}
	f.live = b.liveLocked(j)
	check := rerunCheck(j, f)
	if resume {
		check = resumeCheck(j, f)
	}
	if !check.Ok {
		b.mu.Unlock()
		if f.live {
			return nil, ErrJobLive
		}
		return nil, errors.New(check.Reason)
	}
	mode, line := spawnRerun, "re-run requested — new agent session on the existing worktrees"
	if resume {
		mode, line = spawnResume, "resume requested — continuing grok session "+session
	} else {
		session = ""
	}
	j.Status = "running"
	j.Error = ""
	j.RunStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	// These describe the previous run's code; this run replaces them.
	j.Verification = nil
	j.DiffHygiene = nil
	b.appendLogLocked(j, line, "info", "DefectDrainer")
	batchID, defectIDs := j.BatchID, append([]string(nil), j.DefectIDs...)
	b.mu.Unlock()
	b.persistLive(jobID)

	// SQL off the mutex (see CreatePRs). The failed/cancelled sync reopened
	// the defects; this run owns them again.
	_, _ = b.store.DB.Exec(`UPDATE batches SET status = ?, updated_at = ? WHERE id = ?`, "in_progress", db.NowIso(), batchID)
	b.FlipInProgress(defectIDs)

	if err := b.startSpawn(jobID, defectsRoot, mode, session); err != nil {
		b.MarkFailed(jobID, batchID, err.Error())
		b.SyncBatchAndDefects(b.GetJob(jobID), "failed")
		return nil, err
	}
	return b.GetJob(jobID), nil
}
