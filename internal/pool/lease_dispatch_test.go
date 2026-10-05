package pool

import (
	"os"
	"testing"
	"time"
)

type capSlot struct {
	caps []string
	ago  time.Duration
}

func makeAgedCapSlots(t *testing.T, group, device, osVersion string, slots map[int]capSlot) {
	t.Helper()
	for n, s := range slots {
		dir := SlotDir(group, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		WriteMeta(dir, Meta{Device: device, OSVersion: osVersion, LastUsed: time.Now().Add(-s.ago), Capabilities: s.caps})
	}
}

func leaseFrom(t *testing.T, slots map[int]capSlot, need []string, max int) int {
	t.Helper()
	root := t.TempDir()
	makeAgedCapSlots(t, GroupDir(root, "iPhone Duo", "27.1"), "iPhone Duo", "27.1", slots)

	SetRequestedCapabilities(need)
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slot, err := AcquireLease(root, "iPhone Duo", "27.1", "duo-key", time.Hour, max)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return slot.Number
}

// The shape reported against the iPhone Duo group: slot-0 already had photos
// and was free, slot-1 was slim and had been used more recently. A
// `lease --need photos` walked most-recently-used first, took slot-1 and
// rebooted it into photos, while the slot that could serve the request warm
// sat idle.
func TestLeasePrefersASlotThatAlreadyHasWhatWasAskedFor(t *testing.T) {
	got := leaseFrom(t, map[int]capSlot{
		0: {caps: []string{"photos"}, ago: time.Hour},
		1: {caps: nil, ago: time.Minute},
	}, []string{"photos"}, 2)
	if got != 0 {
		t.Fatalf("a photos lease must land on the free slot that already has photos (slot-0), got slot-%d", got)
	}
}

func TestLeaseLeavesTheCapableSlotForWhoeverNeedsIt(t *testing.T) {
	got := leaseFrom(t, map[int]capSlot{
		0: {caps: []string{"photos"}, ago: time.Minute},
		1: {caps: nil, ago: time.Hour},
	}, nil, 2)
	if got != 1 {
		t.Fatalf("a plain lease must take the leanest slot (slot-1), got slot-%d", got)
	}
}

func TestLeaseProvisionsANewSlotRatherThanReconfiguringOne(t *testing.T) {
	got := leaseFrom(t, map[int]capSlot{
		0: {caps: nil, ago: time.Minute},
	}, []string{"photos"}, 4)
	if got == 0 {
		t.Fatal("under the cap, a lease nothing matches must open a new slot, not reboot the slim one")
	}
}

func TestLeaseReconfiguresOnlyWhenTheGroupIsFull(t *testing.T) {
	got := leaseFrom(t, map[int]capSlot{
		0: {caps: nil, ago: time.Minute},
	}, []string{"photos"}, 1)
	if got != 0 {
		t.Fatalf("at max=1 the only slot must be handed over for reconfiguring, got slot-%d", got)
	}
}

// Reconfiguring reboots the simulator, so the warm-slot reason for walking
// most-recently-used first no longer applies — and the most recently used
// slot is the one most likely to still be in somebody's hands without a
// live lease (a MAV session pinned by UDID, a lease that just lapsed). The
// reconfigure pass reboots the slot idle the longest instead.
func TestLeaseReconfiguresTheLeastRecentlyUsedSlot(t *testing.T) {
	got := leaseFrom(t, map[int]capSlot{
		0: {caps: nil, ago: time.Minute},
		1: {caps: nil, ago: 3 * time.Hour},
	}, []string{"photos"}, 2)
	if got != 1 {
		t.Fatalf("the reconfigure must land on the slot idle longest (slot-1), got slot-%d", got)
	}
}

func TestAcquireReconfiguresTheLeastRecentlyUsedSlot(t *testing.T) {
	root := t.TempDir()
	makeAgedCapSlots(t, GroupDir(root, "iPhone 17 Pro", "26.3"), "iPhone 17 Pro", "26.3", map[int]capSlot{
		0: {ago: time.Minute},
		1: {ago: 3 * time.Hour},
	})

	SetRequestedCapabilities([]string{"photos"})
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 2, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("the reconfigure must land on the slot idle longest (slot-1), got slot-%d", slots[0].Number)
	}
}
