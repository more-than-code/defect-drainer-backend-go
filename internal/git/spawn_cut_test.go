package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStdoutCutRedactsSecret(t *testing.T) {
	// Stderr is the lineForwarder that keeps both sides of a maxLogBlock cut.
	// A newline-less stdout blob is one capped line, so the tail never appears.
	secret := "S3cr3t-Value-42"
	pad := strings.Repeat("a", maxLogBlock-8)
	payload := pad + secret
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '"+payload+"' >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	var lines []string
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-cut", t.TempDir(), wts, "strict", nil, func(line string) {
		lines = append(lines, line)
	}, &CodingAgentOpts{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	joined := strings.Join(lines, "")
	if strings.Contains(joined, secret) {
		t.Fatal("secret straddling the log cut reached stderr lines")
	}
}

func TestJoinedTextCutRedactsSecret(t *testing.T) {
	secret := "S3cr3t-Value-42"
	head := strings.Repeat("b", maxLogBlock-8)
	events := mustTextEvent(t, head) + "\n" + mustTextEvent(t, secret) + "\n"
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-grok")
	script := "#!/bin/sh\nprintf '%s' '" + events + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	var lines []string
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-join", t.TempDir(), wts, "strict", func(line string) {
		lines = append(lines, line)
	}, nil, &CodingAgentOpts{Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if flush != nil {
		flush()
	}
	if strings.Contains(strings.Join(lines, ""), secret) {
		t.Fatal("secret straddling joined text reached stdout lines")
	}
}

func mustTextEvent(t *testing.T, data string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"type": "text", "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
