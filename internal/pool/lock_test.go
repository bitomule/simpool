package pool

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestHelperProcess is not a real test. It is re-exec'd as a subprocess by
// TestFlockTwoRealProcesses (the standard Go trick for spawning real,
// separate OS processes from within `go test`, mirroring how os/exec tests
// itself). Guarded by an env var so `go test` running it normally is a
// harmless no-op.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SIMPOOL_HELPER_TRYLOCK") != "1" {
		return
	}
	path := os.Getenv("SIMPOOL_HELPER_LOCK_PATH")
	l, err := TryLock(path)
	if err != nil {
		os.Stdout.WriteString("BUSY\n")
		os.Exit(0)
	}
	os.Stdout.WriteString("LOCKED\n")
	os.Stdout.Sync()
	// Hold the lock until killed (SIGKILL from the parent test), so the
	// test can prove the flock releases on abrupt death with no cleanup
	// step of our own. time.Sleep (a scheduled timer) rather than
	// `select{}`: an empty select has zero pending events, so the Go
	// runtime's own deadlock detector kills the process immediately with
	// "fatal error: all goroutines are asleep - deadlock!" — which would
	// release the lock right away and defeat the test.
	_ = l
	time.Sleep(time.Hour)
}

// TestFlockTwoRealProcesses is the load-bearing test in this repo: it
// proves, with two (and then three) genuinely separate OS processes, that
// (1) flock() gives exclusive ownership of a slot to exactly one process
// at a time, and (2) killing the holder with SIGKILL — the worst case,
// which a PID-file or heartbeat scheme cannot survive — releases the lock
// immediately with no cooperation from the dead process. This is the
// entire justification for choosing flock over every alternative
// discussed in the design doc (§3).
func TestFlockTwoRealProcesses(t *testing.T) {
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "lock")

	spawn := func() (*exec.Cmd, *bufio.Scanner) {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = append(os.Environ(),
			"SIMPOOL_HELPER_TRYLOCK=1",
			"SIMPOOL_HELPER_LOCK_PATH="+lockPath,
		)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, bufio.NewScanner(stdout)
	}

	readLine := func(t *testing.T, sc *bufio.Scanner) string {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("helper process produced no output: %v", sc.Err())
		}
		return sc.Text()
	}

	// Process A takes the lock and holds it.
	procA, scanA := spawn()
	if got := readLine(t, scanA); got != "LOCKED" {
		t.Fatalf("process A: want LOCKED, got %q", got)
	}

	// Process B, racing for the same lock file, must lose.
	procB, scanB := spawn()
	if got := readLine(t, scanB); got != "BUSY" {
		t.Fatalf("process B: want BUSY (A still holds the lock), got %q", got)
	}
	if err := procB.Wait(); err != nil {
		t.Fatalf("process B should exit 0 after reporting BUSY: %v", err)
	}

	// Confirm from a third, independent vantage point that the slot still
	// reads as held while A is alive.
	if free, err := IsFree(lockPath); err != nil {
		t.Fatal(err)
	} else if free {
		t.Fatal("IsFree reports free while process A is alive and holding the lock")
	}

	// Kill A the hard way — SIGKILL, no chance to run a defer or a signal
	// handler. This is exactly the failure mode a lockfile+PID scheme
	// cannot recover from (§3), and exactly what flock is chosen to
	// survive.
	if err := procA.Process.Kill(); err != nil {
		t.Fatalf("killing process A: %v", err)
	}
	_ = procA.Wait()

	// The kernel must have released the lock the instant A's file
	// descriptor table was torn down — no reap, no cleanup step, no grace
	// period required. Poll briefly to absorb scheduler jitter, but this
	// should succeed on the first try.
	deadline := time.Now().Add(2 * time.Second)
	for {
		free, err := IsFree(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		if free {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lock still held 2s after its holder was SIGKILLed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And a fresh, third process must now be able to acquire it.
	procC, scanC := spawn()
	if got := readLine(t, scanC); got != "LOCKED" {
		t.Fatalf("process C: want LOCKED after A's death freed the slot, got %q", got)
	}
	if err := procC.Process.Kill(); err != nil {
		t.Fatalf("killing process C: %v", err)
	}
	_ = procC.Wait()
}
