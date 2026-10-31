# Temporary worktrees

Use temporary worktrees for short tasks such as PR reviews and experiments.
Collection occurs only when you run `wtm gc`. There is no background collector.

## Usage

```
wtm add <branch> --temp [--ttl <duration>]
```

`--temp` creates a `tmp/<name>` branch. `--ttl` sets the idle window and implies
`--temp`. The default is **1h**. Harness seeding and indexing do not change.
`--temp` requires a new branch: an existing branch is refused so GC never
inherits a branch wtm did not create. Attach an existing `tmp/…` branch without
`--temp`; it then behaves as a permanent worktree.
The checkout directory is named from the prefixed branch, so `add foo --temp`
lands in `tmp/foo` inside `<repo>-tmp-foo`, and the `tmp/` prefix counts toward
the 80-character branch limit. Human add output shows a compact countdown; the
JSON `ttl` field carries the exact duration. Status shows a compact countdown.
Positive fractional durations such as `1.5s` are supported. JSON `expiresAt`
uses RFC3339 with fractional seconds when needed, without rounding the deadline.

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
If the locked verification refuses the removal (the checkout is now ACTIVE,
was promoted, or its temp metadata changed), GC reports the skip and keeps the
checkout, record, local and remote branches, and sessions. Activity after
initial selection therefore protects the checkout.

Session files that cannot be read or attributed to a worktree (unreadable,
corrupt, symlinked, header without cwd) are unverifiable, not fatal by
themselves. Two rules decide what an unverifiable file does:

- **Unknown age** (zero mtime: a dangling symlink, a symlinked session
  directory, or any non-regular `*.jsonl`) blocks idle removal of every
  candidate. Its age cannot be bounded above, so it could be hidden recent use.
  A symlinked session directory never ages: appending an existing session does
  not change the directory timestamp.
- **Known age** blocks removal only when it is NEWER than the checkout's known
  activity — only such a file could hide recent use. An older unverifiable file
  cannot change the verdict, so GC warns and proceeds. File symlinks use the
  target file's timestamp.

A sessions directory that is itself unreadable stays fatal for every candidate.

A missing changed file blocks idle GC because its activity cannot be checked.
Explicit removal remains available after you inspect the checkout.

Permanent worktrees have no fresh idle probe at removal time, so unverified
sessions are their only guard. When every unverified session has a known age,
a session protects a permanent only if the session is not older than the
permanent's known activity — a session older than that activity cannot hide
newer use. A single session with unknown age protects every permanent, failing
closed. This guard runs at discovery only; the protection it grants lasts one
GC run.

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
Unix and Windows readers share the registry file with a handle that permits
concurrent delete (openShared). This prevents a reader from blocking an atomic
replace. No reader ever takes the registry lock.
Every `wtm gc` takes this lock and reconciles all records before selecting
candidates, so a gc run creates the lock file even when no worktree is
temporary. No other command reconciles: `status` and `promote` read the
registry. `promote` creates it, because the promotion decision runs under the
mutation lock even for an already-permanent target. `status` does not create
`~/.wtm`. Reading with no registry file takes no lock and creates nothing.
A hung `wtm` process holding the lock blocks all other `wtm` commands until it
exits.
`~/.wtm/settings.lock` follows the same discipline for the settings store:
`wtm config set` holds it across the read-merge-write and replaces
`~/.wtm/settings.json` atomically at 0600, so concurrent writers cannot lose an
update. `wtm config` (read) takes no lock and shares the file handle, so a read
never creates `~/.wtm` or the lock file. The lock file stays at 0 bytes
forever: writes create it once and never remove it, so its presence alone
carries no meaning. It is never removed because the next writer must reuse the
same inode; deleting it would let a new lock file replace the old one and
defeat crash safety.
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

`promote` and `delete` resolve their branch argument the same way: the exact
name first, then the normalized name, then the bare name of a `tmp/<name>`
branch. So `wtm promote foo` addresses `tmp/foo` when no `foo` exists. The main
worktree and the default branch are refused by both commands.

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
all cleanup failures, and every candidate the locked check refused (now ACTIVE
again, promoted, or changed temp metadata). A refused removal is a skip, not a
cleanup failure: it keeps the checkout and exits 0.
