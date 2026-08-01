package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
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

// TestReapSlot_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv is the regression
// test for the CRITICAL finding that reap's poisoned-slot check relied
// entirely on `pgrep -f <udid>` (procs.LiveConsumers), which is blind to a
// consumer that only ever receives the UDID by environment variable
// (MAV_TARGET_UDID, SIMPOOL_UDID_N — exactly simpool's own handoff
// contract, design doc §5, and exactly how `mav run` receives it) rather
// than anywhere in its own argv.
//
// This reproduces the real failure surface, not the sanitized proxy an
// earlier version of this suite used (`xcrun simctl spawn <udid> log
// stream`, which — unlike a real MAV consumer — does put the UDID in its
// own argv and so was never blind to the old pgrep-based check in the
// first place). The process below stands in for `simpool with`'s child
// (and the `mav run` it in turn execs): it is the leader of its own
// process group, exactly like with.go's Setpgid child, and has nothing in
// its command line a UDID-based pgrep could ever match.
//
// Confirmed against the pre-fix commit (ef5adeb, before ConsumerPGID/
// PGIDAlive existed): this test fails there — reap sees no argv match,
// decides the slot is idle, and would shut down (`--cold 0`) a simulator
// that still has a live consumer attached to it. `simpool` itself needs no
// real simulator to reach this decision: the poisoned check runs, and this
// test asserts on, before reapSlot ever calls simctl.Find.
func TestReapSlot_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	fakeUDID := "simpool-test-udid-not-in-any-argv"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:         fakeUDID,
		Mode:         "with",
		ConsumerPGID: pgid,
	}); err != nil {
		t.Fatal(err)
	}
	// Slot-0's lock starts free (never taken): simulates `simpool` itself
	// being SIGKILLed (design doc §4's one accepted failure window) — the
	// kernel released its flock immediately, but the child it launched,
	// recorded by pgid and not by anything pgrep-able, is still running.

	// Sanity check that this really is invisible to the old mechanism:
	// if this ever starts matching, the test stops exercising the bug.
	if live, _ := procs.LiveConsumers(fakeUDID); len(live) != 0 {
		t.Fatalf("test setup broken: fakeUDID must not be visible to a pgrep-based check, got live=%v", live)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0 /*n*/, 0 /*coldMinutes*/, 0 /*purgeMinutes*/, time.Hour, 3*time.Minute, false /*dryRun*/, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Fatalf("reap should SKIP a slot whose consumer is alive only via ConsumerPGID (no UDID anywhere in argv), got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !procs.PGIDAlive(pgid) {
		t.Fatal("test setup broken: orphan process group should still be alive after reapSlot ran")
	}
}

// TestReapSlot_SkipsSlotWithActiveLease proves reap never touches a slot
// currently reserved by a live `simpool lease` — a lease's flock is free
// by design (see lease.go), so without this check reap would treat an
// actively hot-looped MAV slot exactly like any other idle free slot.
// purgeMinutes is enabled and the slot is old enough to otherwise be
// purged, so a failure here would actually delete the (never-provisioned,
// in this test) slot directory out from under the lease.
func TestReapSlot_SkipsSlotWithActiveLease(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	makeAbandonedSlotDir(t, dir, 2*deadSlotGrace)
	if err := pool.WriteLease(dir, pool.Lease{Key: "hot-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Fatalf("reap should SKIP a slot with an active lease, got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("slot directory with an active lease must survive reap, stat err = %v", err)
	}
	if lease := pool.ReadLease(dir); lease.Key != "hot-repo" {
		t.Fatalf("active lease must be left untouched, got %+v", lease)
	}
}

// TestReapSlot_RemovesExpiredLeaseFile proves reap tidies up a stale
// lease.json once it has actually expired, so `simpool status` doesn't
// keep reporting a reservation that no longer means anything.
func TestReapSlot_RemovesExpiredLeaseFile(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteLease(dir, pool.Lease{Key: "old-repo", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// purgeMinutes=0 (disabled) so the never-provisioned-slot path leaves
	// the directory itself alone — this test is only about the lease file.
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, false, &stdout, &stderr)

	if lease := pool.ReadLease(dir); lease.Key != "" {
		t.Fatalf("expired lease.json should have been removed, got %+v\nstdout:\n%s", lease, stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("expired lease")) {
		t.Errorf("reap should report removing the expired lease, got:\n%s", stdout.String())
	}
}

// TestReapSlot_DryRunNeverRemovesExpiredLease proves --dry-run reports
// what it would clean up without actually touching lease.json.
func TestReapSlot_DryRunNeverRemovesExpiredLease(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteLease(dir, pool.Lease{Key: "old-repo", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, true /*dryRun*/, &stdout, &stderr)

	if lease := pool.ReadLease(dir); lease.Key != "old-repo" {
		t.Fatalf("--dry-run must not remove the expired lease, got %+v", lease)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("would remove expired lease")) {
		t.Errorf("--dry-run should still report the expired lease it would clean up, got:\n%s", stdout.String())
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
