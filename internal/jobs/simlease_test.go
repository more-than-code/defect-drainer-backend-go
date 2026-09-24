package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// simWorld is a BatchRunner whose seeded web app allows simulator writes and
// whose fake grok records its cwd, then waits on a handoff "block" file.
type simWorld struct {
	br      *BatchRunner
	st      *store.Store
	defects string
	data    string
	logPath string
}

func newSimWorld(t *testing.T) *simWorld {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	w := &simWorld{
		br: NewBatchRunner(st, data), st: st, defects: defects, data: data,
		logPath: filepath.Join(t.TempDir(), "started"),
	}
	enableSimulatorWrites(t, w.br, "workspace")
	w.installAgent(t)
	t.Cleanup(func() { w.unblockAll() })
	return w
}

func (w *simWorld) installAgent(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-grok")
	// Ignore SIGINT so Stop can land while the agent is still inside its loop.
	// argv.txt is written before the cwd echo, so a started job has its argv.
	script := `#!/bin/sh
trap '' INT
printf '%s\n' "$@" > argv.txt
echo "$PWD" >> '` + w.logPath + `'
if [ -f block ]; then
  while [ ! -f release ]; do sleep 0.05; done
fi
if [ -d wt ]; then
  touch wt/hold-verify
fi
exit 0
`
	writeExec(t, bin, script)
	t.Setenv("GROK_BUILD_BIN", bin)
}

func (w *simWorld) job(t *testing.T, id, status string) *Job {
	t.Helper()
	defID := "DEF-20260924-" + strings.TrimPrefix(id, "bjob_")
	writeInProgressDefect(t, w.st, defID)
	dir := w.br.handoffDir(id)
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID: id, Kind: "batch", Status: status, Mode: "grok",
		BatchID:   "BATCH-20260924-" + strings.TrimPrefix(id, "bjob_"),
		AppID:     store.SeededTutoredWebappAppID,
		DefectIDs: []string{defID},
		Worktrees: []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}},
	}
	w.br.mu.Lock()
	w.br.jobs[id] = job
	w.br.mu.Unlock()
	return job
}

func (w *simWorld) block(t *testing.T, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.br.handoffDir(id), "block"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *simWorld) unblock(t *testing.T, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.br.handoffDir(id), "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *simWorld) unblockAll() {
	root := filepath.Join(w.data, "batch-jobs")
	ents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range ents {
		_ = os.WriteFile(filepath.Join(root, e.Name(), "release"), nil, 0o644)
		_ = os.WriteFile(filepath.Join(root, e.Name(), "wt", "release-verify"), nil, 0o644)
	}
}

func (w *simWorld) setVerify(t *testing.T, command string) {
	t.Helper()
	raw, err := json.Marshal([]store.VerifyCommand{{Repo: "demo", Command: command}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.st.DB.Exec(`UPDATE apps SET verify_commands_json = ? WHERE id = ?`, string(raw), store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
}

func (w *simWorld) started(id string) bool {
	b, _ := os.ReadFile(w.logPath)
	// Echo writes the cwd and a newline. A bare contains would treat
	// bjob_nosess as started once bjob_nosesshold had run.
	return strings.Contains(string(b), w.br.handoffDir(id)+"\n")
}

func (w *simWorld) waitStarted(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if w.started(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(w.logPath)
	t.Fatalf("agent for %s did not start\nlog:\n%s", id, b)
}

// jobQuiescent is true when no driver goroutine is still using the handoff.
// Status alone flips to completed before that goroutine's last persist returns,
// and removing the directory out from under it fails the test cleanup.
func jobQuiescent(br *BatchRunner, id string) bool {
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.driving[id] || br.looping[id] != nil || br.procs[id] != nil || br.starting[id] != nil {
		return false
	}
	j := br.jobs[id]
	if j == nil {
		return true
	}
	return j.Status != "running" && j.Status != "queued"
}

func (w *simWorld) waitSettled(t *testing.T, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pending := false
		for _, id := range ids {
			if !jobQuiescent(w.br, id) {
				pending = true
			}
		}
		if !pending {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, id := range ids {
		if !jobQuiescent(w.br, id) {
			got := w.br.GetJob(id)
			status := ""
			if got != nil {
				status = got.Status
			}
			t.Errorf("job %s still active (status %s)", id, status)
		}
	}
	t.FailNow()
}

func (w *simWorld) holder() string {
	w.br.mu.Lock()
	defer w.br.mu.Unlock()
	return w.br.simHolder
}

func requireQueued(t *testing.T, w *simWorld, id string, pos int, holder, mode string) {
	t.Helper()
	got := w.br.GetJob(id)
	if got == nil || got.Status != "queued" || got.QueuedMode != mode {
		t.Fatalf("%s queued state: %+v", id, got)
	}
	ts, err := time.Parse(time.RFC3339Nano, got.QueuedAt)
	if err != nil || ts.Location() != time.UTC {
		t.Fatalf("%s queuedAt %q: %v", id, got.QueuedAt, err)
	}
	view := w.br.GetJobView(id)
	if view == nil || view.SimQueue == nil || view.SimQueue.Position != pos || view.SimQueue.Holder != holder {
		t.Fatalf("%s simQueue %+v", id, view)
	}
	want := fmt.Sprintf("waiting for the simulator (position %d, held by %s)", pos, holder)
	if !logHas(got, want) {
		t.Fatalf("%s log missing %q: %v", id, want, got.Log)
	}
	raw, err := os.ReadFile(filepath.Join(w.br.handoffDir(id), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "simQueue") {
		t.Fatalf("simQueue must not be persisted: %s", raw)
	}
	if !strings.Contains(string(raw), `"queuedMode": "`+mode+`"`) {
		t.Fatalf("job.json missing queuedMode %s: %s", mode, raw)
	}
}

func argvFlag(t *testing.T, handoff, flag string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(handoff, "argv.txt"))
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if line == flag {
			if i+1 < len(lines) {
				return lines[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func (w *simWorld) allowRestart(t *testing.T, job *Job) {
	t.Helper()
	job.Status = "failed"
	job.Error = "stopped by operator"
	job.Baseline = &VerificationRun{Ran: true, StartedAt: storedBaseline, Results: []VerifyResult{{Repo: "demo", Ok: true}}}
	job.AgentRun = &AgentRun{Iterations: 1, StoppedBy: StoppedByExit, Turns: 3}
	if _, err := w.st.DB.Exec(`INSERT INTO batches (id, app_id, title, goal, status, defect_ids_json, mode, created, updated_at)
      VALUES (?, ?, 'b', '', 'failed', '[]', 'grok', '2026-09-24', '2026-09-24')`, job.BatchID, job.AppID); err != nil {
		t.Fatal(err)
	}
}

// AC1: a second simulator job waits, and its agent does not run until the
// first job's loop ends.
func TestSimLeaseSecondJobWaitsThenRuns(t *testing.T) {
	w := newSimWorld(t)
	first := w.job(t, "bjob_first", "running")
	second := w.job(t, "bjob_second", "running")
	w.block(t, first.ID)
	w.block(t, second.ID)

	if err := w.br.StartSpawn(first.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, first.ID)
	if err := w.br.StartSpawn(second.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, second.ID, 1, first.ID, "fresh")
	if w.started(second.ID) {
		t.Fatal("queued job's agent ran while the lease was held")
	}

	w.unblock(t, first.ID)
	w.waitStarted(t, second.ID)
	if w.holder() != second.ID {
		t.Fatalf("holder after handover = %q, want %s", w.holder(), second.ID)
	}
	w.unblock(t, second.ID)
	w.waitSettled(t, first.ID, second.ID)
}

// AC2: the lease moves at loop end, so the next agent starts while the holder
// is still inside verify.
func TestSimLeaseReleaseAtLoopEndDuringVerify(t *testing.T) {
	w := newSimWorld(t)
	w.setVerify(t, `if [ -f hold-verify ]; then while [ ! -f release-verify ]; do sleep 0.05; done; fi`)
	first := w.job(t, "bjob_verifyhold", "running")
	second := w.job(t, "bjob_verifynext", "running")
	// Hold the agent until the second job is queued. The script touches
	// hold-verify only after that wait, so post-loop verify blocks and the
	// baseline (which runs first) does not.
	w.block(t, first.ID)

	if err := w.br.StartSpawn(first.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, first.ID)
	if err := w.br.StartSpawn(second.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, second.ID, 1, first.ID, "fresh")
	w.unblock(t, first.ID)

	w.waitStarted(t, second.ID)
	held := w.br.GetJob(first.ID)
	if held == nil || held.Status != "running" || held.Verification != nil {
		t.Fatalf("holder should still be in verify, got %+v", held)
	}
	if _, err := os.Stat(filepath.Join(w.br.handoffDir(first.ID), "wt", "release-verify")); !os.IsNotExist(err) {
		t.Fatalf("verify release file exists too early: %v", err)
	}

	for _, id := range []string{first.ID, second.ID} {
		if err := os.WriteFile(filepath.Join(w.br.handoffDir(id), "wt", "release-verify"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w.waitSettled(t, first.ID, second.ID)
}

// AC3: an app without simulator writes does not wait on the lease.
func TestSimLeaseNonSimulatorJobDoesNotQueue(t *testing.T) {
	w := newSimWorld(t)
	if _, err := w.st.DB.Exec(`UPDATE apps SET allow_simulator_writes = 0 WHERE id = ?`, store.SeededTutoredMobileAppID); err != nil {
		t.Fatal(err)
	}
	holder := w.job(t, "bjob_simhold", "running")
	w.block(t, holder.ID)
	plain := w.job(t, "bjob_plain", "running")
	plain.AppID = store.SeededTutoredMobileAppID

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if err := w.br.StartSpawn(plain.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, plain.ID)
	got := w.br.GetJob(plain.ID)
	if got == nil || got.Status == "queued" || got.QueuedAt != "" {
		t.Fatalf("non-simulator job waited: %+v", got)
	}
	if w.holder() != holder.ID {
		t.Fatalf("lease moved to %q", w.holder())
	}
	w.unblock(t, holder.ID)
	w.waitSettled(t, holder.ID, plain.ID)
}

// AC4: three waiters run in the order they queued.
func TestSimLeaseFIFO(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_fifohold", "running")
	w.block(t, holder.ID)
	ids := []string{"bjob_fifo1", "bjob_fifo2", "bjob_fifo3"}
	for _, id := range ids {
		w.job(t, id, "running")
		w.block(t, id)
	}
	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	for i, id := range ids {
		if err := w.br.StartSpawn(id, w.defects); err != nil {
			t.Fatal(err)
		}
		requireQueued(t, w, id, i+1, holder.ID, "fresh")
	}

	w.unblock(t, holder.ID)
	for i, id := range ids {
		w.waitStarted(t, id)
		for _, waiting := range ids[i+1:] {
			if w.started(waiting) {
				t.Fatalf("%s started before %s finished its turn", waiting, id)
			}
		}
		if w.holder() != id {
			t.Fatalf("holder = %q, want %s", w.holder(), id)
		}
		w.unblock(t, id)
	}
	w.waitSettled(t, append([]string{holder.ID}, ids...)...)
}

// AC5: Stop cancels a queued job without spawning it. Delete drops one.
func TestSimLeaseStopAndDeleteQueued(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_stophold", "running")
	w.block(t, holder.ID)
	stopped := w.job(t, "bjob_stopped", "running")
	deleted := w.job(t, "bjob_deleted", "running")
	survivor := w.job(t, "bjob_survivor", "running")
	w.block(t, survivor.ID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if err := w.br.StartSpawn(stopped.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	if err := w.br.StartSpawn(deleted.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, stopped.ID, 1, holder.ID, "fresh")
	requireQueued(t, w, deleted.ID, 2, holder.ID, "fresh")

	got, err := w.br.Stop(stopped.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "cancelled" {
		t.Fatalf("stop returned %+v", got)
	}
	def, err := w.st.Get(stopped.DefectIDs[0])
	if err != nil || def == nil || def.Status != "open" {
		t.Fatalf("defect after stop: %+v %v", def, err)
	}
	if w.started(stopped.ID) {
		t.Fatal("stopped queued job spawned")
	}

	if _, err := w.br.DeleteJob(deleted.ID); err != nil {
		t.Fatal(err)
	}
	if w.br.GetJob(deleted.ID) != nil {
		t.Fatal("deleted job still mapped")
	}

	if err := w.br.StartSpawn(survivor.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, survivor.ID, 1, holder.ID, "fresh")

	w.unblock(t, holder.ID)
	w.waitStarted(t, survivor.ID)
	if w.started(stopped.ID) || w.started(deleted.ID) {
		t.Fatal("dequeued job spawned after the holder finished")
	}
	w.unblock(t, survivor.ID)
	w.waitSettled(t, holder.ID, survivor.ID)
}

// AC6: a holder that fails before launch releases the lease and the next job runs.
func TestSimLeaseHolderFailBeforeLaunchReleases(t *testing.T) {
	w := newSimWorld(t)
	real := filepath.Join(t.TempDir(), "real-grok")
	writeExec(t, real, "#!/bin/sh\necho \"$PWD\" >> '"+w.logPath+"'\nexit 0\n")
	link := filepath.Join(t.TempDir(), "grok-link")
	if err := os.Symlink("/no/such/grok-bin", link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", link)

	gate := filepath.Join(t.TempDir(), "baseline-gate")
	w.setVerify(t, "while [ ! -f '"+gate+"' ]; do sleep 0.05; done")

	blockNext := filepath.Join(t.TempDir(), "block-next-spawn")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	startSpawnHook = func() {
		if _, err := os.Stat(blockNext); err != nil {
			return
		}
		once.Do(func() { close(entered) })
		<-release
	}
	t.Cleanup(func() {
		startSpawnHook = nil
		_ = os.WriteFile(gate, []byte("1"), 0o644)
		letGo()
	})

	holder := w.job(t, "bjob_failhold", "running")
	next := w.job(t, "bjob_failnext", "running")
	errc := make(chan error, 1)
	go func() { errc <- w.br.StartSpawn(holder.ID, w.defects) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		got := w.br.GetJob(holder.ID)
		if got != nil && logHas(got, "baseline") && w.holder() == holder.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder never reached baseline under the lease: %+v holder=%s", got, w.holder())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := w.br.StartSpawn(next.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, next.ID, 1, holder.ID, "fresh")

	if err := os.WriteFile(blockNext, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("holder started despite a missing binary")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("holder did not fail before launch")
	}
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("next job was not dispatched")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	letGo()
	w.waitStarted(t, next.ID)
	if w.started(holder.ID) {
		t.Fatal("holder's agent ran after a pre-launch failure")
	}
	w.waitSettled(t, next.ID)
}

// AC7: Re-run and Resume wait on the lease, then dispatch in that mode.
// Resume passes -r. A session removed after queueing fails the job and the
// lease moves on (D6).
func TestSimLeaseRerunAndResumeDispatch(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_modehold", "running")
	w.block(t, holder.ID)
	resume := w.job(t, "bjob_resume", "failed")
	rerun := w.job(t, "bjob_rerun", "failed")
	w.allowRestart(t, resume)
	w.allowRestart(t, rerun)
	writeJobSession(t, w.br, resume.ID, testSessionID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)

	resumed, err := w.br.Restart(resume.ID, w.defects, true)
	if err != nil {
		t.Fatal(err)
	}
	if resumed == nil || resumed.Status != "queued" {
		t.Fatalf("resume while leased: %+v", resumed)
	}
	requireQueued(t, w, resume.ID, 1, holder.ID, "resume")
	reran, err := w.br.Restart(rerun.ID, w.defects, false)
	if err != nil {
		t.Fatal(err)
	}
	if reran == nil || reran.Status != "queued" {
		t.Fatalf("rerun while leased: %+v", reran)
	}
	requireQueued(t, w, rerun.ID, 2, holder.ID, "rerun")

	w.unblock(t, holder.ID)
	w.waitStarted(t, resume.ID)
	if !w.started(rerun.ID) {
		// Resume has no block file, so its loop ends and the rerun follows.
		w.waitStarted(t, rerun.ID)
	}
	log, _ := os.ReadFile(w.logPath)
	resumeAt := strings.Index(string(log), w.br.handoffDir(resume.ID)+"\n")
	rerunAt := strings.Index(string(log), w.br.handoffDir(rerun.ID)+"\n")
	if resumeAt < 0 || rerunAt < 0 || resumeAt > rerunAt {
		t.Fatalf("rerun ran before resume:\n%s", log)
	}
	if got, ok := argvFlag(t, w.br.handoffDir(resume.ID), "-r"); !ok || got != testSessionID {
		t.Fatalf("resume -r = %q ok=%v", got, ok)
	}
	if _, ok := argvFlag(t, w.br.handoffDir(rerun.ID), "-r"); ok {
		t.Fatal("rerun passed -r")
	}
	got := w.br.GetJob(resume.ID)
	if got == nil || got.AgentRun == nil || got.AgentRun.Resumes != 1 {
		t.Fatalf("resume did not record a resumed run: %+v", got)
	}
	w.waitSettled(t, holder.ID, resume.ID, rerun.ID)
}

func TestSimLeaseResumeMissingSessionPassesLease(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_nosesshold", "running")
	w.block(t, holder.ID)
	resume := w.job(t, "bjob_nosess", "failed")
	w.allowRestart(t, resume)
	writeJobSession(t, w.br, resume.ID, testSessionID)
	next := w.job(t, "bjob_nosessnext", "running")
	w.block(t, next.ID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if _, err := w.br.Restart(resume.ID, w.defects, true); err != nil {
		t.Fatal(err)
	}
	if err := w.br.StartSpawn(next.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, next.ID, 2, holder.ID, "fresh")

	dir, err := git.GrokSessionsDir(w.br.handoffDir(resume.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	w.unblock(t, holder.ID)

	deadline := time.Now().Add(15 * time.Second)
	for {
		got := w.br.GetJob(resume.ID)
		if got != nil && got.Status == "failed" && got.Error == "no grok session found for this job" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume without a session: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.started(resume.ID) {
		t.Fatal("resume without a session spawned an agent")
	}
	w.waitStarted(t, next.ID)
	w.unblock(t, next.ID)
	w.waitSettled(t, holder.ID, next.ID)
}

// AC8: hydrate keeps queued jobs, in queuedAt order, and ResumeQueue starts
// the first. queuedMode fresh measures a baseline. A running sibling still
// fails as interrupted.
func TestSimLeaseHydrateResumeQueue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	if _, err := st.DB.Exec(`UPDATE apps SET allow_simulator_writes = 1, grok_sandbox = ? WHERE id = ?`, "workspace", store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	baseLog := filepath.Join(t.TempDir(), "baseline")
	verify, err := json.Marshal([]store.VerifyCommand{{Repo: "demo", Command: "echo ran >> '" + baseLog + "'"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE apps SET verify_commands_json = ? WHERE id = ?`, string(verify), store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "started")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\necho \"$PWD\" >> '"+logPath+"'\nif [ -f block ]; then while [ ! -f release ]; do sleep 0.05; done; fi\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)

	writeDiskJob := func(id, status, queuedAt, mode string) {
		t.Helper()
		dir := filepath.Join(data, "batch-jobs", id)
		wt := filepath.Join(dir, "wt")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "block"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		job := &Job{
			ID: id, Kind: "batch", Status: status, Mode: "grok",
			AppID: store.SeededTutoredWebappAppID, BatchID: "BATCH-20260924-" + id,
			QueuedAt: queuedAt, QueuedMode: mode,
			Worktrees: []git.WorktreeBinding{{
				Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir(), Branch: "fix/x",
			}},
		}
		if err := writeJob(dir, job); err != nil {
			t.Fatal(err)
		}
	}
	// Write the later job first. Its name also sorts first, so a queue built
	// from directory order would start the wrong job.
	writeDiskJob("bjob_alate", "queued", "2026-09-02T00:00:00.000000000Z", "rerun")
	writeDiskJob("bjob_zearly", "queued", "2026-09-01T00:00:00.000000000Z", "fresh")
	writeDiskJob("bjob_wasrunning", "running", "", "")

	br := NewBatchRunner(st, data)
	t.Cleanup(func() {
		for _, id := range []string{"bjob_alate", "bjob_zearly", "bjob_wasrunning"} {
			_ = os.WriteFile(filepath.Join(br.handoffDir(id), "release"), nil, 0o644)
		}
	})
	if got := br.GetJob("bjob_wasrunning"); got == nil || got.Status != "failed" || got.Error != "interrupted by backend restart" {
		t.Fatalf("running job after hydrate: %+v", got)
	}
	for _, id := range []string{"bjob_zearly", "bjob_alate"} {
		if got := br.GetJob(id); got == nil || got.Status != "queued" {
			t.Fatalf("%s after hydrate: %+v", id, got)
		}
	}
	early := br.GetJobView("bjob_zearly")
	late := br.GetJobView("bjob_alate")
	if early.SimQueue == nil || early.SimQueue.Position != 1 || early.SimQueue.Holder != "" {
		t.Fatalf("early simQueue %+v", early.SimQueue)
	}
	if late.SimQueue == nil || late.SimQueue.Position != 2 || late.SimQueue.Holder != "" {
		t.Fatalf("late simQueue %+v", late.SimQueue)
	}

	br.ResumeQueue(defects)
	if view := br.GetJobView("bjob_alate"); view.SimQueue == nil || view.SimQueue.Position != 1 || view.SimQueue.Holder != "bjob_zearly" {
		t.Fatalf("after ResumeQueue late simQueue %+v", view.SimQueue)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := br.GetJob("bjob_zearly")
		if got != nil && got.Baseline != nil && got.Baseline.Ran {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh queued job never measured a baseline: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, _ := os.ReadFile(baseLog); !strings.Contains(string(b), "ran") {
		t.Fatalf("baseline command did not run: %q", b)
	}
	if lateNow := br.GetJob("bjob_alate"); lateNow == nil || lateNow.Status != "queued" || lateNow.Baseline != nil {
		t.Fatalf("later job should still be waiting without a baseline: %+v", lateNow)
	}
	started, _ := os.ReadFile(logPath)
	if strings.Contains(string(started), br.handoffDir("bjob_alate")) {
		t.Fatal("later job's agent ran before the earlier one finished")
	}

	_ = os.WriteFile(filepath.Join(br.handoffDir("bjob_zearly"), "release"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(br.handoffDir("bjob_alate"), "release"), nil, 0o644)
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if jobQuiescent(br, "bjob_zearly") && jobQuiescent(br, "bjob_alate") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hydrated jobs did not settle: early %+v late %+v", br.GetJob("bjob_zearly"), br.GetJob("bjob_alate"))
}

// AC10: stopping the holder releases the lease when its loop ends, not inside Stop.
func TestSimLeaseStopHolderReleasesAtLoopEnd(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_killhold", "running")
	w.block(t, holder.ID)
	next := w.job(t, "bjob_killnext", "running")
	w.block(t, next.ID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if err := w.br.StartSpawn(next.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, next.ID, 1, holder.ID, "fresh")

	if _, err := w.br.Stop(holder.ID); err != nil {
		t.Fatal(err)
	}
	if w.holder() != holder.ID {
		t.Fatalf("Stop released the lease early, holder=%s", w.holder())
	}
	if got := w.br.GetJob(next.ID); got == nil || got.Status != "queued" || w.started(next.ID) {
		t.Fatalf("next job moved during Stop: %+v started=%v", got, w.started(next.ID))
	}

	w.unblock(t, holder.ID)
	w.waitStarted(t, next.ID)
	if w.holder() != next.ID {
		t.Fatalf("holder after the loop ended = %q", w.holder())
	}
	w.unblock(t, next.ID)
	w.waitSettled(t, holder.ID, next.ID)
}

// A handover can promote a waiter before the startSpawn that queued it reaches
// its defer. That defer must not drop the lease or cancel the dispatch's verify
// context. The third job stays queued while the promoted job holds.
func TestSimLeaseQueuedDeferKeepsPromotedLease(t *testing.T) {
	w := newSimWorld(t)
	w.setVerify(t, "echo baseline-ok")
	holder := w.job(t, "bjob_deferhold", "running")
	w.block(t, holder.ID)
	waiter := w.job(t, "bjob_deferwait", "running")
	w.block(t, waiter.ID)
	other := w.job(t, "bjob_deferother", "running")
	w.block(t, other.ID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)

	dispatchEntered := make(chan struct{})
	letDispatch := make(chan struct{})
	var signalOnce sync.Once
	var letOnce sync.Once
	letGo := func() { letOnce.Do(func() { close(letDispatch) }) }
	var arm atomic.Bool
	startSpawnHook = func() {
		if !arm.Load() {
			return
		}
		signalOnce.Do(func() { close(dispatchEntered) })
		<-letDispatch
	}
	// Not sync.Once: the hook queues another job, and Once is not reentrant.
	var queuedN atomic.Int32
	simLeaseQueuedHook = func() {
		if queuedN.Add(1) != 1 {
			return
		}
		if err := w.br.StartSpawn(other.ID, w.defects); err != nil {
			t.Errorf("queue other: %v", err)
		}
		arm.Store(true)
		w.br.releaseSimLease(holder.ID)
		select {
		case <-dispatchEntered:
		case <-time.After(15 * time.Second):
			t.Errorf("promoted job did not reach its dispatch spawn")
		}
	}
	t.Cleanup(func() {
		startSpawnHook = nil
		simLeaseQueuedHook = nil
		letGo()
	})

	errc := make(chan error, 1)
	go func() { errc <- w.br.StartSpawn(waiter.ID, w.defects) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("queuing startSpawn did not return")
	}
	letGo()

	deadline := time.Now().Add(15 * time.Second)
	for {
		got := w.br.GetJob(waiter.ID)
		if got != nil && got.Baseline != nil && got.Baseline.Ran && got.Baseline.Ok && w.started(waiter.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("promoted job did not run with a baseline: %+v started=%v", got, w.started(waiter.ID))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.holder() != waiter.ID {
		t.Fatalf("holder = %q, want %s", w.holder(), waiter.ID)
	}
	if w.started(other.ID) {
		t.Fatal("another queued job started while the promoted job held the lease")
	}
	w.unblock(t, holder.ID)
	w.unblock(t, waiter.ID)
	w.unblock(t, other.ID)
	w.waitSettled(t, holder.ID, waiter.ID, other.ID)
}

// A dispatched job is persisted as running before its child is started, so a
// crash in that window hydrates as interrupted rather than queued.
func TestSimLeasePersistRunningBeforeStart(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_diskhold", "running")
	w.block(t, holder.ID)
	next := w.job(t, "bjob_disknext", "running")
	w.block(t, next.ID)

	saw := make(chan struct{})
	var hits int
	startSpawnAfterStartHook = func() {
		hits++
		if hits != 2 {
			return
		}
		raw, err := os.ReadFile(filepath.Join(w.br.handoffDir(next.ID), "job.json"))
		if err != nil {
			t.Errorf("read job.json: %v", err)
		} else {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("job.json: %v", err)
			} else if m["status"] != "running" {
				t.Errorf("disk status = %v, want running before the child is reaped", m["status"])
			}
		}
		close(saw)
	}
	t.Cleanup(func() { startSpawnAfterStartHook = nil })

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if err := w.br.StartSpawn(next.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	requireQueued(t, w, next.ID, 1, holder.ID, "fresh")
	w.unblock(t, holder.ID)
	select {
	case <-saw:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatched job did not reach cmd.Start")
	}
	if t.Failed() {
		t.FailNow()
	}
	w.unblock(t, next.ID)
	w.waitSettled(t, holder.ID, next.ID)
}

// Stop during a promoted Resume's session lookup must stick. The missing
// session still passes the lease to the next job.
func TestSimLeaseStopDuringResumeProbeStaysCancelled(t *testing.T) {
	w := newSimWorld(t)
	holder := w.job(t, "bjob_probehold", "running")
	w.block(t, holder.ID)
	resume := w.job(t, "bjob_proberes", "failed")
	w.allowRestart(t, resume)
	writeJobSession(t, w.br, resume.ID, testSessionID)
	next := w.job(t, "bjob_probenext", "running")
	w.block(t, next.ID)

	if err := w.br.StartSpawn(holder.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	w.waitStarted(t, holder.ID)
	if _, err := w.br.Restart(resume.ID, w.defects, true); err != nil {
		t.Fatal(err)
	}
	if err := w.br.StartSpawn(next.ID, w.defects); err != nil {
		t.Fatal(err)
	}
	sess, err := git.GrokSessionsDir(w.br.handoffDir(resume.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(sess); err != nil {
		t.Fatal(err)
	}

	probing := make(chan struct{})
	release := make(chan struct{})
	var probeOnce sync.Once
	var releaseOnce sync.Once
	letProbe := func() { releaseOnce.Do(func() { close(release) }) }
	simLeaseBeforeResumeProbeHook = func() {
		probeOnce.Do(func() { close(probing) })
		<-release
	}
	t.Cleanup(func() {
		simLeaseBeforeResumeProbeHook = nil
		letProbe()
	})

	w.unblock(t, holder.ID)
	select {
	case <-probing:
	case <-time.After(15 * time.Second):
		t.Fatal("resume probe did not start")
	}
	stopped, err := w.br.Stop(resume.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped == nil || stopped.Status != "cancelled" {
		t.Fatalf("stop returned %+v", stopped)
	}
	letProbe()

	w.waitStarted(t, next.ID)
	got := w.br.GetJob(resume.ID)
	if got == nil || got.Status != "cancelled" || got.Error != "stopped by operator" {
		t.Fatalf("resume after stop during probe: %+v", got)
	}
	if w.started(resume.ID) {
		t.Fatal("stopped resume spawned an agent")
	}
	w.unblock(t, next.ID)
	w.waitSettled(t, holder.ID, next.ID)
}

// A failed app read must fail the start. It must not look like "flag clear"
// and spawn beside whoever holds the simulator.
func TestSimLeaseAppReadErrorDoesNotSpawn(t *testing.T) {
	w := newSimWorld(t)
	job := w.job(t, "bjob_appfail", "running")
	w.block(t, job.ID)
	if err := w.st.DB.Close(); err != nil {
		t.Fatal(err)
	}
	err := w.br.StartSpawn(job.ID, w.defects)
	if err == nil {
		t.Fatal("start succeeded despite an app read error")
	}
	if w.started(job.ID) {
		t.Fatal("agent ran after the app read failed")
	}
}

// queuedAt orders by time, not text. An exact-second stamp sorts before a
// later stamp in that same second that carries a fractional nanosecond.
func TestSimLeaseQueuedAtChronological(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	if _, err := st.DB.Exec(`UPDATE apps SET allow_simulator_writes = 1, grok_sandbox = ? WHERE id = ?`, "workspace", store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "started")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\necho \"$PWD\" >> '"+logPath+"'\nif [ -f block ]; then while [ ! -f release ]; do sleep 0.05; done; fi\nexit 0\n")
	t.Setenv("GROK_BUILD_BIN", bin)

	writeDisk := func(id, at string) {
		t.Helper()
		dir := filepath.Join(data, "batch-jobs", id)
		wt := filepath.Join(dir, "wt")
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "block"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		job := &Job{
			ID: id, Kind: "batch", Status: "queued", Mode: "grok",
			AppID: store.SeededTutoredWebappAppID, BatchID: "BATCH-20260924-" + id,
			QueuedAt: at, QueuedMode: "fresh",
			Worktrees: []git.WorktreeBinding{{
				Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir(), Branch: "fix/x",
			}},
		}
		if err := writeJob(dir, job); err != nil {
			t.Fatal(err)
		}
	}
	// The fractional stamp is later in time but sorts first as text ('.' < 'Z').
	writeDisk("bjob_nano", "2026-09-24T12:00:00.000000001Z")
	writeDisk("bjob_sec", "2026-09-24T12:00:00Z")

	br := NewBatchRunner(st, data)
	t.Cleanup(func() {
		for _, id := range []string{"bjob_sec", "bjob_nano"} {
			_ = os.WriteFile(filepath.Join(br.handoffDir(id), "release"), nil, 0o644)
		}
	})
	sec := br.GetJobView("bjob_sec")
	nano := br.GetJobView("bjob_nano")
	if sec.SimQueue == nil || sec.SimQueue.Position != 1 {
		t.Fatalf("exact-second job position %+v, want 1", sec.SimQueue)
	}
	if nano.SimQueue == nil || nano.SimQueue.Position != 2 {
		t.Fatalf("nanosecond job position %+v, want 2", nano.SimQueue)
	}
	br.ResumeQueue(defects)
	br.mu.Lock()
	holder := br.simHolder
	br.mu.Unlock()
	if holder != "bjob_sec" {
		t.Fatalf("dispatched %q, want the exact-second job", holder)
	}
	_ = os.WriteFile(filepath.Join(br.handoffDir("bjob_sec"), "release"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(br.handoffDir("bjob_nano"), "release"), nil, 0o644)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if jobQuiescent(br, "bjob_sec") && jobQuiescent(br, "bjob_nano") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("jobs did not settle: sec %+v nano %+v", br.GetJob("bjob_sec"), br.GetJob("bjob_nano"))
}
