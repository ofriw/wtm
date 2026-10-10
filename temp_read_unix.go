//go:build !windows

package main

// Unix flock is per open-file-description, and `os.Rename` preserves open
// handles. A reader that opened the file before a writer replaced it sees the
// old complete content; a reader that opens after sees the new complete
// content. No lock is needed.
func readTempStore() (tempStore, error) {
	return readTempStoreUnlocked()
}
