// Package simctl wraps the subset of `xcrun simctl` simpool needs, always
// scoped to a caller-provided device set via --set so that operations on
// one slot can never see or touch another slot's simulators.
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

// ListRuntimes returns every runtime known to the default device set.
// Runtimes are global to the host regardless of --set, so this never
// needs a --set flag.
func ListRuntimes() ([]Runtime, error) {
	out, err := run("", "list", "runtimes", "-j")
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

// Create makes a new simulator in the device set at setDir. Returns its
// UDID.
func Create(setDir, name, deviceTypeID, runtimeID string) (string, error) {
	out, err := run(setDir, "create", name, deviceTypeID, runtimeID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Boot starts udid in setDir. "Already booted" is not an error.
func Boot(setDir, udid string) error {
	_, err := run(setDir, "boot", udid)
	if err != nil && !strings.Contains(err.Error(), "Unable to boot device in current state: Booted") {
		return err
	}
	return nil
}

// Shutdown stops udid in setDir. "Already shutdown" is not an error.
func Shutdown(setDir, udid string) error {
	_, err := run(setDir, "shutdown", udid)
	if err != nil && !strings.Contains(err.Error(), "Unable to shutdown device in current state: Shutdown") {
		return err
	}
	return nil
}

// Delete removes udid (and its data) from setDir.
func Delete(setDir, udid string) error {
	_, err := run(setDir, "delete", udid)
	return err
}

// State returns the device's current state string (e.g. "Booted",
// "Shutdown"), or "" plus false if udid is not found in setDir.
func State(setDir, udid string) (string, bool, error) {
	devices, err := ListDevices(setDir)
	if err != nil {
		return "", false, err
	}
	for _, d := range devices {
		if d.UDID == udid {
			return d.State, true, nil
		}
	}
	return "", false, nil
}

// ListDevices returns every device known to the device set at setDir.
func ListDevices(setDir string) ([]deviceEntry, error) {
	out, err := run(setDir, "list", "devices", "-j")
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

func run(setDir string, args ...string) ([]byte, error) {
	full := []string{"simctl"}
	if setDir != "" {
		full = append(full, "--set", setDir)
	}
	full = append(full, args...)
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
