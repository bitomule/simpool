package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// RunWith implements `simpool with [flags] -- <cmd...>`: acquire N slots,
// export the environment contract, run <cmd> as a child, and release the
// slots when it exits — however it exits.
func RunWith(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("with", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var af acquireFlags
	parseAcquireFlags(fs, &af)
	flagArgs, cmdArgs := splitDoubleDash(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if err := af.validate(); err != nil {
		fmt.Fprintln(stderr, "simpool with:", err)
		return 2
	}
	if len(cmdArgs) == 0 {
		fmt.Fprintln(stderr, "simpool with: missing command after --")
		return 2
	}

	ownerCmd := strings.Join(cmdArgs, " ")
	slots, runDir, err := acquireAndProvision(af.device, af.os, af.count, ownerCmd)
	if err != nil {
		fmt.Fprintln(stderr, "simpool with:", err)
		return 1
	}
	defer releaseAll(slots)

	env := append(os.Environ(), envLines(slots, runDir)...)

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Setpgid puts the child in its own process group, led by its own pid.
	// Combined with never sharing our lock fd (see pool.Lock), this is the
	// whole basis of the design: we hold the lock, the child and every
	// descendant it spawns live in a group we can sweep in one call, and
	// none of them can ever retain the lock themselves.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "simpool with: starting command:", err)
		return 1
	}
	pgid := cmd.Process.Pid

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case sig := <-sigCh:
		_ = procs.KillProcessGroup(pgid, sig.(syscall.Signal))
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			// Child ignored the signal; escalate.
			_ = procs.KillProcessGroup(pgid, syscall.SIGKILL)
			waitErr = <-done
		}
	case waitErr = <-done:
	}

	// Always sweep the child's process group on the way out, regardless of
	// how it exited. This is what reclaims grandchildren the child itself
	// forgot about (a `log stream` MAV spawned and never reaped) — see
	// design doc §4.
	_ = procs.KillProcessGroup(pgid, syscall.SIGKILL)

	if waitErr == nil {
		return 0
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(stderr, "simpool with:", waitErr)
	return 1
}
