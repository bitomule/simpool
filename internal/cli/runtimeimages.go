package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// A simulator runtime disk image is 7.8-8.4GB and is mounted as its own APFS
// volume. Nothing removes one when the Xcode that wanted it is replaced, and
// simpool kept the problem alive on its own: on the machine this was written
// for, the disk reached 98% with iOS 26.3.1, 27.0 and 27.1 installed (22.7GB)
// while the only Xcode left shipped the 27.1 SDK. 27.0 was still in use only
// because four simpool groups were pinned to it and the --warm floor kept one
// of their slots booted, which on its own made the runtime look busy forever.
//
// simpool never deletes a runtime: which OS versions a machine keeps is its
// owner's decision. `doctor` reports the unused ones with the command that
// removes each, and `reap` keeps the pool's own state free of slots whose
// simulator or runtime is gone.
//
// These are the "runtime images" below, to keep them apart from the
// "orphan runtimes" in runtimes.go, which are leftover processes, not disks.

// DefaultStaleAfter is how recently a simpool group must have been used for
// the runtime it asks for to count as needed, and for --warm to keep it warm.
const DefaultStaleAfter = 14 * 24 * time.Hour

var listRuntimeImages = simctl.ListRuntimeImages
var listAvailableRuntimes = simctl.ListRuntimes
var deleteGhostDevice = simctl.Delete

// ghostPass is pruneGhostSlots as RunReap calls it, so tests of other reap
// passes do not depend on this host's runtimes.
var ghostPass = pruneGhostSlots
var installedSimulatorSDKs = func() []simctl.SimulatorSDK {
	return simctl.SimulatorSDKs(simctl.InstalledXcodes())
}

// groupUsage is what the pool knows about one device+OS group when deciding
// whether the runtime it asks for is still needed.
type groupUsage struct {
	Name      string
	OSVersion string
	// LastUsed is the latest acquisition or release of any of its slots.
	LastUsed   time.Time
	RuntimeIDs []string
	// Busy names a slot that is held or leased right now, or whose state
	// could not be read, which counts the same.
	Busy string
}

type runtimeFacts struct {
	Images []simctl.RuntimeImage
	SDKs   []simctl.SimulatorSDK
	Groups []groupUsage
	// Running maps a CoreSimulator runtime id to a device on it that is not
	// shut down, pool-owned or not.
	Running map[string]string
	Now     time.Time
	Window  time.Duration
}

type runtimeVerdict struct {
	Image  simctl.RuntimeImage
	Needed bool
	Reason string
}

// versionMatches is ResolveRuntime's rule ("26.3" picks "26.3.1"), applied
// both ways so a group or SDK naming a more precise version than the image
// still protects it.
func versionMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

func groupWants(g groupUsage, img simctl.RuntimeImage) bool {
	for _, id := range g.RuntimeIDs {
		if id != "" && id == img.RuntimeIdentifier {
			return true
		}
	}
	return versionMatches(g.OSVersion, img.Version)
}

// decideRuntimes says, for every runtime image, whether anything needs it.
// A runtime is needed when a device on it is running, a simpool slot that
// asks for it is held or leased, an installed Xcode ships its simulator SDK,
// or a simpool group asking for it was acquired within the window. Anything
// else is unused.
func decideRuntimes(f runtimeFacts) []runtimeVerdict {
	out := make([]runtimeVerdict, 0, len(f.Images))
	for _, img := range f.Images {
		out = append(out, decideRuntime(f, img))
	}
	return out
}

func decideRuntime(f runtimeFacts, img simctl.RuntimeImage) runtimeVerdict {
	needed := func(reason string) runtimeVerdict {
		return runtimeVerdict{Image: img, Needed: true, Reason: reason}
	}
	if dev, ok := f.Running[img.RuntimeIdentifier]; ok {
		return needed(fmt.Sprintf("device %s is running on it", dev))
	}
	for _, g := range f.Groups {
		if g.Busy != "" && groupWants(g, img) {
			return needed(fmt.Sprintf("simpool group %s: %s", g.Name, g.Busy))
		}
	}
	for _, sdk := range f.SDKs {
		if sdk.Platform == img.PlatformIdentifier && versionMatches(sdk.Version, img.Version) {
			return needed(fmt.Sprintf("simulator SDK %s of %s", sdk.Version, filepath.Base(sdk.Xcode)))
		}
	}
	for _, g := range f.Groups {
		if g.LastUsed.IsZero() || !groupWants(g, img) {
			continue
		}
		if age := f.Now.Sub(g.LastUsed); age < f.Window {
			return needed(fmt.Sprintf("simpool group %s was used %s ago", g.Name, roundAge(age)))
		}
	}
	return runtimeVerdict{Image: img, Reason: fmt.Sprintf("no installed Xcode ships its SDK, no simpool group asked for it in the last %s, nothing is running on it", roundAge(f.Window))}
}

func roundAge(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return d.Round(time.Minute).String()
}

func groupOSVersion(group string) string {
	if i := strings.LastIndex(group, "@"); i >= 0 {
		return group[i+1:]
	}
	return ""
}

// collectGroupUsage reads every group's slots. Any listing that cannot be
// read aborts: a group missing from the answer is a runtime that would look
// unused.
func collectGroupUsage(root string) ([]groupUsage, error) {
	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		return nil, fmt.Errorf("listing pool groups: %w", err)
	}
	var out []groupUsage
	for _, groupDir := range groups {
		name := filepath.Base(groupDir)
		g := groupUsage{Name: name, OSVersion: groupOSVersion(name)}
		nums, err := pool.ListSlotNumbersChecked(groupDir)
		if err != nil {
			return nil, fmt.Errorf("listing slots under %s: %w", name, err)
		}
		for _, n := range nums {
			dir := pool.SlotDir(groupDir, n)
			meta := pool.ReadMeta(dir)
			if meta.LastUsed.After(g.LastUsed) {
				g.LastUsed = meta.LastUsed
			}
			if meta.RuntimeID != "" {
				g.RuntimeIDs = append(g.RuntimeIDs, meta.RuntimeID)
			}
			if g.Busy != "" {
				continue
			}
			if free, ferr := pool.IsSlotFree(dir); ferr != nil {
				g.Busy = fmt.Sprintf("slot-%d lock unreadable (%v)", n, ferr)
			} else if !free {
				g.Busy = fmt.Sprintf("slot-%d is held", n)
			} else if lease, lerr := pool.ReadLease(dir); lerr != nil {
				g.Busy = fmt.Sprintf("slot-%d lease unreadable (%v)", n, lerr)
			} else if lease.Alive() {
				g.Busy = fmt.Sprintf("slot-%d is leased to %s", n, lease.Key)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// gatherRuntimeFacts refuses rather than guesses: every input that could
// make a needed runtime look unused must have been read successfully.
func gatherRuntimeFacts(root string, window time.Duration) (runtimeFacts, error) {
	f := runtimeFacts{Now: time.Now(), Window: window, Running: map[string]string{}}
	images, err := listRuntimeImages()
	if err != nil {
		return f, fmt.Errorf("listing runtime images: %w", err)
	}
	f.Images = images
	if len(images) == 0 {
		return f, nil
	}
	f.SDKs = installedSimulatorSDKs()
	if len(f.SDKs) == 0 {
		return f, errors.New("found no simulator SDK in any installed Xcode, so cannot tell which runtime Xcode itself needs")
	}
	devices, err := listPoolDevices()
	if err != nil {
		return f, fmt.Errorf("listing devices: %w", err)
	}
	if len(devices) == 0 {
		return f, errors.New("device listing came back empty, which cannot be told apart from a listing that failed")
	}
	for _, d := range devices {
		if d.State != "Shutdown" {
			f.Running[d.RuntimeID] = fmt.Sprintf("%s (%s, %s)", d.Name, d.UDID, d.State)
		}
	}
	f.Groups, err = collectGroupUsage(root)
	if err != nil {
		return f, err
	}
	return f, nil
}

func gigabytes(b int64) string {
	return fmt.Sprintf("%.1fGB", float64(b)/1e9)
}

// unusedRuntimeImages is doctor's read-only view.
func unusedRuntimeImages(root string, window time.Duration) ([]runtimeVerdict, error) {
	f, err := gatherRuntimeFacts(root, window)
	if err != nil {
		return nil, err
	}
	var out []runtimeVerdict
	for _, v := range decideRuntimes(f) {
		if !v.Needed {
			out = append(out, v)
		}
	}
	return out, nil
}

type ghostSlot struct {
	group    string
	groupDir string
	n        int
	why      string
	// device is set when the slot's simulator still exists but can never
	// boot again, because its runtime is gone.
	device *simctl.DeviceEntry
}

// findGhostSlots returns the slots `status` shows as "missing" for good: the
// simulator recorded for them no longer exists or is unavailable, or no
// installed runtime provides their group's OS any more. Both listings must
// succeed and be non-empty, or nothing is a ghost: an empty answer cannot be
// told apart from one that failed, and every slot would look gone.
func findGhostSlots(root string) ([]ghostSlot, error) {
	runtimes, err := listAvailableRuntimes()
	if err != nil {
		return nil, fmt.Errorf("listing runtimes: %w", err)
	}
	var available []string
	for _, r := range runtimes {
		if r.IsAvailable {
			available = append(available, r.Version)
		}
	}
	if len(available) == 0 {
		return nil, errors.New("no available runtime listed, which cannot be told apart from a listing that failed")
	}
	devices, err := listPoolDevices()
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	if len(devices) == 0 {
		return nil, errors.New("device listing came back empty, which cannot be told apart from a listing that failed")
	}
	byUDID := map[string]simctl.DeviceEntry{}
	byName := map[string]simctl.DeviceEntry{}
	for _, d := range devices {
		byUDID[d.UDID] = d
		byName[d.Name] = d
	}
	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		return nil, fmt.Errorf("listing pool groups: %w", err)
	}

	var out []ghostSlot
	for _, groupDir := range groups {
		group := filepath.Base(groupDir)
		osv := groupOSVersion(group)
		provided := osv == ""
		for _, v := range available {
			if versionMatches(osv, v) {
				provided = true
				break
			}
		}
		nums, err := pool.ListSlotNumbersChecked(groupDir)
		if err != nil {
			return nil, fmt.Errorf("listing slots under %s: %w", group, err)
		}
		for _, n := range nums {
			meta := pool.ReadMeta(pool.SlotDir(groupDir, n))
			g := ghostSlot{group: group, groupDir: groupDir, n: n}
			d, found := byUDID[meta.UDID]
			if !found || meta.UDID == "" {
				d, found = byName[pool.DeviceNameForGroup(root, group, n)]
			}
			switch {
			case found && d.IsAvailable:
				continue
			case found && !d.IsAvailable:
				dd := d
				g.device = &dd
				g.why = fmt.Sprintf("its simulator %s is unavailable", d.UDID)
			case !provided:
				g.why = fmt.Sprintf("no installed runtime provides OS %s", osv)
			case meta.UDID != "":
				g.why = fmt.Sprintf("its simulator %s no longer exists", meta.UDID)
			default:
				continue
			}
			out = append(out, g)
		}
	}
	return out, nil
}

// pruneGhostSlots removes ghost slots from the pool, deleting the slot's own
// simulator only when it still exists, is unavailable, and carries this
// slot's name. A slot that is held, leased, or quarantined is left alone, and
// nothing outside the pool is touched — runtimes least of all.
func pruneGhostSlots(root string, dryRun bool, stdout, stderr io.Writer) {
	ghosts, err := findGhostSlots(root)
	if err != nil {
		fmt.Fprintf(stderr, "reap ghosts: %v — not pruning any slot this run\n", err)
		return
	}
	for _, g := range ghosts {
		label := fmt.Sprintf("%s/slot-%d", g.group, g.n)
		if dryRun {
			fmt.Fprintf(stdout, "GHOST %s — %s; dry-run, would remove the slot\n", label, g.why)
			continue
		}
		dir := pool.SlotDir(g.groupDir, g.n)
		lock, err := pool.LockExistingSlot(g.groupDir, dir)
		if err != nil {
			fmt.Fprintf(stderr, "reap %s: %v\n", label, err)
			continue
		}
		if lock == nil {
			continue
		}
		if lease, lerr := pool.ReadLease(dir); lerr != nil || lease.Alive() {
			_ = lock.Release()
			continue
		}
		if pool.CheckPoison(pool.ReadMeta(dir)).Poisoned() {
			_ = lock.Release()
			continue
		}
		if g.device != nil && g.device.Name == pool.DeviceNameForGroup(root, g.group, g.n) {
			if err := deleteGhostDevice(g.device.UDID); err != nil {
				fmt.Fprintf(stderr, "reap %s: deleting unavailable simulator %s: %v\n", label, g.device.UDID, err)
				_ = lock.Release()
				continue
			}
		}
		if err := pool.RemoveSlotDir(g.groupDir, dir); err != nil {
			fmt.Fprintf(stderr, "reap %s: removing slot directory: %v\n", label, err)
		} else {
			fmt.Fprintf(stdout, "GHOST %s — removed the slot: %s\n", label, g.why)
		}
		_ = lock.Release()
	}
}

// groupIdleBeyond reports whether a group's latest use is older than window.
// A group never used at all is not called idle: there is nothing to measure.
func groupIdleBeyond(groupDir string, window time.Duration, now time.Time) (time.Duration, bool) {
	var last time.Time
	for _, n := range pool.ListSlotNumbers(groupDir) {
		if m := pool.ReadMeta(pool.SlotDir(groupDir, n)); m.LastUsed.After(last) {
			last = m.LastUsed
		}
	}
	if last.IsZero() {
		return 0, false
	}
	age := now.Sub(last)
	return age, age >= window
}

// reportUnusedRuntimes prints each runtime image nothing needs, its size and
// the command that removes it. It only reads.
func reportUnusedRuntimes(root string, stdout io.Writer) {
	unused, err := unusedRuntimeImages(root, DefaultStaleAfter)
	if err != nil {
		fmt.Fprintf(stdout, "INFO could not tell which simulator runtimes are unused: %v\n", err)
		return
	}
	if len(unused) == 0 {
		return
	}
	var total int64
	for _, v := range unused {
		total += v.Image.SizeBytes
		fmt.Fprintf(stdout, "INFO unused simulator runtime %s (%s), %s — %s. To remove it: xcrun simctl runtime delete %s\n", v.Image.Version, v.Image.Build, gigabytes(v.Image.SizeBytes), v.Reason, v.Image.Identifier)
	}
	fmt.Fprintf(stdout, "INFO %d unused simulator runtime(s) take %s; simpool never deletes runtimes\n", len(unused), gigabytes(total))
}
