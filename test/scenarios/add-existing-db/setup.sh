#!/usr/bin/env bash
# Seed: main with a real db and a config that has NO database.path, so the copy
# path cannot lean on path patching. Only the mandatory root-guard rewrite may
# change the copied workspace.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo 'hello' >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md

printf '{"llm":{"provider":"openai","model":"gpt-4o-mini"}}\n' >"$SBX/repo/.chunkhound.json"
db_index "$SBX/repo"
# --no-embeddings never writes an embedding sidecar, so pin one by hand: the
# clone must carry every db file across byte-for-byte.
printf '{"provider":"voyageai","model":"voyage-3","dims":1024}\n' \
  >"$SBX/repo/.chunkhound/db/chunks.db.embedding.json"
