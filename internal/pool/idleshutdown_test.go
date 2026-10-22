package pool

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// holdForIdleShutdown makes slot n look exactly as it does while reap shuts
// its simulator down: flock held, marker written. The returned func ends the
// shutdown the way reap does, marker first and flock second.
func holdForIdleShutdown(t *testing.T, root string, n int) (finish func()) {
	t.Helper()
	dir := SlotDir(GroupDir(root, "TestDevice", "1.0"), n)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{UDID: "udid-idle", LastUsed: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	lock, err := TryLock(LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	clear, err := MarkShuttingDown(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := false
	finish = func() {
		if done {
			return
		}
		done = true
		clear()
		lock.Release()
	}
	t.Cleanup(finish)
	return finish
}

func leaseAsync(root string, max int) <-chan leaseResult {
	ch := make(chan leaseResult, 1)
	go func() {
		start := time.Now()
		s, err := AcquireLease(root, "TestDevice", "1.0", "agent", time.Minute, max)
		ch <- leaseResult{s, err, time.Since(start)}
	}()
	return ch
}

type leaseResult struct {
	slot *Slot
	err  error
	took time.Duration
}

// At --max 1 the slot being shut down is the only one the group may use. A
// lease used to count reap's flock as somebody's use and fail "at capacity"
// on the spot, seconds before the slot was free.
func TestAcquireLease_WaitsOutAnIdleShutdownInsteadOfFailing(t *testing.T) {
	root := t.TempDir()
	finish := holdForIdleShutdown(t, root, 0)

	got := leaseAsync(root, 1)
	time.Sleep(600 * time.Millisecond)
	finish()

	r := <-got
	if r.err != nil {
		t.Fatalf("a lease arriving during an idle shutdown must get the slot once the shutdown ends, got: %v", r.err)
	}
	if r.slot.Number != 0 {
		t.Fatalf("got slot-%d, want slot-0", r.slot.Number)
	}
	if r.took < 500*time.Millisecond {
		t.Fatalf("lease returned after %s, before the shutdown ended: it must never be handed a half-shut simulator", r.took)
	}
}

// With room under --max, the lease used to skip the slot being shut down and
// provision a brand-new simulator next to it: another device on disk and
// another boot, for a slot that was free seconds later.
func TestAcquireLease_PrefersTheSlotBeingShutDownOverANewSimulator(t *testing.T) {
	root := t.TempDir()
	finish := holdForIdleShutdown(t, root, 0)

	got := leaseAsync(root, 2)
	time.Sleep(600 * time.Millisecond)
	finish()

	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.slot.Number != 0 {
		t.Fatalf("got slot-%d: the lease created a new simulator instead of waiting for slot-0's shutdown", r.slot.Number)
	}
}

// A busy slot without the marker is a consumer, which can hold it for an
// hour: nothing waits on it.
func TestAcquireLease_DoesNotWaitOnAnOrdinaryHolder(t *testing.T) {
	root := t.TempDir()
	dir := SlotDir(GroupDir(root, "TestDevice", "1.0"), 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := TryLock(LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	r := <-leaseAsync(root, 2)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.slot.Number != 1 || r.took > IdleShutdownWait/2 {
		t.Fatalf("got slot-%d after %s, want slot-1 straight away", r.slot.Number, r.took)
	}
}

func TestShutdownInProgress_IgnoresADeadOrStaleMarker(t *testing.T) {
	dir := t.TempDir()
	if ShutdownInProgress(dir) {
		t.Fatal("no marker must not read as a shutdown in progress")
	}
	clear, err := MarkShuttingDown(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ShutdownInProgress(dir) {
		t.Fatal("a fresh marker from a live process must read as a shutdown in progress")
	}
	clear()
	if ShutdownInProgress(dir) {
		t.Fatal("a cleared marker must not read as a shutdown in progress")
	}

	old := time.Now().Add(-shuttingDownStale - time.Second)
	for name, m := range map[string]string{
		"dead pid": `{"pid": 999999, "started": "` + time.Now().Format(time.RFC3339Nano) + `"}`,
		"stale":    `{"pid": ` + strconv.Itoa(os.Getpid()) + `, "started": "` + old.Format(time.RFC3339Nano) + `"}`,
	} {
		if err := os.WriteFile(shuttingDownPath(dir), []byte(m), 0o644); err != nil {
			t.Fatal(err)
		}
		if ShutdownInProgress(dir) {
			t.Fatalf("%s marker must not make an acquirer wait", name)
		}
	}
}
