package pool

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1)
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
