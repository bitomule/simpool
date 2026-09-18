package pool

import (
	"os"
	"testing"
	"time"
)

// makeCapSlots creates slot directories with the given capability sets, all
// last used at the same moment so only the capability ordering can decide
// which one acquisition picks.
func makeCapSlots(t *testing.T, group string, caps map[int][]string) {
	t.Helper()
	used := time.Now().Add(-time.Minute)
	for n, c := range caps {
		dir := SlotDir(group, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		WriteMeta(dir, Meta{Device: "iPhone 17 Pro", OSVersion: "26.3", LastUsed: used, Capabilities: c})
	}
}

// --need is a dispatch criterion, not a reconfigure order: a request some
// resident slot already satisfies must land on that slot, so it is handed
// over warm instead of spending 25-40s reconfiguring whichever slot the
// most-recently-used walk happened to reach first.
func TestAcquirePrefersASlotThatAlreadyHasWhatWasAskedFor(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	makeCapSlots(t, group, map[int][]string{0: nil, 1: {"photos"}})

	SetRequestedCapabilities([]string{"photos"})
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 4, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("a photos request must land on the slot that already has photos (slot-1), got slot-%d", slots[0].Number)
	}
}

// The other half, and the one that keeps capable slots free: an ordinary
// request is satisfied by every slot, so it must take the LEANEST one
// rather than the group's only photos slot — otherwise the next photo job
// pays a reconfigure for a capability that was sitting right there.
func TestAcquireLeavesTheCapableSlotForWhoeverNeedsIt(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	makeCapSlots(t, group, map[int][]string{0: {"photos", "search"}, 1: nil})

	SetRequestedCapabilities(nil)

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 4, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("a plain request must take the leanest slot (slot-1), got slot-%d", slots[0].Number)
	}
}

// Under the cap, a request nothing resident can serve ADDS a slot that can
// rather than stripping one that already serves somebody else. The next
// request for the same thing then finds it warm.
func TestAcquireProvisionsANewSlotRatherThanReconfiguringAMatchingOne(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	makeCapSlots(t, group, map[int][]string{0: {"photos"}})

	SetRequestedCapabilities([]string{"spotlight"})
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 4, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number == 0 {
		t.Fatal("under the cap, a request nothing matches must open a new slot, not take the photos one")
	}
}

// And with the group full it falls back to reconfiguring. The cap is never
// exceeded to satisfy a capability: a profile is a property of a slot
// inside the device+OS group, never a group of its own.
func TestAcquireReconfiguresOnlyWhenTheGroupIsFull(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	makeCapSlots(t, group, map[int][]string{0: {"photos"}})

	SetRequestedCapabilities([]string{"spotlight"})
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 1, 1, 0)
	if err != nil {
		t.Fatalf("a full group must still serve the request by reconfiguring: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 0 {
		t.Fatalf("at max=1 the only slot must be handed over for reconfiguring, got slot-%d", slots[0].Number)
	}
}

// A slot is never handed out twice. The passes walk the same slot list more
// than once, and flock is per open file description — a second claim from
// this same process would succeed — so the pass structure has to skip what
// it already holds.
func TestAcquireNeverHandsTheSameSlotOutTwice(t *testing.T) {
	root := t.TempDir()
	group := GroupDir(root, "iPhone 17 Pro", "26.3")
	makeCapSlots(t, group, map[int][]string{0: nil, 1: nil})

	SetRequestedCapabilities([]string{"photos"})
	t.Cleanup(func() { SetRequestedCapabilities(nil) })

	slots, err := AcquireSlots(root, "iPhone 17 Pro", "26.3", 2, 2, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() {
		for _, s := range slots {
			s.Release()
		}
	}()
	if slots[0].Number == slots[1].Number {
		t.Fatalf("both slots are slot-%d", slots[0].Number)
	}
}

func TestSatisfiesIsSupersetNotEquality(t *testing.T) {
	if !Satisfies([]string{"photos", "search"}, []string{"photos"}) {
		t.Error("a richer slot must serve a narrower request, or the pool grows one family per combination")
	}
	if Satisfies([]string{"photos"}, []string{"photos", "search"}) {
		t.Error("a slot missing a requested category must not be treated as satisfying it")
	}
	if !Satisfies(nil, nil) {
		t.Error("a request for nothing is satisfied by any slot")
	}
}
