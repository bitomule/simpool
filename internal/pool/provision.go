package pool

import (
	"fmt"
	"os"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// deviceName is the deterministic name simpool gives the simulator it
// creates for a slot. Deterministic on purpose: it is what lets
// EnsureProvisioned recover from a lost meta.json (see below) instead of
// leaking the old device forever.
func deviceName(device, osVersion string) string {
	return fmt.Sprintf("simpool-%s", GroupName(device, osVersion))
}

// EnsureProvisioned makes sure s has a booted simulator matching
// s.Device/s.OSVer in its private device set, creating one if this is a
// fresh slot or its recorded UDID no longer exists. It always boots the
// device before returning, since the caller (MAV, a test runner) expects a
// ready-to-use simulator, not one it has to boot itself.
//
// mode records which subcommand is provisioning ("with" or "acquire") so
// `reap`/`doctor` can tell a legitimately child-less `acquire` holder apart
// from a stuck `with` (see Meta.Mode).
func EnsureProvisioned(s *Slot, ownerCmd, mode string) error {
	udid := s.Meta.UDID
	if udid != "" {
		if _, found, err := simctl.State(s.SetDir(), udid); err != nil {
			return fmt.Errorf("checking existing device %s: %w", udid, err)
		} else if !found {
			udid = "" // stale meta; recreate below
		}
	}

	name := deviceName(s.Device, s.OSVer)

	if udid == "" {
		// meta.json is advisory and can be lost (crash mid-write, disk
		// full, a human `rm`). Before creating a new simulator, look for
		// one already sitting in this slot's device set under the
		// deterministic name we always use — otherwise a lost meta.json
		// leaks the previous device forever, since nothing else in simpool
		// ever inspects device-set contents directly.
		if existing, err := simctl.ListDevices(s.SetDir()); err == nil {
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
		newUDID, err := simctl.Create(s.SetDir(), name, deviceTypeID, runtimeID)
		if err != nil {
			return fmt.Errorf("creating simulator: %w", err)
		}
		udid = newUDID
		s.Meta.Created = time.Now()
		s.Meta.RuntimeID = runtimeID
	}

	if err := simctl.Boot(s.SetDir(), udid); err != nil {
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
