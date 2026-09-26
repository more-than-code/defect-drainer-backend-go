package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joe/defect-drainer-go/internal/analytics"
	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

// ConflictError is an operator action the API returns as 409.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// Conflict wraps msg as a 409.
func Conflict(msg string) error { return &ConflictError{Msg: msg} }

const testContinuePrompt = `Continue executing BRIEF.md and PROCESS.md. Resume where you left off.
Do not stop early. Required artifacts (exact; Defect Drainer matches these, not prose):
- RUN.md
- results/<key>.md
- results/x-<slug>.md
- evidence/<key>/<file>
A full line in RUN.md, NOT-RUN: <key>, accounts for a snapshot step with no result file.
Do not invent other marker spellings. Do not delegate onward.
Never type, echo, log or screenshot the password.`

const testProcessMarkdown = `# Test worker contract

You execute the test run in BRIEF.md. You are not fixing defects.

## Build and launch

Use the worktree checkouts BRIEF.md lists. When the prompt names a toolchain, use that to build and launch the product from the worktree. Do not install tools outside this handoff.

## Record

For each step you check, write ` + "`results/<key>.md`" + ` and put one fresh screenshot in ` + "`evidence/<key>/`" + `. The screenshot has to be created during this run: save it under a new file name (for example with a timestamp), never over an existing file. An overwritten file keeps its old creation time and does not count.

The result file's first line is exactly ` + "`STATUS: pass`" + `, ` + "`STATUS: fail`" + `, ` + "`STATUS: blocked`" + ` or ` + "`STATUS: skipped`" + `. The rest is what happened.

To add a step that was not on the plan, write ` + "`results/x-<slug>.md`" + `. ` + "`x-<slug>`" + ` must be a valid step key. Line 1 is the STATUS line, line 2 is ` + "`ACTION: …`" + ` (required), line 3 may be ` + "`EXPECTED: …`" + `, and the rest is the result.

Write ` + "`RUN.md`" + ` before you stop. It is a short summary. For a step you did not run, add a line ` + "`NOT-RUN: <key>`" + ` and, when you know why, a reason after the key.

## Stop

Stop when ` + "`RUN.md`" + ` exists and every step in BRIEF.md has a result file or a ` + "`NOT-RUN`" + ` line. A run with no steps is done once ` + "`RUN.md`" + ` exists.

Do not edit product code to make a step pass. Do not commit, push, or open a pull request. Do not call the Defect Drainer API or start jobs. Never type, echo, log or screenshot the password.
`

const (
	testEvidenceMax   = 25 << 20
	testEvidenceLimit = 12
	testActualMax     = 4000
)

var (
	statusLineRe = regexp.MustCompile(`^STATUS: (pass|fail|blocked|skipped)$`)
	notRunRe     = regexp.MustCompile(`^NOT-RUN: ([a-z0-9][a-z0-9._-]{0,63})(?:[ \t]+(.*))?$`)
)

type testRunStrategy struct{}

func (testRunStrategy) writeHandoff(b *BatchRunner, j *Job, _, handoff string) error {
	return b.writeTestHandoff(j, handoff, "")
}

func (testRunStrategy) prepare(handoff string) {
	_ = os.MkdirAll(filepath.Join(handoff, "results"), 0o755)
	_ = os.MkdirAll(filepath.Join(handoff, "evidence"), 0o755)
}

func (testRunStrategy) verifies() bool { return false }

func (testRunStrategy) prompts(b *BatchRunner, j *Job, handoff, defectsRoot, sandbox string, wts []git.WorktreeBinding, base *git.CodingAgentOpts) (string, string) {
	return buildTestPrompt(b, j, handoff, defectsRoot, sandbox, wts, base), testContinuePrompt
}

func (testRunStrategy) complete(b *BatchRunner, handoff string, j *Job, _ string, cutoff time.Time) (bool, []string) {
	var missing []string
	if _, _, ok := readRegularFile(handoff, filepath.Join(handoff, "RUN.md"), cutoff); !ok {
		missing = append(missing, "RUN.md")
	}
	if j == nil || b == nil || b.store == nil {
		return false, append(missing, "test run missing")
	}
	run, err := store.GetRun(b.store.DB, j.TestRunID)
	if err != nil {
		return false, append(missing, "test run missing")
	}
	b.mu.Lock()
	secret := ""
	if b.secrets != nil {
		secret = b.secrets[j.ID]
	}
	b.mu.Unlock()
	notRun := notRunKeys(handoff, cutoff, secret)
	snapshot := 0
	for _, step := range run.Steps {
		if step.Exploratory {
			continue
		}
		snapshot++
		if _, noted := notRun[step.Key]; validResultFile(handoff, step.Key, cutoff) || noted {
			continue
		}
		missing = append(missing, "results/"+step.Key+".md")
	}
	if snapshot == 0 && len(missing) == 0 {
		return true, nil
	}
	return len(missing) == 0, missing
}

func (testRunStrategy) afterLoop(b *BatchRunner, fin loopFinish) {
	if b.GetJob(fin.jobID) == nil {
		return
	}
	hygiene := measureDiffHygiene(fin.wts, fin.diffBase, fin.log)
	changed := hygieneChanged(hygiene)
	if !changed {
		changed = worktreeDirty(fin.wts)
	}
	b.mu.Lock()
	if live := b.jobs[fin.jobID]; live != nil && !jobGoneOrCancelled(live) {
		h := hygiene
		live.DiffHygiene = &h
		if changed {
			b.appendLogLocked(live, "worktree changes are never shipped from a test job", "warn", "DefectDrainer")
		}
	}
	b.mu.Unlock()
	b.persistLive(fin.jobID)
	if j := b.GetJob(fin.jobID); j == nil || jobGoneOrCancelled(j) {
		return
	}
	b.harvestTestRun(b.GetJob(fin.jobID), fin.handoff, !fin.failed)
	if fin.failed {
		// The driver publishes failed after afterLoop returns.
		return
	}
	b.mu.Lock()
	if cur := b.jobs[fin.jobID]; cur != nil && !jobGoneOrCancelled(cur) {
		cur.Error = ""
	}
	b.mu.Unlock()
	b.finishTerminal(fin.jobID, "completed", "")
}

func hygieneChanged(h DiffHygieneReport) bool {
	for _, r := range h.Repos {
		if r.FilesChanged > 0 || r.ChangedLines > 0 {
			return true
		}
	}
	return false
}

func worktreeDirty(wts []git.WorktreeBinding) bool {
	for _, w := range wts {
		out, err := gitCAbs(w.WorktreeAbs, "status", "--porcelain")
		if err == nil && strings.TrimSpace(out) != "" {
			return true
		}
	}
	return false
}

// CheckAccountSecret refuses a test run whose env:NAME password is unset or
// too short to redact. A free-text secret_ref or no account injects nothing.
// The value is not returned.
func CheckAccountSecret(env map[string]any) error {
	name, ok := store.AccountEnvVar(env)
	if !ok {
		return nil
	}
	_, err := secretFromEnv(name)
	return err
}

func secretFromEnv(name string) (string, error) {
	if store.ReservedSecretEnv(name) {
		return "", fmt.Errorf("%s", store.ReservedSecretMessage(name))
	}
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", fmt.Errorf("test account secret env %s is not set", name)
	}
	if len(v) < 6 {
		return "", fmt.Errorf("test account secret env %s is too short to redact safely", name)
	}
	if !secretRedactable(v) {
		return "", fmt.Errorf("test account secret env %s cannot be redacted safely", name)
	}
	return v, nil
}

// secretRedactable rejects a value that is a substring of "[redacted]" or
// that starts with a non-empty suffix of it. Either shape survives ReplaceAll
// as a piece of the replacement, or grows without bound.
func secretRedactable(secret string) bool {
	const marker = "[redacted]"
	if strings.Contains(marker, secret) {
		return false
	}
	for i := 0; i < len(marker); i++ {
		suf := marker[i:]
		if suf != "" && strings.HasPrefix(secret, suf) {
			return false
		}
	}
	return true
}

// readTestSecret reads the run's account secret for this spawn. Fix jobs
// return an empty value. The password stays off the job record.
func (b *BatchRunner) readTestSecret(j *Job) (string, error) {
	if j == nil || !j.isTestRun() || b.store == nil {
		return "", nil
	}
	run, err := store.GetRun(b.store.DB, j.TestRunID)
	if err != nil {
		return "", nil
	}
	name, ok := store.AccountEnvVar(run.Env)
	if !ok {
		return "", nil
	}
	return secretFromEnv(name)
}

func (b *BatchRunner) setSecret(id, secret string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.secrets == nil {
		b.secrets = map[string]string{}
	}
	if secret == "" {
		delete(b.secrets, id)
		return
	}
	b.secrets[id] = secret
}

func (b *BatchRunner) clearSecret(id string) {
	b.setSecret(id, "")
}

// secretEnvNames is every app account env:NAME. Those variables are stripped
// from every agent child so a fix job cannot inherit a test password.
// A store error is returned, never treated as an empty list. A reserved name
// saved before the save-time check refuses the spawn with the same text.
func (b *BatchRunner) secretEnvNames() ([]string, error) {
	if b.store == nil || b.store.DB == nil {
		return nil, nil
	}
	apps, err := store.ListApps(b.store.DB)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, app := range apps {
		for _, acc := range app.TestAccounts {
			name, ok := store.AccountEnvVar(map[string]any{
				"account": map[string]any{"secret_ref": acc.SecretRef},
			})
			if !ok || seen[name] {
				continue
			}
			if store.ReservedSecretEnv(name) {
				return nil, fmt.Errorf("%s", store.ReservedSecretMessage(name))
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func (b *BatchRunner) markTestRunStarted(jobID string) error {
	b.mu.Lock()
	j := b.jobs[jobID]
	var runID string
	if j != nil && j.isTestRun() {
		runID = j.TestRunID
	}
	b.mu.Unlock()
	if runID == "" || b.store == nil {
		return nil
	}
	return store.MarkRunInProgress(b.store.DB, runID)
}

func (b *BatchRunner) testRunBlock(j *Job) string {
	if j == nil || !j.isTestRun() || b.store == nil {
		return ""
	}
	run, err := store.GetRun(b.store.DB, j.TestRunID)
	if err != nil || run.ID == "" {
		return "test run is deleted"
	}
	switch run.Status {
	case "reviewed":
		return "test run is reviewed"
	case "abandoned":
		return "test run is abandoned"
	default:
		return ""
	}
}

// CreateTestJob persists a test_run job. It does not insert a batches row.
func (b *BatchRunner) CreateTestJob(appID, runID string) (*Job, error) {
	if b.store == nil || b.store.DB == nil {
		return nil, fmt.Errorf("store is not open")
	}
	id, err := store.ResolveAppID(b.store.DB, appID)
	if err != nil {
		return nil, err
	}
	jobID := newJobID("bjob_")
	ts := db.NowIso()
	handoff := filepath.Join(paths.BatchJobsDir(b.dataRoot), jobID)
	j := &Job{
		ID:          jobID,
		Kind:        "test_run",
		Status:      "running",
		Mode:        "grok",
		AppID:       id,
		TestRunID:   runID,
		HandoffPath: "batch-jobs/" + jobID,
		CreatedAt:   ts,
		UpdatedAt:   ts,
		Log: []string{
			"[info] [DefectDrainer] test run " + runID,
			"[info] [DefectDrainer] handoff → " + handoff,
		},
	}
	if err := writeJob(handoff, j); err != nil {
		return nil, err
	}
	analytics.RecordPromptUse(b.store.DB, "test_run.v1", "1", jobID, "", id, analytics.JobStatusToPromptOutcome(j.Status), "", "", nil)
	analytics.UpsertJobSummary(b.store.DB, jobID, "", id, j.Status, "grok", "", ts, 0, 0, 0, 0)
	b.mu.Lock()
	b.jobs[jobID] = j
	b.mu.Unlock()
	return j, nil
}

// FailTestSetup marks a test job failed, abandons its run, and writes a
// handoff that explains why spawn did not start.
func (b *BatchRunner) FailTestSetup(job *Job, msg string) {
	if job == nil {
		return
	}
	handoff := b.handoffDir(job.ID)
	cp := *job
	cp.Error = msg
	_ = b.writeTestHandoff(&cp, handoff, msg)
	b.MarkFailed(job.ID, "", msg)
	if b.store != nil && job.TestRunID != "" {
		_ = store.AbandonAgentRun(b.store.DB, job.TestRunID)
	}
}

func (b *BatchRunner) writeTestHandoff(j *Job, handoff, failure string) error {
	if j == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(handoff, "results"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(handoff, "evidence"), 0o755); err != nil {
		return err
	}
	var run store.TestRun
	if b.store != nil && j.TestRunID != "" {
		if loaded, err := store.GetRun(b.store.DB, j.TestRunID); err == nil {
			run = loaded
		}
	}
	if err := os.WriteFile(filepath.Join(handoff, "PROCESS.md"), []byte(testProcessMarkdown), 0o644); err != nil {
		return err
	}
	skills := buildSkillsMarkdown(j.Worktrees)
	if err := os.WriteFile(filepath.Join(handoff, "SKILLS.md"), []byte(skills), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(handoff, "BRIEF.md"), []byte(buildTestBrief(j, run, failure)), 0o644)
}

func buildTestBrief(j *Job, run store.TestRun, failure string) string {
	planID := "none"
	planVersion := "none"
	if run.PlanID != nil && *run.PlanID != "" {
		planID = *run.PlanID
	}
	if run.PlanVersion != nil {
		planVersion = fmt.Sprintf("%d", *run.PlanVersion)
	}
	lines := []string{
		"# Test run " + j.TestRunID,
		"",
		"## Role",
		"",
		"You are a test executor for Defect Drainer run " + j.TestRunID + ", not a fixer.",
		"Read this brief, then `PROCESS.md`, then `SKILLS.md`.",
		"",
		"## Run",
		"",
		"- Run: " + j.TestRunID,
		"- Title: " + run.Title,
		"- App: " + j.AppID,
		"- Plan: " + planID,
		"- Plan version: " + planVersion,
		"",
		"## Environment",
		"",
	}
	lines = append(lines, envBriefLines(run.Env)...)
	lines = append(lines, "", "## Account", "")
	lines = append(lines, accountBriefLines(run.Env)...)
	lines = append(lines, "", "## Worktrees", "")
	if len(j.Worktrees) == 0 {
		lines = append(lines, "- (none — do not edit product code)")
	} else {
		refByRepo := map[string]store.RunRef{}
		for _, r := range run.Refs {
			refByRepo[r.Repo] = r
		}
		for _, w := range j.Worktrees {
			ref := refByRepo[w.Repo].Ref
			sha := refByRepo[w.Repo].SHA
			if ref == "" {
				ref = w.BaseBranch
			}
			if sha == "" {
				sha = worktreeHead(w.WorktreeAbs)
			}
			lines = append(lines, fmt.Sprintf("- %s: `%s` ref `%s` sha `%s`", w.Repo, w.WorktreeAbs, ref, sha))
		}
	}
	lines = append(lines, "", "## Steps", "")
	if len(run.Steps) == 0 {
		lines = append(lines, "- (none — write `RUN.md` and stop)")
	} else {
		for _, s := range run.Steps {
			lines = append(lines, fmt.Sprintf("- %d · %s · %s · %s · %s", s.Position, s.Key, s.Section, s.Action, s.Expected))
		}
	}
	lines = append(lines,
		"",
		"## Output contract",
		"",
		"Write these under this handoff:",
		"",
		"- `RUN.md` — a summary. It may contain lines `NOT-RUN: <key>` optionally followed by a reason.",
		"- `results/<key>.md` — first line exactly `STATUS: pass|fail|blocked|skipped`; the rest is the actual result.",
		"- `results/x-<slug>.md` — an exploratory step (`x-<slug>` must be a valid step key): line 1 `STATUS: …`, line 2 `ACTION: …` (required), optional line 3 `EXPECTED: …`, then the body.",
		"- `evidence/<key>/<file>` — screenshots, one of png, jpg, jpeg, webp, gif, heic.",
		"",
		"## Rules",
		"",
		"- Do not call the Defect Drainer API or start jobs.",
		"- Do not commit, push or open PRs.",
		"- Do not edit product code to make a step pass. Changes are discarded and flagged.",
		"- Never type, echo, log or screenshot the password.",
		"- Take one fresh screenshot per checked step, saved under a new file name (never overwrite an existing file).",
		"",
	)
	if failure != "" {
		lines = append(lines, "## Spawn", "", "Spawn did not start: "+failure, "")
	}
	return strings.Join(lines, "\n")
}

func envBriefLines(env map[string]any) []string {
	if len(env) == 0 {
		return []string{"- (none)"}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "account" || k == "secret_ref" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return []string{"- (none)"}
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, "- "+k+": "+envBriefValue(env[k]))
	}
	return out
}

func envBriefValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return strings.TrimSpace(fmt.Sprint(stripSecretRef(t)))
	default:
		return strings.TrimSpace(fmt.Sprint(stripSecretRef(v)))
	}
}

func stripSecretRef(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range t {
			if k == "secret_ref" {
				continue
			}
			out[k] = stripSecretRef(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = stripSecretRef(child)
		}
		return out
	default:
		return v
	}
}

func accountBriefLines(env map[string]any) []string {
	acc, _ := env["account"].(map[string]any)
	if acc == nil {
		return []string{"- no password is available to you; mark steps that need one blocked"}
	}
	label, _ := acc["label"].(string)
	username, _ := acc["username"].(string)
	lines := []string{"- Label: " + label, "- Username: " + username}
	if _, inject := store.AccountEnvVar(env); inject {
		lines = append(lines, "- the password is in $DD_TEST_ACCOUNT_PASSWORD")
	} else {
		lines = append(lines, "- no password is available to you; mark steps that need one blocked")
	}
	return lines
}

func buildTestPrompt(b *BatchRunner, j *Job, handoff, defectsRoot, sandbox string, wts []git.WorktreeBinding, base *git.CodingAgentOpts) string {
	label := jobLabel(j)
	var wt []string
	if len(wts) == 0 {
		wt = []string{"- (none — do not edit product code)"}
	} else {
		for _, w := range wts {
			wt = append(wt, "- "+w.Repo+": run the product from "+w.WorktreeAbs+" (detached; do not edit it to force a pass)")
		}
	}
	sandboxNote := "SANDBOX: --sandbox strict / restrict (App Settings)."
	if sandbox == "workspace" {
		sandboxNote = "SANDBOX: --sandbox workspace (App Settings)."
	}
	parts := []string{
		"Test run session for " + label + ".",
		"Handoff directory (cwd): " + handoff,
		"Inventory root (readable, do not call its API): " + defectsRoot,
		"",
		sandboxNote,
		"You are a test executor, not a fixer. Do not delegate onward.",
		"Read BRIEF.md, PROCESS.md and SKILLS.md before the first step.",
		"The password, when one is provided, is only in $DD_TEST_ACCOUNT_PASSWORD. Never print it.",
		"",
		"WORKTREES:",
	}
	parts = append(parts, wt...)
	if base != nil && len(base.ToolchainNotes) > 0 {
		parts = append(parts, "")
		parts = append(parts, base.ToolchainNotes...)
	}
	parts = append(parts, "",
		"Write RUN.md, results/<key>.md, results/x-<slug>.md and evidence/<key>/<file> as BRIEF.md describes.",
		"Do not commit, push or open PRs. Do not edit product code to make a step pass.",
	)
	return strings.Join(parts, "\n")
}

func notRunKeys(handoff string, cutoff time.Time, secret string) map[string]string {
	out := map[string]string{}
	data, _, ok := readRegularFile(handoff, filepath.Join(handoff, "RUN.md"), cutoff)
	if !ok {
		return out
	}
	raw := redactRaw(string(data), secret)
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		m := notRunRe.FindStringSubmatch(line)
		if m == nil || strings.Contains(m[1], "..") || !store.ValidStepKey(m[1]) {
			continue
		}
		reason := ""
		if len(m) > 2 {
			reason = strings.TrimSpace(m[2])
		}
		out[m[1]] = reason
	}
	return out
}

func validResultFile(handoff, key string, cutoff time.Time) bool {
	data, _, ok := readRegularFile(handoff, filepath.Join(handoff, "results", key+".md"), cutoff)
	if !ok {
		return false
	}
	_, _, good := parseResultStatus(string(data))
	return good
}

func parseResultStatus(raw string) (status, body string, ok bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return "", "", false
	}
	lines[0] = strings.TrimRight(lines[0], "\r")
	m := statusLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		return "", "", false
	}
	rest := ""
	if len(lines) > 1 {
		rest = strings.TrimSpace(strings.Join(lines[1:], "\n"))
	}
	return m[1], rest, true
}

func (b *BatchRunner) harvestTestRun(job *Job, handoff string, finish bool) {
	if job == nil || !job.isTestRun() || b.store == nil {
		return
	}
	run, err := store.GetRun(b.store.DB, job.TestRunID)
	if err != nil || (run.Status != "queued" && run.Status != "in_progress") {
		b.appendLog(job, "harvest skipped: run is not queued or in_progress", "warn", "DefectDrainer")
		return
	}
	cutoff := harvestCutoff(job)
	b.mu.Lock()
	secret := ""
	if b.secrets != nil {
		secret = b.secrets[job.ID]
	}
	b.mu.Unlock()

	ignored := 0
	var warns []string
	notRun := notRunKeys(handoff, cutoff, secret)
	byKey := map[string]store.RunStep{}
	for _, step := range run.Steps {
		byKey[step.Key] = step
	}
	updates := map[string]store.HarvestedStep{}
	for key, reason := range notRun {
		step, ok := byKey[key]
		if !ok {
			ignored++
			warns = append(warns, "harvest: ignore NOT-RUN "+key+" (unknown step)")
			continue
		}
		actual := "Not run"
		if reason != "" {
			actual = "Not run: " + reason
		}
		updates[key] = store.HarvestedStep{
			Key: key, Status: "blocked", Actual: redactCut(actual, secret),
			Exploratory: step.Exploratory, Action: step.Action, Expected: step.Expected, HasExpected: true,
		}
	}

	resultsDir := filepath.Join(handoff, "results")
	if isRealDir(resultsDir) {
		ents, err := os.ReadDir(resultsDir)
		if err == nil {
			names := make([]string, 0, len(ents))
			for _, e := range ents {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				if !strings.HasSuffix(name, ".md") {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				key := strings.TrimSuffix(name, ".md")
				path := filepath.Join(resultsDir, name)
				if !store.ValidStepKey(key) {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				data, tooLarge, ok := readRegularFile(handoff, path, cutoff)
				if tooLarge {
					ignored++
					warns = append(warns, fmt.Sprintf("harvest: ignore results/%s (exceeds %d bytes)", name, harvestFileLimit))
					continue
				}
				if !ok {
					continue
				}
				step, exists := byKey[key]
				exploratory := strings.HasPrefix(key, "x-") && !exists
				if exists {
					exploratory = step.Exploratory
				}
				if !exists && !strings.HasPrefix(key, "x-") {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" (unknown step)")
					continue
				}
				parsed, perr := parseHarvestResult(redactRaw(string(data), secret), exploratory)
				if perr != "" {
					ignored++
					warns = append(warns, "harvest: ignore results/"+name+" ("+perr+")")
					continue
				}
				parsed.Key = key
				parsed.Action = redactCut(parsed.Action, secret)
				parsed.Expected = redactCut(parsed.Expected, secret)
				parsed.Actual = redactCut(parsed.Actual, secret)
				if exists && !parsed.Exploratory {
					parsed.Exploratory = false
				}
				updates[key] = parsed
			}
		}
	}

	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	steps := make([]store.HarvestedStep, 0, len(keys))
	for _, key := range keys {
		step := updates[key]
		imgs, extra, imgWarns := harvestStepImages(handoff, key, importCutoff(job))
		ignored += extra
		warns = append(warns, imgWarns...)
		step.Images = imgs
		steps = append(steps, step)
	}
	for _, w := range warns {
		b.appendLog(job, w, "warn", "DefectDrainer")
	}
	summary, err := b.store.ApplyTestHarvest(job.TestRunID, steps)
	if err != nil {
		b.appendLog(job, "harvest failed: "+err.Error(), "error", "DefectDrainer")
		return
	}
	if !summary.Applied {
		b.appendLog(job, "harvest skipped: run is not queued or in_progress", "warn", "DefectDrainer")
		return
	}
	b.appendLog(job, fmt.Sprintf(
		"harvest: pass=%d fail=%d blocked=%d skipped=%d unverified=%d images=%d ignored=%d",
		summary.ByStatus["pass"], summary.ByStatus["fail"], summary.ByStatus["blocked"],
		summary.ByStatus["skipped"], summary.ByStatus["unverified"], summary.Images, ignored,
	), "info", "DefectDrainer")
	if finish {
		if _, err := store.FinishRun(b.store.DB, job.TestRunID); err != nil {
			b.appendLog(job, "finish run: "+err.Error(), "warn", "DefectDrainer")
		}
	}
}

func parseHarvestResult(raw string, exploratory bool) (store.HarvestedStep, string) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return store.HarvestedStep{}, "invalid STATUS line"
	}
	lines[0] = strings.TrimRight(lines[0], "\r")
	m := statusLineRe.FindStringSubmatch(lines[0])
	if m == nil {
		return store.HarvestedStep{}, "invalid STATUS line"
	}
	step := store.HarvestedStep{Status: m[1]}
	rest := lines[1:]
	if exploratory {
		step.Exploratory = true
		if len(rest) == 0 || !strings.HasPrefix(strings.TrimRight(rest[0], "\r"), "ACTION:") {
			return store.HarvestedStep{}, "invalid STATUS line"
		}
		action := strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(rest[0], "\r"), "ACTION:"))
		if action == "" {
			return store.HarvestedStep{}, "invalid STATUS line"
		}
		step.Action = action
		rest = rest[1:]
		if len(rest) > 0 && strings.HasPrefix(strings.TrimRight(rest[0], "\r"), "EXPECTED:") {
			step.HasExpected = true
			step.Expected = strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(rest[0], "\r"), "EXPECTED:"))
			rest = rest[1:]
		}
	}
	if len(rest) > 0 {
		step.Actual = strings.TrimSpace(strings.Join(rest, "\n"))
	}
	return step, ""
}

func harvestStepImages(handoff, key string, cutoff time.Time) ([]store.HarvestedImage, int, []string) {
	dir := filepath.Join(handoff, "evidence", key)
	if !isRealDir(dir) {
		return nil, 0, nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, nil
	}
	var names []string
	for _, e := range ents {
		if harvestableImageName(e.Name(), e.IsDir()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var fresh []store.HarvestedImage
	ignored := 0
	var warns []string
	for _, name := range names {
		data, tooLarge, ok := readRegularFile(handoff, filepath.Join(dir, name), cutoff)
		if tooLarge || (ok && len(data) > testEvidenceMax) {
			ignored++
			warns = append(warns, fmt.Sprintf("harvest %s: skip %s (exceeds 25 MiB)", key, name))
			continue
		}
		if !ok {
			continue
		}
		fresh = append(fresh, store.HarvestedImage{Filename: name, Data: data})
	}
	if len(fresh) > testEvidenceLimit {
		extra := len(fresh) - testEvidenceLimit
		ignored += extra
		warns = append(warns, fmt.Sprintf("harvest %s: skip %d image(s) past the 12 image cap", key, extra))
		fresh = fresh[:testEvidenceLimit]
	}
	return fresh, ignored, warns
}

// redactRaw replaces the secret before newline normalization, so a value
// that itself contains CR LF is one match rather than two lines.
func redactRaw(s, secret string) string {
	if secret == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

func redactCut(s, secret string) string {
	s = strings.TrimSpace(s)
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	if utf8.RuneCountInString(s) <= testActualMax {
		return s
	}
	i := 0
	for n := 0; n < testActualMax; n++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}
