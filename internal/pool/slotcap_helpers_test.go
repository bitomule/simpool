package pool

import (
	"os"
	"testing"
)

// TestRemoveSlotDirAbove_RefusesOnceTheGroupIsAtCap is what makes the cap
// pass's "never shrink a group below its own cap" promise actually hold
// against a peer reaper rather than merely being likely.
//
// The caller counts the group before it evicts, but every peer's own
// RemoveSlotDir funnels through the group allocation lock, so a peer removal
// landing between that count and the caller's own removal would take the
// group one slot BELOW the cap — a healthy simulator deleted for nothing and
// a cold boot to put it back. Counting again inside the same allocation lock
// that does the removal is the only place the two are one decision.
func TestRemoveSlotDirAbove_RefusesOnceTheGroupIsAtCap(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	for n := 0; n < 4; n++ {
		if err := os.MkdirAll(SlotDir(groupDir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 4 slots, cap 3: one is genuinely over and must go.
	removed, err := RemoveSlotDirAbove(groupDir, SlotDir(groupDir, 0), 3)
	if err != nil {
		t.Fatalf("removing a slot from an oversized group: %v", err)
	}
	if !removed {
		t.Fatal("a group of 4 against a cap of 3 must give one slot back")
	}
	if _, err := os.Stat(SlotDir(groupDir, 0)); !os.IsNotExist(err) {
		t.Fatalf("slot-0's directory survived: %v", err)
	}

	// Now at the cap. A second removal — the one a peer's deletion would
	// have made redundant — must be refused, and must leave the directory.
	removed, err = RemoveSlotDirAbove(groupDir, SlotDir(groupDir, 1), 3)
	if err != nil {
		t.Fatalf("second removal: %v", err)
	}
	if removed {
		t.Fatal("a group already at its cap must not give another slot back")
	}
	if _, err := os.Stat(SlotDir(groupDir, 1)); err != nil {
		t.Fatalf("slot-1's directory was removed while the group was at cap: %v", err)
	}
}

// TestLockExistingSlot_NeverResurrectsARemovedSlot pins the one behaviour
// that separates this from claimSlotLock: it must not MkdirAll. Its only
// caller is reap's --max pass, which is trying to SHED slots — recreating a
// directory a peer just removed would put back the very slot the group is
// shedding, and the pass would then delete it again on the next run,
// forever.
func TestLockExistingSlot_NeverResurrectsARemovedSlot(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := SlotDir(groupDir, 7)

	lock, err := LockExistingSlot(groupDir, gone)
	if err != nil {
		t.Fatalf("locking a slot that does not exist should not be an error: %v", err)
	}
	if lock != nil {
		lock.Release()
		t.Fatal("a slot directory that does not exist must not be lockable")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("slot-7's directory was created by the attempt to lock it: %v", err)
	}
}

// TestLockExistingSlot_ReportsBusyRatherThanBlocking keeps the pass
// non-blocking: a slot another process holds is skipped, never waited on.
func TestLockExistingSlot_ReportsBusyRatherThanBlocking(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	held, err := TryLock(lockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	lock, err := LockExistingSlot(groupDir, dir)
	if err != nil {
		t.Fatalf("locking a busy slot should report busy, not error: %v", err)
	}
	if lock != nil {
		lock.Release()
		t.Fatal("a slot whose flock is held elsewhere must not be handed out")
	}
}
