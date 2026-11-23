package harness

import (
	"fmt"
	"time"

	"wtm/internal/core"
	"wtm/internal/temp"
)

// activity.go — the harness-side fresh-activity probe used by temp idle checks.

// FreshWorktreeActivity refreshes both sources under a fresh session index
// at now; discovery's cached index cannot authorize removal. Errors are fatal:
// removal must fail closed when activity is unreadable. Unverified sessions
// ride along so the caller can scope them per candidate instead of failing
// globally.
func FreshWorktreeActivity(path string, now time.Time) (time.Time, []temp.UnverifiedSession, error) {
	idx, unverified, err := IndexSessionsStrict(nil)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("refresh agent sessions: %w", err)
	}
	last, err := core.ReadWorktreeActivityStrict(path, idx, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	return last, unverified, nil
}
