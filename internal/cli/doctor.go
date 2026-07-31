package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// stuckGrace mirrors reap's default --stuck-after: a `with` holder that has
// had zero live children for less than this is presumed to be mid-startup
// (booting a fresh simulator, resolving a runtime), not stuck.
const stuckGrace = 3 * time.Minute

// RunDoctor implements `simpool doctor`: a read-only coherence check.
// Non-zero exit means something is wrong; it never modifies the pool
// itself (that's reap's job).
func RunDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool doctor: FAIL:", err)
		return 1
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		fmt.Fprintln(stdout, "FAIL  pool root missing or not a directory:", root)
		return 1
	}

	var problems []string
	note := func(format string, a ...any) {
		problems = append(problems, fmt.Sprintf(format, a...))
	}

	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		fmt.Fprintln(stderr, "simpool doctor:", err)
		return 1
	}

	for _, groupDir := range groups {
		group := filepath.Base(groupDir)
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			label := fmt.Sprintf("%s/slot-%d", group, n)

			meta := pool.ReadMeta(dir)
			free, err := pool.IsSlotFree(dir)
			if err != nil {
				note("%s: could not check lock: %v", label, err)
				continue
			}

			if meta.UDID != "" {
				if entry, found, err := simctl.Find(meta.UDID); err == nil {
					if !found {
						note("%s: meta.json references device %s which no longer exists", label, meta.UDID)
					} else if !pool.IsPoolName(entry.Name) {
						// The default device set also holds the user's own
						// simulators; meta.json pointing at one of those
						// would be a serious bug elsewhere in simpool, not
						// something to paper over here.
						note("%s: meta.json references device %s (name %q) that is NOT pool-owned — this should never happen", label, meta.UDID, entry.Name)
					} else if want := pool.DeviceName(meta.Device, meta.OSVersion, n); entry.Name != want {
						note("%s: meta.json's device %s is named %q, expected %q", label, meta.UDID, entry.Name, want)
					}
				}
			}

			if free {
				if meta.UDID != "" {
					if live, _ := procs.LiveConsumers(meta.UDID); len(live) > 0 {
						note("%s: lock is free but %d process(es) still reference device %s (run `simpool reap`)", label, len(live), meta.UDID)
					}
				}
				continue
			}

			if meta.Mode != "with" {
				// `acquire` holders are supposed to have zero children for
				// their entire lifetime; that is not a coherence problem
				// (mirrors reap.go's reapHeldSlot — keep both in sync).
				continue
			}

			holders, err := procs.LockHolders(pool.LockPath(dir))
			if err != nil {
				note("%s: could not determine lock holder: %v", label, err)
				continue
			}
			if len(holders) == 0 {
				continue // released between our check and lsof; not a real problem
			}
			for _, h := range holders {
				if !procs.Alive(h) || !procs.IsSimpoolHolder(h, "with") {
					continue
				}
				children, _ := procs.ChildPIDs(h)
				if len(children) == 0 && time.Since(meta.LastUsed) >= stuckGrace {
					note("%s: held by pid %d with no live child work for %s — stuck, run `simpool reap`", label, h, time.Since(meta.LastUsed).Round(time.Second))
				}
			}
		}
	}

	if len(problems) == 0 {
		fmt.Fprintln(stdout, "OK   pool is coherent:", root)
		return 0
	}
	for _, p := range problems {
		fmt.Fprintln(stdout, "FAIL ", p)
	}
	return 1
}
