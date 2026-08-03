package pool

import (
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// SweepIdleThreshold is how long a free slot must have sat idle (since
// LastUsed) before ExitSweep shuts down its simulator. Matches the
// most-recently-used-first reuse heuristic AcquireSlots/AcquireLease rely
// on — kept generous so a hot pool isn't shut down out from under the very
// reuse it exists to enable. This is what actually closes the memory loop
// (design doc §3: ~1.75GB resident per booted slot): three slots left
// booted for days is exactly the scenario that leads to jetsam and SIGKILL
// in the first place.
const SweepIdleThreshold = 10 * time.Minute

// ExitSweep recovers verified orphans and shuts down long-idle simulators
// across every slot in one device+OS group. Called at group-scoped,
// effectively-free moments — the end of `simpool with` (after its own
// slots are already released) and `simpool release` — so the pool tends
// toward cleaning itself up continuously instead of relying entirely on a
// human or cron running `simpool reap`.
func ExitSweep(root, device, osVersion string) {
	ExitSweepGroupDir(root, GroupDir(root, device, osVersion))
}

// ExitSweepGroupDir is ExitSweep for callers that already have a group
// directory (e.g. RunRelease, which learns it from ReleaseLease's return
// value rather than from a device+OS pair).
func ExitSweepGroupDir(root, groupDir string) {
	for _, n := range ListSlotNumbers(groupDir) {
		sweepSlot(root, groupDir, SlotDir(groupDir, n), n)
	}
}

// sweepSlot only ever acts on a slot whose flock it can take non-blocking
// (TryLock): a slot currently in active use by someone else — or reserved
// by a live lease, which deliberately never holds the flock — is left
// completely alone.
func sweepSlot(root, groupDir, dir string, n int) {
	if lease := ReadLease(dir); lease.Alive() {
		return
	}

	lock, err := TryLock(lockPath(dir))
	if err != nil {
		return // busy, or an unexpected error — either way, leave it alone
	}
	defer lock.Release()

	meta := ReadMeta(dir)
	if poison := CheckPoison(meta); poison.Poisoned() {
		// Best-effort: an unverifiable identity (or a kill that doesn't
		// stick within recoveryPostKillWait) just means this pass leaves
		// the slot quarantined, exactly as before this feature existed.
		AttemptRecovery(dir, &meta, poison)
		return
	}

	if meta.UDID == "" || meta.LastUsed.IsZero() {
		return
	}
	if time.Since(meta.LastUsed) < SweepIdleThreshold {
		return
	}

	entry, found, err := simctl.Find(meta.UDID)
	if err != nil || !found || entry.State != "Booted" {
		return
	}
	// Mirrors reap's cross-slot-device guard: meta.json pointing at a real,
	// existing device that isn't the one this exact slot owns should never
	// happen, but this is one of the few places with the power to shut a
	// simulator down, so it refuses outright rather than trust meta.json's
	// UDID at face value.
	if want := DeviceName(root, meta.Device, meta.OSVersion, n); entry.Name != want {
		return
	}
	_ = simctl.Shutdown(meta.UDID)
}
