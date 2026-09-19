package pool

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// These tests cover the other half of ResidueOwnerReleased: a `with` or
// `acquire` process reclaiming its OWN residue as it exits, identified by
// its pid against the slot's Meta.OwnerPID rather than by a lease key.
//
// The reported failure they come from: a node took its slot with `simpool
// acquire`, drove it with mav (which starts an idb_companion nothing
// reaps), and then called `simpool release`. An acquire holds a kernel
// flock and carries no lease and no key, so release matched nothing of its
// on any slot — it said "no active lease for key …", which is true and
// read as a failure, and it reclaimed nothing, which left the slot
// quarantined against every other caller for the full ResidueIdleGrace.
// Two symptoms, one gap: only a lease could declare its session over.
//
// Like the lease-path tests next door, every test here sets up the state
// where BOTH older forms of evidence correctly refuse — device Booted,
// LastUsed now — so anything that passes here is proving this path and not
// one of those.

// warmHolderMeta is the meta a slot carries the instant a `with`/`acquire`
// session ends: used seconds ago, device still booted, remembering the pid
// of the simpool process that held its flock.
func warmHolderMeta(udid, mode string, ownerPID int) Meta {
	return Meta{UDID: udid, Mode: mode, OwnerPID: ownerPID, LastUsed: time.Now()}
}

// ownFinishTestSlot builds a slot this test process can plausibly own: a
// real directory under a group name fakeSlotOwnDevice's device name is
// computed from, plus a Slot value pointing at it.
func ownFinishTestSlot(t *testing.T, udid, mode string, ownerPID int) *Slot {
	t.Helper()
	groupDir, dir := releaseTestSlot(t)
	if err := WriteMeta(dir, warmHolderMeta(udid, mode, ownerPID)); err != nil {
		t.Fatal(err)
	}
	return &Slot{
		Root:     testRoot,
		GroupDir: groupDir,
		Dir:      dir,
		Number:   testSlotN,
		Device:   testSlotDev,
		OSVer:    testSlotOSVer,
		Meta:     ReadMeta(dir),
	}
}

// TestReclaimOwnResidue_ReclaimsWhatItsOwnAcquireLeft is the positive case
// for the path `simpool release` structurally cannot reach.
func TestReclaimOwnResidue_ReclaimsWhatItsOwnAcquireLeft(t *testing.T) {
	udid := "simpool-test-ownfinish-acquire-reclaimed"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	slot := ownFinishTestSlot(t, udid, "acquire", os.Getpid())

	// The control that makes the rest mean something: as every OBSERVER of
	// this slot sees it, it is quarantined and unrecoverable, and it would
	// stay that way for ResidueIdleGrace.
	if got := CheckPoison(ReadMeta(slot.Dir)); got.Reason != PoisonedByLiveConsumers {
		t.Fatalf("setup does not reproduce the reported state: CheckPoison = %v, want PoisonedByLiveConsumers", got.Reason)
	}

	pids, ok := ReclaimOwnResidue(slot)
	if !ok {
		t.Fatal("the acquire that left this companion is exiting; it must reclaim it")
	}
	if len(pids) != 1 || pids[0] != pid {
		t.Fatalf("reclaimed pids = %v, want [%d]", pids, pid)
	}
	waitForDead(t, pid)
	if got := CheckPoison(ReadMeta(slot.Dir)); got.Poisoned() {
		t.Fatalf("slot must be free for every caller after the reclaim, still poisoned: %v", got)
	}
}

// TestReclaimOwnResidue_ReclaimsWhatItsOwnWithLeft is the same for `with`,
// whose process-group kill cannot reach a companion that has reparented
// away from the group.
func TestReclaimOwnResidue_ReclaimsWhatItsOwnWithLeft(t *testing.T) {
	udid := "simpool-test-ownfinish-with-reclaimed"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	slot := ownFinishTestSlot(t, udid, "with", os.Getpid())

	if _, ok := ReclaimOwnResidue(slot); !ok {
		t.Fatal("the `with` that left this companion is exiting; it must reclaim it")
	}
	waitForDead(t, pid)
}

// TestReclaimOwnResidue_RefusesForSomeoneElsesSlot is the control that
// keeps this from being a licence to kill companions on any warm slot:
// the declaration is "MY session is over", and it is checked against the
// pid the slot itself recorded, not taken on trust.
func TestReclaimOwnResidue_RefusesForSomeoneElsesSlot(t *testing.T) {
	udid := "simpool-test-ownfinish-other-holder"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	// A slot whose flock was held by some OTHER simpool process.
	slot := ownFinishTestSlot(t, udid, "acquire", os.Getpid()+1)

	if _, ok := ReclaimOwnResidue(slot); ok {
		t.Fatal("a process that never held this slot must not be able to kill what is attached to it")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestReclaimOwnResidue_RefusesForALeaseSlot mirrors the key form's own
// mode restriction from the other direction: a lease never holds the
// flock, so an exiting process proves nothing about a lease-mode slot —
// that slot's own key is the only thing that can speak for it.
func TestReclaimOwnResidue_RefusesForALeaseSlot(t *testing.T) {
	udid := "simpool-test-ownfinish-lease-mode"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	slot := ownFinishTestSlot(t, udid, "lease", os.Getpid())

	if _, ok := ReclaimOwnResidue(slot); ok {
		t.Fatal("an exiting process must not declare a lease session over")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestReclaimOwnResidue_RefusesWhileRealWorkIsAttached is THE control this
// whole change hangs on, and it is the one asked for by name before any of
// it could ship: a session finishing while something is genuinely still
// using that same slot — live work, not residue.
//
// The distinction is not a judgement call at runtime, which is what makes
// it testable at all: a companion is identified by an exact argv shape
// (isReclaimableResidue), and a single live process outside those classes
// collapses the whole set to PoisonedByLiveConsumers, which is never a kill
// candidate for anybody. So the mixed set below must be refused even
// though the declaring process is the slot's genuine owner and every other
// guard is satisfied — and, critically, the companion sitting next to the
// real work must survive too: a partial kill would be the worst outcome of
// the three.
func TestReclaimOwnResidue_RefusesWhileRealWorkIsAttached(t *testing.T) {
	udid := "simpool-test-ownfinish-live-work"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
	defer cleanupCompanion()
	// A real process carrying the UDID in its own argv and belonging to no
	// disposable class — an `axe` run, a human's `simctl`, an `idb` client
	// mid-call. Exactly what "live work" looks like to LiveConsumers.
	workPID, cleanupWork := spawnLiveConsumerWithToken(t, udid)
	defer cleanupWork()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	slot := ownFinishTestSlot(t, udid, "acquire", os.Getpid())

	if _, ok := ReclaimOwnResidue(slot); ok {
		t.Fatal("a slot with live work attached must never be reclaimed, even by its own holder")
	}
	if syscall.Kill(workPID, 0) != nil {
		t.Fatal("the live work must be untouched")
	}
	if syscall.Kill(companionPID, 0) != nil {
		t.Fatal("the companion beside live work must be untouched too: a partial kill is worse than no kill")
	}
}

// TestReclaimReleasedResidue_RefusesWhileRealWorkIsAttached is the same
// control for the lease path, which is the one a person actually types.
func TestReclaimReleasedResidue_RefusesWhileRealWorkIsAttached(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-release-live-work"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
	defer cleanupCompanion()
	workPID, cleanupWork := spawnLiveConsumerWithToken(t, udid)
	defer cleanupWork()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	if err := WriteMeta(dir, warmLeaseMeta(udid, "mav-key")); err != nil {
		t.Fatal(err)
	}

	if _, ok := ReclaimReleasedResidue(testRoot, dir, "mav-key"); ok {
		t.Fatal("a slot with live work attached must never be reclaimed, even by the key releasing it")
	}
	if syscall.Kill(workPID, 0) != nil {
		t.Fatal("the live work must be untouched")
	}
	if syscall.Kill(companionPID, 0) != nil {
		t.Fatal("the companion beside live work must be untouched too")
	}
}

// TestReclaimOwnResidue_RefusesForAForeignDevice keeps the identity guard
// every other reclaim path has: meta.UDID is advisory and can name one of
// the ~30 simulators a developer keeps for their own use, so a companion
// is only ever killed once the device is confirmed by name to be this
// exact slot's own.
func TestReclaimOwnResidue_RefusesForAForeignDevice(t *testing.T) {
	udid := "simpool-test-ownfinish-foreign-device"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	// Present and booted, but named for something that is not this slot.
	withDeviceBelongsToSlotFind(t, fakeSlotForeignDevice("Booted"))

	slot := ownFinishTestSlot(t, udid, "acquire", os.Getpid())

	if _, ok := ReclaimOwnResidue(slot); ok {
		t.Fatal("a device that is not verifiably this slot's own must never be acted on")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}
