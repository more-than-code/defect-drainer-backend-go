package jobs

import (
	"fmt"
	"sort"
	"time"

	"github.com/joe/defect-drainer-go/internal/git"
)

// SimQueue is where a queued job sits in the host simulator lease. Position is
// 1-based. Computed for GetJobView/ListView only — never written to job.json.
type SimQueue struct {
	Position int    `json:"position"`
	Holder   string `json:"holder"`
}

// simWaiter is one FIFO entry. Mode is what startSpawn was asked to do; the
// same value is persisted on the job as queuedMode so a restart can replay it.
type simWaiter struct {
	id   string
	mode spawnMode
}

func spawnModeName(mode spawnMode) string {
	switch mode {
	case spawnRerun:
		return "rerun"
	case spawnResume:
		return "resume"
	default:
		return "fresh"
	}
}

// queuedAtBefore orders RFC3339Nano chronologically. Go's format omits a
// zero fractional second, and '.' sorts before 'Z', so a later nanosecond
// stamp is less than an earlier exact-second stamp as text. Unparsable
// values sort last. Equal times return false so SliceStable keeps order.
func queuedAtBefore(a, b string) bool {
	ta, aOK := parseQueuedAt(a)
	tb, bOK := parseQueuedAt(b)
	if aOK && bOK {
		return ta.Before(tb)
	}
	return aOK && !bOK
}

func parseQueuedAt(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func parseSpawnMode(name string) spawnMode {
	switch name {
	case "rerun":
		return spawnRerun
	case "resume":
		return spawnResume
	default:
		return spawnFresh
	}
}

// rebuildSimQueue orders hydrated queued jobs by queuedAt as timestamps.
// The holder is empty: this process has no live agent. Equal or unparsable
// timestamps keep ReadDir order, with unparsable values last.
func (b *BatchRunner) rebuildSimQueue(queued []*Job) {
	sort.SliceStable(queued, func(i, k int) bool {
		return queuedAtBefore(queued[i].QueuedAt, queued[k].QueuedAt)
	})
	b.simHolder = ""
	b.simQueue = nil
	seen := map[string]bool{}
	for _, j := range queued {
		if j == nil || j.ID == "" || seen[j.ID] {
			continue
		}
		seen[j.ID] = true
		b.simQueue = append(b.simQueue, simWaiter{id: j.ID, mode: parseSpawnMode(j.QueuedMode)})
	}
}

// holdOrQueueSim gives jobID the simulator lease or parks it. proceed is false
// when the job was queued (or is already gone); startSpawn then returns nil.
// Caller must not hold b.mu. defectsRoot is remembered for a later handover —
// it is not stored in the agent-writable job.json.
func (b *BatchRunner) holdOrQueueSim(jobID string, mode spawnMode, defectsRoot string) (proceed bool) {
	b.mu.Lock()
	if defectsRoot != "" {
		b.defectsRoot = defectsRoot
	}
	j := b.jobs[jobID]
	if j == nil || jobGoneOrCancelled(j) {
		held := b.simHolder == jobID
		b.mu.Unlock()
		if held {
			b.releaseSimLease(jobID)
		}
		return false
	}
	if b.simHolder == jobID {
		b.mu.Unlock()
		return true
	}
	if b.simHolder == "" && len(b.simQueue) == 0 {
		b.simHolder = jobID
		b.mu.Unlock()
		return true
	}
	for _, w := range b.simQueue {
		if w.id == jobID {
			b.mu.Unlock()
			return false
		}
	}
	b.simQueue = append(b.simQueue, simWaiter{id: jobID, mode: mode})
	pos := len(b.simQueue)
	holder := b.simHolder
	j.Status = "queued"
	j.QueuedAt = time.Now().UTC().Format(time.RFC3339Nano)
	j.QueuedMode = spawnModeName(mode)
	b.appendLogLocked(j, fmt.Sprintf("waiting for the simulator (position %d, held by %s)", pos, holder), "info", "DefectDrainer")
	b.mu.Unlock()
	// Queueing is success for the caller even if the disk write fails; the
	// in-memory queue is what this process dispatches from. persistLive logs.
	_ = b.persistLive(jobID)
	return false
}

// dequeueSimLocked drops jobID from the waiter list. The holder is untouched;
// a running holder's lease ends with its agent loop. Requires b.mu.
func (b *BatchRunner) dequeueSimLocked(jobID string) {
	if len(b.simQueue) == 0 {
		return
	}
	kept := make([]simWaiter, 0, len(b.simQueue))
	for _, w := range b.simQueue {
		if w.id != jobID {
			kept = append(kept, w)
		}
	}
	b.simQueue = kept
}

// promoteSimLocked hands the lease to the first waiter that is still queued.
// Stopped and deleted entries are dropped. Requires b.mu. The holder must
// already have been confirmed by the caller.
func (b *BatchRunner) promoteSimLocked() (simWaiter, string, bool) {
	root := b.defectsRoot
	for len(b.simQueue) > 0 {
		w := b.simQueue[0]
		b.simQueue = b.simQueue[1:]
		j := b.jobs[w.id]
		if j == nil || j.Status != "queued" {
			continue
		}
		b.simHolder = w.id
		return w, root, true
	}
	b.simHolder = ""
	return simWaiter{}, root, false
}

// releaseSimLease drops jobID's hold and dispatches the next queued job on a
// new goroutine. No-op when jobID is not the holder. Never calls startSpawn
// while holding b.mu.
func (b *BatchRunner) releaseSimLease(jobID string) {
	b.mu.Lock()
	if b.simHolder != jobID {
		b.mu.Unlock()
		return
	}
	next, root, ok := b.promoteSimLocked()
	b.mu.Unlock()
	if ok {
		go b.dispatchSim(next, root)
	}
}

// dispatchSim replays the mode recorded when the job was queued. Resume looks
// up its grok session now, not at queue time; a missing session fails the job
// and passes the lease on.
func (b *BatchRunner) dispatchSim(w simWaiter, defectsRoot string) {
	session := ""
	if w.mode == spawnResume {
		if simLeaseBeforeResumeProbeHook != nil {
			simLeaseBeforeResumeProbeHook()
		}
		id, found := git.LatestGrokSession(b.handoffDir(w.id))
		if !found {
			b.failSimJob(w.id, "no grok session found for this job")
			b.releaseSimLease(w.id)
			return
		}
		session = id
	}
	if err := b.startSpawn(w.id, defectsRoot, w.mode, session); err != nil {
		// startSpawn already released the lease on the way out.
		b.failSimJob(w.id, err.Error())
	}
}

// failSimJob records a dispatch failure. Stop may have already set cancelled
// while the session lookup or cmd.Start was in flight; that outcome stays.
// A deleted job is left deleted. The caller still releases the lease.
func (b *BatchRunner) failSimJob(jobID, msg string) {
	b.mu.Lock()
	j := b.jobs[jobID]
	if j == nil || j.Status == "cancelled" {
		b.mu.Unlock()
		return
	}
	batchID := j.BatchID
	b.mu.Unlock()
	b.MarkFailed(jobID, batchID, msg)
	if cur := b.GetJob(jobID); cur != nil && cur.Status == "failed" {
		b.SyncBatchAndDefects(cur, "failed")
	}
}

// ResumeQueue starts the oldest hydrated queued job. Call it once the process
// can spawn (api.Build, after NewBatchRunner). defectsRoot is the defects
// directory every later handover passes to startSpawn.
func (b *BatchRunner) ResumeQueue(defectsRoot string) {
	b.mu.Lock()
	if defectsRoot != "" {
		b.defectsRoot = defectsRoot
	}
	if b.simHolder != "" {
		b.mu.Unlock()
		return
	}
	next, root, ok := b.promoteSimLocked()
	b.mu.Unlock()
	if ok {
		go b.dispatchSim(next, root)
	}
}

// fillSimQueue sets the computed wait spot on an API-read clone. A queued job
// that already holds the lease (dispatch has started, the agent has not) is
// not in the waiter list and gets no simQueue.
func (b *BatchRunner) fillSimQueue(c *Job) {
	if c == nil || c.Status != "queued" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	pos := 0
	for i, w := range b.simQueue {
		if w.id == c.ID {
			pos = i + 1
			break
		}
	}
	if pos == 0 {
		return
	}
	c.SimQueue = &SimQueue{Position: pos, Holder: b.simHolder}
}
