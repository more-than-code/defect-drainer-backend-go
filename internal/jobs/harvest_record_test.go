package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

func newHarvestFixture(t *testing.T, ids ...string) (*BatchRunner, *store.Store, *Job, string) {
	t.Helper()
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	for _, id := range ids {
		writeInProgressDefect(t, st, id)
	}
	job := &Job{
		ID: "bjob_harvestrec", Kind: "batch", Status: "completed",
		BatchID: "BATCH-20260924-hr", AppID: store.SeededTutoredWebappAppID,
		DefectIDs: append([]string(nil), ids...),
	}
	br.jobs[job.ID] = job
	return br, st, job, filepath.Join(data, "batch-jobs", job.ID)
}

func writeFixImage(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileBirth(t *testing.T, path string) time.Time {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s has no syscall.Stat_t", path)
	}
	born, has := fileBirthTime(f, sys)
	if !has {
		t.Fatalf("birthtime unavailable for %s", path)
	}
	return born
}

// writeImageNewerThan sets the job's import cutoff just after olderPath's
// birth, then writes name. Harvest imports name and skips olderPath.
func writeImageNewerThan(t *testing.T, job *Job, dir, name, olderPath string) string {
	t.Helper()
	job.RunStartedAt = fileBirth(t, olderPath).Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)
	cutoff, err := time.Parse(time.RFC3339Nano, job.RunStartedAt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if !fileBirth(t, path).Before(cutoff) {
			return path
		}
		time.Sleep(50 * time.Millisecond)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("could not create an image newer than the import cutoff")
	return ""
}

func harvestRuns(t *testing.T, br *BatchRunner, id string) []HarvestRun {
	t.Helper()
	j := br.GetJob(id)
	if j == nil {
		t.Fatalf("job %s missing", id)
	}
	return j.Harvests
}

func onlyDefect(t *testing.T, br *BatchRunner, id string) HarvestDefect {
	t.Helper()
	runs := harvestRuns(t, br, id)
	if len(runs) != 1 || len(runs[0].Defects) != 1 {
		t.Fatalf("harvests %+v", runs)
	}
	if _, err := time.Parse(time.RFC3339, runs[0].At); err != nil {
		t.Fatalf("at %q: %v", runs[0].At, err)
	}
	return runs[0].Defects[0]
}

func TestHarvestRecordResolvedImages(t *testing.T) {
	t.Run("nil verification", func(t *testing.T) {
		assertHarvestResolved(t, nil, nil)
	})
	t.Run("passing verification", func(t *testing.T) {
		pass := []VerifyResult{{Repo: "demo", Command: "true", Ok: true, ExitCode: intPtr(0)}}
		run := &VerificationRun{Ran: true, Results: pass}
		base := &VerificationRun{Ran: true, Results: pass}
		assertHarvestResolved(t, &HarvestOpts{Verification: run, AllowResolve: true}, base)
	})
}

func assertHarvestResolved(t *testing.T, opts *HarvestOpts, baseline *VerificationRun) {
	t.Helper()
	if opts == nil {
		opts = &HarvestOpts{AllowResolve: true}
	}
	id := "DEF-20260924-hr-ok-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	job.Baseline = baseline
	dir := filepath.Join(handoff, "fix-evidence", id)
	writeFixImage(t, dir, "a.png", "png-a")
	writeFixImage(t, dir, "b.png", "png-b")
	writeFixImage(t, dir, ".hidden.png", "no")
	br.harvestFixEvidence(job, handoff, opts)

	got, err := st.Get(id)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("status %+v %v", got, err)
	}
	rec := onlyDefect(t, br, job.ID)
	if rec.Outcome != harvestResolved || rec.Reason != "" {
		t.Fatalf("%+v", rec)
	}
	if len(rec.Images) != 2 || !reflect.DeepEqual(rec.Images, got.FixEvidence) {
		t.Fatalf("images %#v fix_evidence %#v", rec.Images, got.FixEvidence)
	}
	for _, img := range rec.Images {
		if !strings.HasPrefix(img, "evidence/"+id+"/fix-") {
			t.Fatalf("image %q", img)
		}
	}
	raw, err := os.ReadFile(filepath.Join(handoff, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"harvests"`) || !strings.Contains(string(raw), rec.Images[0]) {
		t.Fatalf("job.json missing the run: %s", raw)
	}
	view := br.GetJobView(job.ID)
	if !reflect.DeepEqual(view.Harvests, harvestRuns(t, br, job.ID)) {
		t.Fatalf("view stripped a real image: %+v", view.Harvests)
	}
	for _, j := range br.ListView() {
		if j.ID == job.ID && j.Harvests != nil {
			t.Fatalf("list carried harvests: %+v", j.Harvests)
		}
	}
}

func TestHarvestRecordRegression(t *testing.T) {
	id := "DEF-20260924-hr-reg-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	job.Baseline = &VerificationRun{Ran: true, Results: []VerifyResult{
		{Repo: "demo", Command: "true", Ok: true, ExitCode: intPtr(0)},
	}}
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", id), "fix-01.png", "png")
	run := &VerificationRun{Ran: true, Results: []VerifyResult{
		{Repo: "demo", Command: "true", Ok: false, ExitCode: intPtr(1)},
	}}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{Verification: run, AllowResolve: true})

	got, err := st.Get(id)
	if err != nil || got == nil || got.Status == "resolved" {
		t.Fatalf("failing verdict must not resolve: %+v %v", got, err)
	}
	if len(got.FixEvidence) == 0 {
		t.Fatal("evidence should still be imported")
	}
	rec := onlyDefect(t, br, job.ID)
	want := summarizeVerdict(judgeVerification(run, job.Baseline))
	if rec.Outcome != harvestRegression || rec.Reason != want || want == "" {
		t.Fatalf("outcome %+v want reason %q", rec, want)
	}
	if !reflect.DeepEqual(rec.Images, got.FixEvidence) {
		t.Fatalf("images %#v fix_evidence %#v", rec.Images, got.FixEvidence)
	}
}

func TestHarvestRecordPreExistingStillResolves(t *testing.T) {
	id := "DEF-20260924-hr-pre-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	red := VerifyResult{Repo: "demo", Command: "true", Ok: false, ExitCode: intPtr(1)}
	job.Baseline = &VerificationRun{Ran: true, Results: []VerifyResult{red}}
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", id), "fix-01.png", "png")
	run := &VerificationRun{Ran: true, Results: []VerifyResult{red}}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{Verification: run, AllowResolve: true})
	got, err := st.Get(id)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("pre-existing red must still resolve: %+v %v", got, err)
	}
	if rec := onlyDefect(t, br, job.ID); rec.Outcome != harvestResolved {
		t.Fatalf("%+v", rec)
	}
}

func TestHarvestRecordFailedRunOutcomes(t *testing.T) {
	img := "DEF-20260924-hr-img-0001"
	pic := "DEF-20260924-hr-pic-0001"
	note := "DEF-20260924-hr-note-0001"
	none := "DEF-20260924-hr-none-0001"
	missing := "DEF-20260924-hr-miss-0001"
	br, st, job, handoff := newHarvestFixture(t, img, pic, note, none)
	job.DefectIDs = append(job.DefectIDs, missing, "not a defect")

	writeFixImage(t, filepath.Join(handoff, "fix-evidence", img), "fix-01.png", "png")
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", pic), "fix-01.png", "png")
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{img, "tried"}, {note, "notes only"}, {missing, "gone"}} {
		if err := os.WriteFile(filepath.Join(handoff, "fix-notes", pair[0]+".md"), []byte(pair[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{})

	runs := harvestRuns(t, br, job.ID)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	const deny = "this run may not resolve (failed, cancelled, or incomplete artifacts)"
	want := []HarvestDefect{
		{DefectID: img, Outcome: harvestNotResolved, Reason: deny},
		{DefectID: pic, Outcome: harvestNotResolved, Reason: deny},
		{DefectID: note, Outcome: harvestNotesOnly},
		{DefectID: none, Outcome: harvestNoEvidence},
		{DefectID: missing, Outcome: harvestError, Reason: "defect not found"},
	}
	if len(runs[0].Defects) != len(want) {
		t.Fatalf("defects %+v", runs[0].Defects)
	}
	for i, d := range runs[0].Defects {
		if d.DefectID != want[i].DefectID || d.Outcome != want[i].Outcome || d.Reason != want[i].Reason {
			t.Fatalf("defect %d = %+v want %+v", i, d, want[i])
		}
	}
	imgRec, err := st.Get(img)
	if err != nil || imgRec == nil {
		t.Fatal(err)
	}
	if len(runs[0].Defects[0].Images) != 1 || !reflect.DeepEqual(runs[0].Defects[0].Images, imgRec.FixEvidence) {
		t.Fatalf("images %#v evidence %#v", runs[0].Defects[0].Images, imgRec.FixEvidence)
	}
	if len(runs[0].Defects[1].Images) != 1 {
		t.Fatalf("image-only record %+v", runs[0].Defects[1])
	}
	if len(runs[0].Defects[2].Images) != 0 || len(runs[0].Defects[3].Images) != 0 {
		t.Fatalf("notes/empty carried images: %+v", runs[0].Defects)
	}
	for _, id := range []string{img, pic, note, none} {
		got, err := st.Get(id)
		if err != nil || got == nil || got.Status == "resolved" {
			t.Fatalf("%s should stay open: %+v %v", id, got, err)
		}
	}
}

func TestHarvestRecordTwoRunsKeepTheFirst(t *testing.T) {
	id := "DEF-20260924-hr-two-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	dir := filepath.Join(handoff, "fix-evidence", id)
	firstPath := filepath.Join(dir, "shot-a.png")
	writeFixImage(t, dir, "shot-a.png", "a")
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	first := onlyDefect(t, br, job.ID)
	if first.Outcome != harvestResolved || len(first.Images) != 1 {
		t.Fatalf("first %+v", first)
	}

	writeImageNewerThan(t, job, dir, "shot-b.png", firstPath)
	br.harvestFixEvidence(br.GetJob(job.ID), handoff, &HarvestOpts{})
	runs := harvestRuns(t, br, job.ID)
	if len(runs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	if runs[0].Defects[0].Outcome != harvestResolved || !reflect.DeepEqual(runs[0].Defects[0].Images, first.Images) {
		t.Fatalf("first run changed: %+v", runs[0])
	}
	second := runs[1].Defects[0]
	if second.Outcome != harvestNotResolved || len(second.Images) != 1 || second.Images[0] == first.Images[0] {
		t.Fatalf("second %+v", second)
	}
	got, err := st.Get(id)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.FixEvidence, []string{first.Images[0], second.Images[0]}) {
		t.Fatalf("fix_evidence %#v", got.FixEvidence)
	}
}

func TestHarvestRecordCapsAtTwenty(t *testing.T) {
	id := "DEF-20260924-hr-cap-0001"
	br, _, job, handoff := newHarvestFixture(t, id)
	dir := filepath.Join(handoff, "fix-evidence", id)
	for i := 1; i <= harvestRunCap+1; i++ {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		writeFixImage(t, dir, "shot.png", fmt.Sprintf("png-%d", i))
		br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	}
	runs := harvestRuns(t, br, job.ID)
	if len(runs) != harvestRunCap {
		t.Fatalf("len %d", len(runs))
	}
	if got := runs[0].Defects[0].Images; len(got) != 1 || !strings.HasSuffix(got[0], "fix-02.png") {
		t.Fatalf("oldest kept %#v, want the second run", got)
	}
	if got := runs[len(runs)-1].Defects[0].Images; len(got) != 1 || !strings.HasSuffix(got[0], "fix-21.png") {
		t.Fatalf("newest %#v", got)
	}
	for _, run := range runs {
		for _, img := range run.Defects[0].Images {
			if strings.HasSuffix(img, "fix-01.png") {
				t.Fatal("oldest run was kept")
			}
		}
	}
}

func TestHarvestRecordRestartKeepsRuns(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	body := "while [ ! -f '" + release + "' ]; do sleep 0.05; done\nexit 0"
	f := newRestartFixture(t, "failed", StoppedByExit, body, false)
	t.Setenv("DEFECT_DRAINER_AGENT_MAX_ITERS", "1")
	prior := HarvestRun{At: "2026-09-01T00:00:00Z", Defects: []HarvestDefect{{
		DefectID: f.defID, Outcome: harvestNotesOnly,
	}}}
	f.job.Harvests = []HarvestRun{prior}
	f.job.Verification = &VerificationRun{Ran: true, StartedAt: storedBaseline}
	f.job.DiffHygiene = &DiffHygieneReport{}

	got, err := f.br.Restart(f.job.ID, f.defects, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != nil || got.DiffHygiene != nil {
		t.Fatalf("restart cleared the wrong fields: verification %+v hygiene %+v", got.Verification, got.DiffHygiene)
	}
	if len(got.Harvests) != 1 || got.Harvests[0].At != prior.At || got.Harvests[0].Defects[0].Outcome != harvestNotesOnly {
		t.Fatalf("restart dropped harvests: %+v", got.Harvests)
	}
	if err := os.WriteFile(release, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitExit(t, f.exited)
}

func TestHarvestRecordSkipsWhenCancelled(t *testing.T) {
	id := "DEF-20260924-hr-can-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	job.Status = "cancelled"
	job.Harvests = []HarvestRun{{At: "2020-01-01T00:00:00Z", Defects: []HarvestDefect{{
		DefectID: id, Outcome: harvestNoEvidence,
	}}}}
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", id), "fix-01.png", "png")
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	got, err := st.Get(id)
	if err != nil || got == nil || got.Status != "resolved" {
		t.Fatalf("cancel must not change resolve: %+v %v", got, err)
	}
	runs := harvestRuns(t, br, job.ID)
	if len(runs) != 1 || runs[0].At != "2020-01-01T00:00:00Z" {
		t.Fatalf("cancelled harvest appended or cleared: %+v", runs)
	}
}

func TestHarvestRecordFillHarvests(t *testing.T) {
	const (
		keep  = "DEF-20260924-keep-0001"
		also  = "DEF-20260924-also-0001"
		other = "DEF-20260924-other-0001"
	)
	st, _, _ := testStore(t)
	br := NewBatchRunner(st, st.DefectsRoot)
	evidence := paths.EvidenceDir(st.DefectsRoot)
	for _, id := range []string{keep, also} {
		if err := os.MkdirAll(filepath.Join(evidence, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	good := filepath.Join(evidence, keep, "fix-01.png")
	if err := os.WriteFile(good, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, filepath.Join(evidence, keep, "fix-02.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, also, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	longReason := strings.Repeat("a", 499) + "é" + strings.Repeat("b", 10000)
	if len(longReason) < 10000 {
		t.Fatalf("reason fixture len %d", len(longReason))
	}

	runs := make([]HarvestRun, 25)
	goodImage := "evidence/" + keep + "/fix-01.png"
	for i := range runs {
		runs[i] = HarvestRun{
			At: fmt.Sprintf("2020-01-01T00:%02d:00Z", i),
			Defects: []HarvestDefect{{
				DefectID: keep, Outcome: harvestResolved, Images: []string{goodImage},
			}},
		}
	}
	runs[0].Defects[0].Reason = "oldest"
	runs[23].Defects = []HarvestDefect{{
		DefectID: other, Outcome: harvestResolved,
		Images: []string{"evidence/" + other + "/fix-01.png"},
	}}
	runs[24].Defects = []HarvestDefect{
		{DefectID: "nope", Outcome: harvestResolved},
		{DefectID: other, Outcome: harvestResolved, Images: []string{"evidence/" + other + "/fix-01.png"}},
		{
			DefectID: keep, Outcome: harvestResolved, Reason: longReason,
			Images: []string{
				goodImage,
				"evidence/" + keep + "/../x/fix-01.png",
				"evidence/" + other + "/fix-01.png",
				"evidence/" + keep + "/01.png",
				"evidence/" + keep + "/fix-03.png",
				"evidence/" + keep + "/fix-02.png",
			},
		},
		{
			DefectID: also, Outcome: "bogus", Reason: "nope",
			Images: []string{"evidence/" + also + "/fix-01.png"},
		},
	}
	job := &Job{
		ID: "bjob_forged", Kind: "batch", Status: "completed",
		DefectIDs: []string{keep, also, "nope"},
		Harvests:  runs,
	}
	br.jobs[job.ID] = job

	view := br.GetJobView(job.ID)
	if view == nil {
		t.Fatal("missing view")
	}
	// Last 20 of the 25 are indexes 5..24. Index 23 has no allowed defect, so it drops.
	if len(view.Harvests) != 19 {
		t.Fatalf("view runs %d", len(view.Harvests))
	}
	if view.Harvests[0].At != "2020-01-01T00:05:00Z" {
		t.Fatalf("oldest kept %s", view.Harvests[0].At)
	}
	for _, run := range view.Harvests {
		if run.At == "2020-01-01T00:00:00Z" || run.At == "2020-01-01T00:23:00Z" {
			t.Fatalf("dropped run survived: %s", run.At)
		}
	}
	last := view.Harvests[len(view.Harvests)-1]
	if last.At != "2020-01-01T00:24:00Z" || len(last.Defects) != 2 {
		t.Fatalf("newest %+v", last)
	}
	keepD := last.Defects[0]
	if keepD.DefectID != keep || keepD.Outcome != harvestResolved || len(keepD.Images) != 1 || keepD.Images[0] != goodImage {
		t.Fatalf("keep entry %+v", keepD)
	}
	if len(keepD.Reason) > harvestReasonMax || !utf8.ValidString(keepD.Reason) || keepD.Reason != strings.Repeat("a", 499) {
		t.Fatalf("reason %q", keepD.Reason)
	}
	alsoD := last.Defects[1]
	if alsoD.Outcome != harvestError || alsoD.Reason != "unrecognised outcome" || len(alsoD.Images) != 1 {
		t.Fatalf("unknown outcome %+v", alsoD)
	}

	live := br.jobs[job.ID]
	if len(live.Harvests) != 25 || live.Harvests[0].Defects[0].Reason != "oldest" {
		t.Fatal("live job was rewritten")
	}
	forged := live.Harvests[24]
	if forged.Defects[0].DefectID != "nope" || forged.Defects[1].DefectID != other {
		t.Fatalf("live defects %+v", forged.Defects)
	}
	if forged.Defects[2].Reason != longReason || len(forged.Defects[2].Images) != 6 || forged.Defects[3].Outcome != "bogus" {
		t.Fatal("live showcase entry was mutated")
	}

	var listed *Job
	for _, j := range br.ListView() {
		if j.ID == job.ID {
			listed = j
		}
	}
	if listed == nil || listed.Harvests != nil {
		t.Fatalf("list harvests %+v", listed)
	}
}

func TestHarvestRecordVerdictWithoutImagesIsNotesOnly(t *testing.T) {
	id := "DEF-20260924-hr-vn-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	job.Baseline = &VerificationRun{Ran: true, Results: []VerifyResult{
		{Repo: "demo", Command: "true", Ok: true, ExitCode: intPtr(0)},
	}}
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handoff, "fix-notes", id+".md"), []byte("a note"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &VerificationRun{Ran: true, Results: []VerifyResult{
		{Repo: "demo", Command: "true", Ok: false, ExitCode: intPtr(1)},
	}}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{Verification: run, AllowResolve: true})

	why := summarizeVerdict(judgeVerification(run, job.Baseline))
	rec := onlyDefect(t, br, job.ID)
	if rec.Outcome != harvestNotesOnly || rec.Reason != "verification: "+why+"; note not saved" || len(rec.Images) != 0 {
		t.Fatalf("%+v", rec)
	}
	logged := false
	for _, line := range br.jobs[job.ID].Log {
		if strings.Contains(line, "harvest "+id+": NOT resolved — "+why) {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("verdict log changed: %#v", br.jobs[job.ID].Log)
	}
	got, err := st.Get(id)
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Resolution != "" || got.Status == "resolved" {
		t.Fatalf("note was applied: %+v", got)
	}
}

func TestHarvestRecordHydrateCapsOversizedJob(t *testing.T) {
	const def = "DEF-20260924-huge-0001"
	st, data, _ := testStore(t)
	runs := make([]map[string]any, 1000)
	for i := range runs {
		runs[i] = map[string]any{
			"at": fmt.Sprintf("2020-06-01T00:%02d:%02dZ", i/60, i%60),
			"defects": []map[string]any{{
				"defectId": def, "outcome": "resolved", "reason": "early",
			}},
		}
	}
	runs[0]["at"] = "DROP-ME-OLDEST"
	images := make([]string, 1000)
	for i := range images {
		images[i] = fmt.Sprintf("evidence/%s/fix-%d.png", def, i+1)
	}
	repeated := make([]map[string]any, 1000)
	for i := range repeated {
		repeated[i] = map[string]any{"defectId": def, "outcome": "resolved", "reason": "early"}
	}
	repeated[999] = map[string]any{
		"defectId": def, "outcome": "notes_only",
		"reason": strings.Repeat("r", 2000), "images": images,
	}
	runs[999]["at"] = strings.Repeat("T", 80)
	runs[999]["defects"] = repeated

	dir := filepath.Join(data, "batch-jobs", "bjob_huge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"jobId": "bjob_huge", "kind": "batch", "status": "completed",
		"defect_ids": []string{def}, "harvests": runs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	br := NewBatchRunner(st, data)
	live := br.jobs["bjob_huge"]
	if live == nil {
		t.Fatal("job was not hydrated")
	}
	assertHarvestCaps(t, live.Harvests, 50)
	view := br.GetJobView("bjob_huge")
	if view == nil {
		t.Fatal("missing view")
	}
	assertHarvestCaps(t, view.Harvests, 0)
}

func assertHarvestCaps(t *testing.T, runs []HarvestRun, wantImages int) {
	t.Helper()
	if len(runs) != harvestRunCap {
		t.Fatalf("runs %d", len(runs))
	}
	for _, run := range runs {
		if run.At == "DROP-ME-OLDEST" || len(run.At) > 40 {
			t.Fatalf("at %q", run.At)
		}
		if len(run.Defects) != 1 {
			t.Fatalf("defects %d in %q", len(run.Defects), run.At)
		}
		d := run.Defects[0]
		if len(d.Reason) > harvestReasonMax || len(d.Images) > 50 {
			t.Fatalf("reason %d images %d", len(d.Reason), len(d.Images))
		}
	}
	last := runs[len(runs)-1]
	if last.Defects[0].Outcome != harvestNotesOnly || len(last.Defects[0].Images) != wantImages {
		t.Fatalf("last entry images=%d outcome=%s (want images %d)", len(last.Defects[0].Images), last.Defects[0].Outcome, wantImages)
	}
	if len(last.At) != 40 || len(last.Defects[0].Reason) != harvestReasonMax {
		t.Fatalf("at %d reason %d", len(last.At), len(last.Defects[0].Reason))
	}
}

func TestHarvestRecordRejectsLinkedEvidence(t *testing.T) {
	const (
		hard = "DEF-20260924-hard-0001"
		link = "DEF-20260924-slink-0001"
		real = "DEF-20260924-real-0001"
	)
	st, data, _ := testStore(t)
	br := NewBatchRunner(st, data)
	evidence := paths.EvidenceDir(st.DefectsRoot)

	hardDir := filepath.Join(evidence, hard)
	if err := os.MkdirAll(hardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(st.DefectsRoot, "secret.png")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(hardDir, "fix-01.png")); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(evidence, link)); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(evidence, real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, real, "fix-01.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	job := &Job{
		ID: "bjob_links", Kind: "batch", Status: "completed",
		DefectIDs: []string{hard, link, real},
		Harvests: []HarvestRun{{
			At: "2026-09-24T00:00:00Z",
			Defects: []HarvestDefect{
				{DefectID: hard, Outcome: harvestResolved, Images: []string{"evidence/" + hard + "/fix-01.png"}},
				{DefectID: link, Outcome: harvestResolved, Images: []string{"evidence/" + link + "/fix-01.png"}},
				{DefectID: real, Outcome: harvestResolved, Images: []string{"evidence/" + real + "/fix-01.png"}},
			},
		}},
	}
	br.jobs[job.ID] = job
	view := br.GetJobView(job.ID)
	if view == nil || len(view.Harvests) != 1 {
		t.Fatalf("view %+v", view)
	}
	got := map[string][]string{}
	for _, d := range view.Harvests[0].Defects {
		got[d.DefectID] = d.Images
	}
	if len(got[hard]) != 0 {
		t.Fatalf("hardlink kept: %v", got[hard])
	}
	if len(got[link]) != 0 {
		t.Fatalf("symlinked defect dir kept: %v", got[link])
	}
	if len(got[real]) != 1 {
		t.Fatalf("regular file dropped: %v", got[real])
	}
}

func TestHarvestRecordSaveEvidenceError(t *testing.T) {
	id := "DEF-20260924-hr-sev-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	ev := paths.EvidenceDir(st.DefectsRoot)
	if err := os.MkdirAll(ev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ev, id), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", id), "fix-01.png", "png")
	_, serr := st.SaveEvidence(id, []struct {
		Filename string
		Data     []byte
	}{{"fix-01.png", []byte("png")}}, "fix")
	if serr == nil {
		t.Fatal("setup: SaveEvidence should fail when the evidence dir is a file")
	}
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	rec := onlyDefect(t, br, job.ID)
	if rec.Outcome != harvestError || rec.Reason != serr.Error() || len(rec.Images) != 0 {
		t.Fatalf("got %+v want reason %q", rec, serr.Error())
	}
}

func TestHarvestRecordWriteErrorKeepsImages(t *testing.T) {
	id := "DEF-20260924-hr-wrr-0001"
	br, st, job, handoff := newHarvestFixture(t, id)
	if _, err := st.DB.Exec(`CREATE TRIGGER harvest_fail_write BEFORE UPDATE ON defects
		BEGIN SELECT RAISE(ABORT, 'forced write failure'); END`); err != nil {
		t.Fatal(err)
	}
	writeFixImage(t, filepath.Join(handoff, "fix-evidence", id), "fix-01.png", "png")
	br.harvestFixEvidence(job, handoff, &HarvestOpts{AllowResolve: true})
	rec := onlyDefect(t, br, job.ID)
	if rec.Outcome != harvestError || !strings.Contains(rec.Reason, "forced write failure") || len(rec.Images) != 1 {
		t.Fatalf("%+v", rec)
	}
	if rec.Images[0] != "evidence/"+id+"/fix-01.png" {
		t.Fatalf("images %v", rec.Images)
	}
	if _, err := os.Stat(filepath.Join(paths.EvidenceDir(st.DefectsRoot), id, "fix-01.png")); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(id)
	if err != nil || got == nil || got.Status == "resolved" || len(got.FixEvidence) != 0 {
		t.Fatalf("row should be unchanged: %+v %v", got, err)
	}
}
