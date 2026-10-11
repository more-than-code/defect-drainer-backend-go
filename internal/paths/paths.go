// Package paths resolves inventory and runtime roots. Fail-closed: never mkdir a second SSOT.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joe/defect-drainer-go/internal/env"
)

// ModulePath is the go.mod module line this tree must match when walking cwd.
const ModulePath = "github.com/joe/defect-drainer-go"

const dbFileName = "defect-drainer.db"

// The inventory data dir and the directory that holds it inside the umbrella.
// Both moved out of the retired TS tree (`backend/.data`) on 2026-09-22.
const (
	dataDirName     = ".data"
	goModuleDirName = "backend-go"
)

// Roots are the inventory and runtime directories for serve.
type Roots struct {
	DefectsRoot string
	DataRoot    string
	ModuleRoot  string // empty when go.mod was not found
}

// Options override process env / heuristics. Tests pass Cwd so they cannot see the live umbrella.
type Options struct {
	DefectsRoot string
	DataRoot    string
	Cwd         string
}

// EvidenceDir is {DEFECTS_ROOT}/evidence.
func EvidenceDir(defectsRoot string) string { return filepath.Join(defectsRoot, "evidence") }

// JobsDir is {DATA}/jobs (normalize handoffs).
func JobsDir(dataRoot string) string { return filepath.Join(dataRoot, "jobs") }

// BatchJobsDir is {DATA}/batch-jobs.
func BatchJobsDir(dataRoot string) string { return filepath.Join(dataRoot, "batch-jobs") }

// DbPath is {DATA}/defect-drainer.db.
func DbPath(dataRoot string) string { return filepath.Join(dataRoot, dbFileName) }

// LockPath is {DATA}/defect-drainer.lock (TS + Go flock).
func LockPath(dataRoot string) string { return filepath.Join(dataRoot, "defect-drainer.lock") }

// FindModuleRoot walks up from start for a go.mod whose module is ModulePath.
func FindModuleRoot(start string) string {
	dir := start
	for {
		if modulePathOf(filepath.Join(dir, "go.mod")) == ModulePath {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Resolve DEFECTS_ROOT and DATA for serve. healthcheck must not call this.
//
// Order: explicit opts → process env → moduleRoot (cwd go.mod) → umbrella cwd
// heuristic. When DEFECTS_ROOT was not set, it defaults to the resolved data
// dir. If either root is still unset, return an error. Never create directories.
// A defaulted root plus a legacy <umbrella>/evidence directory is an error;
// the operator moves it by hand.
func Resolve(opts Options) (Roots, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Roots{}, err
		}
	}

	defectsGiven := firstNonEmpty(opts.DefectsRoot, os.Getenv("DEFECTS_ROOT"))
	data := firstNonEmpty(opts.DataRoot, env.EnvDrainer("DATA"))
	moduleRoot := FindModuleRoot(cwd)

	// Legacy evidence lives next to the module, or next to the umbrella
	// findUmbrella recognised when the module's own data dir was not used.
	var umbrella string
	if moduleRoot != "" {
		umbrella = filepath.Dir(moduleRoot)
		if data == "" {
			// The data dir lives inside this module since the TS tree was
			// retired (2026-09-22); it used to be the sibling backend/.data.
			own := filepath.Join(moduleRoot, dataDirName)
			if fileExists(filepath.Join(own, dbFileName)) {
				data = own
			}
		}
	}
	if data == "" {
		if u := findUmbrella(cwd); u != "" {
			umbrella = u
			data = filepath.Join(u, goModuleDirName, dataDirName)
		}
	}

	defects := defectsGiven
	if defects == "" {
		defects = data
	}
	if defects == "" || data == "" {
		return Roots{}, fmt.Errorf("DEFECTS_ROOT and DEFECT_DRAINER_DATA are required when this binary cannot see the umbrella (refusing to create a second inventory)")
	}
	if defectsGiven == "" {
		if err := refuseLegacyEvidence(umbrella, data); err != nil {
			return Roots{}, err
		}
	}
	return Roots{DefectsRoot: defects, DataRoot: data, ModuleRoot: moduleRoot}, nil
}

func findUmbrella(start string) string {
	dir := start
	for {
		if fileExists(filepath.Join(dir, goModuleDirName, dataDirName, dbFileName)) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// refuseLegacyEvidence reports a hand-move when DEFECTS_ROOT was defaulted and
// the old umbrella evidence directory is still present. It creates nothing.
func refuseLegacyEvidence(umbrella, data string) error {
	if umbrella == "" {
		return nil
	}
	legacy := filepath.Join(umbrella, "evidence")
	dest := filepath.Join(data, "evidence")
	if legacy == dest || !dirExists(legacy) {
		return nil
	}
	if dirExists(dest) {
		empty, err := dirEmpty(legacy)
		if err != nil {
			return err
		}
		if empty {
			quoted := shellQuote(legacy)
			return fmt.Errorf("legacy evidence directory %s is empty (an older build recreated it); remove it: rmdir %s", quoted, quoted)
		}
		return fmt.Errorf("legacy evidence directory %s and %s both exist; merge them by hand", legacy, dest)
	}
	return fmt.Errorf("legacy evidence directory %s must be moved into the data dir: mv %s %s", legacy, shellQuote(legacy), shellQuote(dest))
}

// shellQuote wraps p for a POSIX single-quoted argument. An embedded
// apostrophe ends the quote, adds a backslash-escaped apostrophe, and
// reopens the quote.
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

func dirEmpty(p string) (bool, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return false, err
	}
	return len(ents) == 0, nil
}

func modulePathOf(goMod string) string {
	data, err := os.ReadFile(goMod)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
