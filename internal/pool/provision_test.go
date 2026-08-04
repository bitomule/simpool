package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
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

func neverShutdown(t *testing.T) func(udid string) error {
	return func(udid string) error {
		t.Fatal("shutdown should not be called: no substance mismatch was set up in this test")
		return nil
	}
}

func neverDelete(t *testing.T) func(udid string) error {
	return func(udid string) error {
		t.Fatal("delete should not be called: no substance mismatch was set up in this test")
		return nil
	}
}

// matchingEntry builds a simctl.DeviceEntry whose substance (availability,
// runtime, device type) matches stubResolveRuntime("runtime-1",
// "devicetype-1") — the resolveRuntime stub every test in this file uses —
// so tests that aren't specifically about substance verification don't
// trip the substance-mismatch path by accident.
func matchingEntry(udid, name, state string) simctl.DeviceEntry {
	return simctl.DeviceEntry{
		UDID:         udid,
		Name:         name,
		State:        state,
		IsAvailable:  true,
		RuntimeID:    "runtime-1",
		DeviceTypeID: "devicetype-1",
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
			return matchingEntry(udid, name, "Booted"), true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			bootAndWaitCalled = true
			return nil
		},
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	if err := ensureProvisioned(s, "lease (key test)", "lease", "test", deps, time.Second); err != nil {
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
					return matchingEntry(udid, name, state), true, nil
				},
				listDevices:    neverListDevices(t),
				resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
				create:         neverCreate(t),
				bootAndWait: func(u string, timeout time.Duration) error {
					gotUDID = u
					gotTimeout = timeout
					return nil
				},
				shutdown: neverShutdown(t),
				delete:   neverDelete(t),
			}

			wantTimeout := 42 * time.Second
			if err := ensureProvisioned(s, "with", "with", "", deps, wantTimeout); err != nil {
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
			return matchingEntry(udid, name, "Booting"), true, nil
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
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	start := time.Now()
	if err := ensureProvisioned(s, "with", "with", "", deps, time.Second); err != nil {
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
			return matchingEntry(udid, name, "Booting"), true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			return timeoutErr
		},
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	err := ensureProvisioned(s, "with", "with", "", deps, time.Second)
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

	err := ensureProvisioned(s, "with", "with", "", deps, time.Second)
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

	if err := ensureProvisioned(s, "acquire", "acquire", "", deps, time.Second); err != nil {
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
// in an unbounded loop — then gives up with an error once both attempts'
// budgets are exhausted.
//
// The Bootstatus and SpringBoardReady fakes here deliberately honor ctx —
// returning ctx.Err() once it is done, exactly like the real primitives
// they stand in for (exec.CommandContext, runContext) actually behave —
// instead of unconditionally succeeding regardless of the context they were
// handed. This is the fix for a prior version of this test: a fake that
// ignores ctx entirely cannot tell a real second attempt (fresh budget,
// does real polling work) apart from a second call that only *looks* like
// a retry because it shares the first attempt's already-exhausted context
// (see BootAndWaitWithDeps' own doc comment on why the two attempts now get
// independent context.WithTimeout calls) — that shared-context bug shipped
// once with a green `bootstatusCalls == 2` assertion exactly like this one,
// because the old fake could not distinguish "called with real work to do"
// from "called and told instantly to give up".
func TestReadinessGate_RetriesExactlyOnceWhenReadinessNeverArrives(t *testing.T) {
	var mu sync.Mutex
	bootstatusCalls, shutdownCalls, readyCalls := 0, 0, 0
	var bootstatusSawExpiredCtx bool
	var readyCallsAtShutdown int

	deps := simctl.BootWaitDeps{
		Bootstatus: func(ctx context.Context, udid string) error {
			mu.Lock()
			bootstatusCalls++
			if ctx.Err() != nil {
				bootstatusSawExpiredCtx = true
			}
			mu.Unlock()
			// Honor ctx exactly like exec.CommandContext would: an
			// already-dead context fails a command invocation instantly,
			// it never silently succeeds.
			return ctx.Err()
		},
		Find: func(udid string) (simctl.DeviceEntry, bool, error) {
			t.Fatal("Find should never be called: bootstatus never failed on a live context in this scenario, only ctx.Err() would trigger it, and a correct implementation never hands bootstatus an already-expired context")
			return simctl.DeviceEntry{}, false, nil
		},
		SpringBoardReady: func(ctx context.Context, udid string) (bool, error) {
			mu.Lock()
			readyCalls++
			mu.Unlock()
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, nil // SpringBoard never comes up
		},
		Shutdown: func(udid string) error {
			mu.Lock()
			shutdownCalls++
			readyCallsAtShutdown = readyCalls
			mu.Unlock()
			return nil
		},
	}

	// Large enough that the retry attempt's own budget (the true remainder
	// after the first attempt's share, not a shared/exhausted context) is
	// comfortably above minRetryBudget and can complete at least one real
	// SpringBoardReady poll of its own — the exact thing the shared-context
	// bug made impossible.
	const totalTimeout = 4 * time.Second

	start := time.Now()
	err := bootAndWaitFromDeps(deps)("udid-3", totalTimeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BootAndWaitWithDeps returned nil for a device whose readiness probe never once reported ready")
	}
	if elapsed > totalTimeout+2*time.Second {
		t.Fatalf("took %s to give up on a %s-timeout call — the retry is not bounded", elapsed, totalTimeout)
	}
	mu.Lock()
	defer mu.Unlock()
	if bootstatusSawExpiredCtx {
		t.Fatal("a Bootstatus call saw an already-expired context — the retry attempt is sharing the first attempt's exhausted context instead of getting its own fresh budget")
	}
	if shutdownCalls != 1 {
		t.Fatalf("shutdown called %d times, want exactly 1 (bounded retry, never a loop)", shutdownCalls)
	}
	if bootstatusCalls != 2 {
		t.Fatalf("bootstatus called %d times, want exactly 2 (once per attempt: initial + one retry)", bootstatusCalls)
	}
	if readyCalls == 0 {
		t.Fatal("SpringBoardReady was never polled at all")
	}
	if readyCalls <= readyCallsAtShutdown {
		t.Fatalf("SpringBoardReady was polled %d times total, but %d had already happened by the time shutdown ran before the retry — the retry attempt did no real polling work of its own, meaning it ran against an already-dead budget", readyCalls, readyCallsAtShutdown)
	}
}

// provisioningContext is one way EnsureProvisioned's mode/leaseKey pair can
// legitimately show up in production: `with`/`acquire` never hold a lease
// of their own, but `simpool lease` always does — AcquireLease writes its
// caller's own lease.json BEFORE calling EnsureProvisioned (see
// resolveSubstanceMismatch's doc comment), so any test exercising the
// substance-mismatch guard that only ever tries mode="with" is not actually
// covering the one path (mav's `simpool lease`) that guard was built for.
type provisioningContext struct {
	name     string
	mode     string
	ownerCmd string
	leaseKey string
	// setup runs after the slot is built but before ensureProvisioned is
	// called, to reproduce whatever AcquireLease would already have done to
	// the slot by the time EnsureProvisioned sees it.
	setup func(t *testing.T, s *Slot)
}

// substanceGuardProvisioningContexts covers every mode EnsureProvisioned is
// actually called with in production (see acquireAndProvision and
// RunLease). The "lease" case pre-writes the caller's own live lease,
// exactly as AcquireLease does before EnsureProvisioned ever runs — this is
// what makes it a regression test for the self-referential-lease bug: if
// resolveSubstanceMismatch ever again refuses a mismatch just because a
// live lease is present, without checking whose key it is, this case turns
// red across every substance test that uses it.
func substanceGuardProvisioningContexts(leaseKey string) []provisioningContext {
	return []provisioningContext{
		{name: "with", mode: "with", ownerCmd: "with"},
		{name: "acquire", mode: "acquire", ownerCmd: "acquire"},
		{
			name:     "lease/own-key-already-written-by-AcquireLease",
			mode:     "lease",
			ownerCmd: "lease (key " + leaseKey + ")",
			leaseKey: leaseKey,
			setup: func(t *testing.T, s *Slot) {
				t.Helper()
				if err := WriteLease(s.Dir, Lease{Key: leaseKey, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
}

// TestEnsureProvisioned_SubstanceMismatchDeletesAndRecreates covers the
// three ways an adopted device can fail substanceOK: wrong runtime,
// isAvailable=false, wrong device type. Each must delete the stale device
// and create a fresh one — never silently reused just because its name
// still matches — across every provisioning mode, including `simpool
// lease` with its own live lease already sitting on the slot (see
// substanceGuardProvisioningContexts).
func TestEnsureProvisioned_SubstanceMismatchDeletesAndRecreates(t *testing.T) {
	const matchRuntime, matchDevType = "runtime-1", "devicetype-1"
	const leaseKey = "hot-repo"

	cases := []struct {
		name  string
		entry func(udid, deviceName string) simctl.DeviceEntry
	}{
		{
			name: "wrong runtime",
			entry: func(udid, deviceName string) simctl.DeviceEntry {
				return simctl.DeviceEntry{UDID: udid, Name: deviceName, State: "Shutdown", IsAvailable: true, RuntimeID: "runtime-OLD", DeviceTypeID: matchDevType}
			},
		},
		{
			name: "not available",
			entry: func(udid, deviceName string) simctl.DeviceEntry {
				return simctl.DeviceEntry{UDID: udid, Name: deviceName, State: "Shutdown", IsAvailable: false, RuntimeID: matchRuntime, DeviceTypeID: matchDevType}
			},
		},
		{
			name: "wrong device type",
			entry: func(udid, deviceName string) simctl.DeviceEntry {
				return simctl.DeviceEntry{UDID: udid, Name: deviceName, State: "Shutdown", IsAvailable: true, RuntimeID: matchRuntime, DeviceTypeID: "devicetype-OLD"}
			},
		},
	}

	for _, pc := range substanceGuardProvisioningContexts(leaseKey) {
		t.Run(pc.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					device, osVersion := "iPhone 17 Pro", "26.3"
					oldUDID := "stale-udid"
					s := fakeSlot(t, device, osVersion, oldUDID)
					name := s.DeviceName()
					newUDID := "recreated-udid"
					if pc.setup != nil {
						pc.setup(t, s)
					}

					var shutdownCalls, deleteCalls, createCalls int
					var shutdownUDID, deletedUDID string

					deps := provisionDeps{
						find: func(u string) (simctl.DeviceEntry, bool, error) {
							return tc.entry(u, name), true, nil
						},
						listDevices:    neverListDevices(t),
						resolveRuntime: stubResolveRuntime(matchRuntime, matchDevType),
						create: func(n, deviceTypeID, runtimeID string) (string, error) {
							createCalls++
							if deviceTypeID != matchDevType || runtimeID != matchRuntime {
								t.Errorf("create called with (%q,%q), want resolved (%q,%q)", deviceTypeID, runtimeID, matchDevType, matchRuntime)
							}
							return newUDID, nil
						},
						bootAndWait: func(u string, timeout time.Duration) error { return nil },
						shutdown: func(u string) error {
							shutdownCalls++
							shutdownUDID = u
							return nil
						},
						delete: func(u string) error {
							deleteCalls++
							deletedUDID = u
							return nil
						},
					}

					if err := ensureProvisioned(s, pc.ownerCmd, pc.mode, pc.leaseKey, deps, time.Second); err != nil {
						t.Fatalf("ensureProvisioned: %v", err)
					}
					if shutdownCalls != 1 {
						t.Errorf("shutdown called %d times, want 1", shutdownCalls)
					}
					if deleteCalls != 1 {
						t.Fatalf("delete called %d times, want 1", deleteCalls)
					}
					if createCalls != 1 {
						t.Fatalf("create called %d times, want 1", createCalls)
					}
					if shutdownUDID != oldUDID || deletedUDID != oldUDID {
						t.Errorf("shutdown/delete called with (%q,%q), want the stale device %q for both", shutdownUDID, deletedUDID, oldUDID)
					}
					if s.Meta.UDID != newUDID {
						t.Errorf("Meta.UDID = %q, want the recreated %q", s.Meta.UDID, newUDID)
					}
					if s.Meta.RuntimeID != matchRuntime {
						t.Errorf("Meta.RuntimeID = %q, want resolved %q for a freshly created device", s.Meta.RuntimeID, matchRuntime)
					}
				})
			}
		})
	}
}

// TestEnsureProvisioned_SubstanceMatchReusesWithoutTouchingDevice proves the
// non-mismatch case never deletes or creates anything, and that
// Meta.RuntimeID is set from the adopted device's own RuntimeID.
func TestEnsureProvisioned_SubstanceMatchReusesWithoutTouchingDevice(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "good-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return matchingEntry(udid, name, "Shutdown"), true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait:    func(u string, timeout time.Duration) error { return nil },
		shutdown:       neverShutdown(t),
		delete:         neverDelete(t),
	}

	if err := ensureProvisioned(s, "with", "with", "", deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned: %v", err)
	}
	if s.Meta.UDID != udid {
		t.Errorf("Meta.UDID = %q, want %q", s.Meta.UDID, udid)
	}
	if s.Meta.RuntimeID != "runtime-1" {
		t.Errorf("Meta.RuntimeID = %q, want the adopted device's own runtime %q", s.Meta.RuntimeID, "runtime-1")
	}
}

// TestEnsureProvisioned_SubstanceMismatchWithLiveLeaseRefusesToTouch proves
// the "when in doubt, error" guard on the `with`/`acquire` path (leaseKey
// == ""): a substance mismatch that would otherwise be recreated must NOT
// be acted on while a live lease sits on the slot. In production this
// specific combination — mode "with" plus a live lease already present —
// should never actually happen (take() already refuses any slot carrying a
// live lease before EnsureProvisioned ever runs, see slot.go's take()), so
// this is defense in depth: proving the guard still holds even if that
// upstream invariant were ever violated. The path this guard was actually
// built to protect — `simpool lease`, where AcquireLease writes ITS OWN
// live lease before calling EnsureProvisioned — is covered separately below
// (TestEnsureProvisioned_LeasePath*), because leaseKey == "" here can never
// equal a real lease's Key (Lease.Alive() requires a non-empty Key), so
// this test cannot by itself prove anything about own-lease-vs-other-lease.
func TestEnsureProvisioned_SubstanceMismatchWithLiveLeaseRefusesToTouch(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "leased-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	if err := WriteLease(s.Dir, Lease{Key: "hot-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Shutdown", IsAvailable: false, RuntimeID: "runtime-1", DeviceTypeID: "devicetype-1"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			t.Fatal("bootAndWait should not run when the substance mismatch could not be safely resolved")
			return nil
		},
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	err := ensureProvisioned(s, "with", "with", "", deps, time.Second)
	if err == nil {
		t.Fatal("ensureProvisioned returned nil for a substance mismatch with a live lease on the slot — must refuse, not silently reuse or delete")
	}
}

// TestEnsureProvisioned_LeasePathWithDifferentKeysLiveLeaseStillRefuses is
// the actual `simpool lease` shape of the guard above: a substance mismatch
// guarded by a live lease belonging to a DIFFERENT key must still refuse —
// that lease is the real exclusion mechanism during provisioning on this
// path (AcquireLease releases the slot flock before calling
// EnsureProvisioned — see AcquireLease's doc comment), so deleting the
// device out from under some other caller's active lease would be exactly
// the failure mode substance verification must never introduce.
func TestEnsureProvisioned_LeasePathWithDifferentKeysLiveLeaseStillRefuses(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "leased-by-other-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	if err := WriteLease(s.Dir, Lease{Key: "someone-elses-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Shutdown", IsAvailable: false, RuntimeID: "runtime-1", DeviceTypeID: "devicetype-1"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			t.Fatal("bootAndWait should not run when the substance mismatch could not be safely resolved")
			return nil
		},
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	err := ensureProvisioned(s, "lease (key my-repo)", "lease", "my-repo", deps, time.Second)
	if err == nil {
		t.Fatal("ensureProvisioned returned nil on the lease path for a substance mismatch guarded by a DIFFERENT key's live lease — must still refuse, own-key is the only exception")
	}
}

// TestEnsureProvisioned_LeasePathWithOwnFreshLeaseDeletesAndRecreatesDrifted
// is the end-to-end mav path this whole guard exists for: pool.AcquireLease
// (real, not faked) claims a slot and writes ITS OWN lease.json, exactly as
// RunLease does before ever calling EnsureProvisioned — then
// EnsureProvisioned (via the unexported ensureProvisioned so the simctl
// layer can be faked) finds a drifted device sitting under the slot's
// deterministic name and must delete and recreate it despite the live
// lease, because that lease is the caller's own. Before the fix this
// regression-tests, resolveSubstanceMismatch read that just-written lease
// via ReadLease and refused unconditionally — every single `simpool lease`
// call for a drifted device, forever, since sticky renewal re-extends this
// exact lease's TTL on every subsequent call.
func TestEnsureProvisioned_LeasePathWithOwnFreshLeaseDeletesAndRecreatesDrifted(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	root := t.TempDir()
	const key = "hot-repo"

	slot, err := AcquireLease(root, device, osVersion, key, time.Hour, 1)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if lease, err := ReadLease(slot.Dir); err != nil || !lease.Alive() || lease.Key != key {
		t.Fatalf("AcquireLease did not leave a live lease of its own on the slot before provisioning: %+v, err=%v", lease, err)
	}

	// A drifted device already sits under this slot's deterministic name
	// (e.g. an Xcode upgrade replaced the runtime before this call) — no
	// recorded UDID in meta.json yet, so the name-recovery path in
	// ensureProvisioned is what finds it via listDevices.
	oldUDID := "drifted-udid"
	name := slot.DeviceName()
	newUDID := "recreated-udid"

	var shutdownCalls, deleteCalls, createCalls int
	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			t.Fatal("find should not be called: the slot has no recorded UDID yet")
			return simctl.DeviceEntry{}, false, nil
		},
		listDevices: func() ([]simctl.DeviceEntry, error) {
			return []simctl.DeviceEntry{
				{UDID: oldUDID, Name: name, State: "Shutdown", IsAvailable: true, RuntimeID: "runtime-OLD", DeviceTypeID: "devicetype-1"},
			}, nil
		},
		resolveRuntime: stubResolveRuntime("runtime-NEW", "devicetype-1"),
		create: func(n, deviceTypeID, runtimeID string) (string, error) {
			createCalls++
			return newUDID, nil
		},
		bootAndWait: func(u string, timeout time.Duration) error { return nil },
		shutdown: func(u string) error {
			shutdownCalls++
			if u != oldUDID {
				t.Errorf("shutdown called with %q, want stale %q", u, oldUDID)
			}
			return nil
		},
		delete: func(u string) error {
			deleteCalls++
			if u != oldUDID {
				t.Errorf("delete called with %q, want stale %q", u, oldUDID)
			}
			return nil
		},
	}

	ownerCmd := "lease (key " + key + ")"
	if err := ensureProvisioned(slot, ownerCmd, "lease", key, deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned refused a drifted device on the mav `simpool lease` path despite the live lease belonging to its OWN caller: %v", err)
	}
	if shutdownCalls != 1 || deleteCalls != 1 || createCalls != 1 {
		t.Fatalf("shutdown=%d delete=%d create=%d, want 1 each — a drifted device on the mav path must be deleted and recreated, never wedged forever behind the caller's own lease", shutdownCalls, deleteCalls, createCalls)
	}
	if slot.Meta.UDID != newUDID {
		t.Errorf("Meta.UDID = %q, want the recreated %q", slot.Meta.UDID, newUDID)
	}
}

// TestEnsureProvisioned_SubstanceMismatchWithPoisonedMetaRefusesToTouch is
// the same guard for the other exclusion mechanism: a slot whose previous
// consumer is still alive (verified via a real process, mirroring
// slot_test.go's own poisoned-slot tests) must not have its device deleted
// just because it also happens to look substance-mismatched.
func TestEnsureProvisioned_SubstanceMismatchWithPoisonedMetaRefusesToTouch(t *testing.T) {
	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "poisoned-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	s.Meta.Mode = "with"
	s.Meta.ConsumerPGID = pgid

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Shutdown", IsAvailable: false, RuntimeID: "runtime-1", DeviceTypeID: "devicetype-1"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait: func(u string, timeout time.Duration) error {
			t.Fatal("bootAndWait should not run when the substance mismatch could not be safely resolved")
			return nil
		},
		shutdown: neverShutdown(t),
		delete:   neverDelete(t),
	}

	err := ensureProvisioned(s, "with", "with", "", deps, time.Second)
	if err == nil {
		t.Fatal("ensureProvisioned returned nil for a substance mismatch while the slot's previous consumer is still alive — must refuse")
	}
}

// TestEnsureProvisioned_StrictSubstanceDisabledFallsBackToNameOnlyReuse
// proves the SIMPOOL_STRICT_SUBSTANCE=0 escape hatch actually disables the
// check rather than merely suppressing its side effects.
func TestEnsureProvisioned_StrictSubstanceDisabledFallsBackToNameOnlyReuse(t *testing.T) {
	t.Setenv(EnvStrictSubstance, "0")

	device, osVersion := "iPhone 17 Pro", "26.3"
	udid := "drifted-udid"
	s := fakeSlot(t, device, osVersion, udid)
	name := s.DeviceName()

	deps := provisionDeps{
		find: func(u string) (simctl.DeviceEntry, bool, error) {
			return simctl.DeviceEntry{UDID: udid, Name: name, State: "Booted", IsAvailable: false, RuntimeID: "runtime-OLD", DeviceTypeID: "devicetype-OLD"}, true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait:    func(u string, timeout time.Duration) error { return nil },
		shutdown:       neverShutdown(t),
		delete:         neverDelete(t),
	}

	if err := ensureProvisioned(s, "with", "with", "", deps, time.Second); err != nil {
		t.Fatalf("ensureProvisioned: %v", err)
	}
	if s.Meta.UDID != udid {
		t.Errorf("Meta.UDID = %q, want %q — SIMPOOL_STRICT_SUBSTANCE=0 must fall back to name-only reuse", s.Meta.UDID, udid)
	}
}
