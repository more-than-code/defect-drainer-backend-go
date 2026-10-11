package jobs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/joe/defect-drainer-go/internal/paths"
	"github.com/joe/defect-drainer-go/internal/store"
)

const (
	harvestResolved      = "resolved"
	harvestRegression    = "regression"
	harvestNotResolved   = "not_resolved"
	harvestNotesOnly     = "notes_only"
	harvestNoEvidence    = "no_evidence"
	harvestError         = "error"
	harvestReviewBlocked = "review_blocked"

	harvestRunCap    = 20
	harvestReasonMax = 500
	harvestImageCap  = 50
	harvestAtMax     = 40
)

// HarvestDefect is one defect's result inside a single harvest.
// Images are exactly the paths SaveEvidence returned for this run.
type HarvestDefect struct {
	DefectID string   `json:"defectId"`
	Outcome  string   `json:"outcome"`
	Reason   string   `json:"reason,omitempty"`
	Images   []string `json:"images,omitempty"`
}

// HarvestRun is one execution of harvestFixEvidence. Oldest first on the job.
type HarvestRun struct {
	At      string          `json:"at"`
	Defects []HarvestDefect `json:"defects"`
}

// fixImageName is the inventory file harvest is allowed to show on a read.
// SaveEvidence writes fix-NN.ext; the read path refuses anything else.
var fixImageName = regexp.MustCompile(`^fix-[0-9]+\.(png|jpg|jpeg|webp|gif|heic)$`)

func knownHarvestOutcome(outcome string) bool {
	switch outcome {
	case harvestResolved, harvestRegression, harvestNotResolved, harvestNotesOnly, harvestNoEvidence, harvestError, harvestReviewBlocked:
		return true
	default:
		return false
	}
}

func appendHarvestRuns(runs []HarvestRun, run HarvestRun) []HarvestRun {
	runs = append(runs, run)
	if len(runs) <= harvestRunCap {
		return runs
	}
	return append([]HarvestRun(nil), runs[len(runs)-harvestRunCap:]...)
}

// recordHarvest appends one run onto the live job. The harvest call receives
// a clone, so the write has to land on b.jobs under the lock. A job that
// disappeared or was cancelled during harvest is left unchanged.
func (b *BatchRunner) recordHarvest(jobID string, defects []HarvestDefect) {
	if defects == nil {
		defects = []HarvestDefect{}
	}
	run := HarvestRun{
		At:      time.Now().UTC().Format(time.RFC3339),
		Defects: defects,
	}
	b.mu.Lock()
	live := b.jobs[jobID]
	if live != nil && !jobGoneOrCancelled(live) {
		live.Harvests = appendHarvestRuns(live.Harvests, run)
	}
	b.mu.Unlock()
	_ = b.persistLive(jobID)
}

func cloneHarvestRun(r HarvestRun) HarvestRun {
	if r.Defects == nil {
		return r
	}
	defs := make([]HarvestDefect, len(r.Defects))
	for i, d := range r.Defects {
		defs[i] = d
		if d.Images != nil {
			defs[i].Images = append([]string(nil), d.Images...)
		}
	}
	r.Defects = defs
	return r
}

// capHarvestRuns is the structural bound applied before any evidence-dir
// read, and again when a job.json is hydrated into the live job. allowed
// nil keeps every defect id; a non-nil map drops the rest. One entry per
// id, and a repeat keeps the last. Images are the first 50 of that entry.
func capHarvestRuns(runs []HarvestRun, allowed map[string]bool) []HarvestRun {
	if len(runs) == 0 {
		return runs
	}
	if len(runs) > harvestRunCap {
		runs = append([]HarvestRun(nil), runs[len(runs)-harvestRunCap:]...)
	}
	out := make([]HarvestRun, 0, len(runs))
	for _, run := range runs {
		out = append(out, HarvestRun{
			At:      truncateUTF8(run.At, harvestAtMax),
			Defects: capHarvestDefects(run.Defects, allowed),
		})
	}
	return out
}

func capHarvestDefects(defects []HarvestDefect, allowed map[string]bool) []HarvestDefect {
	last := make(map[string]HarvestDefect, len(defects))
	var order []string
	for _, d := range defects {
		if allowed != nil && !allowed[d.DefectID] {
			continue
		}
		if _, seen := last[d.DefectID]; !seen {
			order = append(order, d.DefectID)
		}
		n := len(d.Images)
		if n > harvestImageCap {
			n = harvestImageCap
		}
		if n > 0 {
			d.Images = append([]string(nil), d.Images[:n]...)
		} else {
			d.Images = nil
		}
		d.Reason = truncateUTF8(d.Reason, harvestReasonMax)
		last[d.DefectID] = d
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]HarvestDefect, 0, len(order))
	for _, id := range order {
		out = append(out, last[id])
	}
	return out
}

// fillHarvests sanitises c.Harvests for an API read. job.json is agent-writable
// and hydrate trusts it, so a forged run must not reach the console. Only c
// is updated. The run list is cut to 20 before any Lstat.
func (b *BatchRunner) fillHarvests(c *Job) {
	if c == nil || len(c.Harvests) == 0 {
		return
	}
	evidenceDir := ""
	if b.store != nil {
		evidenceDir = paths.EvidenceDir(b.store.DefectsRoot)
	}
	allowed := make(map[string]bool, len(c.DefectIDs))
	for _, id := range c.DefectIDs {
		if store.IsSafeID(id) {
			allowed[id] = true
		}
	}
	kept := make([]HarvestRun, 0, harvestRunCap)
	for _, run := range capHarvestRuns(c.Harvests, allowed) {
		defects := sanitiseHarvestDefects(evidenceDir, run.Defects)
		if len(defects) == 0 {
			continue
		}
		kept = append(kept, HarvestRun{At: run.At, Defects: defects})
	}
	if len(kept) == 0 {
		c.Harvests = nil
		return
	}
	c.Harvests = kept
}

func sanitiseHarvestDefects(evidenceDir string, defects []HarvestDefect) []HarvestDefect {
	out := make([]HarvestDefect, 0, len(defects))
	for _, d := range defects {
		next := d
		if !knownHarvestOutcome(next.Outcome) {
			next.Outcome = harvestError
			next.Reason = "unrecognised outcome"
		}
		next.Reason = truncateUTF8(next.Reason, harvestReasonMax)
		var images []string
		for _, image := range next.Images {
			if harvestImageKept(evidenceDir, next.DefectID, image) {
				images = append(images, image)
			}
		}
		next.Images = images
		out = append(out, next)
	}
	return out
}

// harvestImageKept accepts only evidence/<defectID>/<fix-NN.ext> when that
// path is a regular file. The string is matched exactly so a ".." segment
// cannot point the read at another defect's directory.
func harvestImageKept(evidenceDir, defectID, image string) bool {
	prefix := "evidence/" + defectID + "/"
	if !strings.HasPrefix(image, prefix) {
		return false
	}
	name := image[len(prefix):]
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return false
	}
	if !fixImageName.MatchString(name) {
		return false
	}
	dir := filepath.Join(evidenceDir, defectID)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, name))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return ok && sys.Nlink == 1
}

// truncateUTF8 cuts s to at most max bytes without splitting a rune.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
