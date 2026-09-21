package pool

import (
	"fmt"
	"os"

	"github.com/bitomule/simpool/internal/procs"
)

// leaseDriverPID, leaseDriverAlive and leaseDriverStartTime are the three
// pieces of process identity this file needs, as package vars so tests can
// stage a driver without spawning one.
var (
	leaseDriverPID       = os.Getppid
	leaseDriverAlive     = procs.Alive
	leaseDriverStartTime = procs.ProcessStartTime
)

// LeaseDriver is one live process driving `simpool lease` under a given
// key: the parent of the lease process itself. See Meta.LeaseDriverPID for
// why the parent and not the lease process.
type LeaseDriver struct {
	PID       int
	StartedAt string
}

// String is what the warning prints — a pid a reader can go and look at
// with `ps`, which is the whole point of naming it at all.
func (d LeaseDriver) String() string { return fmt.Sprintf("pid %d", d.PID) }

// currentLeaseDriver is this call's own driver, fingerprinted. A start
// time that cannot be read yields an empty fingerprint, which is never
// equal to a recorded one — see ConcurrentLeaseDriver for why that
// direction is the safe one.
func currentLeaseDriver() LeaseDriver {
	pid := leaseDriverPID()
	started, err := leaseDriverStartTime(pid)
	if err != nil {
		return LeaseDriver{PID: pid}
	}
	return LeaseDriver{PID: pid, StartedAt: started}
}

// ConcurrentLeaseDriver reports the OTHER live session sharing this lease
// key, if there is one: the driver recorded on the slot is not this
// call's, and it is still alive with the same start time it had when it
// was recorded.
//
// This is the only thing simpool can honestly say about the failure that
// produced the report this was written for — two agents, one key, one
// simulator, contaminated readings. Stickiness is per KEY (see
// AcquireLease), the default key is the caller's git repo root (see
// cli.defaultLeaseKey), and two agents launched from one checkout
// therefore ask under one key and are handed one slot, with --max having
// nothing to say about it. That is the design working as specified; what
// was missing was anybody being told it had happened.
//
// It must be able to stay silent, or it is noise and gets ignored within
// a week. The four quiet cases, all of them the normal ones:
//
//   - One agent's hot loop. Every `mav tap` is a fresh `simpool lease`
//     process with a different pid, but they all share one parent — the
//     mav process driving them — so the recorded driver IS this call's.
//   - A long-lived process leasing twice. Same pid, same parent.
//   - The previous session finished. Its driver is gone, so the recorded
//     pid is dead (or has been reused by something with a different start
//     time) and there is nobody to warn about.
//   - A driver that spawns a fresh wrapper per call (`sh -c 'simpool
//     lease …'`). The parent differs every time, but the previous one
//     exited before the next call ran, so the same "still alive" test
//     holds it quiet.
//
// Every uncertainty resolves to silence: no recorded driver, an
// unreadable start time on either side, a recorded fingerprint that is
// empty. A warning simpool cannot stand behind is worse than none.
func ConcurrentLeaseDriver(meta Meta) (LeaseDriver, bool) {
	if meta.LeaseDriverPID == 0 || meta.LeaseDriverStartedAt == "" {
		return LeaseDriver{}, false
	}
	if meta.LeaseDriverPID == leaseDriverPID() {
		return LeaseDriver{}, false
	}
	if !leaseDriverAlive(meta.LeaseDriverPID) {
		return LeaseDriver{}, false
	}
	// Alive is not enough: macOS recycles pids, and an unrelated process
	// that happens to hold the old number would otherwise be reported as a
	// second agent on this slot. Same rule AttemptRecovery applies before
	// it kills anything by pgid.
	started, err := leaseDriverStartTime(meta.LeaseDriverPID)
	if err != nil || started != meta.LeaseDriverStartedAt {
		return LeaseDriver{}, false
	}
	return LeaseDriver{PID: meta.LeaseDriverPID, StartedAt: started}, true
}
