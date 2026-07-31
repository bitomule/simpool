package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// makeAbandonedSlotDir creates a slot directory that looks like a real
// failed-provisioning leftover: its lock file already exists (from the
// original acquisition attempt, taken and released once, exactly like
// AcquireSlots' take() does) before being back-dated. Pre-creating the
// lock file matters — reap's own TryLock call inside reapSlot opens it
// with O_CREATE, and creating a directory entry that doesn't exist yet
// bumps the directory's mtime, which would silently undo a Chtimes done
// beforehand and make the "old" slot look freshly touched.
func makeAbandonedSlotDir(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestReapSlot_RemovesDeadNeverProvisionedSlotDir is the regression test
// for the finding that nothing ever deleted a dead slot: a slot directory
// left behind by a provisioning attempt that failed before writing a UDID
// to meta.json (simctl.Create errored, ResolveRuntime failed, ...) used to
// sit there forever — an empty, free, lock-file-holding directory that
// reap had no path to ever remove. It never touches simctl (meta.UDID is
// empty), so this can run without booting a real simulator.
func TestReapSlot_RemovesDeadNeverProvisionedSlotDir(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	makeAbandonedSlotDir(t, dir, 2*deadSlotGrace)

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0 /*n*/, 0 /*coldMinutes*/, 1 /*purgeMinutes*/, time.Hour, 3*time.Minute, false, &stdout, &stderr)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("slot directory %s should have been removed, stat err = %v\nstdout:\n%s\nstderr:\n%s", dir, err, stdout.String(), stderr.String())
	}
}

// TestReapSlot_KeepsFreshNeverProvisionedSlotDir proves the grace period
// actually gates the deletion above: a slot directory created moments ago
// (indistinguishable, from reap's vantage point, from another process
// mid-way through AcquireSlots/EnsureProvisioned) must not be swept.
func TestReapSlot_KeepsFreshNeverProvisionedSlotDir(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, false, &stdout, &stderr)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fresh slot directory should survive reap, stat err = %v\nstdout:\n%s", err, stdout.String())
	}
}

// TestReapSlot_PurgeDisabledKeepsDeadSlotDir proves --purge 0 (the
// default) never deletes anything, dead or not — purging is opt-in.
func TestReapSlot_PurgeDisabledKeepsDeadSlotDir(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	makeAbandonedSlotDir(t, dir, 2*deadSlotGrace)

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0 /*purgeMinutes disabled*/, time.Hour, 3*time.Minute, false, &stdout, &stderr)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("slot directory should survive with --purge disabled, stat err = %v", err)
	}
}

// TestReapSlot_DryRunNeverDeletes proves --dry-run reports what it would
// do without actually removing anything, for the dead-slot-directory path.
func TestReapSlot_DryRunNeverDeletes(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	makeAbandonedSlotDir(t, dir, 2*deadSlotGrace)

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, true /*dryRun*/, &stdout, &stderr)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("--dry-run must not remove the slot directory, stat err = %v", err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("PURGE")) {
		t.Errorf("--dry-run should still report what it would purge, got:\n%s", stdout.String())
	}
}

// TestReapHeldSlot_NeverKillsAcquireHolder is the regression test for the
// finding that reap's stuck-holder detection, before Meta.Mode existed,
// could not tell a `simpool acquire` holder (which legitimately has zero
// children for its entire correct lifetime) apart from a stuck `simpool
// with`. Held slots short-circuit on Meta.Mode before any process
// inspection, so this needs no real processes or simulators either.
func TestReapHeldSlot_NeverKillsAcquireHolder(t *testing.T) {
	dir := t.TempDir()
	if err := pool.WriteMeta(dir, pool.Meta{Mode: "acquire", LastUsed: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapHeldSlot(dir, filepath.Base(dir), 3*time.Minute, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("BUSY")) {
		t.Errorf("an `acquire` holder must be reported BUSY, not STUCK, got:\n%s", stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("STUCK")) {
		t.Errorf("reap must never treat an `acquire` holder as stuck, got:\n%s", stdout.String())
	}
}
