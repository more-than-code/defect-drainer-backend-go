package git

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// lineSink collects job-log lines from a lineForwarder. The forwarder's
// consumer runs off the Write goroutine, so snapshot is only safe after Flush.
type lineSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *lineSink) add(line string) {
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
}

func (s *lineSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	copy(out, s.lines)
	return out
}

func newTestStream(t *testing.T, onEnd func(AgentEnd), onMax func()) (*agentStream, *lineSink, *lineSink) {
	t.Helper()
	var out, errs lineSink
	outFwd := newLineForwarder(out.add)
	errFwd := newLineForwarder(errs.add)
	a := &agentStream{text: outFwd, errs: errFwd, onEnd: onEnd, onMaxTurns: onMax}
	t.Cleanup(func() {
		a.Flush()
		outFwd.Flush()
		errFwd.Flush()
	})
	return a, &out, &errs
}

func finishStream(a *agentStream) {
	a.Flush()
	if a.text != nil {
		a.text.Flush()
	}
	if a.errs != nil {
		a.errs.Flush()
	}
}

func writeLines(t *testing.T, a *agentStream, lines ...string) {
	t.Helper()
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	n, err := a.Write(buf.Bytes())
	if err != nil || n != buf.Len() {
		t.Fatalf("Write %d/%d err=%v", n, buf.Len(), err)
	}
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func textLine(t *testing.T, field, value string) string {
	t.Helper()
	return jsonLine(t, map[string]string{"type": "text", field: value})
}

func requireLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("lines = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %#v, want %#v", got, want)
		}
	}
}

func TestAgentStreamJoinsTextDeltasUntilToolCall(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	deltas := []string{"The", " dialog", " wrap", " was", " only", " partly", " applied", "."}
	var lines []string
	for _, d := range deltas {
		lines = append(lines, textLine(t, "data", d))
	}
	// Real tool_call events carry content as an array. A decode that requires
	// content to be a string fails the whole event and would log the raw line.
	lines = append(lines, jsonLine(t, map[string]any{
		"type":    "tool_call",
		"content": []any{},
	}))
	writeLines(t, a, lines...)
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"The dialog wrap was only partly applied."})
}

func TestAgentStreamSplitsTextOnNewlines(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "a\n\nb"),
		textLine(t, "data", "\n \n"),
		textLine(t, "data", "c"),
		`{"type":"tool_call"}`,
	)
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"a", "b", "c"})
}

func TestAgentStreamFlushEmitsTrailingPartial(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "The"),
		textLine(t, "data", " end"),
	)
	// Nothing was handed to the forwarder yet. Give its consumer a moment so
	// an early emit cannot hide in the channel.
	time.Sleep(200 * time.Millisecond)
	if lines := out.snapshot(); len(lines) != 0 || len(a.text.ch) != 0 {
		t.Fatalf("emitted before Flush: %#v (chan %d)", lines, len(a.text.ch))
	}
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"The end"})

	// A text event that never received its newline is still part of the same
	// partial. Flush parses it before emitting, so the two deltas join.
	a2, out2, _ := newTestStream(t, nil, nil)
	body := textLine(t, "data", "hel") + "\n" + textLine(t, "data", "lo")
	n, err := a2.Write([]byte(body))
	if err != nil || n != len(body) {
		t.Fatalf("Write %d/%d err=%v", n, len(body), err)
	}
	finishStream(a2)
	requireLines(t, out2.snapshot(), []string{"hello"})
}

func TestAgentStreamEndAndMaxTurnsSeeJoinedTextFirst(t *testing.T) {
	const prose = "held prose"
	// usage is an object on the real 1.0.40 end event. It must not disturb AgentEnd.
	endLine := jsonLine(t, map[string]any{
		"type":           "end",
		"stopReason":     "end_turn",
		"sessionId":      "sess-1",
		"num_turns":      7,
		"total_cost_usd": 0.25,
		"usage":          map[string]any{"inputTokens": 3},
	})
	maxLine := `{"type":"max_turns_reached"}`
	deliveries := []struct {
		name    string
		deliver func(t *testing.T, a *agentStream, prose, event string)
	}{
		{"no newline", deliverEventNoNewline},
		{"with newline", deliverEventWithNewline},
		{"split writes", deliverEventSplit},
	}
	for _, d := range deliveries {
		t.Run("end/"+d.name, func(t *testing.T) {
			assertCallbackOnce(t, prose, endLine, true, d.deliver)
		})
		t.Run("max_turns_reached/"+d.name, func(t *testing.T) {
			assertCallbackOnce(t, prose, maxLine, false, d.deliver)
		})
	}
}

func deliverEventNoNewline(t *testing.T, a *agentStream, prose, event string) {
	t.Helper()
	mustWrite(t, a, textLine(t, "data", prose)+"\n"+event)
}

func deliverEventWithNewline(t *testing.T, a *agentStream, prose, event string) {
	t.Helper()
	writeLines(t, a, textLine(t, "data", prose), event)
}

func deliverEventSplit(t *testing.T, a *agentStream, prose, event string) {
	t.Helper()
	mustWrite(t, a, textLine(t, "data", prose)+"\n")
	mustWrite(t, a, event+"\n")
}

func mustWrite(t *testing.T, a *agentStream, body string) {
	t.Helper()
	n, err := a.Write([]byte(body))
	if err != nil || n != len(body) {
		t.Fatalf("Write %d/%d err=%v", n, len(body), err)
	}
}

// assertCallbackOnce checks that the terminal event fires its callback exactly
// once, after the joined prose line has been handed to the stdout forwarder.
func assertCallbackOnce(t *testing.T, prose, event string, isEnd bool, deliver func(t *testing.T, a *agentStream, prose, event string)) {
	t.Helper()
	textReady := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var order []string
	var calls int
	var got AgentEnd
	token := "max"
	if isEnd {
		token = "end"
	}
	outFwd := newLineForwarder(func(s string) {
		mu.Lock()
		order = append(order, "text:"+s)
		mu.Unlock()
		once.Do(func() { close(textReady) })
	})
	a := &agentStream{text: outFwd}
	waitReady := func(what string) {
		t.Helper()
		select {
		case <-textReady:
		case <-time.After(2 * time.Second):
			t.Errorf("text line was not emitted before %s", what)
		}
	}
	if isEnd {
		a.onEnd = func(e AgentEnd) {
			waitReady("OnEnd")
			mu.Lock()
			calls++
			order = append(order, "end")
			got = e
			mu.Unlock()
		}
	} else {
		a.onMaxTurns = func() {
			waitReady("OnMaxTurns")
			mu.Lock()
			calls++
			order = append(order, "max")
			mu.Unlock()
		}
	}
	t.Cleanup(func() {
		a.Flush()
		outFwd.Flush()
	})
	deliver(t, a, prose, event)
	finishStream(a)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("%s calls = %d, want 1", token, calls)
	}
	requireLines(t, order, []string{"text:" + prose, token})
	if !isEnd {
		return
	}
	if got.StopReason != "end_turn" || got.SessionID != "sess-1" || got.NumTurns != 7 || got.TotalCostUSD != 0.25 {
		t.Fatalf("AgentEnd = %+v", got)
	}
}

func TestAgentStreamLegacyTextFields(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "text", "x"),
		`{"type":"tool_call"}`,
		textLine(t, "message", "y"),
		`{"type":"tool_call"}`,
		`{"type":"text"}`,
		`{"type":"tool_call"}`,
		textLine(t, "content", "z"),
		`{"type":"tool_call"}`,
		jsonLine(t, map[string]string{"type": "text", "data": "from-data", "text": "from-text"}),
		`{"type":"tool_call"}`,
	)
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"x", "y", "z", "from-data"})
}

func TestAgentStreamCapsTextBuffer(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	payload := strings.Repeat("x", maxLogBlock+10)
	writeLines(t, a, textLine(t, "data", payload))
	// The cap fires as the buffer fills, before Flush. The forwarder consumer
	// is asynchronous, so wait until that chunk arrives.
	deadline := time.Now().Add(2 * time.Second)
	var early []string
	for {
		early = out.snapshot()
		if len(early) >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(early) != 1 || early[0] != payload[:maxLogBlock] {
		t.Fatalf("before Flush, lines = %d first %d bytes", len(early), lenAt(early, 0))
	}
	finishStream(a)
	lines := out.snapshot()
	if len(lines) != 2 {
		t.Fatalf("lines = %d (%d, %d), want 2", len(lines), lenAt(lines, 0), lenAt(lines, 1))
	}
	if len(lines[0]) != maxLogBlock || len(lines[1]) != 10 {
		t.Fatalf("chunk lens %d, %d", len(lines[0]), len(lines[1]))
	}
	if lines[0]+lines[1] != payload {
		t.Fatal("chunks dropped or rewrote payload bytes")
	}
	for _, line := range lines {
		if len(line) > maxLogBlock {
			t.Fatalf("emitted line %d bytes, cap %d", len(line), maxLogBlock)
		}
	}
}

func lenAt(lines []string, i int) int {
	if i >= len(lines) {
		return 0
	}
	return len(lines[i])
}

func TestAgentStreamThoughtFlushesAndIsDropped(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "visible"),
		jsonLine(t, map[string]string{"type": "thought", "data": "hidden reasoning"}),
		textLine(t, "data", " tail"),
		`{"type":"tool_call"}`,
	)
	finishStream(a)
	lines := out.snapshot()
	requireLines(t, lines, []string{"visible", "tail"})
	for _, line := range lines {
		if strings.Contains(line, "hidden") || strings.Contains(line, "thought") {
			t.Fatalf("thought reached the job log: %#v", lines)
		}
	}
}

func TestAgentStreamNonJSONFlushesPendingText(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "pending"),
		"older cli said hello",
	)
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"pending", "older cli said hello"})
}

func TestAgentStreamErrorAfterText(t *testing.T) {
	a, out, errs := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "before"),
		jsonLine(t, map[string]string{"type": "error", "message": "boom"}),
	)
	// Both lines are produced while handling the error event, not by the
	// stream-end flush. The two forwarders deliver asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	var stdout, stderr []string
	for {
		stdout, stderr = out.snapshot(), errs.snapshot()
		if (len(stdout) >= 1 && len(stderr) >= 1) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireLines(t, stdout, []string{"before"})
	requireLines(t, stderr, []string{"agent error: boom"})
}

func TestAgentStreamInvalidJSONLogsMarker(t *testing.T) {
	var raw bytes.Buffer
	a, out, _ := newTestStream(t, nil, nil)
	a.raw = &raw
	huge := truncatedToolUpdate(t, 63018)
	short := "{oops"
	writeLines(t, a,
		textLine(t, "data", "before"),
		short,
		huge,
		"not json",
	)
	finishStream(a)
	want := []string{
		"before",
		"invalid agent event (5 bytes; see agent.log)",
		"invalid agent event (63018 bytes; see agent.log)",
		"not json",
	}
	got := out.snapshot()
	requireLines(t, got, want)
	for _, line := range got {
		if strings.Contains(line, "HEAD-SENTINEL") || strings.Contains(line, "TAIL-SENTINEL") || strings.Contains(line, "{oops") {
			t.Fatalf("job log copied the invalid body: %q", line)
		}
	}
	if !bytes.Contains(raw.Bytes(), []byte("HEAD-SENTINEL")) || !bytes.Contains(raw.Bytes(), []byte("TAIL-SENTINEL")) {
		t.Fatal("agent.log tee dropped the invalid event")
	}
}

// truncatedToolUpdate is the shape of the live truncated tool_call_update:
// one JSON object of exactly n bytes, cut off so it does not parse.
func truncatedToolUpdate(t *testing.T, n int) string {
	t.Helper()
	head := `{"type":"tool_call_update","toolCallId":"call-b60cdf2f","note":"HEAD-SENTINEL"`
	const tailMark = "TAIL-SENTINEL"
	const tailAt = 40000
	if len(head) >= tailAt || tailAt+len(tailMark) >= n {
		t.Fatalf("fixture does not fit in %d bytes", n)
	}
	line := head + strings.Repeat("Q", tailAt-len(head)) + tailMark + strings.Repeat("Z", n-tailAt-len(tailMark))
	if len(line) != n {
		t.Fatalf("truncated line is %d bytes, want %d", len(line), n)
	}
	if json.Valid([]byte(line)) {
		t.Fatal("truncated fixture parsed; it must be invalid JSON")
	}
	return line
}

func TestAgentStreamTextPayloadKeysAreIndependent(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		`{"type":"text","data":"hello","content":[]}`,
		`{"type":"tool_call"}`,
		`{"type":"text","data":"hello","text":1}`,
		`{"type":"tool_call"}`,
		`{"type":"text","text":1,"message":"via-message"}`,
		`{"type":"tool_call"}`,
		`{"type":"text","data":"","text":"from-text"}`,
		`{"type":"tool_call"}`,
	)
	finishStream(a)
	requireLines(t, out.snapshot(), []string{"hello", "hello", "via-message", "from-text"})
}

func TestAgentStreamCapsOnRuneBoundary(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	// 23,999 x's then U+2019 (e2 80 99) straddles maxLogBlock, then more text.
	payload := strings.Repeat("x", maxLogBlock-1) + "’" + "tail"
	writeLines(t, a, textLine(t, "data", payload))
	finishStream(a)
	lines := out.snapshot()
	if len(lines) == 0 {
		t.Fatal("no chunks")
	}
	var joined strings.Builder
	for _, line := range lines {
		if !utf8.ValidString(line) {
			t.Fatalf("chunk is not valid UTF-8 (%d bytes, tail %q)", len(line), line[len(line)-min(4, len(line)):])
		}
		if len(line) > maxLogBlock {
			t.Fatalf("chunk %d bytes exceeds cap %d", len(line), maxLogBlock)
		}
		joined.WriteString(line)
	}
	if joined.String() != payload {
		t.Fatalf("joined %d bytes, payload %d", joined.Len(), len(payload))
	}
}

func TestAgentStreamFlushTextBeforePendTruncate(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	a.pendLimit = 48
	writeLines(t, a, textLine(t, "data", "kept"))
	blob := bytes.Repeat([]byte("Z"), a.pendLimit+1)
	n, err := a.Write(blob)
	if err != nil || n != len(blob) {
		t.Fatalf("Write %d/%d err=%v", n, len(blob), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for {
		lines = out.snapshot()
		if len(lines) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireLines(t, lines, []string{"kept", logTruncateMark})
}

func TestAgentStreamToolCallFlushesBeforeFlush(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	writeLines(t, a,
		textLine(t, "data", "The"),
		textLine(t, "data", " dialog"),
		`{"type":"tool_call","content":[]}`,
	)
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for {
		lines = out.snapshot()
		if len(lines) >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	requireLines(t, lines, []string{"The dialog"})
}

func TestAgentStreamCapsManySmallDeltas(t *testing.T) {
	a, out, _ := newTestStream(t, nil, nil)
	const piece = 100
	n := maxLogBlock/piece + 3
	payload := strings.Repeat("y", n*piece)
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, textLine(t, "data", strings.Repeat("y", piece)))
	}
	writeLines(t, a, lines...)
	deadline := time.Now().Add(2 * time.Second)
	var early []string
	for {
		early = out.snapshot()
		if len(early) >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(early) < 1 || early[0] != strings.Repeat("y", maxLogBlock) {
		t.Fatalf("before Flush, lines=%d first=%d", len(early), lenAt(early, 0))
	}
	finishStream(a)
	got := out.snapshot()
	var joined strings.Builder
	for _, line := range got {
		if len(line) > maxLogBlock {
			t.Fatalf("chunk %d bytes exceeds cap", len(line))
		}
		joined.WriteString(line)
	}
	if joined.String() != payload {
		t.Fatalf("joined %d bytes, payload %d", joined.Len(), len(payload))
	}
}
