package git

import (
	"runtime"
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

func TestChooseFolderNonDarwinMessage(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("osascript would open Finder")
	}
	_, err := ChooseLocalFolder()
	if err == nil || err.Error() != "Finder folder picker is only available on macOS" {
		t.Fatalf("got %v", err)
	}
}
