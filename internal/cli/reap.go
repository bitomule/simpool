package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// RunReap implements `simpool reap [--cold N]`: recycles slots that are
// free and idle, bidirectionally:
//
//  1. lock free, but the simulator still has live processes attached
//     (e.g. simpool was SIGKILLed but its child survived) -> leave alone,
//     never touch a simulator that might be in active use.
//  2. lock held, but only by residual processes with no live work left
//     under them (a stuck holder whose actual consumer already exited)
//     -> kill that process group; the kernel releases the flock on its own.
//
// The lock file itself is never deleted or rewritten by reap — it stays
// the pool's single source of truth for the next acquirer.
func RunReap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	coldMinutes := fs.Int("cold", 0, "minimum idle minutes (since last use) before a free slot's simulator is shut down")
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
			reapSlot(dir, *coldMinutes, *dryRun, stdout, stderr)
		}
	}
	return 0
}

func reapSlot(dir string, coldMinutes int, dryRun bool, stdout, stderr io.Writer) {
	label := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil && err != pool.ErrBusy {
		fmt.Fprintf(stderr, "reap %s: %v\n", label, err)
		return
	}

	if err == pool.ErrBusy {
		reapHeldSlot(dir, label, dryRun, stdout, stderr)
		return
	}
	defer lock.Release()

	meta := pool.ReadMeta(dir)
	if meta.UDID != "" {
		if live, _ := procs.MatchingPIDs(meta.UDID); len(live) > 0 {
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
	if state != "Booted" {
		fmt.Fprintf(stdout, "COLD  %s  already shut down\n", label)
		return
	}
	fmt.Fprintf(stdout, "SHUT  %s  shutting down %s (idle %s)\n", label, meta.UDID, idle.Round(time.Second))
	if !dryRun {
		if err := simctl.Shutdown(pool.SetDirFor(dir), meta.UDID); err != nil {
			fmt.Fprintf(stderr, "reap %s: %v\n", label, err)
		}
	}
}

func reapHeldSlot(dir, label string, dryRun bool, stdout, stderr io.Writer) {
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
		children, err := procs.ChildPIDs(h)
		if err != nil {
			fmt.Fprintf(stderr, "reap %s: listing children of pid %d: %v\n", label, h, err)
			continue
		}
		if len(children) > 0 {
			fmt.Fprintf(stdout, "BUSY  %s  held by pid %d with live work — leaving alone\n", label, h)
			continue
		}
		fmt.Fprintf(stdout, "STUCK %s  held by pid %d with no live child — killing its process group\n", label, h)
		if !dryRun {
			if err := procs.KillProcessGroup(h, syscall.SIGKILL); err != nil {
				fmt.Fprintf(stderr, "reap %s: killing pid %d: %v\n", label, h, err)
			}
		}
	}
}
