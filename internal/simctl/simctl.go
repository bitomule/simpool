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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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
	best, err := resolveRuntimeByVersion(osVersion)
	if err != nil {
		return "", "", err
	}
	for _, dt := range best.DeviceTypes {
		if dt.Name == device {
			return best.Identifier, dt.Identifier, nil
		}
	}
	return "", "", fmt.Errorf("runtime %s does not support device type %q", best.Identifier, device)
}

// ResolveRuntimeVersion finds an available runtime whose version matches
// osVersion the same way ResolveRuntime does, but without also resolving a
// device type. Callers that need to compare a *resolved* runtime identifier
// against a device's actual RuntimeID (see the substance check in
// pool.ensureProvisioned) use this instead of ResolveRuntime so they don't
// have to supply — or care about — a device type just to get the identifier.
func ResolveRuntimeVersion(osVersion string) (identifier, version string, err error) {
	best, err := resolveRuntimeByVersion(osVersion)
	if err != nil {
		return "", "", err
	}
	return best.Identifier, best.Version, nil
}

func resolveRuntimeByVersion(osVersion string) (*Runtime, error) {
	runtimes, err := ListRuntimes()
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("no available simulator runtime matches OS version %q", osVersion)
	}
	return best, nil
}

// deviceEntry mirrors one item under devices[<runtime>] in `simctl list
// devices -j`. IsAvailable and DeviceTypeID are read from the JSON;
// RuntimeID is not part of an individual entry's JSON at all — `simctl`
// only tells you a device's runtime by which map key it was filed under —
// so ListDevices fills it in from that key as it flattens the map.
type deviceEntry struct {
	UDID         string `json:"udid"`
	Name         string `json:"name"`
	State        string `json:"state"`
	IsAvailable  bool   `json:"isAvailable"`
	DeviceTypeID string `json:"deviceTypeIdentifier"`
	RuntimeID    string `json:"-"`
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

// Boot starts udid and returns as soon as the boot has been requested.
// "Already booted" is not an error. Deliberately does NOT wait for the
// device to finish booting — see BootAndWait for a caller that needs udid
// provably ready before it hands the device to someone else. Kept only for
// callers (tests standing up a device outside a slot) that just need
// *a* boot underway and manage their own waiting.
func Boot(udid string) error {
	_, err := run("boot", udid)
	if err != nil && !strings.Contains(err.Error(), "Unable to boot device in current state: Booted") {
		return err
	}
	return nil
}

// BootSettleMargin is slept after `xcrun simctl bootstatus -b` itself
// reports a simulator has finished booting. bootstatus's own "done" signal
// is not sufficient by itself: rules_apple hits the exact same gap
// independently (bazelbuild/rules_apple,
// apple/testing/default_runner/simulator_creator.py's _boot_simulator runs
// the identical `bootstatus <udid> -b` call and then still does
// `time.sleep(3)`, commented "Even bootstatus doesn't wait long enough and
// tests can still fail because the simulator isn't ready"). Adopted
// verbatim rather than re-derived from scratch: it's the same underlying
// symptom — springboard/backboardd answering simctl's own status query
// before the device is actually ready for UI automation — observed by a
// wholly independent, widely-used consumer of the same simctl API on the
// same platform.
const BootSettleMargin = 3 * time.Second

// BootAndWait boots udid if it isn't already booted and blocks until
// `xcrun simctl bootstatus -b` reports it has finished, plus
// BootSettleMargin. Unlike Boot, a nil return here means udid is safe to
// hand to a caller that will immediately try to talk to it (axe, an
// install) — that is the whole point of this function existing instead of
// Boot: Boot only fires the boot and returns immediately, which is exactly
// the gap that let a still-booting simulator's UDID reach a caller that
// then failed to reach it.
//
// timeout bounds the wait: a wedged simulator produces a clear, actionable
// error instead of hanging the caller indefinitely, and a timeout is never
// mistaken for success.
func BootAndWait(udid string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "bootstatus", udid, "-b")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("simulator %s did not finish booting within %s (xcrun simctl bootstatus -b timed out) — it may be wedged; try `xcrun simctl shutdown %s`, or run `xcrun simctl diagnose` if this keeps happening", udid, timeout, udid)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return fmt.Errorf("xcrun simctl bootstatus %s -b: %w: %s", udid, err, msg)
	}

	time.Sleep(BootSettleMargin)
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
	for runtimeID, list := range doc.Devices {
		for _, d := range list {
			d.RuntimeID = runtimeID
			all = append(all, d)
		}
	}
	return all, nil
}

// DeviceEntry re-exports deviceEntry's fields for callers outside the
// package that need to inspect a device without importing the unexported
// type name.
type DeviceEntry = deviceEntry

// EnvSimctlTimeout overrides DefaultSimctlTimeout, parsed with
// time.ParseDuration (e.g. "30s", "500ms"). An unparseable or non-positive
// value is ignored in favor of the default rather than treated as an error —
// a malformed override should never turn into "no timeout at all".
const EnvSimctlTimeout = "SIMPOOL_SIMCTL_TIMEOUT"

// DefaultSimctlTimeout bounds every simctl call routed through run() — every
// one except `bootstatus`, which has its own separate, typically much
// longer deadline via BootAndWait's timeout parameter and must not be
// double-bounded here as well. 120s because `simctl create` on a runtime's
// first use and `simctl delete` of a multi-GB device are legitimately slow
// on a loaded machine; anything past that is a wedge, not work — and a
// wedged simctl must never be allowed to hang `simpool lease`, invoked by
// mav as `target_command` roughly once a minute with nothing to retry it.
const DefaultSimctlTimeout = 120 * time.Second

// ErrSimctlTimeout is the sentinel wrapped into run()'s returned error when
// a simctl invocation is killed for exceeding its deadline. Check with
// errors.Is, not string matching.
var ErrSimctlTimeout = errors.New("simctl call timed out")

func simctlTimeout() time.Duration {
	if v := os.Getenv(EnvSimctlTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultSimctlTimeout
}

// runContext executes one `xcrun` invocation and returns its stdout. It is a
// package-level var — not a plain function — so tests can point it at
// something other than the real `xcrun` binary (e.g. `/bin/sleep`) to
// exercise run()'s timeout handling hermetically, with no simulator and no
// Xcode involved.
var runContext = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// run invokes `xcrun simctl <args...>`, bounded by simctlTimeout() so a
// wedged CoreSimulator process can never hang the caller indefinitely (see
// DefaultSimctlTimeout). The command line is always named in the returned
// error — both on timeout and on ordinary failure — so a caller several
// layers up (e.g. `simpool lease`'s one-line stdout contract, which cannot
// carry this detail itself) still has something actionable on stderr.
func run(args ...string) ([]byte, error) {
	full := append([]string{"simctl"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), simctlTimeout())
	defer cancel()

	out, err := runContext(ctx, full...)
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w: xcrun %s", ErrSimctlTimeout, strings.Join(full, " "))
	}
	if err != nil {
		return nil, fmt.Errorf("xcrun %s: %w", strings.Join(full, " "), err)
	}
	return out, nil
}
