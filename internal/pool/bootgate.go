package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// EnvBootConcurrency overrides DefaultBootConcurrency.
const EnvBootConcurrency = "SIMPOOL_BOOT_CONCURRENCY"

// bootGatePollInterval bounds how often AcquireBootGate re-scans for a free
// slot while waiting. Short, because a boot slot is typically held for tens
// of seconds and the gate itself is cheap to probe (a handful of
// non-blocking flock attempts on tiny local files).
const bootGatePollInterval = 200 * time.Millisecond

// DefaultBootConcurrency returns how many simulators may boot at once,
// machine-wide, across every device+OS group and every simpool process
// sharing this pool root. rules_idb measured 4 boots gated at 13s vs 31s
// serialized — but the reason this exists in *this* codebase specifically
// is jetsam, not speed: several simultaneous cold boots on an 18GB machine
// is exactly what triggers it, and jetsam SIGKILLs consumers, which is what
// leaves the orphans reap/AttemptRecovery exist to clean up in the first
// place. Gating boots closes that feedback loop rather than just widening
// it. Defaults to half the CPU count (a boot is bursty on CPU but the real
// constraint here is memory, and core count is the only cheap proxy for
// "how much of this machine simpool should allow itself to consume at
// once" available without shelling out to `vm_stat`). Override with
// SIMPOOL_BOOT_CONCURRENCY.
func DefaultBootConcurrency() int {
	if v := os.Getenv(EnvBootConcurrency); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU() / 2; n > 0 {
		return n
	}
	return 1
}

// bootGateDir is a fixed, machine-wide directory under root — never per
// group, per device+OS pair, or per slot — because the resource it guards
// (concurrent boots eating RAM/CPU) is shared across every group in the
// pool, not scoped to any one of them.
func bootGateDir(root string) string { return filepath.Join(root, ".boot-gate") }

func bootGateSlotPath(root string, n int) string {
	return filepath.Join(bootGateDir(root), fmt.Sprintf("slot-%d.lock", n))
}

// AcquireBootGate blocks (polling, never a kernel-blocking flock — see
// below) until one of DefaultBootConcurrency's slots is free, or timeout
// elapses. It is a SEPARATE lock from a slot's own flock and from the
// group allocation lock (allocLockPath): callers only ever acquire it while
// already holding a slot's own flock (see EnsureProvisioned), and it is
// released again the moment the boot itself finishes — not held for the
// slot's lifetime — so it never widens the window a slot's own lock is
// unavailable to anyone else.
//
// Lock acquisition order, stated explicitly because getting this wrong
// deadlocks the whole pool: allocLockPath (group allocation lock) is
// ALWAYS released before EnsureProvisioned ever runs (see claimSlotLock's
// doc comment) — so it is never held concurrently with the boot gate. A
// slot's own flock IS held across EnsureProvisioned, and therefore across
// this call, but the boot gate is only ever acquired FROM INSIDE
// EnsureProvisioned, after that flock is already held — this order (slot
// flock, then boot gate) is the only order anything in this codebase ever
// acquires them in, so it can never invert and deadlock against itself.
//
// Implemented as N independent, non-blocking-per-slot lock files rather
// than one kernel-blocking flock: the kernel has no primitive for "block
// until any one of N files is free", so this polls TryLock across every
// slot once per bootGatePollInterval instead. A missing bootGateDir is
// created on first use.
func AcquireBootGate(root string, timeout time.Duration) (*Lock, error) {
	dir := bootGateDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	size := DefaultBootConcurrency()
	deadline := time.Now().Add(timeout)
	for {
		for n := 0; n < size; n++ {
			lock, err := TryLock(bootGateSlotPath(root, n))
			if err == nil {
				return lock, nil
			}
			if err != ErrBusy {
				return nil, err
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("simpool: all %d boot-concurrency slot(s) stayed busy for %s — the machine may be overloaded with simultaneous boots; override with %s", size, timeout, EnvBootConcurrency)
		}
		time.Sleep(bootGatePollInterval)
	}
}
