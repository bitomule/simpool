package pool

import (
	"errors"
	"fmt"
	"os"
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
