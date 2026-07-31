package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

// RunAcquire implements `simpool acquire [flags]`: locks N slots, prints
// their environment as shell `export` lines, then blocks holding the lock
// until it receives SIGINT/SIGTERM/SIGHUP. Meant for scripts that need the
// UDIDs up front and will manage the workload themselves.
func RunAcquire(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("acquire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var af acquireFlags
	parseAcquireFlags(fs, &af)
	flagArgs, cmdArgs := splitDoubleDash(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if err := af.validate(); err != nil {
		fmt.Fprintln(stderr, "simpool acquire:", err)
		return 2
	}
	if len(cmdArgs) > 0 {
		fmt.Fprintln(stderr, "simpool acquire: takes no command; use `simpool with -- <cmd>` to run one")
		return 2
	}

	ownerCmd := "acquire (pid " + strconv.Itoa(os.Getpid()) + ")"
	slots, runDir, err := acquireAndProvision(af.device, af.os, af.count, ownerCmd)
	if err != nil {
		fmt.Fprintln(stderr, "simpool acquire:", err)
		return 1
	}
	defer releaseAll(slots)

	printEnvLines(stdout, envLines(slots, runDir))
	if f, ok := stdout.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	<-sigCh
	signal.Stop(sigCh)
	return 0
}
