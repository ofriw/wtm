//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openNoBlock opens path without waiting for a FIFO writer. A blocking open is
// how status would hang on a hostile path; O_NONBLOCK lets the caller inspect
// the handle and refuse it. Regular-file reads ignore the flag, and POSIX
// rename(2)/unlink preserve the open handle, so an atomic replace never blocks.
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
