package pool

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestHelperAcquire is the re-exec'd helper for TestAcquireSlotsTwoRealProcesses.
// It calls the real allocation path (AcquireSlots) — no simctl involved —
// and reports which slot number it landed on, then blocks holding the
// lock until killed.
func TestHelperAcquire(t *testing.T) {
	if os.Getenv("SIMPOOL_HELPER_ACQUIRE") != "1" {
		return
	}
	root := os.Getenv("SIMPOOL_HELPER_ROOT")
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, DefaultMaxSlotsPerGroup, 0)
	if err != nil {
		fmt.Fprintf(os.Stdout, "ERROR %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "SLOT %d\n", slots[0].Number)
	os.Stdout.Sync()
	// See lock_test.go's TestHelperProcess for why this is time.Sleep and
	// not `select{}`.
	time.Sleep(time.Hour)
}

// TestAcquireSlotsTwoRealProcesses proves the allocation layer built on top
// of flock behaves correctly under real contention: two independent OS
// processes racing AcquireSlots against the very same, brand-new group
// directory must land on two different slot numbers, both held
// simultaneously, and killing one must free exactly that one slot for a
// third process to pick up — never the other.
func TestAcquireSlotsTwoRealProcesses(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")

	spawn := func() (*exec.Cmd, *bufio.Scanner) {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperAcquire")
		cmd.Env = append(os.Environ(),
			"SIMPOOL_HELPER_ACQUIRE=1",
			"SIMPOOL_HELPER_ROOT="+root,
		)
		cmd.Stderr = os.Stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, bufio.NewScanner(stdout)
	}

	readSlot := func(t *testing.T, sc *bufio.Scanner) int {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("helper produced no output: %v", sc.Err())
		}
		line := sc.Text()
		var n int
		if _, err := fmt.Sscanf(line, "SLOT %d", &n); err != nil {
			t.Fatalf("unexpected helper output %q: %v", line, err)
		}
		return n
	}

	procA, scanA := spawn()
	slotA := readSlot(t, scanA)

	procB, scanB := spawn()
	slotB := readSlot(t, scanB)

	if slotA == slotB {
		t.Fatalf("both processes acquired the same slot number %d; exclusivity is broken", slotA)
	}

	// Both must genuinely still be locked while their holders are alive.
	for _, n := range []int{slotA, slotB} {
		free, err := IsSlotFree(SlotDir(groupDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if free {
			t.Fatalf("slot-%d reads free while its holder is alive", n)
		}
	}

	if err := procA.Process.Kill(); err != nil {
		t.Fatalf("killing process A: %v", err)
	}
	_ = procA.Wait()

	freeA, err := IsSlotFree(SlotDir(groupDir, slotA))
	if err != nil {
		t.Fatal(err)
	}
	if !freeA {
		t.Fatalf("slot-%d still held after its process was killed", slotA)
	}
	freeB, err := IsSlotFree(SlotDir(groupDir, slotB))
	if err != nil {
		t.Fatal(err)
	}
	if freeB {
		t.Fatalf("slot-%d (process B, still alive) reads free — killing A must not affect B's slot", slotB)
	}

	if err := procB.Process.Kill(); err != nil {
		t.Fatalf("killing process B: %v", err)
	}
	_ = procB.Wait()
}

// TestAcquireSlots_RefusesCapacityAboveMax is the regression test for the
// finding that AcquireSlots created an unbounded number of slots under
// contention: with max=1 and one slot already busy, a second acquirer must
// not create slot-1 — it must fail (wait=0 means "fail immediately") with
// ErrAtCapacity instead.
func TestAcquireSlots_RefusesCapacityAboveMax(t *testing.T) {
	root := t.TempDir()

	held, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if err != nil {
		t.Fatalf("first acquire (should succeed, slot-0 free): %v", err)
	}
	defer held[0].Release()

	_, err = AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("second acquire at max=1 with slot-0 held: want ErrAtCapacity, got %v", err)
	}

	groupDir := GroupDir(root, "TestDevice", "1.0")
	if got := ListSlotNumbers(groupDir); len(got) != 1 {
		t.Fatalf("AcquireSlots created %v slots beyond --max 1", got)
	}
}

// TestAcquireSlots_SkipsSlotWithLiveOrphanConsumer is the regression test
// for the finding that a slot whose previous `simpool` holder was SIGKILLed
// (releasing the flock immediately) but whose consumer survived was handed
// straight to the next acquirer — putting two consumers on one simulator.
// AcquireSlots must refuse a free-looking slot whose meta.UDID still has a
// live external process attached, and only create a new one once max
// allows it.
func TestAcquireSlots_SkipsSlotWithLiveOrphanConsumer(t *testing.T) {
	root := t.TempDir()
	token := "simpool-test-udid-" + strconv.Itoa(os.Getpid())

	groupDir := GroupDir(root, "TestDevice", "1.0")
	slotDir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(slotDir, Meta{UDID: token}); err != nil {
		t.Fatal(err)
	}
	// slot-0's lock starts free (never taken), simulating: a previous
	// `simpool with` was SIGKILLed and the kernel released its flock, but
	// its consumer (standing in for MAV's `log stream`) is still running.
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "orphan_consumer.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := exec.Command(scriptPath, token)
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(orphan.Process.Pid, syscall.SIGKILL) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", token).Output()
		if len(out) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orphan consumer never showed up as a live process")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// max=1: slot-0 exists but is poisoned by the live orphan, so this must
	// fail rather than hand it out.
	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("acquire with poisoned slot-0 at max=1: want ErrAtCapacity, got %v", err)
	}

	// max=2: must skip slot-0 and create slot-1 instead of reusing it.
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 2, 0)
	if err != nil {
		t.Fatalf("acquire with poisoned slot-0 at max=2: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("expected a fresh slot-1 (slot-0 has a live orphan attached), got slot-%d", slots[0].Number)
	}
}
