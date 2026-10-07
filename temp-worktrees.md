# Temporary worktrees

Use temporary worktrees for short tasks such as PR reviews and experiments.
Collection occurs only when you run `wtm gc`. There is no background collector.

## Usage

```
wtm add <branch> --temp [--ttl <duration>]
```

`--temp` creates a `tmp/<name>` branch. `--ttl` sets the idle window and implies
`--temp`. The default is **1h**. Harness seeding and indexing do not change.
Human add output shows a compact countdown; the JSON `ttl` field carries the
exact duration. Status shows a compact countdown. Positive fractional durations
such as `1.5s` are supported. JSON `expiresAt` uses RFC3339 with fractional
seconds when needed, without rounding the deadline.

## Collection

A temporary worktree becomes a `wtm gc` candidate when:

```
now > max(createdAt, LastUsed) + ttl
```

`LastUsed` includes Pi sessions, Git metadata, commits, and changed files,
including staged modifications and new files. The checkout's private Git index
records staging activity, including staged deletions. Read-only status and GC
inspection do not refresh the index or restart the idle window.
Activity moves the deadline forward. The window measures idle time, not age.
Permanent worktrees use the configured `unusedTTL` instead.

Normal GC, including `gc --all`, removes only idle candidates. Before removing
each temporary candidate, GC holds the registry lock and reads current Git,
file, and Pi activity again. It checks the deadline against that fresh activity.
If the checkout is now ACTIVE, GC reports the skip and keeps the checkout,
record, local and remote branches, and sessions. Activity after initial selection
therefore protects the checkout.

Session files that cannot be read or attributed to a worktree (unreadable,
corrupt, symlinked, header without cwd) are unverifiable, not fatal by
themselves. Only an unverifiable session NEWER than the checkout's known
activity blocks removal — only such a file could be that checkout's hidden
recent activity. An older unverifiable file cannot change the verdict, so GC
warns and proceeds. A sessions directory that is itself unreadable stays
fatal for every candidate. A symlink to a session directory has unknown activity
age and blocks idle removal: appending an existing session does not change the
directory timestamp. File symlinks use the target file's timestamp.

A missing changed file blocks idle GC because its activity cannot be checked.
Explicit removal remains available after you inspect the checkout.

`gc --path <path>` and `delete <branch>` are explicit removal requests. They
ignore the idle deadline, but still enforce identity checks and removal consent.

GC removes the checkout and cleans up remote branches and sessions unless kept.
After each checkout's cleanup succeeds, it force-deletes that checkout's temporary
LOCAL branch, including unmerged commits. Consent identifies those branches. `--keep-remote` does not keep
local temporary branches. Dirty checkouts still require consent for forced
removal. The main worktree is never a candidate.

If cleanup fails, that checkout's local branch remains for recovery. A failure for
one checkout does not prevent successful cleanup of other checkouts. A repo-wide
`git worktree prune` failure is reported, but it does not keep temporary branches. If deletion
of a shared remote branch fails, all affected checkouts retain their local branches.
Failures and recovery instructions are reported. Correct the cause and complete
failed cleanup. Then retry `wtm gc --path <path>` if the checkout remains, or
`wtm delete <branch> --yes` if it is gone. `delete` also keeps the local branch if
cleanup fails.

## Identity and state

`~/.wtm/temp.json` maps canonical checkout paths to records with `createdAt`,
`ttl`, `branch`, and `identity`. The same random identity is stored in the
checkout's private Git admin directory, outside the checkout. This does not
change `git status` or require ignore rules.

A record applies only when both identity and branch match. Reusing a path, even
with the same branch name, does not transfer temporary status to a new checkout.
GC holds the registry lock while it checks identity and the current idle
window, removes the checkout, and clears that identity's record. Promotion and replacement registration cannot occur
between these steps. A changed identity stops removal and reports an error.
Failed checkout removal preserves the record. If saving the registry fails after
removal, GC reports the removed checkout and the error, and keeps the recovery branch.

Mutations hold an OS lock on `~/.wtm/temp.lock` and replace the registry atomically.
The lock file is never removed or renamed. Readers see a complete registry.
Windows readers use the same lock and wait for active mutations. This prevents
an open registry reader from blocking file replacement.
Unix readers do not take this lock: file replacement preserves their open
handles. Every `wtm gc` takes this lock and reconciles all records before
selecting candidates, so a gc run creates the lock file even when no worktree
is temporary. No other command reconciles: `status` and `promote` read the
registry and never create `~/.wtm`. On Windows, `wtm status` and `wtm gc` wait for the registry
lock — but only once a registry exists: reading with no registry file takes no
lock and creates nothing.
A hung `wtm` process holding the lock blocks all other `wtm` commands until it
exits.
GC removes records only when checkout or identity metadata is proven missing, or
identity or branch is mismatched. It does not change those checkouts. Filesystem
and Git errors retain the records and produce warnings with the path and cause,
including records for other repositories.

Recording occurs after successful seeding and indexing. If recording fails, add
reports a warning and the checkout remains permanent. Missing or invalid identity
never permits temporary collection. Invalid registry data produces an error for
mutations; status reports a warning and uses permanent-worktree rules.

## Promotion

```
wtm promote <branch>
```

Promotion removes the registry record and identity metadata. It leaves checkout
files and the branch unchanged. The checkout then uses ordinary `unusedTTL`
rules. Output identifies a retained `tmp/` branch.

Promotion reads and removes the record under one lock. Success is reported only
after permanent state is saved. An already-permanent checkout succeeds with
`promoted: false` in JSON. Unreadable or invalid registry data is an error, not
proof that a checkout is permanent. A checkout proven missing (its path or
`.git` is gone) clears its stale record and identity metadata and succeeds
with `promoted: true`. Git failures on an existing checkout stay errors:
absence is evidence, git errors alone are not.

## Visibility

`status` includes a `TEMP` countdown column. JSON includes `temp` and `expiresAt`.
At the exact deadline second, TEMP shows `0s` and the worktree is not yet a
candidate; the next gc run collects it. GC reports deleted local branches,
all cleanup failures, and every candidate skipped for becoming ACTIVE again.
