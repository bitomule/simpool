package pool

import "fmt"

// A free-memory floor on the one branch of acquisition that adds a
// simulator to the machine.
//
// Why only that branch, and why this is not the same idea as --max:
//
// A memory floor can only ever DECLINE. A slot cap can QUEUE — the work
// waits and then runs. So a floor applied to acquisition as a whole turns a
// slow suite into a failed one, which is strictly worse than the problem it
// was meant to solve, and that is why this is not a general "refuse to
// acquire when memory is low" check.
//
// There is exactly one place where declining costs nothing, and it is the
// branch below: the pass that takes a NEW slot number, creating a slot
// directory whose simulator is then cold-booted by EnsureProvisioned. That
// branch is the one that adds ~1.75 GB to the machine (229-345 MB measured
// idle if the slot stays slim), and it is also the one that already has a
// natural fallback — every other pass of tryAcquireSlots, plus AcquireSlots'
// own poll loop, which waits for a resident slot to free up. Declining there
// converts "boot another simulator now" into "wait for one of the ones
// already booted", which is a queue and not a failure.
//
// Why a floor at all: Undolly's snapshot suite died seven times in one day
// for memory. Free memory at launch predicts it cleanly over the nine
// samples there are — 2 of 2 runs that finished started above 58% free, 7 of
// 7 that died started below. Nine samples is a trend, not a boundary, which
// is why the default sits well below that 58% observation rather than at it.

// EnvAcquireMinFree overrides acquireMinFreeMemory, as an integer percentage
// (0-100). Separate from SIMPOOL_WARM_MIN_FREE on purpose: the two floors
// answer different questions and one machine may well want them apart.
//
// The trap, and it is a real one on this machine: a launchd job inherits
// almost no environment. Anything that resolves a limit from an environment
// variable resolves a DIFFERENT limit inside `reap` than it does in a
// terminal. That matters much less here than for --max — this floor is read
// only by the acquisition path, which is never the scheduled reaper — but do
// not build anything on the assumption that a value set in a shell reaches
// every simpool process.
const EnvAcquireMinFree = "SIMPOOL_ACQUIRE_MIN_FREE"

// acquireMinFreeMemory is the free-memory fraction below which acquisition
// stops ADDING simulators to the machine and waits for an existing one
// instead.
//
// Deliberately the same 0.35 as the --warm floor, from the same nine
// measurements, and deliberately not higher than it. Warming is speculative:
// skipping it costs nothing, so it may be as cautious as it likes.
// Acquisition is real demand: declining costs a queue. A floor that refused
// real demand sooner than it refused speculation would have the two
// backwards. Equal is the honest default until somebody measures the two
// apart; the env override above is how they get separated when they do.
const acquireMinFreeMemory = 0.35

// AcquireFloorThreshold is the free-memory floor the cold-boot branch of
// acquisition will enforce.
func AcquireFloorThreshold() float64 {
	return MinFreeFraction(EnvAcquireMinFree, acquireMinFreeMemory)
}

// coldBootFloor decides whether acquisition may create a new slot and
// cold-boot it right now. It returns the reason when the answer is no, and
// "" when the answer is yes.
//
// fallbacks is how many resident slots this call could still end up on
// without creating one — either by a later pass of tryAcquireSlots, or by
// waiting for one to be released. needed is how many slots the call is still
// short of.
//
// The explicit list of what this does NOT decline, because a limit that
// cannot be released is a hang wearing a safety limit's clothes:
//
//   - Fewer usable fallbacks than the call still needs. This is the whole
//     premise: waiting is only a fallback when there is something to wait
//     for. On an empty group, or one whose remaining slots are quarantined
//     rather than busy, declining here does not queue the job — it fails it
//     after --wait, which is exactly the harm this design exists to avoid.
//     A cold group must boot, however tight memory is.
//   - Memory that could not be read at all. `reap --warm` declines on an
//     unreadable machine state and is right to: warming is speculative and
//     gives way first. Here the same reading would put every acquisition on
//     the machine into a queue because vm_stat failed once, so an
//     unverifiable measurement permits rather than refuses, and the caller
//     says so rather than pretending it checked.
func coldBootFloor(fallbacks, needed int) string {
	if fallbacks < needed {
		return ""
	}
	free, ok := FreeMemoryFraction()
	if !ok {
		return ""
	}
	if floor := AcquireFloorThreshold(); free < floor {
		return fmt.Sprintf(
			"not adding a simulator: %.0f%% memory free, below the %.0f%% floor — waiting for one of the %d resident slot(s) instead, which costs this job a queue where booting another would cost the machine ~1.75GB",
			free*100, floor*100, fallbacks)
	}
	return ""
}
