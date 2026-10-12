//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestOpenSharedAllowsReplaceAndRemove pins the invariant behind every store
// reader: a held reader handle must not block the writer's atomic replace or
// delete, or Windows fails them with ERROR_ACCESS_DENIED. It also pins snapshot
// semantics: the handle keeps reading the replaced file, never a half-written mix.
func TestOpenSharedAllowsReplaceAndRemove(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "store.json")
	writeFile(t, p, `{"v":"old"}`, 0o600)

	f, err := openShared(p)
	if err != nil {
		t.Fatalf("openShared: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := writeFileAtomic(p, []byte(`{"v":"new"}`), 0o600); err != nil {
		t.Fatalf("atomic replace while held open: %v", err)
	}

	held, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read held handle: %v", err)
	}
	if string(held) != `{"v":"old"}` {
		t.Fatalf("held handle = %q, want the pre-replace content", held)
	}

	if err := os.Remove(p); err != nil {
		t.Fatalf("remove while held open: %v", err)
	}
}

// TestExtendedPath pins that long paths get the extended prefix CreateFile
// needs beyond MAX_PATH, including the UNC form.
func TestExtendedPath(t *testing.T) {
	longDrive := `C:\` + strings.Repeat("dir\\", 60) + "store.json"
	if got := extendedPath(longDrive); !strings.HasPrefix(got, `\\?\C:\`) {
		t.Fatalf("drive path = %q, want \\\\?\\C:\\ prefix", got)
	}

	longUNC := `\\server\share\` + strings.Repeat("dir\\", 60) + "store.json"
	if got := extendedPath(longUNC); !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("UNC path = %q, want \\\\?\\UNC\\ prefix", got)
	}

	prefixed := `\\?\C:\already-extended`
	if got := extendedPath(prefixed); got != prefixed {
		t.Fatalf("prefixed path = %q, want unchanged", got)
	}
}

// TestReadFileSharedLongPath pins the end-to-end long-path contract: a path
// beyond MAX_PATH written atomically can be read through the shared primitive.
func TestReadFileSharedLongPath(t *testing.T) {
	deep := t.TempDir()
	for len(filepath.Join(deep, "store.json")) < 300 {
		deep = filepath.Join(deep, "padding-directory-name-xxxxxxxxxx")
	}
	p := filepath.Join(deep, "store.json")

	if err := writeFileAtomic(p, []byte("long-path"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic long path: %v", err)
	}
	got, err := readFileShared(p)
	if err != nil {
		t.Fatalf("readFileShared long path: %v", err)
	}
	if string(got) != "long-path" {
		t.Fatalf("content = %q, want %q", got, "long-path")
	}
}

// TestTempRegistryWindowsReaderSnapshotsWithoutBlocking pins the shared-reader
// contract on Windows: a reader concurrent with an in-flight mutation returns a
// complete pre-commit snapshot instead of waiting for the registry lock. A
// reader-lock regression would make the snapshot process time out here.
func TestTempRegistryWindowsReaderSnapshotsWithoutBlocking(t *testing.T) {
	sandboxTempProcesses(t)
	path := filepath.Join(t.TempDir(), "pending")
	// Seed the registry so the reader sees committed state, then start a holder
	// that mutates only in memory until released.
	mustProcessError(t, storeTempFixture(path, fixtureRecord("tmp/seed")))
	holder := startTempProcess(t, "hold-add", path)
	holder.expect(t, "locked")
	reader := startTempProcess(t, "read-snapshot", path)
	reader.expect(t, "ready")
	if !reader.output.Scan() {
		t.Fatalf("reader blocked or failed before its snapshot (read error: %v)", reader.output.Err())
	}
	assertTempSnapshot(t, reader.output.Text(), tempStore{path: fixtureRecord("tmp/seed")})
	reader.expect(t, "done")
	reader.finish(t)
	releaseTempProcess(t, holder)
}

func assertTempSnapshot(t *testing.T, line string, want tempStore) {
	t.Helper()
	var got tempStore
	mustProcessError(t, json.Unmarshal([]byte(line), &got))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reader snapshot = %#v, want %#v", got, want)
	}
}

func TestTempRegistryWindowsConcurrentReadersAndWriters(t *testing.T) {
	sandboxTempProcesses(t)
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	mustProcessError(t, storeTempFixture(seed, fixtureRecord("tmp/added")))
	holder := startTempProcess(t, "hold-lock", base)
	holder.expect(t, "locked")
	processes, want := startTempReadersAndWriters(t, base)
	want[seed] = fixtureRecord("tmp/added")
	releaseTempProcess(t, holder)
	for _, process := range processes {
		process.expect(t, "done")
		process.finish(t)
	}
	assertTempStore(t, want)
}

func startTempReadersAndWriters(t *testing.T, base string) ([]*tempTestProcess, tempStore) {
	t.Helper()
	want := tempStore{}
	var processes []*tempTestProcess
	for i := range 8 {
		path := filepath.Join(base, fmt.Sprint(i))
		role := "read-many"
		if i%2 == 0 {
			role = "add"
			want[path] = fixtureRecord("tmp/added")
		}
		process := startTempProcess(t, role, path)
		process.expect(t, "ready")
		processes = append(processes, process)
	}
	return processes, want
}
