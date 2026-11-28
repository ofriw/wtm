//go:build windows

package fs

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lock_windows.go — the exclusive OS lock behind ~/.wtm stores on Windows.

// FileLock is an acquired exclusive lock on an open file. Release frees the
// lock; the OS also releases it when the process exits. The lock stays inside
// package fs so no importer can release a lock it does not hold.
type FileLock struct{ f *os.File }

// AcquireFileLock takes the exclusive lock on path (creating it when missing),
// blocking until it is available.
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
		if err == windows.ERROR_LOCK_VIOLATION {
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

// lockFile takes an exclusive lock on one byte of the file. Without
// LOCKFILE_FAIL_IMMEDIATELY the call blocks until the lock is available, which
// matches flock's blocking behavior on unix.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

func tryLockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
