package pool

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"
)

// DefaultMaxSlotsPerGroup caps how many simulators of the same device+OS
// pair simpool will ever have resident at once. Measured cost is ~1.75GB
// booted per slot (design doc §3); on a machine with only a few GB of spare
// budget, an unbounded pool turns ordinary contention into the exact jetsam
// scenario simpool exists to prevent. Override with SIMPOOL_MAX_SLOTS.
const DefaultMaxSlotsPerGroup = 3

// EnvMaxSlots overrides DefaultMaxSlotsPerGroup.
const EnvMaxSlots = "SIMPOOL_MAX_SLOTS"

// ErrAtCapacity is returned by AcquireSlots when a device+OS group already
// has `max` slots, all busy, and none freed up within the wait window.
var ErrAtCapacity = errors.New("simpool: pool at capacity")

const acquirePollInterval = 2 * time.Second

// MaxSlotsPerGroup resolves the effective per-group slot cap: SIMPOOL_MAX_SLOTS
// if set to a positive integer, else DefaultMaxSlotsPerGroup.
func MaxSlotsPerGroup() int {
	if v := os.Getenv(EnvMaxSlots); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxSlotsPerGroup
}

// Slot is one locked (or lockable) unit of the pool: a device set plus its
// flock and informational metadata.
type Slot struct {
	Root     string
	GroupDir string
	Dir      string
	Number   int
	Device   string
	OSVer    string

	lock *Lock
	Meta Meta
}

// DeviceName is the deterministic, pool-wide-unique name of this slot's
// simulator in the (shared, default) device set. See pool.DeviceName.
func (s *Slot) DeviceName() string { return DeviceName(s.Root, s.Device, s.OSVer, s.Number) }

// LockPath is this slot's lock file — the pool's single source of truth.
func (s *Slot) LockPath() string { return lockPath(s.Dir) }

// MetaPath is this slot's informational, non-authoritative metadata file.
func (s *Slot) MetaPath() string { return metaPath(s.Dir) }

// SaveMeta best-effort persists s.Meta to disk.
func (s *Slot) SaveMeta() error { return WriteMeta(s.Dir, s.Meta) }

// LoadMeta refreshes s.Meta from disk.
func (s *Slot) LoadMeta() { s.Meta = ReadMeta(s.Dir) }

// Release drops this slot's lock. After this call the slot may be picked
// up by any other process; the caller must not touch its simulator again.
// It also stamps Meta.LastUsed with the actual release time (best-effort)
// so `reap --cold` measures idleness since the slot was freed, not since it
// was first provisioned at the start of a long-running job.
func (s *Slot) Release() error {
	if s.lock == nil {
		return nil
	}
	s.Meta.LastUsed = time.Now()
	_ = s.SaveMeta()
	err := s.lock.Release()
	s.lock = nil
	return err
}

// AcquireSlots locks `count` slots in the device+osVersion group under
// root. It creates new slot directories as needed but never more than
// `max` total for the group — beyond that it polls (every 2s) for a slot to
// free up until waitTimeout elapses, at which point it returns
// ErrAtCapacity. waitTimeout <= 0 means fail immediately instead of
// polling. On any error, all slots acquired so far in this call are
// released before returning.
//
// Directory creation races between concurrent simpool processes are
// harmless: mkdir merely proposes a slot number, flock() is what actually
// decides ownership, so two processes racing to create slot-N just means
// one of them wins the mkdir and both then race fairly for the lock.
func AcquireSlots(root, device, osVersion string, count, max int, waitTimeout time.Duration) ([]*Slot, error) {
	if count < 1 {
		return nil, fmt.Errorf("count must be >= 1, got %d", count)
	}
	if max < count {
		return nil, fmt.Errorf("--max (%d) must be >= --count (%d)", max, count)
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		slots, err := tryAcquireSlots(root, device, osVersion, count, max)
		if !errors.Is(err, ErrAtCapacity) {
			return slots, err
		}
		if waitTimeout <= 0 || time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s has %d slot(s), all busy — retry later or raise --max/%s", ErrAtCapacity, GroupName(device, osVersion), max, EnvMaxSlots)
		}
		time.Sleep(acquirePollInterval)
	}
}

func tryAcquireSlots(root, device, osVersion string, count, max int) ([]*Slot, error) {
	groupDir := GroupDir(root, device, osVersion)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return nil, err
	}

	var acquired []*Slot
	release := func() {
		for _, s := range acquired {
			s.Release()
		}
	}

	// resident tracks every slot number that has (or, by the end of this
	// call, will have) a directory under groupDir — free, busy, purged, or
	// mid-creation, it doesn't matter: a directory existing at all is what
	// costs a slot number against `max`. Seeded from disk once and kept in
	// sync locally as this call creates new ones, so `max` is enforced
	// against the group's actual footprint instead of the previous
	// highest-numbered slot ever created (which undercounted after a purge
	// freed up a low-numbered gap, and overcounted a group that simply
	// hasn't used its low numbers yet).
	existing := ListSlotNumbers(groupDir)
	resident := make(map[int]bool, len(existing))
	for _, n := range existing {
		resident[n] = true
	}

	take := func(n int) (bool, error) {
		dir := SlotDir(groupDir, n)

		lock, err := claimSlotLock(groupDir, dir)
		if err != nil {
			return false, err
		}
		if lock == nil {
			return false, nil // busy
		}

		if lease := ReadLease(dir); lease.Alive() {
			// The flock was free, but the slot is currently reserved by a
			// `simpool lease` holder (see lease.go) — a lease deliberately
			// never holds the flock, so `with`/`acquire` must consult
			// lease.json explicitly or they would silently steal a slot
			// out from under an active MAV hot-loop session. Safe to check
			// here, outside the allocation lock: our TryLock above already
			// succeeded inside it, so no concurrent AcquireLease call can
			// write a *new* lease for this slot from this point on (its own
			// claimSlotForLease would see the flock we now hold as busy) —
			// only a lease written before we got here can possibly be
			// found, and the allocation lock's ordering guarantees we'd see
			// it.
			lock.Release()
			return false, nil
		}

		meta := ReadMeta(dir)
		if poison := CheckPoison(meta); poison.Poisoned() {
			// The previous holder died abruptly (SIGKILL to simpool itself,
			// not its process group — design doc §4's one accepted failure
			// window) and its consumer may still be running even though the
			// flock is free. Handing this slot to a new owner as-is would
			// put two consumers on one simulator, exactly what simpool
			// exists to prevent — so try to reclaim it first: only ever
			// kills a `with`-spawned process group whose recorded identity
			// (its own start time, fingerprinted under a fixed,
			// locale/timezone-independent environment — see
			// procs.ProcessStartTime) still matches a currently-alive
			// leader process (see AttemptRecovery); never based on
			// LiveConsumers alone, and never when the liveness check itself
			// failed to complete (PoisonedByCheckFailure).
			if !AttemptRecovery(root, dir, n, GroupName(device, osVersion), &meta, poison) {
				// Couldn't verify identity (or the kill didn't stick, or
				// the check itself failed) — quarantine exactly as before
				// this feature existed and skip to the next candidate slot.
				lock.Release()
				return false, nil
			}
		}

		s := &Slot{
			Root:     root,
			GroupDir: groupDir,
			Dir:      dir,
			Number:   n,
			Device:   device,
			OSVer:    osVersion,
			lock:     lock,
			Meta:     meta,
		}
		acquired = append(acquired, s)
		return true, nil
	}

	// Most-recently-used first. A warm slot still has its simulator booted and
	// the consumer's app installed, so reusing it skips a ~30s boot and a
	// reinstall; walking slot-0 upward instead would hand out a cold slot while
	// a warm one sat idle. Busy slots are skipped by the flock either way, so
	// this only changes which *free* slot wins.
	sort.SliceStable(existing, func(i, j int) bool {
		return ReadMeta(SlotDir(groupDir, existing[i])).LastUsed.
			After(ReadMeta(SlotDir(groupDir, existing[j])).LastUsed)
	})

	for _, n := range existing {
		if len(acquired) >= count {
			break
		}
		ok, err := take(n)
		if err != nil {
			release()
			return nil, err
		}
		_ = ok
	}

	next := 0
	for len(acquired) < count {
		for resident[next] {
			next++
		}
		if len(resident) >= max {
			release()
			return nil, ErrAtCapacity
		}
		ok, err := take(next)
		if err != nil {
			release()
			return nil, err
		}
		resident[next] = true
		next++
		if !ok {
			continue
		}
	}

	return acquired, nil
}

// withGroupAllocLock serializes structural mutations to a group's slot
// directories — creating a brand-new one, or removing a purged one (see
// RemoveSlotDir) — under a single, group-wide lock file distinct from any
// individual slot's own lock. See allocLockPath for why this exists.
func withGroupAllocLock(groupDir string, fn func() error) error {
	l, err := lockBlocking(allocLockPath(groupDir))
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

// claimSlotLock ensures dir exists and returns an exclusive lock on its
// lock file, or (nil, nil) if it's currently busy — never an error for
// that case. The mkdir+open+flock-attempt runs inside the group's short-
// lived allocation lock (see allocLockPath), which is released again
// before this returns: it is serialized against reap's RemoveAll of a
// purged slot directory (see RemoveSlotDir) — without that, a process that
// has opened but not yet flocked a slot's lock file could end up holding a
// flock on an inode reap has since unlinked, while a third process creates
// a brand-new lock file for the same slot number — two holders, one slot.
//
// Deliberately scoped to just that brief structural step, not to whatever
// the caller does with the returned lock afterward: holding the group-wide
// allocation lock across a caller's own, potentially slow follow-up work
// (a poisoned-slot recovery's synchronous `simctl shutdown`, measured at
// ~1s) used to mean one stuck recovery blocked every *other* acquisition
// attempt in the same group — not just for the slot in question, but the
// mkdir+TryLock step of any concurrent take()/claimSlotForLease call for
// any slot number, since they all funnel through this same allocation
// lock. See AcquireLease's history (lease.go) for where that used to bite.
func claimSlotLock(groupDir, dir string) (*Lock, error) {
	var lock *Lock
	busy := false
	err := withGroupAllocLock(groupDir, func() error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		l, err := TryLock(lockPath(dir))
		if err != nil {
			if err == ErrBusy {
				busy = true
				return nil
			}
			return err
		}
		lock = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, nil
	}
	return lock, nil
}

// RemoveSlotDir deletes a slot directory in its entirety. Callers (reap)
// must hold that slot's own lock across this call — this only additionally
// serializes against AcquireSlots' take() via the group allocation lock, so
// a slot's lock file is never unlinked in the window after some other
// process has opened it but before it has flocked it (see
// allocLockPath/withGroupAllocLock).
func RemoveSlotDir(groupDir, dir string) error {
	return withGroupAllocLock(groupDir, func() error {
		return os.RemoveAll(dir)
	})
}
