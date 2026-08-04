// Command simpool is a broker for iOS simulators shared by several agents
// on one machine. See the design doc for the full rationale; in short:
// simpool holds an exclusive flock per slot, launches the consumer as a
// child process with the lock fd never inherited, and kills the child's
// whole process group on the way out.
package main

import (
	"fmt"
	"os"

	"github.com/bitomule/simpool/internal/cli"
)

func usage() {
	fmt.Fprintln(os.Stderr, `simpool — iOS simulator pool broker

Usage:
  simpool with [--device D] [--os V] [--count N] [--max M] [--wait D] -- <cmd>
      Acquire N slots, export the environment, run <cmd>, release on exit.

  simpool acquire [--device D] [--os V] [--count N] [--max M] [--wait D]
      Print the environment for N slots and hold the lock until signaled.

  simpool lease --device D --os V [--key K] [--ttl D] [--max M]
      Print just a UDID and exit. For short, independent commands in a
      hot loop (mav tap/swipe/screenshot, wired as MAV's target_command)
      that have nothing to hold "with"'s lock across. Sticky per --key
      (default: git repo root, else cwd): repeated calls with the same
      key return the same slot, renewing a TTL — NOT a flock, see README.

  simpool release [--key K]
      Drop --key's lease immediately instead of waiting out its TTL.

  simpool preboot --device D --os V [--count N] [--max M]
      Warm up N slots (boot their simulators) without a consumer, then
      release them immediately, so the next with/acquire/lease/bazel-test
      call finds a warm slot instead of paying a cold boot itself. Never
      waits for capacity — a full group is left as-is, not blocked on.

  simpool status
      List every slot: lock state, holder, lease, device boot state.

  simpool reap [--cold N] [--stuck-after D] [--purge N] [--prune-runs-after D] [--warm N] [--dry-run]
      Recycle free+cold slots; never touches one with a live owner or an
      active lease. Also clears expired lease files. --warm caps how many
      free simulators stay booted per group, independent of --max (which
      caps how many may be resident/locked at once).

  simpool doctor
      Check pool coherence. Exits non-zero if anything looks wrong.`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "with":
		code = cli.RunWith(rest, os.Stdout, os.Stderr)
	case "acquire":
		code = cli.RunAcquire(rest, os.Stdout, os.Stderr)
	case "lease":
		code = cli.RunLease(rest, os.Stdout, os.Stderr)
	case "release":
		code = cli.RunRelease(rest, os.Stdout, os.Stderr)
	case "preboot":
		code = cli.RunPreboot(rest, os.Stdout, os.Stderr)
	case "status":
		code = cli.RunStatus(rest, os.Stdout, os.Stderr)
	case "reap":
		code = cli.RunReap(rest, os.Stdout, os.Stderr)
	case "doctor":
		code = cli.RunDoctor(rest, os.Stdout, os.Stderr)
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "simpool: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	os.Exit(code)
}
