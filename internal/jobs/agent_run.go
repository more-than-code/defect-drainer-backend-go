package jobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AgentRun records what the driver loop did. Completion is measured in
// artifacts on disk, not in the child's exit code or its prose (K6), so this
// is where an operator sees why the loop stopped.
type AgentRun struct {
	Iterations           int      `json:"iterations"`
	CompletedByArtifacts bool     `json:"completedByArtifacts"`
	StoppedBy            string   `json:"stoppedBy,omitempty"`
	StopReasons          []string `json:"stopReasons,omitempty"`
	Turns                int      `json:"turns,omitempty"`
	TotalCostUSD         float64  `json:"totalCostUsd,omitempty"`
	SessionID            string   `json:"sessionId,omitempty"`
	Missing              []string `json:"missing,omitempty"`
}

// Reasons the loop stopped. Only StoppedByArtifacts means the worker delivered
// what BRIEF.md asked for; the rest are caps and failures.
const (
	StoppedByArtifacts  = "artifacts"
	StoppedByIterations = "iterations"
	StoppedByWall       = "wall"
	StoppedByExit       = "exit"
	StoppedByCancelled  = "cancelled"
)

// Loop caps. Wall covers only the driver loop — never baseline or verify,
// which are 20 min per command on their own clock (verify.go).
const (
	defaultAgentMaxIters   = 8
	defaultAgentMaxWallSec = 2700
)

// parsePositiveInt falls back to def for empty, non-numeric, and n <= 0 so a
// typo'd env var cannot silently disable a cap.
func parsePositiveInt(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// markerLine reports whether line is exactly "<marker>: <id>" after trimming.
// Full-line exact on purpose: a substring match would let "UNFIXED: DEF-1"
// satisfy DEF-10, and a trailing comment would let the worker decline a defect
// while appearing to explain something else.
func markerLine(line, marker, id string) bool {
	return strings.TrimSpace(line) == marker+": "+id
}

// notesMarkers scans NOTES.md once for the two loop markers. A missing or
// unreadable NOTES.md yields no markers and ok=false — the loop then cannot be
// complete, which is the fail-closed direction.
func notesMarkers(handoff string, defectIDs []string, cutoff time.Time) (unfixed, noShot map[string]bool, ok bool) {
	unfixed, noShot = map[string]bool{}, map[string]bool{}
	data, _, ok := readRegularFile(handoff, filepath.Join(handoff, "NOTES.md"), cutoff)
	if !ok {
		return unfixed, noShot, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, id := range defectIDs {
			if markerLine(line, "UNFIXED", id) {
				unfixed[id] = true
			}
			if markerLine(line, "NO-SCREENSHOT", id) {
				noShot[id] = true
			}
		}
	}
	return unfixed, noShot, true
}

// harvestableImageName is the name-level half of harvest's evidence predicate.
// Both harvestFixEvidence and artifactsComplete call it so the loop can never
// stop on a file harvest would then skip. readRegularFile is the other half and
// each caller applies it to the same path.
func harvestableImageName(name string, isDir bool) bool {
	if isDir || strings.HasPrefix(name, ".") {
		return false
	}
	return imageExts[strings.ToLower(filepath.Ext(name))]
}

// hasHarvestableImage mirrors harvest exactly: real directory, name predicate,
// then readRegularFile (regular file under the handoff, not a link, inside the
// cutoff and size limit).
func hasHarvestableImage(handoff, imgDir string, cutoff time.Time) bool {
	if !isRealDir(imgDir) {
		return false
	}
	ents, err := os.ReadDir(imgDir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		// DirEntry.IsDir follows the link, so a symlinked directory reads as a
		// file here; readRegularFile is the real gate.
		if !harvestableImageName(e.Name(), e.IsDir()) {
			continue
		}
		if _, _, ok := readRegularFile(handoff, filepath.Join(imgDir, e.Name()), cutoff); ok {
			return true
		}
	}
	return false
}

// artifactsComplete decides whether the driver loop may stop. It answers only
// "did the worker deliver the files BRIEF.md demands" — never "may this defect
// resolve". Harvest keeps its own gates: a loop that completes on
// NO-SCREENSHOT or UNFIXED still resolves nothing for those defects.
//
// missing lists what was absent, in defect order, for the job log and for
// agentRun.missing when a cap ends the loop instead.
func artifactsComplete(handoff string, defectIDs []string, sandbox string, cutoff time.Time) (bool, []string) {
	if len(defectIDs) == 0 {
		return false, []string{"job has no defect ids"}
	}
	unfixed, noShot, notesOK := notesMarkers(handoff, defectIDs, cutoff)
	var missing []string
	if !notesOK {
		missing = append(missing, "NOTES.md")
	}
	for _, id := range defectIDs {
		if unfixed[id] {
			continue // declined: no notes and no image required
		}
		noteFile := filepath.Join(handoff, "fix-notes", id+".md")
		if _, _, ok := readRegularFile(handoff, noteFile, cutoff); !ok {
			missing = append(missing, "fix-notes/"+id+".md")
		}
		if hasHarvestableImage(handoff, filepath.Join(handoff, "fix-evidence", id), cutoff) {
			continue
		}
		// A strict sandbox genuinely cannot screenshot, so an explicit marker
		// releases the loop. It does not release harvest.
		if sandbox == "strict" && noShot[id] {
			continue
		}
		missing = append(missing, "fix-evidence/"+id+"/<image>")
	}
	return len(missing) == 0, missing
}

// nextLoopStop decides whether the driver loop stops, and why. Empty means
// iterate again. First match wins.
//
// The exit-code rule is the subtle one. Turn exhaustion also exits non-zero
// (verified on Grok Build 1.0.40: exit 1, stopReason "cancelled", plus a
// max_turns_reached event), so breaking on any non-zero exit would treat every
// turn cap as a job failure and never resume — which is the whole point of the
// loop. sawMaxTurns is the positive signal that separates the two.
//
// complete is checked before the caps: if the worker delivered the artifacts on
// its last legal iteration, that is why the loop stopped, and the operator
// should not see it reported as having run out of budget.
func nextLoopStop(iter, maxIters int, elapsed, maxWall time.Duration, exitErr error, sawMaxTurns, complete bool) string {
	if exitErr != nil && !sawMaxTurns {
		return StoppedByExit
	}
	if complete {
		return StoppedByArtifacts
	}
	if elapsed >= maxWall {
		return StoppedByWall
	}
	if iter >= maxIters {
		return StoppedByIterations
	}
	return ""
}

// agentRunAcc accumulates streaming-json events. OnEnd fires on the exec
// package's stdout copy goroutine while the loop goroutine reads the totals, so
// every field is behind the mutex.
type agentRunAcc struct {
	mu          sync.Mutex
	stopReasons []string
	turns       int
	costUSD     float64
	sessionID   string
	maxTurns    bool
}

func (a *agentRunAcc) end(stopReason, sessionID string, turns int, cost float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stopReason != "" {
		a.stopReasons = append(a.stopReasons, stopReason)
	}
	if sessionID != "" {
		a.sessionID = sessionID
	}
	a.turns += turns
	a.costUSD += cost
}

// maxTurnsReached latches per iteration; the loop clears it before each start.
func (a *agentRunAcc) maxTurnsReached() {
	a.mu.Lock()
	a.maxTurns = true
	a.mu.Unlock()
}

func (a *agentRunAcc) takeMaxTurns() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.maxTurns
	a.maxTurns = false
	return v
}

func (a *agentRunAcc) snapshot(iter int, stoppedBy string, missing []string) *AgentRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := &AgentRun{
		Iterations:           iter,
		CompletedByArtifacts: stoppedBy == StoppedByArtifacts,
		StoppedBy:            stoppedBy,
		Turns:                a.turns,
		TotalCostUSD:         a.costUSD,
		SessionID:            a.sessionID,
	}
	if len(a.stopReasons) > 0 {
		run.StopReasons = append([]string(nil), a.stopReasons...)
	}
	if len(missing) > 0 {
		run.Missing = append([]string(nil), missing...)
	}
	return run
}

// loopIterationLine is the operator-facing summary of one iteration. It always
// says what the child did and what, if anything, is still missing, so a capped
// run reads as "ran out of budget with X absent" rather than just "completed".
func loopIterationLine(iter int, exitErr error, sawMaxTurns, complete bool, stoppedBy string, missing []string) string {
	parts := []string{"agent iteration " + strconv.Itoa(iter)}
	switch {
	case sawMaxTurns:
		parts = append(parts, "hit --max-turns")
	case exitErr != nil:
		parts = append(parts, "exited non-zero")
	default:
		parts = append(parts, "exited 0")
	}
	if complete {
		parts = append(parts, "artifacts complete")
	} else {
		miss := "artifacts incomplete"
		if len(missing) > 0 {
			miss += " (missing: " + strings.Join(missing, ", ") + ")"
		}
		parts = append(parts, miss)
	}
	switch stoppedBy {
	case "":
		parts = append(parts, "resuming")
	case StoppedByArtifacts:
		parts = append(parts, "stopping: required files exist")
	case StoppedByIterations:
		parts = append(parts, "stopping: iteration cap")
	case StoppedByWall:
		parts = append(parts, "stopping: wall clock")
	case StoppedByExit:
		parts = append(parts, "stopping: agent failed")
	}
	return strings.Join(parts, " — ")
}

func loopLevel(stoppedBy string) string {
	switch stoppedBy {
	case StoppedByExit:
		return "error"
	case StoppedByIterations, StoppedByWall:
		return "warn"
	default:
		return "info"
	}
}
