package git

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// StartCodingAgent execs the resolved bin. Empty worktrees refuse spawn.
// sandbox is the app grok_sandbox setting; empty falls back to strict.
// onStdout / onStderr receive complete lines (stdout info, stderr warn);
// both streams are still appended to handoff/agent.log. flush drains any
// trailing partial line and closes the log file — call it after Wait.
func StartCodingAgent(handoffAbs, batchID, defectsRoot string, worktrees []WorktreeBinding, sandbox string, onStdout, onStderr func(string)) (*exec.Cmd, func(), error) {
	if len(worktrees) == 0 {
		return nil, nil, fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	bin := ResolveGrokBin()
	if bin == "" {
		return nil, nil, fmt.Errorf("coding agent binary not found")
	}
	sandbox = normalizeSandbox(sandbox)
	var lines []string
	for _, w := range worktrees {
		lines = append(lines, "- "+w.Repo+": EDIT ONLY "+w.WorktreeAbs)
	}
	prompt := strings.Join([]string{
		"Batch defect fix session for " + batchID + ".",
		"Handoff directory (cwd): " + handoffAbs,
		"Inventory root: " + defectsRoot,
		"WORKTREE ENFORCEMENT:",
		strings.Join(lines, "\n"),
		"Read BRIEF.md first. Reply: DONE " + batchID,
	}, "\n")
	maxTurns := os.Getenv("DEFECT_DRAINER_GROK_BATCH_MAX_TURNS")
	if maxTurns == "" {
		maxTurns = os.Getenv("GROK_BATCH_MAX_TURNS")
	}
	if maxTurns == "" {
		maxTurns = "80"
	}
	args := []string{
		"-p", prompt,
		"--cwd", handoffAbs,
		"--sandbox", sandbox,
		"--always-approve",
		"--max-turns", maxTurns,
		"--output-format", "plain",
		"--verbatim",
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = handoffAbs
	wtPaths := make([]string, 0, len(worktrees))
	for _, w := range worktrees {
		wtPaths = append(wtPaths, w.WorktreeAbs)
	}
	cmd.Env = append(os.Environ(),
		"CI=1",
		"GROK_SANDBOX="+sandbox,
		"DEFECT_DRAINER_WORKTREES="+strings.Join(wtPaths, ":"),
	)
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
