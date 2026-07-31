// Package pool manages the on-disk layout of the simulator pool: groups,
// slots, their flock-based locks and their informational metadata.
//
// The lock file is the single source of truth. meta.json is best-effort
// bookkeeping that can be lost or stale without the pool becoming incorrect:
// if flock says a slot is free, the slot is free.
package pool

import (
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

// GroupName returns the on-disk directory name for a given device+OS pair.
func GroupName(device, osVersion string) string {
	return sanitize(device) + "_" + sanitize(osVersion)
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
func setPath(slotDir string) string  { return filepath.Join(slotDir, "set") }
func metaPath(slotDir string) string { return filepath.Join(slotDir, "meta.json") }

// LockPath, SetDirFor and MetaPath are exported accessors for callers (cli,
// tests) that only have a slot directory path, not a live *Slot.
func LockPath(slotDir string) string  { return lockPath(slotDir) }
func SetDirFor(slotDir string) string { return setPath(slotDir) }
func MetaPath(slotDir string) string  { return metaPath(slotDir) }

// IsSlotFree reports whether the slot at slotDir is currently unlocked.
func IsSlotFree(slotDir string) (bool, error) { return IsFree(lockPath(slotDir)) }
