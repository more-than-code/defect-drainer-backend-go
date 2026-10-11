package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/paths"
)

func newPlanStore(t *testing.T) *Store {
	t.Helper()
	data := t.TempDir()
	defects := t.TempDir()
	sqlDB, err := db.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := EnsureSeededApps(sqlDB); err != nil {
		t.Fatal(err)
	}
	return NewStore(sqlDB, defects)
}

func twoSteps() []PlanStepIn {
	return []PlanStepIn{
		{Key: "login", Section: "auth", Action: "Sign in", Expected: "Home"},
		{Key: "logout", Action: "Sign out", Expected: "Login"},
	}
}

func TestPlanUpsertVersionReorderAndUnarchive(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	in := UpsertPlanInput{
		AppID: appID, Key: "smoke", Title: "Smoke", Purpose: "happy path",
		CreatedBy: "ada", Env: map[string]any{"tier": "staging"}, Steps: twoSteps(),
	}
	first, err := UpsertPlan(st.DB, in)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || !first.Changed || first.Plan.Version != 1 {
		t.Fatalf("create %+v", first)
	}
	if first.Plan.Steps[0].Position != 1 || first.Plan.Steps[1].Key != "logout" {
		t.Fatalf("steps %+v", first.Plan.Steps)
	}
	if !IsSafePlanID(first.Plan.ID) {
		t.Fatal(first.Plan.ID)
	}

	again, err := UpsertPlan(st.DB, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Changed || again.Plan.Version != 1 || again.Plan.ID != first.Plan.ID {
		t.Fatalf("unchanged %+v", again)
	}

	in.Steps = []PlanStepIn{twoSteps()[1], twoSteps()[0]}
	reordered, err := UpsertPlan(st.DB, in)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.Created || !reordered.Changed || reordered.Plan.Version != 2 {
		t.Fatalf("reorder %+v", reordered)
	}
	if reordered.Plan.Steps[0].Key != "logout" || reordered.Plan.Steps[0].Position != 1 {
		t.Fatalf("order %+v", reordered.Plan.Steps)
	}

	run, err := CreateRun(st.DB, CreateRunInput{AppID: appID, PlanID: first.Plan.ID, Execute: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	archived, deleted, err := DeletePlan(st.DB, first.Plan.ID)
	if err != nil || !archived || deleted {
		t.Fatalf("archive archived=%v deleted=%v err=%v", archived, deleted, err)
	}
	listed, err := ListPlans(st.DB, appID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range listed {
		if p.ID == first.Plan.ID {
			t.Fatal("archived plan listed")
		}
	}
	in.Steps = []PlanStepIn{twoSteps()[1], twoSteps()[0]}
	back, err := UpsertPlan(st.DB, in)
	if err != nil {
		t.Fatal(err)
	}
	if back.Created || back.Changed || back.Plan.Version != 2 || back.Plan.Archived {
		t.Fatalf("unarchive %+v", back)
	}
	if run.PlanVersion == nil || *run.PlanVersion != 2 {
		t.Fatalf("run snapped version %+v", run.PlanVersion)
	}
}

func TestRunSnapshotIgnoresLaterPlanEdit(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: appID, Key: "snap", Title: "Snap", Steps: []PlanStepIn{{Key: "only", Action: "Do the thing", Expected: "Done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{
		AppID: appID, PlanID: plan.Plan.ID, Execute: "manual",
		Env: map[string]any{"tier": "local"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "in_progress" || run.PlanVersion == nil || *run.PlanVersion != 1 {
		t.Fatalf("run %+v", run)
	}
	if run.Steps[0].Status != "pending" || run.Steps[0].Action != "Do the thing" {
		t.Fatalf("snapshot %+v", run.Steps)
	}
	_, err = UpsertPlan(st.DB, UpsertPlanInput{
		AppID: appID, Key: "snap", Title: "Snap", Steps: []PlanStepIn{{Key: "only", Action: "Do something else", Expected: "Done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].Action != "Do the thing" || got.PlanVersion == nil || *got.PlanVersion != 1 {
		t.Fatalf("run changed with plan: %+v", got.Steps)
	}
	edited, err := GetPlan(st.DB, plan.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version != 2 || edited.Steps[0].Action != "Do something else" {
		t.Fatalf("plan %+v", edited)
	}
}

func TestExploratoryKeyCollision(t *testing.T) {
	st := newPlanStore(t)
	run, err := CreateRun(st.DB, CreateRunInput{AppID: SeededTutoredWebappAppID, Execute: "manual", Title: "Explore"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Title != "Explore" || run.PlanID != nil {
		t.Fatalf("planless %+v", run)
	}
	first, err := AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Sign in", Expected: "Home"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Steps) != 1 || first.Steps[0].Key != "x-sign-in" || !first.Steps[0].Exploratory {
		t.Fatalf("first %+v", first.Steps)
	}
	second, err := AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Sign in!!"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Steps[1].Key != "x-sign-in-2" {
		t.Fatalf("suffix %+v", second.Steps)
	}
	third, err := AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Other", Key: "custom-1"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Steps[2].Key != "custom-1" || third.Steps[2].Position != 3 {
		t.Fatalf("client key %+v", third.Steps)
	}
	_, err = AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Nope", Key: "custom-1"})
	if !isOp(err, 400) {
		t.Fatalf("collision err %v", err)
	}
}

func TestSaveAsPlanCreateAndReplace(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	run, err := CreateRun(st.DB, CreateRunInput{AppID: appID, Execute: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Title != "Exploratory run" {
		t.Fatal(run.Title)
	}
	run, err = AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Sign in", Expected: "Home", Section: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := SaveRunAsPlan(st.DB, run.ID, SaveAsPlanInput{Key: "from-run", Title: "From run"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || !created.Changed || created.Plan.Version != 1 || len(created.Plan.Steps) != 1 {
		t.Fatalf("create %+v", created)
	}
	if created.Plan.Steps[0].Key != "x-sign-in" || created.Plan.Steps[0].Action != "Sign in" {
		t.Fatalf("steps %+v", created.Plan.Steps)
	}
	_, err = SaveRunAsPlan(st.DB, run.ID, SaveAsPlanInput{Key: "from-run", Title: "Again"})
	if !isOp(err, 409) {
		t.Fatalf("dup key %v", err)
	}
	run, err = AppendRunStep(st.DB, run.ID, PlanStepIn{Action: "Sign out"})
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := SaveRunAsPlan(st.DB, run.ID, SaveAsPlanInput{PlanID: created.Plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Created || !replaced.Changed || replaced.Plan.Version != 2 || len(replaced.Plan.Steps) != 2 {
		t.Fatalf("replace %+v steps=%d", replaced, len(replaced.Plan.Steps))
	}
	same, err := SaveRunAsPlan(st.DB, run.ID, SaveAsPlanInput{PlanID: created.Plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if same.Changed || same.Plan.Version != 2 {
		t.Fatalf("replace unchanged %+v", same)
	}
}

func TestFinishConvertsPassWithoutEvidence(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{
		AppID: appID, Key: "finish", Title: "Finish",
		Steps: []PlanStepIn{
			{Key: "seen", Action: "Seen"},
			{Key: "bare", Action: "Bare"},
			{Key: "wait", Action: "Wait"},
			{Key: "broke", Action: "Broke"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{AppID: appID, PlanID: plan.Plan.ID, Execute: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	pass := "pass"
	fail := "fail"
	if _, err := PatchRunStep(st.DB, run.ID, "seen", &pass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchRunStep(st.DB, run.ID, "bare", &pass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PatchRunStep(st.DB, run.ID, "broke", &fail, nil); err != nil {
		t.Fatal(err)
	}
	run, err = AddRunStepEvidence(st, run.ID, "seen", []struct {
		Filename string
		Data     []byte
	}{{Filename: "shot.png", Data: []byte("png")}})
	if err != nil {
		t.Fatal(err)
	}
	unverified := "unverified"
	_, err = PatchRunStep(st.DB, run.ID, "bare", &unverified, nil)
	if !isOp(err, 400) {
		t.Fatalf("client unverified %v", err)
	}
	done, err := FinishRun(st.DB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "awaiting_review" || done.FinishedAt == nil {
		t.Fatalf("finish %+v", done)
	}
	got := map[string]RunStep{}
	for _, s := range done.Steps {
		got[s.Key] = s
	}
	if got["seen"].Status != "pass" || len(got["seen"].Evidence) != 1 {
		t.Fatalf("seen %+v", got["seen"])
	}
	if !strings.Contains(got["seen"].Evidence[0], "/s-seen-01.png") {
		t.Fatal(got["seen"].Evidence)
	}
	if got["bare"].Status != "unverified" {
		t.Fatalf("bare %+v", got["bare"])
	}
	if got["wait"].Status != "pending" || got["broke"].Status != "fail" {
		t.Fatalf("others %+v %+v", got["wait"], got["broke"])
	}
	if _, err := FinishRun(st.DB, run.ID); !isOp(err, 409) {
		t.Fatalf("second finish %v", err)
	}
}

func TestOpenTwiceKeepsPlanAndAccountsColumn(t *testing.T) {
	data := t.TempDir()
	sqlDB, err := db.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureSeededApps(sqlDB); err != nil {
		t.Fatal(err)
	}
	res, err := UpsertPlan(sqlDB, UpsertPlanInput{
		AppID: SeededTutoredWebappAppID, Key: "keep", Title: "Keep",
		Steps: []PlanStepIn{{Key: "only", Action: "Stay"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	sqlDB, err = db.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	got, err := GetPlan(sqlDB, res.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Steps[0].Action != "Stay" {
		t.Fatalf("reopen %+v", got)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('apps') WHERE name = 'test_accounts_json'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("column count %d", n)
	}

	legacy := t.TempDir()
	raw, err := sql.Open("sqlite", paths.DbPath(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE apps (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := db.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err = db.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	n = 0
	if err := opened.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('apps') WHERE name = 'test_accounts_json'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migrated column count %d", n)
	}
}

func isOp(err error, status int) bool {
	var op *OpError
	return errors.As(err, &op) && op.Status == status
}

// TestFinishEvidenceCheckMatchesJSONArrayLength pins the evidence check that
// FinishRun used to do with SQLite's json_array_length(evidence_json) = 0, for
// strict JSON.
func TestFinishEvidenceCheckMatchesJSONArrayLength(t *testing.T) {
	st := newPlanStore(t)
	appID := SeededTutoredWebappAppID
	cases := map[string]struct {
		raw  string
		want string
	}{
		"empty":   {raw: `[]`, want: "unverified"},
		"spaced":  {raw: ` [ ] `, want: "unverified"},
		"object":  {raw: `{"a":1}`, want: "unverified"},
		"scalar":  {raw: `"x"`, want: "unverified"},
		"null":    {raw: `null`, want: "unverified"},
		"one":     {raw: `["evidence/r/s-one-01.png"]`, want: "pass"},
		"nonstrs": {raw: `[1]`, want: "pass"},
		"nested":  {raw: `[[]]`, want: "pass"},
		"bignum":  {raw: `[1e309]`, want: "pass"},
	}
	var keys []string
	for key := range cases {
		keys = append(keys, key)
	}
	run := newPassRun(t, st, appID, "finish-json", keys)
	for key, c := range cases {
		addPassStep(t, st, run, key, c.raw)
	}
	done, err := FinishRun(st.DB, run)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range done.Steps {
		seen[s.Key] = true
		if want := cases[s.Key].want; s.Status != want {
			t.Errorf("%s: status %q, want %q", s.Key, s.Status, want)
		}
	}
	for key := range cases {
		if !seen[key] {
			t.Errorf("%s: step missing from finished run", key)
		}
	}
}

// TestFinishRejectsMalformedEvidenceJSON covers malformed JSON and JSON5 that
// SQLite would accept. Only strict JSON is supported, so finishing fails and
// the whole transaction rolls back, including a sibling step without evidence.
func TestFinishRejectsMalformedEvidenceJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":      `[`,
		"json5-trailing": `[1,]`,
		"extra-bracket":  `[]]`,
	} {
		t.Run(name, func(t *testing.T) {
			st := newPlanStore(t)
			run := newPassRun(t, st, SeededTutoredWebappAppID, "finish-bad", []string{"bad", "bare"})
			addPassStep(t, st, run, "bad", raw)
			addPassStep(t, st, run, "bare", `[]`)
			_, err := FinishRun(st.DB, run)
			if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
				t.Fatalf("finish with evidence_json %q: %v", raw, err)
			}
			got, err := loadRun(st.DB, run, true)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "in_progress" || len(got.Steps) != 2 {
				t.Fatalf("rolled back %+v", got)
			}
			for _, s := range got.Steps {
				if s.Status != "pass" {
					t.Fatalf("%s: status %q after failed finish", s.Key, s.Status)
				}
			}
		})
	}
}

// newPassRun creates an in_progress manual run on a plan with the given step keys.
func newPassRun(t *testing.T, st *Store, appID, key string, stepKeys []string) string {
	t.Helper()
	var steps []PlanStepIn
	for _, k := range stepKeys {
		steps = append(steps, PlanStepIn{Key: k, Action: k})
	}
	plan, err := UpsertPlan(st.DB, UpsertPlanInput{AppID: appID, Key: key, Title: key, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	run, err := CreateRun(st.DB, CreateRunInput{AppID: appID, PlanID: plan.Plan.ID, Execute: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// addPassStep marks a step pass and sets its evidence_json to raw.
func addPassStep(t *testing.T, st *Store, runID, key, raw string) {
	t.Helper()
	pass := "pass"
	if _, err := PatchRunStep(st.DB, runID, key, &pass, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE test_step_results SET evidence_json = ? WHERE run_id = ? AND step_key = ?`,
		raw, runID, key); err != nil {
		t.Fatal(err)
	}
}
