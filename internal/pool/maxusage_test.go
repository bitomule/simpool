package pool

import (
	"errors"
	"os"
	"testing"
	"time"
)

// These two tests exist as a pair and are worth nothing apart. Together they
// pin down what --max actually bounds, which is residency and not use: the
// first proves a group AT its cap really does queue, so the second's "it did
// not queue" is a fact about --max and not about an instrument that cannot
// see waiting. Nine measurements on the day this was written turned out to be
// a broken instrument rather than a broken model, which is why the control
// is not optional here.
//
// If a future change makes --max cap concurrent USE as well, the second test
// is the one that has to be rewritten, and deliberately so — it is the
// written-down form of the gap.

// seedSlotDirs creates n slot directories in a group, leaving it resident at
// n the way a real group is after it was acquired under a larger --max (or
// before `reap --max` has run).
func seedSlotDirs(t *testing.T, root, device, osv string, n int) string {
	t.Helper()
	g := GroupDir(root, device, osv)
	for i := 0; i < n; i++ {
		if err := os.MkdirAll(SlotDir(g, i), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestAcquireSlots_AtMaxQueues is the control: with the group's size equal to
// --max, an acquisition whose only slot is busy waits out --wait and then
// reports ErrAtCapacity. This is the case everybody assumes --max always
// produces.
func TestAcquireSlots_AtMaxQueues(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 1)

	held, err := TryLock(lockPath(SlotDir(g, 0)))
	if err != nil {
		t.Fatalf("holding slot-0: %v", err)
	}
	defer held.Release()

	start := time.Now()
	_, err = AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 2*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("a group at --max with its only slot busy must report ErrAtCapacity, got %v", err)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("the acquisition returned in %s instead of polling for its full 2s --wait: this test cannot detect queueing, so the companion test below cannot be read as detecting its absence", elapsed)
	}
}

// TestAcquireSlots_OverMaxDoesNotQueue is the finding. A group holding more
// slot directories than --max allows hands out the extras with no wait at
// all: --max is consulted only where a NEW slot would be created, so it can
// refuse to grow a group and cannot refuse to use one.
//
// The practical consequence, and the reason this is written down rather than
// left implicit: --max cannot be used to bound how many jobs touch this
// machine at once. `reap --max` deleting the excess is what makes the cap
// true again, and until it runs the flag bounds nothing.
func TestAcquireSlots_OverMaxDoesNotQueue(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 3)

	held, err := TryLock(lockPath(SlotDir(g, 0)))
	if err != nil {
		t.Fatalf("holding slot-0: %v", err)
	}
	defer held.Release()

	start := time.Now()
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 2*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("today --max does not bound use, so this acquisition is expected to succeed on an already-resident slot; got %v after %s", err, elapsed)
	}
	defer func() {
		for _, s := range slots {
			s.Release()
		}
	}()

	if slots[0].Number == 0 {
		t.Fatalf("slot-0 is held by this test and must not have been handed out")
	}
	if elapsed > time.Second {
		t.Fatalf("expected no wait at all (the slot is already resident), waited %s", elapsed)
	}
}
