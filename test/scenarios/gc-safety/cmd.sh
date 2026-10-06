# A fresh worktree is protected from --all even without an agent session. Explicitly
# selecting the active, dirty worktree is consent: it is removed. The picker
# surfaces dirty state so the interactive choice is informed.
wtm gc --all --yes </dev/null
[[ -d ../fresh ]] || { echo 'fresh worktree was collected' >&2; exit 1; }
wtm gc --path ../dirty --yes </dev/null
if [[ -d ../dirty ]]; then
  echo 'explicitly selected dirty worktree survived' >&2
  exit 1
fi
echo 'safety checks passed'
