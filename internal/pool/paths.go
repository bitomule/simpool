// Package pool manages the on-disk layout of the simulator pool: groups,
// slots, their flock-based locks and their informational metadata.
//
// The lock file is the single source of truth. meta.json is best-effort
// bookkeeping that can be lost or stale without the pool becoming incorrect:
// if flock says a slot is free, the slot is free.
package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// EnvPoolHome overrides the pool root directory. Used by tests to avoid
// touching the real ~/Library/Developer/SimPool tree.
const EnvPoolHome = "SIMPOOL_HOME"

// forbiddenRoot is the volume the pool must never live on: it is a 40GB
// quota shared with Bazel's disk cache (see design doc §6).
const forbiddenRoot = "/Volumes/BazelCache"

// Root returns the pool's root directory, creating it if necessary. The
// forbidden-volume guard applies to SIMPOOL_HOME overrides too: that is the
// only way anyone can actually put the pool under the quota-limited Bazel
// cache volume, so it is the branch that most needs the check, not the one
// that can be skipped.
func Root() (string, error) {
	root := ""
	if override := os.Getenv(EnvPoolHome); override != "" {
		root = override
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, "Library", "Developer", "SimPool")
	}
	if strings.HasPrefix(root, forbiddenRoot) {
		return "", errPoolOnForbiddenVolume
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

var errPoolOnForbiddenVolume = poolPathError("pool root resolved under " + forbiddenRoot + ", which is quota-limited and shared with the Bazel disk cache; refusing to use it")

type poolPathError string

func (e poolPathError) Error() string { return string(e) }

var sanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = sanitizeRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// groupSep joins the sanitized device and OS parts of a GroupName. It must
// be a character sanitize() can never produce (sanitize only ever emits
// [A-Za-z0-9._-]), or two different (device, osVersion) pairs can collapse
// onto the same GroupName: sanitize("iPhone 17", "Pro_26.3") and
// sanitize("iPhone 17_Pro", "26.3") both used to end in "...17_Pro_26.3"
// when the separator was "_", which sanitize passes through unchanged.
const groupSep = "@"

// GroupName returns the on-disk directory name for a given device+OS pair.
func GroupName(device, osVersion string) string {
	return sanitize(device) + groupSep + sanitize(osVersion)
}

// NamePrefix marks every simulator simpool owns in the (shared, default)
// device set. Every simulator simpool creates gets a name starting with
// this; reap and doctor must refuse to shut down, delete, or otherwise act
// on any device whose name doesn't start with it — the default set also
// holds the user's own simulators (34 on the dev machine at design time),
// and they must never be touched.
const NamePrefix = "SIMPOOL_"

// RootTag returns a short, stable, filesystem- and simctl-name-safe
// fingerprint of a pool root directory. DeviceName folds this into every
// simulator name because the default device set (design decision "opción
// (b)") is a single, machine-wide namespace, while a pool root is not: two
// independent SIMPOOL_HOME roots (e.g. two test suites, or a real pool
// alongside one under test) otherwise produce byte-identical names for
// "their" slot-0, and the name-based recovery path in EnsureProvisioned
// would make the second root silently adopt — and reap could then delete —
// the first root's simulator out from under a live holder.
func RootTag(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:8]
}

// DeviceName returns the deterministic, pool-wide-unique name simpool gives
// the simulator for one slot of a given pool root, e.g.
// "SIMPOOL_a1b2c3d4_iPhone-17-Pro@26.3_slot-0" for slot 0 of the
// "iPhone 17 Pro" / 26.3 group under root. Deterministic and unique per
// (root, group, slot) so EnsureProvisioned can recover a slot's simulator by
// name alone if meta.json is lost or corrupted — all slots across all pool
// roots now share one device set, so a name collision would be a real leak
// (see RootTag), not just cosmetic.
func DeviceName(root, device, osVersion string, slotNumber int) string {
	return fmt.Sprintf("%s%s_%s_slot-%d", NamePrefix, RootTag(root), GroupName(device, osVersion), slotNumber)
}

// IsPoolName reports whether name looks like a simulator simpool created
// (see NamePrefix). Anything else in the default device set is off-limits.
func IsPoolName(name string) bool {
	return strings.HasPrefix(name, NamePrefix)
}

// GroupDir returns the absolute path of a device+OS group under root.
func GroupDir(root, device, osVersion string) string {
	return filepath.Join(root, GroupName(device, osVersion))
}

var slotDirRe = regexp.MustCompile(`^slot-(\d+)$`)

// SlotDir returns the absolute path of slot N within a group.
func SlotDir(groupDir string, n int) string {
	return filepath.Join(groupDir, "slot-"+strconv.Itoa(n))
}

// ListSlotNumbers returns the slot numbers that already have a directory
// under groupDir, sorted ascending. Missing/unreadable groupDir yields nil.
func ListSlotNumbers(groupDir string) []int {
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return nil
	}
	var nums []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := slotDirRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		nums = append(nums, n)
	}
	sortInts(nums)
	return nums
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// ListGroupDirs returns all group directories under root.
func ListGroupDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs, nil
}

func lockPath(slotDir string) string { return filepath.Join(slotDir, "lock") }
func metaPath(slotDir string) string { return filepath.Join(slotDir, "meta.json") }

// allocLockPath is a group-wide (not per-slot) lock file that serializes
// structural changes to a group's slot directories: creating a brand-new
// slot (first MkdirAll+open of its lock file) and tearing one down
// (os.RemoveAll after a purge). See RemoveSlotDir — without this, a slot's
// lock file could be unlinked by reap in the narrow window after a second
// process has opened it but before it has flocked it, letting that second
// process end up "holding" a deleted, unlinked lock file while a third
// process creates a brand-new one for the same slot number.
func allocLockPath(groupDir string) string { return filepath.Join(groupDir, ".alloc.lock") }

// LockPath and MetaPath are exported accessors for callers (cli, tests)
// that only have a slot directory path, not a live *Slot.
func LockPath(slotDir string) string { return lockPath(slotDir) }
func MetaPath(slotDir string) string { return metaPath(slotDir) }

// IsSlotFree reports whether the slot at slotDir is currently unlocked.
func IsSlotFree(slotDir string) (bool, error) { return IsFree(lockPath(slotDir)) }
