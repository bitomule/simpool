package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// RunReap implements `simpool reap [--cold N]`: recycles slots that are
// free and idle, bidirectionally:
//
//  1. lock free, but the simulator still has a live external process
//     attached (e.g. simpool was SIGKILLed but its child survived) -> leave
//     alone, never touch a simulator that might be in active use.
//  2. lock held, but only by a residual `simpool with` whose actual
//     consumer already exited out from under it -> kill that one process;
//     the kernel releases the flock on its own. `simpool acquire` holders
//     are never touched here: having no children is their entire, correct
//     design (see Meta.Mode) — see design doc §5's "for scripts that will
//     manage the workload themselves".
//
// The lock file itself is never deleted or rewritten by reap — it stays
// the pool's single source of truth for the next acquirer.
func RunReap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	coldMinutes := fs.Int("cold", 0, "minimum idle minutes (since last use) before a free slot's simulator is shut down")
	stuckAfter := fs.Duration("stuck-after", 3*time.Minute, "minimum time a `with` holder must have had zero live children before it is considered stuck (not just mid-provisioning) and killed")
	purgeMinutes := fs.Int("purge", 0, "minutes a slot must have been shut down (already cold) before its simulator is deleted outright, reclaiming disk; 0 disables purging")
	pruneRunsAfter := fs.Duration("prune-runs-after", 24*time.Hour, "delete a free slot's run directories older than this")
	dryRun := fs.Bool("dry-run", false, "report what would happen without changing anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool reap:", err)
		return 1
	}
	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		fmt.Fprintln(stderr, "simpool reap:", err)
		return 1
	}

	for _, groupDir := range groups {
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			reapSlot(dir, *coldMinutes, *purgeMinutes, *pruneRunsAfter, *stuckAfter, *dryRun, stdout, stderr)
		}
	}
	return 0
}

func reapSlot(dir string, coldMinutes, purgeMinutes int, pruneRunsAfter, stuckAfter time.Duration, dryRun bool, stdout, stderr io.Writer) {
	label := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil && err != pool.ErrBusy {
		fmt.Fprintf(stderr, "reap %s: %v\n", label, err)
		return
	}

	if err == pool.ErrBusy {
		reapHeldSlot(dir, label, stuckAfter, dryRun, stdout, stderr)
		return
	}
	defer lock.Release()

	pruneRunDirs(dir, label, pruneRunsAfter, dryRun, stdout, stderr)

	meta := pool.ReadMeta(dir)
	if meta.UDID != "" {
		if live, _ := procs.LiveConsumers(meta.UDID); len(live) > 0 {
			fmt.Fprintf(stdout, "SKIP  %s  lock free but %d live process(es) reference %s — not touching\n", label, len(live), meta.UDID)
			return
		}
	}

	idle := 999999 * time.Hour
	if !meta.LastUsed.IsZero() {
		idle = time.Since(meta.LastUsed)
	}
	if idle < time.Duration(coldMinutes)*time.Minute {
		fmt.Fprintf(stdout, "KEEP  %s  free, idle %s (< --cold %dm)\n", label, idle.Round(time.Second), coldMinutes)
		return
	}

	if meta.UDID == "" {
		fmt.Fprintf(stdout, "IDLE  %s  free, never provisioned\n", label)
		return
	}
	state, found, err := simctl.State(pool.SetDirFor(dir), meta.UDID)
	if err != nil {
		fmt.Fprintf(stderr, "reap %s: checking device state: %v\n", label, err)
		return
	}
	if !found {
		fmt.Fprintf(stdout, "IDLE  %s  free, meta references missing device %s\n", label, meta.UDID)
		return
	}
	if state == "Booted" {
		fmt.Fprintf(stdout, "SHUT  %s  shutting down %s (idle %s)\n", label, meta.UDID, idle.Round(time.Second))
		if !dryRun {
			if err := simctl.Shutdown(pool.SetDirFor(dir), meta.UDID); err != nil {
				fmt.Fprintf(stderr, "reap %s: %v\n", label, err)
			}
		}
		return
	}

	fmt.Fprintf(stdout, "COLD  %s  already shut down\n", label)
	if purgeMinutes <= 0 || idle < time.Duration(purgeMinutes)*time.Minute {
		return
	}
	fmt.Fprintf(stdout, "PURGE %s  deleting %s (idle %s >= --purge %dm), reclaiming disk\n", label, meta.UDID, idle.Round(time.Second), purgeMinutes)
	if dryRun {
		return
	}
	if err := simctl.Delete(pool.SetDirFor(dir), meta.UDID); err != nil {
		fmt.Fprintf(stderr, "reap %s: purging %s: %v\n", label, meta.UDID, err)
		return
	}
	if err := pool.WriteMeta(dir, pool.Meta{}); err != nil {
		fmt.Fprintf(stderr, "reap %s: clearing meta after purge: %v\n", label, err)
	}
}

// pruneRunDirs deletes MAV_EXACT_RUN_DIR directories under a free slot that
// are older than maxAge, so per-invocation artifacts (screenshots, videos,
// HARs, logs) don't accumulate under the pool forever. Only called for
// slots whose lock we hold, so nothing here is currently the active run
// directory of a live consumer; as a second, cheap safety check each run
// dir's own embedded pid (name is "<pid>-<timestamp>") is skipped if that
// pid is still alive.
func pruneRunDirs(slotDir, label string, maxAge time.Duration, dryRun bool, stdout, stderr io.Writer) {
	runsDir := filepath.Join(slotDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		if pidStr, _, ok := strings.Cut(e.Name(), "-"); ok {
			if pid, err := strconv.Atoi(pidStr); err == nil && procs.Alive(pid) {
				continue
			}
		}
		path := filepath.Join(runsDir, e.Name())
		fmt.Fprintf(stdout, "PRUNE %s  removing run dir %s (older than %s)\n", label, path, maxAge)
		if !dryRun {
			if err := os.RemoveAll(path); err != nil {
				fmt.Fprintf(stderr, "reap %s: pruning %s: %v\n", label, path, err)
			}
		}
	}
}

func reapHeldSlot(dir, label string, stuckAfter time.Duration, dryRun bool, stdout, stderr io.Writer) {
	meta := pool.ReadMeta(dir)
	if meta.Mode != "with" {
		// "acquire" (or unknown/never-provisioned) holders are never a
		// stuck-holder candidate: `acquire` legitimately has zero children
		// for its entire, correct lifetime — that is its whole contract
		// (§5: "for scripts that will manage the workload themselves").
		// Reporting it as busy-and-fine, not guessing, is what keeps reap
		// from killing a live acquirer's slot out from under it.
		fmt.Fprintf(stdout, "BUSY  %s  held (mode=%q) — not a `with` holder, leaving alone\n", label, meta.Mode)
		return
	}

	holders, err := procs.LockHolders(pool.LockPath(dir))
	if err != nil {
		fmt.Fprintf(stderr, "reap %s: listing lock holders: %v\n", label, err)
		return
	}
	if len(holders) == 0 {
		// Lost the race: it was released between our TryLock attempt and
		// this lsof call. Nothing to do.
		return
	}
	for _, h := range holders {
		if !procs.Alive(h) {
			continue
		}
		// Darwin's lsof doesn't reliably expose which opener of the lock
		// file actually holds the flock vs. merely probed it (a concurrent
		// `status`/`doctor`/`reap`); verify this candidate's command line
		// actually looks like the `simpool with` that meta.json says owns
		// this slot before treating it as the holder at all.
		if !procs.IsSimpoolHolder(h, "with") {
			continue
		}
		children, err := procs.ChildPIDs(h)
		if err != nil {
			fmt.Fprintf(stderr, "reap %s: listing children of pid %d: %v\n", label, h, err)
			continue
		}
		if len(children) > 0 {
			fmt.Fprintf(stdout, "BUSY  %s  held by pid %d with live work — leaving alone\n", label, h)
			continue
		}
		// meta.LastUsed is stamped when EnsureProvisioned starts the run;
		// a freshly-started `with` can legitimately have zero children for
		// a while (booting a simulator, resolving a runtime) — stuckAfter
		// is the grace period before "no children yet" is treated as "no
		// children anymore".
		age := time.Since(meta.LastUsed)
		if meta.LastUsed.IsZero() || age < stuckAfter {
			fmt.Fprintf(stdout, "BUSY  %s  held by pid %d, no live child yet but only %s old (< --stuck-after %s) — leaving alone\n", label, h, age.Round(time.Second), stuckAfter)
			continue
		}
		fmt.Fprintf(stdout, "STUCK %s  held by pid %d with no live child for %s — killing it\n", label, h, age.Round(time.Second))
		if !dryRun {
			// A single Kill, not KillProcessGroup: we just confirmed h has
			// no live children, so there is nothing else under it left to
			// sweep, and h is not guaranteed to be its own process group
			// leader (it inherits its pgid from whatever shell launched
			// `simpool with`) — kill(-h) could otherwise land on an
			// unrelated process group that happens to share that id.
			if err := procs.Kill(h, syscall.SIGKILL); err != nil {
				fmt.Fprintf(stderr, "reap %s: killing pid %d: %v\n", label, h, err)
			}
		}
	}
}
