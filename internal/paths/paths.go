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
// heuristic. If either root is still unset, return an error. Never create directories.
func Resolve(opts Options) (Roots, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Roots{}, err
		}
	}

	defects := firstNonEmpty(opts.DefectsRoot, os.Getenv("DEFECTS_ROOT"))
	data := firstNonEmpty(opts.DataRoot, env.EnvDrainer("DATA"))
	moduleRoot := FindModuleRoot(cwd)

	if (defects == "" || data == "") && moduleRoot != "" {
		umbrella := filepath.Dir(moduleRoot)
		if defects == "" {
			defects = umbrella
		}
		if data == "" {
			sibling := filepath.Join(umbrella, "backend", ".data")
			if fileExists(filepath.Join(sibling, dbFileName)) {
				data = sibling
			}
		}
	}

	if defects == "" || data == "" {
		if u := findUmbrella(cwd); u != "" {
			if defects == "" {
				defects = u
			}
			if data == "" {
				data = filepath.Join(u, "backend", ".data")
			}
		}
	}

	if defects == "" || data == "" {
		return Roots{}, fmt.Errorf("DEFECTS_ROOT and DEFECT_DRAINER_DATA are required when this binary cannot see the umbrella (refusing to create a second inventory)")
	}
	return Roots{DefectsRoot: defects, DataRoot: data, ModuleRoot: moduleRoot}, nil
}

func findUmbrella(start string) string {
	dir := start
	for {
		if dirExists(filepath.Join(dir, "evidence")) && fileExists(filepath.Join(dir, "backend", ".data", dbFileName)) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
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
