package pool

import (
	"fmt"
	"syscall"
	"time"

	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// PoisonReason distinguishes *why* a free-looking slot's previous consumer
// still appears alive — this matters because only one of these reasons is
// ever safe to act on by killing something. This predicate used to be
// copy-pasted (and drifting) across tryAcquireSlots/take, claimSlotForLease,
// reapSlot, and RunDoctor; CheckPoison is now the single source of truth.
type PoisonReason int

const (
	// NotPoisoned: no evidence of a still-alive previous consumer.
	NotPoisoned PoisonReason = iota
	// PoisonedByConsumerPGID means meta.ConsumerPGID — a process group
	// `simpool with` itself created via Setpgid — still has a live member.
	// This is the ONLY reason AttemptRecovery is ever allowed to kill
	// anything: it is a process group simpool itself spawned, so simpool
	// itself may reap it.
	PoisonedByConsumerPGID
	// PoisonedByLiveConsumers means procs.LiveConsumers found a live
	// process referencing the slot's UDID on its own command line, outside
	// the simulator's own runtime tree. For a slot in `lease` mode this is
	// the healthy case — MAV's hot loop, or a human running `simctl`/`axe`
	// by hand against the leased device — not an orphan. NEVER a kill
	// candidate, regardless of Meta.Mode: see AttemptRecovery.
	PoisonedByLiveConsumers
)

// Poison is the result of CheckPoison: whether a free-looking slot's
// previous consumer is still alive, and why.
type Poison struct {
	Reason PoisonReason
	// PGID is set only when Reason == PoisonedByConsumerPGID.
	PGID int
}

// Poisoned reports whether the slot should be treated as unavailable for
// handout to a new consumer.
func (p Poison) Poisoned() bool { return p.Reason != NotPoisoned }

func (p Poison) String() string {
	switch p.Reason {
	case PoisonedByConsumerPGID:
		return fmt.Sprintf("consumer process group %d is still alive", p.PGID)
	case PoisonedByLiveConsumers:
		return "a live process still references this slot's device"
	default:
		return "not poisoned"
	}
}

// CheckPoison reports whether meta describes a slot whose previous
// consumer is still alive even though the flock is free — the single,
// canonical predicate for that question. ConsumerPGID is checked first
// because it catches a consumer whose only distinguishing trace is an
// environment variable (MAV_TARGET_UDID, SIMPOOL_UDID_N) rather than
// anything in its own argv, which LiveConsumers (pgrep -f <udid>) cannot
// see at all; LiveConsumers remains as a second signal for slots with no
// recorded PGID (e.g. `acquire`/`lease` mode, or a meta.json predating this
// field).
func CheckPoison(meta Meta) Poison {
	if meta.UDID == "" {
		return Poison{}
	}
	if meta.ConsumerPGID != 0 && procs.PGIDAlive(meta.ConsumerPGID) {
		return Poison{Reason: PoisonedByConsumerPGID, PGID: meta.ConsumerPGID}
	}
	if live, _ := procs.LiveConsumers(meta.UDID); len(live) > 0 {
		return Poison{Reason: PoisonedByLiveConsumers}
	}
	return Poison{}
}

// VerifyConsumerIdentity checks meta's recorded consumer fingerprint
// (ConsumerStartedAt/ConsumerBootID, recorded by `simpool with` right after
// launching its child — see with.go) against present reality.
//
// rebooted reports whether the machine has rebooted since the fingerprint
// was recorded: nothing survives a reboot, so a slot in that state can be
// reclaimed with no signal sent to anyone, even if PGIDAlive happens to
// report true for an unrelated process group that has since reused the
// same numeric pgid.
//
// ok reports whether — the machine not having rebooted — the process group
// leader currently alive under meta.ConsumerPGID is provably the exact
// process that was recorded (its `ps -o lstart=` matches). macOS recycles
// pids, so a numeric pgid match alone is never enough evidence to kill.
func VerifyConsumerIdentity(meta Meta) (ok bool, rebooted bool) {
	if meta.ConsumerBootID == "" || meta.ConsumerStartedAt == "" {
		// No fingerprint recorded — meta.json predates this feature, or
		// was written by a build that never captured one. Can't verify,
		// so never kill.
		return false, false
	}
	currentBoot, err := procs.MachineBootTime()
	if err != nil {
		return false, false
	}
	if currentBoot != meta.ConsumerBootID {
		return false, true
	}
	started, err := procs.ProcessStartTime(meta.ConsumerPGID)
	if err != nil {
		// The recorded pgid leader is gone outright (as opposed to still
		// alive under a different identity) — PGIDAlive may still report
		// true for some other member of that numeric group, but there is
		// no leader left to positively identify, so refuse to guess.
		return false, false
	}
	return started == meta.ConsumerStartedAt, false
}

// recoveryPostKillWait is how long AttemptRecovery gives a just-SIGKILLed
// process group to actually disappear before giving up. Kept in the tens
// of milliseconds, not seconds: claimSlotForLease runs this inside the
// group's blocking allocation lock, so anyone else waiting on that lock (a
// concurrent `with`/`acquire`/`lease` call) is blocked for as long as this
// takes.
const recoveryPostKillWait = 50 * time.Millisecond

// AttemptRecovery tries to reclaim dir's slot from a poisoned prior
// consumer, mutating meta in place and persisting it to disk on success.
//
// Callers must already be certain no other simpool process can be
// examining or mutating this exact slot concurrently: take() holds the
// slot's own flock across this call, claimSlotForLease holds the group
// allocation lock and has just re-confirmed IsSlotFree, and ExitSweep takes
// the slot's flock itself before calling this — so the identity check, the
// kill, and the shutdown below can never race a second recovery attempt on
// the same slot.
//
// Returns true if the slot is now safe to hand to a new consumer: meta has
// been updated (ConsumerPGID and its fingerprint cleared) and persisted,
// and the simulator — if any — has been shut down (EnsureProvisioned boots
// a clean one for whoever gets it next). Returns false to mean "leave this
// slot exactly as it was": the caller must fall back to its existing
// quarantine behavior (refuse to hand it out, or leave it alone).
func AttemptRecovery(dir string, meta *Meta, poison Poison) bool {
	if meta.Mode != "with" {
		// Only a `with`-launched process group is something simpool itself
		// spawned (Setpgid) and can therefore safely kill. `acquire` never
		// spawns anything; a lease's whole point is that a user-owned
		// process referencing the UDID is the healthy case, not an orphan.
		return false
	}
	if poison.Reason != PoisonedByConsumerPGID {
		// Never kill based on LiveConsumers alone — see CheckPoison's doc
		// comment and the design's most important safety rule: a process
		// with the UDID in its argv may be a legitimate, user-driven
		// session (axe, simctl, a human) against a leased simulator.
		return false
	}

	verified, rebooted := VerifyConsumerIdentity(*meta)
	if !rebooted && !verified {
		// Could be a recycled pid some unrelated process group has since
		// inherited — refuse to guess, behave exactly as before this
		// feature existed (quarantine).
		return false
	}

	if !rebooted {
		if err := procs.KillProcessGroup(poison.PGID, syscall.SIGKILL); err != nil {
			return false
		}
		time.Sleep(recoveryPostKillWait)
		if procs.PGIDAlive(poison.PGID) {
			// Likely stuck in an uninterruptible sleep inside
			// CoreSimulator; quarantine exactly as before rather than
			// waiting indefinitely — this call must stay fast (see
			// recoveryPostKillWait).
			return false
		}
	}
	// rebooted: nothing survives a reboot, so no signal is sent at all —
	// whatever process now coincidentally shares this pgid number (if any)
	// is necessarily unrelated and must not be touched.

	if meta.UDID != "" {
		// Best-effort: EnsureProvisioned boots a fresh, clean device for
		// the next holder either way.
		_ = simctl.Shutdown(meta.UDID)
	}
	meta.ConsumerPGID = 0
	meta.ConsumerStartedAt = ""
	meta.ConsumerBootID = ""
	_ = WriteMeta(dir, *meta)
	return true
}
