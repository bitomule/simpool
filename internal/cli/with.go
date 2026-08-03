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

	"github.com/bitomule/simpool/internal/pool"
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
	slots, runDir, err := acquireAndProvision(af.device, af.os, af.count, af.max, af.wait, ownerCmd, "with")
	if err != nil {
		fmt.Fprintln(stderr, "simpool with:", err)
		return 1
	}
	// Registered before releaseAll/removeRunDirIfEmpty so it runs LAST
	// (defers are LIFO): the group-scoped exit sweep is most useful once
	// this invocation's own slots are already back to "free", and it costs
	// nothing extra here — the command has already delivered its result.
	defer func() {
		if root, err := pool.Root(); err == nil {
			pool.ExitSweep(root, af.device, af.os)
		}
	}()
	defer releaseAll(slots)
	defer removeRunDirIfEmpty(runDir)

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

	// Fingerprint the child right away, alongside its pgid: its own start
	// time and the machine's current boot both need to be captured while
	// the process is known-good (we just started it ourselves), so that a
	// later recovery attempt (pool.AttemptRecovery, if this `with` dies
	// abruptly and the child survives as an orphan) can prove the process
	// it's about to kill is genuinely this one — macOS recycles pids, so a
	// live process under this numeric pgid later is not proof by itself.
	// Best-effort: a failure here just means a future recovery attempt
	// can't verify identity and will quarantine instead of reclaiming.
	startedAt, _ := procs.ProcessStartTime(pgid)
	bootID, _ := procs.MachineBootTime()

	// Record the child's process-group id (and fingerprint) in every
	// acquired slot's meta.json so a free-looking slot can be told apart
	// from one whose consumer is still alive even when that consumer never
	// puts the simulator's UDID anywhere in its own argv — it gets it by
	// environment (MAV_TARGET_UDID, SIMPOOL_UDID_N), which a pgrep-based
	// check cannot see at all (design review CRITICAL finding). Best-effort:
	// a write failure here must not abort a command that has already
	// started.
	for _, s := range slots {
		s.Meta.ConsumerPGID = pgid
		s.Meta.ConsumerStartedAt = startedAt
		s.Meta.ConsumerBootID = bootID
		_ = s.SaveMeta()
	}

	// SIGHUP matters here specifically because the child lives in its own
	// process group (Setpgid above): closing the terminal or ending an
	// SSH/agent session does not reach it directly the way it would a
	// plain foreground child, so if `with` itself dies to the default
	// SIGHUP action without running this handler, the child (and the slot)
	// is orphaned. `acquire` already listens for it; `with` is the 95% of
	// usage and needs it more.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
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

	// The sweep above guarantees pgid is gone, so ConsumerPGID (and its
	// fingerprint) no longer identifies a live (or even meaningful) process
	// group; clear them before the deferred releaseAll persists LastUsed,
	// so a slot recycled by reap while free is never second-guessed against
	// a stale, long-dead pgid.
	for _, s := range slots {
		s.Meta.ConsumerPGID = 0
		s.Meta.ConsumerStartedAt = ""
		s.Meta.ConsumerBootID = ""
	}

	if waitErr == nil {
		return 0
	}
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			// Conventional 128+signal, not ExitCode()'s -1 (which collapses
			// to os.Exit(-1) -> 255, indistinguishable from a real exit(255)
			// from the command itself).
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	fmt.Fprintln(stderr, "simpool with:", waitErr)
	return 1
}
