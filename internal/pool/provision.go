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
		// deterministic, slot-unique name we always use — otherwise a lost
		// meta.json leaks the previous device forever, since nothing else
		// in simpool ever inspects device-set contents directly.
		if existing, err := simctl.ListDevices(); err == nil {
			for _, d := range existing {
				if d.Name == name {
					udid = d.UDID
					break
				}
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
