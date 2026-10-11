package store

import (
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// failRower fails the test on any statement: on Postgres writeFiledFTS must not
// touch the SQLite-only defects_fts table.
type failRower struct{ t *testing.T }

func (f failRower) Exec(query string, _ ...any) (sql.Result, error) {
	f.t.Fatalf("unexpected Exec on Postgres: %s", query)
	return nil, nil
}

func (f failRower) Query(query string, _ ...any) (*sql.Rows, error) {
	f.t.Fatalf("unexpected Query on Postgres: %s", query)
	return nil, nil
}

func (f failRower) QueryRow(query string, _ ...any) *sql.Row {
	f.t.Fatalf("unexpected QueryRow on Postgres: %s", query)
	return nil
}

func TestWriteFiledFTSSkipsPostgres(t *testing.T) {
	// sql.Open does not dial. Do not Ping.
	pgDB, err := sql.Open("pgx-qmark", "postgres://postgres:unit-test-pw@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pgDB.Close()
	fileDefectFTSHook = func(rower, string) error {
		t.Fatal("FTS hook ran on Postgres")
		return nil
	}
	t.Cleanup(func() { fileDefectFTSHook = nil })
	if err := writeFiledFTS(pgDB, failRower{t}, DefectRecord{ID: "DEF-20261012-pg-abcd"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFiledFTSIndexesOnSQLite(t *testing.T) {
	st := newPlanStore(t)
	rec := DefectRecord{ID: "DEF-20261012-fts-abcd", AppID: SeededTutoredWebappAppID, Title: "Login button", Summary: "s", Body: "b"}
	for i := 0; i < 2; i++ {
		if err := writeFiledFTS(st.DB, st.DB, rec); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var title, summary, body string
	if err := st.DB.QueryRow(`SELECT COUNT(*), MAX(title), MAX(summary), MAX(body) FROM defects_fts WHERE id = ?`, rec.ID).Scan(&n, &title, &summary, &body); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("defects_fts rows for %s = %d, want 1", rec.ID, n)
	}
	if title != rec.Title || summary != rec.Summary || body != rec.Body {
		t.Fatalf("defects_fts row = %q/%q/%q, want %q/%q/%q", title, summary, body, rec.Title, rec.Summary, rec.Body)
	}
}

// TestFileDefectRetriesOnPostgresUniqueViolation drives the ID-collision retry
// with the error pgx returns for a duplicate key. The first insert fails with
// SQLSTATE 23505; filing must roll back to the savepoint and try a new ID.
func TestFileDefectRetriesOnPostgresUniqueViolation(t *testing.T) {
	st, run := failStepNoEvidence(t, "pg-collide")
	var ids []string
	fileDefectInsertHook = func(id string) error {
		ids = append(ids, id)
		if len(ids) == 1 {
			return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
		}
		return nil
	}
	t.Cleanup(func() { fileDefectInsertHook = nil })
	got, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] || got.ID != ids[1] {
		t.Fatalf("attempts %v, filed %s", ids, got.ID)
	}
	if n := defectCount(t, st); n != 1 {
		t.Fatalf("defects = %d, want 1", n)
	}
}

// A driver error that is not a unique violation must not retry.
func TestFileDefectDoesNotRetryOtherPostgresErrors(t *testing.T) {
	st, run := failStepNoEvidence(t, "pg-other")
	calls := 0
	fileDefectInsertHook = func(string) error {
		calls++
		return &pgconn.PgError{Code: "23503", Message: "violates foreign key constraint"}
	}
	t.Cleanup(func() { fileDefectInsertHook = nil })
	if _, _, err := FileRunStepDefect(st, run.ID, "login", FileDefectInput{}); !isOp(err, 500) {
		t.Fatalf("err %v", err)
	}
	if calls != 1 {
		t.Fatalf("attempts %d, want 1", calls)
	}
	if n := defectCount(t, st); n != 0 {
		t.Fatalf("defects = %d, want 0", n)
	}
}

// failStepNoEvidence returns a run whose "login" step failed without evidence,
// so filing does not read evidence files.
func failStepNoEvidence(t *testing.T, key string) (*Store, TestRun) {
	t.Helper()
	st := newPlanStore(t)
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: SeededTutoredWebappAppID, Key: key, Title: "File",
		Steps: []PlanStepIn{{Key: "login", Action: "Sign in", Expected: "Home"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{AppID: SeededTutoredWebappAppID, PlanID: plan.Plan.ID, Execute: "manual", Title: "Run"})
	if err != nil {
		t.Fatal(err)
	}
	fail := "fail"
	if _, err := PatchRunStep(st.DB, run.ID, "login", &fail, nil); err != nil {
		t.Fatal(err)
	}
	return st, run
}
