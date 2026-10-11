package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/store"
)

func TestWriteSimulatorSandboxProfileStaysInHandoff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handoff := t.TempDir()
	got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != "dd-simulator" {
		t.Fatalf("profile %q", got.Profile)
	}
	path := filepath.Join(handoff, ".grok", "sandbox.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "[profiles.dd-simulator]") {
		t.Fatalf("missing profile: %s", s)
	}
	if !strings.Contains(s, `extends = "workspace"`) {
		t.Fatalf("missing extends: %s", s)
	}
	if !strings.Contains(s, "CoreSimulator/Devices") {
		t.Fatalf("missing devices grant: %s", s)
	}
	global := filepath.Join(home, ".grok", "sandbox.toml")
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Fatalf("~/.grok/sandbox.toml must be untouched, stat=%v", err)
	}
}

// isolateGrokHome points the user-level sandbox.toml lookup at an empty dir so
// the host's ~/.grok cannot decide a test.
func isolateGrokHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("GROK_HOME", h)
	return h
}

func TestSimulatorProfileDeniesItsOwnDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	handoff := t.TempDir()
	got, err := writeSimulatorSandboxProfile(handoff, "strict", "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(handoff, ".grok", "sandbox.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(got.Body) {
		t.Fatalf("Body must be the bytes on disk:\n%s\nvs\n%s", got.Body, body)
	}
	deny := `deny = ["` + filepath.Join(handoff, ".grok") + `"]`
	if !strings.Contains(string(body), deny) {
		t.Fatalf("profile must deny its own dir (%s):\n%s", deny, body)
	}
	if !strings.Contains(string(body), `extends = "strict"`) {
		t.Fatalf("missing extends: %s", body)
	}
	if err := checkSimulatorSandboxProfile(handoff, got.Body); err != nil {
		t.Fatalf("fresh profile must pass: %v", err)
	}
}

func TestCheckSimulatorProfileRejectsTamper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	widen := func(t *testing.T, handoff string) {
		f, err := os.OpenFile(filepath.Join(handoff, ".grok", "sandbox.toml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("read_write = [\"/\"]\n"); err != nil {
			t.Fatal(err)
		}
	}
	// Same bytes behind a symlink still fail: the next write would follow it.
	fileLink := func(t *testing.T, handoff string) {
		path := filepath.Join(handoff, ".grok", "sandbox.toml")
		elsewhere := filepath.Join(t.TempDir(), "sandbox.toml")
		if err := os.Rename(path, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, path); err != nil {
			t.Fatal(err)
		}
	}
	dirLink := func(t *testing.T, handoff string) {
		dir := filepath.Join(handoff, ".grok")
		elsewhere := filepath.Join(t.TempDir(), "grok")
		if err := os.Rename(dir, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, dir); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, handoff string) {
		if err := os.Remove(filepath.Join(handoff, ".grok", "sandbox.toml")); err != nil {
			t.Fatal(err)
		}
	}
	for name, tamper := range map[string]func(*testing.T, string){
		"widened grant": widen, "file symlink": fileLink, "dir symlink": dirLink, "removed": remove,
	} {
		t.Run(name, func(t *testing.T) {
			handoff := t.TempDir()
			got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
			if err != nil {
				t.Fatal(err)
			}
			tamper(t, handoff)
			err = checkSimulatorSandboxProfile(handoff, got.Body)
			if err == nil || !strings.Contains(err.Error(), "was modified") {
				t.Fatalf("want a modified-profile error, got %v", err)
			}
		})
	}
}

func TestWriteSimulatorProfileDoesNotFollowPlantedSymlinks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	precious := filepath.Join(t.TempDir(), "zshrc")
	if err := os.WriteFile(precious, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("file", func(t *testing.T) {
		handoff := t.TempDir()
		if err := os.MkdirAll(filepath.Join(handoff, ".grok"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(precious, filepath.Join(handoff, ".grok", "sandbox.toml")); err != nil {
			t.Fatal(err)
		}
		got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(precious); string(b) != "precious" {
			t.Fatalf("write followed the planted symlink: target now %q", b)
		}
		if err := checkSimulatorSandboxProfile(handoff, got.Body); err != nil {
			t.Fatalf("rewritten profile must be a regular file with DD's bytes: %v", err)
		}
	})

	t.Run("dir", func(t *testing.T) {
		handoff := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(handoff, ".grok")); err != nil {
			t.Fatal(err)
		}
		got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(outside, "sandbox.toml")); !os.IsNotExist(err) {
			t.Fatalf("profile written through the planted dir symlink: stat=%v", err)
		}
		if err := checkSimulatorSandboxProfile(handoff, got.Body); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCheckSimulatorProfileRefusesUserLevelShadow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handoff := t.TempDir()
	got, err := writeSimulatorSandboxProfile(handoff, "workspace", "BATCH-1")
	if err != nil {
		t.Fatal(err)
	}
	for name, grokHome := range map[string]string{
		"GROK_HOME":       t.TempDir(),
		"default ~/.grok": filepath.Join(home, ".grok"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "GROK_HOME" {
				t.Setenv("GROK_HOME", grokHome)
			} else {
				t.Setenv("GROK_HOME", "")
			}
			if err := os.MkdirAll(grokHome, 0o755); err != nil {
				t.Fatal(err)
			}
			user := filepath.Join(grokHome, "sandbox.toml")
			if err := os.WriteFile(user, []byte("[profiles.build]\nextends = \"workspace\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := checkSimulatorSandboxProfile(handoff, got.Body); err != nil {
				t.Fatalf("an unrelated user profile must pass: %v", err)
			}
			if err := os.WriteFile(user, []byte("[profiles.dd-simulator]\nextends = \"off\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := checkSimulatorSandboxProfile(handoff, got.Body)
			if err == nil || !strings.Contains(err.Error(), user) {
				t.Fatalf("a user-level dd-simulator must refuse the start, got %v", err)
			}
		})
	}
}

// enableSimulatorWrites turns on the job profile, over base, for the seeded
// app every jobs test uses.
func enableSimulatorWrites(t *testing.T, br *BatchRunner, base string) {
	t.Helper()
	if _, err := br.store.DB.Exec(`UPDATE apps SET allow_simulator_writes = 1, grok_sandbox = ? WHERE id = ?`, base, store.SeededTutoredWebappAppID); err != nil {
		t.Fatal(err)
	}
}

// The acceptance test: an agent that edits its profile in iteration 1 never
// gets an iteration 2, whatever it wrote.
func TestTamperedProfileStopsTheLoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	enableSimulatorWrites(t, br, "workspace")
	defID := "DEF-20260923-sbxtamper-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_sbxtamper", "BATCH-20260923-st", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	// Widens its grant, then leaves NOTES.md undone so the loop wants an
	// iteration 2 — which would run under the widened profile.
	body := `printf 'read_write = ["/"]\n' >> .grok/sandbox.toml` + "\n" +
		"mkdir -p fix-evidence/" + defID + " fix-notes\n" +
		"printf 'png' > fix-evidence/" + defID + "/fix-01.png\n" +
		"printf 'notes' > fix-notes/" + defID + ".md\n" +
		"exit 0"
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, body))
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "3")
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatal(err)
	}
	waitExit(t, exited)

	if got := iterations(t, counter); got != 1 {
		t.Fatalf("a tampered profile must not reach another iteration, got %d", got)
	}
	live := br.GetJob(job.ID)
	if live == nil || live.Status != "failed" || !strings.Contains(live.Error, "was modified") {
		t.Fatalf("want failed with the tamper reason, got %+v", live)
	}
	if live.AgentRun == nil || live.AgentRun.StoppedBy != StoppedByExit {
		t.Fatalf("want stoppedBy=exit, got %+v", live.AgentRun)
	}
	got, err := st.Get(defID)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Status == "resolved" {
		t.Fatalf("a job that tampered with its sandbox must not resolve: %+v", got)
	}
}

// The control: the check does not trip on an honest agent, so a job profile
// still survives a multi-iteration run with --sandbox dd-simulator each time,
// over either base.
func TestUntouchedProfileKeepsLooping(t *testing.T) {
	for _, base := range []string{"workspace", "strict"} {
		t.Run(base, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			isolateGrokHome(t)
			st, data, defects := testStore(t)
			br := NewBatchRunner(st, data)
			enableSimulatorWrites(t, br, base)
			defID := "DEF-20260923-sbxok-0001"
			writeInProgressDefect(t, st, defID)
			job := runningJob(t, br, "bjob_sbxok", "BATCH-20260923-so", defID)

			counter := filepath.Join(t.TempDir(), "iters")
			argv := filepath.Join(t.TempDir(), "argv")
			t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, `echo "$@" >> '`+argv+`'`+"\nexit 0"))
			t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "2")
			exited := make(chan struct{})
			spawnExitHook = func() { close(exited) }
			t.Cleanup(func() { spawnExitHook = nil })

			if err := br.StartSpawn(job.ID, defects); err != nil {
				t.Fatal(err)
			}
			waitExit(t, exited)

			if got := iterations(t, counter); got != 2 {
				t.Fatalf("want both iterations, got %d", got)
			}
			args, _ := os.ReadFile(argv)
			if strings.Count(string(args), "--sandbox dd-simulator") != 2 {
				t.Fatalf("every iteration runs the job profile:\n%s", args)
			}
			profile, _ := os.ReadFile(filepath.Join(data, "batch-jobs", job.ID, ".grok", "sandbox.toml"))
			if !strings.Contains(string(profile), `extends = "`+base+`"`) {
				t.Fatalf("profile must extend %s:\n%s", base, profile)
			}
			if live := br.GetJob(job.ID); live == nil || live.AgentRun == nil || live.AgentRun.StoppedBy != StoppedByIterations {
				t.Fatalf("want stoppedBy=iterations, got %+v", live)
			}
		})
	}
}

// shadowUserProfile defines dd-simulator in the isolated user-level
// sandbox.toml, which grok would let replace the job's profile.
func shadowUserProfile(t *testing.T) string {
	t.Helper()
	user := filepath.Join(isolateGrokHome(t), "sandbox.toml")
	if err := os.WriteFile(user, []byte("[profiles.dd-simulator]\nextends = \"off\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return user
}

// Iteration 1 is checked too: the refusal comes back from StartSpawn itself
// (its caller marks the job failed) and no agent ever runs.
func TestShadowedProfileRefusesIterationOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	user := shadowUserProfile(t)
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	enableSimulatorWrites(t, br, "workspace")
	defID := "DEF-20260923-sbxshadow-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_sbxshadow", "BATCH-20260923-ss", defID)

	counter := filepath.Join(t.TempDir(), "iters")
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, "exit 0"))

	err := br.StartSpawn(job.ID, defects)
	if err == nil || !strings.Contains(err.Error(), user) {
		t.Fatalf("want a refused start naming %s, got %v", user, err)
	}
	if got := iterations(t, counter); got != 0 {
		t.Fatalf("the agent must never start, got %d invocation(s)", got)
	}
}

// And Resume's first iteration: Restart marks the job failed with the reason.
func TestShadowedProfileRefusesResume(t *testing.T) {
	f := newRestartFixture(t, "failed", StoppedByExit, artifactsBody("DEF-20260923-restart-0001"), true)
	user := shadowUserProfile(t)
	// GROK_HOME moved grok's session store too; the session lives there now.
	writeJobSession(t, f.br, f.job.ID, testSessionID)
	enableSimulatorWrites(t, f.br, "workspace")

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err == nil || !strings.Contains(err.Error(), user) {
		t.Fatalf("want a refused resume naming %s, got %v", user, err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatalf("the agent must never start, got %d call(s)", len(calls))
	}
	if live := f.br.GetJob(f.job.ID); live == nil || live.Status != "failed" || !strings.Contains(live.Error, user) {
		t.Fatalf("want failed with the reason, got %+v", live)
	}
}

// A profile that cannot be written stays fail-soft: warn and run on the base
// profile, which only ever narrows what the agent may write.
func TestProfileWriteFailureFallsBackToBase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	isolateGrokHome(t)
	st, data, defects := testStore(t)
	br := NewBatchRunner(st, data)
	enableSimulatorWrites(t, br, "workspace")
	defID := "DEF-20260923-sbxsoft-0001"
	writeInProgressDefect(t, st, defID)
	job := runningJob(t, br, "bjob_sbxsoft", "BATCH-20260923-sf", defID)

	// An entry DD cannot unlink makes RemoveAll, and so the write, fail.
	grokDir := filepath.Join(data, "batch-jobs", job.ID, ".grok")
	if err := os.MkdirAll(grokDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grokDir, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(grokDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(grokDir, 0o755) })

	counter := filepath.Join(t.TempDir(), "iters")
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("GROK_BUILD_BIN", countingAgent(t, counter, `echo "$@" >> '`+argv+`'`+"\nexit 0"))
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	exited := make(chan struct{})
	spawnExitHook = func() { close(exited) }
	t.Cleanup(func() { spawnExitHook = nil })

	if err := br.StartSpawn(job.ID, defects); err != nil {
		t.Fatalf("a profile write failure must not fail the spawn: %v", err)
	}
	waitExit(t, exited)

	args, _ := os.ReadFile(argv)
	if !strings.Contains(string(args), "--sandbox workspace") || strings.Contains(string(args), "dd-simulator") {
		t.Fatalf("want the base profile, got:\n%s", args)
	}
	live := br.GetJob(job.ID)
	if live == nil || !strings.Contains(strings.Join(live.Log, "\n"), "job profile failed — skipped") {
		t.Fatalf("want the fail-soft warning in the log, got %+v", live)
	}
}

// Resume after a run that tampered (or planted a symlink, before this
// profile denied its own dir): the resumed session starts on a fresh profile,
// and the rewrite does not write through the link.
func TestResumeStartsOnAFreshProfile(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	f := newRestartFixture(t, "failed", StoppedByExit,
		"cat .grok/sandbox.toml > '"+seen+"'\n"+artifactsBody("DEF-20260923-restart-0001"), true)
	isolateGrokHome(t)
	// GROK_HOME moved grok's session store too; the session lives there now.
	writeJobSession(t, f.br, f.job.ID, testSessionID)
	enableSimulatorWrites(t, f.br, "workspace")

	precious := filepath.Join(t.TempDir(), "zshrc")
	if err := os.WriteFile(precious, []byte(`read_write = ["/"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	grokDir := filepath.Join(f.br.handoffDir(f.job.ID), ".grok")
	if err := os.MkdirAll(grokDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(precious, filepath.Join(grokDir, "sandbox.toml")); err != nil {
		t.Fatal(err)
	}

	if _, err := f.br.Restart(f.job.ID, f.defects, true); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)

	if b, _ := os.ReadFile(precious); string(b) != `read_write = ["/"]` {
		t.Fatalf("rewrite followed the planted symlink: target now %q", b)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("resumed agent never ran: %v", err)
	}
	if strings.Contains(string(got), `read_write = ["/"]`) || !strings.Contains(string(got), "deny = [") {
		t.Fatalf("resumed session ran on a stale profile:\n%s", got)
	}
	if calls := f.calls(t); len(calls) != 1 {
		t.Fatalf("want one resumed iteration, got %d", len(calls))
	} else if v, _ := flagValue(calls[0], "--sandbox"); v != "dd-simulator" {
		t.Fatalf("resume must keep the job profile name: %q", calls[0])
	}
}

func TestFormatGrokExit(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 3").Run()
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "Grok exited code=null signal=null"},
		{"exit status", exitErr, "Grok exited code=3 signal=null"},
		{"never started", errors.New("sandbox: job profile x was modified"), "sandbox: job profile x was modified"},
	} {
		if got := formatGrokExit(c.err); got != c.want {
			t.Fatalf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
