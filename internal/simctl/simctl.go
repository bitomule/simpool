// Package simctl wraps the subset of `xcrun simctl` simpool needs. All
// operations run against the default device set — the same one Xcode and
// the user's own simulators live in (see design decision "opción (b)":
// custom device sets were tried and dropped because Simulator.app is
// single-instance-per-set and axe/idb cannot see a non-default set at all).
// Isolation is by name, not by set: every simulator simpool creates gets a
// unique, deterministic SIMPOOL_-prefixed name (see pool.DeviceName) so
// callers here and in package pool can positively identify a pool-owned
// device and never mistake one of the user's own for one of ours.
package simctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Runtime describes one entry from `simctl list runtimes -j`.
type Runtime struct {
	Identifier  string `json:"identifier"`
	Version     string `json:"version"`
	IsAvailable bool   `json:"isAvailable"`
	DeviceTypes []struct {
		Name       string `json:"name"`
		Identifier string `json:"identifier"`
	} `json:"supportedDeviceTypes"`
}

type runtimesDoc struct {
	Runtimes []Runtime `json:"runtimes"`
}

// ListRuntimes returns every runtime known to the host.
func ListRuntimes() ([]Runtime, error) {
	out, err := run("list", "runtimes", "-j")
	if err != nil {
		return nil, err
	}
	var doc runtimesDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parsing simctl runtimes: %w", err)
	}
	return doc.Runtimes, nil
}

// ResolveRuntime finds an available runtime whose version matches
// osVersion, either exactly or as a prefix (e.g. "26.3" matches "26.3.1").
// It also resolves the device type identifier for device within that
// runtime's supported device types.
func ResolveRuntime(device, osVersion string) (runtimeID, deviceTypeID string, err error) {
	runtimes, err := ListRuntimes()
	if err != nil {
		return "", "", err
	}
	var best *Runtime
	for i := range runtimes {
		r := &runtimes[i]
		if !r.IsAvailable {
			continue
		}
		if r.Version == osVersion || strings.HasPrefix(r.Version, osVersion+".") {
			best = r
			if r.Version == osVersion {
				break
			}
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("no available simulator runtime matches OS version %q", osVersion)
	}
	for _, dt := range best.DeviceTypes {
		if dt.Name == device {
			return best.Identifier, dt.Identifier, nil
		}
	}
	return "", "", fmt.Errorf("runtime %s does not support device type %q", best.Identifier, device)
}

// deviceEntry mirrors one item under devices[<runtime>] in `simctl list
// devices -j`.
type deviceEntry struct {
	UDID  string `json:"udid"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type devicesDoc struct {
	Devices map[string][]deviceEntry `json:"devices"`
}

// Create makes a new simulator in the default device set. Returns its
// UDID. name must be unique within the set — callers in package pool are
// responsible for that (see pool.DeviceName).
func Create(name, deviceTypeID, runtimeID string) (string, error) {
	out, err := run("create", name, deviceTypeID, runtimeID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Boot starts udid. "Already booted" is not an error.
func Boot(udid string) error {
	_, err := run("boot", udid)
	if err != nil && !strings.Contains(err.Error(), "Unable to boot device in current state: Booted") {
		return err
	}
	return nil
}

// Shutdown stops udid. "Already shutdown" is not an error.
func Shutdown(udid string) error {
	_, err := run("shutdown", udid)
	if err != nil && !strings.Contains(err.Error(), "Unable to shutdown device in current state: Shutdown") {
		return err
	}
	return nil
}

// Delete removes udid (and its data) from the default device set.
func Delete(udid string) error {
	_, err := run("delete", udid)
	return err
}

// Find returns the device set entry for udid — its name and state — or
// found=false if no device with that UDID exists. Callers that are about
// to act destructively on a UDID pulled from meta.json should use this
// (not just State) so they can check Name before trusting it: the default
// device set also holds the user's own simulators, and a UDID is not by
// itself proof that a device is pool-owned (see pool.IsPoolName).
func Find(udid string) (DeviceEntry, bool, error) {
	devices, err := ListDevices()
	if err != nil {
		return DeviceEntry{}, false, err
	}
	for _, d := range devices {
		if d.UDID == udid {
			return d, true, nil
		}
	}
	return DeviceEntry{}, false, nil
}

// State returns the device's current state string (e.g. "Booted",
// "Shutdown"), or "" plus false if udid is not found.
func State(udid string) (string, bool, error) {
	d, found, err := Find(udid)
	if err != nil || !found {
		return "", found, err
	}
	return d.State, true, nil
}

// ListDevices returns every device known to the default device set —
// including the caller's own, unrelated simulators; callers must filter by
// name (pool.IsPoolName) before treating any result as pool-owned.
func ListDevices() ([]deviceEntry, error) {
	out, err := run("list", "devices", "-j")
	if err != nil {
		return nil, err
	}
	var doc devicesDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("parsing simctl devices: %w", err)
	}
	var all []deviceEntry
	for _, list := range doc.Devices {
		all = append(all, list...)
	}
	return all, nil
}

// DeviceEntry re-exports deviceEntry's fields for callers outside the
// package that need to inspect a device without importing the unexported
// type name.
type DeviceEntry = deviceEntry

func run(args ...string) ([]byte, error) {
	full := append([]string{"simctl"}, args...)
	cmd := exec.Command("xcrun", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("xcrun %s: %w: %s", strings.Join(full, " "), err, msg)
	}
	return stdout.Bytes(), nil
}
