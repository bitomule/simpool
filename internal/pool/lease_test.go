package pool

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// TestAcquireLease_StickyByKey proves the whole point of `simpool lease`:
// repeated calls with the same key land on the same slot and renew its
// TTL, rather than wandering to a different slot number each time — that
// stickiness is what lets every `mav tap`/`mav swipe`/... from one repo
// target the same simulator.
func TestAcquireLease_StickyByKey(t *testing.T) {
	root := t.TempDir()

	first, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 3)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	firstExpiry := ReadLease(first.Dir).ExpiresAt

	time.Sleep(10 * time.Millisecond)

	second, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 3)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	if second.Number != first.Number {
		t.Fatalf("same key landed on different slots: %d then %d", first.Number, second.Number)
	}
	secondExpiry := ReadLease(second.Dir).ExpiresAt
	if !secondExpiry.After(firstExpiry) {
		t.Fatalf("renewal did not push ExpiresAt forward: first=%v second=%v", firstExpiry, secondExpiry)
	}
}

// TestAcquireLease_TwoKeysGetTwoSlots proves two independent keys never
// share a slot: each gets its own, distinct slot number.
func TestAcquireLease_TwoKeysGetTwoSlots(t *testing.T) {
	root := t.TempDir()

	a, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 3)
	if err != nil {
		t.Fatalf("lease repo-a: %v", err)
	}
	b, err := AcquireLease(root, "TestDevice", "1.0", "repo-b", time.Hour, 3)
	if err != nil {
		t.Fatalf("lease repo-b: %v", err)
	}
	if a.Number == b.Number {
		t.Fatalf("two different keys landed on the same slot %d", a.Number)
	}
}

// TestAcquireLease_ExpiredLeaseIsReusable proves a lease that has expired
// no longer reserves its slot: a new key must be able to claim it instead
// of being forced to mint a brand-new slot.
func TestAcquireLease_ExpiredLeaseIsReusable(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteLease(dir, Lease{Key: "old-repo", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	slot, err := AcquireLease(root, "TestDevice", "1.0", "new-repo", time.Hour, 1)
	if err != nil {
		t.Fatalf("lease with only an expired lease resident: %v", err)
	}
	if slot.Number != 0 {
		t.Fatalf("expected the expired lease's slot-0 to be reused, got slot-%d", slot.Number)
	}
	lease := ReadLease(slot.Dir)
	if lease.Key != "new-repo" {
		t.Fatalf("expected slot-0's lease to now belong to new-repo, got %q", lease.Key)
	}
}

// TestAcquireLease_RefusesCapacityAboveMax proves --max is a real cap for
// lease too, and that lease fails immediately rather than waiting.
func TestAcquireLease_RefusesCapacityAboveMax(t *testing.T) {
	root := t.TempDir()

	if _, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 1); err != nil {
		t.Fatalf("first lease at max=1: %v", err)
	}

	start := time.Now()
	_, err := AcquireLease(root, "TestDevice", "1.0", "repo-b", time.Hour, 1)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("second key at max=1 with slot-0 leased: want ErrAtCapacity, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("AcquireLease must fail immediately at capacity, took %v", elapsed)
	}
}

// TestAcquireSlots_SkipsSlotWithLiveLease is the with/acquire-side
// counterpart: a slot currently reserved by a live `simpool lease` must
// never be handed to `with`/`acquire`, even though a lease never holds
// the flock and so looks "free" by that measure alone.
func TestAcquireSlots_SkipsSlotWithLiveLease(t *testing.T) {
	root := t.TempDir()

	leased, err := AcquireLease(root, "TestDevice", "1.0", "hot-repo", time.Hour, 1)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased.Number != 0 {
		t.Fatalf("expected the lease to land on slot-0, got slot-%d", leased.Number)
	}

	// max=1 and slot-0 is leased (not flock-held) — AcquireSlots must not
	// steal it, and must not create a second slot beyond --max either.
	_, err = AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("with/acquire against a leased slot-0 at max=1: want ErrAtCapacity, got %v", err)
	}

	// max=2 must skip the leased slot-0 and create a fresh slot-1 instead
	// of reusing it.
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 2, 0)
	if err != nil {
		t.Fatalf("with/acquire against a leased slot-0 at max=2: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("expected with/acquire to land on a fresh slot-1 (slot-0 is leased), got slot-%d", slots[0].Number)
	}
}

// TestAcquireLease_SkipsSlotWithLiveFlock is the reverse: a slot
// currently held by a live `with`/`acquire` flock must never be handed
// out as a lease.
func TestAcquireLease_SkipsSlotWithLiveFlock(t *testing.T) {
	root := t.TempDir()

	held, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if err != nil {
		t.Fatalf("with/acquire: %v", err)
	}
	defer held[0].Release()
	if held[0].Number != 0 {
		t.Fatalf("expected with/acquire to land on slot-0, got slot-%d", held[0].Number)
	}

	start := time.Now()
	_, err = AcquireLease(root, "TestDevice", "1.0", "hot-repo", time.Hour, 1)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("lease against a flock-held slot-0 at max=1: want ErrAtCapacity, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("AcquireLease must fail immediately at capacity, took %v", elapsed)
	}
}

// TestAcquireLease_ConcurrentDifferentKeysNeverCollide is the race test
// the design explicitly calls for: several `simpool lease` calls with
// distinct keys, launched to overlap as much as possible, must each land
// on a distinct slot — never sharing one. Real concurrency, not a
// sequential simulation: BSD flock (which the group allocation lock at
// the heart of AcquireLease relies on) is scoped to the open file
// description, not the process, so contending goroutines in one process
// exercise the exact same kernel-level exclusion two separate `simpool`
// processes would.
func TestAcquireLease_ConcurrentDifferentKeysNeverCollide(t *testing.T) {
	root := t.TempDir()
	const n = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	slots := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			key := "repo-" + string(rune('a'+i))
			s, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, n)
			if err != nil {
				errs[i] = err
				return
			}
			slots[i] = s.Number
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[int]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
		if seen[slots[i]] {
			t.Fatalf("slot %d handed out to more than one key: %v", slots[i], slots)
		}
		seen[slots[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct slots, got %v", n, slots)
	}
}

// TestReleaseLease proves `simpool release`'s underlying primitive: it
// drops exactly the named key's lease, freeing the slot for immediate
// reuse instead of making a new claimant wait out the TTL, and leaves an
// unrelated key's lease untouched.
func TestReleaseLease(t *testing.T) {
	root := t.TempDir()

	a, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 2)
	if err != nil {
		t.Fatalf("lease repo-a: %v", err)
	}
	if _, err := AcquireLease(root, "TestDevice", "1.0", "repo-b", time.Hour, 2); err != nil {
		t.Fatalf("lease repo-b: %v", err)
	}

	released, err := ReleaseLease(root, "repo-a")
	if err != nil {
		t.Fatalf("release repo-a: %v", err)
	}
	if len(released) != 1 || released[0] != a.Dir {
		t.Fatalf("expected release to report repo-a's slot dir %s, got %v", a.Dir, released)
	}
	if ReadLease(a.Dir).Key != "" {
		t.Fatalf("repo-a's lease.json should be gone after release, got %+v", ReadLease(a.Dir))
	}

	// repo-a's slot must now be immediately reusable at max=2 (it was
	// already fully occupied by a+b before the release).
	fresh, err := AcquireLease(root, "TestDevice", "1.0", "repo-c", time.Hour, 2)
	if err != nil {
		t.Fatalf("lease repo-c after releasing repo-a: %v", err)
	}
	if fresh.Number != a.Number {
		t.Fatalf("expected repo-c to reuse repo-a's freshly released slot-%d, got slot-%d", a.Number, fresh.Number)
	}

	// Releasing an unknown key is a no-op, not an error.
	released, err = ReleaseLease(root, "no-such-key")
	if err != nil {
		t.Fatalf("release unknown key: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("release of an unknown key should report nothing released, got %v", released)
	}
}
