. "${HARNESS:-/harness}/lib.sh"
git_init "$SBX/repo" main
echo seed >"$SBX/repo/file"
commit "$SBX/repo" seed file
worktree_add "$SBX/repo" "$SBX/fresh" fresh
worktree_add "$SBX/repo" "$SBX/dirty" dirty
echo dirty >>"$SBX/dirty/file"
mk_session "$SBX/dirty" s-dirty "$OLD_STAMP"
