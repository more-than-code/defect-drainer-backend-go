package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joe/defect-drainer-go/internal/paths"
)

func TestFileDefectFTSFailureRollsBack(t *testing.T) {
	st, run := failStepWithEvidence(t, "fts")
	called := false
	fileDefectFTSHook = func(rower, string) error {
		called = true
		return errors.New("sqlite: injected fts failure UNIQUE constraint failed")
	}
	t.Cleanup(func() { fileDefectFTSHook = nil })
	_, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
	if !called {
		t.Fatal("fts hook was not used")
	}
	var op *OpError
	if !errors.As(err, &op) || op.Status != 500 || op.Msg != "could not file defect" {
		t.Fatalf("err %#v", err)
	}
	if strings.Contains(err.Error(), "sqlite") || strings.Contains(err.Error(), "injected") {
		t.Fatalf("driver text leaked: %v", err)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM defects`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("defects %d %v", n, err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM defects_fts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fts %d %v", n, err)
	}
	got, err := GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].DefectID != nil {
		t.Fatalf("linked %+v", got.Steps[0].DefectID)
	}
	entries, _ := os.ReadDir(paths.EvidenceDir(st.DefectsRoot))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "DEF-") {
			t.Fatalf("defect evidence remains %s", e.Name())
		}
	}
}

func TestFileDefectIDCollisionLeavesExisting(t *testing.T) {
	st, run := failStepWithEvidence(t, "collide")
	const taken = "DEF-20260925-taken-aaaa"
	if !IsSafeID(taken) {
		t.Fatal(taken)
	}
	if _, err := st.Write(DefectRecord{
		ID: taken, AppID: SeededTutoredWebappAppID, Title: "preexisting", Status: "open",
		Bucket: "open", Source: "console", Summary: "keep me", Body: "original body",
		Repos: []string{}, Labels: []string{}, Related: []string{}, KeyFiles: []string{},
		Evidence: []string{"evidence/" + taken + "/01.png"}, FixEvidence: []string{},
		Reported: "2026-09-01",
	}); err != nil {
		t.Fatal(err)
	}
	ev := paths.EvidenceDir(st.DefectsRoot)
	if err := os.MkdirAll(filepath.Join(ev, taken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ev, taken, "01.png"), []byte("original-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	fileDefectNewID = func(title string, now time.Time) string {
		calls++
		if calls == 1 {
			return taken
		}
		if calls > 6 {
			t.Fatal("id attempts did not stop")
		}
		return MakeDefectID(title, now)
	}
	t.Cleanup(func() { fileDefectNewID = nil })

	got, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("attempts %d", calls)
	}
	if got.ID == taken || !IsSafeID(got.ID) {
		t.Fatalf("new id %s", got.ID)
	}
	kept, err := st.Get(taken)
	if err != nil || kept == nil || kept.Title != "preexisting" || kept.Body != "original body" {
		t.Fatalf("preexisting changed %+v %v", kept, err)
	}
	b, err := os.ReadFile(filepath.Join(ev, taken, "01.png"))
	if err != nil || string(b) != "original-bytes" {
		t.Fatalf("preexisting evidence %q %v", b, err)
	}
}

func TestFileDefectDestinationSymlinkNotFollowed(t *testing.T) {
	st, run := failStepWithEvidence(t, "symlink-dest")
	const taken = "DEF-20260925-link-aaaa"
	ev := paths.EvidenceDir(st.DefectsRoot)
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ev, taken)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fileDefectNewID = func(title string, now time.Time) string {
		calls++
		if calls == 1 {
			return taken
		}
		if calls > 6 {
			t.Fatal("id attempts did not stop")
		}
		return MakeDefectID(title, now)
	}
	t.Cleanup(func() { fileDefectNewID = nil })

	got, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || got.ID == taken {
		t.Fatalf("calls %d id %s", calls, got.ID)
	}
	fi, err := os.Lstat(filepath.Join(ev, taken))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %+v", err, fi)
	}
	b, err := os.ReadFile(filepath.Join(target, "keep.txt"))
	if err != nil || string(b) != "sentinel" {
		t.Fatalf("sentinel %q %v", b, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "keep.txt" {
			t.Fatalf("wrote %s through symlink", e.Name())
		}
	}
	if _, err := st.Get(taken); err != nil {
		t.Fatal(err)
	}
	row, err := st.Get(taken)
	if err != nil || row != nil {
		t.Fatalf("insert of the symlink id stuck %+v %v", row, err)
	}
}

func TestFileDefectEvidenceRefusesLinksAndSize(t *testing.T) {
	secret := []byte("PRIVATE KEY")
	cases := []struct {
		name  string
		plant func(t *testing.T, st *Store, run TestRun)
	}{
		{"run dir symlink", plantRunDirSymlink},
		{"intermediate symlink", plantIntermediateSymlink},
		{"hardlink", plantHardlink},
		{"oversize", plantOversize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, run := failStepWithEvidence(t, "ev")
			tc.plant(t, st, run)
			before := defectCount(t, st)
			_, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
			var op *OpError
			if !errors.As(err, &op) || op.Status != 500 || op.Msg != "could not read step evidence" {
				t.Fatalf("err %#v", err)
			}
			if strings.Contains(err.Error(), st.DefectsRoot) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("leaked %v", err)
			}
			if defectCount(t, st) != before {
				t.Fatal("defect row created")
			}
			got, err := GetRun(st.DB, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Steps[0].DefectID != nil {
				t.Fatal("step linked")
			}
			if strings.Contains(string(secret), "unused") {
				t.Fatal(secret)
			}
		})
	}
}

func TestFileDefectMultibyteTitleCap(t *testing.T) {
	st := newPlanStore(t)
	action := strings.Repeat("😀", 80)
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: SeededTutoredWebappAppID, Key: "mb", Title: "MB",
		Steps: []PlanStepIn{{Key: "login", Action: action, Expected: "Home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{AppID: SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	fail := "fail"
	if _, err := PatchRunStep(st.DB, run.ID, "login", &fail, nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("😀", 50)
	if got.Title != want || len(got.Title) > 200 || len([]rune(got.Title)) > 80 {
		t.Fatalf("title runes %d bytes %d", len([]rune(got.Title)), len(got.Title))
	}
}

func failStepWithEvidence(t *testing.T, key string) (*Store, TestRun) {
	t.Helper()
	st := newPlanStore(t)
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: SeededTutoredWebappAppID, Key: key, Title: "File",
		Steps: []PlanStepIn{{Key: "login", Action: "Sign in", Expected: "Home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{
		AppID: SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual", Title: "Run",
	})
	if err != nil {
		t.Fatal(err)
	}
	fail := "fail"
	if _, err := PatchRunStep(st.DB, run.ID, "login", &fail, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := AddRunStepEvidence(st, run.ID, "login", []struct {
		Filename string
		Data     []byte
	}{{Filename: "a.png", Data: []byte("hello-evidence")}}); err != nil {
		t.Fatal(err)
	}
	return st, run
}

func defectCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM defects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func plantRunDirSymlink(t *testing.T, st *Store, run TestRun) {
	t.Helper()
	ev := paths.EvidenceDir(st.DefectsRoot)
	runDir := filepath.Join(ev, run.ID)
	real := runDir + "-real"
	if err := os.Rename(runDir, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, runDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "s-login-01.png"), []byte("DIR LINK"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func plantIntermediateSymlink(t *testing.T, st *Store, run TestRun) {
	t.Helper()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(paths.EvidenceDir(st.DefectsRoot), run.ID, "sub")
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE test_step_results SET evidence_json = ? WHERE run_id = ? AND step_key = 'login'`,
		`["evidence/`+run.ID+`/sub/id_rsa"]`, run.ID); err != nil {
		t.Fatal(err)
	}
}

func plantHardlink(t *testing.T, st *Store, run TestRun) {
	t.Helper()
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := paths.EvidenceDir(st.DefectsRoot)
	ents, err := os.ReadDir(filepath.Join(ev, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, e := range ents {
		if !e.IsDir() {
			name = e.Name()
			break
		}
	}
	if name == "" {
		t.Fatal("no evidence file")
	}
	dst := filepath.Join(ev, run.ID, name)
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, dst); err != nil {
		t.Fatal(err)
	}
}

func plantOversize(t *testing.T, st *Store, run TestRun) {
	t.Helper()
	ev := paths.EvidenceDir(st.DefectsRoot)
	ents, err := os.ReadDir(filepath.Join(ev, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, e := range ents {
		if !e.IsDir() {
			name = e.Name()
			break
		}
	}
	f, err := os.OpenFile(filepath.Join(ev, run.ID, name), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((25 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
}
