//go:build !darwin && !linux && !windows

package main

import "time"

// fileBirthTime reports no creation mark on platforms without a reliable birth
// time, so AGE reads "-" instead of guessing from mtime.
func fileBirthTime(string) time.Time { return time.Time{} }
