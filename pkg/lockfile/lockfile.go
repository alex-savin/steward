// Package lockfile provides an exclusive, non-blocking file lock so only one
// steward instance manages a host at a time. It uses flock(2), which
// exists on both deployment targets (linux, darwin); the kernel releases the
// lock automatically if the process dies, so stale locks cannot wedge deploys.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Lock is a held file lock. Release it on shutdown.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes the lock or fails immediately if another process holds it.
// The holder's PID is written into the file for operator convenience.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another steward instance holds %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_ = f.Sync()
	return &Lock{f: f, path: path}, nil
}

// Release drops the lock. The lock file itself is deliberately left in place:
// unlinking it would open a race where one new instance locks the old inode
// while another locks a freshly created file at the same path — and both
// believe they hold the single-instance lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
