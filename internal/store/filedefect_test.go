package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/paths"
)

// Forces a failure after the evidence copy and asserts the defect row and its
// evidence directory do not remain. Also forces the conditional link to lose
// and asserts the same cleanup.
func TestFileDefectCleansUpAfterCopy(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: appID, Key: "file", Title: "File",
		Steps: []PlanStepIn{{Key: "login", Action: "Sign in", Expected: "Home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{AppID: appID, PlanID: plan.Plan.ID, Execute: "manual", Title: "Run"})
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

	t.Run("write fails", func(t *testing.T) {
		var saw string
		fileDefectAfterCopy = func(_ rower, defectID, _, _ string) error {
			saw = defectID
			dir := filepath.Join(paths.EvidenceDir(st.DefectsRoot), defectID)
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("evidence dir missing before failure: %v", err)
			}
			return errors.New("forced after copy")
		}
		t.Cleanup(func() { fileDefectAfterCopy = nil })
		_, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
		if err == nil || !strings.Contains(err.Error(), "forced after copy") {
			t.Fatalf("err %v", err)
		}
		assertNoFiledDefect(t, st, saw)
		if _, err := os.Stat(filepath.Join(paths.EvidenceDir(st.DefectsRoot), run.ID)); err != nil {
			t.Fatalf("run evidence removed: %v", err)
		}
		got, err := GetRun(st.DB, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Steps[0].DefectID != nil {
			t.Fatalf("step linked: %+v", got.Steps[0])
		}
	})

	t.Run("link loses", func(t *testing.T) {
		var saw string
		fileDefectAfterCopy = func(q rower, defectID, runID, stepKey string) error {
			saw = defectID
			_, err := q.Exec(`UPDATE test_step_results SET defect_id = ? WHERE run_id = ? AND step_key = ?`,
				"DEF-winner", runID, stepKey)
			return err
		}
		t.Cleanup(func() { fileDefectAfterCopy = nil })
		_, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
		if !isOp(err, 409) || !strings.Contains(err.Error(), "step already has defect DEF-winner") {
			t.Fatalf("err %v", err)
		}
		assertNoFiledDefect(t, st, saw)
		got, err := GetRun(st.DB, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Steps[0].DefectID != nil {
			t.Fatalf("rolled-back link stuck: %+v", got.Steps[0].DefectID)
		}
	})
}

func assertNoFiledDefect(t *testing.T, st *Store, id string) {
	t.Helper()
	if !IsSafeID(id) {
		t.Fatalf("defect id %q", id)
	}
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM defects`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("defect rows %d", n)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM defects_fts WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fts rows %d", n)
	}
	if _, err := os.Stat(filepath.Join(paths.EvidenceDir(st.DefectsRoot), id)); !os.IsNotExist(err) {
		t.Fatalf("evidence dir for %s remains: %v", id, err)
	}
}
