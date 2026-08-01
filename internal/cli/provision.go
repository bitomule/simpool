package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// acquireAndProvision locks `count` slots (never more than `max` resident
// for the device+OS group, waiting up to waitTimeout if the group is
// already at capacity) and makes sure each has a booted simulator ready to
// use. mode ("with" or "acquire") is recorded in each slot's meta so
// `reap`/`doctor` can reason about who legitimately holds it. On any
// failure, every slot acquired so far is released before returning —
// callers never have to clean up a partial acquisition themselves.
func acquireAndProvision(device, osVersion string, count, max int, wait time.Duration, ownerCmd, mode string) (slots []*pool.Slot, runDir string, err error) {
	root, err := pool.Root()
	if err != nil {
		return nil, "", err
	}

	slots, err = pool.AcquireSlots(root, device, osVersion, count, max, wait)
	if err != nil {
		return nil, "", err
	}

	release := func() {
		for _, s := range slots {
			s.Release()
		}
	}

	for _, s := range slots {
		if err := pool.EnsureProvisioned(s, ownerCmd, mode); err != nil {
			release()
			return nil, "", fmt.Errorf("slot %s: %w", s.Dir, err)
		}
	}

	runDir = filepath.Join(slots[0].Dir, "runs", fmt.Sprintf("%d-%s", os.Getpid(), nowStamp()))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		release()
		return nil, "", err
	}

	return slots, runDir, nil
}

func releaseAll(slots []*pool.Slot) {
	for _, s := range slots {
		s.Release()
	}
}

// removeRunDirIfEmpty deletes runDir if the consumer never wrote anything
// into it. MAV_EXACT_RUN_DIR is opt-in — plenty of consumers, most notably
// `simpool_ios_test_runner` (a plain `bazel test` action never references
// it at all), never touch it — so without this, `runs/` accumulates one
// empty directory per invocation forever; only `simpool reap
// --prune-runs-after` (default 24h, and nothing calls it automatically)
// ever cleared it before. A non-empty run dir is left untouched: it holds
// evidence (screenshots, videos, HARs, logs) a failing run left behind on
// purpose, and reap's own pruning already handles aging that out safely.
func removeRunDirIfEmpty(runDir string) {
	if runDir == "" {
		return
	}
	entries, err := os.ReadDir(runDir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(runDir)
}
