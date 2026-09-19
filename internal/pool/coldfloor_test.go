package pool

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A floor that declines is trivial to write and worthless on its own: the
// question these tests exist to answer is whether it declines ONLY where
// declining costs a queue instead of a job. So the controls come first and
// there are four of them, one per case the floor must let through, and every
// one of them stays green in the ablation that turns the limit tests red.
//
// The failure being guarded against is not hypothetical. A counting or
// measuring limit with no explicit list of what it does not apply to becomes
// a hang wearing a safety limit's clothes — the same shape --max's use cap
// had to be given three exemptions to avoid.

// stubFreeMemory replaces the machine measurement for one test.
func stubFreeMemory(t *testing.T, fraction float64, ok bool) {
	t.Helper()
	prev := FreeMemoryFraction
	FreeMemoryFraction = func() (float64, bool) { return fraction, ok }
	t.Cleanup(func() { FreeMemoryFraction = prev })
}

// countSlotDirs is the only thing that proves a boot did or did not happen at
// this layer: AcquireSlots is pure filesystem bookkeeping, and a new slot
// DIRECTORY is exactly the commitment that EnsureProvisioned later turns into
// a cold-booted simulator.
func countSlotDirs(t *testing.T, groupDir string) int {
	t.Helper()
	return len(ListSlotNumbers(groupDir))
}

// TestColdFloor_PlentyOfMemoryStillGrowsTheGroup is the first control: with
// memory comfortable, nothing about acquisition may change. A floor that also
// fires at 90% free would be indistinguishable, in every other test here,
// from one that fires correctly.
func TestColdFloor_PlentyOfMemoryStillGrowsTheGroup(t *testing.T) {
	stubFreeMemory(t, 0.90, true)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, groupDir, 0)
	buf := captureWaitNotice(t)

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 2*time.Second)
	if err != nil {
		t.Fatalf("90%% free must still add a slot: %v", err)
	}
	defer slots[0].Release()

	if got := countSlotDirs(t, groupDir); got != 2 {
		t.Errorf("group should have grown to 2 slots, has %d", got)
	}
	if got := buf.String(); got != "" {
		t.Errorf("an acquisition that never waited must print nothing, got:\n%s", got)
	}
}

// TestColdFloor_EmptyGroupBootsHoweverTightMemoryIs is the control the whole
// design rests on. The floor's justification is that declining converts a
// boot into a wait — but on a group with nothing resident there is nothing to
// wait FOR, so the same refusal would fail the job after --wait instead. A
// cold pool must boot at 1% free.
func TestColdFloor_EmptyGroupBootsHoweverTightMemoryIs(t *testing.T) {
	stubFreeMemory(t, 0.01, true)
	root := t.TempDir()

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 2*time.Second)
	if err != nil {
		t.Fatalf("an empty group has nothing to wait for and must boot: %v", err)
	}
	defer slots[0].Release()
}

// TestColdFloor_MoreNeededThanCanEverFreeUpStillBoots is the same argument
// one step further in. One busy slot is a fallback for a request of one; it
// is not a fallback for a request of two, because waiting for it can only
// ever produce one. Declining there would queue for something that cannot
// arrive.
func TestColdFloor_MoreNeededThanCanEverFreeUpStillBoots(t *testing.T) {
	stubFreeMemory(t, 0.01, true)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, groupDir, 0)

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 2, 4, 2*time.Second)
	if err != nil {
		t.Fatalf("one busy slot cannot satisfy a request for two: %v", err)
	}
	for _, s := range slots {
		defer s.Release()
	}
}

// TestColdFloor_UnreadableMemoryPermits is where this floor deliberately
// parts company with `reap --warm`. Warming is speculative, so an
// unverifiable machine state is reason enough to skip it. Acquisition is real
// demand: reading the same unknown as a refusal would put every acquisition
// on the machine into a queue because vm_stat failed once.
func TestColdFloor_UnreadableMemoryPermits(t *testing.T) {
	stubFreeMemory(t, 0, false)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, groupDir, 0)

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 2*time.Second)
	if err != nil {
		t.Fatalf("an unreadable measurement must not refuse real demand: %v", err)
	}
	defer slots[0].Release()

	if got := countSlotDirs(t, groupDir); got != 2 {
		t.Errorf("group should have grown to 2 slots, has %d", got)
	}
}

// TestColdFloor_LowMemoryWaitsInsteadOfAddingASimulator is the limit itself,
// and the assertion that matters is the slot count: the group must NOT have
// grown. That the call then times out is incidental — in production
// AcquireSlots' poll loop keeps re-entering this every 2s, so the same refusal
// is a queue that clears the moment the busy slot is released or memory frees.
func TestColdFloor_LowMemoryWaitsInsteadOfAddingASimulator(t *testing.T) {
	stubFreeMemory(t, 0.10, true)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, groupDir, 0)

	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 500*time.Millisecond)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("a floored acquisition must wait like a full group does, got %v", err)
	}
	if got := countSlotDirs(t, groupDir); got != 1 {
		t.Errorf("the group grew to %d slots: the floor did not stop the cold-boot branch", got)
	}
}

// TestColdFloor_SaysMemoryAndNotCapacity is the "nothing that stays quiet"
// half. A 10-minute wait whose stated reason is wrong is worse than one with
// no reason at all: every sentence of the capacity message points at --max,
// and raising --max here cannot help — the group is not at its cap, it chose
// not to grow.
func TestColdFloor_SaysMemoryAndNotCapacity(t *testing.T) {
	stubFreeMemory(t, 0.10, true)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, groupDir, 0)

	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "memory free") {
		t.Errorf("the refusal never mentions memory, so the reader cannot act on it:\n%s", msg)
	}
	if !strings.Contains(msg, EnvAcquireMinFree) {
		t.Errorf("the refusal never names the override that changes it:\n%s", msg)
	}
	if strings.Contains(msg, "raise --max") {
		t.Errorf("the refusal advises raising --max, which cannot help at 1 of 3 slots:\n%s", msg)
	}

	// And the periodic queue notice — the line somebody actually reads while
	// waiting — must lead with the memory reason rather than "slot-0 busy".
	var ce *capacityError
	if !errors.As(err, &ce) {
		t.Fatalf("expected a capacityError, got %T", err)
	}
	if !strings.Contains(ce.Summary(), "memory free") {
		t.Errorf("the wait notice would say %q, which names the wrong obstacle", ce.Summary())
	}
}

// TestColdFloor_UnwaitableSlotsAreNoFallback covers the exemption that is
// pure reasoning and shows up nowhere in a happy path. A quarantined or
// unverifiable slot does not free up by waiting, so counting it as a fallback
// would let the floor decline a request that then has nothing to wait for —
// a refusal every 2s until --wait runs out, ten minutes by default.
//
// The slot here is made unverifiable the cheapest honest way: a lease.json
// that cannot be parsed is never read as "no lease" (see ReadLease).
func TestColdFloor_UnwaitableSlotsAreNoFallback(t *testing.T) {
	stubFreeMemory(t, 0.01, true)
	root := t.TempDir()
	groupDir := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	if err := os.WriteFile(leasePath(SlotDir(groupDir, 0)), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 3, 2*time.Second)
	if err != nil {
		t.Fatalf("an unverifiable slot is not something to wait for: %v", err)
	}
	defer slots[0].Release()

	if got := countSlotDirs(t, groupDir); got != 2 {
		t.Errorf("group should have grown to 2 slots, has %d", got)
	}
}

// TestAcquireFloorThreshold_OverrideAndItsRefusals mirrors the --warm floor's
// own override test, for the same reason: "0.35" is a valid float in range
// and means 0.35%, which reads like a careful 35% and all but removes the
// floor. Falling back to the default is the difference between a typo and a
// silently disabled guard.
func TestAcquireFloorThreshold_OverrideAndItsRefusals(t *testing.T) {
	t.Setenv(EnvAcquireMinFree, "")
	if got := AcquireFloorThreshold(); got != acquireMinFreeMemory {
		t.Errorf("unset must use the default, got %v", got)
	}
	t.Setenv(EnvAcquireMinFree, "60")
	if got := AcquireFloorThreshold(); got != 0.60 {
		t.Errorf("60 must mean 60%%, got %v", got)
	}
	t.Setenv(EnvAcquireMinFree, "0")
	if got := AcquireFloorThreshold(); got != 0 {
		t.Errorf("an explicit 0 disables the floor and is a real choice, got %v", got)
	}
	for _, bad := range []string{"abc", "-1", "101", "0.35", "35%"} {
		t.Setenv(EnvAcquireMinFree, bad)
		if got := AcquireFloorThreshold(); got != acquireMinFreeMemory {
			t.Errorf("%q must fall back to the default, not disable the floor; got %v", bad, got)
		}
	}
}

// TestFreeMemoryFraction_ReadsTheRealMachine is a smoke test on the live
// probe, not a pinned value: it must come back plausible on whatever machine
// runs it. A probe that silently returned 0 would make both floors decline
// forever, which looks exactly like the floors not being implemented.
func TestFreeMemoryFraction_ReadsTheRealMachine(t *testing.T) {
	got, ok := liveFreeMemoryFraction()
	if !ok {
		t.Skip("could not read memory on this machine; nothing to assert")
	}
	if got <= 0 || got > 1 {
		t.Fatalf("free memory fraction %v is not a fraction — the floors would misjudge every pass", got)
	}
}
