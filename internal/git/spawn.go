package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/joe/defect-drainer-go/internal/env"
)

// normalizeSandbox maps empty / restrict / restricted / anything else → strict.
// workspace is the only non-default profile (store.ParseGrokSandbox).
func normalizeSandbox(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == "workspace" {
		return "workspace"
	}
	return "strict"
}

// TS spawnGrokBatchFix coalesces at MAX_BLOCK = 24_000. We stay line-oriented
// but use the same cap so a child that dumps a huge blob with no newline
// cannot grow the pending buffer (or one job-log line) without bound.
const (
	maxLogBlock      = 24_000
	logTruncateMark  = "...[truncated]"
	lineQueueBound   = 64
	maxAgentLogBytes = 8 << 20 // handoff/agent.log total file cap (not served)
)

// cappedWriter stops writing after limit bytes. Write always reports
// len(p) so io.MultiWriter does not fail the agent stdout copy.
type cappedWriter struct {
	mu    sync.Mutex
	w     io.Writer
	n     int64
	limit int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c == nil || c.w == nil {
		return len(p), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n >= c.limit {
		return len(p), nil
	}
	left := c.limit - c.n
	if int64(len(p)) > left {
		if _, err := c.w.Write(p[:left]); err != nil {
			return len(p), err
		}
		c.n = c.limit
		return len(p), nil
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return len(p), err
}

// lineForwarder splits writes into lines and hands each one to a consumer
// goroutine. Write never persists — it only buffers, splits, and sends.
// The consumer is the only caller of emit (appendLog / persist).
type lineForwarder struct {
	mu     sync.Mutex
	buf    []byte
	emit   func(string)
	ch     chan string
	done   chan struct{}
	closed bool
}

func newLineForwarder(emit func(string)) *lineForwarder {
	f := &lineForwarder{
		emit: emit,
		ch:   make(chan string, lineQueueBound),
		done: make(chan struct{}),
	}
	go f.consume()
	return f
}

func (f *lineForwarder) consume() {
	defer close(f.done)
	for line := range f.ch {
		if f.emit != nil {
			f.emit(line)
		}
	}
}

func capLine(s string) string {
	if len(s) <= maxLogBlock {
		return s
	}
	keep := maxLogBlock - len(logTruncateMark)
	if keep < 0 {
		keep = 0
	}
	return s[:keep] + logTruncateMark
}

func (f *lineForwarder) Write(p []byte) (int, error) {
	if f == nil {
		return len(p), nil
	}
	norm := bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	norm = bytes.ReplaceAll(norm, []byte("\r"), []byte("\n"))
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return len(p), nil
	}
	f.buf = append(f.buf, norm...)
	var lines []string
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i >= 0 {
			line := string(f.buf[:i])
			f.buf = f.buf[i+1:]
			if strings.TrimSpace(line) != "" {
				lines = append(lines, capLine(line))
			}
			continue
		}
		if len(f.buf) >= maxLogBlock {
			lines = append(lines, capLine(string(f.buf[:maxLogBlock])))
			f.buf = append([]byte(nil), f.buf[maxLogBlock:]...)
			continue
		}
		break
	}
	ch := f.ch
	f.mu.Unlock()
	for _, line := range lines {
		ch <- line
	}
	return len(p), nil
}

// Flush emits a trailing partial line, closes the consumer, and waits for it
// to exit. Call after Wait — os/exec has finished copying stdout/stderr by
// then, so Write cannot race this close.
func (f *lineForwarder) Flush() {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		<-f.done
		return
	}
	rem := ""
	if s := strings.TrimSpace(string(f.buf)); s != "" {
		rem = capLine(s)
	}
	f.buf = nil
	f.closed = true
	ch := f.ch
	f.mu.Unlock()
	if rem != "" {
		ch <- rem
	}
	close(ch)
	<-f.done
}

// VerifySpec is one operator verification command, for the spawn prompt only.
type VerifySpec struct {
	Repo    string
	Command string
}

// CodingAgentOpts is extra spawn wiring (toolchain PATH, job-scoped sandbox
// profile, prompt notes). Nil is a no-op.
type CodingAgentOpts struct {
	ExtraEnv       map[string]string
	SandboxProfile string
	ToolchainNotes []string
	VerifyCommands []VerifySpec
	AlreadyFailing []string
}

func buildBatchFixCLIPrompt(handoffAbs, batchID, defectsRoot, sandbox string, worktrees []WorktreeBinding, opts *CodingAgentOpts) string {
	wtLines := make([]string, 0, len(worktrees))
	if len(worktrees) == 0 {
		wtLines = []string{"- (none — do not edit product code)"}
	} else {
		for _, w := range worktrees {
			wtLines = append(wtLines, "- "+w.Repo+": EDIT ONLY "+w.WorktreeAbs+" (branch "+w.Branch+"); NEVER "+w.PrimaryAbs)
		}
	}
	var sandboxNotes []string
	if sandbox == "workspace" {
		sandboxNotes = []string{
			"SANDBOX: --sandbox workspace (App Settings).",
			"- Read: host filesystem (iOS Simulator, Xcode, simctl data allowed).",
			"- Write: cwd (" + handoffAbs + "), ~/.grok/, and temp only.",
			"- Product code: edit only under cwd/worktrees/…",
			"- Visual check / fix screenshots: capture under cwd/fix-evidence/<DEF-id>/fix-01.png (handoff-writable).",
			"- Inventory root " + defectsRoot + " is readable — open report + **historical fix evidence** paths listed in BRIEF.md.",
		}
	} else {
		sandboxNotes = []string{
			"SANDBOX: --sandbox strict / restrict (App Settings).",
			"- Read: cwd + system paths only (no ~/Library Simulator data).",
			"- Write: cwd (" + handoffAbs + "), ~/.grok/, and temp.",
			"- Product code: edit only under cwd/worktrees/…",
			"- Historical fix evidence may be unreadable under strict if paths are outside cwd — prefer workspace sandbox for visual history.",
			"- Inventory updates under " + defectsRoot + " may be blocked; write new proof to handoff fix-evidence/.",
		}
	}
	parts := []string{
		"Batch defect fix session for " + batchID + ".",
		"Handoff directory (cwd): " + handoffAbs,
		"Inventory root: " + defectsRoot,
		"",
	}
	parts = append(parts, sandboxNotes...)
	if opts != nil && len(opts.ToolchainNotes) > 0 {
		parts = append(parts, "")
		parts = append(parts, opts.ToolchainNotes...)
	}
	if opts != nil && len(opts.VerifyCommands) > 0 {
		parts = append(parts, "",
			"VERIFICATION (run by Defect Drainer after you exit — not by you, and not editable):",
		)
		for _, v := range opts.VerifyCommands {
			parts = append(parts, "- "+v.Repo+": "+v.Command)
		}
		parts = append(parts,
			"- A command you BREAK blocks the resolve, however good your screenshots are.",
			"- Run them yourself in the worktree before claiming DONE; fix what you broke.",
			"- Do not report test results you did not actually observe.",
		)
		if len(opts.AlreadyFailing) > 0 {
			parts = append(parts,
				"- ALREADY FAILING before you started (measured, not your doing —",
				"  do not chase these unless the defect is about them):",
			)
			for _, f := range opts.AlreadyFailing {
				parts = append(parts, "    · "+f)
			}
		} else {
			parts = append(parts, "- All of them passed before you started, so any failure after is yours.")
		}
	}
	parts = append(parts, "",
		"WORKTREE ENFORCEMENT (mandatory):",
	)
	parts = append(parts, wtLines...)
	parts = append(parts,
		"- Product edits outside listed worktree paths are forbidden.",
		"- Do not checkout branches on primary trees. Do not run git commands in primaryAbs.",
		"- READ each worktree's own contract file (AGENTS.md / CLAUDE.md — BRIEF.md lists",
		"  the exact paths) BEFORE editing it. They define that repo's mandatory gates,",
		"  skills and conventions; where stricter than this brief, they win. Follow its",
		"  house style — do not reformat code the fix does not need to touch.",
		"",
		"ROLE (SKILL_FORGE_AGENT_ROLE=worker is set on this process):",
		"- You are the worker for this job, not an orchestrator. Do not delegate onward.",
		"- Read, in this order, BEFORE the first product edit: BRIEF.md, PROCESS.md,",
		"  SKILLS.md, then each contract file SKILLS.md lists for the worktree you touch.",
		"- SKILLS.md carries absolute paths because cwd is the handoff, not a product repo;",
		"  directory-based skill discovery will not find them. If SKILLS.md lists none for a",
		"  worktree, use security-baseline, coding-discipline, code-quality and testing-strategy.",
		"- Classify each defect's blast radius (Tier 1/2/3) in NOTES.md, then proceed:",
		"  the operator starting this batch IS the approval. Do not wait to be approved.",
		"- Run the verification commands yourself inside the worktree. DD re-runs them after",
		"  you exit; a result you claim but did not observe is a defect in your notes, not a pass.",
		"- Commit on the worktree branch already checked out. Do not push. Do not commit on",
		"  main/master. Do not commit BRIEF.md / PROCESS.md / SKILLS.md / NOTES.md.",
		"",
		"Read BRIEF.md first. For each defect it lists:",
		"- report evidence (bug as reported)",
		"- historical fix evidence (prior proof on the defect SSOT — use as context / regression baseline)",
		"- what to deliver this run under handoff fix-evidence/ and fix-notes/",
		"",
		"TASK:",
		"1. For each defect: read report + historical fix evidence from BRIEF paths; implement fixes only under worktree paths.",
		"2. Verify (tests / smoke / visual) inside the worktree checkout; compare to historical fix shots when present.",
		"3. REQUIRED **new** deliverables under this cwd (backend harvests onto the defect):",
		"   - fix-evidence/<DEF-id>/fix-01.png (and more images as needed)",
		"   - fix-notes/<DEF-id>.md  (what changed + how verified; note relation to prior fix if any)",
		"   - NOTES.md (plan + tier per defect, decisions, departures, what you could not verify)",
		"   - fix-notes must answer EACH acceptance criterion listed for that defect in BRIEF.md.",
		"     Those criteria are the definition of fixed — not your own reading of the title.",
		"     If one cannot be met, say so explicitly instead of omitting it.",
		"4. Visual check when sandbox=workspace: Simulator / app screenshots into fix-evidence/.",
		"5. Do not claim DONE without those **handoff** files for each fixed defect (historical inventory files alone are not enough for this run).",
		"6. Reply: DONE "+batchID,
	)
	return strings.Join(parts, "\n")
}

// StartCodingAgent execs the resolved bin. Empty worktrees refuse spawn.
// sandbox is the app grok_sandbox setting; empty falls back to strict.
// opts may carry toolchain env, a job-scoped sandbox profile name, and prompt
// notes. onStdout / onStderr receive complete lines (stdout info, stderr warn);
// both streams are still appended to handoff/agent.log. flush drains any
// trailing partial line and closes the log file — call it after Wait.
func StartCodingAgent(handoffAbs, batchID, defectsRoot string, worktrees []WorktreeBinding, sandbox string, onStdout, onStderr func(string), opts *CodingAgentOpts) (*exec.Cmd, func(), error) {
	if len(worktrees) == 0 {
		return nil, nil, fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	bin := ResolveGrokBin()
	if bin == "" {
		return nil, nil, fmt.Errorf("coding agent binary not found")
	}
	sandbox = normalizeSandbox(sandbox)
	// A job-scoped profile extends the built-in one; the prompt still describes
	// the base profile's rules, which the custom profile only widens.
	sandboxArg := sandbox
	if opts != nil && opts.SandboxProfile != "" {
		sandboxArg = opts.SandboxProfile
	}
	prompt := buildBatchFixCLIPrompt(handoffAbs, batchID, defectsRoot, sandbox, worktrees, opts)
	maxTurns := env.EnvDrainer("GROK_BATCH_MAX_TURNS")
	if maxTurns == "" {
		maxTurns = os.Getenv("GROK_BATCH_MAX_TURNS")
	}
	if maxTurns == "" {
		maxTurns = "80"
	}
	args := []string{
		"-p", prompt,
		"--cwd", handoffAbs,
		"--sandbox", sandboxArg,
		"--always-approve",
		"--max-turns", maxTurns,
		"--output-format", "plain",
		"--verbatim",
	}
	if env.EnvDrainerFlag("GROK_BYPASS_PERMISSIONS") {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if tools := strings.TrimSpace(env.EnvDrainer("GROK_TOOLS")); tools != "" {
		args = append(args, "--tools", tools)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = handoffAbs
	wtPaths := make([]string, 0, len(worktrees))
	for _, w := range worktrees {
		wtPaths = append(wtPaths, w.WorktreeAbs)
	}
	// SKILL_FORGE_AGENT_ROLE marks the child as the worker end of the
	// delegation. Never set it on the serve process: a leaked value would tell
	// a human session started from the same shell that it is a worker.
	cmd.Env = append(os.Environ(),
		"CI=1",
		"GROK_SANDBOX="+sandboxArg,
		"SKILL_FORGE_AGENT_ROLE=worker",
	)
	if opts != nil {
		for k, v := range opts.ExtraEnv {
			if k == "" {
				continue
			}
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "DEFECT_DRAINER_WORKTREES="+strings.Join(wtPaths, ":"))
	outFwd := newLineForwarder(onStdout)
	errFwd := newLineForwarder(onStderr)
	logf, err := os.OpenFile(handoffAbs+"/agent.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		var start int64
		if st, serr := logf.Stat(); serr == nil {
			start = st.Size()
		}
		tee := &cappedWriter{w: logf, n: start, limit: maxAgentLogBytes}
		cmd.Stdout = io.MultiWriter(tee, outFwd)
		cmd.Stderr = io.MultiWriter(tee, errFwd)
	} else {
		cmd.Stdout = outFwd
		cmd.Stderr = errFwd
	}
	flush := func() {
		outFwd.Flush()
		errFwd.Flush()
		if logf != nil {
			_ = logf.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		flush()
		return nil, nil, err
	}
	return cmd, flush, nil
}
