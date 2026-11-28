//go:build !windows

package fs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lock_unix.go — the exclusive OS lock behind ~/.wtm stores on unix.

// FileLock is an acquired exclusive advisory lock on an open file. Release
// frees the lock; the OS also releases it when the process exits. The lock
// stays inside package fs so no importer can release a lock it does not hold.
type FileLock struct{ f *os.File }

// AcquireFileLock takes the exclusive lock on path (creating it when missing),
// blocking until it is available. flock is per open file description, so
// releasing/closing one fd never affects another process's lock.
func AcquireFileLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &FileLock{f: f}, nil
}

// TryAcquireFileLock attempts the non-blocking exclusive lock on path. It
// returns ErrLockBusy when another process holds the lock.
func TryAcquireFileLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLockBusy, path)
		}
		return nil, err
	}
	return &FileLock{f: f}, nil
}

// Release unlocks the file and closes it. A nil receiver is a no-op.
func (l *FileLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func tryLockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
