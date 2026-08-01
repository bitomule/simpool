package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// DefaultLeaseTTL is how long `simpool lease` reserves a slot for its key
// before the reservation is considered abandoned and the slot becomes
// available again. Renewed on every call made with the same key.
const DefaultLeaseTTL = 30 * time.Minute

// Lease is a time-bounded, key-scoped reservation on a slot, for the "hot"
// MAV use case: many short, independent `mav tap`/`mav swipe`/
// `mav screenshot` processes, none of which live long enough to hold
// `with`'s flock across them. A Lease is deliberately weaker than a Lock:
// it is NOT tied to a live process and does not detect its holder crashing
// or being SIGKILLed the way the kernel-enforced flock release does — it
// only expires by wall-clock time. Never present this as equivalent to a
// flock; it isn't, on purpose (see README "MAV in the hot loop").
type Lease struct {
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Alive reports whether the lease has not yet expired. A zero-value Lease
// (Key == "") is never alive.
func (l Lease) Alive() bool {
	return l.Key != "" && time.Now().Before(l.ExpiresAt)
}

func leasePath(slotDir string) string { return filepath.Join(slotDir, "lease.json") }

// LeasePath is an exported accessor for callers (cli, tests) that only
// have a slot directory path, not a live *Slot.
func LeasePath(slotDir string) string { return leasePath(slotDir) }

// ReadLease loads lease.json for a slot. A missing or corrupt file yields
// a zero-value Lease (Alive() == false) and no error — same tolerance
// contract as ReadMeta.
func ReadLease(slotDir string) Lease {
	var l Lease
	data, err := os.ReadFile(leasePath(slotDir))
	if err != nil {
		return l
	}
	_ = json.Unmarshal(data, &l)
	return l
}

// WriteLease persists lease.json via write-to-temp + rename, so a crash
// never leaves a half-written file.
func WriteLease(slotDir string, l Lease) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := leasePath(slotDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, leasePath(slotDir))
}

// RemoveLease deletes a slot's lease.json, if any. Not an error if it's
// already gone.
func RemoveLease(slotDir string) error {
	err := os.Remove(leasePath(slotDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AcquireLease assigns a slot in the device+osVersion group under root to
// key, printing nothing itself — callers read the returned Slot's
// Meta/UDID once it has been provisioned. Sticky: a call with a key that
// already holds a live lease anywhere in the group renews that exact slot
// in place rather than picking a new one, which is what makes every
// `mav tap`/`mav swipe`/... from one repo land on the same simulator.
// Never waits for capacity — an immediate ErrAtCapacity when the group is
// full and every slot is either flock-busy or under another key's live
// lease, matching the interactive, one-shot nature of the caller (MAV's
// `target_command`).
//
// This shares the exact same critical section — the group's allocation
// lock (see withGroupAllocLock) — that AcquireSlots' take() uses for its
// own mkdir+open+flock-attempt. That is what makes a lease claim and a
// `with`/`acquire` flock-attempt on the same slot number mutually
// exclusive: whichever call's state-mutating step (TryLock succeeding, or
// WriteLease) runs first inside the lock is guaranteed visible to
// whichever call inspects that state next, so neither ever mistakes a
// slot the other has just claimed for being free.
func AcquireLease(root, device, osVersion, key string, ttl time.Duration, max int) (*Slot, error) {
	if key == "" {
		return nil, fmt.Errorf("lease key must not be empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("--ttl must be > 0")
	}
	if max < 1 {
		return nil, fmt.Errorf("--max must be >= 1")
	}

	groupDir := GroupDir(root, device, osVersion)
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		return nil, err
	}

	var found *Slot
	err := withGroupAllocLock(groupDir, func() error {
		existing := ListSlotNumbers(groupDir)

		// Sticky renewal: this key already holds a live lease somewhere in
		// the group. Refresh its TTL in place and return the same slot —
		// checked before anything else so a key's own repeated calls never
		// wander to a different slot just because another slot happened to
		// sort first.
		for _, n := range existing {
			dir := SlotDir(groupDir, n)
			lease := ReadLease(dir)
			if lease.Key == key && lease.Alive() {
				if err := WriteLease(dir, Lease{Key: key, ExpiresAt: time.Now().Add(ttl)}); err != nil {
					return err
				}
				found = leaseSlotView(root, groupDir, dir, n, device, osVersion)
				return nil
			}
		}

		resident := make(map[int]bool, len(existing))
		for _, n := range existing {
			resident[n] = true
		}

		// Most-recently-used first, same rationale as AcquireSlots: a warm
		// slot's simulator is already booted, so handing it out first skips
		// a ~30s cold boot.
		sort.SliceStable(existing, func(i, j int) bool {
			return ReadMeta(SlotDir(groupDir, existing[i])).LastUsed.
				After(ReadMeta(SlotDir(groupDir, existing[j])).LastUsed)
		})

		for _, n := range existing {
			dir := SlotDir(groupDir, n)
			ok, err := claimSlotForLease(dir, key, ttl)
			if err != nil {
				return err
			}
			if ok {
				found = leaseSlotView(root, groupDir, dir, n, device, osVersion)
				return nil
			}
		}

		next := 0
		for {
			for resident[next] {
				next++
			}
			if len(resident) >= max {
				return ErrAtCapacity
			}
			dir := SlotDir(groupDir, next)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			resident[next] = true
			ok, err := claimSlotForLease(dir, key, ttl)
			if err != nil {
				return err
			}
			if ok {
				found = leaseSlotView(root, groupDir, dir, next, device, osVersion)
				return nil
			}
			next++
		}
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// claimSlotForLease writes a fresh lease for key into dir if — and only
// if — dir is currently free for handout: its flock is uncontended, it
// carries no live lease for a different key, and (mirroring
// AcquireSlots' take()) its previous consumer isn't a poisoned orphan
// left behind by a SIGKILL to `simpool` itself. Must be called from
// inside the group allocation lock (see AcquireLease).
func claimSlotForLease(dir, key string, ttl time.Duration) (bool, error) {
	free, err := IsSlotFree(dir)
	if err != nil {
		return false, err
	}
	if !free {
		return false, nil
	}
	if lease := ReadLease(dir); lease.Alive() {
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
			return false, nil
		}
	}

	if err := WriteLease(dir, Lease{Key: key, ExpiresAt: time.Now().Add(ttl)}); err != nil {
		return false, err
	}
	return true, nil
}

func leaseSlotView(root, groupDir, dir string, n int, device, osVersion string) *Slot {
	return &Slot{
		Root:     root,
		GroupDir: groupDir,
		Dir:      dir,
		Number:   n,
		Device:   device,
		OSVer:    osVersion,
		Meta:     ReadMeta(dir),
	}
}

// ReleaseLease drops key's lease wherever it is resident across every
// device+OS group under root, returning the slot directories it cleared.
// Guarded by each group's allocation lock so a release can never race a
// concurrent claim/renewal for the same slot.
func ReleaseLease(root, key string) ([]string, error) {
	groups, err := ListGroupDirs(root)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, groupDir := range groups {
		for _, n := range ListSlotNumbers(groupDir) {
			dir := SlotDir(groupDir, n)
			var did bool
			err := withGroupAllocLock(groupDir, func() error {
				lease := ReadLease(dir)
				if lease.Key != key {
					return nil
				}
				did = true
				return RemoveLease(dir)
			})
			if err != nil {
				return released, err
			}
			if did {
				released = append(released, dir)
			}
		}
	}
	return released, nil
}

// CleanupExpiredLease removes dir's lease.json if — re-checked under the
// group allocation lock — it is still expired at the moment of removal.
// The re-check (rather than trusting an earlier, lock-free ReadLease) is
// what stops this from ever deleting a lease that a same-key `simpool
// lease` call renewed in the narrow window between an earlier read and
// this call. Callers that already hold this exact slot's own flock (reap
// does) don't strictly need this for exclusion against a *different*
// key's claim — IsSlotFree would already report busy — but a same-key
// renewal never checks the flock at all (see AcquireLease's sticky path),
// so this is what actually closes that race.
func CleanupExpiredLease(groupDir, dir string) (removed bool, err error) {
	err = withGroupAllocLock(groupDir, func() error {
		lease := ReadLease(dir)
		if lease.Key == "" || lease.Alive() {
			return nil
		}
		removed = true
		return RemoveLease(dir)
	})
	return removed, err
}
