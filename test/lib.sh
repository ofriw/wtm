#!/usr/bin/env bash
# Shared helpers for scenario setup.sh / cmd.sh. Sourced, never executed.
# Seeds must be deterministic: fixed dates, fixed content, and no wall-clock
# value may land in a file whose bytes a golden pins.

SBX=/sbx
SEED_DATE='2020-01-01T00:00:00+00:00'
FUTURE_STAMP=203501010000  # 2035-01-01 UTC — future, so the session reads ACTIVE
OLD_STAMP=202001010000     # 2020-01-01 UTC — past the 90d cutoff, so UNUSED

git_init() { # git_init <dir> [branch]
  local dir=$1 branch=${2:-main}
  mkdir -p "$dir"
  git -C "$dir" init -q -b "$branch"
  git -C "$dir" config user.name wtm
  git -C "$dir" config user.email wtm@example.com
}

commit() { # commit <repo> <message> <path>...
  local repo=$1 msg=$2
  shift 2
  git -C "$repo" add -- "$@"
  git -C "$repo" commit -q -m "$msg"
}

worktree_add() { # worktree_add <repo> <path> <branch> [start-point]
  local repo=$1 path=$2 branch=$3 start=${4:-main}
  git -C "$repo" worktree add -b "$branch" "$path" "$start"
}

db_index() { # db_index <worktree-root> — refuses to run without the offline stub
  case "$(command -v chunkhound)" in
    /stub-bin/*) ;;
    *) echo "harness: chunkhound is not the offline stub" >&2; return 1 ;;
  esac
  chunkhound index "$1"
}

mk_session() { # mk_session <worktree> <session-dir> <touch-stamp>
  local wt=$1 dir=$2 stamp=$3 d
  d="$PI_CODING_AGENT_DIR/sessions/$dir"
  mkdir -p "$d"
  printf '{"id":"%s","cwd":"%s"}\n{"type":"message","text":"seed"}\n' "$dir" "$wt" >"$d/$dir.jsonl"
  touch -t "$stamp" "$d/$dir.jsonl"
}

touch_git_activity() { # touch_git_activity <checkout> <touch options>...
  local root=$1 private
  shift
  private=$(git -C "$root" rev-parse --absolute-git-dir)
  touch "$@" "$root/.git" "$private/index"
}

backdate() { # backdate <path>... — pin all selected activity to SEED epoch
  local path
  for path in "$@"; do
    if [[ $path == */.git ]]; then
      # Staging activity lives in the private index, not the checkout backlink.
      touch_git_activity "${path%/.git}" -t "$OLD_STAMP"
    fi
  done
  touch -t "$OLD_STAMP" "$@"
}

status_of() { # status_of <path> [output] — extract STATUS cell for path from wtm status output
  local p=$1 status_out=${2:-$out}
  # The STATUS cell is matched by value (exactly ACTIVE/UNUSED), not by column
  # position: the header itself cannot be whitespace-split because "LAST USED"
  # contains a space, and inserting a column must not shift this helper.
  awk -v p="$p" '$1==p { for (i = 1; i <= NF; i++) if ($i == "ACTIVE" || $i == "UNUSED") print $i }' <<<"$status_out"
}
