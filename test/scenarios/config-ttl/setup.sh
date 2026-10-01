. "${HARNESS:-/harness}/lib.sh"
git_init "$SBX/repo" main
echo seed >"$SBX/repo/file"
commit "$SBX/repo" seed file
