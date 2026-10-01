# wtm

`wtm` automates the repetitive git worktree operations of a bleeding-edge software factory built on [ChunkHound](https://chunkhound.ai) and Pi.dev + [pi-mcp-adapter](https://github.com/nicobailon/pi-mcp-adapter).

## Why it exists

Every new checkout otherwise needs its Pi harness and ChunkHound search index re-created by hand, and abandoned worktrees accumulate invisibly.

`wtm` removes that toil: a new worktree becomes productive in a single step, and reclaiming abandoned worktrees is a safe, non-destructive-by-default choice.
