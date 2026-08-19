package cli

import (
	"fmt"
	"io"
	"sort"
	"syscall"

	"github.com/bitomule/simpool/internal/procs"
)

// simRuntimeSnapshot and killRuntimeProc are package-level vars, mirroring
// listPoolDevices/shutdownOrphan, so tests can drive the decision logic
// against a synthetic host: the situation this code exists for — hundreds of
// live processes belonging to devices that no longer exist — cannot be
// created on demand in a test suite.
var simRuntimeSnapshot = procs.SimRuntimeProcesses
var killRuntimeProc = func(pid int) error { return procs.Kill(pid, syscall.SIGKILL) }

// orphanedRuntime is a dead device's surviving userland: the UDID it was
// started for, and every live process still running inside it.
type orphanedRuntime struct {
	UDID string
	PIDs []int
}

// findOrphanedRuntimes returns the simulated-OS processes whose device no
// longer exists in the device set.
//
// Deleting a simulator does not stop the processes it booted. CoreSimulator
// tears down a device's launchd_sim tree when the device shuts down
// *cleanly*; a device deleted while booted (which is what `simctl delete` on
// a booted device does, and what any crashed or force-killed tooling leaves
// behind) loses its parent and keeps its entire userland running, reparented
// to launchd. Nothing ever collects it: measured on the development machine,
// 452 such processes across 19 deleted devices, the oldest 18 days old,
// 1774 host processes in total, 15.3 GB of swap in use and the machine
// paging itself to a standstill. Every one of those devices was already gone
// from `simctl list`.
//
// Two conditions gate every decision here, and both are refusals rather than
// permissions:
//
//   - The device listing must succeed AND be non-empty. An empty listing is
//     not evidence that every device was deleted, it is evidence that the
//     listing could not be trusted — the same rule CheckPoison applies before
//     acting on a device it cannot see. Without this, one failed simctl call
//     turns every simulator on the machine into a kill target.
//   - A process is only a candidate if procs.SimRuntimeProcesses could name
//     the device it belongs to, from the tag CoreSimulator itself stamped on
//     it. Anything whose identity could not be established is not swept.
//
// The returned groups are sorted by UDID so output and tests are stable.
func findOrphanedRuntimes(stderr io.Writer) ([]orphanedRuntime, bool) {
	devices, err := listPoolDevices()
	if err != nil {
		fmt.Fprintf(stderr, "reap --orphan-runtimes: listing devices: %v — not touching any process this run\n", err)
		return nil, false
	}
	if len(devices) == 0 {
		fmt.Fprintf(stderr, "reap --orphan-runtimes: device listing came back empty, which cannot be distinguished from a listing that failed to report — not touching any process this run\n")
		return nil, false
	}
	live := make(map[string]bool, len(devices))
	for _, d := range devices {
		live[d.UDID] = true
	}

	found, err := simRuntimeSnapshot()
	if err != nil {
		fmt.Fprintf(stderr, "reap --orphan-runtimes: reading process snapshot: %v — not touching any process this run\n", err)
		return nil, false
	}

	byUDID := map[string][]int{}
	for _, p := range found {
		if live[p.UDID] {
			continue
		}
		byUDID[p.UDID] = append(byUDID[p.UDID], p.PID)
	}

	udids := make([]string, 0, len(byUDID))
	for u := range byUDID {
		udids = append(udids, u)
	}
	sort.Strings(udids)

	out := make([]orphanedRuntime, 0, len(udids))
	for _, u := range udids {
		pids := byUDID[u]
		// Ascending order is what puts launchd_sim first at kill time: it
		// is the only process in the tree still able to spawn new members,
		// and it necessarily holds a lower pid than everything it started,
		// so sorting achieves "signal the leader first" without re-reading
		// every process's argv to find out which one it is. Ties and pid
		// wraparound cost ordering only, never correctness — every pid in
		// the list is killed either way, and a child respawned by a device
		// that no longer exists is itself an orphan the next run collects.
		sort.Ints(pids)
		out = append(out, orphanedRuntime{UDID: u, PIDs: pids})
	}
	return out, true
}

// reapOrphanRuntimes reports — and with purge, kills — the userland of
// devices that no longer exist.
//
// Killing here is narrower than it looks. A process whose device is absent
// from a listing that did report other devices can never become valid again:
// simulator UDIDs are not reused, nothing can re-create the device under the
// same id, and no live device's processes can be reached, since a live
// device is by definition present in the listing that just succeeded. That
// is also what makes a second identity re-check at kill time unnecessary
// here in a way it is not for `with`-spawned orphans: for a recycled pid to
// be mistaken for a member of this set, the operating system would have to
// hand the number to a *new* process belonging to the *same deleted device*,
// which nothing on the machine is able to create.
//
// launchd_sim is signalled first within each device. It is the only process
// in the tree that can still spawn new members, so killing the leaves first
// would race against it re-populating them.
func reapOrphanRuntimes(purge, dryRun bool, stdout, stderr io.Writer) {
	groups, ok := findOrphanedRuntimes(stderr)
	if !ok {
		return
	}
	if len(groups) == 0 {
		return
	}

	total := 0
	for _, g := range groups {
		total += len(g.PIDs)
	}

	if !purge || dryRun {
		hint := "rerun with --purge-orphan-runtimes to kill them"
		if dryRun {
			hint = "dry-run, would kill them"
		}
		for _, g := range groups {
			fmt.Fprintf(stdout, "ORPHAN-RUNTIME device %s no longer exists, but %d of its processes are still running (pids %v) — %s\n", g.UDID, len(g.PIDs), g.PIDs, hint)
		}
		fmt.Fprintf(stdout, "ORPHAN-RUNTIME %d process(es) across %d deleted device(s)\n", total, len(groups))
		return
	}

	killed, failed := 0, 0
	for _, g := range groups {
		for _, pid := range g.PIDs {
			if err := killRuntimeProc(pid); err != nil {
				fmt.Fprintf(stderr, "reap --orphan-runtimes: killing pid %d (device %s): %v\n", pid, g.UDID, err)
				failed++
				continue
			}
			killed++
		}
		fmt.Fprintf(stdout, "PURGED-RUNTIME device %s — killed %d process(es) left running after the device was deleted\n", g.UDID, len(g.PIDs))
	}
	fmt.Fprintf(stdout, "PURGED-RUNTIME %d process(es) across %d deleted device(s)", killed, len(groups))
	if failed > 0 {
		fmt.Fprintf(stdout, ", %d could not be signalled", failed)
	}
	fmt.Fprintln(stdout)
}
