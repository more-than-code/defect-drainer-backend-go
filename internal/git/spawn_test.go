package git

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func argvFlagValue(s, flag string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

func TestStartCodingAgentHonoursSandbox(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args.txt")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + out + "\"\nprintf 'GROK_SANDBOX=%s\\n' \"$GROK_SANDBOX\" >> \"" + out + "\"\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}

	cmd, flush, err := StartCodingAgent(handoff, "BATCH-1", t.TempDir(), wts, "workspace", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if argvFlagValue(s, "--sandbox") != "workspace" {
		t.Fatalf("sandbox argv token want workspace, got %q in:\n%s", argvFlagValue(s, "--sandbox"), s)
	}
	if !strings.Contains(s, "GROK_SANDBOX=workspace") {
		t.Fatalf("GROK_SANDBOX: %s", s)
	}

	out2 := filepath.Join(t.TempDir(), "args.txt")
	bin2 := filepath.Join(t.TempDir(), "fake-grok")
	script2 := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + out2 + "\"\nprintf 'GROK_SANDBOX=%s\\n' \"$GROK_SANDBOX\" >> \"" + out2 + "\"\nexit 0\n"
	if err := os.WriteFile(bin2, []byte(script2), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin2)
	cmd, flush, err = StartCodingAgent(t.TempDir(), "BATCH-2", t.TempDir(), wts, "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	got, err = os.ReadFile(out2)
	if err != nil {
		t.Fatal(err)
	}
	s = string(got)
	if argvFlagValue(s, "--sandbox") != "strict" {
		t.Fatalf("empty sandbox flag want strict, got %q in:\n%s", argvFlagValue(s, "--sandbox"), s)
	}
	if !strings.Contains(s, "GROK_SANDBOX=strict") {
		t.Fatalf("empty sandbox should default strict: %s", s)
	}
}

func TestLineForwarderCapsMegabyteWithoutNewline(t *testing.T) {
	var mu sync.Mutex
	var got []string
	f := newLineForwarder(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	blob := bytes.Repeat([]byte("x"), 1<<20)
	n, err := f.Write(blob)
	if err != nil || n != len(blob) {
		t.Fatalf("write %d %v", n, err)
	}
	f.Flush()
	if len(got) == 0 {
		t.Fatal("no lines emitted")
	}
	total := 0
	for _, line := range got {
		total += len(line)
		if len(line) > maxLogBlock {
			t.Fatalf("emitted line %d bytes, cap %d", len(line), maxLogBlock)
		}
	}
	if total > 2<<20 {
		t.Fatalf("emitted %d bytes from 1MB input", total)
	}
}

func TestLineForwarderWriteDoesNotCallEmitInline(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	f := newLineForwarder(func(string) {
		close(started)
		<-block
	})
	done := make(chan struct{})
	go func() {
		_, _ = f.Write([]byte("hello\n"))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer never ran")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on emit — persisted inline")
	}
	close(block)
	f.Flush()
}

func TestCappedWriterBoundsTotalBytes(t *testing.T) {
	var buf bytes.Buffer
	w := &cappedWriter{w: &buf, limit: 8}
	n, err := w.Write([]byte("abcdefghijklmnop"))
	if err != nil || n != 16 {
		t.Fatalf("write %d %v", n, err)
	}
	if buf.String() != "abcdefgh" {
		t.Fatalf("got %q", buf.String())
	}
	n, err = w.Write([]byte("xyz"))
	if err != nil || n != 3 {
		t.Fatalf("second write %d %v", n, err)
	}
	if buf.String() != "abcdefgh" {
		t.Fatalf("cap leaked: %q", buf.String())
	}
}

func TestAgentLogTeeIsCapped(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-grok")
	// 256 KiB is enough to prove the writer is wired; the real cap is 8 MiB.
	// Dump more than the cap by pointing the writer at a tiny limit via a
	// direct StartCodingAgent run that writes 9 MiB — too slow. The unit
	// test above covers the writer; this checks agent.log is teed at all
	// and stays below the package cap after a 1 MiB dump.
	script := "#!/bin/sh\ndd if=/dev/zero bs=1024 count=1024 2>/dev/null | tr '\\0' 'x'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-cap", t.TempDir(), wts, "strict", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	st, err := os.Stat(filepath.Join(handoff, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxAgentLogBytes {
		t.Fatalf("agent.log %d exceeds cap %d", st.Size(), maxAgentLogBytes)
	}
	if st.Size() == 0 {
		t.Fatal("agent.log empty — tee not wired")
	}
}

func TestStartCodingAgentExtraEnvAndSandboxProfile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args.txt")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + out + "\"\nprintf 'GROK_SANDBOX=%s\\n' \"$GROK_SANDBOX\" >> \"" + out + "\"\nprintf 'PATH=%s\\n' \"$PATH\" >> \"" + out + "\"\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-sim", t.TempDir(), wts, "workspace", nil, nil, &CodingAgentOpts{
		ExtraEnv:       map[string]string{"PATH": "/handoff/.tooling/bin:/bin"},
		SandboxProfile: "dd-simulator",
		VerifyCommands: []VerifySpec{{Repo: "demo", Command: "true"}},
		AlreadyFailing: []string{"demo: true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if argvFlagValue(s, "--sandbox") != "dd-simulator" {
		t.Fatalf("sandbox argv want dd-simulator, got %q in:\n%s", argvFlagValue(s, "--sandbox"), s)
	}
	if !strings.Contains(s, "GROK_SANDBOX=dd-simulator") {
		t.Fatalf("GROK_SANDBOX: %s", s)
	}
	if !strings.Contains(s, "PATH=/handoff/.tooling/bin:/bin") {
		t.Fatalf("extra PATH not merged: %s", s)
	}
	if !strings.Contains(s, "VERIFICATION (run by Defect Drainer") {
		t.Fatalf("prompt missing VERIFICATION block:\n%s", s)
	}
	if !strings.Contains(s, "SANDBOX: --sandbox workspace") {
		t.Fatalf("prompt should describe the base profile:\n%s", s)
	}
}
