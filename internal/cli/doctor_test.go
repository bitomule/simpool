package cli

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// TestRunDoctor_FlagsSlotWithLivePGIDEvenWithoutUDIDInArgv is the doctor-side
// counterpart to reap_test.go's
// TestReapSlot_RecoversVerifiedOrphanEvenWithoutUDIDInArgv: doctor.go uses
// the same pool.CheckPoison predicate (meta.ConsumerPGID via
// procs.PGIDAlive, falling back to procs.LiveConsumers's pgrep-based scan
// only when there's no recorded pgid) and must not silently diverge from
// it — though unlike reap, doctor never attempts recovery, only reports.
// A consumer that only ever receives its UDID by environment variable
// (MAV_TARGET_UDID, SIMPOOL_UDID_N — simpool's own handoff contract, design
// doc §5, and exactly how `mav run` receives it) must still be flagged by
// `simpool doctor`, not just missed.
//
// Like the reap test, the process below stands in for `simpool with`'s
// child: leader of its own process group (Setpgid, mirroring with.go),
// nothing in its command line a UDID-based pgrep could ever match.
func TestRunDoctor_FlagsSlotWithLivePGIDEvenWithoutUDIDInArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
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
	// being SIGKILLed — the kernel released its flock immediately, but the
	// child it launched, recorded by pgid and not by anything pgrep-able,
	// is still running.

	// Sanity check that this really is invisible to the old mechanism.
	if live, _ := procs.LiveConsumers(fakeUDID); len(live) != 0 {
		t.Fatalf("test setup broken: fakeUDID must not be visible to a pgrep-based check, got live=%v", live)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should report a problem for a slot whose consumer is alive only via ConsumerPGID, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("still alive")) {
		t.Fatalf("doctor should flag the live-but-invisible consumer, got:\n%s", stdout.String())
	}
}

// TestRunDoctor_FlagsLiveLeaseWithBusyFlock proves doctor catches the one
// invariant violation that must never happen given the lease/flock
// exclusion design (see lease.go's AcquireLease and slot.go's take()): a
// slot whose flock is currently held while it also carries a live lease
// for some key. Either path alone is fine; both together means two
// consumers may be sharing one simulator.
func TestRunDoctor_FlagsLiveLeaseWithBusyFlock(t *testing.T) {
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

	if err := pool.WriteLease(dir, pool.Lease{Key: "hot-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should report a problem for a busy slot with a live lease, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("hot-repo")) {
		t.Fatalf("doctor should name the conflicting lease key, got:\n%s", stdout.String())
	}
}

// TestRunDoctor_FlagsUnreadableLease proves doctor surfaces a lease.json it
// cannot read as a problem — read-only and diagnostic, so it never gates
// anything, but a check that didn't actually complete must not be silently
// dropped either (see pool.ReadLease's doc comment).
func TestRunDoctor_FlagsUnreadableLease(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory sitting where lease.json should be forces os.ReadFile to
	// fail deterministically, standing in for EMFILE/permission/I-O
	// failures in production.
	if err := os.MkdirAll(pool.LeasePath(dir), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should report a problem for an unreadable lease.json, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("lease.json")) {
		t.Fatalf("doctor should mention the unreadable lease.json, got:\n%s", stdout.String())
	}
}

// TestRunDoctor_CatchesMetaCoherentlyPointingAtAnotherGroupsDevice is the
// regression test for the self-referential identity check: slot-0 of group
// A ("iPhone 17 Pro"/26.3) carries a meta.json that is fully, internally
// coherent for group B ("iPhone 16"/26.0) — Device/OSVersion/UDID all
// consistent with each other — but the device that UDID actually names in
// the (faked) device set is B's own slot-0 simulator, not A's.
//
// Computing the expected name from meta.Device/meta.OSVersion (the old
// behavior) reproduces the same values meta.json already claims and always
// matches, so this exact corruption shape passed silently. The expected
// name must instead come from the slot's own directory (its group, on
// disk) — see pool.DeviceNameForGroup — which is what actually catches it.
//
// Ablation: reverting doctor.go's DeviceNameForGroup(root, group, n) back to
// DeviceName(root, meta.Device, meta.OSVersion, n) must turn this red (exit
// 0, no problem reported) — that is what proves the test exercises the real
// fix rather than some other coincidental signal.
func TestRunDoctor_CatchesMetaCoherentlyPointingAtAnotherGroupsDevice(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupADir := pool.GroupDir(home, "iPhone 17 Pro", "26.3")
	slotA := pool.SlotDir(groupADir, 0)
	if err := os.MkdirAll(slotA, 0o755); err != nil {
		t.Fatal(err)
	}
	groupBDir := pool.GroupDir(home, "iPhone 16", "26.0")
	slotB := pool.SlotDir(groupBDir, 0)
	if err := os.MkdirAll(slotB, 0o755); err != nil {
		t.Fatal(err)
	}

	const sharedUDID = "coherent-but-wrong-group-udid"
	if err := pool.WriteMeta(slotA, pool.Meta{
		Device:    "iPhone 16",
		OSVersion: "26.0",
		UDID:      sharedUDID,
	}); err != nil {
		t.Fatal(err)
	}

	bDeviceName := pool.DeviceName(home, "iPhone 16", "26.0", 0)

	orig := findDevice
	t.Cleanup(func() { findDevice = orig })
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		if udid != sharedUDID {
			return simctl.DeviceEntry{}, false, nil
		}
		return simctl.DeviceEntry{
			UDID:        sharedUDID,
			Name:        bDeviceName,
			State:       "Booted",
			IsAvailable: true,
		}, true, nil
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should flag slot-0 of group A pointing (coherently) at group B's device, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	wantLabel := "iPhone-17-Pro@26.3/slot-0"
	if !bytes.Contains(stdout.Bytes(), []byte(wantLabel)) {
		t.Fatalf("doctor should name %s (the slot whose meta.json is wrong), got:\n%s", wantLabel, stdout.String())
	}
}
