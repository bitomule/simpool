package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

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

			if info, err := os.Stat(pool.SetDirFor(dir)); err != nil || !info.IsDir() {
				note("%s: device set directory missing", label)
			}

			meta := pool.ReadMeta(dir)
			free, err := pool.IsSlotFree(dir)
			if err != nil {
				note("%s: could not check lock: %v", label, err)
				continue
			}

			if meta.UDID != "" {
				if _, found, err := simctl.State(pool.SetDirFor(dir), meta.UDID); err == nil && !found {
					note("%s: meta.json references device %s which no longer exists", label, meta.UDID)
				}
			}

			if free {
				if meta.UDID != "" {
					if live, _ := procs.MatchingPIDs(meta.UDID); len(live) > 0 {
						note("%s: lock is free but %d process(es) still reference device %s (run `simpool reap`)", label, len(live), meta.UDID)
					}
				}
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
				if !procs.Alive(h) {
					continue
				}
				children, _ := procs.ChildPIDs(h)
				if len(children) == 0 {
					note("%s: held by pid %d with no live child work — stuck, run `simpool reap`", label, h)
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
