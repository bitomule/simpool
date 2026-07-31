package pool

import (
	"fmt"
	"os"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// EnsureProvisioned makes sure s has a booted simulator matching
// s.Device/s.OSVer in its private device set, creating one if this is a
// fresh slot or its recorded UDID no longer exists. It always boots the
// device before returning, since the caller (MAV, a test runner) expects a
// ready-to-use simulator, not one it has to boot itself.
func EnsureProvisioned(s *Slot, ownerCmd string) error {
	udid := s.Meta.UDID
	if udid != "" {
		if _, found, err := simctl.State(s.SetDir(), udid); err != nil {
			return fmt.Errorf("checking existing device %s: %w", udid, err)
		} else if !found {
			udid = "" // stale meta; recreate below
		}
	}

	if udid == "" {
		runtimeID, deviceTypeID, err := simctl.ResolveRuntime(s.Device, s.OSVer)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("simpool-%s", GroupName(s.Device, s.OSVer))
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
	return s.SaveMeta()
}
