package pool

import (
	"fmt"
	"strings"
)

// --max used to be checked in exactly one place: the branch that creates
// slot number --max+1. So it bounded how many slots a group could HAVE and
// never how many were in use at once, and the two only coincided while
// `reap --max` had kept the group at its declared size. In a group that had
// drifted above the cap — the normal state, since nothing but that reap pass
// ever removes a slot — every extra slot was handed out on demand. Measured
// before this change: a three-slot group at --max 1 with one slot already
// held gave out a second in 2 ms.
//
// That is the wrong bound for what people reach for --max to do. Simulators
// are not expensive because they exist, they are expensive because they are
// running: this machine ran out of memory seven times in one day, and free
// memory at launch separated the outcomes cleanly (2 of 2 runs that finished
// started above 58% free, 7 of 7 that died started below it). Capping how
// many slots are in use at once is a real safety mechanism; capping how many
// directories exist is bookkeeping.
//
// So --max now bounds use as well, and this file is the counting half.
//
// It is NOT a second flag. A new opt-in knob would have gone the way of
// `preboot`, which exists precisely to pay cold-boot cost ahead of time and
// is invoked by nothing — zero occurrences across the Makefiles, MUSTS.yml
// files and hooks on this machine. What had to change was the flag people
// already write.

// slotUse is one slot that is currently in somebody's hands, and the
// evidence for saying so.
type slotUse struct {
	n  int
	av Availability
}

// censusInUse counts the group's slots that are currently in use by someone
// other than this caller, and returns the evidence per slot so a refusal can
// name who is holding what instead of asserting a number.
//
// Two things count as in use, and only two:
//
//   - the flock is held — a live `with`/`acquire` consumer.
//   - a live lease sits on the slot — `simpool lease`, which deliberately
//     never holds the flock (see the Lease doc comment), so it is invisible
//     to the probe above and would otherwise be free capacity.
//
// A quarantined, unverifiable or otherwise unusable slot deliberately does
// NOT count. It is not in use by anybody, and counting it would turn a
// group with `max` broken slots into a permanent queue that no amount of
// waiting clears — a hang, sold as a safety limit. Those slots still refuse
// themselves individually, one at a time, which is the honest failure and
// the one `simpool doctor` can act on.
//
// MUST be called with the group's allocation lock held (see
// withGroupAllocLock). That is what makes the probe safe as well as
// accurate: every other claim path funnels through the same lock, so no
// concurrent claimer can be sitting between opening a slot's lock file and
// flocking it while this walks the group. The TryLock here therefore never
// takes a lock out from under a claim in progress, and its immediate
// Release is invisible to any process that was not already excluded.
//
// forKey exempts a live lease belonging to that same key, which is a
// RENEWAL and not a second use. Without it the cap would close on `simpool
// lease`'s own hot loop: mav leases slot-0, comes back a second later for
// the next action, and its own still-live lease would be counted against it
// — at --max 1 the key could never renew at all, an absorbing state the
// lease path has been bitten by before (see ownLeaseResidue). Two callers
// sharing one key already share one slot by design; that is what stickiness
// means. "" exempts nothing, which is what the `with`/`acquire` path wants.
func censusInUse(groupDir string, skip map[int]bool, skipN int, forKey string) (int, []slotUse) {
	var uses []slotUse
	for _, n := range ListSlotNumbers(groupDir) {
		if n == skipN || skip[n] {
			continue
		}
		dir := SlotDir(groupDir, n)

		if lease, err := ReadLease(dir); err == nil && lease.Alive() {
			if forKey != "" && lease.Key == forKey {
				continue
			}
			uses = append(uses, slotUse{n: n, av: Availability{State: SlotLeased, Lease: lease}})
			continue
		}

		lock, err := TryLock(lockPath(dir))
		if err == ErrBusy {
			uses = append(uses, slotUse{n: n, av: Availability{State: SlotBusy}})
			continue
		}
		if err != nil {
			// An unreadable lock file is not evidence that somebody is
			// using the slot, and reading it as such is the failure mode
			// that matters here: a slot nobody can open would otherwise
			// hold a permanent share of the cap and no wait would ever
			// clear it. The slot's own claim attempt still refuses it —
			// as SlotUnverifiable, with the error — which is where that
			// belongs.
			continue
		}
		lock.Release()
	}
	return len(uses), uses
}

// claimOutcome is what a capped claim attempt decided.
type claimOutcome int

const (
	// claimTaken: the lock is held by this caller now.
	claimTaken claimOutcome = iota
	// claimBusy: somebody else holds this particular slot.
	claimBusy
	// claimOverUseCap: this slot was free, and handing it over would have
	// put the group past --max slots in use at once. Nothing about this
	// slot is wrong, so it is never reported as if something were.
	claimOverUseCap
)

// claimSlotLockCapped is claimSlotLock plus the use cap, enforced inside the
// SAME allocation lock as the flock it guards. That is the whole reason this
// is not a check in the caller: count-then-claim is only a cap if nothing can
// claim in between, and holding the group's allocation lock across both is
// what guarantees it — two racing acquirers serialize on that lock, so the
// second one's census sees the first one's flock and refuses.
//
// The lock is not extended one instruction beyond that. The census is flock
// probes over a handful of slot directories; the slow work a claim can
// trigger (a poisoned slot's recovery does a synchronous `simctl shutdown`,
// measured at ~1s) still happens after this function has released the
// allocation lock, exactly as before. That distinction was learned the hard
// way once already — see claimSlotLock's own comment.
//
// max <= 0 disables the use cap entirely and this behaves exactly like
// claimSlotLock. `mine` is the set of slot numbers this same caller already
// holds, which must not be counted against it: flock is per open file
// description, so a caller's own slots are indistinguishable from a stranger's
// from the outside, and counting them would make `--count N` fail against
// its own `--max N`.
func claimSlotLockCapped(groupDir, dir string, n, max int, mine map[int]bool, forKey string) (*Lock, claimOutcome, []slotUse, error) {
	var lock *Lock
	outcome := claimTaken
	var uses []slotUse

	err := withGroupAllocLock(groupDir, func() error {
		if max > 0 {
			inUse, census := censusInUse(groupDir, mine, n, forKey)
			if inUse+len(mine)+1 > max {
				outcome, uses = claimOverUseCap, census
				return nil
			}
		}

		if err := mkdirAllSlot(dir); err != nil {
			return err
		}
		l, err := TryLock(lockPath(dir))
		if err != nil {
			if err == ErrBusy {
				outcome = claimBusy
				return nil
			}
			return err
		}
		lock = l
		return nil
	})
	if err != nil {
		return nil, claimTaken, nil, err
	}
	return lock, outcome, uses, nil
}

// atUseCapError is atCapacityError's sibling for the other half of --max:
// the group is not out of slots, it is out of FREE ones, because --max of
// them are in somebody's hands right now.
//
// The two failures read almost identically from outside and want opposite
// responses, which is why they are not one message. "At its --max of 3
// resident slots" sends a reader to `reap`, or to raising --max, because a
// residency cap is a standing condition that does not clear by itself. This
// one clears by itself: it is a queue, and the right response is usually to
// let --wait do its job. Saying so is not politeness — until this change the
// README asserted that --max bounded concurrency while the code bounded
// residency, so a reader arriving here has a decent chance of holding the
// old idea and needs to be told which cap they hit.
//
// Wraps the same capacityError, so it wraps ErrAtCapacity: every errors.Is
// check is unaffected, and AcquireSlots' poll loop queues on this exactly as
// it queues on the residency cap — the same notice, the same 2s-then-15s
// heartbeat, the same per-slot reasons. A new refusal on a mute path of its
// own would have reintroduced the silence that heartbeat was just added to
// remove.
func atUseCapError(group, forKey string, max int, uses []slotUse) error {
	refusals := make([]SlotRefusal, 0, len(uses))
	for _, u := range uses {
		refusals = append(refusals, SlotRefusal{Number: u.n, Availability: u.av})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s already has --max (%d) slot(s) in use", group, max)
	if forKey != "" {
		fmt.Fprintf(&b, ", so none can be handed to key %q yet", forKey)
	} else {
		b.WriteString(", so this request has to wait for one of them")
	}
	b.WriteString(":")
	for _, r := range refusals {
		fmt.Fprintf(&b, "\n  slot-%d: %s", r.Number, r.Availability.State)
		if detail := r.Availability.Detail(); detail != "" {
			fmt.Fprintf(&b, " — %s", detail)
		}
	}
	b.WriteString("\nthis is a cap on slots IN USE at once, not on how many exist: the group")
	b.WriteString("\nis not oversized and `simpool reap` will not help. Wait for a holder above")
	b.WriteString("\nto finish, or raise --max/" + EnvMaxSlots + " to allow more at a time")
	return &capacityError{msg: b.String(), refusals: refusals}
}
