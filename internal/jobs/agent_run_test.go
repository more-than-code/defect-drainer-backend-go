package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingAgent writes one line per invocation to counter so a test can assert
// how many iterations the driver loop actually ran, then runs body.
func countingAgent(t *testing.T, counter, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\necho iter >> \""+counter+"\"\n"+body+"\n")
	return bin
}

func iterations(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(data)))
}

// waitExit blocks on the completion goroutine or fails the test.
func waitExit(t *testing.T, exited chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("completion goroutine did not exit")
	}
}

func TestLoopCapsIterationsWhenNoArtifacts(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopcap-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopcap", "BATCH-20260922-lc", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	// Exit 0 with no artifacts at all: the pre-H2 harness treated this as a
	// finished job. The loop must resume instead, and stop at the cap.
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, "exit 0"))
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "2")
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 2 {
		t.Fatalf("want 2 agent iterations, got %d", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.AgentRun == nil {
		t.Fatalf("no agentRun recorded: %+v", live)
	}
	if live.AgentRun.Iterations != 2 {
		t.Fatalf("agentRun.iterations = %d, want 2", live.AgentRun.Iterations)
	}
	if live.AgentRun.StoppedBy != StoppedByIterations {
		t.Fatalf("stoppedBy = %q, want %q", live.AgentRun.StoppedBy, StoppedByIterations)
	}
	if live.AgentRun.CompletedByArtifacts {
		t.Fatal("completedByArtifacts must be false when the cap ended the loop")
	}
	if len(live.AgentRun.Missing) == 0 {
		t.Fatal("agentRun.missing should name what was absent")
	}
	// Exit 0 is not failure: the operator still gets verify and a completed job.
	if live.Status != "completed" {
		t.Fatalf("status = %q, want completed (the process exited 0)", live.Status)
	}
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatal("a defect with no evidence must not resolve")
	}
}

func TestLoopStopsOnceArtifactsExist(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopdone-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopdone", "BATCH-20260922-ld", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	body := "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'fixed it' > fix-notes/" + defID + ".md\n" +
		"printf 'plan' > NOTES.md\nexit 0"
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, body))
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 1 {
		t.Fatalf("complete artifacts should stop after 1 iteration, got %d", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.AgentRun == nil {
		t.Fatalf("no agentRun: %+v", live)
	}
	if !live.AgentRun.CompletedByArtifacts || live.AgentRun.StoppedBy != StoppedByArtifacts {
		t.Fatalf("agentRun = %+v, want completed by artifacts", live.AgentRun)
	}
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status != "resolved" {
		t.Fatalf("defect should resolve on this-run evidence: %+v", got)
	}
}

func TestLoopStopsOnWallClock(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopwall-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopwall", "BATCH-20260922-lw", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	// No verify commands are configured, so the clock covers only the agent —
	// a slow baseline must never consume the agent's wall budget.
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, "sleep 2\nexit 0"))
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_WALL_SEC", "1")
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "8")
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 1 {
		t.Fatalf("wall clock should stop after the first iteration, got %d", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.AgentRun == nil || live.AgentRun.StoppedBy != StoppedByWall {
		t.Fatalf("want stoppedBy=wall, got %+v", live.AgentRun)
	}
}

func TestStopBetweenIterationsDoesNotStartAnother(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopstop-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopstop", "BATCH-20260922-ls", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, "exit 0"))
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	// Land the stop exactly in the gap: iteration 1 reaped, no process handle
	// installed, next child not yet started.
	loopBeforeNextHook = func() {
		loopBeforeNextHook = nil
		_, _ = br.Stop(job.ID)
	}
	t.Cleanup(func() {
		spawnExitHook = nil
		loopBeforeNextHook = nil
	})

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 1 {
		t.Fatalf("stop in the gap must not start iteration 2, got %d iterations", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.Status != "cancelled" {
		t.Fatalf("job should be cancelled, got %+v", live)
	}
}

func TestDeleteBetweenIterationsWaitsForLoop(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopdel-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopdel", "BATCH-20260922-ldl", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, "exit 0"))
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	deleted := make(chan error, 1)
	// The hook runs ON the loop goroutine, so DeleteJob must be called from
	// another one: it waits for the loop to release, which this goroutine is
	// the only one able to do.
	loopBeforeNextHook = func() {
		loopBeforeNextHook = nil
		go func() { _, err := br.DeleteJob(job.ID); deleted <- err }()
	}
	t.Cleanup(func() {
		spawnExitHook = nil
		loopBeforeNextHook = nil
	})

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete in the inter-iteration gap failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("DeleteJob blocked in the inter-iteration gap")
	}
	waitExit(t, exited)

	// The guarantee is that DeleteJob neither hangs nor leaves an orphan. It
	// races the loop for the gap, so losing that race and starting iteration 2
	// — which DeleteJob then kills — is correct, not a failure.
	if got := iterations(t, counter); got > 2 {
		t.Fatalf("deleted job kept iterating: %d iterations", got)
	}
	if br.GetJob(job.ID) != nil {
		t.Fatal("deleted job still in map")
	}
	if _, err := os.Stat(filepath.Join(data, "batch-jobs", job.ID, "job.json")); !os.IsNotExist(err) {
		t.Fatalf("job.json survived delete: %v", err)
	}
}

func TestFailedAgentSavesEvidenceButDoesNotResolve(t *testing.T) {
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-loopfail-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_loopfail", "BATCH-20260922-lf", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	// A genuine failure: exits non-zero with no max_turns_reached event. It
	// must not resume, and its screenshot must not buy a resolve.
	body := "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'notes' > fix-notes/" + defID + ".md\n" +
		"printf 'plan' > NOTES.md\nexit 3"
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, body))
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 1 {
		t.Fatalf("a real failure must not resume, got %d iterations", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.Status != "failed" {
		t.Fatalf("job should be failed, got %+v", live)
	}
	if live.AgentRun == nil || live.AgentRun.StoppedBy != StoppedByExit {
		t.Fatalf("want stoppedBy=exit, got %+v", live.AgentRun)
	}
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatalf("a failed agent must not resolve a defect: %+v", got)
	}
	if len(got.FixEvidence) == 0 {
		t.Fatalf("evidence from a failed run should still be imported: %+v", got)
	}
}

func TestHistoricalEvidenceDoesNotResolveWithoutThisRunImages(t *testing.T) {
	st, data, defects := testStore(t)
	_ = defects
	br := NewBatchRunner(st, data)
	defID := "DEF-20260922-histonly-0001"
	writeInProgressDefect(t, st, defID)

	// Give the defect evidence from an earlier job, the way a prior fix run
	// would have left it.
	added, err := st.SaveEvidence(defID, []struct {
		Filename string
		Data     []byte
	}{{"fix-01.png", []byte("old")}}, "fix")
	if err != nil {
		t.Fatal(err)
	}
	// SaveEvidence stores the files; the record has to be written separately,
	// the same way harvest does it.
	cur, err := st.Get(defID)
	if err != nil || cur == nil {
		t.Fatal(err)
	}
	cur.FixEvidence = append(cur.FixEvidence, added...)
	if _, err := st.Write(*cur); err != nil {
		t.Fatal(err)
	}
	cur, err = st.Get(defID)
	if err != nil || cur == nil {
		t.Fatal(err)
	}
	if len(cur.FixEvidence) == 0 {
		t.Fatal("setup: defect should already carry fix evidence")
	}

	job := runningJob(t, br, "bjob_histonly", "BATCH-20260922-ho", defID)
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Notes this run, but no new image.
	if err := os.WriteFile(filepath.Join(handoff, "fix-notes", defID+".md"), []byte("I claim it works"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestFixEvidence(br.GetJob(job.ID), handoff, &HarvestOpts{AllowResolve: true})

	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatalf("prose plus a previous run's screenshot must not resolve: %+v", got)
	}
}
