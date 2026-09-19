package pool

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// What --max means changed here, so these tests are written as a set rather
// than one each: the interesting claim is not "the cap fires" but "the cap
// fires exactly where it should and nowhere else", and only the controls can
// say the second half.
//
// The order below is the order they were written, and it is deliberate. A
// cap that queues is indistinguishable from a cap that hangs unless you also
// prove the case that must NOT queue, so the group-under-its-cap control
// comes before anything that asserts waiting.

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

// holdSlot takes slot n's flock for the duration of the test, standing in
// for another process's live `with`/`acquire`.
func holdSlot(t *testing.T, groupDir string, n int) {
	t.Helper()
	l, err := TryLock(lockPath(SlotDir(groupDir, n)))
	if err != nil {
		t.Fatalf("holding slot-%d: %v", n, err)
	}
	t.Cleanup(func() { l.Release() })
}

// TestAcquireSlots_UnderUseCapNeitherWaitsNorPrints is the control, and it
// is the one that matters most. Making --max bound use means adding a reason
// to wait, and a new wait on the common path would be a latency regression
// felt everywhere by people with no way to know why. A group holding three
// slots with nobody using them, asked for one under --max 3, must return
// immediately and say nothing.
func TestAcquireSlots_UnderUseCapNeitherWaitsNorPrints(t *testing.T) {
	root := t.TempDir()
	seedSlotDirs(t, root, "TestDevice", "1.0", 3)
	buf := captureWaitNotice(t)

	start := time.Now()
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 3 /*max*/, time.Minute)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a group with three idle slots must hand one over under --max 3, got %v", err)
	}
	defer slots[0].Release()

	if elapsed > time.Second {
		t.Errorf("handing over an idle slot took %s: the use cap must cost nothing when nobody is at it", elapsed)
	}
	if got := buf.String(); got != "" {
		t.Errorf("an acquisition that never waited must print no queue notice, got:\n%s", got)
	}
}

// TestAcquireSlots_OwnSlotsDoNotCountAgainstIt is the other control. Slots
// this same call is holding must not be counted against its own cap — flock
// is per open file description, so from the outside a caller's own slot is
// indistinguishable from a stranger's, and counting them would make
// `--count N` fail against its own `--max N` on an empty pool.
func TestAcquireSlots_OwnSlotsDoNotCountAgainstIt(t *testing.T) {
	root := t.TempDir()

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 3 /*count*/, 3 /*max*/, 2*time.Second)
	if err != nil {
		t.Fatalf("--count 3 under --max 3 on an empty pool must succeed, got %v", err)
	}
	defer func() {
		for _, s := range slots {
			s.Release()
		}
	}()
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(slots))
	}
}

// TestAcquireSlots_AtMaxQueues is the case that already worked before --max
// bounded use, and it still has to: with the group's size equal to --max, an
// acquisition whose only slot is busy waits out --wait and reports
// ErrAtCapacity. It is kept because it is the one shape where residency and
// use happen to coincide, so a change that broke it would look correct in
// every other test here.
func TestAcquireSlots_AtMaxQueues(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	holdSlot(t, g, 0)

	start := time.Now()
	_, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 2*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("a group at --max with its only slot busy must report ErrAtCapacity, got %v", err)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("the acquisition returned in %s instead of polling for its full 2s --wait", elapsed)
	}
}

// TestAcquireSlots_OverMaxQueuesOnUse is the change itself, and it is the
// inverse of what this repo did until now.
//
// Before: --max was consulted only where a NEW slot would be created, so a
// group with more slot directories than --max handed the extras out on
// demand — measured at 2 ms against a --max of 1. The flag could refuse to
// grow a group and could not refuse to use one, which is the opposite of
// what anybody reaches for it to do.
//
// After: the group has three slots and one is held, so exactly one slot is
// in use, --max 1 is met, and the second acquisition queues.
func TestAcquireSlots_OverMaxQueuesOnUse(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 3)
	holdSlot(t, g, 0)

	start := time.Now()
	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 2*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		for _, s := range slots {
			s.Release()
		}
		t.Fatalf("--max 1 with one slot already in use must not hand over a second, got slot-%d after %s", slots[0].Number, elapsed)
	}
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got %v", err)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("the acquisition gave up after %s instead of queueing for its full 2s --wait: a cap that refuses instead of waiting turns a slow job into a failed one", elapsed)
	}
}

// TestAcquireSlots_UseCapQueuesDownTheSameNoticePath proves the new wait is
// not a silent one. A ten-minute queue that prints nothing is indistinguishable
// from a hang — several nodes queueing normally were peeked, messaged and
// reported as stuck on exactly that evidence, which is what the notice was
// added for. A cap that introduced a second, mute way to wait would have
// undone it within a day.
func TestAcquireSlots_UseCapQueuesDownTheSameNoticePath(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 3)
	holdSlot(t, g, 0)
	buf := captureWaitNotice(t)

	_, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 4*time.Second)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got %v", err)
	}

	got := buf.String()
	if got == "" {
		t.Fatal("queueing on the use cap printed nothing: the wait must go down the same notice path as every other wait")
	}
	if !strings.Contains(got, "waiting") || !strings.Contains(got, "TestDevice") {
		t.Errorf("the notice must name what it is waiting for, got:\n%s", got)
	}
	// The per-slot reason is the load-bearing half of that notice, not the
	// elapsed time: "slot-0 busy" tells a reader to wait, and its absence is
	// what made normal queues look like failures.
	if !strings.Contains(got, "slot-0") {
		t.Errorf("the notice must name the slot that is holding the cap, got:\n%s", got)
	}
}

// TestAcquireSlots_UseCapErrorSaysTheCapIsOnUse guards the wording, which is
// load-bearing here for one reason: until this change the README asserted
// that --max bounded concurrency while the code bounded residency, so a
// reader hitting this error has a fair chance of arriving with the old idea.
// "At its --max of 3 resident slots" would send them to `simpool reap` or to
// raising --max, and neither is the answer — the group is not oversized and
// the queue clears by itself.
func TestAcquireSlots_UseCapErrorSaysTheCapIsOnUse(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 3)
	holdSlot(t, g, 0)

	_, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 0 /*fail immediately*/)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "in use") {
		t.Errorf("the message must say the cap is on slots IN USE, got:\n%s", msg)
	}
	if !strings.Contains(msg, "not on how many exist") {
		t.Errorf("the message must distinguish itself from the residency cap, got:\n%s", msg)
	}
	if strings.Contains(msg, "resident slot(s)") {
		t.Errorf("this is not the residency cap and must not claim to be, got:\n%s", msg)
	}
}

// TestAcquireSlots_ExpiredLeaseDoesNotHoldTheCap is the anti-hang control.
// The cap counts slots somebody is actually using; anything else holding a
// share of it would produce a queue that no amount of waiting clears, which
// is a hang wearing a safety limit's clothes. An expired lease is the
// cheapest example of a reservation that is over.
func TestAcquireSlots_ExpiredLeaseDoesNotHoldTheCap(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 2)
	if err := WriteLease(SlotDir(g, 0), Lease{Key: "gone", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	slots, err := AcquireSlots(root, "TestDevice", "1.0", 1 /*count*/, 1 /*max*/, 2*time.Second)
	if err != nil {
		t.Fatalf("an expired lease reserves nothing and must not hold a share of --max, got %v", err)
	}
	slots[0].Release()
}

// TestAcquireLease_SameKeyRenewalIsNotASecondUse is the lease path's own
// anti-hang control, and it guards a dead end this codebase has been in
// before (see ownLeaseResidue): at --max 1, a key whose own live lease was
// counted against it could never renew, and `simpool lease`'s entire reason
// for existing is a hot loop that renews once per mav action.
func TestAcquireLease_SameKeyRenewalIsNotASecondUse(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
	if err := WriteLease(SlotDir(g, 0), Lease{Key: "mav-key", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	slot, err := AcquireLease(root, "TestDevice", "1.0", "mav-key", time.Hour, 1 /*max*/)
	if err != nil {
		t.Fatalf("a key renewing its own live lease must not be capped by it, got %v", err)
	}
	if slot.Number != 0 {
		t.Errorf("stickiness: the renewal must land on slot-0, got slot-%d", slot.Number)
	}
}

// TestAcquireLease_OtherKeyIsHeldOffByTheUseCap is the lease half of the
// change. A leased slot is just as much in use as a flocked one, so leaving
// this path uncapped would have made the whole cap optional: one `simpool
// lease` and the group is over it again.
func TestAcquireLease_OtherKeyIsHeldOffByTheUseCap(t *testing.T) {
	root := t.TempDir()
	g := seedSlotDirs(t, root, "TestDevice", "1.0", 3)
	if err := WriteLease(SlotDir(g, 0), Lease{Key: "someone-else", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireLease(root, "TestDevice", "1.0", "me", time.Hour, 1 /*max*/)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("one live lease meets --max 1, so a second key must be refused; got %v", err)
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("the refusal must say the cap is on use, got:\n%s", err)
	}
}
