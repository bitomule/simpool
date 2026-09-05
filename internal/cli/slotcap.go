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

// shutdownExcess and deleteExcess are package-level vars, not direct simctl
// calls, so tests can exercise enforceSlotCap's decision logic without ever
// touching a real simulator — the same seam listPoolDevices/shutdownOrphan/
// deleteOrphan/shutdownWarm/findDevice already use.
var shutdownExcess = simctl.Shutdown
var deleteExcess = simctl.Delete

// slotCapGrace is how long a slot must have been idle before the --max
// enforcement pass will delete it for being over the cap.
//
// Why a grace period at all, when the slot is already free, unleased and
// unpoisoned: --max is resolved independently by every process that reads
// it (SIMPOOL_MAX_SLOTS or a --max flag), and `reap` usually runs from
// launchd, which inherits almost no environment. A reaper that resolved a
// SMALLER --max than the acquirers around it would otherwise delete healthy
// simulators out from under a group that is actively cycling through them,
// several times an hour, turning a misconfiguration into a permanent cold
// boot. Requiring the excess to have been untouched for half an hour means
// that failure mode is loud (the CAP lines report every skip) and slow
// rather than silent and immediate, while a group that genuinely overshot
// and went quiet — the case this pass exists for — still gets collected on
// the very next run.
const slotCapGrace = 30 * time.Minute

// capCandidate is one slot considered by enforceSlotCap. A candidate with a
// non-empty blocked reason is never evicted; it still occupies one of the
// group's `max` places, because the cap can only ever be enforced against
// what is genuinely idle.
type capCandidate struct {
	n       int
	meta    pool.Meta
	entry   simctl.DeviceEntry
	exists  bool
	blocked string
}

// enforceSlotCap brings a device+OS group back down to `max` slots by
// deleting the excess ones outright — simulator and slot directory both.
//
// This is the other half of `max`, and the half that was missing. Every
// other enforcement point (AcquireSlots' and AcquireLease's `len(resident)
// >= max` checks) only ever refuses to CREATE slot number max+1; nothing
// anywhere ever removed one. That makes `max` a one-way ratchet: any moment
// a group is allowed past the cap — an operator raising --max or
// SIMPOOL_MAX_SLOTS once to get past an ErrAtCapacity, a policy change that
// lowers the default later — is permanent, because the extra slot
// directories survive, every acquisition happily reuses them, and `reap`'s
// only device-deleting pass (--purge) is keyed on idle time, not on the
// cap, and is deliberately off by default. On the machine this was written
// for, one group sat at seven slots against a cap of three for seventeen
// days: four simulators that should never have existed, 2.4 GB each, on a
// disk with 6.9 GB left.
//
// The eviction order is deliberate. Slots that cannot be touched — held,
// leased, quarantined, unverifiable, or simply used too recently (see
// slotCapGrace) — are counted as kept first, since the pass has no
// authority over them; the remaining places go to the most-recently-used
// free slots, so what gets deleted is always the coldest end of the group.
// If the untouchable slots alone already meet or exceed `max`, every free
// slot beyond them is over the cap and goes: that is what `max` means.
//
// Locking mirrors enforceWarmCap exactly: the classification pass takes,
// reads and releases one slot's flock at a time (never accumulates them,
// which would make a concurrent `simpool lease` — which never waits for
// capacity — see the whole group as busy), and each slot actually selected
// for eviction is locked a second time with the full classification re-run
// under that second lock, so a slot that became busy, leased, poisoned or
// was reassigned in between is skipped rather than acted on. That lock is
// held across pool.RemoveSlotDir for the same reason reapSlot's --purge
// path holds it: RemoveSlotDir additionally serializes against
// AcquireSlots' take() through the group allocation lock, closing the
// window where another process has opened this slot's lock file but not yet
// flocked it.
func enforceSlotCap(root, groupDir string, max int, dryRun bool, stdout, stderr io.Writer) {
	group := filepath.Base(groupDir)
	nums := pool.ListSlotNumbers(groupDir)
	if len(nums) <= max {
		return
	}

	var blocked, free []capCandidate
	for _, n := range nums {
		dir := pool.SlotDir(groupDir, n)
		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s/slot-%d: --max: %v\n", group, n, err)
			}
			blocked = append(blocked, capCandidate{n: n, blocked: "held by a live consumer"})
			continue
		}
		c := classifyCapSlot(root, group, n, dir)
		_ = lock.Release()
		if c.blocked != "" {
			blocked = append(blocked, c)
			continue
		}
		free = append(free, c)
	}

	sort.SliceStable(free, func(i, j int) bool {
		return free[i].meta.LastUsed.After(free[j].meta.LastUsed)
	})

	keep := max - len(blocked)
	if keep < 0 {
		keep = 0
	}
	if len(free) <= keep {
		// Everything over the cap is untouchable right now. Say so once,
		// naming each one, rather than silently doing nothing every half
		// hour while the group stays oversized.
		for _, c := range blocked {
			fmt.Fprintf(stdout, "CAP   %s/slot-%d  group has %d slot(s) over --max %d but this one is not evictable: %s\n", group, c.n, len(nums), max, c.blocked)
		}
		return
	}

	for i, c := range free {
		if i < keep {
			continue
		}
		label := fmt.Sprintf("%s/slot-%d", group, c.n)
		dir := pool.SlotDir(groupDir, c.n)

		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s: --max: %v\n", label, err)
			}
			continue // became busy since classification — not this pass's to touch
		}
		re := classifyCapSlot(root, group, c.n, dir)
		if re.blocked != "" {
			fmt.Fprintf(stdout, "CAP   %s  skipped: %s\n", label, re.blocked)
			_ = lock.Release()
			continue
		}

		if dryRun {
			fmt.Fprintf(stdout, "CAP   %s  would delete %s and its slot directory: group has %d slots, --max %d\n", label, describeCapDevice(re), len(nums), max)
			_ = lock.Release()
			continue
		}
		fmt.Fprintf(stdout, "CAP   %s  deleting %s and its slot directory: group has %d slots, --max %d\n", label, describeCapDevice(re), len(nums), max)

		if re.exists {
			if re.entry.State != "Shutdown" {
				if err := shutdownExcess(re.meta.UDID); err != nil {
					fmt.Fprintf(stderr, "reap %s: --max: shutting down %s: %v\n", label, re.meta.UDID, err)
					_ = lock.Release()
					continue
				}
			}
			if err := deleteExcess(re.meta.UDID); err != nil {
				fmt.Fprintf(stderr, "reap %s: --max: deleting %s: %v\n", label, re.meta.UDID, err)
				_ = lock.Release()
				continue
			}
		}
		if err := pool.RemoveSlotDir(groupDir, dir); err != nil {
			fmt.Fprintf(stderr, "reap %s: --max: removing slot directory: %v\n", label, err)
		}
		_ = lock.Release()
	}
}

func describeCapDevice(c capCandidate) string {
	if !c.exists {
		if c.meta.UDID == "" {
			return "no device (never provisioned)"
		}
		return "already-missing device " + c.meta.UDID
	}
	return "device " + c.meta.UDID
}

// classifyCapSlot applies every safety check a slot must pass before the
// --max pass may delete it. Assumes dir's flock is already held by the
// caller; never releases it. A non-empty blocked reason means "this slot
// still counts against the cap but must not be touched" — the same
// fail-safe rule the rest of reap applies: an unreadable lease.json, a
// poison check that did not complete, or a device-state lookup that failed
// all read as occupied, never as free.
//
// The device-identity check is the same one reapSlot's own --purge path
// makes, and for the same reason: this is a pass with the power to DELETE a
// simulator, so it verifies the device's actual name in the default set
// against pool.DeviceNameForGroup — derived from the slot's own directory,
// never from meta.Device/meta.OSVersion, which cannot be its own witness —
// rather than trusting a UDID out of a file that is explicitly allowed to
// be stale. A slot whose device is already gone, or that was never
// provisioned at all, is still evictable: there is nothing to delete but
// the directory, and the directory is exactly what costs a slot number.
func classifyCapSlot(root, group string, n int, dir string) capCandidate {
	c := capCandidate{n: n}

	lease, err := pool.ReadLease(dir)
	if err != nil {
		c.blocked = fmt.Sprintf("lease.json could not be read (%v) — treating as occupied", err)
		return c
	}
	if lease.Alive() {
		c.blocked = fmt.Sprintf("active lease (key %q, expires in %s)", lease.Key, time.Until(lease.ExpiresAt).Round(time.Second))
		return c
	}

	c.meta = pool.ReadMeta(dir)
	if poison := pool.CheckPoison(c.meta); poison.Poisoned() {
		c.blocked = fmt.Sprintf("quarantined (%s)", poison)
		return c
	}
	if !c.meta.LastUsed.IsZero() {
		if idle := time.Since(c.meta.LastUsed); idle < slotCapGrace {
			c.blocked = fmt.Sprintf("used %s ago (< %s grace)", idle.Round(time.Second), slotCapGrace)
			return c
		}
	}
	if c.meta.UDID == "" {
		// meta.json is advisory and can be lost entirely (crash mid-write,
		// disk full, a human `rm`) without the simulator itself going
		// anywhere — ensureProvisioned's own by-name recovery path exists
		// for exactly this. Blindly treating this as "never provisioned"
		// would delete only the directory and free the slot number for
		// reuse, orphaning a real, multi-gigabyte simulator that the
		// scheduled --max pass was supposed to reclaim, and that nothing
		// else in simpool will ever find again without an opt-in `reap
		// --orphans/--purge-orphans`. So look for a device already sitting
		// in the default set under this slot's own deterministic name
		// before assuming there is nothing to delete.
		devices, err := listPoolDevices()
		if err != nil {
			c.blocked = fmt.Sprintf("device set could not be checked (%v)", err)
			return c
		}
		want := pool.DeviceNameForGroup(root, group, n)
		var matches []simctl.DeviceEntry
		for _, d := range devices {
			if d.Name == want {
				matches = append(matches, d)
			}
		}
		switch len(matches) {
		case 0:
			return c // truly never provisioned: only a directory to remove
		case 1:
			c.meta.UDID = matches[0].UDID
			c.entry = matches[0]
			c.exists = true
			return c
		default:
			// simctl does not enforce unique names; refuse to guess which
			// one is this slot's, mirroring ensureProvisioned's own
			// refuse-to-guess branch.
			c.blocked = fmt.Sprintf("%d devices named %q in the default set — refusing to guess which is this slot's", len(matches), want)
			return c
		}
	}

	entry, found, err := findDevice(c.meta.UDID)
	if err != nil {
		c.blocked = fmt.Sprintf("device state could not be checked (%v)", err)
		return c
	}
	if !found {
		return c // device already gone: only a directory to remove
	}
	if want := pool.DeviceNameForGroup(root, group, n); entry.Name != want {
		c.blocked = fmt.Sprintf("meta references device %s named %q, expected %q — not this slot's device", c.meta.UDID, entry.Name, want)
		return c
	}
	c.entry = entry
	c.exists = true
	return c
}
