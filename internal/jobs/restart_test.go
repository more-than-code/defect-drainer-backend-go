package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

const (
	testSessionID  = "01a0c8be-7e0e-7c71-92ad-cd85bf99e86f"
	storedBaseline = "2026-09-01T00:00:00Z" // StartedAt marks the stored baseline
)

// restartFixture is a stopped grok job ready for Re-run or Resume: a real git
// worktree whose DiffBase is its first commit (a second commit stands for the
// prior run's work), a stored baseline and agentRun, and a verify command that
// counts its own executions.
type restartFixture struct {
	br        *BatchRunner
	st        *store.Store
	defects   string
	job       *Job
	defID     string
	argsLog   string // every fake-grok argv, each call opened by a "---" line
	verifyLog string // one line per verify-command execution
	base      string
	exited    chan struct{}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, repo, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", name)
	gitRun(t, repo, "commit", "-q", "-m", name)
	return gitRun(t, repo, "rev-parse", "HEAD")
}

// writeJobSession puts a grok session for the job's handoff cwd where
// git.LatestGrokSession looks for it.
func writeJobSession(t *testing.T, br *BatchRunner, jobID, id string) {
	t.Helper()
	root, err := git.GrokSessionsDir(br.handoffDir(jobID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, id, "chat_history.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// artifactsBody makes the fake agent deliver everything BRIEF.md demands and
// report 7 turns / $0.25, so the loop stops after one iteration.
func artifactsBody(defID string) string {
	return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'fixed it' > fix-notes/" + defID + ".md\n" +
		"printf 'plan' > NOTES.md\n" +
		`echo '{"type":"end","stopReason":"end_turn","sessionId":"` + testSessionID + `","num_turns":7,"total_cost_usd":0.25}'` + "\n" +
		"exit 0"
}

func newRestartFixture(t *testing.T, status, stoppedBy, grokBody string, withSession bool) *restartFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GROK_HOME", "")
	st, data, defects := testStore(t)
	f := &restartFixture{
		br: NewBatchRunner(st, data), st: st, defects: defects,
		defID:     "DEF-20260923-restart-0001",
		argsLog:   filepath.Join(t.TempDir(), "args"),
		verifyLog: filepath.Join(t.TempDir(), "verify"),
		exited:    make(chan struct{}),
	}
	writeInProgressDefect(t, st, f.defID)

	verify, _ := json.Marshal([]store.VerifyCommand{{Repo: "demo", Command: "echo run >> '" + f.verifyLog + "'"}})
	if _, err := st.DB.Exec(`UPDATE apps SET verify_commands_json = ? WHERE id = ?`, string(verify), store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	f.base = commitFile(t, repo, "base.txt")
	commitFile(t, repo, "prior-run.txt")

	f.job = &Job{
		ID: "bjob_restart", Kind: "batch", Status: status, Mode: "grok",
		BatchID: "BATCH-20260923-rs", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{f.defID},
		CreatedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		Worktrees: []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}},
		DiffBase: map[string]string{"demo": f.base},
		Baseline: &VerificationRun{Ran: true, StartedAt: storedBaseline, Results: []VerifyResult{
			{Repo: "demo", Command: "stored", Ok: true},
		}},
		AgentRun: &AgentRun{
			Iterations: 3, StoppedBy: stoppedBy, Turns: 40, TotalCostUSD: 1.5,
			SessionID: "prior", StopReasons: []string{"cancelled"},
		},
	}
	f.br.jobs[f.job.ID] = f.job
	if _, err := st.DB.Exec(`INSERT INTO batches (id, app_id, title, goal, status, defect_ids_json, mode, created, updated_at)
      VALUES (?, ?, 'b', '', 'failed', '[]', 'grok', '2026-09-23', '2026-09-23')`, f.job.BatchID, store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
	if withSession {
		writeJobSession(t, f.br, f.job.ID, testSessionID)
	}

	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, "#!/bin/sh\n{ echo ---; printf '%s\\n' \"$@\"; } >> '"+f.argsLog+"'\n"+grokBody+"\n")
	t.Setenv("GROK_BUILD_BIN", bin)
	spawnExitHook = func() { close(f.exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	return f
}

// calls splits the argv log into one []string per agent invocation.
func (f *restartFixture) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(f.argsLog)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, block := range strings.Split(string(data), "---\n")[1:] {
		out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n"))
	}
	return out
}

func (f *restartFixture) verifyRuns() int {
	data, _ := os.ReadFile(f.verifyLog)
	return len(strings.Fields(string(data)))
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

func logHas(j *Job, sub string) bool {
	for _, l := range j.Log {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// AC1–4: Resume continues the named session, reuses the clean-code baseline
// and the first diff base, and adds to the job's run record.
func TestResumeContinuesSessionWithStoredState(t *testing.T) {
	f := newRestartFixture(t, "failed", StoppedByExit, artifactsBody("DEF-20260923-restart-0001"), true)

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)

	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("want 1 agent call, got %d: %q", len(calls), calls)
	}
	if id, _ := flagValue(calls[0], "-r"); id != testSessionID {
		t.Fatalf("iteration 1 must resume -r %s: %q", testSessionID, calls[0])
	}
	if _, ok := flagValue(calls[0], "-c"); ok {
		t.Fatalf("-r and -c together: %q", calls[0])
	}
	if p, _ := flagValue(calls[0], "-p"); !strings.HasPrefix(p, "Continue executing BRIEF.md") {
		t.Fatalf("resume must send ContinuePrompt, got %q", p)
	}
	if got := f.verifyRuns(); got != 1 {
		t.Fatalf("verify command ran %d times; want 1 (post-run only — the baseline must be reused)", got)
	}

	live := f.br.GetJob(f.job.ID)
	if live.Baseline == nil || live.Baseline.StartedAt != storedBaseline {
		t.Fatalf("stored baseline replaced: %+v", live.Baseline)
	}
	run := live.AgentRun
	if run.Iterations != 4 || run.Turns != 47 || run.TotalCostUSD != 1.75 || run.Resumes != 1 {
		t.Fatalf("agentRun not cumulative: %+v", run)
	}
	if run.SessionID != testSessionID {
		t.Fatalf("sessionId = %q, want this run's", run.SessionID)
	}
	if live.DiffBase["demo"] != f.base {
		t.Fatalf("diffBase moved: %v", live.DiffBase)
	}
	if live.DiffHygiene == nil || len(live.DiffHygiene.Repos) != 1 || live.DiffHygiene.Repos[0].BaseSha != f.base {
		t.Fatalf("hygiene must measure from the first diff base: %+v", live.DiffHygiene)
	}
	if live.DiffHygiene.Repos[0].FilesChanged < 1 {
		t.Fatalf("the prior run's commit fell out of the diff: %+v", live.DiffHygiene.Repos[0])
	}
	if !logHas(live, "continuing grok session "+testSessionID) {
		t.Fatalf("log must name the resumed session: %q", live.Log)
	}
	if live.Status != "completed" {
		t.Fatalf("status = %q, want completed", live.Status)
	}
}

// AC8: Re-run is a new session, but still never re-measures the baseline.
func TestRerunStartsFreshSessionKeepsBaseline(t *testing.T) {
	f := newRestartFixture(t, "completed", StoppedByArtifacts, artifactsBody("DEF-20260923-restart-0001"), false)

	if _, err := f.br.Restart(f.job.ID, f.defects, false); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)

	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("want 1 agent call, got %d", len(calls))
	}
	for _, flag := range []string{"-c", "-r"} {
		if _, ok := flagValue(calls[0], flag); ok {
			t.Fatalf("re-run must start a new session, saw %s: %q", flag, calls[0])
		}
	}
	if p, _ := flagValue(calls[0], "-p"); strings.HasPrefix(p, "Continue executing BRIEF.md") {
		t.Fatal("re-run must send the full brief prompt")
	}
	if got := f.verifyRuns(); got != 1 {
		t.Fatalf("verify command ran %d times; want 1 (baseline reused)", got)
	}
	live := f.br.GetJob(f.job.ID)
	if live.Baseline == nil || live.Baseline.StartedAt != storedBaseline {
		t.Fatalf("stored baseline replaced: %+v", live.Baseline)
	}
	if live.AgentRun.Iterations != 4 || live.AgentRun.Resumes != 0 {
		t.Fatalf("agentRun = %+v, want iterations 4 and no resumes", live.AgentRun)
	}
	if !logHas(live, "re-run requested") {
		t.Fatalf("log: %q", live.Log)
	}
}

// AC13: a job that ran out of iterations gets a fresh budget on Resume.
func TestResumeGetsFreshIterationBudget(t *testing.T) {
	f := newRestartFixture(t, "completed", StoppedByIterations, "exit 0", true)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "2")

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)

	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("want a fresh cap of 2 more iterations, got %d", len(calls))
	}
	if id, _ := flagValue(calls[0], "-r"); id != testSessionID {
		t.Fatalf("iteration 1: %q", calls[0])
	}
	if _, ok := flagValue(calls[1], "-c"); !ok {
		t.Fatalf("iteration 2 continues with -c: %q", calls[1])
	}
	live := f.br.GetJob(f.job.ID)
	if live.AgentRun.Iterations != 5 || live.AgentRun.StoppedBy != StoppedByIterations {
		t.Fatalf("agentRun = %+v, want 3 prior + 2", live.AgentRun)
	}
}

// AC5: of two overlapping restarts exactly one starts an agent.
func TestRestartRejectsWhileLive(t *testing.T) {
	f := newRestartFixture(t, "failed", StoppedByExit, artifactsBody("DEF-20260923-restart-0001"), true)
	entered, release := make(chan struct{}), make(chan struct{})
	startSpawnHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { startSpawnHook = nil })

	errc := make(chan error, 1)
	go func() {
		_, err := f.br.Restart(f.job.ID, f.defects, true)
		errc <- err
	}()
	<-entered
	for _, resume := range []bool{true, false} {
		if _, err := f.br.Restart(f.job.ID, f.defects, resume); !errors.Is(err, ErrJobLive) {
			t.Fatalf("restart(resume=%v) during a start: err = %v, want ErrJobLive", resume, err)
		}
	}
	if c := f.br.GetJobView(f.job.ID); c.Resumable.Ok || c.Rerunnable.Ok {
		t.Fatalf("a live job must not offer restarts: %+v %+v", c.Resumable, c.Rerunnable)
	}
	var batchStatus string
	if err := f.st.DB.QueryRow(`SELECT status FROM batches WHERE id = ?`, f.job.BatchID).Scan(&batchStatus); err != nil {
		t.Fatal(err)
	}
	if batchStatus != "in_progress" {
		t.Fatalf("batch status during the restart = %q, want in_progress", batchStatus)
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)
	if got := len(f.calls(t)); got != 1 {
		t.Fatalf("want exactly 1 agent, got %d", got)
	}
}

// AC6: a Stopped job really restarts (it used to be a silent no-op), and the
// defects the cancel sync reopened are in progress again.
func TestRestartCancelledJobRuns(t *testing.T) {
	f := newRestartFixture(t, "cancelled", "", "exit 0", false)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	rec, err := f.st.Get(f.defID)
	if err != nil || rec == nil {
		t.Fatal(err)
	}
	rec.Status = "open"
	if _, err := f.st.Write(*rec); err != nil {
		t.Fatal(err)
	}

	j, err := f.br.Restart(f.job.ID, f.defects, false)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "running" {
		t.Fatalf("status after restart = %q, want running", j.Status)
	}
	waitExit(t, f.exited)
	if got := len(f.calls(t)); got != 1 {
		t.Fatalf("want 1 agent call, got %d", got)
	}
	rec, err = f.st.Get(f.defID)
	if err != nil || rec == nil {
		t.Fatal(err)
	}
	if rec.Status != "in_progress" {
		t.Fatalf("defect status = %q, want in_progress", rec.Status)
	}
}

// Stop can land while the driver goroutine is in harvest, after its loop
// bookkeeping is gone. Until that goroutine exits, a restart would run beside
// it on the same handoff, so it must be refused.
func TestRestartWaitsForStoppedDriverToExit(t *testing.T) {
	f := newRestartFixture(t, "failed", StoppedByExit, artifactsBody("DEF-20260923-restart-0001"), true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once bool
	harvestStartHook = func() {
		if once {
			return
		}
		once = true
		close(entered)
		<-release
	}
	t.Cleanup(func() { harvestStartHook = nil })

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := f.br.Stop(f.job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.br.Restart(f.job.ID, f.defects, false); !errors.Is(err, ErrJobLive) {
		t.Fatalf("restart while the stopped run is still harvesting: err = %v, want ErrJobLive", err)
	}
	close(release)
	waitExit(t, f.exited)

	f.exited = make(chan struct{})
	spawnExitHook = func() { close(f.exited) }
	if _, err := f.br.Restart(f.job.ID, f.defects, false); err != nil {
		t.Fatalf("restart after the old run exited: %v", err)
	}
	waitExit(t, f.exited)
	if got := len(f.calls(t)); got != 2 {
		t.Fatalf("want 2 agents in sequence, got %d", got)
	}
}

// D6: an earlier run's screenshot still counts toward loop completion but is
// never imported again — harvest takes only images created after the restart,
// so the defect gains exactly this run's image and resolves on it. The earlier
// note still supplies the resolution text.
func TestRestartImportsOnlyThisRunsImages(t *testing.T) {
	const def = "DEF-20260923-restart-0001"
	body := "printf 'png2' > fix-evidence/" + def + "/fix-02.png\nexit 0"
	f := newRestartFixture(t, "failed", StoppedByExit, body, true)
	handoff := f.br.handoffDir(f.job.ID)
	for path, data := range map[string]string{
		"fix-evidence/" + def + "/fix-01.png": "png1",
		"fix-notes/" + def + ".md":            "prior run's note",
		"NOTES.md":                            "plan",
	} {
		full := filepath.Join(handoff, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)

	if got := len(f.calls(t)); got != 1 {
		t.Fatalf("earlier artifacts plus a new image should complete in 1 iteration, got %d", got)
	}
	rec, err := f.st.Get(def)
	if err != nil || rec == nil {
		t.Fatal(err)
	}
	if len(rec.FixEvidence) != 1 {
		t.Fatalf("fix_evidence = %v; want only this run's image (no re-import of fix-01)", rec.FixEvidence)
	}
	if rec.Status != "resolved" || rec.Resolution != "prior run's note" {
		t.Fatalf("status %q resolution %q; want resolved on this run's image with the earlier note", rec.Status, rec.Resolution)
	}
}

// The negative reasons end to end: Restart derives them from disk, refuses,
// starts nothing, and leaves the job as it was.
func TestRestartRefusesFromDiskFacts(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		f := newRestartFixture(t, "failed", StoppedByExit, "exit 0", false)
		_, err := f.br.Restart(f.job.ID, f.defects, true)
		if err == nil || err.Error() != "no grok session found for this job" {
			t.Fatalf("err = %v", err)
		}
		if got := f.br.GetJobView(f.job.ID); got.Status != "failed" || got.Resumable.Reason != err.Error() || !got.Rerunnable.Ok {
			t.Fatalf("job after refusal: status %q resumable %+v rerunnable %+v", got.Status, got.Resumable, got.Rerunnable)
		}
		if n := len(f.calls(t)); n != 0 {
			t.Fatalf("a refused restart started %d agent(s)", n)
		}
	})
	t.Run("worktree gone", func(t *testing.T) {
		f := newRestartFixture(t, "failed", StoppedByExit, "exit 0", true)
		if err := os.RemoveAll(f.job.Worktrees[0].WorktreeAbs); err != nil {
			t.Fatal(err)
		}
		for _, resume := range []bool{true, false} {
			if _, err := f.br.Restart(f.job.ID, f.defects, resume); err == nil || err.Error() != "worktree missing: demo" {
				t.Fatalf("restart(resume=%v) err = %v", resume, err)
			}
		}
		if n := len(f.calls(t)); n != 0 {
			t.Fatalf("a refused restart started %d agent(s)", n)
		}
	})
}

// AC7/AC12: one row per D1/D5 case, asserting the exact operator reason.
func TestRestartChecks(t *testing.T) {
	wt := []git.WorktreeBinding{{Repo: "demo"}}
	ok := restartFacts{sessionFound: true}
	job := func(status, stoppedBy string) *Job {
		j := &Job{Mode: "grok", Status: status, Worktrees: wt}
		if stoppedBy != "-" {
			j.AgentRun = &AgentRun{StoppedBy: stoppedBy}
		}
		return j
	}
	withPR := func(j *Job, p PR) *Job { j.PRs = []PR{p}; return j }
	manual := job("manual", "-")
	manual.Mode = "manual"
	noWt := job("failed", StoppedByExit)
	noWt.Worktrees = nil

	const finished = "agent finished its run — use Re-run for another pass"
	const landed = "PR merged/closed — file a new batch"
	cases := []struct {
		name          string
		j             *Job
		f             restartFacts
		resume, rerun string // "" means allowed
	}{
		{"cancelled", job("cancelled", ""), ok, "", ""},
		{"backend restart", job("failed", "-"), ok, "", ""},
		{"crash", job("failed", StoppedByExit), ok, "", ""},
		{"iteration cap", job("completed", StoppedByIterations), ok, "", ""},
		{"wall cap", job("completed", StoppedByWall), ok, "", ""},
		{"artifacts delivered", job("completed", StoppedByArtifacts), ok, finished, ""},
		{"legacy completed", job("completed", "-"), ok, finished, ""},
		{"no session", job("failed", StoppedByExit), restartFacts{}, "no grok session found for this job", ""},
		{"live", job("running", ""), restartFacts{live: true, sessionFound: true}, ErrJobLive.Error(), ErrJobLive.Error()},
		{"manual mode", manual, ok, "not a coding-agent job", "not a coding-agent job"},
		{"no worktrees", noWt, ok, "job has no worktrees", "job has no worktrees"},
		{"worktree gone", job("failed", StoppedByExit), restartFacts{sessionFound: true, missingWorktree: "demo"}, "worktree missing: demo", "worktree missing: demo"},
		{"open PR", withPR(job("failed", StoppedByExit), PR{GhState: "open"}), ok, "", ""},
		{"merged PR", withPR(job("failed", StoppedByExit), PR{GhState: "merged"}), ok, landed, landed},
		{"closed PR", withPR(job("failed", StoppedByExit), PR{GhState: "closed"}), ok, landed, landed},
		{"odd status", job("manual", "-"), ok, "job status manual cannot be resumed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, got := range []struct {
				label string
				check *RestartCheck
				want  string
			}{
				{"resume", resumeCheck(c.j, c.f), c.resume},
				{"rerun", rerunCheck(c.j, c.f), c.rerun},
			} {
				if got.check.Ok != (got.want == "") || got.check.Reason != got.want {
					t.Fatalf("%s = %+v, want reason %q", got.label, got.check, got.want)
				}
			}
		})
	}
}

func TestMissingWorktreeNeedsCheckout(t *testing.T) {
	plain := t.TempDir()
	if got := missingWorktree([]git.WorktreeBinding{{Repo: "demo", WorktreeAbs: plain}}); got != "demo" {
		t.Fatalf("a dir without .git is not a worktree, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(plain, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := missingWorktree([]git.WorktreeBinding{{Repo: "demo", WorktreeAbs: plain}}); got != "" {
		t.Fatalf("a linked worktree has a .git file, got %q", got)
	}
}
