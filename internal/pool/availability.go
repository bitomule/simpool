package pool

import (
	"fmt"
	"strings"
)

// SlotState is the single verdict on whether a slot can be handed to a new
// consumer right now. It exists because there used to be two answers to
// that question and they disagreed: `simpool status` printed a slot "free"
// on the strength of its flock alone, while the acquisition paths
// additionally consulted lease.json and CheckPoison and refused the very
// same slot. A caller who was told to "run `simpool status` to see who
// holds them" was then shown a pool with nothing wrong in it — the report
// that led to this type (see README "Why a slot can be refused").
//
// One computation, two consumers: SlotAvailability (reporting: status,
// doctor) and slotAvailabilityLocked (deciding: claimSlotForLease,
// tryAcquireSlots' take). Neither may re-derive availability on its own.
type SlotState int

const (
	// SlotFree: nothing stands between this slot and a new consumer.
	SlotFree SlotState = iota
	// SlotBusy: the slot's flock is held — a live `with`/`acquire`.
	SlotBusy
	// SlotLeased: a live `simpool lease` reservation belonging to some
	// OTHER key (a key's own live lease is a renewal, not an obstacle).
	SlotLeased
	// SlotQuarantined: the flock is free and no live lease holds it, but
	// CheckPoison found a still-alive previous consumer (or a check that
	// could not complete) — see Poison.
	SlotQuarantined
	// SlotUnverifiable: a check itself failed (lease.json unreadable, the
	// lock file unopenable). Never read as free; see ReadLease.
	SlotUnverifiable
)

func (s SlotState) String() string {
	switch s {
	case SlotFree:
		return "free"
	case SlotBusy:
		return "busy"
	case SlotLeased:
		return "leased"
	case SlotQuarantined:
		return "quarantined"
	case SlotUnverifiable:
		return "unverifiable"
	default:
		return "unknown"
	}
}

// Availability is SlotState plus the evidence behind it, so every caller
// can say WHY a slot was refused instead of guessing (`simpool lease`'s
// error used to assert "every slot is busy or leased elsewhere" about
// slots that were neither).
type Availability struct {
	State SlotState
	// Lease is the slot's lease as read, alive or expired; zero if none.
	Lease Lease
	// Poison is CheckPoison's verdict. Set whenever the check ran, even
	// when it did not decide the state — a slot can be poisoned AND
	// exempt (see OwnLeaseResidue).
	Poison Poison
	// Meta is the slot's bookkeeping as read (advisory, may be zero).
	Meta Meta
	// OwnLeaseResidue is true when the live processes CheckPoison found are
	// the residue of one lease key's own previous session on this slot.
	// For that key it means the poison was disregarded and State is
	// SlotFree; for a keyless viewer (`status`) State stays SlotQuarantined
	// — nobody else may have the slot — and this flag is what lets the
	// report name the one key that can still claim it, instead of sending
	// the reader looking for a holder that does not exist.
	OwnLeaseResidue bool
	// Err is set only when State == SlotUnverifiable.
	Err error
}

// Detail is a one-line, human-readable explanation of State, suitable for
// a `status` column or a line in `lease`'s refusal message. Empty for a
// plainly free slot.
func (a Availability) Detail() string {
	switch a.State {
	case SlotFree:
		if a.OwnLeaseResidue {
			return fmt.Sprintf("residue of its own lease key %q (%s)", a.leaseOwner(), a.Poison)
		}
		return ""
	case SlotBusy:
		return "flock held by a live `with`/`acquire`"
	case SlotLeased:
		return fmt.Sprintf("leased by %q", a.Lease.Key)
	case SlotQuarantined:
		if a.OwnLeaseResidue {
			return fmt.Sprintf("%s; its own lease key %q can still claim it", a.Poison, a.leaseOwner())
		}
		return a.Poison.String()
	case SlotUnverifiable:
		return fmt.Sprintf("could not verify: %v", a.Err)
	default:
		return ""
	}
}

// leaseOwner is the key whose session this slot's residue belongs to: the
// lease it still carries if it has one, else the last lease key its meta
// recorded (a `simpool release` removes lease.json but not that).
func (a Availability) leaseOwner() string {
	if a.Lease.Key != "" {
		return a.Lease.Key
	}
	return a.Meta.LeaseKey
}

// SlotAvailability computes, for a caller holding no locks and mutating
// nothing, the same verdict the acquisition paths reach — the reporting
// half of this file's contract (see SlotState). forKey is the lease key
// the caller would claim with, or "" for a keyless viewer (`status`,
// `doctor`), which asks the general question: could ANY caller take this
// slot as it stands?
//
// A keyless view still reports OwnLeaseResidue, so a slot that is
// quarantined for everyone else while remaining claimable by the key that
// left the residue is shown as exactly that, rather than as a flat "free"
// (which is what sent the original report chasing a pool with nothing
// visibly wrong in it) or a flat "quarantined" (which would be a second,
// opposite lie to the one key that can still use it).
func SlotAvailability(dir, forKey string) Availability {
	free, err := IsSlotFree(dir)
	if err != nil {
		return Availability{State: SlotUnverifiable, Err: err, Meta: ReadMeta(dir)}
	}
	if !free {
		return Availability{State: SlotBusy, Meta: ReadMeta(dir)}
	}
	return slotAvailabilityLocked(dir, forKey)
}

// slotAvailabilityLocked is SlotAvailability minus the flock check: the
// deciding half of the contract, for callers that already hold the slot's
// own flock (claimSlotForLease, take) and therefore already know the
// answer to that part. It never mutates and never attempts recovery —
// a SlotQuarantined verdict is the caller's cue to try AttemptRecovery
// under the exclusion its own flock provides.
func slotAvailabilityLocked(dir, forKey string) Availability {
	lease, err := ReadLease(dir)
	if err != nil {
		// An unreadable lease.json is never "no lease" — see ReadLease.
		return Availability{State: SlotUnverifiable, Err: err, Meta: ReadMeta(dir)}
	}
	meta := ReadMeta(dir)
	if lease.Alive() && lease.Key != forKey {
		return Availability{State: SlotLeased, Lease: lease, Meta: meta}
	}

	poison := CheckPoison(meta)
	av := Availability{State: SlotFree, Lease: lease, Meta: meta, Poison: poison}
	if !poison.Poisoned() {
		return av
	}
	if ownLeaseResidue(meta, lease, poison, forKey) {
		av.OwnLeaseResidue = true
		return av
	}
	av.State = SlotQuarantined
	// A keyless viewer (`status`) is not exempt from anything — it speaks
	// for no key — but it is exactly who needs to be told that this
	// quarantine has an owner who can still claim it, or the report is
	// "quarantined, go find the holder" about a slot whose own key would
	// have been handed it on the spot.
	if forKey == "" && ownLeaseResidue(meta, lease, poison, av.leaseOwner()) {
		av.OwnLeaseResidue = true
	}
	return av
}

// ownLeaseResidue reports whether a poison verdict is nothing but the
// live remains of forKey's OWN previous lease session on this same slot —
// the case that made `simpool lease` unusable for the workload it exists
// for, and the reason this predicate exists.
//
// The failure it fixes, reproduced end to end: a MAV hot loop leases
// slot-0, and the tools it drives leave processes carrying that slot's
// UDID in their own argv (`simctl spawn <udid> log stream`, `axe
// describe-ui --udid <udid>`, the booted app itself). CheckPoison sees
// them as PoisonedByLiveConsumers, which is never a kill candidate and
// never recoverable — correctly, since simpool did not spawn them. So the
// slot was quarantined against every caller, INCLUDING the very key whose
// session those processes belong to, the moment its lease lapsed. With
// --max 1 (a deliberate disk bound: ~1.75GB per resident slot) there was
// no second slot to fall back to, so the key could never lease again at
// all, and `simpool release` — which does exactly what it claims, removing
// lease.json — changed nothing about the refusal, because the lease was
// never what was blocking it.
//
// The exemption is deliberately narrow:
//
//   - Only for a slot last held in "lease" mode. A `with`/`acquire`
//     consumer's residue means something that held the flock died; that
//     stays quarantined.
//   - Only for a NON-EMPTY key that matches the slot's own recorded lease
//     key: the lease it still carries (expired or not), or, once `simpool
//     release` has removed lease.json, Meta.LeaseKey. A keyless viewer
//     (`status`) never matches, and neither does a different key.
//   - Only for the two consumer-residue reasons. PoisonedByConsumerPGID
//     (a `with`-spawned process group still alive) and
//     PoisonedByCheckFailure (a check that did not complete) are never
//     exempt: the first is a real orphan with its own recovery path, and
//     the second is an inability to check, which this codebase never reads
//     as "free" for anyone.
//
// What it cannot do is put two DIFFERENT consumers on one simulator, which
// is the harm the quarantine exists to prevent: the only caller it ever
// lets through is the one whose own session left the processes behind.
// Two concurrent callers sharing one key are already sharing one slot by
// design — that is what stickiness means (see AcquireLease).
// SlotRefusal is one slot an acquisition path could not use, and why.
// Collected as the path walks the group so the failure can name the actual
// obstacle per slot instead of asserting one blanket reason for all of
// them.
type SlotRefusal struct {
	Number       int
	Availability Availability
}

// atCapacityError is the single failure both acquisition paths return when
// a group has run out of usable slots and `max` forbids adding another.
//
// It still wraps ErrAtCapacity (callers branch on that — see RunPreboot),
// and "at capacity" is the accurate half of what used to be reported: the
// group really is at its cap. What was NOT accurate, and is what this
// function exists to fix, is the rest of the old message — "every slot is
// busy or leased elsewhere" — asserted about slots that were frequently
// neither, then followed by "run `simpool status` to see who holds them",
// pointing at a view that showed those same slots free (see SlotState).
// Every refusal now carries the reason that slot was actually refused.
//
// forKey is the lease key the refused claim was for, or "" on the
// `with`/`acquire` path, which has no key.
func atCapacityError(group, forKey string, max int, refusals []SlotRefusal) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is at its --max of %d resident slot(s)", group, max)
	if forKey != "" {
		fmt.Fprintf(&b, " and none of them can be handed to key %q", forKey)
	} else {
		b.WriteString(" and none of them can be handed out")
	}
	b.WriteString(":")
	for _, r := range refusals {
		fmt.Fprintf(&b, "\n  slot-%d: %s", r.Number, r.Availability.State)
		if detail := r.Availability.Detail(); detail != "" {
			fmt.Fprintf(&b, " — %s", detail)
		}
	}
	b.WriteString("\nraise --max/" + EnvMaxSlots + " to allow another resident slot, or clear what holds the ones above (`simpool doctor`, `simpool reap`)")
	return fmt.Errorf("%w: %s", ErrAtCapacity, b.String())
}

func ownLeaseResidue(meta Meta, lease Lease, poison Poison, forKey string) bool {
	if forKey == "" || meta.Mode != "lease" {
		return false
	}
	switch poison.Reason {
	case PoisonedByLiveConsumers, PoisonedByOrphanedResidue:
	default:
		return false
	}
	if lease.Key != "" {
		return lease.Key == forKey
	}
	return meta.LeaseKey == forKey
}
