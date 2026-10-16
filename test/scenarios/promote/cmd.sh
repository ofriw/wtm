#!/usr/bin/env bash
# Promoting clears the record only: the checkout and its tmp/x branch survive,
# so the branch-addressed argv is enough and no confirmation is needed.
# Both the command assertion and normalized manifest pin the empty store.
wtm promote tmp/x
[[ $(tr -d '[:space:]' <"$HOME/.wtm/temp.json") == '{}' ]] || { echo 'temp store not emptied' >&2; exit 1; }
