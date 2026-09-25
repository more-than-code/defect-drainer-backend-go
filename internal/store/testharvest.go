package store

import (
	"database/sql"
	"encoding/json"

	"github.com/joe/defect-drainer-go/internal/db"
)

// HarvestedImage is one screenshot imported onto a step during a test-job harvest.
type HarvestedImage struct {
	Filename string
	Data     []byte
}

// HarvestedStep is one step update applied by a test-job harvest.
// Exploratory creates the step when it does not already exist.
// Images are appended. A pass with no images in this harvest is stored as unverified.
type HarvestedStep struct {
	Key         string
	Status      string
	Actual      string
	Exploratory bool
	Action      string
	Expected    string
	HasExpected bool
	Section     string
	Images      []HarvestedImage
}

// HarvestSummary is the count line a test-job harvest logs.
type HarvestSummary struct {
	Applied  bool
	ByStatus map[string]int
	Images   int
}

// SetRunJob records the executing job id on a run.
func SetRunJob(sqlDB *sql.DB, runID, jobID string) error {
	if jobID == "" {
		return badRequest("job_id is required")
	}
	return withTx(sqlDB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE test_runs SET job_id = ?, updated_at = ? WHERE id = ?`, jobID, db.NowIso(), runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("test run not found")
		}
		return nil
	})
}

// SetRunRefs replaces refs_json. Callers pass the resolved SHAs.
func SetRunRefs(sqlDB *sql.DB, runID string, refs []RunRef) error {
	if refs == nil {
		refs = []RunRef{}
	}
	raw, err := jsonMarshalRefs(refs)
	if err != nil {
		return err
	}
	return withTx(sqlDB, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE test_runs SET refs_json = ?, updated_at = ? WHERE id = ?`, raw, db.NowIso(), runID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound("test run not found")
		}
		return nil
	})
}

func jsonMarshalRefs(refs []RunRef) (string, error) {
	b, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MarkRunInProgress sets a queued, in_progress, or awaiting_review run to
// in_progress, stamps started_at, and clears finished_at. Reviewed, abandoned,
// and deleted stay refused by the caller before this runs.
func MarkRunInProgress(sqlDB *sql.DB, runID string) error {
	return withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, false)
		if err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "in_progress" && run.Status != "awaiting_review" {
			return conflictErr("run is not in_progress")
		}
		ts := db.NowIso()
		_, err = tx.Exec(`UPDATE test_runs SET status = 'in_progress', started_at = ?, finished_at = NULL, updated_at = ? WHERE id = ?`, ts, ts, runID)
		return err
	})
}

// AbandonAgentRun abandons a queued or in_progress run after test-job setup fails.
// A run already past those states is left as it is.
func AbandonAgentRun(sqlDB *sql.DB, runID string) error {
	return withTx(sqlDB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, false)
		if err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "in_progress" {
			return nil
		}
		_, err = tx.Exec(`UPDATE test_runs SET status = 'abandoned', updated_at = ? WHERE id = ?`, db.NowIso(), runID)
		return err
	})
}

// ApplyTestHarvest writes step results and their new screenshots in one transaction.
// It does nothing when the run is missing or not queued or in_progress.
// Files written before a failed commit are removed.
func (s *Store) ApplyTestHarvest(runID string, steps []HarvestedStep) (HarvestSummary, error) {
	var summary HarvestSummary
	summary.ByStatus = map[string]int{}
	var written []string
	err := withTx(s.DB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, true)
		if err != nil {
			return err
		}
		if run.Status != "queued" && run.Status != "in_progress" {
			return nil
		}
		summary.Applied = true
		byKey := map[string]RunStep{}
		pos := 1
		for _, step := range run.Steps {
			byKey[step.Key] = step
			if step.Position >= pos {
				pos = step.Position + 1
			}
		}
		ts := db.NowIso()
		for _, in := range steps {
			if !validKey(in.Key) {
				continue
			}
			status := in.Status
			if status == "pass" && len(in.Images) == 0 {
				status = "unverified"
			}
			cur, exists := byKey[in.Key]
			var ev []string
			if exists {
				ev = append([]string{}, cur.Evidence...)
			}
			if len(in.Images) > 0 {
				files := make([]struct {
					Filename string
					Data     []byte
				}, 0, len(in.Images))
				for _, img := range in.Images {
					files = append(files, struct {
						Filename string
						Data     []byte
					}{img.Filename, img.Data})
				}
				paths, err := s.writeNumberedEvidence(runID, "s-"+in.Key+"-", files)
				written = append(written, paths...)
				if err != nil {
					return err
				}
				ev = append(ev, paths...)
				summary.Images += len(paths)
			}
			if ev == nil {
				ev = []string{}
			}
			if !exists {
				if !in.Exploratory {
					continue
				}
				action := in.Action
				if action == "" {
					action = in.Key
				}
				if _, err := tx.Exec(`INSERT INTO test_step_results (
					run_id, step_key, position, section, action, expected, exploratory,
					status, actual, evidence_json, defect_id, review, review_note, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, NULL, '', '', ?)`,
					runID, in.Key, pos, in.Section, action, in.Expected, status, in.Actual, db.JSONArray(ev), ts); err != nil {
					return err
				}
				pos++
				byKey[in.Key] = RunStep{Key: in.Key, Position: pos - 1, Evidence: ev, Exploratory: true}
			} else if in.Exploratory {
				action := in.Action
				if action == "" {
					action = cur.Action
				}
				expected := in.Expected
				if !in.HasExpected {
					expected = cur.Expected
				}
				if _, err := tx.Exec(`UPDATE test_step_results SET status = ?, actual = ?, action = ?, expected = ?, review = '', review_note = '', evidence_json = ?, updated_at = ? WHERE run_id = ? AND step_key = ?`,
					status, in.Actual, action, expected, db.JSONArray(ev), ts, runID, in.Key); err != nil {
					return err
				}
			} else {
				if _, err := tx.Exec(`UPDATE test_step_results SET status = ?, actual = ?, review = '', review_note = '', evidence_json = ?, updated_at = ? WHERE run_id = ? AND step_key = ?`,
					status, in.Actual, db.JSONArray(ev), ts, runID, in.Key); err != nil {
					return err
				}
			}
			summary.ByStatus[status]++
		}
		if _, err := tx.Exec(`UPDATE test_runs SET updated_at = ? WHERE id = ?`, ts, runID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		s.removeEvidenceFiles(written)
		return HarvestSummary{}, err
	}
	return summary, nil
}
