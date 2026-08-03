package pool

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// TestHelperAcquire is the re-exec'd helper for TestAcquireSlotsTwoRealProcesses
// and TestAcquireSlots_RecoversPoisonedSlotExactlyOnce. It calls the real
// allocation path (AcquireSlots) — no simctl involved — and reports which
// slot number it landed on, then blocks holding the lock until killed.
// SIMPOOL_HELPER_MAX optionally overrides --max (default
// DefaultMaxSlotsPerGroup) so a test can force two helpers to actually
// contend for the same slot instead of each landing on its own free one.
func TestHelperAcquire(t *testing.T) {
	if os.Getenv("SIMPOOL_HELPER_ACQUIRE") != "1" {
		return
	}
	root := os.Getenv("SIMPOOL_HELPER_ROOT")
	max := DefaultMaxSlotsPerGroup
	if v := os.Getenv("SIMPOOL_HELPER_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			max = n
		}
	}
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, max, 0)
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

// TestAcquireSlots_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv is the
// regression test for the CRITICAL finding that the poisoned-slot check
// relied entirely on `pgrep -f <udid>` (MatchingPIDs/LiveConsumers), which
// is blind to a consumer that only ever receives the UDID by environment
// variable (MAV_TARGET_UDID, SIMPOOL_UDID_N — exactly simpool's own
// contract, §5) rather than anywhere in its own argv. `simpool with`
// records that consumer's process-group id in meta.ConsumerPGID; this must
// be enough on its own to refuse a slot whose consumer is still alive, with
// nothing UDID-shaped in its command line at all.
func TestAcquireSlots_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv(t *testing.T) {
	root := t.TempDir()

	groupDir := GroupDir(root, "TestDevice", "1.0")
	slotDir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Stands in for the child `simpool with` launches: its own process
	// group leader (Setpgid, mirroring with.go), with nothing in its argv
	// or the process table that a UDID-based pgrep could ever match.
	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	if err := WriteMeta(slotDir, Meta{
		UDID:         "simpool-test-udid-not-in-any-argv",
		ConsumerPGID: pgid,
	}); err != nil {
		t.Fatal(err)
	}

	// slot-0's lock starts free (never taken), simulating: `simpool` itself
	// was SIGKILLed (design doc §4's one accepted failure window) and the
	// kernel released its flock, but the child it launched — recorded by
	// pgid, not by anything pgrep-able — is still running.
	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("acquire with poisoned slot-0 (live ConsumerPGID, no UDID anywhere in argv) at max=1: want ErrAtCapacity, got %v", err)
	}
}

// TestAcquireSlots_FillsGapLeftByRemovedSlotDir is the regression test for
// the HIGH finding that capacity was tracked by the highest slot number ever
// created (`next = existing[len(existing)-1] + 1`) rather than by how many
// slot directories are actually resident. Once reap started deleting purged
// slot directories, a gap below the high-water mark became the normal
// post-purge state, and the old logic refused to ever reuse it: it demanded
// a brand-new, higher-numbered slot while comparing that number (not the
// resident count) against --max, permanently wedging the group even with
// spare capacity sitting empty.
func TestAcquireSlots_FillsGapLeftByRemovedSlotDir(t *testing.T) {
	root := t.TempDir()

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 2, 2, 0)
	if err != nil {
		t.Fatalf("initial acquire of 2 slots at max=2: %v", err)
	}
	for _, s := range slots {
		if err := s.Release(); err != nil {
			t.Fatal(err)
		}
	}

	groupDir := GroupDir(root, "TestDevice", "1.0")
	// Mirrors what reap does after purging slot-0: the directory (lock file
	// included) is gone, leaving a gap below slot-1.
	if err := RemoveSlotDir(groupDir, SlotDir(groupDir, 0)); err != nil {
		t.Fatal(err)
	}
	if got := ListSlotNumbers(groupDir); len(got) != 1 || got[0] != 1 {
		t.Fatalf("expected only slot-1 to remain resident after removing slot-0, got %v", got)
	}

	got, err := AcquireSlots(root, "TestDevice", "1.0", 2, 2, 0)
	if err != nil {
		t.Fatalf("acquiring 2 slots at max=2 with only 1 resident (slot-0's gap should be fillable): %v", err)
	}
	defer func() {
		for _, s := range got {
			s.Release()
		}
	}()
	var nums []int
	for _, s := range got {
		nums = append(nums, s.Number)
	}
	sortInts(nums)
	if len(nums) != 2 || nums[0] != 0 || nums[1] != 1 {
		t.Fatalf("expected slot numbers [0 1] (slot-0's gap filled), got %v", nums)
	}
}

func TestAcquirePrefersMostRecentlyUsedSlot(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	if err := os.MkdirAll(group, 0o755); err != nil {
		t.Fatal(err)
	}

	// slot-0 is the lower number, so a naive ascending walk would pick it.
	// slot-1 was used more recently, so a warm-first walk must pick that one:
	// its simulator is the one still booted with the caller's app installed.
	for n, used := range map[int]time.Time{
		0: time.Now().Add(-2 * time.Hour),
		1: time.Now().Add(-1 * time.Minute),
	} {
		dir := SlotDir(group, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		WriteMeta(dir, Meta{Device: "iPhone 17 Pro", OSVersion: "26.3", UDID: "", LastUsed: used})
	}

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 4, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer slots[0].Release()

	if slots[0].Number != 1 {
		t.Fatalf("expected the most recently used slot (1), got slot-%d", slots[0].Number)
	}
}

// TestAcquireSlots_RecoversPoisonedSlotExactlyOnce extends the
// TestHelperAcquire real-process pattern (see TestAcquireSlotsTwoRealProcesses
// above) with a poisoned slot: two independent OS processes race
// AcquireSlots against a group with exactly one slot, already poisoned by a
// verifiably-identified SIGKILL orphan (a real process, correct fingerprint,
// Mode "with"). Recovery must happen exactly once — the winner reclaims the
// slot and the loser fails with ErrAtCapacity — never both succeeding (two
// consumers on one simulator) and never both failing (a recoverable slot
// going unclaimed). max=1 via SIMPOOL_HELPER_MAX forces real contention over
// the same slot number instead of each helper landing on its own free one.
func TestAcquireSlots_RecoversPoisonedSlotExactlyOnce(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	slotDir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}

	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	go func() { _ = orphan.Wait() }() // reap on death — see poison_test.go's spawnRealOrphan

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("capturing orphan's start time: %v", err)
	}
	bootID, err := procs.MachineBootTime()
	if err != nil {
		t.Fatalf("capturing machine boot time: %v", err)
	}
	if err := WriteMeta(slotDir, Meta{
		UDID:              "simpool-test-udid-concurrency",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		ConsumerBootID:    bootID,
	}); err != nil {
		t.Fatal(err)
	}

	spawn := func() (*exec.Cmd, *bufio.Scanner) {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperAcquire")
		cmd.Env = append(os.Environ(),
			"SIMPOOL_HELPER_ACQUIRE=1",
			"SIMPOOL_HELPER_ROOT="+root,
			"SIMPOOL_HELPER_MAX=1",
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

	procA, scanA := spawn()
	procB, scanB := spawn()

	readLine := func(t *testing.T, sc *bufio.Scanner) string {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("helper produced no output: %v", sc.Err())
		}
		return sc.Text()
	}

	resA := readLine(t, scanA)
	resB := readLine(t, scanB)

	slotWinners, errWinners := 0, 0
	for _, res := range []string{resA, resB} {
		switch {
		case strings.HasPrefix(res, "SLOT "):
			slotWinners++
		case strings.HasPrefix(res, "ERROR "):
			errWinners++
		default:
			t.Fatalf("unexpected helper output %q", res)
		}
	}
	if slotWinners != 1 || errWinners != 1 {
		t.Fatalf("expected exactly one winner and one ErrAtCapacity loser, got A=%q B=%q", resA, resB)
	}

	// Clean up whichever process won and is now holding the lock; the loser
	// already exited on its own.
	_ = procA.Process.Kill()
	_ = procA.Wait()
	_ = procB.Process.Kill()
	_ = procB.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if !procs.PGIDAlive(pgid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the original orphan process group should have been killed by whichever process recovered the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
