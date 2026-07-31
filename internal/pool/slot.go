package pool

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/bitomule/simpool/internal/procs"
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

		// The mkdir+open+flock-attempt is serialized against reap's
		// RemoveAll of a purged slot directory via a short-lived,
		// group-wide lock (see RemoveSlotDir): without this, a process that
		// has opened but not yet flocked a slot's lock file could end up
		// holding a flock on an inode reap has since unlinked, while a
		// third process creates a brand-new lock file for the same slot
		// number — two holders, one slot.
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
			return false, err
		}
		if busy {
			return false, nil
		}

		meta := ReadMeta(dir)
		if meta.UDID != "" {
			poisoned := meta.ConsumerPGID != 0 && procs.PGIDAlive(meta.ConsumerPGID)
			if !poisoned {
				if live, _ := procs.LiveConsumers(meta.UDID); len(live) > 0 {
					poisoned = true
				}
			}
			if poisoned {
				// The previous holder died abruptly (SIGKILL to simpool
				// itself, not its process group — design doc §4's one
				// accepted failure window) and its consumer is still
				// running even though the flock is free. Handing this slot
				// to a new owner would put two consumers on one simulator,
				// exactly what simpool exists to prevent. `simpool reap`
				// is what cleans this up; skip to the next candidate slot.
				// ConsumerPGID is checked first because it catches a
				// consumer whose only distinguishing trace is an
				// environment variable (MAV_TARGET_UDID, SIMPOOL_UDID_0)
				// rather than anything in its argv, which LiveConsumers
				// (pgrep -f <udid>) cannot see at all; LiveConsumers remains
				// as a second signal for slots with no recorded PGID (e.g.
				// `acquire` mode, or meta.json predating this field).
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
