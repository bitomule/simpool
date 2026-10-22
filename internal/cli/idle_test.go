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

// idleFakes replaces every simctl call the idle-shutdown path can reach and
// records what was asked of it. A delete through any seam is recorded too,
// so a test can prove an idle shutdown never deletes anything.
type idleFakes struct {
	mu       sync.Mutex
	shut     []string
	deleted  []string
	booted   []string
	markerOn []bool
}

func installIdleFakes(t *testing.T, root, group string, states map[string]int, booted map[string]string) *idleFakes {
	t.Helper()
	f := &idleFakes{}
	fakeDeviceStates(t, root, group, states, booted)

	origList, origShut, origDel := listPoolDevices, shutdownIdle, deletePurged
	origWarm, origExcess, origExcessDel := shutdownWarm, shutdownExcess, deleteExcess
	origOrphan, origOrphanDel, origBoot := shutdownOrphan, deleteOrphan, warmFloorBoot
	t.Cleanup(func() {
		listPoolDevices, shutdownIdle, deletePurged = origList, origShut, origDel
		shutdownWarm, shutdownExcess, deleteExcess = origWarm, origExcess, origExcessDel
		shutdownOrphan, deleteOrphan, warmFloorBoot = origOrphan, origOrphanDel, origBoot
	})

	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	listPoolDevices = func() ([]simctl.DeviceEntry, error) {
		var out []simctl.DeviceEntry
		for udid := range states {
			d, _, _ := findDevice(udid)
			out = append(out, d)
		}
		return out, nil
	}
	shut := func(udid string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.shut = append(f.shut, udid)
		f.markerOn = append(f.markerOn, pool.ShutdownInProgress(pool.SlotDir(groupDir, states[udid])))
		return nil
	}
	del := func(udid string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, udid)
		return nil
	}
	shutdownIdle, shutdownWarm, shutdownExcess, shutdownOrphan = shut, shut, shut, shut
	deletePurged, deleteExcess, deleteOrphan = del, del, del
	warmFloorBoot = func(udid string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.booted = append(f.booted, udid)
		return nil
	}
	return f
}

// idlePool builds slots 0..len(idle)-1, each Booted and last used idle[n]
// ago.
func idlePool(t *testing.T, root string, idle ...time.Duration) (*idleFakes, string) {
	t.Helper()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	states, booted := map[string]int{}, map[string]string{}
	for n, d := range idle {
		dir := pool.SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		udid := "idle-udid-" + string(rune('a'+n))
		if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, LastUsed: time.Now().Add(-d)}); err != nil {
			t.Fatal(err)
		}
		states[udid], booted[udid] = n, "Booted"
	}
	return installIdleFakes(t, root, filepath.Base(groupDir), states, booted), groupDir
}

func reapIdle(root, dir string, n int, cold time.Duration, keepWarm bool) (string, string) {
	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, n, cold, 1 /*purgeMinutes*/, time.Hour, 3*time.Minute, false, false, 1 /*scrubMinutes*/, []string{"caches"}, keepWarm, &stdout, &stderr)
	return stdout.String(), stderr.String()
}

func TestReapSlot_ShutsDownASlotIdleBeyondCold(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, 6*time.Minute)
	dir := pool.SlotDir(groupDir, 0)

	out, errOut := reapIdle(root, dir, 0, 5*time.Minute, false)

	if len(f.shut) != 1 || f.shut[0] != "idle-udid-a" {
		t.Fatalf("a free slot idle 6m must be shut down at --cold 5m, shut=%v\nstdout:\n%s\nstderr:\n%s", f.shut, out, errOut)
	}
	if !f.markerOn[0] {
		t.Fatal("the shutdown ran without the marker that tells a lease to wait for it")
	}
	if pool.ShutdownInProgress(dir) {
		t.Fatal("the marker must be gone once the shutdown has finished")
	}
}

func TestReapSlot_KeepsASlotIdleUnderCold(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, 2*time.Minute)

	out, _ := reapIdle(root, pool.SlotDir(groupDir, 0), 0, 5*time.Minute, false)

	if len(f.shut) != 0 {
		t.Fatalf("a slot idle 2m must stay booted at --cold 5m, shut=%v", f.shut)
	}
	if !strings.Contains(out, "KEEP") {
		t.Fatalf("want KEEP, got:\n%s", out)
	}
}

func TestReapSlot_NeverShutsDownALeasedSlot(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, time.Hour)
	dir := pool.SlotDir(groupDir, 0)
	if err := pool.WriteLease(dir, pool.Lease{Key: "agent", ExpiresAt: time.Now().Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	reapIdle(root, dir, 0, time.Minute, false)

	if len(f.shut) != 0 {
		t.Fatalf("a slot under a live lease must never be shut down, however long since its last use, shut=%v", f.shut)
	}
}

func TestReapSlot_NeverShutsDownAHeldSlot(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, time.Hour)
	dir := pool.SlotDir(groupDir, 0)
	meta := pool.ReadMeta(dir)
	meta.Mode = "acquire"
	if err := pool.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	reapIdle(root, dir, 0, time.Minute, false)

	if len(f.shut) != 0 {
		t.Fatalf("a slot whose flock is held must never be shut down, shut=%v", f.shut)
	}
}

func TestReapSlot_IdleShutdownNeverDeletes(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, 48*time.Hour)

	reapIdle(root, pool.SlotDir(groupDir, 0), 0, time.Minute, false)

	if len(f.shut) != 1 {
		t.Fatalf("want the idle slot shut down, shut=%v", f.shut)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("an idle shutdown deleted %v: shutting down must never delete a simulator", f.deleted)
	}
	if _, err := os.Stat(pool.SlotDir(groupDir, 0)); err != nil {
		t.Fatalf("the slot directory must survive an idle shutdown: %v", err)
	}
}

func TestReapSlot_LeavesTheSlotsWarmKeepsBooted(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, time.Hour)

	reapIdle(root, pool.SlotDir(groupDir, 0), 0, time.Minute, true)

	if len(f.shut) != 0 {
		t.Fatalf("a slot --warm keeps must not be shut down by --cold only for the floor to boot it again, shut=%v", f.shut)
	}
}

func TestWarmKept_IsTheMostRecentlyUsedFreeSlots(t *testing.T) {
	root := t.TempDir()
	_, groupDir := idlePool(t, root, 3*time.Hour, 10*time.Minute, time.Hour, 20*time.Minute)
	if err := pool.WriteLease(pool.SlotDir(groupDir, 1), pool.Lease{Key: "agent", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}

	kept := warmKept(groupDir, 2)

	if len(kept) != 2 || !kept[3] || !kept[2] {
		t.Fatalf("--warm 2 keeps the two most recently used free slots (3 and 2; 1 is leased), got %v", kept)
	}
	if warmKept(groupDir, 0) != nil {
		t.Fatal("--warm 0 keeps nothing")
	}
}

func runReap(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := RunReap(args, &stdout, &stderr); code != 0 {
		t.Fatalf("reap %v exited %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

func poolRootFor(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	root, err := pool.Root()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunReap_FastPassShutsDownIdleSlotsAndLogsOnlyWhatItDid(t *testing.T) {
	root := poolRootFor(t)
	f, groupDir := idlePool(t, root, 10*time.Minute, time.Minute)
	if err := os.MkdirAll(pool.SlotDir(groupDir, 2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stampFullPass(root, time.Now()); err != nil {
		t.Fatal(err)
	}

	out, _ := runReap(t, "--cold", "5m", "--full-every", "30m")

	if len(f.shut) != 1 || f.shut[0] != "idle-udid-a" {
		t.Fatalf("the fast pass must shut down the slot idle 10m and only that one, shut=%v\n%s", f.shut, out)
	}
	if strings.Contains(out, "KEEP") || strings.Contains(out, "IDLE") {
		t.Fatalf("the fast pass must log only what it did, got:\n%s", out)
	}
}

func TestRunReap_FastPassSkipsTheExpensivePasses(t *testing.T) {
	root := poolRootFor(t)
	f, _ := idlePool(t, root, 10*time.Minute)
	booted := findDevice
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		d, ok, err := booted(udid)
		d.State = "Shutdown"
		return d, ok, err
	}
	withFreeMemory(t, 0.9, true)

	if err := stampFullPass(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	runReap(t, "--full-every", "30m", "--warm", "1", "--max", "0", "--prune-ghosts=false")
	if len(f.booted) != 0 {
		t.Fatalf("the fast pass ran --warm and booted %v", f.booted)
	}

	if err := stampFullPass(root, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	runReap(t, "--full-every", "30m", "--warm", "1", "--max", "0", "--prune-ghosts=false")
	if len(f.booted) != 1 {
		t.Fatalf("once --full-every has elapsed --warm must run again, booted=%v", f.booted)
	}
}

func TestRunReap_FullPassRunsOnceFullEveryHasElapsed(t *testing.T) {
	root := poolRootFor(t)
	idlePool(t, root, 10*time.Minute)
	old := time.Now().Add(-31 * time.Minute)
	if err := stampFullPass(root, old); err != nil {
		t.Fatal(err)
	}

	runReap(t, "--cold", "5m", "--full-every", "30m", "--max", "0", "--prune-ghosts=false")

	info, err := os.Stat(filepath.Join(root, fullPassStampFile))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old.Add(time.Minute)) {
		t.Fatal("a full pass ran past --full-every without recording itself, so every run would be full")
	}
	if fullPassDue(root, 30*time.Minute, time.Now()) {
		t.Fatal("right after a full pass the next run must be a fast one")
	}
	if !fullPassDue(root, 0, time.Now()) {
		t.Fatal("--full-every 0 makes every run a full pass")
	}
}

func TestParseIdleDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"60":  time.Hour,
		"0":   0,
		"5m":  5 * time.Minute,
		"90s": 90 * time.Second,
	} {
		got, err := parseIdleDuration(in)
		if err != nil || got != want {
			t.Errorf("parseIdleDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "-5m", "five", ""} {
		if _, err := parseIdleDuration(in); err == nil {
			t.Errorf("parseIdleDuration(%q) must fail", in)
		}
	}
}

func TestDefaultIdleShutdown_ComesFromTheEnvironment(t *testing.T) {
	t.Setenv(EnvIdleShutdown, "")
	if d, err := defaultIdleShutdown(); err != nil || d != 0 {
		t.Fatalf("unset must keep --cold's old default of 0: %v, %v", d, err)
	}
	t.Setenv(EnvIdleShutdown, "7m")
	if d, err := defaultIdleShutdown(); err != nil || d != 7*time.Minute {
		t.Fatalf("got %v, %v; want 7m", d, err)
	}
	t.Setenv(EnvIdleShutdown, "soon")
	if _, err := defaultIdleShutdown(); err == nil {
		t.Fatal("a value reap cannot read must fail loudly, not silently fall back to another idle time")
	}
}
