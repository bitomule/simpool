package pool

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mobai-app/simslim"
)

// Between `reap --cold` (shut the simulator down) and `reap --purge`
// (delete it outright) there was nothing: a slot that stayed useful kept
// every log, cache and temporary file it had ever generated, and the only
// way to get that disk back was to destroy the slot and re-provision it.
// Measured on this pool's own slots, each ~4GB simulator was holding
// 1.3-1.9GB of exactly that: over a gigabyte of unified logs alone per
// slot.
//
// Purging to reclaim it is worse than it looks. Re-provisioning is not
// free (a cold boot, and the first-boot settling behind it), and a
// recreated simulator is a different simulator: this project has already
// spent days on snapshot suites that pass on one slot's renderer and fail
// on another's. Scrubbing empties the generated data and leaves the device
// — its UDID, its installed apps, its recorded baselines' renderer —
// exactly where it was.
//
// Only the categories whose contents iOS regenerates on demand are ever
// touched. Installed apps, documents, app data and user media are not
// cleanup categories at all; simslim reports them as stored data and this
// never passes them to anything.
const (
	// DefaultScrubCategories are simslim's three lower-risk cleanup
	// categories: generated caches, logs and diagnostics, and temporary
	// files. Everything else it can clean (downloaded language models,
	// Siri assets) is left alone by default — those come back over the
	// network, which on a CI machine is a cost paid at the worst moment.
	DefaultScrubCategories = "caches,logs,temporary"
)

// ScrubCategories parses and validates a comma-separated category list,
// rejecting anything simslim does not know how to clean. Validation
// happens before any slot is walked so a typo fails the command outright
// rather than after half the pool has already been scrubbed with a
// silently different selection than the operator asked for.
func ScrubCategories(list string) ([]string, error) {
	var ids []string
	for _, part := range strings.Split(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			ids = append(ids, part)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no cleanup categories given")
	}
	valid, err := simslim.ValidateDiskCleanupSelection(ids)
	if err != nil {
		return nil, err
	}
	return valid, nil
}

// PlanScrub reports how many bytes the given categories would reclaim from
// udid, without deleting anything. Used for --dry-run, so a preview costs
// a directory walk and nothing else.
func PlanScrub(udid string, categories []string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	plan, err := simslim.PlanDiskCleanup(ctx, udid)
	if err != nil {
		return 0, err
	}
	wanted := map[string]bool{}
	for _, id := range categories {
		wanted[id] = true
	}
	var total int64
	for _, c := range plan.Categories {
		if wanted[c.ID] && c.CanClean {
			total += c.Bytes
		}
	}
	return total, nil
}

// ScrubDevice deletes the given categories' contents from udid and reports
// how many bytes came back. The device is returned to the boot state it
// was in — which for every slot reap scrubs is "shut down", since reap
// only ever reaches the scrub step after confirming exactly that.
func ScrubDevice(udid string, categories []string, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := simslim.CleanDeviceDisk(ctx, udid, categories, true)
	if err != nil {
		return 0, err
	}
	return result.ReclaimedBytes, nil
}

// DefaultScrubTimeout bounds one slot's cleanup. A scrub is a directory
// walk plus unlinks over a few GB of small files; minutes is generous, but
// a slot wedged on a stuck unlink must not hold up the rest of the pass.
const DefaultScrubTimeout = 5 * time.Minute

// HumanBytes renders a byte count for reap's log lines, which are read by
// people deciding whether the scheduled service is earning its keep.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
