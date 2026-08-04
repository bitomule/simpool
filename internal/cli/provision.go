package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
//
// Writes a handful of diagnostic lines to stderr along the way — resolved
// pool root, per-phase timings, any pool-named devices belonging to a
// foreign pool root — so a slow or wrong acquisition inside a `bazel test`
// log can be diagnosed without re-running anything (see
// reportForeignRootDevices). Not done for `simpool lease`'s hot loop (see
// lease.go, which times its own phases but skips the extra device-set scan
// this performs): mav calls lease roughly once a minute per action, and an
// extra `simctl list devices` subprocess there would tax exactly the path
// this codebase has otherwise worked to keep as fast as possible. Here, on
// `with`/`acquire`, that cost is negligible next to the cold boot the
// caller is often already paying.
func acquireAndProvision(device, osVersion string, count, max int, wait time.Duration, ownerCmd, mode string, stderr io.Writer) (slots []*pool.Slot, runDir string, err error) {
	root, err := pool.Root()
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintf(stderr, "simpool: pool root %s\n", root)
	reportForeignRootDevices(root, stderr)

	acquireStart := time.Now()
	slots, err = pool.AcquireSlots(root, device, osVersion, count, max, wait)
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintf(stderr, "simpool: acquired %d slot(s) for %s in %s\n", len(slots), pool.GroupName(device, osVersion), time.Since(acquireStart).Round(time.Millisecond))

	release := func() {
		for _, s := range slots {
			s.Release()
		}
	}

	for _, s := range slots {
		provisionStart := time.Now()
		// "with"/"acquire" never hold a lease of their own — see
		// EnsureProvisioned's leaseKey doc comment — so "" here is not a
		// placeholder, it is the correct value: no real lease key can ever
		// equal it (Lease.Alive() requires a non-empty Key), so the
		// substance-mismatch guard's "is this MY lease" check can never
		// accidentally match someone else's.
		if err := pool.EnsureProvisioned(s, ownerCmd, mode, ""); err != nil {
			release()
			return nil, "", fmt.Errorf("slot %s: %w", s.Dir, err)
		}
		fmt.Fprintf(stderr, "simpool: %s/slot-%d provisioned (udid %s) in %s\n", filepath.Base(s.GroupDir), s.Number, s.Meta.UDID, time.Since(provisionStart).Round(time.Millisecond))
	}

	runDir = filepath.Join(slots[0].Dir, "runs", fmt.Sprintf("%d-%s", os.Getpid(), nowStamp()))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		release()
		return nil, "", err
	}

	return slots, runDir, nil
}

// reportForeignRootDevices scans the default device set for pool-named
// devices (see pool.IsPoolName) whose embedded RootTag doesn't match this
// pool root's own — i.e. devices left behind by, or still owned by, a
// DIFFERENT simpool pool root (a different SIMPOOL_HOME, or one that no
// longer exists on disk at all — see pool.RootTag's doc comment). Purely
// informational: this never touches or acts on what it finds (that's
// `reap --orphans`' job, an explicit, opt-in action elsewhere), it only
// surfaces them on stderr so "why does this machine seem to have more
// simulators than my pool's own slots" is answerable from a single log
// instead of a manual `xcrun simctl list devices` + cross-reference.
// Best-effort: a scan failure is itself reported but never fails the
// caller's acquisition — this is diagnostics, not a gate.
func reportForeignRootDevices(root string, stderr io.Writer) {
	// listPoolDevices (declared in reap.go) is the same injectable seam
	// `reap --orphans` uses — reused here rather than calling
	// simctl.ListDevices directly so this scan is exercisable in tests
	// without a real device set.
	devices, err := listPoolDevices()
	if err != nil {
		fmt.Fprintf(stderr, "simpool: could not scan the device set for foreign-root devices: %v\n", err)
		return
	}
	tag := pool.RootTag(root)
	var foreign []string
	for _, d := range devices {
		if !pool.IsPoolName(d.Name) {
			continue
		}
		dtag, ok := poolDeviceTag(d.Name)
		if !ok || dtag == tag {
			continue
		}
		foreign = append(foreign, d.Name+" ("+d.UDID+")")
	}
	if len(foreign) > 0 {
		fmt.Fprintf(stderr, "simpool: %d pool-named device(s) belong to a different/foreign pool root (not this pool's %s): %s\n", len(foreign), tag, strings.Join(foreign, ", "))
	}
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
