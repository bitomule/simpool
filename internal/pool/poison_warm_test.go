package pool

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// --- Orphaned idb_companion on a WARM (still Booted) pool slot ---
//
// The production state these cover, measured on a real machine on
// 2026-08-20: every one of seven pool slots FAILing `simpool doctor`
// identically, each poisoned by exactly one orphaned `idb_companion --udid
// <slot's device> --grpc-domain-sock /tmp/idb/<udid>_companion.sock`, all
// reparented to launchd, all alive for over a day — and NO path reclaiming
// them. `reap` skipped all seven, and `reap --disown-poisoned` — the escape
// hatch its own help text describes for exactly this case — rejected all
// seven too.
//
// The cause was neither the identity guard nor the ConsumerPGID
// fingerprint: the ONLY evidence the companion branch accepted was "the
// target device is not running", and a warm pool slot's device is Booted by
// construction — that is what a warm pool IS. So the branch was
// structurally unreachable for the exact scenario it was written for, and
// every slot fell through to the never-a-kill-candidate
// PoisonedByLiveConsumers. See ResidueSlotLongIdle.
//
// Deliberately NOT fixed by looking at the companion process itself: a
// companion is spawned by the short-lived `idb` client under
// preexec_fn=os.setpgrp (read directly out of idb's own
// companion_spawner.py) and cached in /tmp/idb/state for later reuse, so
// EVERY companion — healthy or orphaned — ends up reparented to init within
// seconds of being spawned. PPID, age and socket ownership therefore cannot
// tell a live one from residue. The slot's own state can.

// longIdleMeta returns a Meta for a warm slot last used well beyond
// ResidueIdleGrace — the shape the seven production orphans had, at over
// a day of idleness each.
func longIdleMeta(udid, mode string) Meta {
	return Meta{UDID: udid, Mode: mode, LastUsed: time.Now().Add(-24 * time.Hour)}
}

// TestCheckPoison_CompanionOnWarmBootedSlot_ReclaimedOnceLongIdle is the
// primary regression test for the production incident above: a companion
// pinned to a device that is still Booted, on a slot whose flock is free
// (guaranteed by every caller of AttemptRecovery — see that function's
// documented precondition), unused for far longer than ResidueIdleGrace,
// and independently confirmed by name to be this exact slot's own device,
// must be reclaimed automatically — regardless of Meta.Mode, since
// idb_companion residue has nothing to do with which subcommand held the
// slot.
//
// Before the fix this failed on its very first assertion: CheckPoison
// returned PoisonedByLiveConsumers for every one of these modes.
func TestCheckPoison_CompanionOnWarmBootedSlot_ReclaimedOnceLongIdle(t *testing.T) {
	for _, mode := range []string{"lease", "acquire", "with", ""} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			udid := "simpool-test-companion-warm-booted-" + mode
			pid, cleanup := spawnIdbCompanion(t, udid)
			defer cleanup()
			waitForLiveConsumer(t, udid)
			withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
			withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

			meta := longIdleMeta(udid, mode)
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByOrphanedResidue {
				t.Fatalf("mode=%q: expected PoisonedByOrphanedResidue for a long-idle warm slot's companion, got %v", mode, poison.Reason)
			}
			if poison.ResidueEvidence != ResidueSlotLongIdle {
				t.Fatalf("mode=%q: expected ResidueSlotLongIdle evidence (the device is up), got %v", mode, poison.ResidueEvidence)
			}
			if len(poison.ResiduePIDs) != 1 || poison.ResiduePIDs[0] != pid {
				t.Fatalf("mode=%q: expected ResiduePIDs=[%d], got %v", mode, pid, poison.ResiduePIDs)
			}
			if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatalf("mode=%q: AttemptRecovery should have reclaimed the orphaned companion on a long-idle warm slot", mode)
			}
			waitForDead(t, pid)
			if meta.UDID != udid {
				t.Errorf("mode=%q: the warm device must be kept, not forgotten — it is what the next consumer boots into; got %+v", mode, meta)
			}
		})
	}
}

// TestCheckPoison_CompanionOnWarmBootedSlot_NeverReclaimedWithinGrace is
// the safety half of the same fix, and the specific negative case the
// production investigation demanded be spared: a companion belonging to a
// session that is merely quiet.
//
// A MAV hot loop stamps Meta.LastUsed on every single call (EnsureProvisioned
// — see provision.go), so a recently-used slot means a session that is
// alive even if its 3-minute lease has lapsed between two calls while a
// long `mav run` build is in flight. Killing that companion would be
// survivable on its own (idb respawns it), but freeing the slot on the
// strength of it would hand a running session's simulator to someone else.
// Within ResidueIdleGrace this must stay the ordinary,
// never-a-kill-candidate PoisonedByLiveConsumers.
func TestCheckPoison_CompanionOnWarmBootedSlot_NeverReclaimedWithinGrace(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-warm-recent"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	// Deliberately just inside the grace, not comfortably inside it: this
	// is the boundary that decides whether a live-but-quiet MAV session
	// keeps its slot.
	meta := Meta{UDID: udid, Mode: "lease", LastUsed: time.Now().Add(-ResidueIdleGrace + time.Minute)}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers for a companion on a slot used %s ago, got %v", time.Since(meta.LastUsed).Round(time.Second), poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a companion on a slot still within ResidueIdleGrace — that is a session that may merely be quiet")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}

// TestCheckPoison_CompanionOnWarmBootedSlot_ZeroLastUsedNeverReclaimed pins
// the fail-safe reading of a missing timestamp: a zero Meta.LastUsed is an
// inability to check how long this slot has been idle, and this codebase
// never reads a check that could not complete as "confirmed free" (see
// PoisonedByCheckFailure). It must NOT be read as "infinitely idle,
// therefore reclaimable", which is how reap's own --cold accounting treats
// it — that accounting only ever shuts a device down, this one kills a
// process.
func TestCheckPoison_CompanionOnWarmBootedSlot_ZeroLastUsedNeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-warm-nolastused"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := Meta{UDID: udid, Mode: "lease"} // LastUsed deliberately zero
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers for a slot with no recorded LastUsed, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never treat an unrecorded LastUsed as proof of idleness")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}

// TestCheckPoison_CompanionOnRunningForeignDevice_NeverTouchedByEitherPath
// is the "someone else's live simulator" regression test for the new
// evidence. meta.UDID is advisory and can be stale or outright wrong, so a
// long-idle slot pointing at a RUNNING device that is not this slot's own —
// a developer's own iPhone simulator they are driving with `idb` by hand —
// must be immune to both reclaim paths:
//
//   - AttemptRecovery refuses because deviceBelongsToSlot cannot name-match
//     it (an unchanged guard, now load-bearing for this case too).
//   - DisownPoisonedSlot refuses because it deliberately SKIPS
//     deviceBelongsToSlot, which leaves a confirmed-not-running device as
//     its only remaining guard — and this device is up. Without this gate,
//     `--disown-poisoned` would SIGKILL the companion of a simulator simpool
//     was never asked to touch.
func TestCheckPoison_CompanionOnRunningForeignDevice_NeverTouchedByEitherPath(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-warm-foreign"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotForeignDevice("Booted"))

	meta := longIdleMeta(udid, "lease")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedResidue || poison.ResidueEvidence != ResidueSlotLongIdle {
		t.Fatalf("expected PoisonedByOrphanedResidue/ResidueSlotLongIdle (classification is identity-blind by design), got %v/%v", poison.Reason, poison.ResidueEvidence)
	}
	if poison.ResidueDisownable() {
		t.Fatal("a companion on a RUNNING device must never be reported as disownable")
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must refuse a companion whose running target could not be name-matched to this slot")
	}
	err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison)
	if !errors.Is(err, ErrNotDisownable) {
		t.Fatalf("--disown-poisoned must refuse a companion attached to a RUNNING device outright, got %v", err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the foreign device's companion must still be alive — neither path may touch it")
	}
	if meta.UDID != udid {
		t.Errorf("nothing may be forgotten for a refused slot, got %+v", meta)
	}
}

// TestCheckPoison_NonCompanionAlongsideCompanionOnWarmSlot_NeverReclaimed
// reproduces the one production slot of the seven that carried MORE than a
// companion: an orphaned `simctl spawn <udid> ...` (pid 52024, reparented
// to launchd, the same age as the companion beside it). A slot is only ever
// narrowly explained by companion residue when EVERY live consumer is a
// verified companion; one generic process alongside them means the slot
// stays quarantined, companion and all. That slot is expected to keep
// FAILing `doctor` after this fix, and correctly so.
func TestCheckPoison_NonCompanionAlongsideCompanionOnWarmSlot_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-warm-mixed"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
	defer cleanupCompanion()
	otherPID, cleanupOther := spawnLiveConsumerWithToken(t, udid)
	defer cleanupOther()
	waitForConsumerCount(t, udid, 2)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := longIdleMeta(udid, "lease")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("a non-companion consumer alongside the companion must fall back to PoisonedByLiveConsumers, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a slot that is not explained by companion residue alone")
	}
	if syscall.Kill(companionPID, 0) != nil || syscall.Kill(otherPID, 0) != nil {
		t.Fatal("both processes must still be alive")
	}
}

// TestReclaimOrphanedCompanions_RefusesWhenANewConsumerAppears proves the
// re-verification actually re-reads the LIVE CONSUMER SET rather than just
// re-checking the PIDs it was handed. The window it closes is real: `reap`
// runs several `xcrun simctl` invocations between CheckPoison and this
// call, and the thing most worth catching in that window is not a companion
// that changed identity but a NON-companion that appeared — the `idb`
// client process of a hot-loop call that arrived just now, carrying the
// UDID in its own argv. Re-checking only poison.ResiduePIDs would be
// blind to it and would reclaim the slot anyway.
func TestReclaimOrphanedCompanions_RefusesWhenANewConsumerAppears(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-warm-newconsumer"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
	defer cleanupCompanion()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := longIdleMeta(udid, "lease")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedResidue {
		t.Fatalf("setup: expected PoisonedByOrphanedResidue, got %v", poison.Reason)
	}

	// The window opens here: a live client arrives after the determination
	// was made but before anything acts on it.
	otherPID, cleanupOther := spawnLiveConsumerWithToken(t, udid)
	defer cleanupOther()
	waitForConsumerCount(t, udid, 2)

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must re-read the live consumer set immediately before killing and refuse once a non-companion consumer has appeared")
	}
	if syscall.Kill(companionPID, 0) != nil || syscall.Kill(otherPID, 0) != nil {
		t.Fatal("both processes must still be alive after the refused attempt")
	}
}

func waitForConsumerCount(t *testing.T, udid string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		live, _ := procs.LiveConsumers(udid)
		if len(live) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least %d live consumers for %s, saw %d", want, udid, len(live))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
