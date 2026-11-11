#!/usr/bin/env bash
# The branch already exists under another case: the checkout must land on its
# stored spelling, keep its upstream, and never create a lowercase duplicate.
tip=$(git rev-parse refs/heads/Feature/Attach)
main_tip=$(git rev-parse refs/heads/main)
wtm add feature/attach --no-index
[[ $(git rev-parse refs/heads/Feature/Attach) == "$tip" ]] || { echo 'existing branch tip moved' >&2; exit 1; }
[[ $(git -C ../repo-feature-attach rev-parse HEAD) == "$tip" ]] || { echo 'checkout is not on the branch tip' >&2; exit 1; }
[[ $(git -C ../repo-feature-attach rev-parse --abbrev-ref HEAD) == Feature/Attach ]] || { echo 'checkout is not on the stored branch' >&2; exit 1; }
[[ $(git config --get branch.Feature/Attach.remote) == origin ]] || { echo 'upstream remote lost' >&2; exit 1; }
[[ $(git config --get branch.Feature/Attach.merge) == refs/heads/Feature/Attach ]] || { echo 'upstream merge lost' >&2; exit 1; }

# The same lower-case request must remove the attached checkout and branch.
wtm delete feature/attach --yes >/dev/null
[[ ! -e ../repo-feature-attach ]] || { echo 'delete left the attached worktree' >&2; exit 1; }
! git rev-parse --verify --quiet refs/heads/Feature/Attach >/dev/null || { echo 'delete left the attached branch' >&2; exit 1; }
[[ $(git rev-parse refs/heads/main) == "$main_tip" ]] || { echo 'delete moved main' >&2; exit 1; }
