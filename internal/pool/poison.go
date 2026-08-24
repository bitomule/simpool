package pool

import (
	"errors"
	"fmt"
	"os"
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
	// PoisonedByOrphanedResidue means every PID procs.LiveConsumers
	// found is independently verified (procs.IsIdbCompanionFor) to be an
	// idb_companion daemon pinned, via its own --udid flag, to THIS slot's
	// device — and there is independent evidence that none of them can be
	// serving live work (see ResidueEvidence for the two forms that
	// evidence takes). Unlike PoisonedByLiveConsumers, this reason IS a
	// kill candidate for AttemptRecovery, regardless of Meta.Mode:
	// idb_companion is disposable infrastructure `idb` respawns on demand,
	// never "the consumer" itself. A single non-companion PID mixed in —
	// the `idb` client process itself, an `axe` run, a stray
	// `simctl spawn <udid>` — means this slot is not narrowly explained by
	// companion residue alone and falls back to PoisonedByLiveConsumers
	// instead; so does a companion set with no evidence behind it. See
	// CheckPoison.
	PoisonedByOrphanedResidue
)

// ResidueEvidence names WHY a set of verified idb_companion daemons was
// judged reclaimable residue rather than infrastructure something is
// actively using. The distinction is load-bearing, not cosmetic: only
// ResidueTargetNotRunning is ever eligible for the operator-driven
// `--disown-poisoned` path, which deliberately skips the
// deviceBelongsToSlot identity guard (see DisownPoisonedSlot) and therefore
// has nothing left but the device's own state to keep it off a simulator
// that was never this slot's.
type ResidueEvidence int

const (
	// NoResidueEvidence: no grounds to treat companions as residue.
	NoResidueEvidence ResidueEvidence = iota
	// ResidueTargetNotRunning means the companion's target device is
	// independently confirmed not currently running — simctl reports it
	// Shutdown, or it no longer exists in a populated device set at all
	// (the original production incident: a companion still holding the
	// UDID of a simulator deleted nine days earlier). Nothing can be
	// talking to a device that is not running, so the companion is inert
	// by construction.
	ResidueTargetNotRunning
	// ResidueSlotLongIdle means the target device IS running, but the
	// slot it belongs to is provably unheld and has been untouched for at
	// least ResidueIdleGrace.
	//
	// This is the case a warm pool actually hits, and the one the
	// device-state evidence above structurally cannot cover: simpool's
	// whole purpose is to keep slot devices Booted between consumers, so
	// "confirmed not running" is never true for a healthy pool slot, and
	// before this existed every orphaned companion on a warm slot poisoned
	// it permanently — reported by `doctor`, skipped by `reap`, and
	// rejected by `--disown-poisoned` alike.
	//
	// "Provably unheld" is not an assumption: every caller that can act on
	// a Poison (take, claimSlotForLease, reapSlot) holds this slot's own
	// flock across the call AND has already established there is no live
	// lease on it — that is the documented precondition of AttemptRecovery
	// and DisownPoisonedSlot. No `with`, `acquire` or `lease` consumer can
	// be holding a slot in that state. ResidueIdleGrace then covers the
	// one consumer that legitimately does not hold the flock and may have
	// let its lease lapse: an active MAV hot loop mid-session. See that
	// constant.
	ResidueSlotLongIdle
)

// ResidueIdleGrace is how long a slot whose device is still running must
// have gone untouched — Meta.LastUsed, stamped by EnsureProvisioned on
// every acquisition AND every lease renewal, so it advances on every single
// `mav tap`/`mav swipe` in a hot loop — before a live idb_companion
// attached to it may be treated as residue rather than as infrastructure
// for a session in progress.
//
// Deliberately an order of magnitude above DefaultLeaseTTL (3m) rather than
// equal to it. A lapsed lease alone already makes a slot available to a new
// consumer by design, but a companion is the last trace of a MAV session
// that might merely be quiet — stuck in a long `mav run` build between two
// calls — and the cost of guessing wrong is reclaiming a slot out from
// under running work. Half an hour of MAV silence is a session that has
// stopped renewing anything simpool can see; the real orphans this exists
// to reclaim sit at hours-to-days of idleness, so nothing is lost by
// waiting well past any plausible inter-call gap.
//
// A zero Meta.LastUsed is NOT treated as "infinitely idle": it means the
// slot's own bookkeeping never recorded a last use, which is an inability
// to check, and this codebase's rule for that is uniform — never read a
// check that could not complete as "confirmed free" (see
// PoisonedByCheckFailure). Such a slot stays quarantined exactly as it
// does today.
const ResidueIdleGrace = 30 * time.Minute

// Poison is the result of CheckPoison: whether a free-looking slot's
// previous consumer is still alive (or unverifiable), and why.
type Poison struct {
	Reason PoisonReason
	// PGID is set only when Reason == PoisonedByConsumerPGID.
	PGID int
	// Err is set only when Reason == PoisonedByCheckFailure.
	Err error
	// ResiduePIDs is set only when Reason == PoisonedByOrphanedResidue:
	// every live PID CheckPoison verified is an idb_companion daemon
	// pinned to this slot's device.
	ResiduePIDs []int
	// ResidueEvidence is set only when Reason ==
	// PoisonedByOrphanedResidue: why those PIDs were judged residue.
	ResidueEvidence ResidueEvidence
}

// ResidueDisownable reports whether this poison is one `--disown-poisoned`
// may act on via its companion branch. Only ResidueTargetNotRunning
// qualifies: that path deliberately skips the deviceBelongsToSlot identity
// guard, leaving the device's own confirmed-not-running state as the only
// thing standing between it and a simulator a stale meta.UDID happens to
// name — a developer's own, say — so a companion attached to a device that
// IS running must never be reachable through it. The running-device case
// needs no escape hatch anyway: it is exactly the case AttemptRecovery now
// handles automatically (see ResidueSlotLongIdle).
func (p Poison) ResidueDisownable() bool {
	return p.Reason == PoisonedByOrphanedResidue && p.ResidueEvidence == ResidueTargetNotRunning
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
	case PoisonedByOrphanedResidue:
		if p.ResidueEvidence == ResidueTargetNotRunning {
			return fmt.Sprintf("orphaned idb_companion daemon(s) attached to a non-running device (pids %v)", p.ResiduePIDs)
		}
		return fmt.Sprintf("orphaned idb_companion daemon(s) left on a slot with no holder and no use in over %s (pids %v)", ResidueIdleGrace, p.ResiduePIDs)
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
	if len(live) == 0 {
		return Poison{}
	}
	if companions, evidence, ok := allReclaimableResidue(meta, live); ok {
		return Poison{Reason: PoisonedByOrphanedResidue, ResiduePIDs: companions, ResidueEvidence: evidence}
	}
	return Poison{Reason: PoisonedByLiveConsumers}
}

// residueDeviceList is simctl.ListDevices, as a package-level var so
// tests can simulate a healthy device set (with or without udid present)
// versus a suspiciously EMPTY one for a synthetic UDID without creating or
// booting a real simulator — mirrors the seams already used elsewhere in
// this codebase (reap.go's findDevice/shutdownOrphan, provision.go's
// liveProvisionDeps). Deliberately NOT simctl.Find: Find collapses "udid is
// absent from a populated, healthy listing" and "the whole listing came
// back empty" into the same found=false, and those two cases must be told
// apart here — see residueDeviceOffline.
var residueDeviceList = simctl.ListDevices

// residueDeviceOffline reports whether udid names a simulator that is
// definitely not currently running — the only condition under which an
// idb_companion process attached to it can possibly be doing no useful
// work. Two conclusive cases:
//
//   - udid is present in a device listing and its own State is exactly
//     "Shutdown" — an ALLOWLIST, not `!= "Booted"`: a companion pinned to a
//     device mid-transition ("Booting", "Shutting Down", "Creating", an
//     unrecognized future state, or even "" if the JSON omits the field
//     entirely) is not provably idle. "Booting" in particular names a
//     device whose launchd_sim is already up and whose boot is actively
//     underway — exactly the state a lease renewal race can produce (see
//     this function's own review history) — so it must never be treated as
//     safe to act on merely because it isn't literally "Booted" yet.
//   - udid is absent from a listing that is itself non-empty — the actual
//     production incident this exists to fix: a companion still holding the
//     UDID of a simulator deleted nine days earlier. There is no device left
//     to check its boot state, but "missing from a listing that visibly
//     still contains other devices" is itself conclusive: CoreSimulator
//     successfully enumerated everything it knows about and this UDID
//     simply isn't among them.
//
// A listing that comes back completely EMPTY is neither of those: a
// degraded or mid-restart CoreSimulator can report zero devices — including
// none of the user's own — even though every device, this one included,
// still genuinely exists; a probe reproduced exactly this (`xcrun` exiting
// 0 with `{"devices":{}}`) driving the real simctl.Find. An empty listing is
// therefore read the same as an outright listing failure: ok=false, never
// "confirmed deleted". Any failure to list at all (simctl.ListDevices
// itself erroring) reports ok=false for the identical reason — the same
// fail-safe rule every poison check in this codebase follows: an inability
// to verify must never be read as "confirmed offline".
func residueDeviceOffline(udid string) (offline, ok bool) {
	devices, err := residueDeviceList()
	if err != nil {
		return false, false
	}
	if len(devices) == 0 {
		return false, false
	}
	for _, d := range devices {
		if d.UDID == udid {
			return d.State == "Shutdown", true
		}
	}
	return true, true
}

// residueSlotLongIdle reports whether meta describes a slot simpool
// itself has not touched for at least ResidueIdleGrace — the second,
// independent form of evidence that a live companion attached to it is
// residue rather than infrastructure for a session in progress. See
// ResidueSlotLongIdle and ResidueIdleGrace for why a zero LastUsed
// deliberately fails this rather than reading as "infinitely idle".
func residueSlotLongIdle(meta Meta) bool {
	if meta.LastUsed.IsZero() {
		return false
	}
	return time.Since(meta.LastUsed) >= ResidueIdleGrace
}

// allReclaimableResidue reports whether every pid in live is verified
// (procs.IsIdbCompanionFor) to be an idb_companion daemon pinned to
// meta.UDID, AND there is independent evidence that none of them can be
// serving live work — either the device itself is confirmed not running
// (residueDeviceOffline) or the slot has gone untouched for at least
// ResidueIdleGrace (residueSlotLongIdle). Both forms are conclusive on
// their own and neither is required alongside the other; see
// ResidueEvidence.
//
// A single non-companion PID mixed in with real companions means this slot
// is not narrowly explained by companion residue alone — the `idb` client
// process itself carries the UDID in its own argv, as does `axe`, a human's
// `simctl`, or the stray `simctl spawn <udid>` measured alongside these
// orphans in production — so the whole set falls back to the ordinary,
// never-a-kill-candidate PoisonedByLiveConsumers reason. That check runs
// first now, ahead of the device lookup: it is the cheap one, and it is
// also the one that most often rules the whole branch out, so there is no
// reason to pay for a `simctl list devices` before it.
func allReclaimableResidue(meta Meta, live []int) ([]int, ResidueEvidence, bool) {
	if len(live) == 0 {
		return nil, NoResidueEvidence, false
	}
	for _, pid := range live {
		if !procs.IsIdbCompanionFor(pid, meta.UDID) {
			return nil, NoResidueEvidence, false
		}
	}
	if offline, ok := residueDeviceOffline(meta.UDID); ok && offline {
		return live, ResidueTargetNotRunning, true
	}
	if residueSlotLongIdle(meta) {
		return live, ResidueSlotLongIdle, true
	}
	return nil, NoResidueEvidence, false
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

// residueKill is procs.Kill, as a package-level var so tests can prove
// reclaimOrphanedResidue/disownOrphanedResidue attempt EVERY pid in
// poison.ResiduePIDs even when an earlier one in the loop reports an
// error — the only way procs.Kill itself ever actually returns a non-nil
// error is a real EPERM (signaling a process this caller doesn't own),
// which is not reliably reproducible on demand the same way killFunc's
// EPERM simulation in package procs is not.
var residueKill = procs.Kill

// recoveryPostKillWait is how long AttemptRecovery gives a just-SIGKILLed
// process group to actually disappear before giving up. Kept in the tens
// of milliseconds, not seconds: claimSlotForLease runs this inside the
// group's blocking allocation lock, so anyone else waiting on that lock (a
// concurrent `with`/`acquire`/`lease` call) is blocked for as long as this
// takes.
const recoveryPostKillWait = 50 * time.Millisecond

// deviceBelongsToSlotFind is simctl.Find, as a package-level var — mirrors
// residueDeviceList's own reasoning for existing as a seam — so tests can
// simulate a device genuinely present and correctly (or incorrectly) named
// for a given slot without creating a real simulator. Needed specifically
// so reclaimOrphanedResidue' identity guard (see finding review) can be
// exercised on BOTH its true and false paths at unit-test speed: the
// existing ConsumerPGID tests in this file only ever needed the false path
// (a synthetic UDID the real device set never contains), which the real
// simctl.Find already gives them for free — but proving the companion path
// actually reclaims once identity IS verified needs a positive case too.
var deviceBelongsToSlotFind = simctl.Find

// deviceBelongsToSlot reports whether udid's actual entry in the default
// device set is exactly the deterministic device this one slot (root,
// device, osVersion, n) is supposed to own — see DeviceName. This is the
// one guard every path that might shut down or delete a simulator — or, as
// of reclaimOrphanedResidue, kill a process attached to one — based on a
// UDID pulled from meta.json must pass first: meta.json is advisory and
// can be stale or outright corrupt (a UDID left over from a previous,
// unrelated slot, or pointing at one of the *other* ~30 simulators a
// developer keeps around for their own use) while everything else about it
// — a verifiable ConsumerPGID fingerprint, or a companion's own exact --udid
// match, included — still checks out. A bare `found` is not enough either:
// the default device set holds every slot's simulator plus the user's own,
// so only an exact name match proves this UDID is actually this slot's, not
// merely *a* pool-owned device that happens to still exist.
// groupName must come from the slot's own directory, never from meta —
// see DeviceNameForGroup for why deriving it from meta makes this guard
// self-referential and blind to a coherent-but-wrong meta.json.
func deviceBelongsToSlot(root, udid, groupName string, n int) bool {
	entry, found, err := deviceBelongsToSlotFind(udid)
	if err != nil || !found {
		return false
	}
	return entry.Name == DeviceNameForGroup(root, groupName, n)
}

// ResidueDeviceVerified reports whether meta.UDID can be positively
// confirmed, by name, to be this exact slot's (root, groupName, n) own
// device — exported purely for `reap`'s --dry-run reporting, so it can
// preview whether AttemptRecovery's automatic PoisonedByOrphanedResidue
// branch would actually reclaim on a real run (the Shutdown-and-verified
// case) versus refuse and require --disown-poisoned (the deleted-device
// case, or a Shutdown device whose identity can't be confirmed) — see
// reclaimOrphanedResidue' own identity guard, which this mirrors exactly
// without ever mutating or killing anything itself.
func ResidueDeviceVerified(root, groupName string, n int, meta Meta) bool {
	return meta.UDID != "" && deviceBelongsToSlot(root, meta.UDID, groupName, n)
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
func AttemptRecovery(root, dir string, n int, groupName string, meta *Meta, poison Poison) bool {
	if poison.Reason == PoisonedByOrphanedResidue {
		// Mode-independent, and handled entirely separately from the
		// ConsumerPGID branch below: idb_companion residue has nothing to
		// do with which subcommand is holding (or held) this slot, only
		// with what `idb` itself left behind, so this runs ahead of — not
		// behind — the Mode == "with" gate that scopes every other branch
		// of this function. See reclaimOrphanedResidue.
		return reclaimOrphanedResidue(root, groupName, n, meta, poison)
	}

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

	if meta.UDID != "" && deviceBelongsToSlot(root, meta.UDID, groupName, n) {
		// Best-effort: simctl.Shutdown itself is measured SYNCHRONOUS (it
		// blocks until the device's reported state is genuinely "Shutdown"
		// — 5-7.5s wall time observed — not the async, returns-immediately
		// call an earlier version of this comment assumed), so by the time
		// this line returns the state is already accurate. What is not
		// waited on here is CoreSimulator's own teardown of the device's
		// underlying process tree, which can still be settling for a beat
		// after that state flip. Callers must still never chain an
		// immediate --purge/delete onto a just-recovered slot in the same
		// pass — see reap.go's RECOVER handling, which returns immediately
		// rather than falling through to same-pass --purge accounting for
		// exactly this reason.
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

// reclaimOrphanedResidue kills exactly the PIDs CheckPoison already
// verified are idb_companion daemons pinned to a device confirmed not
// currently running (see PoisonedByOrphanedResidue) — never a process
// group: idb_companion is not something simpool spawned via Setpgid, so
// there is no pgid relationship to exploit the way KillProcessGroup needs.
// Each pid is signaled individually, exactly like reap's own STUCK-holder
// kill in reapHeldSlot, for the identical reason: a companion's own pgid is
// whatever `idb` (or launchd, if reparented) happened to assign it, not
// guaranteed to be its own — kill(-pid) could otherwise land on an
// unrelated process group that happens to share that numeric id.
//
// Gated on deviceBelongsToSlot, exactly like the ConsumerPGID branch's own
// Shutdown call above, and for the identical reason: meta.UDID is advisory
// and can be stale or corrupt, and CheckPoison's own verification (a
// companion pinned to udid via its own --udid flag, plus evidence that
// nothing can be using it) proves the COMPANION's identity, never that udid
// is actually THIS SLOT's own device rather than some other real simulator
// — including one of a developer's own, entirely unrelated to the pool —
// that a stale meta.UDID happens to name. This guard is what keeps a
// long-idle slot pointing (wrongly) at someone else's booted
// simulator from getting that simulator's companion killed under
// ResidueSlotLongIdle.
//
// deviceBelongsToSlot can only ever succeed when udid's device still exists
// and is name-matched against THIS slot's own directory (root, groupName,
// n) — so it covers the Shutdown and still-running cases but never the
// deleted-device one: a deleted device can never be found at all, so this
// guard can never be satisfied for it. That is deliberate, not a bug to
// route around — see DisownPoisonedSlot's PoisonedByOrphanedResidue
// branch for the explicit, operator-opt-in path that exists specifically to
// cover the deleted-device case, which this function refuses outright.
//
// Also re-runs CheckPoison's whole companion determination from scratch
// immediately before killing anything, not from any earlier result — the
// same "re-verify right before acting" rule reap --orphans applies to its
// own device scan (see reapOrphans' doc comment). It deliberately re-reads
// the LIVE CONSUMER SET rather than just re-checking the recorded PIDs:
// CheckPoison's determination and this call are seconds apart in `reap`,
// and the thing most worth catching in that window is not a companion that
// changed identity but a NON-companion that appeared — the `idb` client
// process of a hot-loop call that arrived just now, carrying the UDID in
// its own argv. Any difference at all from the recorded set, in either
// direction, refuses the whole attempt and leaves the next pass to
// re-evaluate from a coherent snapshot.
//
// Every PID gets a kill attempt regardless of whether an earlier one in the
// loop failed — an early return partway through would leave some PIDs
// already dead while the function's own contract implies nothing happened,
// which is worse than attempting the full set and then verifying explicitly
// below. Unlike the ConsumerPGID branch above, nothing in meta.json ever
// records an idb_companion's pid — `idb` spawns it completely outside
// simpool's own bookkeeping — so there is nothing to clear or persist here
// on success: the slot is simply safe to hand out again once every kill is
// verified to have actually stuck. The device itself is never touched, in
// either evidence case — for ResidueTargetNotRunning there is nothing
// left to shut down, and for ResidueSlotLongIdle the device is a warm
// pool slot's own simulator that the next consumer wants exactly as it is.
func reclaimOrphanedResidue(root, groupName string, n int, meta *Meta, poison Poison) bool {
	if len(poison.ResiduePIDs) == 0 {
		return false
	}
	if meta.UDID == "" || !deviceBelongsToSlot(root, meta.UDID, groupName, n) {
		return false
	}
	live, err := procs.LiveConsumers(meta.UDID)
	if err != nil {
		// The re-verification itself could not complete — never read that
		// as "still safe to kill", the same rule PoisonedByCheckFailure
		// applies one layer up.
		return false
	}
	companions, evidence, ok := allReclaimableResidue(*meta, live)
	if !ok || evidence != poison.ResidueEvidence || !samePIDSet(companions, poison.ResiduePIDs) {
		// Re-verified and no longer holds: the device came back (or its
		// state became unreadable), the slot was used again, a
		// non-companion consumer appeared, or the PID set moved under us.
		// Never act on a stale determination.
		return false
	}
	for _, pid := range poison.ResiduePIDs {
		_ = residueKill(pid, syscall.SIGKILL)
	}
	time.Sleep(recoveryPostKillWait)
	for _, pid := range poison.ResiduePIDs {
		if procs.Alive(pid) {
			// The signal didn't stick (or something else about this pid
			// couldn't be confirmed dead) — quarantine exactly like the
			// ConsumerPGID branch's own post-kill verification rather than
			// declaring victory on the strength of a signal send alone.
			return false
		}
	}
	why := "device confirmed not running"
	if poison.ResidueEvidence == ResidueSlotLongIdle {
		why = fmt.Sprintf("slot unheld and unused for over %s", ResidueIdleGrace)
	}
	fmt.Fprintf(os.Stderr, "simpool: reclaimed %d orphaned idb_companion daemon(s) attached to device %s (pids %v) — %s, and device confirmed this slot's own\n", len(poison.ResiduePIDs), meta.UDID, poison.ResiduePIDs, why)
	return true
}

// samePIDSet reports whether a and b hold exactly the same PIDs, order
// independent. Used by reclaimOrphanedResidue to insist its re-verified
// live set is byte-for-byte the determination it was handed, rather than
// silently acting on whichever PIDs happen to be there now.
func samePIDSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int]int, len(a))
	for _, p := range a {
		seen[p]++
	}
	for _, p := range b {
		seen[p]--
		if seen[p] < 0 {
			return false
		}
	}
	return true
}

// ErrNotDisownable is returned by DisownPoisonedSlot when asked to act on a
// poison reason it must never touch — see DisownPoisonedSlot's own gate.
var ErrNotDisownable = errors.New("simpool: not eligible for --disown-poisoned")

// DisownPoisonedSlot is the manual, explicit-only escape hatch for a slot
// that CANNOT be recovered by AttemptRecovery and never will be: meta.
// ConsumerPGID keeps testing "alive" forever, either because the pid has
// since been recycled by some unrelated process macOS happened to assign
// the very same pgid, or because kill(-pgid, 0) fails with EPERM outright —
// that process group belongs to a different user entirely. Neither case is
// something VerifyConsumerIdentity's start-time fingerprint can ever
// resolve (see its own doc comment), so with DefaultMaxSlotsPerGroup a
// small number, a slot stuck like this is lost for good without some
// explicit way out.
//
// Why forget instead of force-kill: a "--force" flag that just sends
// SIGKILL despite a failed identity check was considered and rejected.
// First, it cannot even work for one of the two motivating cases — EPERM
// means the kernel itself refuses to let us signal that process group, so
// a kill-based escape hatch would fail on exactly the scenario that most
// needs one. Second, for the pid-recycle case, VerifyConsumerIdentity's
// whole reason to exist is that a bare numeric pgid match is never
// sufficient evidence of identity; a "--force" override would reintroduce
// precisely the failure mode that check was built to rule out, just with
// an operator's finger on the trigger instead of AttemptRecovery's. Forgetting
// — clearing this slot's recorded fingerprint and, if the device is
// verifiably its own, deleting that device outright so the next
// acquisition provisions a genuinely new one — recovers the slot without
// ever gambling on whether the thing behind ConsumerPGID is safe to kill.
// If it is in fact still running, it is left running, untouched: it is
// simply no longer simpool's problem, and no longer standing between this
// slot and reuse. Deleting the old device (not just forgetting its UDID)
// matters for the same reason EnsureProvisioned's name-based reuse exists:
// leaving the old device around but unreferenced would either leak it
// forever (nothing else in simpool inspects device-set contents to find
// it) or, worse, get silently picked back up by EnsureProvisioned's
// by-name lookup — right back to a fresh consumer sharing a device with
// whatever might still be poking at it.
//
// Gated exactly like AttemptRecovery's own kill decision (never a kill
// here, but the same restraint applies to what may be deleted): only a
// `with`-launched slot (meta.Mode == "with") whose poison reason is
// PoisonedByConsumerPGID is eligible for THIS branch. PoisonedByLiveConsumers
// is EXPLICITLY the healthy case for `lease`/`acquire` (a live user session
// against the device) and must never have its device pulled out from
// under it; PoisonedByCheckFailure means the liveness check itself never
// completed, which proves nothing is actually wrong — disowning on the
// strength of a check that didn't run would be reckless, not an escape
// hatch. Returns ErrNotDisownable, unchanged, for either.
//
// PoisonedByOrphanedResidue is routed to disownOrphanedResidue
// instead — a deliberately different shape (it DOES kill, see that
// function's own doc comment for why) covering the one gap
// AttemptRecovery's automatic companion path can never close on its own:
// reclaimOrphanedResidue requires deviceBelongsToSlot, which can only
// ever succeed against a device that still exists. A companion pinned to a
// UDID that no longer names any device at all (the deleted-device case —
// the actual production incident this whole feature exists to fix) can
// never pass that guard, by construction: there is no device left to
// name-check. Rather than paper over that gap with a weaker automatic
// guard, it is deliberately left unreachable by the fully-automatic path
// and made available here instead, exactly as opt-in and operator-driven as
// this function's existing role for an unverifiable ConsumerPGID.
//
// Must only ever be invoked on the caller's explicit, opt-in request
// (`simpool reap --disown-poisoned`), never automatically — this is not a
// "we're confident this is safe" recovery like AttemptRecovery, it is a
// deliberate override an operator reaches for after watching a slot stay
// quarantined across repeated `reap`/`doctor` runs. Callers must already
// hold this slot's own flock, exactly like AttemptRecovery's callers.
func DisownPoisonedSlot(root, dir string, n int, groupName string, meta *Meta, poison Poison) error {
	if poison.Reason == PoisonedByOrphanedResidue {
		return disownOrphanedResidue(dir, meta, poison)
	}
	if meta.Mode != "with" || poison.Reason != PoisonedByConsumerPGID {
		return ErrNotDisownable
	}
	if meta.UDID != "" && deviceBelongsToSlot(root, meta.UDID, groupName, n) {
		// Best-effort: shut down then delete outright, verified first
		// exactly like AttemptRecovery's own shutdown — meta.UDID is
		// advisory and can be stale or corrupt, so nothing is ever deleted
		// without deviceBelongsToSlot's exact-name confirmation first.
		_ = simctl.Shutdown(meta.UDID)
		_ = simctl.Delete(meta.UDID)
	}
	meta.UDID = ""
	meta.ConsumerPGID = 0
	meta.ConsumerStartedAt = ""
	meta.Mode = ""
	return WriteMeta(dir, *meta)
}

// disownOrphanedResidue is DisownPoisonedSlot's branch for
// PoisonedByOrphanedResidue — see that function's doc comment for why
// this exists at all (the deleted-device gap reclaimOrphanedResidue'
// identity guard leaves deliberately unreachable automatically).
//
// Unlike the ConsumerPGID branch above, this DOES signal the process it
// disowns: "forget without touching" makes sense for ConsumerPGID because
// the two motivating cases (EPERM, pid recycling) mean either the kernel
// refuses to let simpool signal it, or the pid isn't provably the same
// process simpool ever cared about in the first place — in both, NOT
// killing is the only safe option. Neither applies here: poison.ResiduePIDs
// is already narrowly, positively verified (procs.IsIdbCompanionFor's exact
// binary-name-plus-flag match, re-verified again below, immediately before
// acting) to be idb_companion daemons pinned to udid via their own --udid
// flag — real, killable, ordinary processes, not a permission or identity
// puzzle. And forgetting alone would accomplish nothing: idb_companion never
// self-terminates when its target goes offline (see IsIdbCompanionFor's doc
// comment on --terminate-offline defaulting to false), so leaving it running
// means it goes on consuming system resources and re-poisoning this slot on
// every subsequent CheckPoison call forever — there is no "it's simply no
// longer simpool's problem" outcome available here the way there is for a
// live process group simpool merely stops tracking.
//
// What this function deliberately does NOT do, unlike reclaimOrphanedResidue,
// is require deviceBelongsToSlot: that is precisely the guard this exists to
// route around for a device that no longer exists to name-check at all. The
// operator invoking --disown-poisoned is the judgment call this codebase
// substitutes for that guard here — mirroring exactly how the ConsumerPGID
// branch above substitutes operator judgment for VerifyConsumerIdentity's
// unattainable proof in its own two motivating cases.
//
// What this function does still require, exactly like reclaimOrphanedResidue
// (and unlike the deviceBelongsToSlot guard it skips), is re-verifying
// residueDeviceOffline(meta.UDID) immediately before acting: routing around
// deviceBelongsToSlot only ever concerns whether udid is confirmed to be THIS
// slot's own device, never whether the device it currently names is actually
// not running. CheckPoison's own determination and this call are seconds
// apart in production (reapSlot runs several real `xcrun simctl` invocations
// in between — see reap.go), a real window in which the device behind a
// stale meta.UDID could come back up: either because it is this slot's own
// device that booted since CheckPoison ran, or — since identity is never
// confirmed on this path at all — because meta.UDID now points at some
// entirely different, currently-Booted simulator (another slot's, or a
// developer's own). Skipping this re-check used to let --disown-poisoned
// kill a companion attached to a device the operator's request never
// actually asked it to touch.
func disownOrphanedResidue(dir string, meta *Meta, poison Poison) error {
	if len(poison.ResiduePIDs) == 0 || meta.UDID == "" {
		return ErrNotDisownable
	}
	if !poison.ResidueDisownable() {
		// ResidueSlotLongIdle is deliberately out of reach here: this
		// function skips the deviceBelongsToSlot identity guard on purpose
		// (see the doc comment), which leaves the device's own
		// confirmed-not-running state as the ONLY thing keeping it off a
		// simulator a stale meta.UDID happens to name. A still-running
		// device removes that last guard entirely — and needs no escape
		// hatch anyway, since AttemptRecovery handles it automatically
		// with the identity guard intact. See Poison.ResidueDisownable.
		return ErrNotDisownable
	}
	if offline, ok := residueDeviceOffline(meta.UDID); !ok || !offline {
		// Re-verified and no longer holds — the device came back (or its
		// state became unreadable) in the window between CheckPoison's
		// original determination and this call, exactly the same rule
		// reclaimOrphanedResidue applies to its own automatic path (see
		// its own re-verify above). Deliberately NOT gated on
		// deviceBelongsToSlot — see this function's own doc comment for why
		// that guard is routed around here on purpose — but the device
		// still not currently running is never something this function may
		// skip re-checking: meta.UDID could just as easily now name a real,
		// booted device (this slot's own, having booted in the window since
		// CheckPoison ran, or — since identity is never confirmed on this
		// path — someone else's real simulator a stale meta.UDID happens to
		// point at) whose companion is doing genuine work.
		return ErrNotDisownable
	}
	for _, pid := range poison.ResiduePIDs {
		if !procs.IsIdbCompanionFor(pid, meta.UDID) {
			// Re-verify right before acting, not from any earlier
			// determination — the same rule reclaimOrphanedResidue
			// applies to its own automatic path.
			return ErrNotDisownable
		}
	}
	// Every PID gets a kill attempt regardless of whether an earlier one in
	// the loop failed — see reclaimOrphanedResidue' identical rationale;
	// an early return here would leave some already dead while reporting
	// nothing happened.
	for _, pid := range poison.ResiduePIDs {
		_ = residueKill(pid, syscall.SIGKILL)
	}
	time.Sleep(recoveryPostKillWait)
	for _, pid := range poison.ResiduePIDs {
		if procs.Alive(pid) {
			return fmt.Errorf("simpool: --disown-poisoned: companion pid %d did not exit after SIGKILL", pid)
		}
	}
	meta.UDID = ""
	return WriteMeta(dir, *meta)
}
