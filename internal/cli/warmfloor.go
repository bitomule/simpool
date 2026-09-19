package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// --warm used to be a ceiling only: it shut down free+booted simulators
// beyond N and never started the ones that were missing. So the concept of
// "keep N warm" already existed, along with the scheduled `reap` that runs
// it — what was absent was the other half.
//
// Why that half is worth having, measured on this machine: provisioning a
// slot whose simulator is already booted takes 5.9-6.2s, and provisioning
// one that is Shutdown takes 18.5s. Getting the lock is 21-79ms either way,
// so cold boot is the entire difference — about 12 seconds paid on the
// critical path of every task that happens to land on a cold slot. And
// `preboot`, which exists precisely to pay that cost ahead of time, is
// invoked by nothing: zero occurrences across the Makefiles, MUSTS.yml
// files and hooks on this machine. That is why this lives in the scheduled
// reap rather than in a hook or a Makefile target. Anything that depends on
// somebody remembering to call it has already been tried and is already
// failing.

// warmFloorBoot is simctl.BootAndWait, as a package-level var so tests can
// exercise which UDIDs this decides to boot without booting a real
// simulator — the same seam shutdownWarm, findDevice and the rest already
// use.
var warmFloorBoot = func(udid string) error {
	return simctl.BootAndWait(udid, warmFloorBootTimeout)
}

// warmFloorBootTimeout bounds one boot. Cold boot was measured at 18.5s on
// an idle machine; this is several times that because a reap pass can run
// while the machine is loaded, and a boot that is merely slow must not be
// abandoned halfway — a half-booted simulator is worse than a cold one.
const warmFloorBootTimeout = 3 * time.Minute

// warmFloorMinFreeMemory is how much of the machine's memory must be free
// before this will start a simulator nobody has asked for yet.
//
// The number comes from a real failure, not from caution in the abstract:
// Undolly's snapshot suite died seven times in one day for memory, and free
// memory at launch predicted it cleanly — 2 of 2 runs that finished were
// above 58% free, 7 of 7 that died were below. Warming is by definition
// speculative work, so it is the first thing that should give way: booting
// a simulator at the wrong moment turns somebody else's passing suite into
// a failure, and the slot it warms may never be used at all.
//
// Deliberately below that 58% observation rather than at it. This is not
// the snapshot suite and a slim slot is far cheaper (229-345MB resident
// measured idle, against ~1.75GB for a full one), so matching the suite's
// own threshold would refuse to warm on a machine that is perfectly able
// to. What it must not do is warm while memory is already tight.
const warmFloorMinFreeMemory = 0.35

// EnvWarmMinFree overrides warmFloorMinFreeMemory, as a percentage (an
// integer 0-100). It exists because the default is a judgement made from one
// machine's measurements: a machine with more memory, or one not also
// running snapshot suites, can afford to warm at a level this one cannot,
// and the opposite is just as true. A value outside 0-100, or one that does
// not parse, is ignored in favour of the default rather than treated as 0 —
// silently disabling the guard is the one failure mode that matters here.
const EnvWarmMinFree = "SIMPOOL_WARM_MIN_FREE"

// warmFloorThreshold is the free-memory floor this pass will enforce.
func warmFloorThreshold() float64 {
	return pool.MinFreeFraction(EnvWarmMinFree, warmFloorMinFreeMemory)
}

// coldWarmCandidate is a free, safe, currently SHUT DOWN slot that could be
// booted to bring its group up to the --warm floor.
type coldWarmCandidate struct {
	n    int
	meta pool.Meta
}

// classifyColdWarmSlot is classifyWarmSlot's mirror image: every one of the
// same safety checks, except that the device must be Shutdown rather than
// Booted. Shutdown is an allowlist, exactly as it is in the poison checks:
// a device that is "Booting", "Shutting Down", "Creating" or reporting an
// unrecognised state is mid-transition and must not be booted again on top
// of whatever it is already doing.
//
// Assumes dir's flock is held by the caller and never releases it.
func classifyColdWarmSlot(root, group string, n int, dir string) (pool.Meta, bool) {
	if lease, err := pool.ReadLease(dir); err != nil || lease.Alive() {
		return pool.Meta{}, false
	}
	meta := pool.ReadMeta(dir)
	if poison := pool.CheckPoison(meta); poison.Poisoned() {
		return pool.Meta{}, false
	}
	if meta.UDID == "" {
		return pool.Meta{}, false
	}
	entry, found, err := findDevice(meta.UDID)
	if err != nil || !found {
		return pool.Meta{}, false
	}
	if want := pool.DeviceNameForGroup(root, group, n); entry.Name != want || entry.State != "Shutdown" {
		return pool.Meta{}, false
	}
	return meta, true
}

// enforceWarmFloor boots free, shut-down slots until the group has warmCap
// simulators warm, so the next consumer finds one booted instead of paying
// the ~12s difference itself.
//
// Called only when enforceWarmCap has already run, so the ceiling is
// already satisfied: the two can never fight over the same slot, because
// one only ever acts on Booted devices and the other only on Shutdown ones.
//
// Same two-pass locking discipline as enforceWarmCap, and for the same
// reason: classify without holding anything across the whole group, then
// re-lock and re-classify each slot immediately before acting, so a slot
// that became busy, leased, poisoned or reassigned in between is skipped
// rather than acted on from stale state.
//
// Speculative work gives way first. If free memory is below
// warmFloorMinFreeMemory, or could not be determined at all, nothing is
// booted — and it SAYS so, with the number. An earlier silent wait in this
// same tool was investigated as a hang several times over; a warm floor
// that quietly declines to do its job would be the same defect wearing a
// different hat.
func enforceWarmFloor(root, groupDir string, warmCap int, dryRun bool, stdout, stderr io.Writer) {
	group := filepath.Base(groupDir)

	var booted int
	var cold []coldWarmCandidate
	for _, n := range pool.ListSlotNumbers(groupDir) {
		dir := pool.SlotDir(groupDir, n)
		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s/slot-%d: --warm floor: %v\n", group, n, err)
			}
			// Busy is not this pass's to touch — and it is also already
			// serving somebody, which is the thing warming exists to
			// arrange. Counted as warm for that reason.
			booted++
			continue
		}
		if _, ok := classifyWarmSlot(root, group, n, dir); ok {
			booted++
			_ = lock.Release()
			continue
		}
		meta, ok := classifyColdWarmSlot(root, group, n, dir)
		_ = lock.Release()
		if ok {
			cold = append(cold, coldWarmCandidate{n: n, meta: meta})
		}
	}

	missing := warmCap - booted
	if missing <= 0 {
		return
	}
	if len(cold) == 0 {
		return
	}

	free, ok := pool.FreeMemoryFraction()
	if !ok {
		fmt.Fprintf(stdout, "WARM  %s  not warming %d slot(s): could not read free memory, and warming is speculative work — never done on an unverified machine state\n", group, missing)
		return
	}
	if floor := warmFloorThreshold(); free < floor {
		fmt.Fprintf(stdout, "WARM  %s  not warming %d slot(s): %.0f%% memory free, below the %.0f%% floor — a simulator booted now would cost a running suite more than it saves the next one\n", group, missing, free*100, floor*100)
		return
	}

	// Most-recently-used first, the same preference the acquisition path
	// sorts by: the slot most likely to be asked for next is the one worth
	// having warm.
	sort.SliceStable(cold, func(i, j int) bool {
		return cold[i].meta.LastUsed.After(cold[j].meta.LastUsed)
	})

	for _, c := range cold {
		if missing <= 0 {
			return
		}
		label := fmt.Sprintf("%s/slot-%d", group, c.n)
		dir := pool.SlotDir(groupDir, c.n)

		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s: --warm floor: %v\n", label, err)
			}
			// It became busy, which means somebody is using it: the floor
			// is that much closer to satisfied without this pass acting.
			missing--
			continue
		}
		meta, ok := classifyColdWarmSlot(root, group, c.n, dir)
		if !ok {
			_ = lock.Release()
			continue
		}

		if dryRun {
			fmt.Fprintf(stdout, "WARM  %s  would boot %s to reach the --warm %d floor (%.0f%% memory free)\n", label, meta.UDID, warmCap, free*100)
			_ = lock.Release()
			missing--
			continue
		}
		fmt.Fprintf(stdout, "WARM  %s  booting %s to reach the --warm %d floor (%.0f%% memory free)\n", label, meta.UDID, warmCap, free*100)
		if err := warmFloorBoot(meta.UDID); err != nil {
			fmt.Fprintf(stderr, "reap %s: --warm floor: %v\n", label, err)
			_ = lock.Release()
			continue
		}
		_ = lock.Release()
		missing--

		// Re-read between boots rather than trusting the figure this pass
		// started with. Each simulator this loop starts consumes memory
		// itself, so a group several slots short could otherwise walk the
		// machine down past the floor one boot at a time while still
		// quoting the comfortable number it measured before the first one.
		free, ok = pool.FreeMemoryFraction()
		if !ok || free < warmFloorThreshold() {
			if missing > 0 {
				fmt.Fprintf(stdout, "WARM  %s  stopping short of the --warm %d floor with %d slot(s) still cold: memory is now %.0f%% free\n", group, warmCap, missing, free*100)
			}
			return
		}
	}
}
