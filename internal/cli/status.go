package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// RunStatus implements `simpool status`: lists every slot, whether it is
// free or busy, who (best-effort) holds it, and whether its simulator is
// booted or cold.
func RunStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool status:", err)
		return 1
	}

	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		fmt.Fprintln(stderr, "simpool status:", err)
		return 1
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "pool is empty:", root)
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tSLOT\tLOCK\tHELD BY\tDEVICE STATE\tUDID")
	for _, groupDir := range groups {
		group := filepath.Base(groupDir)
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			meta := pool.ReadMeta(dir)

			free, err := pool.IsSlotFree(dir)
			lockCol := "free"
			heldBy := "-"
			if err != nil {
				lockCol = "error"
			} else if !free {
				lockCol = "busy"
				if holders, _ := procs.LockHolders(pool.LockPath(dir)); len(holders) > 0 {
					var parts []string
					for _, h := range holders {
						parts = append(parts, fmt.Sprintf("pid %d", h))
					}
					heldBy = strings.Join(parts, ", ")
				} else if meta.OwnerPID != 0 {
					heldBy = fmt.Sprintf("pid %d (meta, unverified)", meta.OwnerPID)
				}
			}

			deviceState := "unprovisioned"
			if meta.UDID != "" {
				if state, found, err := simctl.State(meta.UDID); err == nil && found {
					deviceState = state
				} else {
					deviceState = "missing"
				}
			}

			fmt.Fprintf(tw, "%s\tslot-%d\t%s\t%s\t%s\t%s\n", group, n, lockCol, heldBy, deviceState, meta.UDID)
		}
	}
	tw.Flush()
	return 0
}
