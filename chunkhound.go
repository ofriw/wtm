package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// chunkhound.go — the only file that knows ChunkHound's workspace layout.

const (
	chunkhoundConfigFile = ".chunkhound.json"
	chunkhoundDir        = ".chunkhound"
	chunkhoundDBDirName  = "db"
	rootGuardName        = "chunks.db.root.json"
)

func chunkhoundDBDir(root string) string {
	return filepath.Join(root, chunkhoundDir, chunkhoundDBDirName)
}

func chunkhoundDBPath(root string) string {
	return filepath.Join(chunkhoundDBDir(root), "chunks.db")
}

// copyChunkHound replicates config and db from srcRoot to dstRoot. Live daemon
// state is never copied: it is root-specific and lies about a running daemon.
func copyChunkHound(srcRoot, dstRoot string) error {
	if src := filepath.Join(srcRoot, chunkhoundConfigFile); exists(src) {
		if err := copyFile(src, filepath.Join(dstRoot, chunkhoundConfigFile)); err != nil {
			return err
		}
	}
	if src := chunkhoundDBDir(srcRoot); exists(src) {
		if err := cloneDir(src, chunkhoundDBDir(dstRoot)); err != nil {
			return err
		}
	}
	return nil
}

// rewriteRootGuard points the db's recorded root at the new worktree. This is
// the one mandatory transform: chunkhound refuses to open a db whose path
// differs (DuckDBIndexedRootMismatchError). Unknown keys are preserved.
func rewriteRootGuard(dstRoot string) error {
	p := filepath.Join(chunkhoundDBDir(dstRoot), rootGuardName)
	if !exists(p) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := readJSON(p, &m); err != nil {
		return fmt.Errorf("root guard %s: %w", p, err)
	}
	raw, err := json.Marshal(filepath.ToSlash(canonical(dstRoot)))
	// WHY canonical, not Clean: symlinked parents (/tmp -> /private/tmp on
	// macOS) must match what DuckDB records, else IndexedRootMismatch.
	if err != nil {
		return err
	}
	m["indexed_root_path"] = raw
	return writeJSON(p, m)
}

func rewriteDBPath(srcRoot, dstRoot string, db map[string]json.RawMessage) (bool, error) {
	var path string
	if pr, ok := db["path"]; !ok || json.Unmarshal(pr, &path) != nil || !filepath.IsAbs(path) {
		return false, nil
	}
	rel, err := filepath.Rel(srcRoot, canonical(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	raw, err := json.Marshal(filepath.ToSlash(filepath.Join(dstRoot, rel)))
	if err != nil {
		return false, err
	}
	db["path"] = raw
	return true, nil
}

func applyPatchedDB(p string, m, db map[string]json.RawMessage) error {
	raw, err := json.Marshal(db)
	if err != nil {
		return err
	}
	m["database"] = raw
	return writeJSON(p, m)
}

// patchDatabasePath rewrites an absolute database.path located inside srcRoot
// so the copied config addresses the new root's db. Relative paths already
// resolve against the new root, and absolute paths outside srcRoot are
// explicit user intent, so both stay verbatim.
// Silent nil on non-object/non-absolute/unparseable database.path is deliberate:
// explicit user intent and relative paths resolve without rewrite.
func patchDatabasePath(srcRoot, dstRoot string) error {
	p := filepath.Join(dstRoot, chunkhoundConfigFile)
	if !exists(p) {
		return nil
	}
	var m map[string]json.RawMessage
	if err := readJSON(p, &m); err != nil {
		return err
	}
	var db map[string]json.RawMessage
	if raw, ok := m["database"]; !ok || json.Unmarshal(raw, &db) != nil {
		return nil
	}
	if changed, err := rewriteDBPath(srcRoot, dstRoot, db); err != nil || !changed {
		return err
	}
	return applyPatchedDB(p, m, db)
}

// runIndex refreshes the copied db. PATH decides which chunkhound runs: tests
// pin the version and inject --no-embeddings, production uses the real one.
// chunkhound's progress goes to stderr so wtm's stdout stays machine-clean.
func runIndex(root string) error {
	cmd := exec.Command("chunkhound", "index", root)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.Env = append(os.Environ(), "CHUNKHOUND_NO_PROMPTS=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chunkhound index %s: %w", root, err)
	}
	return nil
}
