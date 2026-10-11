package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuggestShipMessageUsesDefectTitleAndAgentFix(t *testing.T) {
	handoff := t.TempDir()
	if err := os.Mkdir(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	note := "# DEF-1\n\n**Fix:** Transparent `Material` on the dialog builders and the login version label.\n"
	if err := os.WriteFile(filepath.Join(handoff, "fix-notes", "DEF-1.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	got := suggestShipMessage([]string{"Dialog text uses the error style.\n\nEvery dialog"}, handoff)
	if !strings.HasPrefix(got, "Dialog text uses the error style.") {
		t.Fatalf("subject: %q", got)
	}
	if !strings.Contains(got, "Transparent Material on the dialog builders") {
		t.Fatalf("body: %q", got)
	}
	if strings.Contains(got, "Every dialog") || strings.Contains(got, "`") {
		t.Fatalf("message kept junk: %q", got)
	}
}

func TestSuggestShipMessageFallsBackToNotes(t *testing.T) {
	handoff := t.TempDir()
	notes := "# batch\n\n### What was done\n- Wrapped the dialogs in Material.\n- Wrapped the login label.\n\n### Could not verify\n- Simulator.\n"
	if err := os.WriteFile(filepath.Join(handoff, "NOTES.md"), []byte(notes), 0o644); err != nil {
		t.Fatal(err)
	}
	got := suggestShipMessage(nil, handoff)
	if !strings.Contains(got, "Wrapped the dialogs in Material.") || strings.Contains(got, "Simulator") {
		t.Fatalf("notes fallback: %q", got)
	}
}
