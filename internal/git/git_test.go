package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseGitRepoURLAllowlist(t *testing.T) {
	ok, err := ParseGitRepoURL("https://github.com/example/ttd-webapp.git")
	if err != nil || ok.Name != "ttd-webapp" {
		t.Fatalf("%+v %v", ok, err)
	}
	if _, err := ParseGitRepoURL("https://github.com/ex/../evil.git"); err == nil {
		t.Fatal("dotdot")
	}
	if _, err := ParseGitRepoURL("https://github.com/ex/foo.git\n-c core.sshCommand=evil"); err == nil {
		t.Fatal("newline")
	}
	if _, err := ParseGitRepoURL(""); err == nil {
		t.Fatal("empty")
	}
	local, err := ParseGitRepoURL("/Users/joe/workspace/tutored/ttd-webapp")
	if err != nil || local.Source != "local" {
		t.Fatalf("%+v %v", local, err)
	}
}

func TestListRepoBranchesEmptyLocation(t *testing.T) {
	_, err := ListRepoBranches("local", "")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAcceptPRURL(t *testing.T) {
	if got := AcceptPRURL("https://github.com/example/demo/pull/7"); got == "" {
		t.Fatal("https URL rejected")
	}
	if AcceptPRURL("-https://github.com/example/demo/pull/7") != "" {
		t.Fatal("leading-dash URL accepted")
	}
	if AcceptPRURL("http://github.com/example/demo/pull/7") != "" {
		t.Fatal("http URL accepted")
	}
	if AcceptPRURL("https:///nohost") != "" {
		t.Fatal("empty host accepted")
	}
	if AcceptPRURL("ftp://example.com/x") != "" {
		t.Fatal("ftp URL accepted")
	}
	if createdPRURL([]byte("Creating pull request\nhttps://github.com/ex/r/pull/7\n")) != "https://github.com/ex/r/pull/7" {
		t.Fatal("createdPRURL missed last https token")
	}
	if createdPRURL([]byte("done -evil")) != "" {
		t.Fatal("createdPRURL accepted leading-dash last token")
	}
}

func TestRefreshPRStatusesUsesNumberNotRawURL(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gh.log")
	gh := filepath.Join(dir, "fake-gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\ncase \"$*\" in\n  *pr\\ view*) echo '{\"state\":\"OPEN\",\"mergedAt\":\"\",\"number\":7,\"url\":\"https://github.com/example/demo/pull/7\"}'; exit 0 ;;\nesac\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_BIN", gh)
	out := RefreshPRStatuses([]PRResult{{
		Repo: "demo", Number: 7, URL: "-evil", Status: "created",
	}}, nil)
	if len(out) != 1 || out[0].URL != "https://github.com/example/demo/pull/7" {
		t.Fatalf("refresh %+v", out)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "pr view 7") {
		t.Fatalf("expected pr view <number>, got %q", s)
	}
	if strings.Contains(s, "-evil") {
		t.Fatalf("raw URL passed to gh: %q", s)
	}
}

func TestRefreshPRStatusesDoesNotPassNonHTTPSURL(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "gh.log")
	gh := filepath.Join(dir, "fake-gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_BIN", gh)
	out := RefreshPRStatuses([]PRResult{{
		Repo: "demo", URL: "http://evil.example/x", Status: "created",
	}}, nil)
	if len(out) != 1 || out[0].URL != "" {
		t.Fatalf("http URL should be stripped: %+v", out)
	}
	if _, err := os.Stat(logPath); err == nil {
		raw, _ := os.ReadFile(logPath)
		if strings.Contains(string(raw), "http://evil.example") {
			t.Fatalf("non-https URL passed to gh: %s", raw)
		}
	}
}

func TestChooseFolderNonDarwinMessage(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript would open Finder")
	}
	_, err := ChooseLocalFolder()
	if err == nil || err.Error() != "Finder folder picker is only available on macOS" {
		t.Fatalf("got %v", err)
	}
}
