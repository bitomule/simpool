package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	disownPoisoned := fs.Bool("disown-poisoned", false, "for a poisoned slot automatic recovery could not verify: for an unverifiable `with` process-group fingerprint (a recycled pid, or a process group owned by another user), forget this slot's identity and delete its device WITHOUT signaling the process, which if actually still alive is left running untouched; for orphaned residue (an idb_companion daemon, or an orphaned `simctl spawn <udid> log stream`) whose target device is confirmed NOT RUNNING but whose identity as this slot's own could not be confirmed (deleted, or named for something else), KILL the already narrowly-verified residue pid(s) and forget the stale device reference — unlike the first case, forgetting alone would leave them running and re-poisoning this slot forever, since neither self-terminates. Never a live lease/acquire consumer, a check that merely failed to run, or residue attached to a device that is still running (that one is reclaimed automatically instead, with the device-identity guard intact — this flag skips that guard, so it must never act without a not-running device behind it). Use after `simpool doctor`/`reap` keep reporting the same slot stuck across multiple runs")
	maxSlots := fs.Int("max", pool.MaxSlotsPerGroup(), "maximum slots a device+OS group may have at all: excess slots that are free, unleased, unquarantined and idle are DELETED — simulator and slot directory both — newest kept, coldest first. This is the enforcement half of the same --max that AcquireSlots/AcquireLease apply at claim time; without it --max is a one-way ratchet, since nothing else ever removes a slot and every acquisition happily reuses whatever exists (env "+pool.EnvMaxSlots+"). 0 disables the pass entirely. Unlike --purge this is on by default: a group over its own declared cap is by definition holding simulators that should never have existed. Make sure this process resolves the SAME --max as the acquirers around it — a launchd job inherits almost no environment")
	warmCap := fs.Int("warm", 0, "maximum free+booted simulators to keep warm per device+OS group, independent of --max (which caps how many may be resident/locked at once, not how many stay booted afterward); the most-recently-used ones are kept, the rest are shut down regardless of --cold. 0 (default) disables this and preserves today's behavior, where only --cold's idle-time check ever shuts a free slot down")
	scrubMinutes := fs.Int("scrub", 0, "minutes a slot must have been shut down (already cold) before its generated caches, logs and temporary files are deleted WITHOUT deleting the simulator; 0 disables scrubbing. This is the step between --cold and --purge: measured on this pool's own slots, a ~4GB simulator holds 1.3-1.9GB of exactly this data, over a gigabyte of it unified logs. Unlike --purge it keeps the device — its UDID, its installed apps, and the renderer its snapshot baselines were recorded against — so reclaiming disk never costs a re-provision or a re-recorded baseline. Only touches a slot that is free, unleased, unquarantined, confirmed to own its device, and shut down")
	scrubCategoryList := fs.String("scrub-categories", pool.DefaultScrubCategories, "comma-separated categories --scrub deletes. The default three are the ones iOS regenerates locally on demand; `linguistic-data` is also cleanable but comes back over the network. Installed apps, documents, app data and user media are not cleanup categories and are never touched")
	orphans := fs.Bool("orphans", false, "scan the default device set for pool-named simulators no slot under this pool root currently references (e.g. left behind by a purged slot directory, or by a different/vanished pool root — see the RootTag doc comment) and report them. Read-only by itself; combine with --purge-orphans to actually delete what it finds")
	purgeOrphans := fs.Bool("purge-orphans", false, "delete the orphaned devices --orphans finds, after verifying no live process still references each one. Implies --orphans. Still respects --dry-run for a preview")
	purgeOrphanRuntimes := fs.Bool("purge-orphan-runtimes", false, "kill the still-running processes of simulators that no longer exist. Deleting a booted device does not stop the userland it booted: its launchd_sim tree is reparented to launchd and runs forever (measured here: 452 processes across 19 deleted devices, oldest 18 days, the machine paging itself to a standstill). Every `simpool reap` already REPORTS these; this flag is what kills them. Only ever touches processes whose device is absent from a device listing that succeeded and returned other devices — never one belonging to a live device, and nothing at all if that listing could not be trusted. Respects --dry-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Validated up front, before a single slot is walked: a typo in
	// --scrub-categories must fail the command outright rather than
	// halfway through a pass that has already deleted data under a
	// different selection than the operator asked for.
	var scrubCategories []string
	if *scrubMinutes > 0 {
		valid, err := pool.ScrubCategories(*scrubCategoryList)
		if err != nil {
			fmt.Fprintln(stderr, "simpool reap: --scrub-categories:", err)
			return 2
		}
		scrubCategories = valid
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
			reapSlot(root, dir, n, *coldMinutes, *purgeMinutes, *pruneRunsAfter, *stuckAfter, *dryRun, *disownPoisoned, *scrubMinutes, scrubCategories, stdout, stderr)
		}
	}

	// Before --warm, and after the per-slot pass above: --cold has by now
	// shut down whatever was idle, so an excess slot is usually already
	// cold here and needs no shutdown of its own; and there is no point
	// deciding which simulators to keep warm among slots this pass is about
	// to delete.
	if *maxSlots > 0 {
		for _, groupDir := range groups {
			enforceSlotCap(root, groupDir, *maxSlots, *dryRun, stdout, stderr)
		}
	}

	if *warmCap > 0 {
		for _, groupDir := range groups {
			enforceWarmCap(root, groupDir, *warmCap, *dryRun, stdout, stderr)
		}
	}

	if *orphans || *purgeOrphans {
		reapOrphans(root, *purgeOrphans, *dryRun, stdout, stderr)
	}

	// Unconditional, unlike --orphans. A deleted device's surviving userland
	// belongs to no slot and no pool root, so no flag a caller happens to
	// have passed makes it more or less relevant — and it accumulated
	// unnoticed for 18 days precisely because nothing ever mentioned it.
	// Reporting costs one `ps` and prints nothing when there is nothing to
	// say; killing still requires asking.
	reapOrphanRuntimes(*purgeOrphanRuntimes, *dryRun, stdout, stderr)
	return 0
}

// listPoolDevices and shutdownOrphan/deleteOrphan are package-level vars —
// not direct simctl calls — so tests can exercise reapOrphans' decision
// logic (which devices count as referenced, the live-consumer safety check,
// --dry-run) with a fake device set instead of the real one, mirroring
// doctor.go's findDevice seam.
var listPoolDevices = simctl.ListDevices
var shutdownOrphan = simctl.Shutdown
var deleteOrphan = simctl.Delete

// reapOrphans finds pool-named devices in the default device set that no
// slot currently under root references by name, and — only when purge is
// true, i.e. only ever on the caller's explicit, opt-in request, exactly
// like --disown-poisoned — deletes them. Scanning and reporting is always
// safe and side-effect-free; deletion additionally requires (re-verified
// here, not trusted from any earlier pass) that no live process still
// references the device, failing safe (skip, don't delete) if that check
// itself cannot complete.
//
// "Referenced" is checked purely by name — pool.DeviceNameForGroup, derived
// from a slot's own directory (root, group, slot number), the same
// self-referential-guard rule every other cross-slot check in this codebase
// follows (see paths.go's DeviceNameForGroup doc comment) — never from any
// slot's meta.json, which is exactly the kind of stale/lost bookkeeping
// that produces an orphan in the first place. A device whose name embeds a
// DIFFERENT pool root's tag (see RootTag) is unconditionally orphaned from
// THIS root's point of view, whether that other root still exists
// elsewhere on disk or not: this invocation only ever knows about slots
// under its own root, so a foreign-tag device can never be "referenced" by
// anything it can see.
//
// reapOrphans deliberately re-lists both groups and slots itself, strictly
// after its own listPoolDevices() call, rather than trusting any slice
// RunReap snapshotted earlier — RunReap's reapSlot pass runs a `simctl list
// devices` per slot plus multi-second synchronous shutdowns, so by the time
// this runs, minutes may have passed. A group or slot created during that
// window (e.g. mav leasing a brand-new device+OS pair) can only ever ADD to
// `known` this way, never remove from it, which is what makes the race
// collapse in the safe direction: a listing taken before this device scan
// can only under-report devices that already existed, never over-report
// slots that don't.
//
// Building `known` from an unreadable listing is exactly the "couldn't
// verify must read as busy, don't touch" rule this codebase learned the
// hard way, applied to the one path that deletes simulators: if any group's
// slot listing can't be read (EACCES, EMFILE, ...), reapOrphans aborts the
// entire pass rather than purge against a `known` set that might be silently
// missing a live slot's device.
func reapOrphans(root string, purge, dryRun bool, stdout, stderr io.Writer) {
	devices, err := listPoolDevices()
	if err != nil {
		fmt.Fprintf(stderr, "reap --orphans: listing devices: %v\n", err)
		return
	}

	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		fmt.Fprintf(stderr, "reap --orphans: listing pool groups: %v\n", err)
		return
	}

	known := map[string]bool{}
	for _, groupDir := range groups {
		group := filepath.Base(groupDir)
		nums, err := pool.ListSlotNumbersChecked(groupDir)
		if err != nil {
			fmt.Fprintf(stderr, "reap --orphans: listing slots under %s: %v — aborting, not touching any device this run\n", group, err)
			return
		}
		for _, n := range nums {
			known[pool.DeviceNameForGroup(root, group, n)] = true
		}
	}
	rootTag := pool.RootTag(root)

	for _, d := range devices {
		if !pool.IsPoolName(d.Name) || known[d.Name] {
			continue
		}

		reason := "no slot directory under this pool root currently references this name"
		if tag, ok := poolDeviceTag(d.Name); !ok {
			reason = "malformed pool-prefixed name"
		} else if tag != rootTag {
			reason = fmt.Sprintf("belongs to a different (possibly vanished) pool root %s, not this pool's root %s", tag, rootTag)
		}

		if !purge {
			fmt.Fprintf(stdout, "ORPHAN %s (%s)  %s — rerun with --purge-orphans to delete\n", d.Name, d.UDID, reason)
			continue
		}

		// Never a witness to itself: re-verify liveness right before
		// acting, not from any earlier state, and treat a check that
		// cannot complete as "still referenced" — the same fail-safe rule
		// CheckPoison and every poisoned-slot guard in this codebase
		// applies to a liveness check that didn't finish.
		live, lerr := procs.LiveConsumers(d.UDID)
		if lerr != nil {
			fmt.Fprintf(stdout, "SKIP   %s (%s)  could not verify no live process still references it (%v) — not touching\n", d.Name, d.UDID, lerr)
			continue
		}
		if len(live) > 0 {
			fmt.Fprintf(stdout, "SKIP   %s (%s)  a live process still references this device — not touching\n", d.Name, d.UDID)
			continue
		}

		fmt.Fprintf(stdout, "PURGE  %s (%s)  %s\n", d.Name, d.UDID, reason)
		if dryRun {
			continue
		}
		if err := shutdownOrphan(d.UDID); err != nil {
			fmt.Fprintf(stderr, "reap --orphans: shutting down %s: %v\n", d.UDID, err)
			continue
		}
		if err := deleteOrphan(d.UDID); err != nil {
			fmt.Fprintf(stderr, "reap --orphans: deleting %s: %v\n", d.UDID, err)
		}
	}
}

// warmCandidate is a free, verified-safe, currently-booted slot considered
// by enforceWarmCap for its group's --warm accounting. Deliberately holds no
// lock — see enforceWarmCap's doc comment on why the two passes never share
// one.
type warmCandidate struct {
	n    int
	meta pool.Meta
}

// shutdownWarm is a package-level var, not a direct simctl.Shutdown call, so
// tests can exercise enforceWarmCap's destructive branch (which UDIDs it
// shuts down, which it leaves alone) with a fake instead of a real
// simulator, mirroring shutdownOrphan/deleteOrphan/findDevice's existing
// seams.
var shutdownWarm = simctl.Shutdown

// classifyWarmSlot applies every safety check a slot must pass to be
// eligible for --warm accounting: unreadable/live lease treated as busy, a
// poisoned slot, no UDID, the device missing or not found, and a name/state
// mismatch against what this exact slot (root, group, slot number — never
// meta.Device/meta.OSVersion, the same self-referential-guard rule as
// reapSlot's own cross-slot check) is supposed to own. Assumes dir's flock
// is already held by the caller; never releases it. Returns the slot's meta
// and true only when every check passes.
func classifyWarmSlot(root, group string, n int, dir string) (pool.Meta, bool) {
	if lease, err := pool.ReadLease(dir); err != nil || lease.Alive() {
		// Unreadable or alive: never guess-touch, mirrors reapSlot's own
		// "unreadable means occupied" rule for lease.json.
		return pool.Meta{}, false
	}

	meta := pool.ReadMeta(dir)
	if poison := pool.CheckPoison(meta); poison.Poisoned() {
		// A free-looking slot whose previous consumer might still be
		// alive is never eligible — same rule as reapSlot's own idle/
		// --cold/--purge accounting.
		return pool.Meta{}, false
	}
	if meta.UDID == "" {
		return pool.Meta{}, false
	}
	entry, found, err := findDevice(meta.UDID)
	if err != nil || !found {
		return pool.Meta{}, false
	}
	if want := pool.DeviceNameForGroup(root, group, n); entry.Name != want || entry.State != "Booted" {
		return pool.Meta{}, false
	}
	return meta, true
}

// enforceWarmCap separates "how many slots may be resident/locked at once"
// (--max, unchanged, enforced by AcquireSlots/AcquireLease at claim time)
// from "how many stay booted once freed" (--warm, this function): rules_idb
// found that conflating the two — capping residue at the same number as
// concurrency — makes sustained runs 2-4x slower and flaky, since every run
// past the first pays a fresh cold boot for a slot that was needlessly shut
// down the moment it went idle.
//
// Only ever acts on a group's MOST-RECENTLY-used slots beyond warmCap: it
// keeps the warmCap most-recently-used free+booted slots running and shuts
// down the rest, so the ones most likely to be reused next stay warm.
//
// Unlike an earlier version of this function, the classification pass never
// holds more than one slot's flock at a time: every other lock-taking path
// in this codebase (reapSlot, AcquireSlots, reapOrphans) follows that rule,
// and accumulating every eligible slot's flock before releasing any of them
// made a --warm pass read as "the whole group is busy" to a concurrent
// `simpool lease` — which never waits for capacity — for as long as the
// scan (a real `simctl list devices` per slot) plus every synchronous
// shutdown that followed took. Each candidate's lock is now taken,
// classified, and released before moving to the next slot; only the slots
// actually selected for shutdown are locked a second time, immediately
// before acting, with the full classification re-run under that second lock
// so a slot that turned busy, leased, poisoned, or was reassigned in between
// is skipped rather than acted on.
func enforceWarmCap(root, groupDir string, warmCap int, dryRun bool, stdout, stderr io.Writer) {
	group := filepath.Base(groupDir)
	var warm []warmCandidate

	for _, n := range pool.ListSlotNumbers(groupDir) {
		dir := pool.SlotDir(groupDir, n)
		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s/slot-%d: --warm: %v\n", group, n, err)
			}
			continue // busy (held, or leased flock-free) is never this pass's to touch
		}
		meta, ok := classifyWarmSlot(root, group, n, dir)
		_ = lock.Release()
		if !ok {
			continue
		}
		warm = append(warm, warmCandidate{n: n, meta: meta})
	}

	sort.SliceStable(warm, func(i, j int) bool {
		return warm[i].meta.LastUsed.After(warm[j].meta.LastUsed)
	})

	for i, c := range warm {
		if i < warmCap {
			continue
		}
		label := fmt.Sprintf("%s/slot-%d", group, c.n)
		dir := pool.SlotDir(groupDir, c.n)

		lock, err := pool.TryLock(pool.LockPath(dir))
		if err != nil {
			if err != pool.ErrBusy {
				fmt.Fprintf(stderr, "reap %s: --warm: %v\n", label, err)
			}
			continue // became busy since classification — not this pass's to touch
		}
		meta, ok := classifyWarmSlot(root, group, c.n, dir)
		if !ok {
			// Something about this slot changed since it was classified
			// (leased, poisoned, reassigned, shut down already) — skip it
			// rather than act on stale first-pass state.
			_ = lock.Release()
			continue
		}

		idle := time.Since(meta.LastUsed)
		if dryRun {
			fmt.Fprintf(stdout, "WARM  %s  would shut down %s (idle %s) to stay within --warm %d cap\n", label, meta.UDID, idle.Round(time.Second), warmCap)
			_ = lock.Release()
			continue
		}
		fmt.Fprintf(stdout, "WARM  %s  shutting down %s (idle %s) to stay within --warm %d cap\n", label, meta.UDID, idle.Round(time.Second), warmCap)
		if err := shutdownWarm(meta.UDID); err != nil {
			fmt.Fprintf(stderr, "reap %s: --warm: %v\n", label, err)
		}
		_ = lock.Release()
	}
}

// scrubSlot reclaims a cold slot's generated caches, logs and temporary
// files without deleting the simulator. Failure is reported and never
// fatal: a slot that could not be scrubbed is exactly as usable as it was
// before, and taking a scheduled reap pass down over reclaimable disk
// would cost more than the disk is worth.
func scrubSlot(label, udid string, idle time.Duration, scrubMinutes int, categories []string, dryRun bool, stdout, stderr io.Writer) {
	if scrubMinutes <= 0 || idle < time.Duration(scrubMinutes)*time.Minute {
		return
	}
	if dryRun {
		reclaimable, err := pool.PlanScrub(udid, categories, pool.DefaultScrubTimeout)
		if err != nil {
			fmt.Fprintf(stderr, "reap %s: planning scrub of %s: %v\n", label, udid, err)
			return
		}
		fmt.Fprintf(stdout, "SCRUB %s  would reclaim %s from %s (%s), simulator kept\n", label, pool.HumanBytes(reclaimable), udid, strings.Join(categories, ","))
		return
	}
	reclaimed, err := pool.ScrubDevice(udid, categories, pool.DefaultScrubTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "reap %s: scrubbing %s: %v\n", label, udid, err)
		return
	}
	fmt.Fprintf(stdout, "SCRUB %s  reclaimed %s from %s (%s), simulator kept\n", label, pool.HumanBytes(reclaimed), udid, strings.Join(categories, ","))
}

func reapSlot(root, dir string, n, coldMinutes, purgeMinutes int, pruneRunsAfter, stuckAfter time.Duration, dryRun, disownPoisoned bool, scrubMinutes int, scrubCategories []string, stdout, stderr io.Writer) {
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
			var msg string
			switch {
			case poison.Reason == pool.PoisonedByOrphanedResidue && pool.ResidueDeviceVerified(root, filepath.Base(groupDir), n, meta):
				msg = fmt.Sprintf("SKIP  %s  %s — device confirmed this slot's own; dry-run, would kill %d orphaned idb_companion / simctl log-stream process(es), device itself left untouched", label, poison, len(poison.ResiduePIDs))
			case poison.Reason == pool.PoisonedByOrphanedResidue:
				msg = fmt.Sprintf("SKIP  %s  %s — device %s's identity as this slot's own could not be confirmed (deleted, or named for something else); dry-run, recovery would quarantine rather than auto-kill", label, poison, meta.UDID)
				if disownPoisoned && poison.ResidueDisownable() {
					msg += fmt.Sprintf("; --disown-poisoned would then kill %d residue pid(s) %v and forget this slot's stale device reference", len(poison.ResiduePIDs), poison.ResiduePIDs)
				} else if disownPoisoned {
					// The residue's target device is still running, so
					// --disown-poisoned deliberately refuses too: without
					// deviceBelongsToSlot (which it skips) and without a
					// confirmed-not-running device, nothing would be left
					// proving this UDID is not someone else's live
					// simulator. See pool.Poison.ResidueDisownable.
					msg += "; --disown-poisoned would refuse it too — that path needs a device confirmed not running, and this one is up"
				}
			default:
				msg = fmt.Sprintf("SKIP  %s  lock free but its consumer is still alive (device %s, %s) — dry-run, not attempting recovery", label, meta.UDID, poison)
				if disownPoisoned && meta.Mode == "with" && poison.Reason == pool.PoisonedByConsumerPGID {
					msg += fmt.Sprintf("; if recovery still can't verify identity on a real run, --disown-poisoned would then forget pgid %d's fingerprint and delete device %s (pgid itself left completely untouched, not killed)", meta.ConsumerPGID, meta.UDID)
				}
			}
			fmt.Fprintln(stdout, msg)
			return
		}
		if pool.AttemptRecovery(root, dir, n, filepath.Base(groupDir), &meta, poison) {
			// True either for a verified `with`-spawned orphan (see
			// AttemptRecovery's ConsumerPGID branch) — never for a plain
			// LiveConsumers-only signal, which for a leased slot is the
			// healthy case, not an orphan — or for a verified-inert
			// residue process set reclaimed regardless of Mode (see
			// PoisonedByOrphanedResidue); never for a failed liveness
			// check either way.
			if poison.Reason == pool.PoisonedByOrphanedResidue {
				why := "it was already confirmed not running"
				if poison.ResidueEvidence == pool.ResidueSlotLongIdle {
					why = fmt.Sprintf("the slot was unheld and unused for over %s and the device stays warm for the next consumer", pool.ResidueIdleGrace)
				}
				fmt.Fprintf(stdout, "RECOVER %s  killed %d orphaned idb_companion / simctl log-stream process(es) attached to device %s — device left untouched, %s\n", label, len(poison.ResiduePIDs), meta.UDID, why)
			} else {
				fmt.Fprintf(stdout, "RECOVER %s  reclaimed a verified orphan (device %s, %s) — killed and shut down\n", label, meta.UDID, poison)
			}
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
			if poison.Reason == pool.PoisonedByOrphanedResidue {
				residuePIDs, udid := append([]int(nil), poison.ResiduePIDs...), meta.UDID
				if err := pool.DisownPoisonedSlot(root, dir, n, filepath.Base(groupDir), &meta, poison); err != nil {
					if errors.Is(err, pool.ErrNotDisownable) && !poison.ResidueDisownable() {
						fmt.Fprintf(stdout, "SKIP  %s  %s — not eligible for --disown-poisoned: that path skips the device-identity guard, so it only ever acts on a device confirmed not running, and %s is still up. Automatic recovery is the path for this one and it already declined, which means device %s could not be confirmed to be this slot's own — fix or clear meta.json's device reference instead of forcing it\n", label, poison, udid, udid)
					} else if errors.Is(err, pool.ErrNotDisownable) {
						fmt.Fprintf(stdout, "SKIP  %s  %s — not eligible for --disown-poisoned (a residue pid could not be re-verified against device %s), not touching; the next acquisition (with/acquire/lease) will retry automatically\n", label, poison, udid)
					} else {
						fmt.Fprintf(stderr, "reap %s: --disown-poisoned: could not confirm every residue pid was killed: %v\n", label, err)
					}
					return
				}
				fmt.Fprintf(stdout, "DISOWN %s  device %s's identity as this slot's own could not be confirmed — killed %d orphaned idb_companion / simctl log-stream process(es) (pids %v) and forgot this slot's stale device reference on your explicit --disown-poisoned request\n", label, udid, len(residuePIDs), residuePIDs)
				return
			}
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
		if poison.Reason == pool.PoisonedByOrphanedResidue {
			if poison.ResidueDisownable() {
				fmt.Fprintf(stdout, "SKIP  %s  %s — device %s's identity as this slot's own could not be confirmed (deleted, or named for something else), not touching; rerun `simpool reap --disown-poisoned` to kill %d residue pid(s) and forget this slot's stale device reference\n", label, poison, meta.UDID, len(poison.ResiduePIDs))
				return
			}
			// Running device: --disown-poisoned refuses this shape on
			// purpose (see pool.Poison.ResidueDisownable), so pointing at
			// it would be the same dead end the pre-fix version of this
			// message sent operators down. Automatic recovery IS the path
			// here and it just declined, which can only mean the device's
			// identity as this slot's own could not be confirmed.
			fmt.Fprintf(stdout, "SKIP  %s  %s — device %s is running but could not be confirmed to be this slot's own, not touching; --disown-poisoned deliberately will not act on a running device either, so fix or clear meta.json's device reference for this slot\n", label, poison, meta.UDID)
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

	// Everything above has already proven this slot is safe to act on:
	// the flock is free, no live lease sits on it, it is not poisoned, the
	// device exists, it is named exactly what this slot's device must be
	// named, and it is shut down. That is a stricter set of guarantees
	// than --purge itself needs, which is why the scrub can run here and
	// nowhere else.
	scrubSlot(label, meta.UDID, idle, scrubMinutes, scrubCategories, dryRun, stdout, stderr)

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
