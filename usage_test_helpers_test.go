package main

// buildUsageForTest is the test convenience form: it runs the production report
// entry with the conservative LastUsed session set, so the suite exercises the
// same fold as status without building the worktree inventory.
func buildUsageForTest(wtPath string, idx sessionIndex, cat catalog, subs subscriptions) (worktreeUsage, error) {
	u, _, err := buildUsageSessionsReport(wtPath, sessionsUnder(wtPath, idx), cat, subs, nowFunc(), nil)
	return u, err
}

// dedupeKeyedEvents decides which occurrence of each billed turn prices. It
// returns a per-session billed mask: true marks the occurrence that contributes
// cost. Spans ignore the mask and fold every event, so dedupe never changes
// activity time. Keyless events always bill. The winner is the first occurrence
// of a key with the best score, independent of coverage: the best measurement
// copy prices the turn. Coverage is a separate per-key verdict the fold applies
// (see disputedKeys and meterDisputed). The caller keeps its event slices; only
// the returned masks describe the change.
func dedupeKeyedEvents(sessions [][]usageEvent, covered []coveredProof) [][]bool {
	return dedupeMasks(sessions, bestScores(sessions, covered))
}
