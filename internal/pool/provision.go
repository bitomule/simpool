package pool

import (
	"fmt"
	"os"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// EnsureProvisioned makes sure s has a booted simulator matching
// s.Device/s.OSVer, in the shared default device set, named
// s.DeviceName() (see pool.DeviceName). Creates one if this is a fresh
// slot, its recorded UDID no longer exists, or its recorded UDID exists
// but under a different name than expected — meta.json is advisory, not
// authoritative, so it is never trusted blindly for something as
// consequential as "which simulator is this". It always boots the device
// before returning, since the caller (MAV, a test runner) expects a
// ready-to-use simulator, not one it has to boot itself.
//
// mode records which subcommand is provisioning ("with" or "acquire") so
// `reap`/`doctor` can tell a legitimately child-less `acquire` holder apart
// from a stuck `with` (see Meta.Mode).
func EnsureProvisioned(s *Slot, ownerCmd, mode string) error {
	name := s.DeviceName()
	udid := s.Meta.UDID

	if udid != "" {
		entry, found, err := simctl.Find(udid)
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
		}
	}

	if udid == "" {
		// meta.json is advisory and can be lost entirely (crash mid-write,
		// disk full, a human `rm`). Before creating a new simulator, look
		// for one already sitting in the default set under the
		// deterministic, root+slot-unique name we always use — otherwise a
		// lost meta.json leaks the previous device forever, since nothing
		// else in simpool ever inspects device-set contents directly.
		if existing, err := simctl.ListDevices(); err == nil {
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
			}
		}
	}

	if udid == "" {
		runtimeID, deviceTypeID, err := simctl.ResolveRuntime(s.Device, s.OSVer)
		if err != nil {
			return err
		}
		newUDID, err := simctl.Create(name, deviceTypeID, runtimeID)
		if err != nil {
			return fmt.Errorf("creating simulator: %w", err)
		}
		udid = newUDID
		s.Meta.Created = time.Now()
		s.Meta.RuntimeID = runtimeID
	}

	if s.Meta.RuntimeID == "" {
		// udid was adopted (matched by UDID or recovered by name) rather
		// than freshly created above, so the branch that resolves and
		// records RuntimeID never ran. Left empty, MAV_TARGET_RUNTIME (§5)
		// would silently and permanently export "" for this slot. Best
		// effort: a failure here must not block handing out an otherwise
		// perfectly usable simulator.
		if runtimeID, _, err := simctl.ResolveRuntime(s.Device, s.OSVer); err == nil {
			s.Meta.RuntimeID = runtimeID
		}
	}

	if err := simctl.Boot(udid); err != nil {
		return fmt.Errorf("booting %s: %w", udid, err)
	}

	s.Meta.Device = s.Device
	s.Meta.OSVersion = s.OSVer
	s.Meta.UDID = udid
	s.Meta.LastUsed = time.Now()
	s.Meta.OwnerPID = os.Getpid()
	s.Meta.OwnerCmd = ownerCmd
	s.Meta.Mode = mode
	return s.SaveMeta()
}
