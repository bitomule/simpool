package pool

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These tests cover ResidueOwnerReleased: the evidence form that exists
// because the reported failure produces neither of the other two.
//
// The reported shape, reproduced end to end on the real iPad slot before
// any of this was written: a complete, successful `mav run`, then `simpool
// release --key`, then `xcrun simctl shutdown`. `idb` leaves its companion
// attached to the device and nothing reaps it, so the slot sits in
// `quarantined` — reason "orphaned idb_companion / simctl log-stream
// process(es) attached to a non-running device" — until something happens
// to try to acquire that exact slot, a `simpool reap` pass arrives, or a
// person reads the pid out of `simpool status` and kills it.
//
// At the moment of the release the device is still Booted and
// Meta.LastUsed is seconds old, so residueDeviceOffline and
// residueSlotLongIdle both correctly refuse, and CheckPoison can only
// reach PoisonedByLiveConsumers — never a kill candidate. That is the
// state every test below sets up: fakeDeviceList(udid, "Booted") plus a
// LastUsed of now. Any test here that passed with the device Shutdown or
// the slot long idle would be proving one of the OLD evidence paths, not
// this one.

// releaseTestSlot builds a real slot directory whose group name is the one
// fakeSlotOwnDevice's device name is computed from, so deviceBelongsToSlot
// can be satisfied through the seam without a real simulator.
func releaseTestSlot(t *testing.T) (groupDir, dir string) {
	t.Helper()
	groupDir = filepath.Join(t.TempDir(), GroupName(testSlotDev, testSlotOSVer))
	dir = SlotDir(groupDir, testSlotN)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return groupDir, dir
}

// warmLeaseMeta is the meta a slot carries the instant a successful lease
// session ends: used seconds ago, device still booted, remembering the key
// that held it.
func warmLeaseMeta(udid, key string) Meta {
	return Meta{UDID: udid, Mode: "lease", LeaseKey: key, LastUsed: time.Now()}
}

// TestReclaimReleasedResidue_ReclaimsTheCompanionItsOwnLeaseLeft is the
// positive case, and the whole point: a warm slot, a companion left by key
// K, K releasing. Nothing else in the codebase can act here — the
// negative-control tests below are the same setup with one input changed
// and must all refuse.
func TestReclaimReleasedResidue_ReclaimsTheCompanionItsOwnLeaseLeft(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-reclaimed"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	if err := WriteMeta(dir, warmLeaseMeta(udid, "mav-key")); err != nil {
		t.Fatal(err)
	}

	// The control that makes the rest mean something: as every OBSERVER of
	// this slot sees it right now, it is quarantined and unrecoverable.
	if got := CheckPoison(ReadMeta(dir)); got.Reason != PoisonedByLiveConsumers {
		t.Fatalf("setup does not reproduce the reported state: CheckPoison = %v, want PoisonedByLiveConsumers", got.Reason)
	}

	pids, ok := ReclaimReleasedResidue(testRoot, dir, "mav-key")
	if !ok {
		t.Fatal("the key whose own lease left this companion released the slot; the companion must be reclaimed")
	}
	if len(pids) != 1 || pids[0] != pid {
		t.Fatalf("reclaimed pids = %v, want [%d]", pids, pid)
	}
	waitForDead(t, pid)
	if got := CheckPoison(ReadMeta(dir)); got.Poisoned() {
		t.Fatalf("slot must be free for every caller after the reclaim, still poisoned: %v", got)
	}
}

// TestReclaimReleasedResidue_RefusesForAnotherKey is the control that keeps
// this from being a general licence to kill companions on a warm slot: the
// evidence is "the session that left this is over", and only the key that
// held the lease can say so.
func TestReclaimReleasedResidue_RefusesForAnotherKey(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-other-key"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	if err := WriteMeta(dir, warmLeaseMeta(udid, "mav-key")); err != nil {
		t.Fatal(err)
	}

	if _, ok := ReclaimReleasedResidue(testRoot, dir, "some-other-key"); ok {
		t.Fatal("a key that never held this slot must not be able to kill what is attached to it")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestReclaimReleasedResidue_RefusesWhenTheSlotWasNotHeldByALease mirrors
// ownLeaseResidue's own Mode restriction: residue on a slot last held by
// `with`/`acquire` means something that held the flock died, which is the
// ConsumerPGID branch's business.
func TestReclaimReleasedResidue_RefusesWhenTheSlotWasNotHeldByALease(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-with-mode"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := warmLeaseMeta(udid, "mav-key")
	meta.Mode = "with"
	if err := WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	if _, ok := ReclaimReleasedResidue(testRoot, dir, "mav-key"); ok {
		t.Fatal("a slot last held by `with` must not be reclaimable through the release path")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestReclaimReleasedResidue_RefusesWhileALeaseIsAliveOnTheSlot covers the
// one real concurrency hazard: stickiness is per KEY, not per process, so
// two callers can share a key. If one of them has written a fresh lease
// since the release, its session is in progress and its companion is
// infrastructure, not residue.
func TestReclaimReleasedResidue_RefusesWhileALeaseIsAliveOnTheSlot(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-live-lease"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	if err := WriteMeta(dir, warmLeaseMeta(udid, "mav-key")); err != nil {
		t.Fatal(err)
	}
	if err := WriteLease(dir, Lease{Key: "mav-key", ExpiresAt: time.Now().Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	if _, ok := ReclaimReleasedResidue(testRoot, dir, "mav-key"); ok {
		t.Fatal("a same-key session that re-leased the slot since the release must not have its companion killed")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestReclaimReleasedResidue_RefusesWhenADeviceIsNotVerifiablyThisSlots
// pins that this path does not weaken the identity guard every other kill
// decision in this file passes: meta.UDID is advisory and may name a
// developer's own simulator.
func TestReclaimReleasedResidue_RefusesWhenADeviceIsNotVerifiablyThisSlots(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-foreign-device"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotForeignDevice("Booted"))

	if err := WriteMeta(dir, warmLeaseMeta(udid, "mav-key")); err != nil {
		t.Fatal(err)
	}

	if _, ok := ReclaimReleasedResidue(testRoot, dir, "mav-key"); ok {
		t.Fatal("a UDID that is not provably this slot's own device must never have processes killed for it")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion must still be alive")
	}
}

// TestCheckPoison_NeverProducesOwnerReleasedEvidence is the containment
// test: ResidueOwnerReleased must be unreachable from the predicate every
// observer uses, or `status`, `doctor`, `reap` and another caller's
// acquisition would each start treating a warm slot's companion as
// reclaimable on the strength of a key none of them is speaking for.
func TestCheckPoison_NeverProducesOwnerReleasedEvidence(t *testing.T) {
	_, dir := releaseTestSlot(t)
	udid := "simpool-test-released-residue-containment"
	_, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := warmLeaseMeta(udid, "mav-key")
	if err := WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	got := CheckPoison(meta)
	if got.Reason != PoisonedByLiveConsumers {
		t.Fatalf("CheckPoison = %v (evidence %v), want PoisonedByLiveConsumers: no observer may reach owner-released evidence", got.Reason, got.ResidueEvidence)
	}
}

// TestOwnerDeclaration_NeverMatchesOnZeroValues pins the one way each
// form of attribution can silently become a wildcard: a slot with no
// recorded lease key and a caller releasing no key would otherwise match
// each other, and so would a slot with no recorded owner pid and a
// declaration carrying none.
func TestOwnerDeclaration_NeverMatchesOnZeroValues(t *testing.T) {
	if (ownerDeclaration{}).declares(Meta{Mode: "lease"}) {
		t.Fatal("an empty key must never match an empty Meta.LeaseKey")
	}
	if (ownerDeclaration{}).declares(Meta{Mode: "lease", LeaseKey: "k"}) {
		t.Fatal("an empty releasing key must never match")
	}
	if (ownerDeclaration{releasedKey: "k"}).declares(Meta{Mode: "lease"}) {
		t.Fatal("a slot that remembers no lease key must never match")
	}
	if (ownerDeclaration{}).declares(Meta{Mode: "acquire"}) {
		t.Fatal("a zero pid must never match an unrecorded Meta.OwnerPID")
	}
	if (ownerDeclaration{finishingPID: 4242}).declares(Meta{Mode: "acquire"}) {
		t.Fatal("a slot that recorded no owner pid must never match")
	}
	if (ownerDeclaration{}).declares(Meta{Mode: "acquire", OwnerPID: 4242}) {
		t.Fatal("a declaration carrying no pid must never match")
	}
}

// TestOwnerDeclaration_EachFormIsScopedToItsOwnModes pins the crossover
// neither form may make. A key speaks only for a lease (a `with`/`acquire`
// slot's residue means something that held the flock died, which is the
// ConsumerPGID branch's business); a pid speaks only for a flock holder (a
// lease never holds the flock, so an exiting lease process proves nothing
// about the slot). Letting either cross would let a caller declare a
// session over that it never owned.
func TestOwnerDeclaration_EachFormIsScopedToItsOwnModes(t *testing.T) {
	key := ownerDeclaration{releasedKey: "k"}
	for _, mode := range []string{"with", "acquire"} {
		if key.declares(Meta{Mode: mode, LeaseKey: "k"}) {
			t.Errorf("a lease key must not declare for a %q slot", mode)
		}
	}
	pid := ownerDeclaration{finishingPID: 99}
	if pid.declares(Meta{Mode: "lease", OwnerPID: 99}) {
		t.Error("a finishing pid must not declare for a lease slot")
	}
	if !key.declares(Meta{Mode: "lease", LeaseKey: "k"}) {
		t.Error("a matching key must declare for its own lease slot")
	}
	for _, mode := range []string{"with", "acquire"} {
		if !pid.declares(Meta{Mode: mode, OwnerPID: 99}) {
			t.Errorf("a matching pid must declare for its own %q slot", mode)
		}
	}
	if pid.declares(Meta{Mode: "acquire", OwnerPID: 100}) {
		t.Error("a different pid must never declare for someone else's slot")
	}
}
