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
  simpool with [--device D] [--os V] [--count N] -- <cmd>
      Acquire N slots, export the environment, run <cmd>, release on exit.

  simpool acquire [--device D] [--os V] [--count N]
      Print the environment for N slots and hold the lock until signaled.

  simpool status
      List every slot: lock state, holder, device boot state.

  simpool reap [--cold N] [--dry-run]
      Recycle free+cold slots; never touches a locked one.

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
