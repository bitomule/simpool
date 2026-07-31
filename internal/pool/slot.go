package pool

import (
	"fmt"
	"os"
)

// Slot is one locked (or lockable) unit of the pool: a device set plus its
// flock and informational metadata.
type Slot struct {
	GroupDir string
	Dir      string
	Number   int
	Device   string
	OSVer    string

	lock *Lock
	Meta Meta
}

// SetDir is where this slot's private `xcrun simctl --set` device set
// lives.
func (s *Slot) SetDir() string { return setPath(s.Dir) }

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
func (s *Slot) Release() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Release()
	s.lock = nil
	return err
}

// AcquireSlots locks `count` slots in the device+osVersion group under
// root, creating new slot directories as needed. It never blocks: slots
// already held by another process are skipped in favor of the next
// candidate. On any error, all slots acquired so far in this call are
// released before returning.
//
// Directory creation races between concurrent simpool processes are
// harmless: mkdir merely proposes a slot number, flock() is what actually
// decides ownership, so two processes racing to create slot-N just means
// one of them wins the mkdir and both then race fairly for the lock.
func AcquireSlots(root, device, osVersion string, count int) ([]*Slot, error) {
	if count < 1 {
		return nil, fmt.Errorf("count must be >= 1, got %d", count)
	}
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

	take := func(n int) (bool, error) {
		dir := SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		lock, err := TryLock(lockPath(dir))
		if err != nil {
			if err == ErrBusy {
				return false, nil
			}
			return false, err
		}
		if err := os.MkdirAll(setPath(dir), 0o755); err != nil {
			lock.Release()
			return false, err
		}
		s := &Slot{
			GroupDir: groupDir,
			Dir:      dir,
			Number:   n,
			Device:   device,
			OSVer:    osVersion,
			lock:     lock,
		}
		s.LoadMeta()
		acquired = append(acquired, s)
		return true, nil
	}

	for _, n := range ListSlotNumbers(groupDir) {
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
	if existing := ListSlotNumbers(groupDir); len(existing) > 0 {
		next = existing[len(existing)-1] + 1
	}
	for len(acquired) < count {
		ok, err := take(next)
		if err != nil {
			release()
			return nil, err
		}
		next++
		if !ok {
			continue
		}
	}

	return acquired, nil
}
