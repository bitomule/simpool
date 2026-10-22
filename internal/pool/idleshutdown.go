package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// An idle shutdown holds the slot's flock for the 5-7.5s `simctl shutdown`
// takes, so nobody can be handed a half-shut simulator. What the flock alone
// does not say is that the hold is about to end: to an acquirer it looks
// exactly like a consumer that may keep the slot for an hour, so a lease
// with nowhere else to go used to fail "at capacity" while the one slot it
// could have had was seconds from free. The marker below is how reap says
// "this hold is a shutdown, wait for it".

const shuttingDownFile = "shutting-down.json"

// IdleShutdownWait bounds how long an acquirer waits out an idle shutdown
// before giving up on that slot. A measured shutdown takes 5-7.5s; the rest
// is headroom for a machine under memory pressure, which is exactly when
// idle shutdowns happen.
const IdleShutdownWait = 60 * time.Second

// shuttingDownStale is how old a marker may get before it is no longer
// believed even if its pid is alive (a recycled pid, a reap wedged in
// simctl). Past it the slot is just busy, as it was before markers existed.
const shuttingDownStale = 2 * time.Minute

var idleShutdownPoll = 250 * time.Millisecond

type shuttingDown struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
}

func shuttingDownPath(slotDir string) string {
	return filepath.Join(slotDir, shuttingDownFile)
}

// MarkShuttingDown records that the caller, which must hold slotDir's
// flock, is shutting its simulator down. The returned func removes the
// marker and must run before the flock is released.
func MarkShuttingDown(slotDir string) (func(), error) {
	data, err := json.Marshal(shuttingDown{PID: os.Getpid(), Started: time.Now()})
	if err != nil {
		return nil, err
	}
	tmp := shuttingDownPath(slotDir) + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, shuttingDownPath(slotDir)); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return func() { _ = os.Remove(shuttingDownPath(slotDir)) }, nil
}

// ShutdownInProgress reports whether slotDir's flock is held by an idle
// shutdown that is still running.
func ShutdownInProgress(slotDir string) bool {
	data, err := os.ReadFile(shuttingDownPath(slotDir))
	if err != nil {
		return false
	}
	var m shuttingDown
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	return procs.Alive(m.PID) && time.Since(m.Started) < shuttingDownStale
}

// waitOutIdleShutdown waits for slotDir's idle shutdown to finish and then
// claims the slot. A slot with no shutdown in progress is not waited on.
func waitOutIdleShutdown(slotDir string, claim func() (*Slot, error)) (*Slot, error) {
	if !ShutdownInProgress(slotDir) {
		return nil, nil
	}
	deadline := time.Now().Add(IdleShutdownWait)
	for ShutdownInProgress(slotDir) && time.Now().Before(deadline) {
		time.Sleep(idleShutdownPoll)
	}
	// The marker goes a moment before the flock does; a few retries cover
	// that gap without waiting on a slot somebody else just took.
	for i := 0; i < 8 && time.Now().Before(deadline); i++ {
		if s, err := claim(); s != nil || err != nil {
			return s, err
		}
		time.Sleep(idleShutdownPoll / 2)
	}
	return nil, nil
}
