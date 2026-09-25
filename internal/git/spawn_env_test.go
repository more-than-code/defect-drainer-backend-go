package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartCodingAgentStripsInheritedTestSecrets(t *testing.T) {
	const hostPassword = "host-password-should-not-pass"
	const appPassword = "app-password-should-not-pass"
	const injected = "S3cr3t-Value-42"
	t.Setenv("DD_TEST_ACCOUNT_PASSWORD", hostPassword)
	t.Setenv("QA_PASSWORD", appPassword)
	proof := filepath.Join(t.TempDir(), "proof")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	script := "#!/bin/sh\n" +
		"if [ -z \"$DD_TEST_ACCOUNT_PASSWORD\" ]; then echo DD=UNSET; else echo DD=$DD_TEST_ACCOUNT_PASSWORD; fi > '" + proof + "'\n" +
		"if [ -z \"$QA_PASSWORD\" ]; then echo QA=UNSET; else echo QA=$QA_PASSWORD; fi >> '" + proof + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-env", t.TempDir(), wts, "strict", nil, nil, &CodingAgentOpts{
		StripEnv: []string{"QA_PASSWORD"},
		ExtraEnv: map[string]string{"DD_TEST_ACCOUNT_PASSWORD": injected},
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
	got, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, hostPassword) || strings.Contains(text, appPassword) {
		t.Fatal("child inherited a host test secret")
	}
	if !strings.Contains(text, "DD="+injected) || !strings.Contains(text, "QA=UNSET") {
		t.Fatalf("child env proof:\n%s", text)
	}
}

func TestStartCodingAgentDropsStripKeysReintroducedByExtraEnv(t *testing.T) {
	const injected = "S3cr3t-Value-42"
	const leaked = "extra-should-not-pass"
	proof := filepath.Join(t.TempDir(), "proof")
	bin := filepath.Join(t.TempDir(), "fake-grok")
	script := "#!/bin/sh\n" +
		"if [ -z \"$DD_TEST_ACCOUNT_PASSWORD\" ]; then echo DD=UNSET; else echo DD=$DD_TEST_ACCOUNT_PASSWORD; fi > '" + proof + "'\n" +
		"if [ -z \"$QA_PASSWORD\" ]; then echo QA=UNSET; else echo QA=$QA_PASSWORD; fi >> '" + proof + "'\n" +
		"printf '%s\\n' \"$PATH\" >> '" + proof + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}
	cmd, flush, err := StartCodingAgent(handoff, "BATCH-restrip", t.TempDir(), wts, "strict", nil, nil, &CodingAgentOpts{
		StripEnv: []string{"QA_PASSWORD", "DD_TEST_ACCOUNT_PASSWORD", "PATH"},
		ExtraEnv: map[string]string{
			"QA_PASSWORD":              leaked,
			"DD_TEST_ACCOUNT_PASSWORD": injected,
			"PATH":                     "/only-from-extra",
		},
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
	got, err := os.ReadFile(proof)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, leaked) || strings.Contains(text, "/only-from-extra") || !strings.Contains(text, "DD="+injected) || !strings.Contains(text, "QA=UNSET") {
		t.Fatalf("child env proof:\n%s", text)
	}
}

func TestContinuePromptOverrideLeavesDefaultUntouched(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "fake-grok")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsPath + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BUILD_BIN", bin)
	handoff := t.TempDir()
	wts := []WorktreeBinding{{Repo: "demo", WorktreeAbs: t.TempDir(), PrimaryAbs: t.TempDir(), Branch: "b"}}

	run := func(opts *CodingAgentOpts) string {
		t.Helper()
		_ = os.Remove(argsPath)
		cmd, flush, err := StartCodingAgent(handoff, "BATCH-prompt", t.TempDir(), wts, "strict", nil, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if flush != nil {
			flush()
		}
		b, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	def := run(&CodingAgentOpts{Continue: true})
	if !strings.Contains(def, "NOTES.md") || !strings.Contains(def, "fix-evidence") {
		t.Fatalf("default continue prompt changed:\n%s", def)
	}
	over := run(&CodingAgentOpts{Continue: true, ContinuePrompt: "CONTINUE-MARKER results/<key>.md evidence/<key>/<file>"})
	if !strings.Contains(over, "CONTINUE-MARKER results/<key>.md") {
		t.Fatalf("override missing:\n%s", over)
	}
	if strings.Contains(over, "NOTES.md") {
		t.Fatal("override still sent the batch continue prompt")
	}
	full := run(&CodingAgentOpts{Prompt: "TEST-PROMPT-ONLY"})
	if !strings.Contains(full, "TEST-PROMPT-ONLY") || strings.Contains(full, "Batch defect fix session") {
		t.Fatalf("prompt override:\n%s", full)
	}
}
