package cli

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// TestRunLease_FailsFastAtCapacity proves `simpool lease` never waits for
// a slot the way `with`/`acquire` can: at capacity, with the one slot
// already flock-held (as `with`/`acquire` would leave it), it must return
// a non-zero exit and an actionable stderr message immediately. This
// never reaches pool.EnsureProvisioned/simctl — AcquireLease fails at
// capacity before any of that runs — so it needs no real simulator.
func TestRunLease_FailsFastAtCapacity(t *testing.T) {
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
	code := RunLease([]string{"--device", "TestDevice", "--os", "1.0", "--key", "repo-a", "--max", "1"}, &stdout, &stderr)
	elapsed := time.Since(start)

	if code == 0 {
		t.Fatalf("lease at capacity should fail, got exit 0, stdout:\n%s", stdout.String())
	}
	if elapsed > time.Second {
		t.Fatalf("simpool lease must fail immediately at capacity, took %v", elapsed)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("capacity")) {
		t.Errorf("expected an actionable capacity message on stderr, got:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a failed lease must print nothing on stdout, got:\n%s", stdout.String())
	}
}

// TestRunLease_RequiresDeviceAndOS proves flag validation happens before
// anything else (no pool.Root(), no filesystem writes).
func TestRunLease_RequiresDeviceAndOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	var stdout, stderr bytes.Buffer
	code := RunLease(nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("lease with no --device/--os: want exit 2, got %d, stderr:\n%s", code, stderr.String())
	}
}

// TestRunRelease_NoActiveLeaseIsNotAnError proves releasing a key with
// nothing leased is reported, not treated as a failure.
func TestRunRelease_NoActiveLeaseIsNotAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	var stdout, stderr bytes.Buffer
	code := RunRelease([]string{"--key", "nobody-leased-this"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("release of an unheld key should exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("no active lease")) {
		t.Errorf("expected a \"no active lease\" message, got:\n%s", stdout.String())
	}
}

// TestRunRelease_DropsLeaseImmediately proves `simpool release` actually
// clears a live lease (assigned directly via pool.AcquireLease, bypassing
// EnsureProvisioned/simctl — RunRelease itself never touches simctl at
// all) so the slot becomes available before its TTL elapses.
func TestRunRelease_DropsLeaseImmediately(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	slot, err := pool.AcquireLease(home, "TestDevice", "1.0", "repo-a", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunRelease([]string{"--key", "repo-a"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("release: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("released")) {
		t.Errorf("expected a \"released\" confirmation, got:\n%s", stdout.String())
	}
	if lease := pool.ReadLease(slot.Dir); lease.Key != "" {
		t.Fatalf("lease should be gone after release, got %+v", lease)
	}
}
