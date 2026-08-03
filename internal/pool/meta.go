package pool

import (
	"encoding/json"
	"os"
	"time"
)

// Meta is informational bookkeeping about a slot. It is never consulted to
// decide whether a slot is free — the lock file is the only source of truth
// for that. Meta can be missing, stale, or corrupt without the pool
// becoming incorrect; callers must tolerate a zero-value Meta.
type Meta struct {
	Device    string    `json:"device"`
	OSVersion string    `json:"osVersion"`
	RuntimeID string    `json:"runtimeId"`
	UDID      string    `json:"udid"`
	Created   time.Time `json:"created"`
	LastUsed  time.Time `json:"lastUsed"`
	OwnerPID  int       `json:"ownerPid"`
	OwnerCmd  string    `json:"ownerCmd,omitempty"`
	// Mode is "with" or "acquire" — which subcommand currently holds (or
	// last held) this slot. `reap` uses it to tell a legitimately
	// child-less holder (`acquire`, which never spawns anything by design)
	// apart from a `with` whose consumer already exited out from under it.
	Mode string `json:"mode,omitempty"`
	// ConsumerPGID is the process-group id of the command `simpool with`
	// launched (which, thanks to Setpgid, always equals that command's own
	// pid). AcquireSlots and reap use it to tell whether a free-looking
	// slot's consumer is still alive even when that consumer never puts the
	// simulator's UDID anywhere in its own command line — it gets the UDID
	// by environment (MAV_TARGET_UDID, SIMPOOL_UDID_N), so a pgrep-based
	// check alone cannot see it. Unset (0) for `acquire`, which never
	// spawns a child.
	ConsumerPGID int `json:"consumerPgid,omitempty"`
	// ConsumerStartedAt is `ps -o lstart=` for ConsumerPGID, captured by
	// `simpool with` immediately after it launches its child (see with.go).
	// Recovering a poisoned slot (AttemptRecovery) refuses to kill anything
	// by pgid alone — macOS recycles pids, so a live process under that
	// numeric group could be a completely unrelated one — and instead
	// requires this to match exactly before sending a signal. Opaque:
	// never parsed, only ever compared for equality against a fresh
	// `ps -o lstart=` of the same pgid.
	ConsumerStartedAt string `json:"consumerStartedAt,omitempty"`
	// ConsumerBootID is `sysctl kern.boottime`, captured alongside
	// ConsumerStartedAt, fingerprinting the machine boot the consumer was
	// launched under. If this no longer matches the current boot, the
	// machine has rebooted since — nothing survives that, so the slot can
	// be reclaimed with no signal sent to anyone, regardless of what
	// ConsumerPGID's number might coincidentally match post-reboot. Opaque,
	// same equality-only contract as ConsumerStartedAt.
	ConsumerBootID string `json:"consumerBootId,omitempty"`
}

// ReadMeta loads meta.json for a slot. A missing or corrupt file yields a
// zero-value Meta and no error — meta is advisory, never authoritative.
func ReadMeta(slotDir string) Meta {
	var m Meta
	data, err := os.ReadFile(metaPath(slotDir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// WriteMeta best-effort persists meta.json via write-to-temp + rename so a
// crash never leaves a half-written file. Errors are non-fatal to callers
// by design (meta is informational); this function still returns them so
// callers can log if they wish.
func WriteMeta(slotDir string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := metaPath(slotDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath(slotDir))
}
