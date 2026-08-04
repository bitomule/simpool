package cli

import (
	"bytes"
	"os"
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
