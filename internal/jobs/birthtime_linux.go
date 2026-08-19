//go:build linux

package jobs

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// fileBirthTime uses statx(2) STATX_BTIME via golang.org/x/sys/unix (already
// in go.mod through modernc.org/sqlite). The frozen syscall package only
// exposes SYS_STATX on loong64, so a raw syscall.SYS_STATX is not portable
// across linux/amd64 and linux/arm64. Fail open when the syscall or the
// BTIME bit is unavailable (old kernel, tmpfs, etc.).
func fileBirthTime(f *os.File, _ *syscall.Stat_t) (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	var stx unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err != nil {
		return time.Time{}, false
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}, false
	}
	if stx.Btime.Sec == 0 && stx.Btime.Nsec == 0 {
		return time.Time{}, false
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)), true
}
