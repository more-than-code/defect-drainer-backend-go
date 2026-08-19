//go:build darwin

package jobs

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime uses Stat_t.Birthtimespec (verified in Go 1.26.4
// syscall/ztypes_darwin_{arm64,amd64}.go). Zero means the field was not
// populated — fail open.
func fileBirthTime(_ *os.File, sys *syscall.Stat_t) (time.Time, bool) {
	if sys == nil {
		return time.Time{}, false
	}
	ts := sys.Birthtimespec
	if ts.Sec == 0 && ts.Nsec == 0 {
		return time.Time{}, false
	}
	return time.Unix(ts.Sec, ts.Nsec), true
}
