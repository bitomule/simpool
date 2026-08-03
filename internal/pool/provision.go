package pool

import (
	"fmt"
	"os"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// DefaultBootTimeout bounds how long EnsureProvisioned will wait for a
// simulator to finish booting (see simctl.BootAndWait) before giving up
// with a clear, actionable error instead of hanging the caller forever. A
// cold boot has been measured at ~110s on real hardware under load; 180s
// leaves headroom above that without leaving a caller stuck indefinitely on
// a device that is genuinely wedged. Override with SIMPOOL_BOOT_TIMEOUT (a
// Go duration string, e.g. "4m").
const DefaultBootTimeout = 180 * time.Second

// EnvBootTimeout overrides DefaultBootTimeout.
const EnvBootTimeout = "SIMPOOL_BOOT_TIMEOUT"

// BootTimeout resolves the effective boot-wait timeout: SIMPOOL_BOOT_TIMEOUT
// if set to a valid positive duration, else DefaultBootTimeout.
func BootTimeout() time.Duration {
	if v := os.Getenv(EnvBootTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultBootTimeout
}

// provisionDeps abstracts every simctl entry point EnsureProvisioned calls.
// Its own decision logic — skip the boot-and-wait entirely for a device
// that's already booted, propagate a timeout as a clear error rather than
// hanging, never treat a failed state check as "ready" — is exactly the
// kind of thing a previous review found this package shipped with zero
// unit coverage for. Faking these lets provision_test.go exercise that
// logic directly instead of shelling out to the real `xcrun simctl`, which
// the default `go test ./...` run must never do (see
// SIMPOOL_RUN_INTEGRATION in internal/cli/integration_test.go).
type provisionDeps struct {
	find           func(udid string) (simctl.DeviceEntry, bool, error)
	listDevices    func() ([]simctl.DeviceEntry, error)
	resolveRuntime func(device, osVersion string) (runtimeID, deviceTypeID string, err error)
	create         func(name, deviceTypeID, runtimeID string) (string, error)
	bootAndWait    func(udid string, timeout time.Duration) error
}

var liveProvisionDeps = provisionDeps{
	find:           simctl.Find,
	listDevices:    simctl.ListDevices,
	resolveRuntime: simctl.ResolveRuntime,
	create:         simctl.Create,
	bootAndWait:    simctl.BootAndWait,
}

// EnsureProvisioned makes sure s has a booted simulator matching
// s.Device/s.OSVer, in the shared default device set, named
// s.DeviceName() (see pool.DeviceName). Creates one if this is a fresh
// slot, its recorded UDID no longer exists, or its recorded UDID exists
// but under a different name than expected — meta.json is advisory, not
// authoritative, so it is never trusted blindly for something as
// consequential as "which simulator is this". It always blocks until the
// device is genuinely ready before returning, since the caller (MAV, a
// test runner) expects a usable simulator, not a UDID it has to poll or
// retry against itself — see simctl.BootAndWait.
//
// Deliberately does NOT hold any group-wide lock while it waits: callers
// (AcquireSlots' take(), AcquireLease's claimSlotForLease) already release
// the group allocation lock before invoking this — see claimSlotLock's doc
// comment — and this function only ever touches the one slot handed to it.
// A slow cold boot here therefore blocks nothing beyond this one slot's own
// caller; every other concurrent acquisition in the same (or any other)
// device+OS group proceeds unaffected.
//
// mode records which subcommand is provisioning ("with" or "acquire") so
// `reap`/`doctor` can tell a legitimately child-less `acquire` holder apart
// from a stuck `with` (see Meta.Mode).
func EnsureProvisioned(s *Slot, ownerCmd, mode string) error {
	return ensureProvisioned(s, ownerCmd, mode, liveProvisionDeps, BootTimeout())
}

func ensureProvisioned(s *Slot, ownerCmd, mode string, deps provisionDeps, bootTimeout time.Duration) error {
	name := s.DeviceName()
	udid := s.Meta.UDID
	// knownState carries a device's already-known state (from the Find or
	// ListDevices lookup below) into the boot decision, so a device this
	// call can already prove is "Booted" never pays for a redundant
	// bootstatus round trip (~2s measured on this machine) on top of the
	// lookup that already told us so. Left "" whenever no lookup happened
	// to report a state (the fresh-create path never has one to report,
	// since a just-created device is never anything but Shutdown), which
	// simply means the boot-and-wait step below always runs — the safe
	// default.
	knownState := ""

	if udid != "" {
		entry, found, err := deps.find(udid)
		if err != nil {
			return fmt.Errorf("checking existing device %s: %w", udid, err)
		}
		// Require an exact name match, not just "found": meta.json can be
		// stale or corrupt, and the default device set also holds every
		// other slot's simulators plus the user's own — a UDID alone is
		// not proof this device belongs to this slot. Refusing anything
		// less than an exact match is what makes the recovery-by-name
		// path below the only way forward when in doubt, instead of ever
		// silently reusing (or shutting down/deleting elsewhere) a device
		// that might not actually be ours.
		if !found || entry.Name != name {
			udid = ""
		} else {
			knownState = entry.State
		}
	}

	if udid == "" {
		// meta.json is advisory and can be lost entirely (crash mid-write,
		// disk full, a human `rm`). Before creating a new simulator, look
		// for one already sitting in the default set under the
		// deterministic, root+slot-unique name we always use — otherwise a
		// lost meta.json leaks the previous device forever, since nothing
		// else in simpool ever inspects device-set contents directly.
		if existing, err := deps.listDevices(); err == nil {
			var matches []simctl.DeviceEntry
			for _, d := range existing {
				if d.Name == name {
					matches = append(matches, d)
				}
			}
			// simctl itself does not enforce unique names — verified by
			// creating two devices with the same name directly — so this is
			// not just theoretical. Recovering from an arbitrary one of
			// several matches would be non-deterministic across runs (Go's
			// map iteration order for `simctl list devices -j`'s JSON is
			// randomized) and could silently hand two different callers two
			// different simulators for what they each believe is the same
			// slot. Refuse loudly instead of guessing; this should never
			// happen given DeviceName's own uniqueness contract, so it is
			// always worth surfacing rather than working around.
			if len(matches) > 1 {
				return fmt.Errorf("%d simulators are named %q in the default device set — refusing to guess which one belongs to this slot; delete the duplicates manually", len(matches), name)
			}
			if len(matches) == 1 {
				udid = matches[0].UDID
				knownState = matches[0].State
			}
		}
	}

	if udid == "" {
		runtimeID, deviceTypeID, err := deps.resolveRuntime(s.Device, s.OSVer)
		if err != nil {
			return err
		}
		newUDID, err := deps.create(name, deviceTypeID, runtimeID)
		if err != nil {
			return fmt.Errorf("creating simulator: %w", err)
		}
		udid = newUDID
		s.Meta.Created = time.Now()
		s.Meta.RuntimeID = runtimeID
		// A device simctl.Create just minted is never anything but
		// Shutdown — no lookup ran to tell us so, but none is needed
		// either: knownState is left "" (meaning "unknown/not booted"),
		// which is exactly what makes the boot-and-wait step below run
		// unconditionally for it.
	}

	if s.Meta.RuntimeID == "" {
		// udid was adopted (matched by UDID or recovered by name) rather
		// than freshly created above, so the branch that resolves and
		// records RuntimeID never ran. Left empty, MAV_TARGET_RUNTIME (§5)
		// would silently and permanently export "" for this slot. Best
		// effort: a failure here must not block handing out an otherwise
		// perfectly usable simulator.
		if runtimeID, _, err := deps.resolveRuntime(s.Device, s.OSVer); err == nil {
			s.Meta.RuntimeID = runtimeID
		}
	}

	// Only pay for the boot-and-wait round trip when the device isn't
	// already known-booted. This is the idempotency fix for the hot path:
	// `simpool lease` on a warm slot used to call simctl.Boot unconditionally
	// (measured ~2s of pure subprocess overhead on this machine for the
	// no-op "already booted" case) on top of whatever lookup above already
	// told us the device's state for free. knownState is only ever "Booted"
	// here when a lookup this call already had to do anyway (deps.find or
	// deps.listDevices, both above) reported it — never a separate check
	// invented just for this — and a state-check failure (knownState left
	// "") always falls through to the wait below rather than ever being
	// read as "ready".
	//
	// Trusting a bare "Booted" here (without re-running bootstatus) is safe
	// specifically because of simpool's own exclusivity guarantee: a
	// SIMPOOL_-owned device only ever transitions into "Booted" via this
	// exact function's own bootAndWait call below (nothing else in this
	// codebase ever boots one), and only one process at a time can ever be
	// inside EnsureProvisioned for a given slot (its flock, or its lease's
	// mutual exclusion with the flock — see claimSlotForLease). So a
	// "Booted" read here can only ever be the tail of a PRIOR call's own
	// bootAndWait — margin already slept, never a boot this same call is
	// racing partway through. This is a different fast path than skipping
	// the wait mid-boot: that would reproduce the exact bug being fixed.
	if knownState != "Booted" {
		if err := deps.bootAndWait(udid, bootTimeout); err != nil {
			return fmt.Errorf("booting %s: %w", udid, err)
		}
	}

	s.Meta.Device = s.Device
	s.Meta.OSVersion = s.OSVer
	s.Meta.UDID = udid
	s.Meta.LastUsed = time.Now()
	s.Meta.OwnerPID = os.Getpid()
	s.Meta.OwnerCmd = ownerCmd
	s.Meta.Mode = mode
	// Always clear the previous consumer's identity here, regardless of
	// mode: `with` records its own child's ConsumerPGID/fingerprint AFTER
	// this call returns (see with.go, right after cmd.Start()), so this
	// never clobbers a value the current invocation is about to set — but
	// without it, a slot that moves from `with` to `acquire`/`lease` (which
	// never set these fields themselves) would otherwise carry a stale
	// pgid/fingerprint forever. Stale beyond "incorrect": if that old pgid
	// number is ever reused by an unrelated process group after this slot
	// has moved on, a poison check would misidentify it as a still-live
	// consumer of a `with` session that is long gone.
	s.Meta.ConsumerPGID = 0
	s.Meta.ConsumerStartedAt = ""
	return s.SaveMeta()
}
