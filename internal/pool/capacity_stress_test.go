package pool

import (
	"errors"
	"sync"
	"testing"
)

// TestAcquireSlots_ConcurrentClaimantsNeverExceedMax hammers a brand-new
// group with far more simultaneous claimants than --max allows and asserts
// the group never ends up with more slot DIRECTORIES than --max.
//
// The directory count is the thing that matters, not how many claimants
// succeeded: a slot directory is what costs a simulator (EnsureProvisioned
// creates one per slot number) and what `max` is documented to cap. Every
// claimant seeds its `resident` map from a lock-free ListSlotNumbers read,
// so if that snapshot can go stale between the read and the mkdir, this is
// where it shows.
func TestAcquireSlots_ConcurrentClaimantsNeverExceedMax(t *testing.T) {
	const (
		max       = 3
		claimants = 24
	)
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")

	start := make(chan struct{})
	release := make(chan struct{})
	// attempted counts down once per claimant the moment its acquire
	// attempt returns — before a winner parks on `release` — so the
	// measurement below happens exactly at the pool's peak footprint.
	var attempted sync.WaitGroup
	var parked sync.WaitGroup
	attempted.Add(claimants)
	parked.Add(claimants)

	var mu sync.Mutex
	var held []*Slot

	for i := 0; i < claimants; i++ {
		go func() {
			defer parked.Done()
			<-start
			slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, max, 0)
			if err != nil {
				attempted.Done()
				if !errors.Is(err, ErrAtCapacity) {
					t.Errorf("AcquireSlots: unexpected error %v", err)
				}
				return
			}
			mu.Lock()
			held = append(held, slots...)
			mu.Unlock()
			attempted.Done()
			// Hold until the measurement is taken, so a winner can never
			// free a slot that a loser would then reuse instead of creating
			// a new one — the point is to maximise creation pressure.
			<-release
		}()
	}
	close(start)
	attempted.Wait()

	got := listSlotDirs(t, groupDir)

	close(release)
	parked.Wait()
	mu.Lock()
	for _, s := range held {
		_ = s.Release()
	}
	mu.Unlock()

	if len(got) > max {
		t.Fatalf("group has %d slot directories %v with --max %d: the cap is not enforced against concurrent claimants", len(got), got, max)
	}
}

func listSlotDirs(t *testing.T, groupDir string) []int {
	t.Helper()
	nums, err := ListSlotNumbersChecked(groupDir)
	if err != nil {
		t.Fatalf("listing slot dirs: %v", err)
	}
	return nums
}
