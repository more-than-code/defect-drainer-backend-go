package db

import (
	"fmt"
	"os"
	"syscall"

	"github.com/joe/defect-drainer-go/internal/paths"
)

// Lock is an exclusive non-blocking flock on {DATA}/defect-drainer.lock.
type Lock struct {
	file *os.File
	path string
}

// AcquireLock takes LOCK_EX|LOCK_NB. Fails immediately if another process holds it.
func AcquireLock(dataRoot string) (*Lock, error) {
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, err
	}
	p := paths.LockPath(dataRoot)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data dir locked: %s", dataRoot)
	}
	return &Lock{file: f, path: p}, nil
}

// Close unlocks and closes the lock file.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
