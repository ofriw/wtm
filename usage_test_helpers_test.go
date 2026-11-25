package main

// buildUsageForTest is the test convenience form: it runs the production report
// entry with a nil worktree list, so the suite exercises the same fold as
// status without building the worktree inventory.
func buildUsageForTest(wtPath string, idx sessionIndex, cat catalog, subs subscriptions) (worktreeUsage, error) {
	u, _, err := buildUsageForReportAt(wtPath, nil, idx, cat, subs, nowFunc())
	return u, err
}
