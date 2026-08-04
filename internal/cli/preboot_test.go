package cli

import (
	"bytes"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// TestRunPreboot_RequiresDeviceAndOS proves flag validation happens before
// pool.Root()/any filesystem writes, mirroring RunLease's own contract.
func TestRunPreboot_RequiresDeviceAndOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	var stdout, stderr bytes.Buffer
	code := RunPreboot(nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("preboot with no --device/--os: want exit 2, got %d, stderr:\n%s", code, stderr.String())
	}
}

// TestRunPreboot_MaxBelowCountIsRejected proves the same --max >= --count
// invariant acquireFlags enforces for with/acquire also holds here, before
// anything touches the pool.
func TestRunPreboot_MaxBelowCountIsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	var stdout, stderr bytes.Buffer
	code := RunPreboot([]string{"--device", "TestDevice", "--os", "1.0", "--count", "3", "--max", "1"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("preboot with --max < --count: want exit 2, got %d, stderr:\n%s", code, stderr.String())
	}
}

// TestRunPreboot_NoOpAtCapacity proves preboot never waits for capacity —
// with the group's one slot already flock-held (as with/acquire would
// leave it), it must report there's nothing to do and exit 0 immediately,
// never touching pool.EnsureProvisioned/simctl, so this needs no real
// simulator. This is also the regression test for "cannot starve a real
// acquisition": preboot must not block here waiting for the busy slot.
func TestRunPreboot_NoOpAtCapacity(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := RunPreboot([]string{"--device", "TestDevice", "--os", "1.0", "--max", "1"}, &stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("preboot at capacity should exit 0 (nothing to do, not an error), got %d, stderr:\n%s", code, stderr.String())
	}
	if elapsed > time.Second {
		t.Fatalf("preboot must never wait for capacity, took %v", elapsed)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("nothing to do")) {
		t.Errorf("expected a \"nothing to do\" message, got:\n%s", stdout.String())
	}
}

// TestRunPreboot_NeverHoldsMoreThanOneSlotAtOnce is the regression test for
// preboot's central claim: it acquires and provisions one slot at a time,
// releasing each before requesting the next, rather than taking every
// --count flock up front and holding all of them for the whole warm-up loop.
// Deterministic, no goroutines/timing needed: while provisioning slot N,
// every slot preboot already finished (0..N-1) must already be free again —
// if the old batch-acquire behavior regressed, this TryLock would find it
// still held by preboot's own outer loop and fail with ErrBusy.
func TestRunPreboot_NeverHoldsMoreThanOneSlotAtOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	device, osVersion := "TestDevice", "1.0"
	groupDir := pool.GroupDir(home, device, osVersion)

	var provisioned []int
	origProvision := provisionForPreboot
	t.Cleanup(func() { provisionForPreboot = origProvision })
	provisionForPreboot = func(s *pool.Slot, ownerCmd, mode, leaseKey string) error {
		for _, prevN := range provisioned {
			lock, err := pool.TryLock(pool.LockPath(pool.SlotDir(groupDir, prevN)))
			if err != nil {
				t.Fatalf("slot-%d is still locked while preboot is provisioning slot-%d — preboot must release each slot before requesting the next", prevN, s.Number)
			}
			_ = lock.Release()
		}
		provisioned = append(provisioned, s.Number)
		s.Meta.UDID = "fake-udid-" + strconv.Itoa(s.Number)
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := RunPreboot([]string{"--device", device, "--os", osVersion, "--count", "3", "--max", "3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("preboot: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	if len(provisioned) != 3 {
		t.Fatalf("expected 3 slots to be provisioned, got %d: %v", len(provisioned), provisioned)
	}
	seen := map[int]bool{}
	for _, n := range provisioned {
		if seen[n] {
			t.Fatalf("slot-%d was provisioned more than once — --count 3 must warm 3 DISTINCT slots, not rewarm the one AcquireSlots' most-recently-used-first policy keeps handing back, got: %v", n, provisioned)
		}
		seen[n] = true
	}
}

// TestRunPreboot_ConcurrentLeaseSucceedsDuringWarmup is the regression test
// the finding this fixes explicitly asks for: a concurrent `simpool lease`
// (mav's target_command, which by design never waits for capacity) must
// succeed while preboot is mid-warm-up, even when --count equals --max —
// the exact configuration under which the old batch-acquire behavior held
// every slot in the group for the whole loop and starved a real acquirer.
func TestRunPreboot_ConcurrentLeaseSucceedsDuringWarmup(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	device, osVersion := "TestDevice", "1.0"

	var concurrentCalled bool
	var concurrentErr error
	origProvision := provisionForPreboot
	t.Cleanup(func() { provisionForPreboot = origProvision })
	provisionForPreboot = func(s *pool.Slot, ownerCmd, mode, leaseKey string) error {
		if !concurrentCalled {
			concurrentCalled = true
			// Stands in for mav's `simpool lease` racing in while preboot
			// still holds this one slot — with --count == --max (3), the
			// old code held all 3 flocks for the entire loop, leaving
			// nothing for this call but ErrAtCapacity.
			concurrentSlots, err := pool.AcquireSlots(home, device, osVersion, 1, 3, 0)
			concurrentErr = err
			for _, cs := range concurrentSlots {
				cs.Release()
			}
		}
		s.Meta.UDID = "fake-udid-" + strconv.Itoa(s.Number)
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := RunPreboot([]string{"--device", device, "--os", osVersion, "--count", "3", "--max", "3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("preboot: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	if !concurrentCalled {
		t.Fatal("test setup bug: the concurrent-acquisition hook never ran")
	}
	if concurrentErr != nil {
		t.Fatalf("a concurrent real acquisition must succeed while preboot is mid-warm-up (never holds more than one slot at a time), got: %v", concurrentErr)
	}
}
