package store

import "testing"

func TestAppIDFromSeedMatchesTS(t *testing.T) {
	web := AppIDFromSeed("tutored-webapp")
	mobile := AppIDFromSeed("tutored-mobileapp")
	if web != SeededTutoredWebappAppID || !IsSafeAppID(web) {
		t.Fatalf("web %s", web)
	}
	if mobile != SeededTutoredMobileAppID || !IsSafeAppID(mobile) {
		t.Fatalf("mobile %s", mobile)
	}
	if CanonicalizeAppID("tutored") != web {
		t.Fatal("legacy tutored")
	}
	if CanonicalizeAppID("tutored-mobile") != mobile {
		t.Fatal("legacy mobile")
	}
	if CanonicalizeAppID("") != web {
		t.Fatal("empty")
	}
}

func TestParseGitRefNameHostile(t *testing.T) {
	if ParseGitRefName("--upload-pack=evil", "main") != "main" {
		t.Fatal("hostile")
	}
	if ParseGitRefName("dev", "main") != "dev" {
		t.Fatal("dev")
	}
}

func TestParseGrokSandbox(t *testing.T) {
	if ParseGrokSandbox("restrict") != "strict" {
		t.Fatal("restrict")
	}
	if ParseGrokSandbox("workspace") != "workspace" {
		t.Fatal("workspace")
	}
}

func TestIsSafeID(t *testing.T) {
	if !IsSafeID("DEF-20260817-x-abcd") {
		t.Fatal("valid")
	}
	if IsSafeID("../etc/passwd") {
		t.Fatal("path")
	}
}
