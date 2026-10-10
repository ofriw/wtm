//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTempRegistryWindowsReaderWaitsForMutation(t *testing.T) {
	sandboxTempProcesses(t)
	path := filepath.Join(t.TempDir(), "pending")
	// Seed the registry so the reader takes the lock instead of the absent-file
	// fast path, then require it to observe the holder's committed mutation.
	mustProcessError(t, storeTempFixture(path, fixtureRecord("tmp/seed")))
	holder := startTempProcess(t, "hold-add", path)
	holder.expect(t, "locked")
	reader := startTempProcess(t, "read-snapshot", path)
	reader.expect(t, "ready")
	releaseTempProcess(t, holder)
	if !reader.output.Scan() {
		t.Fatalf("reader ended before its snapshot (read error: %v)", reader.output.Err())
	}
	assertTempSnapshot(t, reader.output.Text(), tempStore{path: fixtureRecord("tmp/first")})
	reader.expect(t, "done")
	reader.finish(t)
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
