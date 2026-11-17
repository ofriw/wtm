//go:build windows

package cli

import "encoding/json"
import "fmt"
import "path/filepath"
import "reflect"
import "testing"

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
