package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	reapSlot(root, dir, 0 /*n*/, 0 /*coldMinutes*/, 1 /*purgeMinutes*/, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

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
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

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
	reapSlot(root, dir, 0, 0, 0 /*purgeMinutes disabled*/, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

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
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, true /*dryRun*/, false, &stdout, &stderr)

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("--dry-run must not remove the slot directory, stat err = %v", err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("PURGE")) {
		t.Errorf("--dry-run should still report what it would purge, got:\n%s", stdout.String())
	}
}

// TestReapSlot_RecoversVerifiedOrphanEvenWithoutUDIDInArgv is the
// regression test for the CRITICAL finding that reap's poisoned-slot check
// relied entirely on `pgrep -f <udid>` (procs.LiveConsumers), which is
// blind to a consumer that only ever receives the UDID by environment
// variable (MAV_TARGET_UDID, SIMPOOL_UDID_N — exactly simpool's own
// handoff contract, design doc §5, and exactly how `mav run` receives it)
// rather than anywhere in its own argv — combined with the newer contract
// that reap doesn't just detect this and report SKIP forever, it verifies
// the consumer's recorded identity (start time, fingerprinted under a
// fixed, locale/timezone-independent environment — see
// procs.ProcessStartTime) and, on an exact match, reclaims the slot: kills
// the process, shuts down the simulator.
//
// This reproduces the real failure surface, not the sanitized proxy an
// earlier version of this suite used (`xcrun simctl spawn <udid> log
// stream`, which — unlike a real MAV consumer — does put the UDID in its
// own argv and so was never blind to the old pgrep-based check in the
// first place). The process below stands in for `simpool with`'s child
// (and the `mav run` it in turn execs): it is the leader of its own
// process group, exactly like with.go's Setpgid child, and has nothing in
// its command line a UDID-based pgrep could ever match.
func TestReapSlot_RecoversVerifiedOrphanEvenWithoutUDIDInArgv(t *testing.T) {
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
	// Reap it the instant it exits (by any means): the test binary is its
	// real parent here, and an un-Wait()'d zombie still answers
	// kill(-pgid, 0) with EPERM (not ESRCH) on Darwin, which PGIDAlive
	// correctly reads as "still alive" — making a successful recovery look
	// like a failed one. Not an issue in real usage — `simpool with` is
	// already dead by the time this scenario happens, so the orphan
	// reparents to launchd, which reaps it immediately once killed.
	go func() { _ = orphan.Wait() }()

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("capturing orphan's start time: %v", err)
	}

	fakeUDID := "simpool-test-udid-not-in-any-argv"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:              fakeUDID,
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	// Slot-0's lock starts free (never taken): simulates `simpool` itself
	// being SIGKILLed (design doc §4's one accepted failure window) — the
	// kernel released its flock immediately, but the child it launched,
	// recorded by pgid and not by anything pgrep-able, is still running.

	// Sanity check that this really is invisible to the old, pgrep-only
	// mechanism: if this ever starts matching, the test stops exercising
	// the bug ConsumerPGID exists to catch.
	if live, _ := procs.LiveConsumers(fakeUDID); len(live) != 0 {
		t.Fatalf("test setup broken: fakeUDID must not be visible to a pgrep-based check, got live=%v", live)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0 /*n*/, 0 /*coldMinutes*/, 0 /*purgeMinutes*/, time.Hour, 3*time.Minute, false /*dryRun*/, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("RECOVER")) {
		t.Fatalf("reap should RECOVER a slot whose consumer is alive only via a verified ConsumerPGID (no UDID anywhere in argv), got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if procs.PGIDAlive(pgid) {
		t.Fatal("reap's recovery should have killed the verified orphan process group")
	}
	if got := pool.ReadMeta(dir).ConsumerPGID; got != 0 {
		t.Errorf("ConsumerPGID should be cleared on disk after recovery, got %d", got)
	}
}

// TestReapSlot_SkipsOrphanWithUnverifiableIdentity proves reap still
// refuses to kill anything when the recorded consumer fingerprint is
// missing (meta.json predating ConsumerStartedAt, or otherwise never
// captured) — macOS recycles pids, so a live process under a recorded pgid
// is not by itself proof it is the same process reap thinks it is. This
// preserves the safety property the original version of this test (before
// recovery existed) was built around, and is the mandated "identidad no
// verificable -> cuarentena" regression test at the CLI layer.
func TestReapSlot_SkipsOrphanWithUnverifiableIdentity(t *testing.T) {
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

	fakeUDID := "simpool-test-udid-unverifiable"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:         fakeUDID,
		Mode:         "with",
		ConsumerPGID: pgid,
		// Deliberately no ConsumerStartedAt.
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Fatalf("reap should SKIP a slot it cannot verify the identity of, got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !procs.PGIDAlive(pgid) {
		t.Fatal("an unverifiable orphan must never be killed")
	}
}

// TestReapSlot_DisownPoisonedFreesAnUnverifiableSlotWithoutKilling is the
// end-to-end regression test for the "no way out of a permanent
// quarantine" finding: a `with` slot whose identity can never be verified
// (here: no fingerprint was ever recorded, mirroring a meta.json predating
// the feature, or macOS having recycled the pid) stays SKIPped by ordinary
// `reap` forever — --disown-poisoned is the operator's explicit way out. It
// must reclaim the slot (meta forgotten) WITHOUT ever signaling the process
// that poisoned it.
func TestReapSlot_DisownPoisonedFreesAnUnverifiableSlotWithoutKilling(t *testing.T) {
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

	fakeUDID := "simpool-test-udid-disown-cli"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:         fakeUDID,
		Mode:         "with",
		ConsumerPGID: pgid,
		// Deliberately no ConsumerStartedAt: unverifiable, exactly the
		// permanent-quarantine case --disown-poisoned exists for.
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, false /*dryRun*/, true /*disownPoisoned*/, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("DISOWN")) {
		t.Fatalf("reap --disown-poisoned should report reclaiming the slot, got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !procs.PGIDAlive(pgid) {
		t.Fatal("--disown-poisoned must never signal the process it disowns — it should still be alive")
	}

	persisted := pool.ReadMeta(dir)
	if persisted.ConsumerPGID != 0 || persisted.UDID != "" || persisted.Mode != "" {
		t.Fatalf("expected the slot's identity to be fully forgotten, got %+v", persisted)
	}
}

// TestReapSlot_DisownPoisonedDryRunNeverMutates proves --dry-run still
// applies to --disown-poisoned: it may report what it would do, but must
// never actually forget the slot's identity or touch anything.
func TestReapSlot_DisownPoisonedDryRunNeverMutates(t *testing.T) {
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

	fakeUDID := "simpool-test-udid-disown-dry-run"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:         fakeUDID,
		Mode:         "with",
		ConsumerPGID: pgid,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, true /*dryRun*/, true /*disownPoisoned*/, &stdout, &stderr)

	persisted := pool.ReadMeta(dir)
	if persisted.ConsumerPGID != pgid || persisted.UDID != fakeUDID {
		t.Fatalf("--dry-run must never mutate meta, got %+v", persisted)
	}
	if !procs.PGIDAlive(pgid) {
		t.Fatal("process must still be alive")
	}
}

// TestReapSlot_DisownPoisonedNeverTouchesLiveLeaseConsumer proves
// --disown-poisoned respects the exact same restraint as AttemptRecovery: a
// live process referencing a (TTL-expired) lease's UDID on its own command
// line is the healthy case, not an orphan, and must never be disowned out
// from under it even when the caller explicitly asked for
// --disown-poisoned.
func TestReapSlot_DisownPoisonedNeverTouchesLiveLeaseConsumer(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The lease itself has already expired (reap must be allowed past the
	// lease check), but a live process still references the device — the
	// realistic "hot loop went idle for a bit" case, not an orphan.
	if err := pool.WriteLease(dir, pool.Lease{Key: "repo-a", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	token := "simpool-test-udid-disown-live-lease"
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "live_consumer.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	consumer := exec.Command(scriptPath, token)
	if err := consumer.Start(); err != nil {
		t.Fatal(err)
	}
	pid := consumer.Process.Pid
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		live, _ := procs.LiveConsumers(token)
		if len(live) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live consumer process never became visible")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := pool.WriteMeta(dir, pool.Meta{UDID: token, Mode: "lease"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, false, true /*disownPoisoned*/, &stdout, &stderr)

	if bytes.Contains(stdout.Bytes(), []byte("DISOWN")) {
		t.Fatalf("--disown-poisoned must never act on a live lease consumer, got:\n%s", stdout.String())
	}
	persisted := pool.ReadMeta(dir)
	if persisted.UDID != token {
		t.Fatalf("meta must be untouched, got %+v", persisted)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the live consumer process must never be touched")
	}
}

// TestReapSlot_RecoveryNeverFallsThroughToSamePassPurge is the regression
// test for the HIGH finding that reapSlot, on a successful recovery, used
// to fall through into the same pass's idle/cold/--purge accounting instead
// of returning immediately. AttemptRecovery's simctl.Shutdown call is
// measured SYNCHRONOUS (5-7.5s wall time to return the device's own
// reported state, not an async call that returns before the device is
// really down), but CoreSimulator's own teardown of the device's
// underlying process tree can still lag a beat behind that state flip; the
// reverted version of this feature called simctl.Delete on exactly that
// window, which is the documented catastrophic failure mode (see
// integration_test.go's cleanupPool and the README): deleting a simulator
// before its process tree finishes tearing down orphans hundreds of
// runtime processes. Uses a fake, non-simctl-backed UDID and an already
// --purge-eligible LastUsed so a regression (falling through) would be
// visible even without a real simulator: it would print IDLE/SHUT/COLD or
// attempt PURGE for the (nonexistent) device right after RECOVER.
func TestReapSlot_RecoveryNeverFallsThroughToSamePassPurge(t *testing.T) {
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
	go func() { _ = orphan.Wait() }() // see the EPERM-vs-zombie comment above

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("capturing orphan's start time: %v", err)
	}

	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:              "simpool-test-udid-no-fallthrough",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		// Already far past any --cold/--purge threshold, so a regression
		// (falling through to the idle/purge accounting below RECOVER)
		// would visibly act on it in this very same reapSlot call.
		LastUsed: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// --purge 1 (minute): eligible immediately given the backdated LastUsed
	// above, if reapSlot were to (incorrectly) fall through to that
	// accounting in this same pass.
	reapSlot(root, dir, 0, 0 /*coldMinutes*/, 1 /*purgeMinutes*/, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

	out := stdout.String()
	if !strings.Contains(out, "RECOVER") {
		t.Fatalf("expected RECOVER, got:\n%s", out)
	}
	for _, forbidden := range []string{"PURGE", "SHUT", "COLD", "IDLE", "KEEP"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("reapSlot must return immediately after RECOVER, not fall through to this pass's idle/cold/purge accounting — got %s in output:\n%s", forbidden, out)
		}
	}
	if procs.PGIDAlive(pgid) {
		t.Fatal("the orphan should have been killed by recovery")
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
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Fatalf("reap should SKIP a slot with an active lease, got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("slot directory with an active lease must survive reap, stat err = %v", err)
	}
	lease, _ := pool.ReadLease(dir)
	if lease.Key != "hot-repo" {
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
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

	lease, _ := pool.ReadLease(dir)
	if lease.Key != "" {
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
	reapSlot(root, dir, 0, 0, 0, time.Hour, 3*time.Minute, true /*dryRun*/, false, &stdout, &stderr)

	lease, _ := pool.ReadLease(dir)
	if lease.Key != "old-repo" {
		t.Fatalf("--dry-run must not remove the expired lease, got %+v", lease)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("would remove expired lease")) {
		t.Errorf("--dry-run should still report the expired lease it would clean up, got:\n%s", stdout.String())
	}
}

// TestReapSlot_UnreadableLeaseIsTreatedAsBusy is the regression test for
// the "can't verify a lease means busy, not free" fix: reap must never
// treat an unreadable lease.json as "no lease" and fall through to its
// idle/poison/--cold/--purge accounting. purgeMinutes and coldMinutes are
// both wide open here (the slot is old and never-provisioned) — the only
// thing standing between reap and deleting this slot's directory is that
// unreadable lease.json, so a regression here would actually purge it.
func TestReapSlot_UnreadableLeaseIsTreatedAsBusy(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	makeAbandonedSlotDir(t, dir, 2*deadSlotGrace)
	// A directory sitting where lease.json should be forces os.ReadFile to
	// fail deterministically, standing in for EMFILE/permission/I-O
	// failures in production.
	if err := os.MkdirAll(pool.LeasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 0, 1, time.Hour, 3*time.Minute, false, false, &stdout, &stderr)

	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Fatalf("reap should SKIP a slot whose lease.json cannot be read, got:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a slot with an unreadable lease.json must survive reap untouched, stat err = %v", err)
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
