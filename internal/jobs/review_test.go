package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// Existing spawn tests were written before the reviewer existed. Their fakes
// are not reviewers, and several of them count invocations or expect a resolve
// that a fail-closed review would block. Review stays off unless a test opts in.
func TestMain(m *testing.M) {
	if os.Getenv("DEFECT_DRAINER_REVIEW") == "" {
		os.Setenv("DEFECT_DRAINER_REVIEW", "0")
	}
	os.Exit(m.Run())
}

const emitEmptyFindings = `printf '%s\n' '{"type":"text","data":"{\"findings\":[]}"}'
printf '%s\n' '{"type":"end","stopReason":"end_turn","sessionId":"rev","num_turns":1,"total_cost_usd":0.02}'
exit 0`

// emitPassingReview is empty findings plus a completed git diff of the
// worktree path REVIEW.md names. Without that pair the lens is not ok.
func emitPassingReview() string {
	return `wt=$(sed -n 's/^- [^:]*: //p' REVIEW.md | head -1)
printf '%s\n' "{\"type\":\"tool_call\",\"toolCallId\":\"c1\",\"toolName\":\"run_terminal_command\",\"rawInput\":{\"command\":\"git -C ${wt} diff\",\"description\":\"diff\"}}"
printf '%s\n' '{"type":"tool_call_update","toolCallId":"c1","status":"completed"}'
` + emitEmptyFindings
}

func implArtifacts(defID string) string {
	return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'fixed it' > fix-notes/" + defID + ".md\n" +
		"printf 'plan' > NOTES.md\nexit 0\n"
}

func reviewScript(logPath, reviewer, impl string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if logPath != "" {
		fmt.Fprintf(&b, "{ echo ---; pwd; printf '%%s\\n' \"$@\"; } >> '%s'\n", logPath)
	}
	b.WriteString("case \"$*\" in\n*\"--sandbox read-only\"*)\n")
	b.WriteString(reviewer)
	b.WriteString("\n;;\n*)\n")
	b.WriteString(impl)
	b.WriteString("\n;;\nesac\n")
	return b.String()
}

func shellFindings(severity, defectID string) string {
	finding := fmt.Sprintf(
		`printf '%%s\n' "{\"type\":\"text\",\"data\":\"{\\\"ok\\\":true,\\\"findings\\\":[{\\\"severity\\\":\\\"%s\\\",\\\"lens\\\":\\\"$lens\\\",\\\"defect_id\\\":\\\"%s\\\",\\\"location\\\":\\\"demo:file.go:1\\\",\\\"finding\\\":\\\"still wrong\\\",\\\"reasoning\\\":\\\"the diff misses it\\\",\\\"suggested_fix\\\":\\\"replace the control\\\"}]}\"}"`,
		severity, defectID,
	)
	return `cwd=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--cwd" ]; then
    cwd=$a
  fi
  prev=$a
done
lens=${cwd##*/}
` + finding + `
printf '%s\n' '{"type":"end","stopReason":"end_turn","sessionId":"rev","num_turns":2,"total_cost_usd":0.2}'
exit 0
`
}

func argvBlocks(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	parts := strings.Split(string(data), "---\n")
	var out []string
	for _, p := range parts[1:] {
		out = append(out, p)
	}
	return out
}

func reviewerBlocks(blocks []string) []string {
	var out []string
	for _, b := range blocks {
		if strings.Contains(b, "--sandbox\nread-only") {
			out = append(out, b)
		}
	}
	return out
}

func startReviewedJob(t *testing.T, jobID, defID, script string, mutate func(br *BatchRunner, job *Job)) (*BatchRunner, *store.Store) {
	t.Helper()
	t.Setenv("DEFECT_DRAINER_REVIEW", "1")
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, jobID, "BATCH-20260926-rv", defID)
	if mutate != nil {
		mutate(br, job)
	}
	// A worktree with no .git now fails the manifest before any lens. Tests
	// that use runningJob's empty directory still need a real diff so the
	// lens under test is reached.
	ensureReviewableRepo(t, job)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, script)
	t.Setenv("GROK_BUILD_BIN", bin)
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() {
		spawnExitHook = nil
		reviewLensStartedHook = nil
		reviewLensFinishedHook = nil
		reviewBeforeSnapshotHook = nil
	})
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	return br, st
}

// ensureReviewableRepo gives a non-git worktree a commit and a dirty file so
// manifest capture can run. Bindings that already have a .git, or no path,
// are left alone.
func ensureReviewableRepo(t *testing.T, job *Job) {
	t.Helper()
	for _, w := range job.Worktrees {
		if w.WorktreeAbs == "" || samePath(w.WorktreeAbs, w.PrimaryAbs) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(w.WorktreeAbs, ".git")); err == nil {
			continue
		}
		gitRun(t, w.WorktreeAbs, "init", "-q")
		base := filepath.Join(w.WorktreeAbs, "review-base.txt")
		if err := os.WriteFile(base, []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, w.WorktreeAbs, "add", "review-base.txt")
		gitRun(t, w.WorktreeAbs, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "base")
		if err := os.WriteFile(base, []byte("base\ndirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func jobLog(j *Job) string {
	if j == nil {
		return ""
	}
	return strings.Join(j.Log, "\n")
}

func TestReviewUIPathHeuristic(t *testing.T) {
	yes := []string{
		"lib/widgets/button.dart",
		"src/components/Button.tsx",
		"pkg/lib/chat/composer_widget.dart",
		"src/Widget.tsx",
	}
	no := []string{
		"lib/main.dart",
		"src/index.ts",
		"README.md",
		"widget.go",
		"lib",
	}
	for _, p := range yes {
		if !pathLooksLikeUI(p) {
			t.Errorf("want UI: %s", p)
		}
	}
	for _, p := range no {
		if pathLooksLikeUI(p) {
			t.Errorf("want not UI: %s", p)
		}
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib", "widgets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "widgets", "a.dart"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: root, PrimaryAbs: t.TempDir()}}
	got := reviewLenses(wts)
	if strings.Join(got, ",") != "spec,security,code,tests,ui" {
		t.Fatalf("lenses = %v", got)
	}
	plain := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plain, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "lib", "main.dart"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = reviewLenses([]git.WorktreeBinding{{Repo: "demo", WorktreeAbs: plain, PrimaryAbs: t.TempDir()}})
	if strings.Join(got, ",") != "spec,security,code,tests" {
		t.Fatalf("lib/main.dart must not add ui: %v", got)
	}
}

func TestReviewLensOrder(t *testing.T) {
	defID := "DEF-20260926-revorder-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv("DEFECT_DRAINER_REVIEW_MAX_TURNS", "5")
	br, st := startReviewedJob(t, "bjob_revorder", defID, reviewScript(logPath, emitPassingReview(), implArtifacts(defID)), nil)
	blocks := reviewerBlocks(argvBlocks(t, logPath))
	if len(blocks) != 4 {
		t.Fatalf("reviewer spawns = %d\n%v", len(blocks), blocks)
	}
	var cwds []string
	for _, b := range blocks {
		lines := strings.Split(strings.TrimRight(b, "\n"), "\n")
		cwd := lines[0]
		cwds = append(cwds, filepath.Base(cwd))
		if filepath.Base(filepath.Dir(cwd)) != "review" {
			t.Fatalf("cwd = %s", cwd)
		}
		for _, banned := range []string{"-c", "-r", "--resume"} {
			for _, line := range lines[1:] {
				if line == banned {
					t.Fatalf("argv contains %s\n%s", banned, b)
				}
			}
		}
		if !strings.Contains(b, "--max-turns\n5\n") && !strings.Contains(b, "--max-turns\n5") {
			t.Fatalf("max-turns not from REVIEW_MAX_TURNS:\n%s", b)
		}
	}
	if strings.Join(cwds, ",") != "spec,security,code,tests" {
		t.Fatalf("order = %v", cwds)
	}
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("empty findings should resolve, got %+v %v", got, err)
	}
	live := br.GetJob("bjob_revorder")
	if live == nil || live.Status != "completed" || live.Review == nil || !live.Review.OK || !live.Review.Required {
		t.Fatalf("review = %+v job = %+v", live.Review, live)
	}
}

func TestReviewUILensIsSpawned(t *testing.T) {
	defID := "DEF-20260926-revui-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	br, _ := startReviewedJob(t, "bjob_revui", defID, reviewScript(logPath, emitEmptyFindings, implArtifacts(defID)), func(_ *BatchRunner, job *Job) {
		dir := filepath.Join(job.Worktrees[0].WorktreeAbs, "src", "components")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Button.tsx"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	blocks := reviewerBlocks(argvBlocks(t, logPath))
	if len(blocks) != 5 {
		t.Fatalf("want 5 lenses, got %d", len(blocks))
	}
	last := strings.Split(strings.TrimRight(blocks[4], "\n"), "\n")[0]
	if filepath.Base(last) != "ui" {
		t.Fatalf("last cwd = %s", last)
	}
	if live := br.GetJob("bjob_revui"); live == nil || live.Review == nil || len(live.Review.Lenses) != 5 {
		t.Fatalf("review = %+v", live.Review)
	}
}

func TestReviewHighFindingBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revhigh-0001"
	marker := filepath.Join(t.TempDir(), "review-pwned")
	// suggested_fix names a shell command. Harvest must not run it.
	body := strings.ReplaceAll(shellFindings("high", defID), "replace the control", "touch "+marker)
	br, st := startReviewedJob(t, "bjob_revhigh", defID, reviewScript("", body, implArtifacts(defID)), nil)
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatalf("high finding resolved the defect: %+v", got)
	}
	if len(got.FixEvidence) == 0 {
		t.Fatal("evidence should still be imported")
	}
	if got.Resolution == "" {
		t.Fatal("fix note should still be imported")
	}
	live := br.GetJob("bjob_revhigh")
	if live == nil || live.Status != "completed" || live.Review == nil || live.Review.OK {
		t.Fatalf("job = %+v review = %+v", live, live.Review)
	}
	if len(live.Harvests) != 1 || len(live.Harvests[0].Defects) != 1 {
		t.Fatalf("harvests = %+v", live.Harvests)
	}
	h := live.Harvests[0].Defects[0]
	if h.Outcome != harvestReviewBlocked || !strings.Contains(h.Reason, "4 critical/high") {
		t.Fatalf("harvest = %+v", h)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("suggested_fix was executed")
	}
}

func TestReviewMissingPayloadBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revmiss-0001"
	reviewer := "echo hello from reviewer\nprintf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"end_turn\",\"num_turns\":1,\"total_cost_usd\":0}'\nexit 0\n"
	br, st := startReviewedJob(t, "bjob_revmiss", defID, reviewScript("", reviewer, implArtifacts(defID)), nil)
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("missing payload resolved: %+v %v", got, err)
	}
	live := br.GetJob("bjob_revmiss")
	if live == nil || live.Review == nil || live.Review.OK {
		t.Fatalf("review = %+v", live.Review)
	}
	if len(live.Review.Lenses) != 4 || !strings.Contains(live.Review.Lenses[0].Error, "missing") || !strings.Contains(live.Review.Lenses[0].Error, "spec") {
		t.Fatalf("lenses = %+v", live.Review.Lenses)
	}
}

func TestReviewWallDeadlineBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revwall-0001"
	t.Setenv("DEFECT_DRAINER_REVIEW_MAX_WALL_SEC", "1")
	reviewer := "exec sleep 30\n"
	br, st := startReviewedJob(t, "bjob_revwall", defID, reviewScript("", reviewer, implArtifacts(defID)), nil)
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("deadline resolved: %+v %v", got, err)
	}
	live := br.GetJob("bjob_revwall")
	if live == nil || live.Status != "completed" || live.Review == nil {
		t.Fatalf("job = %+v", live)
	}
	if len(live.Review.Lenses) != 4 {
		t.Fatalf("lenses = %+v", live.Review.Lenses)
	}
	for _, l := range live.Review.Lenses {
		if l.Error != "deadline" {
			t.Fatalf("lens %s error = %q", l.Lens, l.Error)
		}
	}
}

func TestReviewSandboxSocketBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revsock-0001"
	reviewer := "echo 'endpoint is a symlink: /var/run/docker.sock' >&2\necho 'Refusing to start' >&2\nexit 1\n"
	br, st := startReviewedJob(t, "bjob_revsock", defID, reviewScript("", reviewer, implArtifacts(defID)), nil)
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("socket refusal resolved: %+v %v", got, err)
	}
	live := br.GetJob("bjob_revsock")
	if live == nil || live.Review == nil || len(live.Review.Lenses) != 4 {
		t.Fatalf("review = %+v", live.Review)
	}
	for _, l := range live.Review.Lenses {
		if l.Error != reviewSocketError {
			t.Fatalf("lens %s error = %q", l.Lens, l.Error)
		}
	}
	if !strings.Contains(jobLog(live), "runtime socket is a symlink") {
		t.Fatalf("log missing socket error:\n%s", jobLog(live))
	}
	sawError := false
	for _, line := range live.Log {
		if strings.Contains(line, reviewSocketError) && strings.Contains(line, "[error]") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("socket refusal was not logged at error")
	}
}

func TestReviewDisabledByEnvResolves(t *testing.T) {
	defID := "DEF-20260926-revoff-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv("DEFECT_DRAINER_REVIEW", "0")
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_revoff", "BATCH-20260926-rv", defID)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, reviewScript(logPath, emitEmptyFindings, implArtifacts(defID)))
	t.Setenv("GROK_BUILD_BIN", bin)
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	if len(reviewerBlocks(argvBlocks(t, logPath))) != 0 {
		t.Fatal("REVIEW=0 spawned a reviewer")
	}
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("REVIEW=0 should resolve like before the reviewer: %+v %v", got, err)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.Review == nil || live.Review.Required || live.Review.SkippedReason != "REVIEW=0" {
		t.Fatalf("review = %+v", live.Review)
	}
	if !strings.Contains(jobLog(live), "[warn] [DefectDrainer] review: skipped (REVIEW=0)") {
		t.Fatalf("log:\n%s", jobLog(live))
	}
}

func TestReviewSkippedWhenNothingToReview(t *testing.T) {
	defID := "DEF-20260926-revnone-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	br, st := startReviewedJob(t, "bjob_revnone", defID, reviewScript(logPath, emitEmptyFindings, "exit 0\n"), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	if len(reviewerBlocks(argvBlocks(t, logPath))) != 0 {
		t.Fatal("reviewer ran with no diff and no fix-notes")
	}
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("no evidence resolved: %+v %v", got, err)
	}
	live := br.GetJob("bjob_revnone")
	if live == nil || live.Status != "completed" || live.Review == nil || !live.Review.Required || live.Review.OK || live.Review.SkippedReason != "no diff" {
		t.Fatalf("review = %+v status=%v", live.Review, live)
	}
	if !strings.Contains(jobLog(live), "review: skipped (no diff)") {
		t.Fatalf("log:\n%s", jobLog(live))
	}
}

func TestReviewSkippedWhenImplementerFails(t *testing.T) {
	defID := "DEF-20260926-revfail-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	impl := "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'notes' > fix-notes/" + defID + ".md\nexit 3\n"
	br, st := startReviewedJob(t, "bjob_revfail", defID, reviewScript(logPath, emitEmptyFindings, impl), nil)
	if len(reviewerBlocks(argvBlocks(t, logPath))) != 0 {
		t.Fatal("failed implementer spawned a reviewer")
	}
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" || len(got.FixEvidence) == 0 {
		t.Fatalf("failed run = %+v %v", got, err)
	}
	live := br.GetJob("bjob_revfail")
	if live == nil || live.Status != "failed" || live.Review == nil || live.Review.SkippedReason != "implementer failed" || live.Review.Required {
		t.Fatalf("job status=%s review=%+v", live.Status, live.Review)
	}
}

func TestReviewNotSpawnedWhenCancelled(t *testing.T) {
	defID := "DEF-20260926-revcancel-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	pid := filepath.Join(t.TempDir(), "pid")
	impl := "echo $$ > '" + pid + "'\nexec sleep 60\n"
	t.Setenv("DEFECT_DRAINER_REVIEW", "1")
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_revcancel", "BATCH-20260926-rv", defID)
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, reviewScript(logPath, emitEmptyFindings, impl))
	t.Setenv("GROK_BUILD_BIN", bin)
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitFile(t, pid, 5*time.Second)
	if _, err := br.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	if len(reviewerBlocks(argvBlocks(t, logPath))) != 0 {
		t.Fatal("cancelled job spawned a reviewer")
	}
	if live := br.GetJob(job.ID); live == nil || live.Status != "cancelled" {
		t.Fatalf("status = %+v", live)
	}
	assertProcessGone(t, pid)
}

func TestReviewNotSpawnedForTestRun(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv")
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	plan, err := store.UpsertPlan(st.DB, store.UpsertPlanInput{
		AppID: store.SeededTutoredWebappAppID, Key: "rev-skip", Title: "Review skip",
		Steps: []store.PlanStepIn{{Key: "login", Action: "sign in", Expected: "home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(st.DB, store.CreateRunInput{
		AppID: store.SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := br.CreateTestJob(store.SeededTutoredWebappAppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	br.mu.Lock()
	job.Worktrees = []git.WorktreeBinding{{
		Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "",
	}}
	job.Mode = "grok"
	br.mu.Unlock()
	impl := "wt=${DEFECT_DRAINER_WORKTREES%%:*}\nprintf 'dirty\\n' > \"$wt/dirty.txt\"\nmkdir -p results\nprintf 'STATUS: pass\\nhome\\n' > results/login.md\nprintf 'done\\n' > RUN.md\nexit 0\n"
	bin := filepath.Join(t.TempDir(), "fake-grok")
	writeExec(t, bin, reviewScript(logPath, emitEmptyFindings, impl))
	t.Setenv("GROK_BUILD_BIN", bin)
	t.Setenv("DEFECT_DRAINER_REVIEW", "1")
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })
	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)
	if len(reviewerBlocks(argvBlocks(t, logPath))) != 0 {
		t.Fatal("test-run job spawned a reviewer")
	}
	if _, err := os.Stat(filepath.Join(repo, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFakeReviewerChangeBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revsnap-0001"
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	head := gitRun(t, repo, "rev-parse", "HEAD")
	reviewer := `wt=$(sed -n 's/^- [^:]*: //p' REVIEW.md | head -1)
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
printf 'reviewer\n' > "$wt/kept.txt"
git -C "$wt" -c core.hooksPath=/dev/null -c commit.gpgsign=false add kept.txt
git -C "$wt" -c core.hooksPath=/dev/null -c commit.gpgsign=false commit -q -m reviewer
printf 'new\n' > "$wt/reviewer-new.txt"
mkdir -p "$wt/build"
printf 'x\n' > "$wt/build/extra"
` + emitPassingReview()
	impl := "wt=${DEFECT_DRAINER_WORKTREES%%:*}\n" +
		"printf 'implementer\\n' > \"$wt/kept.txt\"\n" +
		"printf 'impl-untracked\\n' > \"$wt/impl-untracked.txt\"\n" +
		"mkdir -p \"$wt/build\"\n" +
		"printf 'obj\\n' > \"$wt/build/out.o\"\n" +
		implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_revsnap", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	if gitRun(t, repo, "rev-parse", "HEAD") == head {
		t.Fatal("reviewer commit did not move HEAD")
	}
	got, err := os.ReadFile(filepath.Join(repo, "kept.txt"))
	if err != nil || string(got) != "reviewer\n" {
		t.Fatalf("kept.txt = %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(repo, "impl-untracked.txt"))
	if err != nil || string(got) != "impl-untracked\n" {
		t.Fatalf("untracked = %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(repo, "reviewer-new.txt"))
	if err != nil || string(got) != "new\n" {
		t.Fatalf("reviewer-new.txt = %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(repo, "build", "out.o"))
	if err != nil || string(got) != "obj\n" {
		t.Fatalf("ignored file = %q %v", got, err)
	}
	assertWorktreeChanged(t, br, st, "bjob_revsnap", defID, "demo: HEAD")
}

func TestStopDuringReviewKillsLens(t *testing.T) {
	defID := "DEF-20260926-revstop-0001"
	logPath := filepath.Join(t.TempDir(), "argv")
	pid := filepath.Join(t.TempDir(), "pid")
	reviewer := "echo $$ > '" + pid + "'\nexec sleep 60\n"
	br, _ := startReviewedJob(t, "bjob_revstop", defID, reviewScript(logPath, reviewer, implArtifacts(defID)), func(br *BatchRunner, job *Job) {
		reviewLensStartedHook = func(string) {
			reviewLensStartedHook = nil
			waitFile(t, pid, 5*time.Second)
			br.mu.Lock()
			_, ok := br.procs[job.ID]
			br.mu.Unlock()
			if !ok {
				t.Error("reviewer was not registered in procs")
			}
			if _, err := br.Stop(job.ID); err != nil {
				t.Error(err)
			}
		}
	})
	if n := len(reviewerBlocks(argvBlocks(t, logPath))); n != 1 {
		t.Fatalf("reviewer spawns after stop = %d", n)
	}
	live := br.GetJob("bjob_revstop")
	if live == nil || live.Status != "cancelled" {
		t.Fatalf("status = %+v", live)
	}
	assertProcessGone(t, pid)
}

func TestDeleteDuringReviewDoesNotHang(t *testing.T) {
	defID := "DEF-20260926-revdel-0001"
	pid := filepath.Join(t.TempDir(), "pid")
	reviewer := "echo $$ > '" + pid + "'\nexec sleep 60\n"
	deleted := make(chan error, 1)
	startReviewedJob(t, "bjob_revdel", defID, reviewScript("", reviewer, implArtifacts(defID)), func(br *BatchRunner, job *Job) {
		reviewLensStartedHook = func(string) {
			reviewLensStartedHook = nil
			waitFile(t, pid, 5*time.Second)
			go func() {
				_, err := br.DeleteJob(job.ID)
				deleted <- err
			}()
		}
	})
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("DeleteJob hung during a review lens")
	}
	assertProcessGone(t, pid)
}

func TestRestartRefusedWhileReviewerRuns(t *testing.T) {
	defID := "DEF-20260926-revlive-0001"
	t.Setenv("DEFECT_DRAINER_REVIEW_MAX_WALL_SEC", "1")
	errc := make(chan error, 1)
	startReviewedJob(t, "bjob_revlive", defID, reviewScript("", "exec sleep 30\n", implArtifacts(defID)), func(br *BatchRunner, job *Job) {
		job.Mode = "grok"
		reviewLensStartedHook = func(string) {
			reviewLensStartedHook = nil
			br.mu.Lock()
			_, ok := br.procs[job.ID]
			br.mu.Unlock()
			if !ok {
				errc <- errors.New("reviewer was not registered in procs")
				return
			}
			// defectsRoot is unused when the job is live; ErrJobLive returns first.
			_, err := br.Restart(job.ID, t.TempDir(), false)
			if !errors.Is(err, ErrJobLive) {
				errc <- err
				return
			}
			errc <- nil
		}
	})
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("restart hook did not run")
	}
}

func TestReviewPersistedAfterEachLens(t *testing.T) {
	defID := "DEF-20260926-revpers-0001"
	var seen []string
	br, _ := startReviewedJob(t, "bjob_revpers", defID, reviewScript("", emitPassingReview(), implArtifacts(defID)), func(br *BatchRunner, job *Job) {
		reviewLensFinishedHook = func(lens string) {
			raw, err := os.ReadFile(filepath.Join(br.dataRoot, "batch-jobs", job.ID, "job.json"))
			if err != nil {
				t.Errorf("read job.json: %v", err)
				return
			}
			var got Job
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("job.json: %v", err)
				return
			}
			if got.Review == nil {
				t.Errorf("lens %s: review missing on disk", lens)
				return
			}
			seen = append(seen, fmt.Sprintf("%s:%d", lens, len(got.Review.Lenses)))
		}
	})
	if strings.Join(seen, ",") != "spec:1,security:2,code:3,tests:4" {
		t.Fatalf("persisted = %v", seen)
	}
	live := br.GetJob("bjob_revpers")
	if live == nil || live.Review == nil || len(live.Review.Lenses) != 4 || !live.Review.OK {
		t.Fatalf("final review = %+v", live.Review)
	}
}

func assertProcessGone(t *testing.T, pidfile string) {
	t.Helper()
	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReviewRejectsInvalidRows(t *testing.T) {
	defID := "DEF-20260926-revrow-0001"
	valid := func(over map[string]any) string {
		row := map[string]any{
			"severity": "low", "lens": "spec", "defect_id": defID,
			"location": "demo:file.go:1", "finding": "a note", "reasoning": "because",
		}
		for k, v := range over {
			row[k] = v
		}
		b, err := json.Marshal(map[string]any{"ok": true, "findings": []any{row}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		name string
		text string
		rule string
	}{
		{"severity", valid(map[string]any{"severity": "nope"}), "findings[0]: severity"},
		{"lens", valid(map[string]any{"lens": "docs"}), "findings[0]: lens"},
		{"defect", valid(map[string]any{"defect_id": "DEF-not-in-this-job"}), "findings[0]: defect_id"},
		{"backtick", valid(map[string]any{"location": "a`b"}), "findings[0]: location"},
		{"newline", valid(map[string]any{"location": "a\nb"}), "findings[0]: location"},
		{"empty", valid(map[string]any{"finding": "  "}), "findings[0]: finding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lens := parseLensText("spec", "preamble\n"+tc.text+"\nthanks", false, []string{defID})
			if lens.OK || !strings.Contains(lens.Error, "spec") || !strings.Contains(lens.Error, tc.rule) {
				t.Fatalf("lens = %+v", lens)
			}
			if len(lens.Findings) != 0 {
				t.Fatalf("bad row was kept: %+v", lens.Findings)
			}
			status, outcome, images := harvestReviewed(t, defID, &ReviewRun{Required: true, Lenses: []ReviewLens{lens}})
			if status == "resolved" || outcome != harvestReviewBlocked || images == 0 {
				t.Fatalf("status=%s outcome=%s images=%d", status, outcome, images)
			}
		})
	}
}

func TestReviewTooManyFindingsBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revmany-0001"
	rows := make([]map[string]string, 101)
	for i := range rows {
		rows[i] = map[string]string{"severity": "low", "lens": "spec", "finding": fmt.Sprintf("f%d", i)}
	}
	raw, err := json.Marshal(map[string]any{"findings": rows})
	if err != nil {
		t.Fatal(err)
	}
	lens := parseLensText("spec", string(raw), false, []string{defID})
	if lens.OK || !strings.Contains(lens.Error, "too many findings") || len(lens.Findings) != 0 {
		t.Fatalf("lens = %+v", lens)
	}
	status, outcome, images := harvestReviewed(t, defID, &ReviewRun{Required: true, Error: lens.Error, Lenses: []ReviewLens{lens}})
	if status == "resolved" || outcome != harvestReviewBlocked || images == 0 {
		t.Fatalf("status=%s outcome=%s images=%d", status, outcome, images)
	}

	rows = rows[:100]
	raw, err = json.Marshal(map[string]any{"findings": rows})
	if err != nil {
		t.Fatal(err)
	}
	ok := parseLensText("spec", string(raw), false, []string{defID})
	if !ok.OK || len(ok.Findings) != 100 {
		t.Fatalf("100 medium findings should stand: %+v", ok)
	}
	status, outcome, images = harvestReviewed(t, defID, &ReviewRun{Required: true, OK: true, Lenses: []ReviewLens{ok}})
	if status != "resolved" || outcome != harvestResolved || images == 0 {
		t.Fatalf("medium findings must not block resolve: status=%s outcome=%s images=%d", status, outcome, images)
	}
}

func TestReviewModelOKDoesNotResolve(t *testing.T) {
	defID := "DEF-20260926-revok-0001"
	text := `{"ok":true,"findings":[{"severity":"critical","lens":"spec","finding":"boom","location":"demo:a.go:1"}]}`
	lens := parseLensText("spec", "model says\n"+text, false, []string{defID})
	if lens.OK || len(lens.Findings) != 1 || lens.Findings[0].Severity != "critical" {
		t.Fatalf("model ok was honored: %+v", lens)
	}
	rev := &ReviewRun{Required: true, Lenses: []ReviewLens{lens}}
	rev.finish()
	if rev.OK {
		t.Fatal("review.ok followed the model")
	}
	status, outcome, images := harvestReviewed(t, defID, rev)
	if status == "resolved" || outcome != harvestReviewBlocked || images == 0 {
		t.Fatalf("status=%s outcome=%s images=%d", status, outcome, images)
	}
}

func TestReviewLastObjectWins(t *testing.T) {
	defID := "DEF-20260926-revlast-0001"
	text := "draft {\"findings\":[{\"severity\":\"critical\",\"lens\":\"spec\",\"finding\":\"ignore me\"}]}\nfinal {\"findings\":[]}"
	lens := parseLensText("spec", text, false, []string{defID})
	if !lens.OK || len(lens.Findings) != 0 {
		t.Fatalf("last object did not win: %+v", lens)
	}
	missing := parseLensText("spec", "no object here", false, []string{defID})
	if missing.OK || !strings.Contains(missing.Error, "missing") {
		t.Fatalf("missing = %+v", missing)
	}
}

func harvestReviewed(t *testing.T, defID string, rev *ReviewRun) (status, outcome string, images int) {
	t.Helper()
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	writeInProgressDefect(t, st, defID)
	job := &Job{
		ID: "bjob_revgate", Kind: "batch", Status: "running",
		BatchID: "BATCH-20260926-rv", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{defID},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	img := filepath.Join(handoff, "fix-evidence", defID)
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "fix-notes", defID+".md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rev != nil && !rev.OK {
		rev.finish()
	}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true, Review: rev})
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	live := br.GetJob(job.ID)
	if live == nil || len(live.Harvests) != 1 || len(live.Harvests[0].Defects) != 1 {
		t.Fatalf("harvests = %+v", live)
	}
	return got.Status, live.Harvests[0].Defects[0].Outcome, len(got.FixEvidence)
}

func TestReviewJSONRoundTripAndClone(t *testing.T) {
	j := &Job{
		ID: "bjob_revrt", Status: "completed",
		Review: &ReviewRun{
			OK: true, Required: true, TotalCostUSD: 1.25,
			Lenses: []ReviewLens{{
				Lens: "spec", OK: true, Turns: 2, CostUSD: 1.25, SessionID: "sess",
				Findings: []ReviewFinding{{
					Severity: "low", Lens: "spec", Finding: "nit", Location: "a.go:1",
				}},
			}},
		},
		Extra: map[string]any{"custom": "keep"},
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Extra["custom"] != "keep" {
		t.Fatalf("extra dropped: %+v", got.Extra)
	}
	if got.Review == nil || !got.Review.OK || !got.Review.Required || got.Review.TotalCostUSD != 1.25 {
		t.Fatalf("review = %+v\n%s", got.Review, raw)
	}
	if len(got.Review.Lenses) != 1 || got.Review.Lenses[0].SessionID != "sess" || got.Review.Lenses[0].Findings[0].Finding != "nit" {
		t.Fatalf("lenses = %+v", got.Review.Lenses)
	}
	c := cloneJob(j)
	c.Review.Lenses[0].Findings[0].Finding = "changed"
	c.Review.Lenses = append(c.Review.Lenses, ReviewLens{Lens: "code"})
	if j.Review.Lenses[0].Findings[0].Finding != "nit" || len(j.Review.Lenses) != 1 {
		t.Fatal("cloneJob shares review slices")
	}
}

func TestManifestRefusesPrimary(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	marker := filepath.Join(dir, "kept.txt")
	if err := os.WriteFile(marker, []byte("primary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := captureWorktreeManifests([]git.WorktreeBinding{{
		Repo: "demo", WorktreeAbs: dir, PrimaryAbs: dir,
	}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("err = %v", err)
	}
	got, rerr := os.ReadFile(marker)
	if rerr != nil || string(got) != "primary\n" {
		t.Fatalf("primary bytes = %q %v", got, rerr)
	}
}

func TestReviewReadEvidence(t *testing.T) {
	wt := t.TempDir()
	real, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: wt, PrimaryAbs: t.TempDir()}}
	call := func(cmd, id, status string) []byte {
		ev := map[string]any{
			"type": "tool_call", "toolCallId": id, "toolName": "run_terminal_command",
			"rawInput": map[string]string{"command": cmd, "description": "diff"},
		}
		if status != "" {
			ev = map[string]any{"type": "tool_call_update", "toolCallId": id, "status": status}
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	diffCmd := "git -C " + real + " diff"
	withDone := [][]byte{call("git -C "+wt+" diff", "c1", ""), call("", "c1", "completed")}
	if !readEvidence(withDone, wts) {
		t.Fatal("completed git diff should count")
	}
	// The command names only the EvalSymlinks form.
	if real != wt && !readEvidence([][]byte{call(diffCmd, "c2", ""), call("", "c2", "completed")}, wts) {
		t.Fatal("private/var form of the worktree path should count")
	}
	if readEvidence([][]byte{call("git -C "+wt+" diff", "c3", "")}, wts) {
		t.Fatal("a diff call without a completed update counted")
	}
	readFile := map[string]any{
		"type": "tool_call", "toolCallId": "r1", "toolName": "read_file",
		"rawInput": map[string]string{"command": "git -C " + wt + " diff"},
	}
	raw, _ := json.Marshal(readFile)
	done, _ := json.Marshal(map[string]any{"type": "tool_call_update", "toolCallId": "r1", "status": "completed"})
	if readEvidence([][]byte{raw, done}, wts) {
		t.Fatal("read_file counted as diff evidence")
	}
}

func TestReviewNothingToReviewDoesNotResolve(t *testing.T) {
	defID := "DEF-20260926-revempty-0001"
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	impl := "mkdir -p fix-evidence/" + defID + "\nprintf 'png' > fix-evidence/" + defID + "/fix-01.png\nexit 0\n"
	br, st := startReviewedJob(t, "bjob_revempty", defID, reviewScript("", emitEmptyFindings, impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatal("a screenshot with no diff and no fix-notes resolved")
	}
	if len(got.FixEvidence) == 0 {
		t.Fatal("evidence should still be imported")
	}
	live := br.GetJob("bjob_revempty")
	if live == nil || len(live.Harvests) != 1 || len(live.Harvests[0].Defects) != 1 {
		t.Fatalf("harvests = %+v", live)
	}
	h := live.Harvests[0].Defects[0]
	if h.Outcome != harvestReviewBlocked || !strings.Contains(h.Reason, "nothing to review") {
		t.Fatalf("harvest = %+v", h)
	}
}

func TestHarvestHonoursUnfixedMarker(t *testing.T) {
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	short := "DEF-1"
	longer := "DEF-10"
	for _, id := range []string{short, longer} {
		writeInProgressDefect(t, st, id)
	}
	job := &Job{
		ID: "bjob_unfixed", Kind: "batch", Status: "running",
		BatchID: "BATCH-20260926-uf", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{short, longer},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	for _, id := range []string{short, longer} {
		img := filepath.Join(handoff, "fix-evidence", id)
		if err := os.MkdirAll(img, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(img, "fix-01.png"), []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(handoff, "NOTES.md"), []byte("UNFIXED: DEF-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	gotShort, err := st.Get(short)
	if err != nil || gotShort == nil || gotShort.Status == "resolved" || len(gotShort.FixEvidence) == 0 {
		t.Fatalf("DEF-1 = %+v %v", gotShort, err)
	}
	gotLong, err := st.Get(longer)
	if err != nil || gotLong == nil || gotLong.Status != "resolved" {
		t.Fatalf("DEF-10 should resolve: %+v %v", gotLong, err)
	}
	live := br.GetJob(job.ID)
	for _, d := range live.Harvests[0].Defects {
		if d.DefectID == short && (d.Outcome != harvestNotResolved || !strings.Contains(d.Reason, "UNFIXED")) {
			t.Fatalf("short harvest = %+v", d)
		}
	}
}

func TestReviewRefusesSwappedWorktree(t *testing.T) {
	defID := "DEF-20260926-revswap-0001"
	marker := filepath.Join(t.TempDir(), "scratch-marker")
	scratch := t.TempDir()
	gitRun(t, scratch, "init", "-q")
	if err := os.WriteFile(filepath.Join(scratch, "precious"), []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, scratch, "add", "precious")
	gitRun(t, scratch, "commit", "-q", "-m", "base")
	head := gitRun(t, scratch, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(scratch, "precious"), []byte("safe\ndirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, scratch, "config", "filter.x.clean", "touch "+marker)
	orig := t.TempDir()
	gitRun(t, orig, "init", "-q")
	gitRun(t, orig, "commit", "-q", "--allow-empty", "-m", "base")
	impl := "wt=${DEFECT_DRAINER_WORKTREES%%:*}\nrm -rf \"$wt\"\nln -s '" + scratch + "' \"$wt\"\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_revswap", defID, reviewScript("", emitEmptyFindings, impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: orig, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("server git ran in the swapped scratch repo")
	}
	if gitRun(t, scratch, "rev-parse", "HEAD") != head {
		t.Fatal("scratch HEAD moved")
	}
	got, err := os.ReadFile(filepath.Join(scratch, "precious"))
	if err != nil || string(got) != "safe\ndirty\n" {
		t.Fatalf("precious = %q %v", got, err)
	}
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status == "resolved" {
		t.Fatalf("swapped worktree resolved: %+v %v\n%s", rec, err, jobLog(br.GetJob("bjob_revswap")))
	}
}

func TestReviewIgnoresRewrittenGitdir(t *testing.T) {
	defID := "DEF-20260926-revgit-0001"
	marker := filepath.Join(t.TempDir(), "filter-marker")
	primary := t.TempDir()
	gitRun(t, primary, "init", "-q")
	if err := os.WriteFile(filepath.Join(primary, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, primary, "add", "f")
	gitRun(t, primary, "commit", "-q", "-m", "base")
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, primary, "worktree", "add", "--detach", wt, "HEAD")
	impl := "wt=${DEFECT_DRAINER_WORKTREES%%:*}\n" +
		"fake=$(dirname \"$wt\")/fake\n" +
		"git init -q \"$fake\"\n" +
		"git -C \"$fake\" config user.email t@t\n" +
		"git -C \"$fake\" config user.name t\n" +
		"git -C \"$fake\" config filter.x.clean 'touch " + marker + "'\n" +
		"git -C \"$fake\" commit -q --allow-empty -m base\n" +
		"printf 'gitdir: %s/.git\\n' \"$fake\" > \"$wt/.git\"\n" +
		"printf '* filter=x\\n' > \"$wt/.gitattributes\"\n" +
		"printf 'dirty\\n' >> \"$wt/f\"\n" +
		implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_revgit", defID, reviewScript("", emitEmptyFindings, impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: wt, PrimaryAbs: primary, Branch: "",
		}}
	})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("clean filter in the fake git dir ran")
	}
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status == "resolved" || len(rec.FixEvidence) == 0 {
		t.Fatalf("rewritten gitdir: %+v %v\n%s", rec, err, jobLog(br.GetJob("bjob_revgit")))
	}
}

func TestDeleteDuringReviewDoesNotRecreateBranch(t *testing.T) {
	defID := "DEF-20260926-revbr-0001"
	primary := t.TempDir()
	gitRun(t, primary, "init", "-q")
	gitRun(t, primary, "commit", "-q", "--allow-empty", "-m", "base")
	base := gitRun(t, primary, "rev-parse", "--abbrev-ref", "HEAD")
	branch := "defect-drainer/h3r6"
	gitRun(t, primary, "branch", branch)
	var br *BatchRunner
	var wt string
	deleted := make(chan error, 1)
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	startReviewedJob(t, "bjob_revbr", defID, reviewScript("", "exec sleep 60\n", impl), func(runner *BatchRunner, job *Job) {
		br = runner
		handoff := runner.handoffDir(job.ID)
		wt = filepath.Join(handoff, "worktrees", "demo")
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, primary, "worktree", "add", wt, branch)
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: wt, PrimaryAbs: primary, Branch: branch,
			BaseBranch: base, BaseRef: base,
		}}
		reviewLensStartedHook = func(string) {
			reviewLensStartedHook = nil
			go func() {
				_, err := runner.DeleteJob(job.ID)
				deleted <- err
			}()
		}
	})
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("DeleteJob hung during review")
	}
	if err := exec.Command("git", "-C", primary, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run(); err == nil {
		t.Fatal("job branch was recreated after delete")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree directory recreated: %v", err)
	}
	_ = br
}

func TestReviewWallKillsProcessGroup(t *testing.T) {
	defID := "DEF-20260926-revpg-0001"
	pid := filepath.Join(t.TempDir(), "child-pid")
	t.Setenv("DEFECT_DRAINER_REVIEW_MAX_WALL_SEC", "1")
	reviewer := "sleep 60 &\necho $! > '" + pid + "'\nwait\n"
	startReviewedJob(t, "bjob_revpg", defID, reviewScript("", reviewer, implArtifacts(defID)), nil)
	assertProcessGone(t, pid)
}

func TestReviewToolDeniedBlocksResolve(t *testing.T) {
	defID := "DEF-20260926-revdeny-0001"
	denial := `{"type":"tool_call_update","toolCallId":"t1","content":[{"type":"text","text":"User cancelled the execution for tool ` + "`run_terminal_command`" + `"}]}`
	reviewer := "printf '%s\\n' '" + denial + "'\n" +
		"printf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"cancelled\",\"sessionId\":\"rev\",\"num_turns\":1,\"total_cost_usd\":0}'\n" +
		"exit 0\n"
	br, st := startReviewedJob(t, "bjob_revdeny", defID, reviewScript("", reviewer, implArtifacts(defID)), nil)
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("tool denial resolved: %+v %v", got, err)
	}
	live := br.GetJob("bjob_revdeny")
	if live == nil || live.Review == nil || live.Review.OK || len(live.Review.Lenses) != 4 {
		t.Fatalf("review = %+v", live.Review)
	}
	for _, l := range live.Review.Lenses {
		if !strings.HasPrefix(l.Error, "tool-denied:") || !strings.Contains(l.Error, "run_terminal_command") {
			t.Fatalf("lens %s error = %q", l.Lens, l.Error)
		}
	}
	if !strings.Contains(jobLog(live), "[error]") || !strings.Contains(jobLog(live), "tool-denied: run_terminal_command") {
		t.Fatalf("log = %s", jobLog(live))
	}
	md, err := os.ReadFile(filepath.Join(br.handoffDir("bjob_revdeny"), "review", "spec", "REVIEW.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(md)
	for _, want := range []string{
		"Review by reading only",
		"git diff", "git status", "git log", "git show",
		"Do not create repositories",
		"ends your review and blocks the fix",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("REVIEW.md missing %q", want)
		}
	}
}

func TestReviewOutputStopsAfterCancel(t *testing.T) {
	defID := "DEF-20260926-revout-0001"
	gate := filepath.Join(t.TempDir(), "go")
	const marker = "REVIEWER-OUTPUT-AFTER-CANCEL"
	reviewer := "while [ ! -f '" + gate + "' ]; do sleep 0.02; done\n" +
		"echo " + marker + "\n" +
		"printf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":\"s\",\"num_turns\":1,\"total_cost_usd\":0}'\n" +
		"exit 0\n"
	var snap []byte
	br, _ := startReviewedJob(t, "bjob_revout", defID, reviewScript("", reviewer, implArtifacts(defID)), func(br *BatchRunner, job *Job) {
		reviewLensStartedHook = func(string) {
			reviewLensStartedHook = nil
			br.mu.Lock()
			if live := br.jobs[job.ID]; live != nil {
				live.Status = "cancelled"
			}
			br.mu.Unlock()
			if err := br.persistLive(job.ID); err != nil {
				t.Error(err)
			}
			b, err := os.ReadFile(filepath.Join(br.handoffDir(job.ID), "job.json"))
			if err != nil {
				t.Error(err)
				return
			}
			snap = append([]byte(nil), b...)
			if err := os.WriteFile(gate, []byte("x"), 0o644); err != nil {
				t.Error(err)
			}
		}
	})
	got, err := os.ReadFile(filepath.Join(br.handoffDir("bjob_revout"), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), marker) || string(got) != string(snap) {
		t.Fatalf("reviewer output persisted after cancel")
	}
}

func TestNoDiffDoesNotResolve(t *testing.T) {
	// (a) empty fix-notes for this defect.
	assertNoDiffBlocked(t, "bjob_nodiff_a", "DEF-20260926-nodiff-0001", func(defID string) string {
		return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
			"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
			": > fix-notes/" + defID + ".md\nexit 0\n"
	}, nil)
	// (b) same tree, reviewer switched off.
	assertNoDiffBlocked(t, "bjob_nodiff_b", "DEF-20260926-nodiff-0002", func(defID string) string {
		return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
			"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
			": > fix-notes/" + defID + ".md\nexit 0\n"
	}, func(t *testing.T) { t.Setenv("DEFECT_DRAINER_REVIEW", "0") })
	// (c) a note for a different defect.
	assertNoDiffBlocked(t, "bjob_nodiff_c", "DEF-20260926-nodiff-0003", func(defID string) string {
		return "mkdir -p fix-evidence/" + defID + " fix-notes\n" +
			"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
			"printf 'other\\n' > fix-notes/DEF-20260926-other-0009.md\nexit 0\n"
	}, nil)
}

func assertNoDiffBlocked(t *testing.T, jobID, defID string, impl func(string) string, mutateEnv func(t *testing.T)) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	br, st := startReviewedJob(t, jobID, defID, reviewScript("", emitPassingReview(), impl(defID)), func(_ *BatchRunner, job *Job) {
		if mutateEnv != nil {
			mutateEnv(t)
		}
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" || len(got.FixEvidence) == 0 {
		t.Fatalf("%s resolved or lost evidence: %+v %v\n%s", defID, got, err, jobLog(br.GetJob(jobID)))
	}
	live := br.GetJob(jobID)
	if live == nil || live.Review == nil || !live.Review.Required || live.Review.OK || !strings.Contains(live.Review.Error, "nothing to review") {
		t.Fatalf("%s review = %+v", defID, live.Review)
	}
}

func TestDiffStillResolves(t *testing.T) {
	defID := "DEF-20260926-yesdiff-0001"
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	_, st := startReviewedJob(t, "bjob_yesdiff", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("real diff did not resolve: %+v %v", got, err)
	}
}

func TestReviewDetectsEditedFile(t *testing.T) {
	defID := "DEF-20260926-mani-a-0001"
	repo := initTrackedRepo(t)
	reviewer := "printf 'reviewer\\n' > '" + repo + "/f'\n" + emitPassingReview()
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_mania", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	got, err := os.ReadFile(filepath.Join(repo, "f"))
	if err != nil || string(got) != "reviewer\n" {
		t.Fatalf("f = %q %v", got, err)
	}
	assertWorktreeChanged(t, br, st, "bjob_mania", defID, "demo: f")
}

func TestReviewDetectsCommit(t *testing.T) {
	defID := "DEF-20260926-mani-b-0001"
	repo := initTrackedRepo(t)
	reviewer := "git -C '" + repo + "' -c core.hooksPath=/dev/null -c user.email=t@t -c user.name=t commit --allow-empty -q -m r\n" + emitPassingReview()
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_manib", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	assertWorktreeChanged(t, br, st, "bjob_manib", defID, "demo: HEAD")
}

func TestReviewDetectsUntrackedFile(t *testing.T) {
	defID := "DEF-20260926-mani-c-0001"
	repo := initTrackedRepo(t)
	reviewer := "printf 'new\\n' > '" + repo + "/extra.txt'\n" + emitPassingReview()
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_manic", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	got, err := os.ReadFile(filepath.Join(repo, "extra.txt"))
	if err != nil || string(got) != "new\n" {
		t.Fatalf("extra.txt = %q %v", got, err)
	}
	assertWorktreeChanged(t, br, st, "bjob_manic", defID, "demo: extra.txt")
}

func TestReviewIgnoredDirStillResolves(t *testing.T) {
	defID := "DEF-20260926-mani-d-0001"
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitignore", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	reviewer := "mkdir -p '" + repo + "/build'\nprintf 'obj\\n' > '" + repo + "/build/out.o'\n" + emitPassingReview()
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	_, st := startReviewedJob(t, "bjob_manid", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	got, err := os.ReadFile(filepath.Join(repo, "build", "out.o"))
	if err != nil || string(got) != "obj\n" {
		t.Fatalf("ignored file = %q %v", got, err)
	}
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status != "resolved" {
		t.Fatalf("ignored change blocked resolve: %+v %v", rec, err)
	}
}

func TestReviewDetectsSymlinkSwap(t *testing.T) {
	defID := "DEF-20260926-mani-e-0001"
	repo := initTrackedRepo(t)
	scratch := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(scratch, []byte("target-bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reviewer := "rm -f '" + repo + "/f'\nln -s '" + scratch + "' '" + repo + "/f'\n" + emitPassingReview()
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_manie", defID, reviewScript("", reviewer, impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	got, err := os.ReadFile(scratch)
	if err != nil || string(got) != "target-bytes\n" {
		t.Fatalf("symlink target = %q %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(repo, "f"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("f is not a symlink: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(repo, "f")); err != nil || link != scratch {
		t.Fatalf("link = %q %v", link, err)
	}
	assertWorktreeChanged(t, br, st, "bjob_manie", defID, "demo: f")
}

func initTrackedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	return repo
}

func bindReviewRepo(t *testing.T, job *Job, repo string) {
	t.Helper()
	job.Worktrees = []git.WorktreeBinding{{
		Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
	}}
}

func assertWorktreeChanged(t *testing.T, br *BatchRunner, st *store.Store, jobID, defID, want string) {
	t.Helper()
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status == "resolved" {
		t.Fatalf("changed worktree resolved: %+v %v", rec, err)
	}
	live := br.GetJob(jobID)
	if live == nil || live.Review == nil || live.Review.OK || !strings.Contains(live.Review.Error, "worktree changed during review: "+want) {
		t.Fatalf("review = %+v\n%s", live.Review, jobLog(live))
	}
	if !strings.Contains(jobLog(live), "[error]") || !strings.Contains(jobLog(live), "worktree changed during review: "+want) {
		t.Fatalf("log = %s", jobLog(live))
	}
}

func TestReviewIgnoresSeededHooks(t *testing.T) {
	defID := "DEF-20260926-hooks-0001"
	marker := filepath.Join(t.TempDir(), "hook-marker")
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	impl := "mkdir -p review-hooks\n" +
		"printf '%s\\n' '#!/bin/sh' 'touch " + marker + "' 'exit 0' > review-hooks/reference-transaction\n" +
		"cp review-hooks/reference-transaction review-hooks/post-index-change\n" +
		"chmod +x review-hooks/reference-transaction review-hooks/post-index-change\n" +
		"printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" +
		implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_hooks", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
	})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("seeded hook ran")
	}
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("hooks run did not finish: %+v %v\n%s", got, err, jobLog(br.GetJob("bjob_hooks")))
	}
}

func TestImplementerGroupDeadBeforeSnapshot(t *testing.T) {
	defID := "DEF-20260926-pgid-0001"
	pidfile := filepath.Join(t.TempDir(), "child-pid")
	t.Cleanup(func() { killPidFile(pidfile) })
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	impl := "sleep 600 &\necho $! > '" + pidfile + "'\n" +
		"printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\nexit 0\n"
	sawSnapshot := false
	startReviewedJob(t, "bjob_pgid", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{{
			Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/x",
		}}
		reviewBeforeSnapshotHook = func() {
			sawSnapshot = true
			assertProcessGone(t, pidfile)
		}
	})
	if !sawSnapshot {
		t.Fatal("manifest capture did not run")
	}
}

func killPidFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func TestReviewIgnoresGlobalFilter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "global-marker")
	home := t.TempDir()
	script := writeCleanFilter(t, marker)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(
		"[filter \"x\"]\n\tclean = "+script+"\n\trequired = true\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filterRepo(t)
	t.Setenv("HOME", home)
	assertNoDiffFilterQuiet(t, repo, marker)
}

func TestReviewIgnoresLocalFilter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "local-marker")
	script := writeCleanFilter(t, marker)
	repo := filterRepo(t)
	gitRun(t, repo, "config", "filter.x.clean", script)
	gitRun(t, repo, "config", "filter.x.required", "true")
	assertNoDiffFilterQuiet(t, repo, marker)
}

func TestReviewNoDiffIgnoresIncludedFilter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "include-marker")
	script := writeCleanFilter(t, marker)
	repo := filterRepo(t)
	inc := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(inc, []byte(
		"[filter \"x\"]\n\tclean = "+script+"\n\trequired = true\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "config", "include.path", inc)
	assertNoDiffFilterQuiet(t, repo, marker)
}

func TestReviewNoDiffIgnoresDottedFilter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "dotted-marker")
	script := writeCleanFilter(t, marker)
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	gitRun(t, repo, "config", "filter.a.b.clean", script)
	gitRun(t, repo, "config", "filter.a.b.required", "true")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=a.b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertNoDiffFilterQuiet(t, repo, marker)
}

func assertNoDiffFilterQuiet(t *testing.T, repo, marker string) {
	t.Helper()
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir()}}
	dirs := snapshotGitDirs(wts)
	if dirs["demo"] == "" {
		t.Fatal("no pinned git dir")
	}
	base := gitRun(t, repo, "rev-parse", "HEAD")
	if !worktreeHasDiff(repo, base, dirs["demo"]) {
		t.Fatal("dirty tree reported no diff")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("clean filter ran during the no-diff check")
	}
}

func writeCleanFilter(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clean")
	body := "#!/bin/sh\ntouch '" + marker + "'\ncat\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func filterRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestReviewMissingGitDirBlocksResolve(t *testing.T) {
	defID := "DEF-20260927-manigit-0001"
	good := initTrackedRepo(t)
	bad := initTrackedRepo(t)
	impl := "rm -rf '" + bad + "/.git'\nprintf 'dirty\\n' >> '" + good + "/f'\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_manigit", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{
			{Repo: "good", WorktreeAbs: good, PrimaryAbs: t.TempDir(), Branch: "fix/g"},
			{Repo: "missing-git", WorktreeAbs: bad, PrimaryAbs: t.TempDir(), Branch: "fix/b"},
		}
	})
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status == "resolved" {
		t.Fatalf("missing .git resolved: %+v %v", rec, err)
	}
	live := br.GetJob("bjob_manigit")
	if live == nil || live.Review == nil || live.Review.OK || len(live.Review.Lenses) != 0 || !strings.Contains(live.Review.Error, "missing-git") {
		t.Fatalf("review = %+v", live.Review)
	}
}

func TestReviewEmptyWorktreePathBlocksResolve(t *testing.T) {
	defID := "DEF-20260927-manipath-0001"
	repo := initTrackedRepo(t)
	impl := "printf 'dirty\\n' >> '" + repo + "/f'\n" + implArtifacts(defID)
	br, st := startReviewedJob(t, "bjob_manipath", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		job.Worktrees = []git.WorktreeBinding{
			{Repo: "good", WorktreeAbs: repo, PrimaryAbs: t.TempDir(), Branch: "fix/g"},
			{Repo: "no-path", WorktreeAbs: "", PrimaryAbs: t.TempDir(), Branch: "fix/e"},
		}
	})
	rec, err := st.Get(defID)
	if err != nil || rec == nil || rec.Status == "resolved" {
		t.Fatalf("empty path resolved: %+v %v", rec, err)
	}
	live := br.GetJob("bjob_manipath")
	if live == nil || live.Review == nil || live.Review.OK || !strings.Contains(live.Review.Error, "no-path") {
		t.Fatalf("review = %+v", live.Review)
	}
}

func TestManifestDiffReportsRepoMissingAfter(t *testing.T) {
	before := []wtManifest{{repo: "demo", head: "abc"}, {repo: "other", head: "def"}}
	after := []wtManifest{{repo: "demo", head: "abc"}}
	if got := diffManifestLists(before, after); got != "worktree changed during review: other: HEAD" {
		t.Fatalf("absent after: %q", got)
	}
	if got := diffManifestLists(after, before); got != "worktree changed during review: other: HEAD" {
		t.Fatalf("absent before: %q", got)
	}
}

func TestOversizedNotesBlockResolve(t *testing.T) {
	defID := "DEF-20260927-notesbig-0001"
	br, st, handoff := harvestNotesJob(t, "bjob_notesbig", defID)
	f, err := os.Create(filepath.Join(handoff, "NOTES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("UNFIXED: " + defID + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(harvestFileLimit + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	assertDeclineUnreadable(t, br, st, "bjob_notesbig", defID, handoff)
}

func TestSymlinkNotesBlockResolve(t *testing.T) {
	defID := "DEF-20260927-noteslink-0001"
	br, st, handoff := harvestNotesJob(t, "bjob_noteslink", defID)
	target := filepath.Join(t.TempDir(), "notes-target")
	if err := os.WriteFile(target, []byte("UNFIXED: "+defID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(handoff, "NOTES.md")); err != nil {
		t.Fatal(err)
	}
	assertDeclineUnreadable(t, br, st, "bjob_noteslink", defID, handoff)
}

func TestNoNotesStillResolves(t *testing.T) {
	defID := "DEF-20260927-notesnone-0001"
	repo := initTrackedRepo(t)
	impl := "printf 'dirty\\n' >> \"${DEFECT_DRAINER_WORKTREES%%:*}/f\"\n" +
		"mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'fixed it' > fix-notes/" + defID + ".md\n" +
		"exit 0\n"
	_, st := startReviewedJob(t, "bjob_notesnone", defID, reviewScript("", emitPassingReview(), impl), func(_ *BatchRunner, job *Job) {
		bindReviewRepo(t, job, repo)
	})
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("missing NOTES.md did not resolve: %+v %v", got, err)
	}
}

func harvestNotesJob(t *testing.T, jobID, defID string) (*BatchRunner, *store.Store, string) {
	t.Helper()
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	writeInProgressDefect(t, st, defID)
	job := &Job{
		ID: jobID, Kind: "batch", Status: "running",
		BatchID: "BATCH-20260927-notes", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: []string{defID},
	}
	br.jobs[job.ID] = job
	handoff := filepath.Join(data, "batch-jobs", job.ID)
	img := filepath.Join(handoff, "fix-evidence", defID)
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	return br, st, handoff
}

func assertDeclineUnreadable(t *testing.T, br *BatchRunner, st *store.Store, jobID, defID, handoff string) {
	t.Helper()
	br.harvestFixEvidence(br.GetJob(jobID), handoff, &HarvestOpts{AllowResolve: true})
	got, err := st.Get(defID)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("unreadable NOTES.md resolved: %+v %v", got, err)
	}
	live := br.GetJob(jobID)
	if live == nil || len(live.Harvests) != 1 || len(live.Harvests[0].Defects) != 1 {
		t.Fatalf("harvests = %+v", live)
	}
	h := live.Harvests[0].Defects[0]
	if h.Outcome != harvestNotResolved || !strings.Contains(h.Reason, "decline file could not be read") {
		t.Fatalf("harvest = %+v", h)
	}
}

func TestManifestByteCapDoesNotReadOversizedFile(t *testing.T) {
	oldCap := manifestByteCap
	manifestByteCap = 64
	t.Cleanup(func() { manifestByteCap = oldCap })
	var read int64
	oldOpen := manifestOpen
	manifestOpen = func(name string) (io.ReadCloser, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		return countingReadCloser{ReadCloser: f, n: &read}, nil
	}
	t.Cleanup(func() { manifestOpen = oldOpen })

	repo := emptyCommitRepo(t)
	big := filepath.Join(repo, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = captureRepo(t, repo)
	if read != 0 {
		t.Fatalf("read %d bytes of a file over the cap", read)
	}
	if err == nil || !strings.Contains(err.Error(), "regular-file bytes exceed") {
		t.Fatalf("err = %v", err)
	}
}

func TestManifestByteCapStopsWhenFileGrows(t *testing.T) {
	oldCap := manifestByteCap
	manifestByteCap = 64
	t.Cleanup(func() { manifestByteCap = oldCap })
	var read int64
	oldOpen := manifestOpen
	manifestOpen = func(string) (io.ReadCloser, error) {
		return countingReadCloser{ReadCloser: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("a"), 1000))), n: &read}, nil
	}
	t.Cleanup(func() { manifestOpen = oldOpen })

	repo := emptyCommitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "small"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := captureRepo(t, repo)
	if read != manifestByteCap+1 {
		t.Fatalf("read %d bytes, want %d", read, manifestByteCap+1)
	}
	if err == nil || !strings.Contains(err.Error(), "regular-file bytes exceed") {
		t.Fatalf("err = %v", err)
	}
}

func emptyCommitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", "base")
	return repo
}

func captureRepo(t *testing.T, repo string) error {
	t.Helper()
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: repo, PrimaryAbs: t.TempDir()}}
	_, err := captureWorktreeManifests(wts, snapshotWorktreeReals(wts), snapshotGitDirs(wts))
	return err
}

type countingReadCloser struct {
	io.ReadCloser
	n *int64
}

func (c countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}
