//go:build windows

package cli

import (
	"fmt"
	"path/filepath"
	"testing"
)

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
