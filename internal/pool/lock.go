package pool

import (
	"errors"
	"os"
	"syscall"
)

// ErrBusy is returned by TryLock when the slot is already held by another
// process.
var ErrBusy = errors.New("slot busy")

// Lock holds an open, flock()'d file descriptor on a slot's lock file.
// The lock is released by closing it (Release). The zero value is not
// usable; construct via TryLock or WaitLock.
//
// Critically: this file is opened with O_CLOEXEC (Go's default for
// os.OpenFile on unix), and it is never placed in a child process's
// ExtraFiles. That is what keeps the flock held exclusively by this
// process — neither the child we launch nor any of its descendants ever
// see the descriptor, so they can never retain the lock past our lifetime.
type Lock struct {
	f    *os.File
	path string
}

// TryLock attempts a non-blocking exclusive lock on path (created if
// missing). Returns ErrBusy if another process already holds it.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{f: f, path: path}, nil
}

// lockBlocking acquires an exclusive lock on path, waiting if another
// process currently holds it (unlike TryLock, which never blocks). Only
// used for the group allocation lock (see allocLockPath): that lock is only
// ever held for the duration of a single mkdir+open or RemoveAll, so
// waiting for it is always brief, and — same as every other lock in this
// package — the kernel releases it immediately if its holder dies for any
// reason, including mid-syscall.
func lockBlocking(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f, path: path}, nil
}

// Release unlocks and closes the underlying descriptor. Safe to call once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// Path returns the lock file's path.
func (l *Lock) Path() string { return l.path }

// IsFree reports whether path's lock is currently uncontended, without
// altering ownership: it takes the lock and immediately releases it.
// A missing lock file counts as free.
func IsFree(path string) (bool, error) {
	l, err := TryLock(path)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return false, nil
		}
		return false, err
	}
	return true, l.Release()
}
