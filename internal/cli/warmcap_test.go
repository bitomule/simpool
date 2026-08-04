package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// fakeBootedDevice makes findDevice report udid as a booted, correctly
// named device for slot n of groupDir under root — the "everything checks
// out" case enforceWarmCap's candidate scan requires before ever
// considering a slot for --warm accounting.
func fakeBootedDevice(t *testing.T, root, group string, byUDID map[string]int) {
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
			State:       "Booted",
			IsAvailable: true,
		}, true, nil
	}
}

// TestEnforceWarmCap_KeepsMostRecentlyUsedShutsDownRest proves --warm keeps
// the N most-recently-used free+booted slots and shuts down the rest,
// entirely independent of --cold/--max — the whole point of separating the
// residue cap from the concurrency cap (see reap.go's enforceWarmCap doc
// comment).
func TestEnforceWarmCap_KeepsMostRecentlyUsedShutsDownRest(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	group := filepath.Base(groupDir)

	now := time.Now()
	udids := map[int]string{0: "udid-oldest", 1: "udid-middle", 2: "udid-newest"}
	lastUsed := map[int]time.Time{0: now.Add(-3 * time.Hour), 1: now.Add(-2 * time.Hour), 2: now.Add(-1 * time.Hour)}

	byUDID := map[string]int{}
	for n, udid := range udids {
		dir := pool.SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, LastUsed: lastUsed[n]}); err != nil {
			t.Fatal(err)
		}
		byUDID[udid] = n
	}
	fakeBootedDevice(t, root, group, byUDID)

	var stdout, stderr bytes.Buffer
	enforceWarmCap(root, groupDir, 1, true /*dryRun*/, &stdout, &stderr)

	out := stdout.String()
	if !bytes.Contains([]byte(out), []byte("slot-0")) {
		t.Errorf("the oldest slot (slot-0) should be selected for shutdown, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("slot-1")) {
		t.Errorf("the middle slot (slot-1) should be selected for shutdown, got:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("slot-2")) {
		t.Errorf("the most-recently-used slot (slot-2) must be KEPT warm, not selected, got:\n%s", out)
	}

	// dry-run must never leave anything locked.
	for n := range udids {
		free, err := pool.IsSlotFree(pool.SlotDir(groupDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if !free {
			t.Fatalf("slot-%d should be free after enforceWarmCap, dry-run or not", n)
		}
	}
}

// TestEnforceWarmCap_SkipsBusyLeasedAndPoisonedSlots proves the --warm pass
// re-applies the same safety guards reapSlot's own idle/--cold path does —
// it must never touch a slot that is held, actively leased, or whose
// previous consumer might still be alive.
func TestEnforceWarmCap_SkipsBusyLeasedAndPoisonedSlots(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	group := filepath.Base(groupDir)

	// slot-0: held right now.
	busyDir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(busyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := pool.TryLock(pool.LockPath(busyDir))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	// slot-1: free, but actively leased.
	leasedDir := pool.SlotDir(groupDir, 1)
	if err := os.MkdirAll(leasedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(leasedDir, pool.Meta{UDID: "udid-leased", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteLease(leasedDir, pool.Lease{Key: "someone", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// slot-2: free, but poisoned — a live process (its own process group,
	// mirroring how `simpool with` fingerprints its child) still referenced
	// by ConsumerPGID.
	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	go func() { _ = orphan.Wait() }()

	poisonedDir := pool.SlotDir(groupDir, 2)
	if err := os.MkdirAll(poisonedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(poisonedDir, pool.Meta{
		UDID:         "udid-poisoned",
		Mode:         "with",
		ConsumerPGID: pgid,
		LastUsed:     time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// slot-3 and slot-4: free, valid, booted — the only real candidates.
	// Two of them (not one) so --warm 1, a cap RunReap can actually produce
	// (it gates on *warmCap > 0 — warmCap=0 never reaches this function),
	// still has something over-cap to select: slot-3 is older and gets
	// shut down, slot-4 is kept warm and silent.
	okDir := pool.SlotDir(groupDir, 3)
	if err := os.MkdirAll(okDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(okDir, pool.Meta{UDID: "udid-ok", LastUsed: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	newerDir := pool.SlotDir(groupDir, 4)
	if err := os.MkdirAll(newerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(newerDir, pool.Meta{UDID: "udid-ok-newer", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}

	fakeBootedDevice(t, root, group, map[string]int{
		"udid-leased":   1,
		"udid-poisoned": 2,
		"udid-ok":       3,
		"udid-ok-newer": 4,
	})

	var stdout, stderr bytes.Buffer
	enforceWarmCap(root, groupDir, 1 /*warmCap*/, true /*dryRun*/, &stdout, &stderr)

	out := stdout.String()
	for _, forbidden := range []string{"slot-0", "slot-1", "slot-2"} {
		if bytes.Contains([]byte(out), []byte(forbidden)) {
			t.Errorf("enforceWarmCap must never mention %s (busy/leased/poisoned), got:\n%s", forbidden, out)
		}
	}
	if !bytes.Contains([]byte(out), []byte("slot-3")) {
		t.Errorf("slot-3 is the older of the two safe candidates and should be selected over the --warm 1 cap, got:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("slot-4")) {
		t.Errorf("slot-4 is the most-recently-used safe candidate and must be kept within the --warm 1 cap, got:\n%s", out)
	}

	freeBusy, err := pool.IsSlotFree(busyDir)
	if err != nil {
		t.Fatal(err)
	}
	if freeBusy {
		t.Fatal("enforceWarmCap must never release a lock it didn't take")
	}
}

// TestRunReap_WarmDisabledByDefault proves --warm's zero value keeps
// today's behavior unchanged: no WARM decisions at all unless a caller
// explicitly opts in.
func TestRunReap_WarmDisabledByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(dir, pool.Meta{UDID: "udid-ok", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fakeBootedDevice(t, home, filepath.Base(groupDir), map[string]int{"udid-ok": 0})

	var stdout, stderr bytes.Buffer
	code := RunReap([]string{"--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("reap: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("WARM")) {
		t.Fatalf("--warm defaults to disabled (0); reap must not emit WARM decisions without it, got:\n%s", stdout.String())
	}
}

// TestEnforceWarmCap_ShutsDownOnlyOverCapSlots is the non-dry-run test the
// destructive branch previously had none of: every earlier warmcap test ran
// with dryRun=true, so no test ever reached the call that actually shuts a
// simulator down. This one proves shutdownWarm is called with exactly the
// over-cap slots' UDIDs and never the kept ones.
func TestEnforceWarmCap_ShutsDownOnlyOverCapSlots(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	group := filepath.Base(groupDir)

	now := time.Now()
	udids := map[int]string{0: "udid-oldest", 1: "udid-middle", 2: "udid-newest"}
	lastUsed := map[int]time.Time{0: now.Add(-3 * time.Hour), 1: now.Add(-2 * time.Hour), 2: now.Add(-1 * time.Hour)}

	byUDID := map[string]int{}
	for n, udid := range udids {
		dir := pool.SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, LastUsed: lastUsed[n]}); err != nil {
			t.Fatal(err)
		}
		byUDID[udid] = n
	}
	fakeBootedDevice(t, root, group, byUDID)

	origShutdown := shutdownWarm
	t.Cleanup(func() { shutdownWarm = origShutdown })
	var shutDown []string
	shutdownWarm = func(udid string) error {
		shutDown = append(shutDown, udid)
		return nil
	}

	var stdout, stderr bytes.Buffer
	enforceWarmCap(root, groupDir, 1 /*warmCap*/, false /*dryRun*/, &stdout, &stderr)

	want := map[string]bool{"udid-oldest": true, "udid-middle": true}
	if len(shutDown) != len(want) {
		t.Fatalf("got shutdownWarm calls %v, want exactly %v", shutDown, want)
	}
	for _, udid := range shutDown {
		if !want[udid] {
			t.Errorf("shutdownWarm called with unexpected udid %q", udid)
		}
		if udid == "udid-newest" {
			t.Error("the most-recently-used slot must be kept within the --warm cap, never shut down")
		}
	}

	free, err := pool.IsSlotFree(pool.SlotDir(groupDir, byUDID["udid-newest"]))
	if err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("enforceWarmCap must release the lock it re-took on the kept slot too")
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", stderr.String())
	}
}

// TestEnforceWarmCap_SkipsSlotThatBecameBusyBetweenPasses proves the second,
// shutdown-time lock+reclassify actually matters: a slot that was a valid
// candidate during the first (classify-then-release) pass but got claimed
// by something else before the second (shutdown) pass reaches it — a
// concurrent `simpool lease`/`with`/`acquire` — must be skipped, never shut
// down out from under its new holder. The race is made deterministic, not
// timing-dependent: slot-0 is processed (and its first-pass lock released)
// before slot-1 in ascending slot-number order, so hooking findDevice's call
// for slot-1 to grab slot-0's now-free lock reliably lands in the exact
// window enforceWarmCap's second pass needs to observe as busy.
func TestEnforceWarmCap_SkipsSlotThatBecameBusyBetweenPasses(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	group := filepath.Base(groupDir)
	oldestDir := pool.SlotDir(groupDir, 0)
	newestDir := pool.SlotDir(groupDir, 1)

	now := time.Now()
	if err := os.MkdirAll(oldestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(oldestDir, pool.Meta{UDID: "udid-oldest", LastUsed: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(newestDir, pool.Meta{UDID: "udid-newest", LastUsed: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var raceLock *pool.Lock
	origFind := findDevice
	t.Cleanup(func() { findDevice = origFind })
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		if udid == "udid-newest" && raceLock == nil {
			lock, err := pool.TryLock(pool.LockPath(oldestDir))
			if err != nil {
				t.Fatal(err)
			}
			raceLock = lock
			t.Cleanup(func() { _ = raceLock.Release() })
		}
		n := 0
		if udid == "udid-newest" {
			n = 1
		}
		return simctl.DeviceEntry{
			UDID:        udid,
			Name:        pool.DeviceNameForGroup(root, group, n),
			State:       "Booted",
			IsAvailable: true,
		}, true, nil
	}

	origShutdown := shutdownWarm
	t.Cleanup(func() { shutdownWarm = origShutdown })
	var shutDown []string
	shutdownWarm = func(udid string) error {
		shutDown = append(shutDown, udid)
		return nil
	}

	var stdout, stderr bytes.Buffer
	enforceWarmCap(root, groupDir, 0 /*warmCap*/, false /*dryRun*/, &stdout, &stderr)

	for _, udid := range shutDown {
		if udid == "udid-oldest" {
			t.Fatalf("a slot locked by someone else between the classify and shutdown passes must never be shut down, got calls: %v", shutDown)
		}
	}
	if bytes.Contains(stdout.Bytes(), []byte("slot-0")) {
		t.Errorf("a slot that became busy before the shutdown pass must not be reported as shut down, got:\n%s", stdout.String())
	}
	if len(shutDown) != 1 || shutDown[0] != "udid-newest" {
		t.Errorf("the still-free slot should still be shut down normally, got calls: %v", shutDown)
	}
}
