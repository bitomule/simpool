package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// capHarness builds a group with the given slots and wires the simctl seams
// enforceSlotCap uses to a fake device set, recording every shutdown and
// delete it performs. Each slot gets a correctly-named, shut-down device.
type capHarness struct {
	root     string
	groupDir string
	group    string
	deleted  []string
	shutdown []string
}

func newCapHarness(t *testing.T, lastUsed map[int]time.Time) *capHarness {
	t.Helper()
	h := &capHarness{root: t.TempDir()}
	h.groupDir = pool.GroupDir(h.root, "TestDevice", "1.0")
	h.group = filepath.Base(h.groupDir)

	byUDID := map[string]int{}
	for n, lu := range lastUsed {
		dir := pool.SlotDir(h.groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		udid := udidFor(n)
		if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, LastUsed: lu}); err != nil {
			t.Fatal(err)
		}
		byUDID[udid] = n
	}

	origFind, origShut, origDel := findDevice, shutdownExcess, deleteExcess
	t.Cleanup(func() { findDevice, shutdownExcess, deleteExcess = origFind, origShut, origDel })

	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		n, ok := byUDID[udid]
		if !ok {
			return simctl.DeviceEntry{}, false, nil
		}
		return simctl.DeviceEntry{
			UDID:        udid,
			Name:        pool.DeviceNameForGroup(h.root, h.group, n),
			State:       "Shutdown",
			IsAvailable: true,
		}, true, nil
	}
	shutdownExcess = func(udid string) error { h.shutdown = append(h.shutdown, udid); return nil }
	deleteExcess = func(udid string) error { h.deleted = append(h.deleted, udid); return nil }
	return h
}

func udidFor(n int) string { return "udid-slot-" + string(rune('0'+n)) }

func (h *capHarness) slotDirs(t *testing.T) []int {
	t.Helper()
	nums, err := pool.ListSlotNumbersChecked(h.groupDir)
	if err != nil {
		t.Fatalf("listing slots: %v", err)
	}
	sort.Ints(nums)
	return nums
}

// TestEnforceSlotCap_TrimsAnOversizedGroupBackToMax is the regression test
// for the bug that filled a 460 GB disk: a group holding more slots than
// --max keeps every one of them forever. `max` used to be checked in
// exactly two places (AcquireSlots and AcquireLease), both of which only
// ever refuse to CREATE slot number max+1 — so a group that got past the
// cap once, by any route, stayed past it, and every extra slot kept its own
// multi-gigabyte simulator alive indefinitely.
//
// Seven slots against a cap of three is not a hypothetical: it is the exact
// state measured on the machine this was written for, seventeen days after
// the last slot was created.
func TestEnforceSlotCap_TrimsAnOversizedGroupBackToMax(t *testing.T) {
	now := time.Now()
	lastUsed := map[int]time.Time{}
	for n := 0; n < 7; n++ {
		// slot-0 oldest ... slot-6 newest, all well past slotCapGrace.
		lastUsed[n] = now.Add(-time.Duration(10-n) * time.Hour)
	}
	h := newCapHarness(t, lastUsed)

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 3, false /*dryRun*/, &stdout, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr:\n%s", stderr.String())
	}
	got := h.slotDirs(t)
	want := []int{4, 5, 6} // the three most-recently-used survive
	if len(got) != len(want) {
		t.Fatalf("group still has %d slot directories %v, want %v (--max 3)\n%s", len(got), got, want, stdout.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kept slots %v, want %v (the most-recently-used ones)\n%s", got, want, stdout.String())
		}
	}
	if len(h.deleted) != 4 {
		t.Fatalf("deleted %d simulators %v, want the 4 over the cap\n%s", len(h.deleted), h.deleted, stdout.String())
	}
	for n := 0; n < 4; n++ {
		if !containsStr(h.deleted, udidFor(n)) {
			t.Errorf("simulator for slot-%d was not deleted; deleted=%v", n, h.deleted)
		}
	}
}

// TestEnforceSlotCap_WithinCapDoesNothing is the other half of the
// ablation: the pass must be inert for a group that is already inside its
// cap, so enabling it by default cannot cost anyone a simulator they were
// entitled to.
func TestEnforceSlotCap_WithinCapDoesNothing(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
		1: now.Add(-8 * time.Hour),
		2: now.Add(-7 * time.Hour),
	})

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 3, false, &stdout, &stderr)

	if len(h.deleted) != 0 || len(h.shutdown) != 0 {
		t.Fatalf("a group at exactly --max must be left alone, deleted=%v shutdown=%v", h.deleted, h.shutdown)
	}
	if len(h.slotDirs(t)) != 3 {
		t.Fatalf("slot directories changed: %v", h.slotDirs(t))
	}
	if stdout.Len() != 0 {
		t.Fatalf("unexpected output for a group within cap:\n%s", stdout.String())
	}
}

// TestEnforceSlotCap_NeverTouchesAnOccupiedSlot proves the pass inherits
// every safety rule the rest of reap follows. A slot under a live lease, a
// slot used more recently than slotCapGrace, and a slot whose flock is held
// are all untouchable — and each one still counts against the cap, so the
// eviction pressure lands on the genuinely idle slots instead of being
// silently dropped.
func TestEnforceSlotCap_NeverTouchesAnOccupiedSlot(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),   // idle, coldest  -> evictable
		1: now.Add(-8 * time.Hour),   // idle           -> evictable
		2: now.Add(-7 * time.Hour),   // will be leased -> untouchable
		3: now.Add(-1 * time.Minute), // used just now -> inside the grace
		4: now.Add(-6 * time.Hour),   // will be locked -> untouchable
	})

	leasedDir := pool.SlotDir(h.groupDir, 2)
	if err := pool.WriteLease(leasedDir, pool.Lease{Key: "repo-a", ExpiresAt: now.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	held, err := pool.TryLock(pool.LockPath(pool.SlotDir(h.groupDir, 4)))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	var stdout, stderr bytes.Buffer
	// --max 2 with two hard-untouchable slots (2 leased, 4 locked) plus
	// slot-3 protected only by slotCapGrace means keep = 2-2 = 0: nothing
	// but the grace itself keeps slot-3 alive here, so this is the only
	// case in the file where slotCapGrace is load-bearing rather than
	// redundant with MRU-first keep.
	enforceSlotCap(h.root, h.groupDir, 2, false, &stdout, &stderr)

	for _, udid := range []string{udidFor(2), udidFor(3), udidFor(4)} {
		if containsStr(h.deleted, udid) {
			t.Errorf("deleted an occupied slot's simulator %s; deleted=%v\n%s", udid, h.deleted, stdout.String())
		}
	}
	got := h.slotDirs(t)
	if len(got) != 3 {
		t.Fatalf("want slots [2 3 4] to survive, got %v\n%s", got, stdout.String())
	}
	if _, err := os.Stat(pool.SlotDir(h.groupDir, 3)); err != nil {
		t.Fatalf("slot-3's directory should still exist, protected by slotCapGrace: %v", err)
	}
}

// TestEnforceSlotCap_RefusesADeviceThatIsNotThisSlots is the guard that
// makes this pass safe to have on by default: it holds the power to delete
// a simulator, so it must verify the device's actual name in the shared
// default device set against the name derived from the slot's own
// directory, never trust a UDID out of a meta.json that is explicitly
// allowed to be stale. The default set also holds the user's own
// simulators.
func TestEnforceSlotCap_RefusesADeviceThatIsNotThisSlots(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
		1: now.Add(-8 * time.Hour),
	})

	orig := findDevice
	t.Cleanup(func() { findDevice = orig })
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		// Every lookup reports somebody else's simulator.
		return simctl.DeviceEntry{UDID: udid, Name: "David's iPhone 17 Pro", State: "Shutdown", IsAvailable: true}, true, nil
	}

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 1, false, &stdout, &stderr)

	if len(h.deleted) != 0 {
		t.Fatalf("deleted a simulator that is not this slot's own: %v\n%s", h.deleted, stdout.String())
	}
	if len(h.slotDirs(t)) != 2 {
		t.Fatalf("removed a slot directory whose device could not be confirmed: %v", h.slotDirs(t))
	}
}

// TestEnforceSlotCap_DryRunChangesNothing keeps `--dry-run` honest for the
// one pass in reap that deletes simulators without being asked to.
func TestEnforceSlotCap_DryRunChangesNothing(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
		1: now.Add(-8 * time.Hour),
		2: now.Add(-7 * time.Hour),
		3: now.Add(-6 * time.Hour),
	})

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 2, true /*dryRun*/, &stdout, &stderr)

	if len(h.deleted) != 0 || len(h.shutdown) != 0 {
		t.Fatalf("--dry-run acted: deleted=%v shutdown=%v", h.deleted, h.shutdown)
	}
	if len(h.slotDirs(t)) != 4 {
		t.Fatalf("--dry-run removed slot directories: %v", h.slotDirs(t))
	}
	if !bytes.Contains(stdout.Bytes(), []byte("would delete")) {
		t.Fatalf("--dry-run reported nothing:\n%s", stdout.String())
	}
	// A dry run must not leave anything locked behind.
	for _, n := range h.slotDirs(t) {
		l, err := pool.TryLock(pool.LockPath(pool.SlotDir(h.groupDir, n)))
		if err != nil {
			t.Fatalf("slot-%d still locked after --dry-run: %v", n, err)
		}
		l.Release()
	}
}

// TestEnforceSlotCap_RecoversAZeroUDIDSlotByName proves a slot whose
// meta.json was lost (crash mid-write, disk full, a human `rm`) before a
// UDID was ever recorded is not treated as "nothing to delete": if a
// device sitting in the default set under this slot's own deterministic
// name is found, the pass recovers it and deletes it exactly like any
// other evictable slot, rather than removing only the directory and
// leaking the simulator forever.
func TestEnforceSlotCap_RecoversAZeroUDIDSlotByName(t *testing.T) {
	now := time.Now()
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
		1: now.Add(-8 * time.Hour),
	})
	// slot-2: a directory with no meta.json at all — never got as far as
	// recording a UDID.
	if err := os.MkdirAll(pool.SlotDir(h.groupDir, 2), 0o755); err != nil {
		t.Fatal(err)
	}
	recoveredUDID := "udid-recovered-slot-2"
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: recoveredUDID, Name: pool.DeviceNameForGroup(h.root, h.group, 2), State: "Shutdown", IsAvailable: true},
	})

	var stdout, stderr bytes.Buffer
	enforceSlotCap(h.root, h.groupDir, 2, false, &stdout, &stderr)

	if !containsStr(h.deleted, recoveredUDID) {
		t.Fatalf("zero-UDID slot's recovered device was not deleted; deleted=%v\n%s", h.deleted, stdout.String())
	}
	got := h.slotDirs(t)
	want := []int{0, 1}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got slots %v, want %v (slot-2's directory removed along with its recovered device)", got, want)
	}
}

// TestEnforceSlotCap_RefusesAmbiguousZeroUDIDRecovery proves that when more
// than one device in the default set carries this slot's deterministic
// name, the pass refuses to guess which one is really this slot's, the
// same refuse-to-guess rule ensureProvisioned's own by-name recovery
// follows.
func TestEnforceSlotCap_RefusesAmbiguousZeroUDIDRecovery(t *testing.T) {
	now := time.Now()
	// slot-0: idle, evictable free candidate.
	// slot-2: leased, hard-blocked, forces the group over --max 1 so the
	// pass actually runs classification instead of exiting early at the
	// "already within cap" check.
	h := newCapHarness(t, map[int]time.Time{
		0: now.Add(-9 * time.Hour),
	})
	if err := os.MkdirAll(pool.SlotDir(h.groupDir, 1), 0o755); err != nil {
		t.Fatal(err)
	}
	leasedDir := pool.SlotDir(h.groupDir, 2)
	if err := os.MkdirAll(leasedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteLease(leasedDir, pool.Lease{Key: "repo-a", ExpiresAt: now.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	name := pool.DeviceNameForGroup(h.root, h.group, 1)
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-ambiguous-a", Name: name, State: "Shutdown", IsAvailable: true},
		{UDID: "udid-ambiguous-b", Name: name, State: "Shutdown", IsAvailable: true},
	})

	var stdout, stderr bytes.Buffer
	// --max 1 with slot-1 (ambiguous) and slot-2 (leased) both blocked
	// already meets the cap on its own, so only slot-0 is ever evicted —
	// isolating the assertion to whether the ambiguous slot was touched.
	enforceSlotCap(h.root, h.groupDir, 1, false, &stdout, &stderr)

	for _, udid := range []string{"udid-ambiguous-a", "udid-ambiguous-b"} {
		if containsStr(h.deleted, udid) {
			t.Fatalf("deleted a device from an ambiguous zero-UDID match: %v\n%s", h.deleted, stdout.String())
		}
	}
	got := h.slotDirs(t)
	want := []int{1, 2}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got surviving slots %v, want %v (slot-0 evicted, ambiguous slot-1 and leased slot-2 kept)", got, want)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
