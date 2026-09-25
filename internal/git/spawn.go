package git

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

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
	secret []byte
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
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return len(p), nil
	}
	f.buf = append(f.buf, p...)
	f.redactBuf()
	lines := f.drain(false)
	ch := f.ch
	f.mu.Unlock()
	for _, line := range lines {
		ch <- line
	}
	return len(p), nil
}

// redactBuf replaces the secret before any newline normalization or cut, and
// holds back len(secret)-1 bytes inside drain so a value split across writes
// is still one match.
func (f *lineForwarder) redactBuf() {
	if len(f.secret) == 0 || len(f.buf) == 0 {
		return
	}
	f.buf = bytes.ReplaceAll(f.buf, f.secret, []byte(redactedMark))
}

func (f *lineForwarder) drain(flush bool) []string {
	limit := f.normalizeSafe(flush)
	if limit == 0 {
		return nil
	}
	var lines []string
	for {
		window := f.buf[:limit]
		i := bytes.IndexByte(window, '\n')
		if i >= 0 {
			line := string(f.buf[:i])
			f.buf = f.buf[i+1:]
			limit = len(f.buf)
			if !flush && len(f.secret) > 1 {
				keep := len(f.secret) - 1
				if limit <= keep {
					break
				}
				limit -= keep
			}
			if strings.TrimSpace(line) != "" {
				lines = append(lines, capLine(line))
			}
			continue
		}
		if limit >= maxLogBlock {
			lines = append(lines, capLine(string(f.buf[:maxLogBlock])))
			f.buf = append([]byte(nil), f.buf[maxLogBlock:]...)
			limit = len(f.buf)
			if !flush && len(f.secret) > 1 {
				keep := len(f.secret) - 1
				if limit <= keep {
					break
				}
				limit -= keep
			}
			continue
		}
		break
	}
	return lines
}

// normalizeSafe turns CR line endings into LF in the portion of buf that is
// safe to emit. The held-back tail is left raw so a secret containing a
// carriage return is still intact for the next Write.
func (f *lineForwarder) normalizeSafe(flush bool) int {
	keep := 0
	if !flush && len(f.secret) > 1 {
		keep = len(f.secret) - 1
	}
	if keep >= len(f.buf) {
		return 0
	}
	head := f.buf[:len(f.buf)-keep]
	tail := append([]byte(nil), f.buf[len(f.buf)-keep:]...)
	head = bytes.ReplaceAll(head, []byte("\r\n"), []byte("\n"))
	head = bytes.ReplaceAll(head, []byte("\r"), []byte("\n"))
	f.buf = append(head, tail...)
	return len(head)
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
	f.redactBuf()
	lines := f.drain(true)
	f.buf = nil
	f.closed = true
	ch := f.ch
	f.mu.Unlock()
	for _, line := range lines {
		ch <- line
	}
	close(ch)
	<-f.done
}

// VerifySpec is one operator verification command, for the spawn prompt only.
// defaultBatchMaxTurns is the fallback when neither App Settings nor the env
// sets one. Kept as the historical value so an unset app behaves as before.
const defaultBatchMaxTurns = "80"

type VerifySpec struct {
	Repo    string
	Command string
}

// AgentEnd is the streaming-json `end` event. Field names verified against
// Grok Build 1.0.40 (2026-09-22) and deliberately mixed-case: stopReason and
// sessionId are camel, num_turns and total_cost_usd are snake. stopReason is
// snake_case *values* since 1.0 (`end_turn`, `cancelled`) — a driver loop
// matching the pre-1.0 `EndTurn` matches nothing and loops until its cap.
type AgentEnd struct {
	StopReason   string  `json:"stopReason"`
	SessionID    string  `json:"sessionId"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// ContinuePrompt resumes an iteration. It is a code constant, never
// model-authored: the worker may not widen its own completion contract, and the
// marker spellings here are the ones artifactsComplete matches exactly.
const ContinuePrompt = `Continue executing BRIEF.md and PROCESS.md. Resume where you left off.
Do not stop early. Required artifacts (exact; Defect Drainer matches these, not prose):
- NOTES.md
- for each defect you claim fixed: fix-notes/<DEF-id>.md AND
  fix-evidence/<DEF-id>/fix-01.png
- to decline a defect, a full line in NOTES.md: UNFIXED: <DEF-id>
- if the sandbox is strict and you cannot screenshot, a full line:
  NO-SCREENSHOT: <DEF-id>
  (that stops the loop only; harvest will not resolve without a new image)
Do not invent other marker spellings. Do not delegate onward.`

// CodingAgentOpts is extra spawn wiring (toolchain PATH, job-scoped sandbox
// profile, prompt notes). Nil is a no-op.
type CodingAgentOpts struct {
	ExtraEnv       map[string]string
	SandboxProfile string
	ToolchainNotes []string
	VerifyCommands []VerifySpec
	AlreadyFailing []string
	// Continue resumes the most recent session for this cwd (-c) with
	// ContinuePrompt instead of sending the full brief prompt again.
	Continue bool
	// ResumeSession resumes that exact session (-r <id>) with ContinuePrompt.
	// An operator Resume uses it so the log names the session continued.
	// Wins over Continue.
	ResumeSession string
	// OnEnd receives the `end` event; OnMaxTurns fires on the separate
	// max_turns_reached event, which is how turn exhaustion is told apart from
	// a genuine failure (both exit non-zero).
	OnEnd      func(AgentEnd)
	OnMaxTurns func()
	// MaxTurns is the app's configured turn cap. Zero falls back to the env
	// override, then the built-in default.
	MaxTurns int
	// Prompt, when set, replaces the batch-fix prompt. Empty keeps today's text.
	Prompt string
	// ContinuePrompt, when set, replaces ContinuePrompt on -c / -r. Empty
	// keeps the batch-fix continue prompt.
	ContinuePrompt string
	// Secret is redacted from agent.log. It is never written anywhere else
	// by this function. Empty disables the redactor.
	Secret string
	// StripEnv are host environment names removed before ExtraEnv is applied.
	// DD_TEST_ACCOUNT_PASSWORD is always removed from the inherited environment.
	StripEnv []string
}

// agentStream parses streaming-json (NDJSON) stdout **before** lineForwarder
// sees it. The order is load-bearing: lineForwarder caps a line at maxLogBlock
// mid-line, so unmarshalling its output would slice a JSON object in half, put
// thoughts in the job log before they could be filtered, and could lose the
// `end` event entirely. Every raw byte still reaches agent.log.
type agentStream struct {
	raw        io.Writer
	text       *lineForwarder
	errs       *lineForwarder
	onEnd      func(AgentEnd)
	onMaxTurns func()
	pend       []byte
	// pendLimit caps a newline-less stdout blob. Zero keeps maxAgentLogBytes
	// so production stays at 8 MiB; tests set a smaller value.
	pendLimit int
	// textPend joins word-sized `text` deltas. Write runs on os/exec's copy
	// loop and Flush runs after Wait, so this buffer needs no lock.
	textPend []byte
	secret   []byte
}

func (a *agentStream) Write(p []byte) (int, error) {
	if a.raw != nil {
		_, _ = a.raw.Write(p)
	}
	a.pend = append(a.pend, p...)
	for {
		i := bytes.IndexByte(a.pend, '\n')
		if i < 0 {
			// A child dumping a huge blob with no newline must not grow this
			// buffer without bound. Drop it with a marker rather than unmarshal.
			if len(a.pend) > a.pendCap() {
				// Prose already parsed must stay ahead of the marker. The
				// blob itself is dropped; agent.log already has the bytes.
				a.flushText()
				a.emit(a.text, logTruncateMark)
				a.pend = a.pend[:0]
			}
			break
		}
		line := append([]byte(nil), a.pend[:i]...)
		a.pend = a.pend[i+1:]
		a.handle(line)
	}
	return len(p), nil
}

func (a *agentStream) pendCap() int {
	if a != nil && a.pendLimit > 0 {
		return a.pendLimit
	}
	return maxAgentLogBytes
}

func (a *agentStream) emit(f *lineForwarder, line string) {
	if f == nil || line == "" {
		return
	}
	_, _ = f.Write([]byte(line + "\n"))
}

func (a *agentStream) handle(line []byte) {
	t := bytes.TrimSpace(line)
	if len(t) == 0 {
		return
	}
	// Non-JSON: an older CLI, or a fake bin in tests that echoes argv. Keep it
	// as an ordinary log line rather than discarding the run's only output.
	if t[0] != '{' {
		a.flushText()
		a.emit(a.text, string(t))
		return
	}
	// Type only. tool_call and tool_call_update carry `content` as an array
	// (1.0.40); decoding that field as a string fails the whole event and
	// would dump the raw line into the job log. `end` is decoded on its own
	// so a nested `usage` object does not disturb AgentEnd.
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(t, &head); err != nil {
		// A truncated tool_call_update is still one `{` line, tens of KB.
		// The bytes are in agent.log; the job log gets a length, not the body.
		a.flushText()
		a.emit(a.text, fmt.Sprintf("invalid agent event (%d bytes; see agent.log)", len(t)))
		return
	}
	switch head.Type {
	case "text":
		a.appendText(textPayload(t))
	case "end":
		a.flushText()
		var ev struct{ AgentEnd }
		if err := json.Unmarshal(t, &ev); err != nil {
			a.emit(a.text, string(t))
			return
		}
		if a.onEnd != nil {
			a.onEnd(ev.AgentEnd)
		}
	case "max_turns_reached":
		// The positive signal for turn exhaustion, which also exits non-zero.
		a.flushText()
		if a.onMaxTurns != nil {
			a.onMaxTurns()
		}
	case "error":
		a.flushText()
		// message, with content kept as a string so a non-string content still
		// falls through to the raw line the way the shared decode used to.
		var ev struct {
			Message string `json:"message"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(t, &ev); err != nil {
			a.emit(a.text, string(t))
			return
		}
		msg := ev.Message
		if msg == "" {
			msg = string(t)
		}
		a.emit(a.errs, "agent error: "+msg)
	default:
		// thought, tool_call, tool_call_update, usage, available_commands, and
		// whatever a later version adds. agent.log keeps them; job.log stays
		// readable. Flush first so prose before the event is one line.
		a.flushText()
	}
}

// textPayload is the operator-visible body of a text event. Grok 1.0.40 sends
// `data`. Older shapes use text, then message, then content. Each key is read
// on its own so a sibling that is not a string (content:[] or text:1) does
// not drop a string sitting next to it. Absent, null, non-string, and empty
// values are skipped. No string payload logs nothing.
func textPayload(raw []byte) string {
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ""
	}
	for _, key := range []string{"data", "text", "message", "content"} {
		v, ok := ev[key]
		if !ok || len(v) == 0 || string(v) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if s == "" {
			continue
		}
		return s
	}
	return ""
}

func (a *agentStream) appendText(payload string) {
	if payload == "" {
		return
	}
	a.textPend = append(a.textPend, payload...)
	if len(a.secret) > 0 {
		a.textPend = bytes.ReplaceAll(a.textPend, a.secret, []byte(redactedMark))
	}
	a.takeTextLines()
}

// takeTextLines emits completed lines and any maxLogBlock chunk. A short
// partial stays buffered. Newline-terminated lines keep their own spacing;
// a line that is only whitespace is dropped.
func (a *agentStream) textLimit(flush bool) int {
	n := len(a.textPend)
	if !flush && len(a.secret) > 1 {
		keep := len(a.secret) - 1
		if n <= keep {
			return 0
		}
		return n - keep
	}
	return n
}

func (a *agentStream) takeTextLines() {
	a.takeText(false)
}

func (a *agentStream) takeText(flush bool) {
	limit := a.textLimit(flush)
	for limit > 0 {
		end := limit
		if end > maxLogBlock {
			end = maxLogBlock
		}
		if i := bytes.IndexByte(a.textPend[:end], '\n'); i >= 0 {
			line := string(a.textPend[:i])
			a.textPend = a.textPend[i+1:]
			limit = a.textLimit(flush)
			if strings.TrimSpace(line) != "" {
				a.emit(a.text, line)
			}
			continue
		}
		if !flush && limit < maxLogBlock {
			break
		}
		if len(a.textPend) < maxLogBlock {
			break
		}
		cut := textCut(a.textPend)
		if cut < 1 {
			cut = 1
		}
		chunk := string(a.textPend[:cut])
		a.textPend = a.textPend[cut:]
		limit = a.textLimit(flush)
		if strings.TrimSpace(chunk) != "" {
			a.emit(a.text, chunk)
		}
	}
	// Drop the backing array of a huge delta once the live partial is short,
	// so one long event cannot pin megabytes for the rest of the run.
	if cap(a.textPend) > maxLogBlock {
		a.textPend = append([]byte(nil), a.textPend...)
	}
}

// textCut is the next chunk length for a buffer that has reached maxLogBlock
// with no newline in that window. If a UTF-8 rune straddles the cap, the cut
// moves back at most 3 bytes to the rune start so the partial rune stays
// buffered. A window of continuation bytes still returns at least one byte.
func textCut(buf []byte) int {
	cut := maxLogBlock
	if cut > len(buf) {
		cut = len(buf)
	}
	if cut == 0 {
		return 0
	}
	if cut < len(buf) && !utf8.RuneStart(buf[cut]) {
		for n := 1; n <= 3 && cut-n > 0; n++ {
			if utf8.RuneStart(buf[cut-n]) {
				return cut - n
			}
		}
	}
	return cut
}

// flushText emits the short partial as one trimmed line.
func (a *agentStream) flushText() {
	if len(a.secret) > 0 {
		a.textPend = bytes.ReplaceAll(a.textPend, a.secret, []byte(redactedMark))
	}
	a.takeText(true)
	if s := strings.TrimSpace(string(a.textPend)); s != "" {
		a.emit(a.text, s)
	}
	a.textPend = nil
}

// Flush handles a trailing line with no newline, so a final `end` on a stream
// that does not end in \n is still recorded. Pending prose is emitted after
// that line is parsed: a trailing text event still joins the partial, and a
// trailing end / max_turns_reached / error flushes inside handle before its
// callback or stderr write.
func (a *agentStream) Flush() {
	if len(a.pend) > 0 {
		line := a.pend
		a.pend = nil
		a.handle(line)
	}
	a.flushText()
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
	// A job profile denies its own cwd/.grok (it holds the profile), so cwd is
	// writable except there.
	cwdWrite := "cwd (" + handoffAbs + ")"
	if opts != nil && opts.SandboxProfile != "" {
		cwdWrite = "cwd (" + handoffAbs + ") except its .grok/ (the locked job sandbox profile — no read or write)"
	}
	var sandboxNotes []string
	if sandbox == "workspace" {
		sandboxNotes = []string{
			"SANDBOX: --sandbox workspace (App Settings).",
			"- Read: host filesystem (iOS Simulator, Xcode, simctl data allowed).",
			"- Write: " + cwdWrite + ", ~/.grok/, and temp only.",
			"- Product code: edit only under cwd/worktrees/…",
			"- Visual check / fix screenshots: capture under cwd/fix-evidence/<DEF-id>/fix-01.png (handoff-writable).",
			"- Inventory root " + defectsRoot + " is readable — open report + **historical fix evidence** paths listed in BRIEF.md.",
		}
	} else {
		sandboxNotes = []string{
			"SANDBOX: --sandbox strict / restrict (App Settings).",
			"- Read: cwd + system paths only (no ~/Library Simulator data).",
			"- Write: " + cwdWrite + ", ~/.grok/, and temp.",
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

// filterInheritedEnv drops DD_TEST_ACCOUNT_PASSWORD and any extra names from
// the host environment before a coding agent is spawned.
// FilterInheritedEnv drops DD_TEST_ACCOUNT_PASSWORD and any extra names.
// Verify commands and the toolchain probe use the same list as the agent.
func FilterInheritedEnv(environ, extra []string) []string {
	return filterInheritedEnv(environ, extra)
}

// dropListedEnv removes strip-list keys after the child env is fully built.
// keep retains the test job's intentional DD_TEST_ACCOUNT_PASSWORD assignment.
func dropListedEnv(environ, strip []string, keep map[string]bool) []string {
	drop := map[string]bool{}
	for _, name := range strip {
		if name != "" && !keep[name] {
			drop[name] = true
		}
	}
	if len(drop) == 0 {
		return environ
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if ok && drop[key] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func filterInheritedEnv(environ, extra []string) []string {
	drop := map[string]bool{"DD_TEST_ACCOUNT_PASSWORD": true}
	for _, name := range extra {
		if name != "" {
			drop[name] = true
		}
	}
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		key, _, ok := strings.Cut(e, "=")
		if !ok || drop[key] {
			continue
		}
		out = append(out, e)
	}
	return out
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
	if opts != nil && opts.Prompt != "" {
		prompt = opts.Prompt
	}
	// App Settings wins, then either env spelling, then the built-in default.
	// A Flutter repo burns turns far faster than a Node one — each pub get,
	// analyze and test is a turn — so this is per app, not per install.
	maxTurns := ""
	if opts != nil && opts.MaxTurns > 0 {
		maxTurns = strconv.Itoa(opts.MaxTurns)
	}
	if maxTurns == "" {
		maxTurns = env.EnvDrainer("GROK_BATCH_MAX_TURNS")
	}
	if maxTurns == "" {
		maxTurns = os.Getenv("GROK_BATCH_MAX_TURNS")
	}
	if maxTurns == "" {
		maxTurns = defaultBatchMaxTurns
	}
	// Iteration 2+ resumes the session for this cwd with the fixed continue
	// prompt. Everything else about the argv is identical, so the sandbox and
	// turn budget cannot drift between iterations.
	args := []string{}
	promptArg := prompt
	cont := ContinuePrompt
	if opts != nil && opts.ContinuePrompt != "" {
		cont = opts.ContinuePrompt
	}
	if opts != nil && opts.ResumeSession != "" {
		promptArg = cont
		args = append(args, "-r", opts.ResumeSession)
	} else if opts != nil && opts.Continue {
		promptArg = cont
		args = append(args, "-c")
	}
	args = append(args,
		"-p", promptArg,
		"--cwd", handoffAbs,
		"--sandbox", sandboxArg,
		"--always-approve",
		"--max-turns", maxTurns,
		// streaming-json, not plain: plain prints prose at exit and hides
		// stopReason, so the harness cannot tell turn exhaustion from a finish.
		"--output-format", "streaming-json",
		"--verbatim",
	)
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
	// Inherited DD_TEST_ACCOUNT_PASSWORD and any app env: secret names are
	// removed here. ExtraEnv may set DD_TEST_ACCOUNT_PASSWORD again for a
	// test job; a fix job never does.
	var strip []string
	if opts != nil {
		strip = opts.StripEnv
	}
	cmd.Env = append(filterInheritedEnv(os.Environ(), strip),
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
	keep := map[string]bool{}
	if opts != nil {
		if _, ok := opts.ExtraEnv["DD_TEST_ACCOUNT_PASSWORD"]; ok {
			keep["DD_TEST_ACCOUNT_PASSWORD"] = true
		}
	}
	cmd.Env = dropListedEnv(cmd.Env, strip, keep)
	outFwd := newLineForwarder(onStdout)
	errFwd := newLineForwarder(onStderr)
	stream := &agentStream{text: outFwd, errs: errFwd}
	if opts != nil && opts.Secret != "" {
		sec := []byte(opts.Secret)
		outFwd.secret = sec
		errFwd.secret = sec
		stream.secret = sec
	}
	if opts != nil {
		stream.onEnd = opts.OnEnd
		stream.onMaxTurns = opts.OnMaxTurns
	}
	logf, err := os.OpenFile(handoffAbs+"/agent.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var stdoutRed, stderrRed *redactWriter
	if err == nil {
		var start int64
		if st, serr := logf.Stat(); serr == nil {
			start = st.Size()
		}
		tee := &cappedWriter{w: logf, n: start, limit: maxAgentLogBytes}
		// stdout is NDJSON and goes through the parser, which owns the raw tee.
		// stderr is plain text and keeps the old path. Each stream redacts on
		// its own so a secret split across two writes of that stream is caught.
		var rawOut io.Writer = tee
		var rawErr io.Writer = tee
		if opts != nil && opts.Secret != "" {
			stdoutRed = newRedactWriter(tee, opts.Secret)
			stderrRed = newRedactWriter(tee, opts.Secret)
			rawOut = stdoutRed
			rawErr = stderrRed
		}
		stream.raw = rawOut
		cmd.Stdout = stream
		cmd.Stderr = io.MultiWriter(rawErr, errFwd)
	} else {
		cmd.Stdout = stream
		cmd.Stderr = errFwd
	}
	flush := func() {
		if stdoutRed != nil {
			_ = stdoutRed.Flush()
		}
		if stderrRed != nil {
			_ = stderrRed.Flush()
		}
		stream.Flush()
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
