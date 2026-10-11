package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartReviewAgentIsFreshReadOnly(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "review", "spec")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(dir, "argv")
	cwdPath := filepath.Join(dir, "cwd")
	rolePath := filepath.Join(dir, "role")
	bin := filepath.Join(dir, "fake-grok")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argvPath + "\"\npwd > \"" + cwdPath + "\"\nprintf '%s\\n' \"$SKILL_FORGE_AGENT_ROLE\" > \"" + rolePath + "\"\nprintf '%s\\n' '{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":\"s\",\"num_turns\":1,\"total_cost_usd\":0.1}'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)

	res, err := StartReviewAgent(ReviewAgentOpts{
		Cwd:      cwd,
		Prompt:   "Read REVIEW.md and reply with the findings JSON object it describes.",
		MaxTurns: 6,
		Wall:     30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
	for _, banned := range []string{"-c", "-r", "--resume", "--always-approve"} {
		for _, a := range args {
			if a == banned {
				t.Fatalf("argv contains %q:\n%s", banned, argv)
			}
		}
	}
	got := map[string]string{}
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			got[args[i]] = args[i+1]
			i++
		} else if strings.HasPrefix(args[i], "--") {
			got[args[i]] = ""
		}
	}
	if got["--sandbox"] != "read-only" {
		t.Fatalf("sandbox = %q\n%s", got["--sandbox"], argv)
	}
	if _, ok := got["--no-subagents"]; !ok {
		t.Fatalf("missing --no-subagents\n%s", argv)
	}
	if got["--output-format"] != "streaming-json" {
		t.Fatalf("output format = %q", got["--output-format"])
	}
	if got["--max-turns"] != "6" {
		t.Fatalf("max-turns = %q", got["--max-turns"])
	}
	if got["--cwd"] != cwd {
		t.Fatalf("cwd flag = %q", got["--cwd"])
	}
	pwd, err := os.ReadFile(cwdPath)
	if err != nil {
		t.Fatal(err)
	}
	gotCwd := strings.TrimSpace(string(pwd))
	wantCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	gotResolved, err := filepath.EvalSymlinks(gotCwd)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != wantCwd || !strings.HasSuffix(gotCwd, "/review/spec") {
		t.Fatalf("process cwd = %q, want %q", gotCwd, wantCwd)
	}
	role, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(role)) != "worker" {
		t.Fatalf("role = %q", role)
	}
	if res.End.SessionID != "s" || res.End.NumTurns != 1 {
		t.Fatalf("end = %+v", res.End)
	}
}

func TestKillReviewProcessSkipsAfterReap(t *testing.T) {
	var signaled []int
	prev := signalReviewGroup
	signalReviewGroup = func(pid int) error {
		signaled = append(signaled, pid)
		return nil
	}
	t.Cleanup(func() { signalReviewGroup = prev })

	dir := t.TempDir()
	cwd := filepath.Join(dir, "review", "spec")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	reviewAfterReapHook = func(cmd *exec.Cmd) {
		if KillReviewProcess(cmd) {
			t.Errorf("signal sent after reap")
		}
	}
	t.Cleanup(func() { reviewAfterReapHook = nil })

	if _, err := StartReviewAgent(ReviewAgentOpts{Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
	if len(signaled) != 0 {
		t.Fatalf("signaled %v after reap", signaled)
	}
}
