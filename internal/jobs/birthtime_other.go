//go:build !darwin && !linux

package jobs

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime is unavailable on this GOOS. Callers fail open (Nlink/Dev
// checks still apply). Do not invent a portable no-op that looks like a
// successful birthtime read.
func fileBirthTime(*os.File, *syscall.Stat_t) (time.Time, bool) {
	return time.Time{}, false
}
