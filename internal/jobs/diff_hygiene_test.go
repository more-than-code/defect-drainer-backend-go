package jobs

import "testing"

func TestParseDiffHunksReflowVsTokenChange(t *testing.T) {
	// Unified-0 fixture. Do not implement this test via `git diff -w`:
	// `-w` treats the first hunk as a change because it is line-oriented.
	diff := "" +
		"diff --git a/a.dart b/a.dart\n" +
		"--- a/a.dart\n" +
		"+++ b/a.dart\n" +
		"@@ -1,3 +1 @@\n" +
		"-foo\n" +
		"-  bar\n" +
		"-baz\n" +
		"+foo bar baz\n" +
		"@@ -10 +10 @@\n" +
		"-oldToken\n" +
		"+newToken\n"
	hunks := ParseDiffHunks(diff)
	if len(hunks) != 2 {
		t.Fatalf("hunks=%d %+v", len(hunks), hunks)
	}
	if hunks[0].File != "a.dart" {
		t.Fatalf("file %q", hunks[0].File)
	}
	if !hunks[0].Reflow {
		t.Fatalf("joined-whitespace hunk should be reflow: %+v", hunks[0])
	}
	if hunks[1].Reflow {
		t.Fatalf("token change must not be reflow: %+v", hunks[1])
	}
}

func TestCollectHunksRejectsDashSha(t *testing.T) {
	_, _, _, err := CollectHunks(t.TempDir(), "--output=/tmp/x", "all", 10)
	if err == nil || err.Error() != "invalid diff baseline" {
		t.Fatalf("got %v", err)
	}
}
