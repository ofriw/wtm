package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
)

// Pipes hold a real registry transaction open while another process requests one.
// The timeout is only a deadlock guard; no scheduling delay drives the tests.
type tempTestProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
}

func startTempProcess(t *testing.T, role, path string) *tempTestProcess {
	t.Helper()
	cmd := tempProcessCommand(t, role, path)
	input, err := cmd.StdinPipe()
	mustProcessError(t, err)
	output, err := cmd.StdoutPipe()
	mustProcessError(t, err)
	mustProcessError(t, cmd.Start())
	t.Cleanup(func() {
		_ = input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &tempTestProcess{cmd: cmd, input: input, output: bufio.NewScanner(output)}
}

func tempProcessCommand(t *testing.T, role, path string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTempRegistryProcessHelper$")
	cmd.Env = append(os.Environ(), "WTM_TEMP_PROCESS="+role, "WTM_TEMP_PROCESS_PATH="+path)
	cmd.Stderr = os.Stderr
	return cmd
}

func mustProcessError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (p *tempTestProcess) expect(t *testing.T, want string) {
	t.Helper()
	if !p.output.Scan() || p.output.Text() != want {
		t.Fatalf("process signal = %q, want %q (read error: %v)", p.output.Text(), want, p.output.Err())
	}
}

func (p *tempTestProcess) finish(t *testing.T) {
	t.Helper()
	mustProcessError(t, p.input.Close())
	mustProcessError(t, p.cmd.Wait())
}

func (p *tempTestProcess) skipLines(t *testing.T, until string) {
	t.Helper()
	for {
		if !p.output.Scan() {
			t.Fatalf("process ended before %q (read error: %v)", until, p.output.Err())
		}
		if p.output.Text() == until {
			return
		}
	}
}

func releaseTempProcess(t *testing.T, holder *tempTestProcess) {
	t.Helper()
	_, err := fmt.Fprintln(holder.input, "release")
	mustProcessError(t, err)
	holder.expect(t, "done")
	holder.finish(t)
}

// TestMain builds the CLI in child processes too. Keep Go caches outside HOME
// so their read-only module files cannot leak into the registry Sandbox.
func sandboxTempProcesses(t *testing.T) {
	t.Helper()
	for _, variable := range []string{"GOMODCACHE", "GOCACHE"} {
		value, err := exec.Command("go", "env", variable).Output()
		mustProcessError(t, err)
		t.Setenv(variable, strings.TrimSpace(string(value)))
	}
	Sandbox(t)
}

func TestTempRegistryProcessesPreserveUpdates(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		t.Run(operation, func(t *testing.T) { testTempProcessUpdates(t, operation) })
	}
}

func testTempProcessUpdates(t *testing.T, operation string) {
	t.Helper()
	sandboxTempProcesses(t)
	base := t.TempDir()
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	mustProcessError(t, storeTempFixture(second, fixtureRecord("tmp/second")))
	holder := startTempProcess(t, "hold-add", first)
	contendTempProcesses(t, holder, operation, second)
	want := temp.Store{first: fixtureRecord("tmp/first")}
	if operation == "add" {
		want[second] = fixtureRecord("tmp/added")
	}
	assertTempStore(t, want)
}

func TestTempRegistryProcessReadsCompleteRecords(t *testing.T) {
	sandboxTempProcesses(t)
	path := filepath.Join(t.TempDir(), "retained")
	want := temp.Store{path: fixtureRecord("tmp/added")}
	mustProcessError(t, storeTempFixture(path, want[path]))
	data, err := json.Marshal(want)
	mustProcessError(t, err)
	for _, role := range []string{"read-snapshot", "read-many"} {
		reader := startTempProcess(t, role, path)
		reader.expect(t, "ready")
		if role == "read-snapshot" {
			reader.expect(t, string(data))
		}
		reader.expect(t, "done")
		reader.finish(t)
	}
}

func TestTempRegistryProcessLockContention(t *testing.T) {
	sandboxTempProcesses(t)
	base := t.TempDir()
	holder := startTempProcess(t, "hold-lock", base)
	holder.expect(t, "locked")
	assertTempProcessContention(t, base)
	releaseTempProcess(t, holder)
}

func TestTempRegistryKilledHolderReleasesLock(t *testing.T) {
	sandboxTempProcesses(t)
	path := filepath.Join(t.TempDir(), "retained")
	want := temp.Store{path: fixtureRecord("tmp/retained")}
	mustProcessError(t, storeTempFixture(path, want[path]))
	holder := startTempProcess(t, "hold-lock", path)
	holder.expect(t, "locked")
	lockPath, before := tempLockFileInfo(t)
	waiter := startTempProcess(t, "wait-lock", path)
	waiter.expect(t, "ready")
	assertTempProcessContention(t, path)
	killTempProcess(t, holder)
	waiter.expect(t, "locked")
	assertTempLockFile(t, lockPath, before)
	releaseTempProcess(t, waiter)
	assertPersistentTempLock(t, lockPath, before, want)
}

func killTempProcess(t *testing.T, process *tempTestProcess) {
	t.Helper()
	mustProcessError(t, process.cmd.Process.Kill())
	var exit *exec.ExitError
	if err := process.cmd.Wait(); !errors.As(err, &exit) {
		t.Fatalf("killed process wait = %v, want exit error", err)
	}
}

func tempLockFileInfo(t *testing.T) (string, os.FileInfo) {
	t.Helper()
	path, err := temp.LockPath()
	mustProcessError(t, err)
	info, err := os.Stat(path)
	mustProcessError(t, err)
	return path, info
}

func assertPersistentTempLock(t *testing.T, path string, before os.FileInfo, want temp.Store) {
	t.Helper()
	assertTempLockFile(t, path, before)
	assertTempStore(t, want)
}

func assertTempLockFile(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path)
	mustProcessError(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("registry lock file was replaced")
	}
}

func contendTempProcesses(t *testing.T, holder *tempTestProcess, operation, path string) {
	t.Helper()
	holder.expect(t, "locked")
	contender := startTempProcess(t, operation, path)
	contender.expect(t, "ready")
	assertTempProcessContention(t, path)
	releaseTempProcess(t, holder)
	contender.expect(t, "done")
	contender.finish(t)
}

func assertTempProcessContention(t *testing.T, path string) {
	t.Helper()
	probe := startTempProcess(t, "check-lock", path)
	probe.expect(t, "contended")
	probe.expect(t, "done")
	probe.finish(t)
}

func TestTempRegistryProcessHelper(t *testing.T) {
	role := os.Getenv("WTM_TEMP_PROCESS")
	if role == "" {
		return
	}
	path := os.Getenv("WTM_TEMP_PROCESS_PATH")
	mustProcessError(t, runTempRegistryProcess(role, path))
	fmt.Println("done")
}

func runTempRegistryProcess(role, path string) error {
	if role == "hold-settings-lock" {
		return holdSettingsLockProcess()
	}
	if role == "hold-add" || role == "hold-lock" {
		return holdTempTransaction(role, path)
	}
	if role == "check-lock" {
		return checkTempProcessLock()
	}
	if role == "idle-remove" {
		fmt.Println("ready")
		root, err := git.RepoRoot(path)
		if err != nil {
			return err
		}
		return cmdGC(&globals{Root: root, Yes: true}, []string{"--path", path, "--yes"})
	}
	if role == "reconcile" {
		fmt.Println("ready")
		return temp.ReconcileTempStore()
	}
	fmt.Println("ready")
	return tempProcessOperation(role, path)
}

func tempProcessOperation(role, path string) error {
	switch role {
	case "wait-lock":
		return holdTempTransaction("hold-lock", path)
	case "add-deadline":
		return cmdAdd(&globals{Root: path, JSON: true}, []string{"deadline", "--no-index", "--ttl", "1.5s"})
	case "add":
		// storeTempFixture acquires the exclusive lock itself (mutateTempStore).
		return storeTempFixture(path, fixtureRecord("tmp/added"))
	case "remove":
		// clearTempRecord acquires the exclusive lock itself (mutateTempStore).
		return clearTempRecord(path)
	case "read-snapshot", "read-many":
		return readTempProcessStore(role)
	default:
		return fmt.Errorf("unknown process role %q", role)
	}
}

func readTempProcessStore(role string) error {
	if role == "read-many" {
		return checkTempProcessSnapshots()
	}
	store, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(store)
}

// Every snapshot must contain whole fixture records, never mixed fields.
func checkTempProcessSnapshots() error {
	for range 100 {
		store, err := temp.ReadTempStoreUnlocked()
		if err != nil {
			return err
		}
		for path, rec := range store {
			if rec != fixtureRecord("tmp/added") {
				return fmt.Errorf("incomplete process snapshot record at %s: %+v", path, rec)
			}
		}
	}
	return nil
}

func checkTempProcessLock() (err error) {
	path, err := temp.LockPath()
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := requireTempLockContention(file); err != nil {
		return err
	}
	fmt.Println("contended")
	return nil
}

func requireTempLockContention(file *os.File) error {
	err := nonblockingTempFileLock(file)
	if err == nil {
		unlockErr := fs.UnlockFile(file)
		return fmt.Errorf("acquired registry lock while holder retained it (unlock error: %v)", unlockErr)
	}
	if !isTempLockContention(err) {
		return fmt.Errorf("nonblocking registry lock attempt: %w", err)
	}
	return nil
}

func waitTempProcessRelease() error {
	command, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return err
	}
	if command != "release\n" {
		return fmt.Errorf("unexpected release command %q", command)
	}
	return nil
}

// holdSettingsLockProcess holds settings.lock until released or killed.
// WHY: a killed holder must not leave a stale lock; the OS frees the lock
// while the file stays, so the next writer proceeds on the same inode.
func holdSettingsLockProcess() error {
	lock, err := settingsLockPath()
	if err != nil {
		return err
	}
	return fs.WithFileLock(lock, func() error {
		fmt.Println("locked")
		return waitTempProcessRelease()
	})
}

// hold-add mutates in memory, then commits only after the release signal, so a
// concurrent reader observes the pre-commit seed-only store.
func holdTempTransaction(role, path string) error {
	return temp.WithLockedTempStore(func(store temp.Store) error {
		if role == "hold-add" {
			store[path] = fixtureRecord("tmp/first")
		}
		fmt.Println("locked")
		return waitTempProcessRelease()
	})
}

// Process-level idle-check contention: the holder occupies the registry lock
// while the contender runs a full idle removal and must wait.
func TestTempRegistryProcessIdleCheckContention(t *testing.T) {
	sandboxTempProcesses(t)
	repo, w, _ := idleTempCheckout(t)
	holder := startTempProcess(t, "hold-lock", repo)
	holder.expect(t, "locked")
	contender := startTempProcess(t, "idle-remove", w.Path)
	contender.expect(t, "ready")
	assertTempProcessContention(t, repo)
	releaseTempProcess(t, holder)
	contender.skipLines(t, "done")
	contender.finish(t)
	assertRemovedTempCheckout(t, w.Path)
}

// Process-level reconcile contention: the holder occupies the registry lock
// while the contender runs temp.ReconcileTempStore and must wait.
func TestTempRegistryProcessReconcileContention(t *testing.T) {
	sandboxTempProcesses(t)
	base := t.TempDir()
	keep := filepath.Join(base, "keep")
	if err := storeTempFixture(keep, fixtureRecord("tmp/keep")); err != nil {
		t.Fatal(err)
	}
	holder := startTempProcess(t, "hold-lock", base)
	holder.expect(t, "locked")
	contender := startTempProcess(t, "reconcile", base)
	contender.expect(t, "ready")
	assertTempProcessContention(t, base)
	releaseTempProcess(t, holder)
	contender.expect(t, "done")
	contender.finish(t)
	if _, ok := mustReadTempStore(t)[keep]; ok {
		t.Fatal("reconcile retained a stale record while lock was held")
	}
}

// harness.FreshWorktreeActivity duration benchmark: future changes can track the
// critical-section cost of the idle-check path.
func TestFreshWorktreeActivityDuration(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	MustGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	path := filepath.Join(filepath.Dir(repo), "wt-bench")
	GitWorktreeAdd(t, repo, path, "tmp/bench", "HEAD")
	MkSession(t, path, "bench", TempNow)
	BackdateGitActivity(t, path, TempNow)
	start := time.Now()
	_, _, err := harness.FreshWorktreeActivity(path, TempNow)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("harness.FreshWorktreeActivity: %v", err)
	}
	t.Logf("harness.FreshWorktreeActivity duration: %s", elapsed)
}
