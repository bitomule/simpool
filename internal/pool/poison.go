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
	// PoisonedByCheckFailure means the liveness check itself could not
	// complete (e.g. `pgrep` failing to fork under load — reproduced
	// directly on a machine at load 239 while this was being reviewed).
	// Never a kill candidate: a failed check must be read as "busy, don't
	// touch", never as "confirmed free".
	PoisonedByCheckFailure
)

// Poison is the result of CheckPoison: whether a free-looking slot's
// previous consumer is still alive (or unverifiable), and why.
type Poison struct {
	Reason PoisonReason
	// PGID is set only when Reason == PoisonedByConsumerPGID.
	PGID int
	// Err is set only when Reason == PoisonedByCheckFailure.
	Err error
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
	case PoisonedByCheckFailure:
		return fmt.Sprintf("could not verify liveness: %v", p.Err)
	default:
		return "not poisoned"
	}
}

// CheckPoison reports whether meta describes a slot whose previous
// consumer is still alive (or whose liveness could not be checked at all)
// even though the flock is free — the single, canonical predicate for that
// question. ConsumerPGID is checked first because it catches a consumer
// whose only distinguishing trace is an environment variable
// (MAV_TARGET_UDID, SIMPOOL_UDID_N) rather than anything in its own argv,
// which LiveConsumers (pgrep -f <udid>) cannot see at all; LiveConsumers
// remains as a second signal for slots with no recorded PGID (e.g.
// `acquire`/`lease` mode, or a meta.json predating this field).
//
// A LiveConsumers failure (pgrep itself could not run — a real failure
// mode under resource pressure, not merely "no matches") is reported as
// PoisonedByCheckFailure rather than silently treated as "no live
// consumer": a check that could not complete must never be read as "free".
func CheckPoison(meta Meta) Poison {
	if meta.UDID == "" {
		return Poison{}
	}
	if meta.ConsumerPGID != 0 && procs.PGIDAlive(meta.ConsumerPGID) {
		return Poison{Reason: PoisonedByConsumerPGID, PGID: meta.ConsumerPGID}
	}
	live, err := procs.LiveConsumers(meta.UDID)
	if err != nil {
		return Poison{Reason: PoisonedByCheckFailure, Err: err}
	}
	if len(live) > 0 {
		return Poison{Reason: PoisonedByLiveConsumers}
	}
	return Poison{}
}

// VerifyConsumerIdentity checks meta's recorded consumer fingerprint
// (ConsumerStartedAt, recorded by `simpool with` right after launching its
// child — see with.go) against present reality: is the process currently
// alive under meta.ConsumerPGID provably the exact process that was
// recorded (its start time matches exactly)? macOS recycles pids, so a
// numeric pgid match alone is never enough evidence to kill.
//
// Deliberate, documented scope limit: this only ever re-identifies the
// process-group LEADER (the pid == pgid). If that exact process has
// already exited — even though PGIDAlive(meta.ConsumerPGID) is still true
// because other members of its group (grandchildren it spawned before
// exiting) are still alive — there is no live leader left to compare a
// start time against, and this returns false. Trusting bare pgid
// membership alone at that point would be exactly the evidence PGIDAlive
// already provides, which this whole mechanism exists specifically not to
// trust blindly for a kill decision. This is judged an acceptable, rare gap
// (see README "Architecture") rather than something to route around with a
// weaker check: the dominant real failure mode — `simpool` itself
// SIGKILLed while its own direct child (the leader) is still actively
// running — is unaffected, since the leader is exactly what's still alive
// in that case.
func VerifyConsumerIdentity(meta Meta) bool {
	if meta.ConsumerStartedAt == "" || meta.ConsumerPGID <= 0 {
		// No fingerprint recorded — meta.json predates this feature, or
		// was written by a build that never captured one. Can't verify,
		// so never kill.
		return false
	}
	started, err := procs.ProcessStartTime(meta.ConsumerPGID)
	if err != nil {
		// The recorded pgid leader is gone outright (as opposed to still
		// alive under a different identity) — refuse to guess.
		return false
	}
	return started == meta.ConsumerStartedAt
}

// recoveryPostKillWait is how long AttemptRecovery gives a just-SIGKILLed
// process group to actually disappear before giving up. Kept in the tens
// of milliseconds, not seconds: claimSlotForLease runs this inside the
// group's blocking allocation lock, so anyone else waiting on that lock (a
// concurrent `with`/`acquire`/`lease` call) is blocked for as long as this
// takes.
const recoveryPostKillWait = 50 * time.Millisecond

// deviceBelongsToSlot reports whether udid's actual entry in the default
// device set is exactly the deterministic device this one slot (root,
// device, osVersion, n) is supposed to own — see DeviceName. This is the
// one guard every path that might shut down or delete a simulator based on
// a UDID pulled from meta.json must pass first: meta.json is advisory and
// can be stale or outright corrupt (a UDID left over from a previous,
// unrelated slot, or pointing at one of the *other* ~30 simulators a
// developer keeps around for their own use) while everything else about it
// — a verifiable ConsumerPGID fingerprint included — still checks out. A
// bare `found` is not enough either: the default device set holds every
// slot's simulator plus the user's own, so only an exact name match proves
// this UDID is actually this slot's, not merely *a* pool-owned device that
// happens to still exist.
func deviceBelongsToSlot(root, udid, device, osVersion string, n int) bool {
	entry, found, err := simctl.Find(udid)
	if err != nil || !found {
		return false
	}
	return entry.Name == DeviceName(root, device, osVersion, n)
}

// AttemptRecovery tries to reclaim dir's slot from a poisoned prior
// consumer, mutating meta in place and persisting it to disk on success.
//
// Callers must already be certain no other simpool process can be
// examining or mutating this exact slot concurrently: take() and
// claimSlotForLease each hold the slot's own flock across this call (see
// their doc comments) — that, plus each already running inside the group
// allocation lock, is what guarantees no concurrent recovery attempt on the
// same slot regardless of whether the caller is `with`/`acquire` or
// `lease`.
//
// root, n, device, and osVersion identify which exact simulator THIS slot
// is supposed to own (via DeviceName) — see deviceBelongsToSlot. Killing
// the poisoned orphan process group never depends on meta.UDID at all (it
// only ever needs meta.ConsumerPGID, verified against
// meta.ConsumerStartedAt), but shutting down a simulator does, and
// meta.UDID is exactly the field the design explicitly tolerates being
// stale or corrupt. A slot whose meta.json was clobbered to point at a
// live simulator belonging to another slot — or one of a developer's own,
// unrelated simulators — combined with a genuinely verifiable
// ConsumerPGID (the recovery this function performs does not clear that
// combination) used to still shut down whatever device that stale UDID
// happened to name, unconditionally. That is fixed here: the orphan is
// still killed and the slot is still reclaimed either way (killing the
// process never needed the UDID to be trustworthy), but the simulator is
// only ever asked to shut down once deviceBelongsToSlot confirms the UDID
// actually names this slot's own device.
//
// Returns true if the slot is now safe to hand to a new consumer: meta has
// been updated (ConsumerPGID and its fingerprint cleared) and persisted.
// The simulator, if meta.UDID names one, is asked to shut down
// (best-effort) only once its identity has been verified this way —
// EnsureProvisioned boots a clean one for whoever gets it next regardless
// of the device's exact state when handed over, so skipping the shutdown
// when identity can't be confirmed never blocks handing the slot out, it
// only avoids touching a simulator that was never provably this slot's to
// begin with. Returns false to mean "leave this slot exactly as it was":
// the caller must fall back to its existing quarantine behavior (refuse to
// hand it out, or leave it alone).
func AttemptRecovery(root, dir string, n int, device, osVersion string, meta *Meta, poison Poison) bool {
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
		// session (axe, simctl, a human) against a leased simulator. And
		// never act on PoisonedByCheckFailure — an incomplete check is
		// never grounds for a kill decision either way.
		return false
	}

	if !VerifyConsumerIdentity(*meta) {
		// Could be a recycled pid some unrelated process group has since
		// inherited, or the recorded leader has already exited while a
		// descendant lingers (see VerifyConsumerIdentity's doc comment) —
		// refuse to guess, behave exactly as before this feature existed
		// (quarantine).
		return false
	}

	if err := procs.KillProcessGroup(poison.PGID, syscall.SIGKILL); err != nil {
		return false
	}
	time.Sleep(recoveryPostKillWait)
	if procs.PGIDAlive(poison.PGID) {
		// Likely stuck in an uninterruptible sleep inside CoreSimulator;
		// quarantine exactly as before rather than waiting indefinitely —
		// this call must stay fast (see recoveryPostKillWait).
		return false
	}

	if meta.UDID != "" && deviceBelongsToSlot(root, meta.UDID, device, osVersion, n) {
		// Best-effort, and deliberately asynchronous (not waited on here):
		// EnsureProvisioned boots a fresh device for the next holder
		// regardless of exactly what state this one is in when handed
		// over. Callers must never treat a just-recovered slot's device as
		// already fully "Shutdown" in the same pass — see reap.go's RECOVER
		// handling, which returns immediately rather than falling through
		// to same-pass --purge accounting for exactly this reason.
		//
		// Gated on deviceBelongsToSlot: see this function's own doc comment
		// for why meta.UDID alone is never enough grounds to shut anything
		// down. If the check fails, this branch simply does nothing to the
		// device — the orphan is still killed and the slot still reclaimed
		// below regardless.
		_ = simctl.Shutdown(meta.UDID)
	}
	meta.ConsumerPGID = 0
	meta.ConsumerStartedAt = ""
	_ = WriteMeta(dir, *meta)
	return true
}
