package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// Every other test in this package runs RunReap or RunDoctor, which now read
// this host's runtimes. These keep them off the real host; the tests below
// install their own fakes.
func init() {
	listRuntimeImages = func() ([]simctl.RuntimeImage, error) { return nil, nil }
	ghostPass = func(string, bool, io.Writer, io.Writer) {}
}

// runtimeListFixture is `xcrun simctl runtime list -j` as captured on the
// machine this was written for (13 Oct 2026), with the iOS 27.0 image that
// had just been deleted by hand put back in.
const runtimeListFixture = `{
  "2D066EDC-2F36-49B5-95DE-6D9D678160CE" : {
    "build" : "23D8133", "deletable" : true,
    "identifier" : "2D066EDC-2F36-49B5-95DE-6D9D678160CE",
    "kind" : "Patchable Cryptex Disk Image",
    "lastUsedAt" : "2026-10-13T18:50:13Z",
    "platformIdentifier" : "com.apple.platform.iphonesimulator",
    "runtimeIdentifier" : "com.apple.CoreSimulator.SimRuntime.iOS-26-3",
    "signatureState" : "Verified", "sizeBytes" : 8393503393,
    "state" : "Ready", "version" : "26.3.1"
  },
  "9A3F0C11-0000-4000-8000-000000000270" : {
    "build" : "24A335", "deletable" : true,
    "identifier" : "9A3F0C11-0000-4000-8000-000000000270",
    "kind" : "Patchable Cryptex Disk Image",
    "lastUsedAt" : "2026-10-13T09:00:00Z",
    "platformIdentifier" : "com.apple.platform.iphonesimulator",
    "runtimeIdentifier" : "com.apple.CoreSimulator.SimRuntime.iOS-27-0",
    "signatureState" : "Verified", "sizeBytes" : 8400000000,
    "state" : "Ready", "version" : "27.0"
  },
  "4F1C26F9-1B22-4DA3-B7C2-1F18ED45B732" : {
    "build" : "24A94401", "deletable" : true,
    "identifier" : "4F1C26F9-1B22-4DA3-B7C2-1F18ED45B732",
    "kind" : "Patchable Cryptex Disk Image",
    "lastUsedAt" : "2026-10-12T16:46:03Z",
    "platformIdentifier" : "com.apple.platform.iphonesimulator",
    "runtimeIdentifier" : "com.apple.CoreSimulator.SimRuntime.iOS-27-1",
    "signatureState" : "Unknown", "sizeBytes" : 7873719649,
    "state" : "Ready", "version" : "27.1"
  }
}`

const (
	image263 = "2D066EDC-2F36-49B5-95DE-6D9D678160CE"
	image270 = "9A3F0C11-0000-4000-8000-000000000270"
	image271 = "4F1C26F9-1B22-4DA3-B7C2-1F18ED45B732"
)

var xcode271 = simctl.SimulatorSDK{Xcode: "/Applications/Xcode-27.1.0-Release.Candidate.app", Platform: "com.apple.platform.iphonesimulator", Version: "27.1"}

func fixtureImages(t *testing.T) []simctl.RuntimeImage {
	t.Helper()
	imgs, err := simctl.ParseRuntimeImages([]byte(runtimeListFixture))
	if err != nil {
		t.Fatal(err)
	}
	return imgs
}

func verdictFor(t *testing.T, vs []runtimeVerdict, id string) runtimeVerdict {
	t.Helper()
	for _, v := range vs {
		if v.Image.Identifier == id {
			return v
		}
	}
	t.Fatalf("no verdict for image %s", id)
	return runtimeVerdict{}
}

var testNow = time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)

func facts(t *testing.T, groups []groupUsage, running map[string]string, sdks ...simctl.SimulatorSDK) runtimeFacts {
	if running == nil {
		running = map[string]string{}
	}
	return runtimeFacts{Images: fixtureImages(t), SDKs: sdks, Groups: groups, Running: running, Now: testNow, Window: DefaultStaleAfter}
}

// The incident this exists for: one Xcode (27.1 SDK), four simpool groups
// pinned to 27.0 last used three weeks ago, nothing booted. 27.1 stays for
// Xcode; 27.0 and 26.3.1 have nothing left asking for them.
func TestDecideRuntimes_OnlyTheInstalledXcodesRuntimeSurvivesStaleGroups(t *testing.T) {
	stale := testNow.Add(-21 * 24 * time.Hour)
	groups := []groupUsage{
		{Name: "iPhone-17-Pro@27.0", OSVersion: "27.0", LastUsed: stale},
		{Name: "iPad-Pro-11-inch-M5@27.0", OSVersion: "27.0", LastUsed: stale},
		{Name: "iPhone-17-Pro-Max@27.0", OSVersion: "27.0", LastUsed: stale},
		{Name: "iPhone-SE-3rd-generation@27.0", OSVersion: "27.0", LastUsed: stale},
	}
	vs := decideRuntimes(facts(t, groups, nil, xcode271))

	if v := verdictFor(t, vs, image271); !v.Needed || !strings.Contains(v.Reason, "Xcode-27.1.0-Release.Candidate.app") {
		t.Errorf("27.1 must be kept for the installed Xcode, got %+v", v)
	}
	if v := verdictFor(t, vs, image270); v.Needed {
		t.Errorf("27.0 is asked for only by groups idle for 21d, must be unused, got needed: %s", v.Reason)
	}
	if v := verdictFor(t, vs, image263); v.Needed {
		t.Errorf("26.3.1 is wanted by nothing, must be unused, got needed: %s", v.Reason)
	}
}

// A group asking for "26.3" resolves to the 26.3.1 image (ResolveRuntime's
// prefix rule), so using it two days ago must keep that image.
func TestDecideRuntimes_RecentlyUsedGroupKeepsItsRuntime(t *testing.T) {
	groups := []groupUsage{{Name: "iPhone-17-Pro@26.3", OSVersion: "26.3", LastUsed: testNow.Add(-48 * time.Hour)}}
	vs := decideRuntimes(facts(t, groups, nil, xcode271))
	if v := verdictFor(t, vs, image263); !v.Needed || !strings.Contains(v.Reason, "iPhone-17-Pro@26.3") {
		t.Errorf("26.3.1 must be kept for a group used 2d ago, got %+v", v)
	}
}

// Age stops mattering the moment a slot is held or leased.
func TestDecideRuntimes_HeldOrLeasedSlotKeepsRuntimeHoweverOld(t *testing.T) {
	groups := []groupUsage{{Name: "iPhone-17-Pro@27.0", OSVersion: "27.0", LastUsed: testNow.Add(-90 * 24 * time.Hour), Busy: "slot-0 is leased to mav"}}
	vs := decideRuntimes(facts(t, groups, nil, xcode271))
	if v := verdictFor(t, vs, image270); !v.Needed || !strings.Contains(v.Reason, "leased") {
		t.Errorf("27.0 must be kept while a slot asking for it is leased, got %+v", v)
	}
}

// simctl shuts down whatever is booted on an image it deletes, pool-owned
// or the user's own, so any running device protects its runtime.
func TestDecideRuntimes_RunningDeviceKeepsRuntime(t *testing.T) {
	running := map[string]string{"com.apple.CoreSimulator.SimRuntime.iOS-26-3": "My iPhone (ABC, Booted)"}
	vs := decideRuntimes(facts(t, nil, running, xcode271))
	if v := verdictFor(t, vs, image263); !v.Needed || !strings.Contains(v.Reason, "My iPhone") {
		t.Errorf("26.3.1 must be kept while a device runs on it, got %+v", v)
	}
}

// A slot's meta records the runtime id it was actually provisioned on, which
// protects the image even when the group's OS string does not match it.
func TestDecideRuntimes_GroupPinsRuntimeByRecordedRuntimeID(t *testing.T) {
	groups := []groupUsage{{Name: "iPhone-17-Pro@latest", OSVersion: "latest", LastUsed: testNow.Add(-time.Hour), RuntimeIDs: []string{"com.apple.CoreSimulator.SimRuntime.iOS-27-0"}}}
	vs := decideRuntimes(facts(t, groups, nil, xcode271))
	if v := verdictFor(t, vs, image270); !v.Needed {
		t.Errorf("27.0 must be kept by a slot provisioned on it an hour ago, got %+v", v)
	}
}

// An SDK only protects runtimes of its own platform: a watchOS 27.1 SDK
// says nothing about the iOS 27.1 runtime.
func TestDecideRuntimes_SDKOfAnotherPlatformDoesNotCount(t *testing.T) {
	watch := simctl.SimulatorSDK{Xcode: "/Applications/Xcode.app", Platform: "com.apple.platform.watchsimulator", Version: "27.1"}
	vs := decideRuntimes(facts(t, nil, nil, watch))
	if v := verdictFor(t, vs, image271); v.Needed {
		t.Errorf("iOS 27.1 must not be kept by a watchOS SDK, got needed: %s", v.Reason)
	}
}

// runtimeHost is a fake host for the runtime report and ghost pruning: what simctl and Xcode
// report, and a record of what it was asked to delete.
type runtimeHost struct {
	images    []simctl.RuntimeImage
	sdks      []simctl.SimulatorSDK
	devices   []simctl.DeviceEntry
	available []simctl.Runtime
	ghostDevs []string
}

func installRuntimeHost(t *testing.T, h *runtimeHost) {
	t.Helper()
	origImages, origSDKs, origDevices, origAvail, origGhost := listRuntimeImages, installedSimulatorSDKs, listPoolDevices, listAvailableRuntimes, deleteGhostDevice
	t.Cleanup(func() {
		listRuntimeImages, installedSimulatorSDKs, listPoolDevices, listAvailableRuntimes, deleteGhostDevice = origImages, origSDKs, origDevices, origAvail, origGhost
	})
	listRuntimeImages = func() ([]simctl.RuntimeImage, error) { return h.images, nil }
	installedSimulatorSDKs = func() []simctl.SimulatorSDK { return h.sdks }
	listPoolDevices = func() ([]simctl.DeviceEntry, error) { return h.devices, nil }
	listAvailableRuntimes = func() ([]simctl.Runtime, error) { return h.available, nil }
	deleteGhostDevice = func(udid string) error { h.ghostDevs = append(h.ghostDevs, udid); return nil }
}

func incidentHost(t *testing.T) *runtimeHost {
	return &runtimeHost{
		images:  fixtureImages(t),
		sdks:    []simctl.SimulatorSDK{xcode271},
		devices: []simctl.DeviceEntry{{UDID: "USER-1", Name: "iPhone 17 Pro", State: "Shutdown", IsAvailable: true, RuntimeID: "com.apple.CoreSimulator.SimRuntime.iOS-27-1"}},
		available: []simctl.Runtime{
			{Identifier: "com.apple.CoreSimulator.SimRuntime.iOS-26-3", Version: "26.3.1", IsAvailable: true},
			{Identifier: "com.apple.CoreSimulator.SimRuntime.iOS-27-1", Version: "27.1", IsAvailable: true},
		},
	}
}

func writeSlot(t *testing.T, root, device, osv string, n int, meta pool.Meta) string {
	t.Helper()
	dir := pool.SlotDir(pool.GroupDir(root, device, osv), n)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeLease(t *testing.T, dir, key string) {
	t.Helper()
	data, _ := json.Marshal(pool.Lease{Key: key, ExpiresAt: time.Now().Add(time.Hour)})
	if err := os.WriteFile(pool.LeasePath(dir), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// doctor's report for the incident: 27.0 and 26.3.1 listed with their size
// and the exact command that removes each, 27.1 kept for Xcode.
func TestReportUnusedRuntimes_NamesSizeAndManualCommand(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "iPhone 17 Pro", "27.0", 0, pool.Meta{UDID: "U270", LastUsed: time.Now().Add(-21 * 24 * time.Hour)})
	installRuntimeHost(t, incidentHost(t))

	var stdout bytes.Buffer
	reportUnusedRuntimes(root, &stdout)

	out := stdout.String()
	for _, want := range []string{
		"unused simulator runtime 27.0 (24A335), 8.4GB",
		"xcrun simctl runtime delete " + image270,
		"xcrun simctl runtime delete " + image263,
		"2 unused simulator runtime(s) take 16.8GB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, image271) {
		t.Errorf("27.1 is the installed Xcode's runtime and must not be reported:\n%s", out)
	}
}

// A held lock on a stale group keeps its runtime out of the report.
func TestReportUnusedRuntimes_HeldSlotKeepsRuntime(t *testing.T) {
	root := t.TempDir()
	dir := writeSlot(t, root, "iPhone 17 Pro", "27.0", 0, pool.Meta{UDID: "U270", LastUsed: time.Now().Add(-21 * 24 * time.Hour)})
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	installRuntimeHost(t, incidentHost(t))

	var stdout bytes.Buffer
	reportUnusedRuntimes(root, &stdout)
	if strings.Contains(stdout.String(), image270) {
		t.Fatalf("27.0 reported unused while slot-0 asking for it is held:\n%s", stdout.String())
	}
}

// Without the inputs that make a runtime needed, nothing is called unused.
func TestReportUnusedRuntimes_RefusesWhenItCannotSeeWhatIsNeeded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(h *runtimeHost)
		want   string
	}{
		{"no Xcode SDK found", func(h *runtimeHost) { h.sdks = nil }, "found no simulator SDK"},
		{"empty device listing", func(h *runtimeHost) { h.devices = nil }, "device listing came back empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := incidentHost(t)
			tc.mutate(h)
			installRuntimeHost(t, h)

			var stdout bytes.Buffer
			reportUnusedRuntimes(t.TempDir(), &stdout)
			if strings.Contains(stdout.String(), "runtime delete") || !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("want only a refusal saying %q, got:\n%s", tc.want, stdout.String())
			}
		})
	}
}

// What `status` shows as "missing" for good is removed: slots of an OS no
// runtime provides, a slot whose simulator was deleted, a slot whose
// simulator is unavailable (and that simulator, being the pool's own). A
// leased slot, a healthy one and a never-provisioned one stay.
func TestPruneGhostSlots_RemovesOnlyFreeGhosts(t *testing.T) {
	root := t.TempDir()
	ghost := writeSlot(t, root, "iPhone 17 Pro", "27.0", 0, pool.Meta{UDID: "GONE", LastUsed: time.Now().Add(-21 * 24 * time.Hour)})
	leased := writeSlot(t, root, "iPhone 17 Pro", "27.0", 1, pool.Meta{UDID: "GONE2"})
	writeLease(t, leased, "mav")
	unavailableDir := writeSlot(t, root, "iPad Pro 11-inch (M5)", "27.0", 0, pool.Meta{UDID: "UNAVAIL"})
	healthy := writeSlot(t, root, "iPhone 17 Pro", "27.1", 0, pool.Meta{UDID: "USER-1"})
	vanished := writeSlot(t, root, "iPhone 17 Pro", "27.1", 1, pool.Meta{UDID: "DELETED-BY-HAND"})
	unprovisioned := writeSlot(t, root, "iPhone 17 Pro", "27.1", 2, pool.Meta{})

	h := incidentHost(t)
	ipadGroup := filepath.Base(pool.GroupDir(root, "iPad Pro 11-inch (M5)", "27.0"))
	h.devices = append(h.devices, simctl.DeviceEntry{UDID: "UNAVAIL", Name: pool.DeviceNameForGroup(root, ipadGroup, 0), State: "Shutdown", IsAvailable: false, RuntimeID: "com.apple.CoreSimulator.SimRuntime.iOS-27-0"})
	installRuntimeHost(t, h)

	var stdout, stderr bytes.Buffer
	pruneGhostSlots(root, false, &stdout, &stderr)

	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Errorf("ghost slot must be removed, stat err=%v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(unavailableDir); !os.IsNotExist(err) {
		t.Errorf("slot of an unavailable device must be removed, stat err=%v", err)
	}
	if len(h.ghostDevs) != 1 || h.ghostDevs[0] != "UNAVAIL" {
		t.Errorf("deleted ghost devices %v, want only the pool's own unavailable UNAVAIL", h.ghostDevs)
	}
	if _, err := os.Stat(leased); err != nil {
		t.Errorf("leased slot must survive: %v", err)
	}
	if _, err := os.Stat(healthy); err != nil {
		t.Errorf("slot whose simulator exists must survive: %v", err)
	}
	if _, err := os.Stat(vanished); !os.IsNotExist(err) {
		t.Errorf("slot whose simulator was deleted must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(unprovisioned); err != nil {
		t.Errorf("never-provisioned slot of an installed runtime must survive: %v", err)
	}
}

func TestPruneGhostSlots_UntrustworthyListingPrunesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(h *runtimeHost)
	}{
		{"empty runtime listing", func(h *runtimeHost) { h.available = nil }},
		{"empty device listing", func(h *runtimeHost) { h.devices = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			slot := writeSlot(t, root, "iPhone 17 Pro", "27.1", 0, pool.Meta{UDID: "USER-1"})
			h := incidentHost(t)
			tc.mutate(h)
			installRuntimeHost(t, h)

			var stdout, stderr bytes.Buffer
			pruneGhostSlots(root, false, &stdout, &stderr)
			if _, err := os.Stat(slot); err != nil {
				t.Fatalf("an empty listing must not turn every slot into a ghost: %v", err)
			}
		})
	}
}

func TestPruneGhostSlots_DryRunRemovesNothing(t *testing.T) {
	root := t.TempDir()
	ghost := writeSlot(t, root, "iPhone 17 Pro", "27.0", 0, pool.Meta{UDID: "GONE"})
	installRuntimeHost(t, incidentHost(t))

	var stdout, stderr bytes.Buffer
	pruneGhostSlots(root, true, &stdout, &stderr)
	if _, err := os.Stat(ghost); err != nil {
		t.Fatalf("dry-run removed the slot: %v", err)
	}
	if !strings.Contains(stdout.String(), "dry-run, would remove the slot") {
		t.Errorf("dry-run must say what it would do:\n%s", stdout.String())
	}
}

// A group nobody used within the window is not kept warm: a warm slot is a
// running device, which would keep its runtime needed forever.
func TestGroupIdleBeyond(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "Stale", "27.0", 0, pool.Meta{LastUsed: testNow.Add(-15 * 24 * time.Hour)})
	writeSlot(t, root, "Stale", "27.0", 1, pool.Meta{LastUsed: testNow.Add(-20 * 24 * time.Hour)})
	writeSlot(t, root, "Fresh", "27.1", 0, pool.Meta{LastUsed: testNow.Add(-24 * time.Hour)})
	writeSlot(t, root, "Never", "27.1", 0, pool.Meta{})

	if _, idle := groupIdleBeyond(pool.GroupDir(root, "Stale", "27.0"), DefaultStaleAfter, testNow); !idle {
		t.Error("group last used 15d ago must be idle beyond a 14d window")
	}
	if _, idle := groupIdleBeyond(pool.GroupDir(root, "Fresh", "27.1"), DefaultStaleAfter, testNow); idle {
		t.Error("group used 1d ago must not be idle")
	}
	if _, idle := groupIdleBeyond(pool.GroupDir(root, "Never", "27.1"), DefaultStaleAfter, testNow); idle {
		t.Error("group never used has no age to measure and must not be called idle")
	}
}
