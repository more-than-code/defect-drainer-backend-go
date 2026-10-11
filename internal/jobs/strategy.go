package jobs

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// spawnStrategy is the per-kind piece of startSpawn. The lease, toolchain,
// sandbox profile, driver loop, caps and stop stay in startSpawn.
type spawnStrategy interface {
	writeHandoff(b *BatchRunner, j *Job, defectsRoot, handoff string) error
	prepare(handoff string)
	verifies() bool
	prompts(b *BatchRunner, j *Job, handoff, defectsRoot, sandbox string, wts []git.WorktreeBinding, base *git.CodingAgentOpts) (prompt, continuePrompt string)
	complete(b *BatchRunner, handoff string, j *Job, sandbox string, cutoff time.Time) (bool, []string)
	afterLoop(b *BatchRunner, fin loopFinish)
}

// loopFinish is what the driver loop hands the strategy once it has stopped
// for a reason other than Stop or Delete.
type loopFinish struct {
	jobID        string
	handoff      string
	failed       bool
	ctx          context.Context
	wts          []git.WorktreeBinding
	diffBase     map[string]string
	verifyCmds   []store.VerifyCommand
	repoEntries  []store.AppRepoEntry
	verifyEnv    map[string]string
	stripEnv     []string
	realByRepo   map[string]string
	gitDirByRepo map[string]string
	baseline     *VerificationRun
	log          func(line, level string)
}

func strategyFor(j *Job) spawnStrategy {
	if j.isTestRun() {
		return testRunStrategy{}
	}
	return batchFixStrategy{}
}

func (j *Job) isTestRun() bool {
	return j != nil && j.Kind == "test_run"
}

// jobLabel is the name log lines, the sandbox profile and the prompt use.
// A test job has no batch id; the run id stands in for it.
func jobLabel(j *Job) string {
	if j != nil && j.isTestRun() && j.TestRunID != "" {
		return j.TestRunID
	}
	if j == nil {
		return ""
	}
	return j.BatchID
}

type batchFixStrategy struct{}

func (batchFixStrategy) writeHandoff(b *BatchRunner, j *Job, defectsRoot, handoff string) error {
	return b.writeSpawnBrief(j, defectsRoot, handoff)
}

func (batchFixStrategy) prepare(handoff string) {
	_ = os.MkdirAll(filepath.Join(handoff, "fix-evidence"), 0o755)
	_ = os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755)
}

func (batchFixStrategy) verifies() bool { return true }

func (batchFixStrategy) prompts(*BatchRunner, *Job, string, string, string, []git.WorktreeBinding, *git.CodingAgentOpts) (string, string) {
	return "", ""
}

func (batchFixStrategy) complete(_ *BatchRunner, handoff string, j *Job, sandbox string, cutoff time.Time) (bool, []string) {
	var ids []string
	if j != nil {
		ids = j.DefectIDs
	}
	return artifactsComplete(handoff, ids, sandbox, cutoff)
}

func (batchFixStrategy) afterLoop(b *BatchRunner, fin loopFinish) {
	if b.GetJob(fin.jobID) == nil {
		return
	}
	var harvestOpts *HarvestOpts
	if !fin.failed {
		run := runVerification(fin.ctx, fin.verifyCmds, fin.wts, fin.handoff, fin.repoEntries, fin.verifyEnv, fin.realByRepo, fin.log, fin.stripEnv)
		hygiene := measureDiffHygiene(fin.wts, fin.diffBase, fin.log)
		verdict := judgeVerification(&run, fin.baseline)
		level := "info"
		if !verdict.Ok {
			level = "warn"
		}
		b.mu.Lock()
		if live := b.jobs[fin.jobID]; live != nil && !jobGoneOrCancelled(live) {
			cp := run
			live.Verification = &cp
			h := hygiene
			live.DiffHygiene = &h
			if fin.baseline != nil {
				live.Baseline = fin.baseline
			}
			b.appendLogLocked(live, "verify: "+summarizeVerdict(verdict), level, "DefectDrainer")
			b.appendLogLocked(live, "Grok process exited OK — reviewing, then harvesting fix-evidence…", "info", "DefectDrainer")
		}
		b.mu.Unlock()
		b.persistLive(fin.jobID)
		harvestOpts = &HarvestOpts{Verification: &run, AllowResolve: true}
	}
	if j := b.GetJob(fin.jobID); j == nil || jobGoneOrCancelled(j) {
		return
	}
	// Review sits between hygiene and harvest. A failed implementer records a
	// skip and does not spawn. Test-run jobs use a different strategy.
	rev, reviewStopped := b.runJobReview(fin)
	if reviewStopped {
		return
	}
	if harvestOpts != nil {
		harvestOpts.Review = rev
	}
	if j := b.GetJob(fin.jobID); j == nil || jobGoneOrCancelled(j) {
		return
	}
	b.harvestFixEvidence(b.GetJob(fin.jobID), fin.handoff, harvestOpts)
	live := b.GetJob(fin.jobID)
	if live == nil || jobGoneOrCancelled(live) {
		return
	}
	if fin.failed {
		// Status stays running. The driver publishes failed after this
		// returns, which is the last job.json write.
		b.SyncBatchAndDefects(live, "failed")
		return
	}
	b.SyncBatchAndDefects(live, "complete")
	b.finishTerminal(fin.jobID, "completed", "batch complete — review worktrees; merge when satisfied")
}
