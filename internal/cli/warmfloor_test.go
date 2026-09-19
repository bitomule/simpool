package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// fakeDeviceStates makes findDevice report each slot's device with the state
// this test wants — the floor's mirror of fakeBootedDevice, which can only
// produce Booted ones.
func fakeDeviceStates(t *testing.T, root, group string, byUDID map[string]int, state map[string]string) {
	t.Helper()
	orig := findDevice
	t.Cleanup(func() { findDevice = orig })
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		n, ok := byUDID[udid]
		if !ok {
			return simctl.DeviceEntry{}, false, nil
		}
		return simctl.DeviceEntry{
			UDID:        udid,
			Name:        pool.DeviceNameForGroup(root, group, n),
			State:       state[udid],
			IsAvailable: true,
		}, true, nil
	}
}

// withFreeMemory pins what the floor believes about the machine, so both
// sides of the threshold are testable without a machine in that state.
func withFreeMemory(t *testing.T, fraction float64, ok bool) {
	t.Helper()
	orig := freeMemoryFraction
	t.Cleanup(func() { freeMemoryFraction = orig })
	freeMemoryFraction = func() (float64, bool) { return fraction, ok }
}

// recordBoots captures which UDIDs the floor decides to boot.
func recordBoots(t *testing.T) *[]string {
	t.Helper()
	orig := warmFloorBoot
	t.Cleanup(func() { warmFloorBoot = orig })
	var mu sync.Mutex
	booted := []string{}
	warmFloorBoot = func(udid string) error {
		mu.Lock()
		defer mu.Unlock()
		booted = append(booted, udid)
		return nil
	}
	return &booted
}

// warmFloorPool builds a group of slots with the given device states.
func warmFloorPool(t *testing.T, root string, states map[int]string, lastUsed map[int]time.Time) (groupDir, group string) {
	t.Helper()
	groupDir = pool.GroupDir(root, "TestDevice", "1.0")
	group = filepath.Base(groupDir)
	byUDID := map[string]int{}
	byState := map[string]string{}
	for n, state := range states {
		dir := pool.SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		udid := "udid-" + string(rune('a'+n))
		lu := time.Now().Add(-time.Duration(n+1) * time.Hour)
		if v, ok := lastUsed[n]; ok {
			lu = v
		}
		if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, LastUsed: lu}); err != nil {
			t.Fatal(err)
		}
		byUDID[udid] = n
		byState[udid] = state
	}
	fakeDeviceStates(t, root, group, byUDID, byState)
	return groupDir, group
}

// TestEnforceWarmFloor_BootsNothingWhenAlreadyWarm is the control, and it
// goes first for the same reason every measurement today did: prove the
// thing can report "nothing to do" before trusting it to act. A group that
// already has its --warm N simulators booted must boot nothing at all. A
// floor that fires when the floor is already met would boot a simulator on
// every scheduled reap pass, forever.
func TestEnforceWarmFloor_BootsNothingWhenAlreadyWarm(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Booted", 1: "Booted"}, nil)
	withFreeMemory(t, 0.90, true)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, false, &stdout, &stderr)

	if len(*booted) != 0 {
		t.Errorf("the floor is already met; nothing may be booted, got %v", *booted)
	}
	if out := stdout.String(); out != "" {
		t.Errorf("nothing to do must say nothing, got:\n%s", out)
	}
}

// TestEnforceWarmFloor_BootsUpToTheFloor is the change: a group below its
// --warm N gets shut-down slots booted until it reaches N, and not one more.
// Most-recently-used first, the same preference the acquisition path sorts
// by — the slot most likely to be asked for next is the one worth warming.
func TestEnforceWarmFloor_BootsUpToTheFloor(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	groupDir, _ := warmFloorPool(t, root,
		map[int]string{0: "Shutdown", 1: "Shutdown", 2: "Shutdown"},
		map[int]time.Time{0: now.Add(-3 * time.Hour), 1: now.Add(-1 * time.Hour), 2: now.Add(-2 * time.Hour)})
	withFreeMemory(t, 0.90, true)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, false, &stdout, &stderr)

	if len(*booted) != 2 {
		t.Fatalf("want exactly 2 boots to reach a floor of 2, got %v", *booted)
	}
	// slot-1 is the most recently used, slot-2 next, slot-0 oldest.
	if (*booted)[0] != "udid-b" || (*booted)[1] != "udid-c" {
		t.Errorf("want the two most-recently-used slots booted (udid-b then udid-c), got %v", *booted)
	}
}

// TestEnforceWarmFloor_CountsAlreadyBootedTowardsTheFloor keeps the two
// halves of --warm from fighting: the ceiling pass keeps N booted, so the
// floor must count those and only make up the difference.
func TestEnforceWarmFloor_CountsAlreadyBootedTowardsTheFloor(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Booted", 1: "Shutdown", 2: "Shutdown"}, nil)
	withFreeMemory(t, 0.90, true)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, false, &stdout, &stderr)

	if len(*booted) != 1 {
		t.Errorf("one slot is already warm and the floor is 2; want exactly 1 boot, got %v", *booted)
	}
}

// TestEnforceWarmFloor_RefusesWhenMemoryIsTight is the guard David asked
// for, and the reason is measured rather than cautious: Undolly's snapshot
// suite died seven times in one day for memory, and free memory at launch
// separated the outcomes cleanly — 2 of 2 runs that finished were above 58%
// free, 7 of 7 that died were below. Warming is speculative by definition,
// so it is the first thing that must give way; a simulator booted at the
// wrong moment turns somebody else's passing suite into a failure, for a
// slot that may never be used.
func TestEnforceWarmFloor_RefusesWhenMemoryIsTight(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Shutdown", 1: "Shutdown"}, nil)
	withFreeMemory(t, 0.10, true)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, false, &stdout, &stderr)

	if len(*booted) != 0 {
		t.Fatalf("memory is tight; nothing may be booted, got %v", *booted)
	}
	out := stdout.String()
	if !strings.Contains(out, "not warming") {
		t.Errorf("declining must be stated, not silent — that is the defect just fixed in the wait loop; got:\n%s", out)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("the message must carry the number it decided on, got:\n%s", out)
	}
}

// TestEnforceWarmFloor_RefusesWhenMemoryIsUnknowable applies this codebase's
// uniform rule for a check that could not complete: never read it as
// permission. An unreadable machine state is not a green light for
// speculative work.
func TestEnforceWarmFloor_RefusesWhenMemoryIsUnknowable(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Shutdown"}, nil)
	withFreeMemory(t, 0, false)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 1, false, &stdout, &stderr)

	if len(*booted) != 0 {
		t.Fatalf("free memory could not be read; nothing may be booted, got %v", *booted)
	}
	if !strings.Contains(stdout.String(), "could not read free memory") {
		t.Errorf("must say why it declined, got:\n%s", stdout.String())
	}
}

// TestEnforceWarmFloor_StopsWhenMemoryRunsDownMidPass pins the failure a
// single up-front check would miss: each simulator this loop starts consumes
// memory itself, so a group several slots short could walk the machine past
// the floor one boot at a time while still quoting the comfortable figure it
// measured before the first one.
func TestEnforceWarmFloor_StopsWhenMemoryRunsDownMidPass(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Shutdown", 1: "Shutdown", 2: "Shutdown"}, nil)

	// Comfortable to begin with, tight after the first boot.
	var mu sync.Mutex
	calls := 0
	orig := freeMemoryFraction
	t.Cleanup(func() { freeMemoryFraction = orig })
	freeMemoryFraction = func() (float64, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return 0.90, true
		}
		return 0.05, true
	}
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 3, false, &stdout, &stderr)

	if len(*booted) != 1 {
		t.Fatalf("memory went tight after the first boot; want exactly 1, got %v", *booted)
	}
	if !strings.Contains(stdout.String(), "stopping short") {
		t.Errorf("stopping early must be stated with the reason, got:\n%s", stdout.String())
	}
}

// TestEnforceWarmFloor_SkipsUnsafeSlots proves the floor re-applies the same
// guards the ceiling pass does. A leased or poisoned slot is not a cold slot
// waiting to be warmed — booting somebody's leased device out from under
// them, or a slot whose previous consumer may still be alive, is exactly
// what these checks exist to prevent.
func TestEnforceWarmFloor_SkipsUnsafeSlots(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Shutdown", 1: "Shutdown"}, nil)
	withFreeMemory(t, 0.90, true)
	booted := recordBoots(t)

	// A live lease on slot-0.
	if err := pool.WriteLease(pool.SlotDir(groupDir, 0), pool.Lease{
		Key:       "someone-else",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, false, &stdout, &stderr)

	for _, u := range *booted {
		if u == "udid-a" {
			t.Errorf("slot-0 carries a live lease and must never be booted by this pass, got %v", *booted)
		}
	}
}

// TestEnforceWarmFloor_DryRunBootsNothing keeps --dry-run honest: it must
// report what it would do and start no simulator.
func TestEnforceWarmFloor_DryRunBootsNothing(t *testing.T) {
	root := t.TempDir()
	groupDir, _ := warmFloorPool(t, root, map[int]string{0: "Shutdown", 1: "Shutdown"}, nil)
	withFreeMemory(t, 0.90, true)
	booted := recordBoots(t)

	var stdout, stderr bytes.Buffer
	enforceWarmFloor(root, groupDir, 2, true /*dryRun*/, &stdout, &stderr)

	if len(*booted) != 0 {
		t.Fatalf("dry-run must boot nothing, got %v", *booted)
	}
	if !strings.Contains(stdout.String(), "would boot") {
		t.Errorf("dry-run must say what it would do, got:\n%s", stdout.String())
	}
	for n := 0; n < 2; n++ {
		free, err := pool.IsSlotFree(pool.SlotDir(groupDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if !free {
			t.Fatalf("slot-%d must be free after the pass, dry-run or not", n)
		}
	}
}

// TestWarmFloorThreshold_OverrideAndItsRefusals pins the one thing that
// must not happen to a safety threshold made configurable: a malformed value
// silently becoming 0, which would disable the guard entirely while looking
// like it was set on purpose. Anything unparseable or out of range falls back
// to the default instead.
func TestWarmFloorThreshold_OverrideAndItsRefusals(t *testing.T) {
	t.Setenv(EnvWarmMinFree, "")
	if got := warmFloorThreshold(); got != warmFloorMinFreeMemory {
		t.Errorf("unset must use the default, got %v", got)
	}
	t.Setenv(EnvWarmMinFree, "60")
	if got := warmFloorThreshold(); got != 0.60 {
		t.Errorf("60 must mean 60%%, got %v", got)
	}
	t.Setenv(EnvWarmMinFree, "0")
	if got := warmFloorThreshold(); got != 0 {
		t.Errorf("an explicit 0 is a real choice and must be honoured, got %v", got)
	}
	for _, bad := range []string{"abc", "-1", "101", "0.35", "35%"} {
		t.Setenv(EnvWarmMinFree, bad)
		if got := warmFloorThreshold(); got != warmFloorMinFreeMemory {
			t.Errorf("%q must fall back to the default, not disable the guard; got %v", bad, got)
		}
	}
}

// TestFreeMemoryFraction_ReadsTheRealMachine is a smoke test on the live
// probe, not a pinned value: it must come back plausible on whatever machine
// runs it. A probe that silently returns 0 would make the floor decline
// forever, which looks exactly like the floor not being implemented.
func TestFreeMemoryFraction_ReadsTheRealMachine(t *testing.T) {
	got, ok := liveFreeMemoryFraction()
	if !ok {
		t.Skip("could not read memory on this machine; nothing to assert")
	}
	if got <= 0 || got > 1 {
		t.Fatalf("free memory fraction %v is not a fraction — the floor would misjudge every pass", got)
	}
}
