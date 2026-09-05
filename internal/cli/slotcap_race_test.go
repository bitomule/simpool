package cli

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// TestEnforceSlotCap_NeverShrinksAGroupBelowMax covers the race the cap
// pass has no other defence against. Nothing serializes two reap processes
// — launchd fires every 30 minutes, manual runs are encouraged, and a run
// stalled on a slow simctl call overlaps the next — and a peer reaper that
// holds a slot's flock while it deletes that slot is classified here as
// "held by a live consumer", which inflates len(blocked), shrinks keep, and
// would have this pass evict one extra perfectly healthy slot for every
// slot the peer removes. The per-slot re-classification cannot catch it:
// the extra victim passes every check it makes.
//
// The peer is simulated through the deleteExcess seam, which removes one
// unrelated slot directory the first time it is called. Without the
// re-count at the top of the eviction loop, this group ends below its own
// cap — a healthy simulator deleted for nothing.
func TestEnforceSlotCap_NeverShrinksAGroupBelowMax(t *testing.T) {
	now := time.Now()
	lastUsed := map[int]time.Time{}
	for n := 0; n < 5; n++ {
		lastUsed[n] = now.Add(-time.Duration(10-n) * time.Hour)
	}
	h := newCapHarness(t, lastUsed)

	origDelete := deleteExcess
	t.Cleanup(func() { deleteExcess = origDelete })
	peerRan := false
	deleteExcess = func(udid string) error {
		err := origDelete(udid)
		if !peerRan {
			peerRan = true
			// A concurrent reaper finishes deleting slot-4 right now.
			if rerr := os.RemoveAll(pool.SlotDir(h.groupDir, 4)); rerr != nil {
				t.Fatal(rerr)
			}
		}
		return err
	}

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 3, false, &stdout, &stderr)

	if !peerRan {
		t.Fatal("the simulated peer reaper never ran — the fixture no longer reaches an eviction")
	}
	if got := h.slotDirs(t); len(got) < 3 {
		t.Fatalf("group shrank to %d slots %v against --max 3: a peer reaper's deletions were counted against this pass's own budget\n%s", len(got), got, stdout.String())
	}
}

// TestEnforceSlotCap_ANeverProvisionedSlotStillAgesOut is the regression
// test for an absorbing state this pass introduced and nearly shipped. A
// slot with no meta.json has no recorded last-use, so the grace falls back
// to the slot directory's mtime — and pool.TryLock creates the lock file
// when it is missing, which bumps that exact mtime. Reading it after
// locking made every such slot look touched "0s ago" on every run, forever:
// the pass would report the group as oversized every half hour and never be
// allowed to do anything about it.
func TestEnforceSlotCap_ANeverProvisionedSlotStillAgesOut(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
		1: now.Add(-8 * time.Hour),
	})
	// No meta.json, no lock file yet, backdated well past the grace, and no
	// device in the set under its name either — nothing to delete but the
	// directory, which is exactly what costs a slot number.
	backdatedSlotDir(t, h.groupDir, 2)
	withFakeDevices(t, nil)

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 2, false, &stdout, &stderr)

	for _, n := range h.slotDirs(t) {
		if n == 2 {
			t.Fatalf("slot-2 has no meta.json and its directory is 9h old, but it survived: the grace is reading an mtime this pass bumped itself by creating the lock file\n%s", stdout.String())
		}
	}
}
