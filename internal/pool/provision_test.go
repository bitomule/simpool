package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// fakeSlot builds a *Slot with a temp dir, standing in for one AcquireSlots
// would have handed back. Only the fields ensureProvisioned actually reads
// are populated.
func fakeSlot(t *testing.T, device, osVersion string, udid string) *Slot {
	t.Helper()
	root := t.TempDir()
	groupDir := GroupDir(root, device, osVersion)
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating slot dir: %v", err)
	}
	s := &Slot{
		Root:     root,
		GroupDir: groupDir,
		Dir:      dir,
		Number:   0,
		Device:   device,
		OSVer:    osVersion,
	}
	s.Meta.UDID = udid
	return s
}

func neverListDevices(t *testing.T) func() ([]simctl.DeviceEntry, error) {
	return func() ([]simctl.DeviceEntry, error) {
		t.Fatal("listDevices should not be called: an exact UDID+name match was already found")
		return nil, nil
	}
}

func neverCreate(t *testing.T) func(name, deviceTypeID, runtimeID string) (string, error) {
	return func(name, deviceTypeID, runtimeID string) (string, error) {
		t.Fatal("create should not be called: an existing device was already adopted")
		return "", nil
	}
}

func stubResolveRuntime(runtimeID, deviceTypeID string) func(device, osVersion string) (string, string, error) {
	return func(device, osVersion string) (string, string, error) {
		return runtimeID, deviceTypeID, nil
	}
}

// TestEnsureProvisioned_AlreadyBootedSkipsWait is the regression test for
// the idempotency fix: a slot whose recorded device is already reported
// "Booted" by the same lookup that confirmed its identity must never pay
// for a bootAndWait round trip on top of that lookup. Before this fix,
// EnsureProvisioned called simctl.Boot unconditionally, which cost real
// wall time (~2s measured against a real, already-booted simulator) on
// every single `simpool lease` in a hot loop.
func TestEnsureProvisioned_AlreadyBootedSkipsWait(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "already-booted-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	bootAndWaitCalled := false
	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			if u != udid {
				t.Fatalf("find called with %q, want %q", u, udid)
			}
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Booted"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			bootAndWaitCalled = true
			return nil
		},
	}

	if err := ensureProvisioned(s, "lease (key test)", "lease", deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned: %v", err)
	}
	if bootAndWaitCalled {
		t.Fatal("bootAndWait was called for a device the find lookup already reported as Booted — the hot-path idempotency fix regressed")
	}
	if s.Meta.UDID != udid {
		t.Errorf("Meta.UDID = %q, want %q", s.Meta.UDID, udid)
	}
}

// TestEnsureProvisioned_NotBootedWaits proves the other half of the same
// contract: a device NOT already known to be Booted (Shutdown, Booting, or
// simply unknown) must always go through bootAndWait before
// EnsureProvisioned returns — this is the actual bug fix (a `simpool lease`
// UDID must never come back for a simulator that's still mid-boot).
func TestEnsureProvisioned_NotBootedWaits(t *testing.T) {
	for _, state := range []string{"Shutdown", "Booting", "Shutting Down", ""} {
		t.Run(state, func(t *testing.T) {
			device, osVersion := "iPhone 17 Pro", "26.3"
			udid := "cold-udid"
			s := fakeSlot(t, device, osVersion, udid)
			name := s.DeviceName()

			var gotUDID string
			var gotTimeout time.Duration
			deps := provisionDeps{
				find: func(u string) (simctl.DeviceEntry, bool, error) {
					return simctl.DeviceEntry{UDID: udid, Name: name, State: state}, true, nil
				},
				listDevices:    neverListDevices(t),
				resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
				create:         neverCreate(t),
				bootAndWait: func(u string, timeout time.Duration) error {
					gotUDID = u
					gotTimeout = timeout
					return nil
				},
			}

			wantTimeout := 42 * time.Second
			if err := ensureProvisioned(s, "with", "with", deps, wantTimeout); err != nil {
				t.Fatalf("ensureProvisioned: %v", err)
			}
			if gotUDID != udid {
				t.Fatalf("bootAndWait was not called with the slot's device (state %q) — got udid %q", state, gotUDID)
			}
			if gotTimeout != wantTimeout {
				t.Errorf("bootAndWait timeout = %v, want %v", gotTimeout, wantTimeout)
			}
		})
	}
}

// TestEnsureProvisioned_SlowBootWaitsInsteadOfFailing reproduces the exact
// scenario from the bug report: a simulator that takes a while to finish
// booting must make the caller wait, never fail and never hand back a UDID
// before bootAndWait has confirmed readiness.
func TestEnsureProvisioned_SlowBootWaitsInsteadOfFailing(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "slow-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Booting"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			// Stands in for a slow-but-eventually-successful cold boot: by
			// the time this returns, the real simctl.BootAndWait contract
			// guarantees the device is genuinely ready.
			time.Sleep(30 * time.Millisecond)
			return nil
		},
	}

	start := time.Now()
	if err := ensureProvisioned(s, "with", "with", deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned returned an error for a slow-but-successful boot: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("ensureProvisioned returned before bootAndWait's simulated delay elapsed (%v) — it must actually wait, not race ahead", elapsed)
	}
}

// TestEnsureProvisioned_BootTimeoutIsClearAndNeverSilent is the regression
// test for "un timeout nunca puede colgarse indefinidamente ni leerse como
// listo": when bootAndWait reports it gave up, EnsureProvisioned must
// surface a clear error — never nil, never a hang — and must never reach
// the code path that marks the slot ready (SaveMeta persisting a UDID as
// if it were usable).
func TestEnsureProvisioned_BootTimeoutIsClearAndNeverSilent(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "wedged-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()
	timeoutErr := fmt.Errorf("simulator %s did not finish booting within %s (xcrun simctl bootstatus -b timed out)", udid, time.Second)

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Booting"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			return timeoutErr
		},
	}

	err := ensureProvisioned(s, "with", "with", deps, time.Second)
	if err == nil {
		t.Fatal("ensureProvisioned returned nil for a boot that never finished — a timeout must never be read as ready")
	}
	if !errors.Is(err, timeoutErr) {
		t.Errorf("ensureProvisioned's error does not wrap the boot timeout error: %v", err)
	}
	// meta.UDID is only ever set to the provisioned device AFTER the boot
	// step succeeds (see ensureProvisioned's own ordering) — a wedged boot
	// must leave the caller unable to mistake s.Meta for a usable slot.
	if s.Meta.UDID == udid && s.Meta.OwnerCmd != "" {
		t.Errorf("slot metadata was marked ready despite a boot timeout: %+v", s.Meta)
	}
}

// TestEnsureProvisioned_FindErrorNeverTreatedAsReady is the regression test
// for "un error al comprobar el estado nunca puede leerse como listo": if
// the identity/state lookup itself fails, EnsureProvisioned must return
// that error immediately and never fall through to booting or creating
// anything.
func TestEnsureProvisioned_FindErrorNeverTreatedAsReady(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "unverifiable-udid"
	s := fakeSlot(t, device, osVersion, udid)
	findErr := errors.New("xcrun simctl list devices -j: boom")

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{}, false, findErr
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			t.Fatal("bootAndWait must never run when the state check itself failed")
			return nil
		},
	}

	err := ensureProvisioned(s, "with", "with", deps, time.Second)
	if err == nil {
		t.Fatal("ensureProvisioned returned nil despite a failed device-state check")
	}
	if !errors.Is(err, findErr) {
		t.Errorf("ensureProvisioned's error does not wrap the find error: %v", err)
	}
}

// TestEnsureProvisioned_FreshCreateAlwaysWaits proves a brand-new simulator
// (no recorded UDID, none recovered by name either) always goes through
// bootAndWait — there is no lookup to short-circuit on, so it must never
// silently skip the wait just because a fresh device has no known state.
func TestEnsureProvisioned_FreshCreateAlwaysWaits(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	s := fakeSlot(t, device, osVersion, "") // no recorded UDID: fresh slot
	newUDID := "brand-new-udid"

	bootAndWaitCalled := false
	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			t.Fatal("find should not be called when the slot has no recorded UDID")
			return simctl.DeviceEntry{}, false, nil
		},
		listDevices: func() ([]simctl.DeviceEntry, error) {
			return nil, nil // nothing pre-existing under this deterministic name
		},
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create: func(name, deviceTypeID, runtimeID string) (string, error) {
			return newUDID, nil
		},
		bootAndWait: func(u string, timeout time.Duration) error {
			bootAndWaitCalled = true
			if u != newUDID {
				t.Errorf("bootAndWait called with %q, want freshly created %q", u, newUDID)
			}
			return nil
		},
	}

	if err := ensureProvisioned(s, "acquire", "acquire", deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned: %v", err)
	}
	if !bootAndWaitCalled {
		t.Fatal("bootAndWait was never called for a freshly created simulator")
	}
	if s.Meta.UDID != newUDID {
		t.Errorf("Meta.UDID = %q, want %q", s.Meta.UDID, newUDID)
	}
}

// bootAndWaitFromDeps builds a provisionDeps.bootAndWait replacement out of
// simctl.BootWaitDeps — proving EnsureProvisioned's readiness gate is driven
// by the real state machine in simctl.BootAndWaitWithDeps (triage
// bootstatus's exit code against device state, poll for real readiness,
// retry at most once), exercised here entirely with fakes: no `xcrun`, no
// simulator.
func bootAndWaitFromDeps(deps simctl.BootWaitDeps) func(udid string, timeout time.Duration) error {
	return func(udid string, timeout time.Duration) error {
		return simctl.BootAndWaitWithDeps(udid, timeout, deps)
	}
}

// TestReadinessGate_BootstatusFailureButDeviceActuallyBooted proves
// bootstatus's own exit code is triaged against the device's real state
// instead of trusted blindly: bootstatus can report a terminal failure yet
// the device turns out to be genuinely Booted (the same ambiguity the Bazel
// runner template already triages), and that must not be read as fatal.
func TestReadinessGate_BootstatusFailureButDeviceActuallyBooted(t *testing.T) {
	var mu sync.Mutex
	shutdownCalls := 0

	deps := simctl.BootWaitDeps{
		Bootstatus: func(ctx context.Context, udid string) error {
			return errors.New("bootstatus: simulated non-zero exit")
		},
		Find: func(udid string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, State: "Booted"}, true, nil
		},
		SpringBoardReady: func(ctx context.Context, udid string) (bool, error) {
			return true, nil
		},
		Shutdown: func(udid string) error {
			mu.Lock()
			shutdownCalls++
			mu.Unlock()
			return nil
		},
	}

	if err := bootAndWaitFromDeps(deps)("udid-1", time.Second); err != nil {
		t.Fatalf("BootAndWaitWithDeps returned an error despite the device actually being Booted: %v", err)
	}
	if shutdownCalls != 0 {
		t.Fatalf("shutdown was called %d times; a bootstatus failure that resolves to an actually-Booted device must not trigger a retry", shutdownCalls)
	}
}

// TestReadinessGate_BootstatusFailureAndDeviceNotBooted proves the other
// half: when bootstatus fails AND the device's real state is not Booted,
// that is a genuine, non-retriable failure — it must error, and must NEVER
// return nil (a nil here would hand out a UDID that isn't actually usable).
func TestReadinessGate_BootstatusFailureAndDeviceNotBooted(t *testing.T) {
	bootstatusErr := errors.New("bootstatus: simulated non-zero exit")
	deps := simctl.BootWaitDeps{
		Bootstatus: func(ctx context.Context, udid string) error {
			return bootstatusErr
		},
		Find: func(udid string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, State: "Shutdown"}, true, nil
		},
		SpringBoardReady: func(ctx context.Context, udid string) (bool, error) {
			t.Fatal("SpringBoardReady should never be polled when bootstatus failed and the device is confirmed not Booted")
			return false, nil
		},
		Shutdown: func(udid string) error {
			t.Fatal("shutdown should never run for a non-retriable failure")
			return nil
		},
	}

	err := bootAndWaitFromDeps(deps)("udid-2", time.Second)
	if err == nil {
		t.Fatal("BootAndWaitWithDeps returned nil for a device that bootstatus failed on and is confirmed Shutdown — must never be read as ready")
	}
	if !errors.Is(err, bootstatusErr) {
		t.Errorf("error does not wrap the original bootstatus error: %v", err)
	}
}

// TestReadinessGate_RetriesExactlyOnceWhenReadinessNeverArrives proves the
// bounded-retry guarantee: if SpringBoard never comes up, BootAndWaitWithDeps
// shuts the device down and retries the whole sequence exactly once — never
// in an unbounded loop — then gives up with an error once the deadline
// (shared across both attempts) is exhausted.
func TestReadinessGate_RetriesExactlyOnceWhenReadinessNeverArrives(t *testing.T) {
	var mu sync.Mutex
	bootstatusCalls, shutdownCalls, readyCalls := 0, 0, 0

	deps := simctl.BootWaitDeps{
		Bootstatus: func(ctx context.Context, udid string) error {
			mu.Lock()
			bootstatusCalls++
			mu.Unlock()
			return nil // bootstatus itself succeeds every time
		},
		Find: func(udid string) (simctl.DeviceEntry, bool, error) {
			t.Fatal("Find should never be called when bootstatus itself never fails")
			return simctl.DeviceEntry{}, false, nil
		},
		SpringBoardReady: func(ctx context.Context, udid string) (bool, error) {
			mu.Lock()
			readyCalls++
			mu.Unlock()
			return false, nil // SpringBoard never comes up
		},
		Shutdown: func(udid string) error {
			mu.Lock()
			shutdownCalls++
			mu.Unlock()
			return nil
		},
	}

	start := time.Now()
	err := bootAndWaitFromDeps(deps)("udid-3", 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BootAndWaitWithDeps returned nil for a device whose readiness probe never once reported ready")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s to give up on a 150ms-timeout call — the retry is not bounded", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if shutdownCalls != 1 {
		t.Fatalf("shutdown called %d times, want exactly 1 (bounded retry, never a loop)", shutdownCalls)
	}
	if bootstatusCalls != 2 {
		t.Fatalf("bootstatus called %d times, want exactly 2 (once per attempt: initial + one retry)", bootstatusCalls)
	}
	if readyCalls == 0 {
		t.Fatal("SpringBoardReady was never polled at all")
	}
}
