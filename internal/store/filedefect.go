package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/paths"
)

// FileDefectInput is POST /api/test-runs/{id}/steps/{key}/file-defect.
// HasRepos distinguishes an omitted repos field from an explicit empty list.
type FileDefectInput struct {
	Title    string
	Severity string
	Repos    []string
	HasRepos bool
	Reporter string
	Note     string
}

// fileDefectAfterCopy runs inside the filing transaction after the defect row
// is inserted and its evidence directory exists. Tests use it to force a
// failure or to win the conditional link. Nil in production.
var fileDefectAfterCopy func(q rower, defectID, runID, stepKey string) error

// fileDefectNewID, when set, supplies the id for each filing attempt.
// Nil uses MakeDefectID.
var fileDefectNewID func(title string, now time.Time) string

// fileDefectFTSHook, when set, runs before the FTS write. A non-nil error
// fails the filing. Nil in production.
var fileDefectFTSHook func(q rower, id string) error

// fileDefectMaxBytes matches the upload cap (api maxFileBytes). A copied
// screenshot cannot be larger than one an operator could have uploaded.
const fileDefectMaxBytes int64 = 25 << 20

const fileDefectIDAttempts = 5

// FileRunStepDefect creates one open defect from a fail or blocked step,
// copies that step's screenshots, and links the step. The status check, copy,
// insert, and conditional link share one transaction. A failure rolls that
// transaction back and removes only an evidence directory this call created.
func FileRunStepDefect(s *Store, runID, key string, in FileDefectInput) (DefectRecord, TestRun, error) {
	var outDef DefectRecord
	var outRun TestRun
	var createdDir string
	err := withTx(s.DB, func(tx *sql.Tx) error {
		run, err := loadRun(tx, runID, true)
		if err != nil {
			return err
		}
		var step *RunStep
		for i := range run.Steps {
			if run.Steps[i].Key == key {
				step = &run.Steps[i]
				break
			}
		}
		if step == nil {
			return notFound("step not found")
		}
		if run.Status == "abandoned" {
			return conflictErr("run is abandoned")
		}
		if step.Status != "fail" && step.Status != "blocked" {
			return badRequest("step must be fail or blocked")
		}
		if step.DefectID != nil {
			return conflictErr("step already has defect " + *step.DefectID)
		}
		app, err := requireApp(tx, run.AppID)
		if err != nil {
			return err
		}
		title, err := fileDefectTitle(in.Title, step.Action)
		if err != nil {
			return err
		}
		severity, err := fileDefectSeverity(in.Severity)
		if err != nil {
			return err
		}
		reporter, err := fileDefectReporter(in.Reporter, run.ExecutedBy)
		if err != nil {
			return err
		}
		note, err := trimLimit("note", in.Note, maxTextLen, false)
		if err != nil {
			return err
		}
		repos, err := fileDefectRepos(app, run.Refs, in.Repos, in.HasRepos)
		if err != nil {
			return err
		}
		files, err := s.readRunEvidence(run.ID, step.Evidence)
		if err != nil {
			return err
		}
		ts := db.NowIso()
		base := DefectRecord{
			AppID: run.AppID, Title: title, Severity: severity, Status: "open",
			Area: "other", Client: ClientHintForApp(app), Surface: "",
			Repos: repos, Labels: []string{}, Related: []string{run.ID + "/steps/" + step.Key},
			Source: "test-run", Reporter: reporter, KeyFiles: []string{},
			FixEvidence: []string{}, Reported: db.TodayUTC(),
			Summary: stepDefectSummary(step.Status, step.Action), Bucket: "open",
		}
		var filed DefectRecord
		placed := false
		for attempt := 0; attempt < fileDefectIDAttempts; attempt++ {
			if _, err := tx.Exec(`SAVEPOINT file_defect_id`); err != nil {
				return &OpError{Status: 500, Msg: "could not file defect"}
			}
			id := newFileDefectID(title, time.Now())
			if !IsSafeID(id) {
				rollbackFileDefectSavepoint(tx)
				continue
			}
			rec := base
			rec.ID = id
			rec.Evidence = plannedEvidencePaths(id, files)
			rec.Body = defectBodyFromStep(run, *step, note, rec.Evidence)
			if err := insertFiledDefect(tx, rec, ts); err != nil {
				rollbackFileDefectSavepoint(tx)
				if isUniqueViolation(err) {
					continue
				}
				return &OpError{Status: 500, Msg: "could not file defect"}
			}
			if len(files) > 0 {
				dir := filepath.Join(paths.EvidenceDir(s.DefectsRoot), id)
				if err := os.Mkdir(dir, 0o755); err != nil {
					rollbackFileDefectSavepoint(tx)
					if errors.Is(err, os.ErrExist) {
						continue
					}
					return &OpError{Status: 500, Msg: "could not store evidence"}
				}
				createdDir = dir
				if err := writeEvidenceFiles(dir, files); err != nil {
					return &OpError{Status: 500, Msg: "could not store evidence"}
				}
			}
			if _, err := tx.Exec(`RELEASE file_defect_id`); err != nil {
				return &OpError{Status: 500, Msg: "could not file defect"}
			}
			filed = rec
			placed = true
			break
		}
		if !placed {
			return &OpError{Status: 500, Msg: "could not file defect"}
		}
		if err := writeFiledFTS(tx, filed); err != nil {
			return &OpError{Status: 500, Msg: "could not file defect"}
		}
		if fileDefectAfterCopy != nil {
			if err := fileDefectAfterCopy(tx, filed.ID, run.ID, step.Key); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`UPDATE test_step_results SET defect_id = ?, updated_at = ?
			WHERE run_id = ? AND step_key = ? AND defect_id IS NULL`, filed.ID, ts, run.ID, step.Key)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var cur sql.NullString
			_ = tx.QueryRow(`SELECT defect_id FROM test_step_results WHERE run_id = ? AND step_key = ?`, run.ID, step.Key).Scan(&cur)
			if cur.Valid && cur.String != "" {
				return conflictErr("step already has defect " + cur.String)
			}
			return conflictErr("step already has defect")
		}
		if _, err := tx.Exec(`UPDATE test_runs SET updated_at = ? WHERE id = ?`, ts, run.ID); err != nil {
			return err
		}
		saved, err := scanDefect(tx.QueryRow(`SELECT `+defectSelect+` FROM defects WHERE id = ?`, filed.ID))
		if err != nil {
			return err
		}
		outRun, err = loadRun(tx, run.ID, true)
		if err != nil {
			return err
		}
		outDef = saved
		return nil
	})
	if err != nil {
		if createdDir != "" {
			_ = os.RemoveAll(createdDir)
		}
		return DefectRecord{}, TestRun{}, err
	}
	return outDef, outRun, nil
}

func newFileDefectID(title string, now time.Time) string {
	if fileDefectNewID != nil {
		if id := fileDefectNewID(title, now); id != "" {
			return id
		}
	}
	return MakeDefectID(title, now)
}

func rollbackFileDefectSavepoint(tx *sql.Tx) {
	_, _ = tx.Exec(`ROLLBACK TO file_defect_id`)
	_, _ = tx.Exec(`RELEASE file_defect_id`)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func writeFiledFTS(q rower, rec DefectRecord) error {
	if fileDefectFTSHook != nil {
		if err := fileDefectFTSHook(q, rec.ID); err != nil {
			return err
		}
	}
	if _, err := q.Exec(`DELETE FROM defects_fts WHERE id = ?`, rec.ID); err != nil {
		return err
	}
	_, err := q.Exec(`INSERT INTO defects_fts (id, app_id, title, summary, body) VALUES (?,?,?,?,?)`,
		rec.ID, rec.AppID, rec.Title, rec.Summary, rec.Body)
	return err
}

func plannedEvidencePaths(id string, files []struct {
	Filename string
	Data     []byte
}) []string {
	if len(files) == 0 {
		return []string{}
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = fmt.Sprintf("evidence/%s/%02d%s", id, i+1, evidenceExt(f.Filename))
	}
	return out
}

func writeEvidenceFiles(dir string, files []struct {
	Filename string
	Data     []byte
}) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for i, file := range files {
		name := fmt.Sprintf("%02d%s", i+1, evidenceExt(file.Filename))
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return err
		}
		_, werr := f.Write(file.Data)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

func insertFiledDefect(tx *sql.Tx, rec DefectRecord, ts string) error {
	_, err := tx.Exec(`INSERT INTO defects (
		id, app_id, title, severity, status, area, client, surface,
		repos_json, labels_json, related_json, source, reporter, key_files_json,
		evidence_json, fix_evidence_json, reported, summary, body,
		resolution, resolved_date, duplicate_of, bucket, created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.AppID, rec.Title, rec.Severity, rec.Status, rec.Area, rec.Client, rec.Surface,
		db.JSONArray(rec.Repos), db.JSONArray(rec.Labels), db.JSONArray(rec.Related),
		rec.Source, rec.Reporter, db.JSONArray(rec.KeyFiles),
		db.JSONArray(rec.Evidence), db.JSONArray(rec.FixEvidence),
		rec.Reported, rec.Summary, rec.Body,
		nil, nil, nil, rec.Bucket, ts, ts,
	)
	return err
}

func fileDefectTitle(given, action string) (string, error) {
	if strings.TrimSpace(given) == "" {
		title := truncateRunes(strings.TrimSpace(action), 80)
		return truncateUTF8Bytes(title, maxTitleLen), nil
	}
	return trimLimit("title", given, maxTitleLen, true)
}

func truncateUTF8Bytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	used := 0
	for i, r := range s {
		n := utf8.RuneLen(r)
		if used+n > max {
			return s[:i]
		}
		used += n
	}
	return s
}

func fileDefectSeverity(given string) (string, error) {
	sev := strings.TrimSpace(given)
	switch sev {
	case "":
		return "P2", nil
	case "P1", "P2", "P3":
		return sev, nil
	default:
		return "", badRequest("severity must be P1, P2, or P3")
	}
}

func fileDefectReporter(given, executedBy string) (string, error) {
	if strings.TrimSpace(given) == "" {
		return executedBy, nil
	}
	return trimLimit("reporter", given, maxActorLen, true)
}

func fileDefectRepos(app *AppRecord, refs []RunRef, repos []string, hasRepos bool) ([]string, error) {
	known := map[string]bool{}
	for _, n := range appRepoNames(app) {
		known[n] = true
	}
	if !hasRepos {
		seen := map[string]bool{}
		var out []string
		for _, r := range refs {
			if r.Repo == "" || seen[r.Repo] {
				continue
			}
			seen[r.Repo] = true
			out = append(out, r.Repo)
		}
		if len(out) == 0 {
			out = appRepoNames(app)
		}
		if out == nil {
			out = []string{}
		}
		return out, nil
	}
	out := make([]string, 0, len(repos))
	seen := map[string]bool{}
	for _, raw := range repos {
		name := strings.TrimSpace(raw)
		if !known[name] {
			return nil, badRequest("unknown repo")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func appRepoNames(app *AppRecord) []string {
	if app == nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, n := range app.Repos {
		add(n)
	}
	for _, e := range app.RepoEntries {
		add(e.Name)
	}
	return out
}

func stepDefectSummary(status, action string) string {
	verb := "failed"
	if status == "blocked" {
		verb = "blocked"
	}
	return truncateRunes("Test step "+verb+": "+action, 200)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if n < 0 || len(r) <= n {
		if n < 0 {
			return ""
		}
		return s
	}
	return string(r[:n])
}

func defectBodyFromStep(run TestRun, step RunStep, note string, evidence []string) string {
	var b strings.Builder
	b.WriteString("## Reporter notes\n\n")
	writeQuoted(&b, note)
	if note != "" {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Filed from test run %s (%s), step %d %s.\n", run.ID, oneLine(run.Title), step.Position, step.Key)
	if run.PlanID != nil {
		ver := 0
		if run.PlanVersion != nil {
			ver = *run.PlanVersion
		}
		fmt.Fprintf(&b, "Plan %s version %d.\n", *run.PlanID, ver)
	}
	b.WriteString("\n## Screenshot observations\n\n")
	b.WriteString("## Repro\n\n")
	n := 1
	for _, s := range run.Steps {
		if s.Position < step.Position {
			fmt.Fprintf(&b, "%d. %s\n", n, oneLine(s.Action))
			n++
		}
	}
	fmt.Fprintf(&b, "%d. %s\n", n, oneLine(step.Action))
	b.WriteString("\n## Expected\n\n")
	writeQuoted(&b, step.Expected)
	actual := step.Actual
	if strings.TrimSpace(actual) == "" {
		actual = "(not recorded)"
	}
	b.WriteString("\n\n## Actual\n\n")
	writeQuoted(&b, actual)
	b.WriteString("\n\n## Notes\n\n")
	for _, line := range envNoteLines(run.Env, run.Refs) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n## Acceptance\n\n")
	fmt.Fprintf(&b, "- [ ] %s\n", acceptanceItem(step))
	b.WriteString("\n## Evidence\n\n")
	for _, p := range evidence {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	return b.String()
}

func acceptanceItem(step RunStep) string {
	if strings.TrimSpace(step.Expected) == "" {
		return fmt.Sprintf("Step \"%s\" passes on retest", oneLine(step.Action))
	}
	return oneLine(step.Expected) + " (retest: test run step " + step.Key + ")"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// writeQuoted prefixes every line so user text cannot open a markdown heading.
func writeQuoted(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("> ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func envNoteLines(env map[string]any, refs []RunRef) []string {
	if env == nil {
		env = map[string]any{}
	}
	var lines []string
	for _, key := range []string{"target", "device", "api_base", "flavor"} {
		s, ok := envString(env, key)
		if ok && s != "" {
			lines = append(lines, "- "+key+": "+flattenNewlines(s))
		}
	}
	if acc, ok := env["account"].(map[string]any); ok {
		label, _ := envString(acc, "label")
		user, _ := envString(acc, "username")
		if label != "" || user != "" {
			lines = append(lines, "- account: "+flattenNewlines(label)+" ("+flattenNewlines(user)+")")
		}
	}
	for _, r := range refs {
		line := r.Repo + "@" + r.Ref
		if r.SHA != "" {
			line += " (" + r.SHA + ")"
		}
		lines = append(lines, "- "+line)
	}
	return lines
}

func envString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func flattenNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func (s *Store) readRunEvidence(runID string, rels []string) ([]struct {
	Filename string
	Data     []byte
}, error) {
	out := make([]struct {
		Filename string
		Data     []byte
	}, 0, len(rels))
	if len(rels) == 0 {
		return out, nil
	}
	if !IsSafeRunID(runID) {
		return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
	}
	runDir := filepath.Join(paths.EvidenceDir(s.DefectsRoot), runID)
	info, err := os.Lstat(runDir)
	if err != nil || !info.IsDir() {
		return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
	}
	dirSys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
	}
	defer root.Close()
	for _, rel := range rels {
		name, err := evidenceLeaf(runID, rel)
		if err != nil {
			return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
		}
		data, err := readEvidenceLeaf(root, name, dirSys)
		if err != nil {
			return nil, &OpError{Status: 500, Msg: "could not read step evidence"}
		}
		out = append(out, struct {
			Filename string
			Data     []byte
		}{Filename: name, Data: data})
	}
	return out, nil
}

func evidenceLeaf(runID, rel string) (string, error) {
	if rel == "" || strings.Contains(rel, "..") || strings.ContainsAny(rel, "\x00\n\r") {
		return "", fmt.Errorf("invalid evidence path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("invalid evidence path")
	}
	norm := strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	prefix := "evidence/" + runID + "/"
	if !strings.HasPrefix(norm, prefix) {
		return "", fmt.Errorf("invalid evidence path")
	}
	name := strings.TrimPrefix(norm, prefix)
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid evidence path")
	}
	return name, nil
}

func readEvidenceLeaf(root *os.Root, name string, dirSys *syscall.Stat_t) ([]byte, error) {
	li, err := root.Lstat(name)
	if err != nil || !li.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	before, ok := li.Sys().(*syscall.Stat_t)
	if !ok || before.Nlink != 1 || before.Dev != dirSys.Dev {
		return nil, fmt.Errorf("refusing linked file")
	}
	if li.Size() > fileDefectMaxBytes {
		return nil, fmt.Errorf("evidence too large")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink != 1 || sys.Dev != dirSys.Dev || sys.Ino != before.Ino {
		return nil, fmt.Errorf("refusing linked file")
	}
	data, err := io.ReadAll(io.LimitReader(f, fileDefectMaxBytes+1))
	if err != nil || int64(len(data)) > fileDefectMaxBytes {
		return nil, fmt.Errorf("evidence too large")
	}
	return data, nil
}
