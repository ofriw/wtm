//go:build windows

package main

import "os"

// Windows readers must close the registry before a writer replaces it.
// Without the exclusive mutation lock, a writer's os.Rename could fail with
// ERROR_SHARING_VIOLATION if a reader holds an open handle. The mutation lock
// serialises reads and writes so that writers never block on readers.
func readTempStore() (tempStore, error) {
	// No registry means no writer ever registered a temp worktree, so there is
	// nothing to serialise against — and taking the lock would create the lock
	// file, giving a read-only command a filesystem side effect. The only loss
	// is a record registered concurrently, which then reads as permanent: the
	// same safe direction as every other verification failure.
	p, err := tempPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return tempStore{}, nil
	}
	var store tempStore
	err = withLockedTempStore(func(current tempStore, _ string) error {
		store = current
		return nil
	})
	return store, err
}
