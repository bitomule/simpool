package pool

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// TestDefaultLeaseTTL_IsShortEnoughToRotateASmallPool pins DefaultLeaseTTL
// at 3 minutes so a future change doesn't silently drift back toward the
// old 30-minute default. The number matters operationally, not just as a
// magic constant: with --max slots per group shared by several repos, a
// TTL this short is what lets an idle repo give its slot back quickly
// enough for others to rotate through a small pool, while still comfortably
// covering the gap between consecutive hot-loop calls (seconds, not
// minutes) that it actually exists to bridge. A longer silent gap — the
// build inside one `mav run` step — is deliberately not this TTL's problem
// to solve; that is MAV's own target_command keepalive's job (see MAV's
// README, "MAV in the hot loop" below).
func TestDefaultLeaseTTL_IsShortEnoughToRotateASmallPool(t *testing.T) {
	if DefaultLeaseTTL != 3*time.Minute {
		t.Fatalf("DefaultLeaseTTL=%v, want 3m", DefaultLeaseTTL)
	}
}

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
	firstLease, err := ReadLease(first.Dir)
	if err != nil {
		t.Fatalf("reading first lease: %v", err)
	}
	firstExpiry := firstLease.ExpiresAt

	time.Sleep(10 * time.Millisecond)

	second, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 3)
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	if second.Number != first.Number {
		t.Fatalf("same key landed on different slots: %d then %d", first.Number, second.Number)
	}
	secondLease, err := ReadLease(second.Dir)
	if err != nil {
		t.Fatalf("reading second lease: %v", err)
	}
	secondExpiry := secondLease.ExpiresAt
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
	lease, err := ReadLease(slot.Dir)
	if err != nil {
		t.Fatalf("reading lease: %v", err)
	}
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
	if lease, _ := ReadLease(a.Dir); lease.Key != "" {
		t.Fatalf("repo-a's lease.json should be gone after release, got %+v", lease)
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

// TestAcquireLease_RecoversVerifiedOrphanLeftByWith proves `simpool lease`
// can reclaim a slot left poisoned by a `with` session that got SIGKILLed —
// not just `with`/`acquire`'s own take(): whichever caller happens to be
// the next one to touch a poisoned slot must be able to recover it, cross-
// mode. AttemptRecovery still gates on the *previous* consumer's own
// recorded Mode ("with"), never on which command is doing the reclaiming.
func TestAcquireLease_RecoversVerifiedOrphanLeftByWith(t *testing.T) {
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
	go func() { _ = orphan.Wait() }() // see poison_test.go's spawnRealOrphan

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("capturing orphan's start time: %v", err)
	}
	if err := WriteMeta(slotDir, Meta{
		UDID:              "simpool-test-udid-lease-recovers-with",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
	}); err != nil {
		t.Fatal(err)
	}

	slot, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 1)
	if err != nil {
		t.Fatalf("lease against a verified, recoverable `with` orphan at max=1: want success, got %v", err)
	}
	if slot.Number != 0 {
		t.Fatalf("expected the recovered slot-0 to be reused, got slot-%d", slot.Number)
	}
	if lease, _ := ReadLease(slot.Dir); lease.Key != "repo-a" {
		t.Fatalf("expected slot-0's lease to belong to repo-a, got %+v", lease)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if !procs.PGIDAlive(pgid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the with-mode orphan should have been killed by lease's recovery")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestClaimSlotForLease_NeverRacesConcurrentFlockAcquire proves
// AcquireSlots (`with`/`acquire`) and AcquireLease (`simpool lease`) racing
// for the very same, single-slot group never both win: the group
// allocation lock already serializes claimSlotForLease's whole scan against
// take()'s per-slot mkdir+TryLock step (see claimSlotForLease's doc
// comment), and claimSlotForLease additionally takes the slot's own flock
// across its own check-and-mutate sequence — the same discipline
// AttemptRecovery's other callers (take(), reap) already follow. Hammered
// many times: a lock-ordering bug of this shape does not necessarily
// reproduce on every single attempt.
func TestClaimSlotForLease_NeverRacesConcurrentFlockAcquire(t *testing.T) {
	for i := 0; i < 200; i++ {
		root := t.TempDir()

		var wg sync.WaitGroup
		var withSlots []*Slot
		var withErr error
		var leaseSlot *Slot
		var leaseErr error

		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			withSlots, withErr = AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
		}()
		go func() {
			defer wg.Done()
			<-start
			leaseSlot, leaseErr = AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 1)
		}()
		close(start)
		wg.Wait()

		withWon := withErr == nil
		leaseWon := leaseErr == nil
		if withWon && leaseWon && withSlots[0].Number == leaseSlot.Number {
			t.Fatalf("iteration %d: with/acquire and lease both won slot-%d at max=1 — two consumers on one simulator", i, withSlots[0].Number)
		}
		if !withWon && !leaseWon {
			t.Fatalf("iteration %d: both with/acquire and lease failed at max=1 with one slot available: withErr=%v leaseErr=%v", i, withErr, leaseErr)
		}
		if withWon {
			withSlots[0].Release()
		}
		if leaseWon {
			if _, err := ReleaseLease(root, "repo-a"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestReadLease_MissingFileIsFreeNotError proves the genuinely-free case:
// no lease.json at all yields a zero Lease and no error — the only case
// that may ever be read as "no lease".
func TestReadLease_MissingFileIsFreeNotError(t *testing.T) {
	dir := t.TempDir()
	lease, err := ReadLease(dir)
	if err != nil {
		t.Fatalf("a missing lease.json must not be an error, got %v", err)
	}
	if lease.Key != "" || lease.Alive() {
		t.Fatalf("a missing lease.json must read as no lease, got %+v", lease)
	}
}

// TestReadLease_UnreadableFileIsErrorNotFree is the regression test for the
// class of bug that hit v0.6.0: reading "I couldn't check" as "confirmed
// free". A directory sitting where lease.json should be forces os.ReadFile
// to fail deterministically (no platform-specific permission bits needed,
// and root-proof) — standing in for EMFILE/permission/I-O failures in
// production. ReadLease must report this as an error, never silently as a
// zero (i.e. "no lease") Lease.
func TestReadLease_UnreadableFileIsErrorNotFree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(leasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	lease, err := ReadLease(dir)
	if err == nil {
		t.Fatalf("expected an error reading an unreadable lease.json, got lease=%+v, nil error", lease)
	}
	if lease.Key != "" || lease.Alive() {
		t.Fatalf("an unreadable lease.json must never read as an alive or claimable lease, got %+v", lease)
	}
}

// TestReadLease_CorruptFileIsErrorNotFree proves truncated/corrupt JSON is
// reported as an error too — the old code silently discarded
// json.Unmarshal's error and returned a zero Lease exactly as if the file
// had never existed.
func TestReadLease_CorruptFileIsErrorNotFree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath(dir), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	lease, err := ReadLease(dir)
	if err == nil {
		t.Fatalf("expected an error reading corrupt lease.json, got lease=%+v, nil error", lease)
	}
	if lease.Key != "" {
		t.Fatalf("corrupt lease.json must never read as a valid lease, got %+v", lease)
	}
}

// TestAcquireSlots_SkipsSlotWithUnreadableLease proves the fix reaches
// with/acquire's own caller: a slot whose lease.json cannot be read must
// never be handed out just because it "looked" free by every other measure
// (flock free, no poison). Mirrors TestAcquireSlots_SkipsSlotWithLiveLease.
func TestAcquireSlots_SkipsSlotWithUnreadableLease(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(leasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}

	// max=1 and slot-0's lease can't be verified — must not be treated as
	// free, and must not create a second slot beyond --max either.
	start := time.Now()
	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("with/acquire against a slot with unreadable lease.json at max=1: want ErrAtCapacity, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("AcquireSlots must fail immediately at capacity, not wait")
	}

	// max=2 must skip slot-0 and land on a fresh slot-1 instead of trusting
	// the unreadable lease as "free".
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1, 2, 0)
	if err != nil {
		t.Fatalf("with/acquire against a slot with unreadable lease.json at max=2: %v", err)
	}
	defer slots[0].Release()
	if slots[0].Number != 1 {
		t.Fatalf("expected with/acquire to land on a fresh slot-1 (slot-0's lease.json is unreadable), got slot-%d", slots[0].Number)
	}
}

// TestAcquireLease_SkipsSlotWithUnreadableLease is the lease-claiming
// side's counterpart: claimSlotForLease must apply the same "can't verify
// means busy" rule to a slot whose OWN prior lease.json cannot be read.
func TestAcquireLease_SkipsSlotWithUnreadableLease(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(leasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 1)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("lease against a slot with unreadable lease.json at max=1: want ErrAtCapacity, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("AcquireLease must fail immediately at capacity, not wait")
	}

	slot, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 2)
	if err != nil {
		t.Fatalf("lease at max=2 should land on a fresh slot instead of the unreadable one: %v", err)
	}
	if slot.Number != 1 {
		t.Fatalf("expected a fresh slot-1 (slot-0's lease.json is unreadable), got slot-%d", slot.Number)
	}
}

// TestCleanupExpiredLease_UnreadableFileReturnsError proves reap's own
// pruning path can't silently no-op past a lease.json it can no longer
// read: it must return an error, never removed=false-with-nil-error (which
// reap.go would otherwise misreport as an ordinary "renewed just in time"
// and, worse, without this fix could fall through to idle/cold/purge
// accounting on a slot it never actually verified as unleased).
func TestCleanupExpiredLease_UnreadableFileReturnsError(t *testing.T) {
	groupDir := t.TempDir()
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(leasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := CleanupExpiredLease(groupDir, dir)
	if err == nil {
		t.Fatalf("expected an error, got removed=%v, nil error", removed)
	}
	if removed {
		t.Fatalf("must never report removed=true when the lease could not even be read")
	}
}

// TestReleaseLease_UnreadableSlotDoesNotBlockOthers proves a transient read
// failure on one unrelated slot's lease.json never stops repo-a's own,
// perfectly readable lease from being released elsewhere — but is still
// surfaced as an error so the caller (`simpool release`) knows the sweep
// was incomplete, rather than silently treating the unreadable slot as "not
// mine, nothing to do".
func TestReleaseLease_UnreadableSlotDoesNotBlockOthers(t *testing.T) {
	root := t.TempDir()

	a, err := AcquireLease(root, "TestDevice", "1.0", "repo-a", time.Hour, 2)
	if err != nil {
		t.Fatalf("lease repo-a: %v", err)
	}

	groupDir := GroupDir(root, "TestDevice", "1.0")
	badDir := SlotDir(groupDir, 5)
	if err := os.MkdirAll(leasePath(badDir), 0o755); err != nil {
		t.Fatal(err)
	}

	released, err := ReleaseLease(root, "repo-a")
	if err == nil {
		t.Fatalf("expected ReleaseLease to report the unreadable slot's read failure")
	}
	if len(released) != 1 || released[0] != a.Dir {
		t.Fatalf("repo-a's own lease should still be released despite the unrelated unreadable slot, got %v", released)
	}
	if lease, _ := ReadLease(a.Dir); lease.Key != "" {
		t.Fatalf("repo-a's lease.json should be gone after release, got %+v", lease)
	}
}
