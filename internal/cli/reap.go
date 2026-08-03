package cli

import (
	"errors"
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
//     attached (e.g. simpool was SIGKILLed but its child survived) -> if
//     that process is a verified `with`-spawned orphan (its recorded
//     start-time fingerprint still matches — see pool.AttemptRecovery),
//     kill it and shut down the simulator, reclaiming the slot; otherwise
//     (unverifiable identity, or a kill that doesn't stick) leave it alone
//     and let a later `reap` or the next acquisition retry.
//  2. lock held, but only by a residual `simpool with` whose actual
//     consumer already exited out from under it -> kill that one process;
//     the kernel releases the flock on its own. `simpool acquire` holders
//     are never touched here: having no children is their entire, correct
//     design (see Meta.Mode) — see design doc §5's "for scripts that will
//     manage the workload themselves".
//
// Every slot's simulator lives in the default device set (design decision
// "opción (b)"), alongside the user's own simulators. Before shutting down
// or deleting anything, reap always re-checks the device's actual name in
// that set against pool.IsPoolName/pool.DeviceName — never just trusts a
// UDID out of meta.json — so a stale or corrupt meta.json can never make
// reap touch a simulator that isn't ours.
//
// The lock file itself is never deleted or rewritten while a slot's
// simulator is still alive — it stays the pool's single source of truth
// for the next acquirer. Once a slot's simulator has actually been purged
// (or it was never provisioned and has sat abandoned past --purge), reap
// removes the whole slot directory, lock file included: that is the "dead
// slot" cleanup path, distinct from and in addition to deleting simulators.
func RunReap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	coldMinutes := fs.Int("cold", 0, "minimum idle minutes (since last use) before a free slot's simulator is shut down")
	stuckAfter := fs.Duration("stuck-after", 3*time.Minute, "minimum time a `with` holder must have had zero live children before it is considered stuck (not just mid-provisioning) and killed")
	purgeMinutes := fs.Int("purge", 0, "minutes a slot must have been shut down (already cold) before its simulator is deleted outright, reclaiming disk; 0 disables purging")
	pruneRunsAfter := fs.Duration("prune-runs-after", 24*time.Hour, "delete a free slot's run directories older than this")
	dryRun := fs.Bool("dry-run", false, "report what would happen without changing anything")
	disownPoisoned := fs.Bool("disown-poisoned", false, "for a poisoned slot automatic recovery could not verify (a recycled pid, or a process group owned by another user): forget this slot's identity and delete its device, WITHOUT signaling the process that poisoned it — that process, if actually still alive, is left running untouched. Only ever affects a `with` slot whose poison is an unverifiable process-group fingerprint; never a live lease/acquire consumer or a check that merely failed to run. Use after `simpool doctor`/`reap` keep reporting the same slot stuck across multiple runs")
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
			reapSlot(root, dir, n, *coldMinutes, *purgeMinutes, *pruneRunsAfter, *stuckAfter, *dryRun, *disownPoisoned, stdout, stderr)
		}
	}
	return 0
}

func reapSlot(root, dir string, n, coldMinutes, purgeMinutes int, pruneRunsAfter, stuckAfter time.Duration, dryRun, disownPoisoned bool, stdout, stderr io.Writer) {
	groupDir := filepath.Dir(dir)
	label := filepath.Base(groupDir) + "/" + filepath.Base(dir)
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

	// A leased slot's flock is free by design (a Lease never holds it —
	// see lease.go), so without this check reap would treat an actively
	// hot-looped MAV slot exactly like any other idle free slot and could
	// shut down or purge its simulator out from under the lease holder.
	lease, err := pool.ReadLease(dir)
	if err != nil {
		// Could not tell whether this slot is under an active lease
		// (EMFILE, a permission error, a truncated lease.json) — never
		// read that as "no lease". Treat it exactly like a live one: leave
		// the slot alone rather than falling through to idle/poison/
		// --cold/--purge accounting on the strength of a check that never
		// actually completed.
		fmt.Fprintf(stdout, "SKIP  %s  could not read lease.json (%v) — treating as occupied, not touching\n", label, err)
		return
	}
	if lease.Alive() {
		fmt.Fprintf(stdout, "SKIP  %s  active lease (key=%q, expires in %s) — not touching\n", label, lease.Key, time.Until(lease.ExpiresAt).Round(time.Second))
		return
	} else if lease.Key != "" {
		if dryRun {
			fmt.Fprintf(stdout, "PRUNE %s  would remove expired lease (key=%q)\n", label, lease.Key)
		} else {
			removed, err := pool.CleanupExpiredLease(groupDir, dir)
			if err != nil {
				// Same "unreadable means occupied" rule applies to
				// CleanupExpiredLease's own re-check: bail out rather than
				// falling through, exactly like the "still alive" and
				// "renewed just now" branches below.
				fmt.Fprintf(stderr, "reap %s: removing expired lease: %v — treating as occupied, not touching\n", label, err)
				return
			} else if removed {
				fmt.Fprintf(stdout, "PRUNE %s  removed expired lease (key=%q)\n", label, lease.Key)
			} else {
				// CleanupExpiredLease re-checked under the group allocation
				// lock and found the lease alive after all — some other
				// `simpool lease` call renewed it in the narrow window
				// between our lock-free ReadLease above and this call. Bail
				// out exactly like the "still alive" branch above: falling
				// through to the idle/poison/--cold/--purge accounting below
				// would otherwise treat a slot re-leased moments ago as an
				// ordinary idle free slot and could shut down or purge its
				// simulator out from under the new holder.
				fmt.Fprintf(stdout, "SKIP  %s  lease was renewed for key %q just as reap was about to prune it — not touching\n", label, lease.Key)
				return
			}
		}
	}

	pruneRunDirs(dir, label, pruneRunsAfter, dryRun, stdout, stderr)

	meta := pool.ReadMeta(dir)
	if poison := pool.CheckPoison(meta); poison.Poisoned() {
		if dryRun {
			msg := fmt.Sprintf("SKIP  %s  lock free but its consumer is still alive (device %s, %s) — dry-run, not attempting recovery", label, meta.UDID, poison)
			if disownPoisoned && meta.Mode == "with" && poison.Reason == pool.PoisonedByConsumerPGID {
				msg += fmt.Sprintf("; if recovery still can't verify identity on a real run, --disown-poisoned would then forget pgid %d's fingerprint and delete device %s (pgid itself left completely untouched, not killed)", meta.ConsumerPGID, meta.UDID)
			}
			fmt.Fprintln(stdout, msg)
			return
		}
		if pool.AttemptRecovery(root, dir, n, filepath.Base(groupDir), &meta, poison) {
			// Only ever true for a verified `with`-spawned orphan (see
			// AttemptRecovery) — never for a LiveConsumers-only signal,
			// which for a leased slot is the healthy case, not an orphan,
			// and never for a failed liveness check.
			fmt.Fprintf(stdout, "RECOVER %s  reclaimed a verified orphan (device %s, %s) — killed and shut down\n", label, meta.UDID, poison)
			// Return immediately rather than falling through to this same
			// pass's idle/cold/--purge accounting: AttemptRecovery's
			// simctl.Shutdown call is measured SYNCHRONOUS (5-7.5s wall
			// time to return the device's own reported state — see
			// internal/simctl — not the async, returns-before-it's-really-
			// down call an earlier version of this comment assumed), so the
			// device's state is already accurate by the time it returns.
			// What can still lag a beat behind that state flip is
			// CoreSimulator's own teardown of the device's underlying
			// process tree — deleting a simulator on the heels of an
			// already-synchronous shutdown has still been reproduced (a
			// real, reverted regression) to orphan hundreds of runtime
			// processes, exactly the catastrophic failure mode
			// cleanupPool's test-cleanup comment documents. A later `reap`
			// run will see the device's actual settled state (still
			// mid-teardown, or genuinely Shutdown) and act on it then,
			// exactly like the ordinary SHUT case above.
			return
		}
		if disownPoisoned {
			pgid, udid := meta.ConsumerPGID, meta.UDID
			if err := pool.DisownPoisonedSlot(root, dir, n, filepath.Base(groupDir), &meta, poison); err != nil {
				if errors.Is(err, pool.ErrNotDisownable) {
					fmt.Fprintf(stdout, "SKIP  %s  lock free but its consumer is still alive (device %s, %s) — not eligible for --disown-poisoned (only an unverifiable `with`-holder process-group fingerprint qualifies; a live lease/acquire consumer or a failed check never does), not touching; the next acquisition (with/acquire/lease) will retry automatically\n", label, meta.UDID, poison)
				} else {
					fmt.Fprintf(stderr, "reap %s: --disown-poisoned: %v\n", label, err)
				}
				return
			}
			fmt.Fprintf(stdout, "DISOWN %s  could not verify pgid %d's identity (device %s, %s) — forgot this slot's fingerprint and deleted that device on your explicit --disown-poisoned request; if pgid %d is still alive, it is left running untouched, just no longer tracked by simpool\n", label, pgid, udid, poison, pgid)
			return
		}
		fmt.Fprintf(stdout, "SKIP  %s  lock free but its consumer is still alive (device %s, %s) — could not verify its identity, not touching; the next acquisition (with/acquire/lease) will retry automatically, or rerun `simpool reap --disown-poisoned` to forget this slot's identity (without signaling anything) and free it for reuse\n", label, meta.UDID, poison)
		return
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
		removeDeadSlotDir(groupDir, dir, label, purgeMinutes, dryRun, stdout, stderr)
		return
	}
	entry, found, err := simctl.Find(meta.UDID)
	if err != nil {
		fmt.Fprintf(stderr, "reap %s: checking device state: %v\n", label, err)
		return
	}
	if !found {
		fmt.Fprintf(stdout, "IDLE  %s  free, meta references missing device %s\n", label, meta.UDID)
		return
	}
	if want := pool.DeviceNameForGroup(root, filepath.Base(groupDir), n); entry.Name != want {
		// meta.json points at a real, pool-owned-or-not device that exists,
		// but isn't the one this exact slot is supposed to own — this
		// should never happen (see EnsureProvisioned's exact-name check),
		// but reap is the one place with the power to shut down or delete a
		// simulator, so it is the one place that must refuse outright
		// rather than trust meta.json's UDID at face value. Checking
		// IsPoolName alone was not enough: it let a stale/corrupt meta.json
		// in one slot point at the live, correctly-named simulator of
		// *another* slot (or a different pool root's identically-numbered
		// slot before RootTag existed) and have reap shut it down or delete
		// it out from under its real, live holder. The default device set
		// also holds the user's own simulators; they are never touched
		// either way.
		fmt.Fprintf(stdout, "SKIP  %s  meta references device %s named %q, expected %q — not this slot's device, refusing to touch it (this should never happen)\n", label, meta.UDID, entry.Name, want)
		return
	}
	if entry.State == "Booted" {
		fmt.Fprintf(stdout, "SHUT  %s  shutting down %s (idle %s)\n", label, meta.UDID, idle.Round(time.Second))
		if !dryRun {
			if err := simctl.Shutdown(meta.UDID); err != nil {
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
	if err := simctl.Delete(meta.UDID); err != nil {
		fmt.Fprintf(stderr, "reap %s: purging %s: %v\n", label, meta.UDID, err)
		return
	}
	// The slot directory (lock file included) is removed outright rather
	// than just clearing meta.json: post-purge, an empty numbered slot
	// directory has zero informational value over no directory at all, and
	// leaving it around is exactly the "no subcommand ever deletes a dead
	// slot" gap the design review flagged. pool.RemoveSlotDir — not a bare
	// os.RemoveAll — serializes this against AcquireSlots' take() via the
	// group allocation lock: we still hold this slot's own lock (fd I1)
	// across the call, which blocks a *second* opener of I1 from racing us,
	// but does nothing about a process that already opened I1 before we got
	// here and hasn't flocked it yet — that process's pending flock would
	// otherwise succeed the instant our own lock.Release() (deferred above)
	// runs, on an inode we just unlinked, while a third process creates a
	// brand-new lock file for the same slot number. The allocation lock
	// closes that gap. AcquireSlots picks the freed slot number back up on
	// its own — it discovers slots (and now, capacity) by walking whatever
	// directories exist, not by any persisted count.
	if err := pool.RemoveSlotDir(groupDir, dir); err != nil {
		fmt.Fprintf(stderr, "reap %s: removing purged slot directory: %v\n", label, err)
	}
}

// deadSlotGrace is the minimum time a never-provisioned slot directory must
// have existed before reap considers it abandoned rather than "another
// process is mid-acquire right now". take() in pool.AcquireSlots always
// holds the lock across mkdir+provision, so reap can only ever observe an
// unlocked, never-provisioned slot dir here if provisioning failed and the
// caller released it — but the grace period costs nothing and removes any
// doubt.
const deadSlotGrace = 2 * time.Minute

// removeDeadSlotDir deletes a free, never-provisioned slot directory (a
// leftover from a provisioning attempt that failed before writing a UDID
// to meta.json) once it is old enough and --purge is enabled — otherwise
// failed provisioning attempts accumulate slot numbers under a group
// forever, each one an empty directory nothing will ever clean up.
func removeDeadSlotDir(groupDir, dir, label string, purgeMinutes int, dryRun bool, stdout, stderr io.Writer) {
	if purgeMinutes <= 0 {
		return
	}
	info, err := os.Stat(dir)
	if err != nil || time.Since(info.ModTime()) < deadSlotGrace {
		return
	}
	fmt.Fprintf(stdout, "PURGE %s  removing empty, never-provisioned slot directory\n", label)
	if dryRun {
		return
	}
	// pool.RemoveSlotDir, not a bare os.RemoveAll — same race this slot's
	// provisioned-purge path above closes (see its comment).
	if err := pool.RemoveSlotDir(groupDir, dir); err != nil {
		fmt.Fprintf(stderr, "reap %s: removing dead slot directory: %v\n", label, err)
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
		// file actually holds the flock vs. merely probed it — that
		// includes not just a concurrent `status`/`doctor`/`reap` probe,
		// but another `simpool with` that is polling this same lock file
		// every acquirePollInterval while it waits for capacity (§ AcquireSlots):
		// during that brief open+TryLock-fails+close window it, too, can
		// legitimately have zero children and an IsSimpoolHolder(h,"with")
		// command line, which would otherwise make it look identical to a
		// genuinely stuck titular holder. meta.OwnerPID is the pid
		// EnsureProvisioned recorded for whichever process actually
		// completed provisioning while holding this exact lock, so
		// requiring an exact match — not just "looks like a `with`" — is
		// what tells the real titular apart from a waiter's transient probe.
		if h != meta.OwnerPID || !procs.IsSimpoolHolder(h, "with") {
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
