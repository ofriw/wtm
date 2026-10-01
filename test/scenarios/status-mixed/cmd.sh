#!/usr/bin/env bash
# Main's LAST USED is the seeded future session clamped to `now` by
# worktreeLastUsed, so it must never enter a golden. Normalize only line 2 (main
# sorts first); the fixed 2020 rows of the other worktrees stay pinned. The
# placeholder keeps the 10-char column width so the table stays aligned.
wtm status | sed -E '2s/[0-9]{4}-[0-9]{2}-[0-9]{2}/<DATE>    /'
