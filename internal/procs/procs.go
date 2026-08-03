// Package procs provides small process-inspection helpers used by reap and
// doctor to tell a genuinely idle slot from one that still has live work
// attached to it.
package procs

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Alive reports whether pid currently exists, without sending it a signal
// that would affect it (signal 0).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// MatchingPIDs returns the PIDs of live processes whose command line
// contains needle (via `pgrep -f`). Used to detect orphaned consumers
// (e.g. `simctl spawn <udid> log stream`) referencing a simulator's UDID.
func MatchingPIDs(needle string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", needle).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil // pgrep: no matches
		}
		return nil, err
	}
	return parsePIDs(out), nil
}

// ChildPIDs returns the direct children of pid (via `pgrep -P`).
func ChildPIDs(pid int) ([]int, error) {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	return parsePIDs(out), nil
}

// LockHolders returns the PIDs that currently have path open (via `lsof`).
func LockHolders(path string) ([]int, error) {
	out, err := exec.Command("lsof", "-t", path).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil // lsof: nothing has it open
		}
		return nil, err
	}
	return parsePIDs(out), nil
}

func parsePIDs(out []byte) []int {
	var pids []int
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		n, err := strconv.Atoi(string(line))
		if err != nil {
			continue
		}
		pids = append(pids, n)
	}
	return pids
}

// KillProcessGroup sends sig to the process group led by pid (i.e. -pid).
// Errors are swallowed for ESRCH (already gone).
//
// Only meaningful when pid is actually the leader of its own process group
// (its pgid == its pid). Callers that don't know that must not use this —
// see Kill.
func KillProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// Kill sends sig to exactly pid (never a process group). Errors are
// swallowed for ESRCH (already gone). Use this instead of KillProcessGroup
// whenever the caller has not verified pid leads its own process group —
// e.g. `simpool with` itself inherits its pgid from the invoking shell, so
// `kill(-pid, sig)` on it targets whatever unrelated process group happens
// to share that numeric id, not "with" and its descendants.
func Kill(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// Descendants returns every PID transitively spawned by pid (children,
// grandchildren, ...), best-effort: a pgrep failure partway through just
// truncates the results rather than failing the whole call, since this is
// only ever used to build an exclusion set, not a safety-critical list.
func Descendants(pid int) []int {
	var out []int
	children, err := ChildPIDs(pid)
	if err != nil {
		return out
	}
	for _, c := range children {
		out = append(out, c)
		out = append(out, Descendants(c)...)
	}
	return out
}

// PGIDAlive reports whether any process still belongs to process group pgid,
// via kill(-pgid, 0): the kernel delivers a null signal to every member of
// the group and reports ESRCH only if there are none left. Unlike
// MatchingPIDs/LiveConsumers, this needs no command-line evidence at all —
// it catches a consumer whose only distinguishing trace is an environment
// variable (MAV_TARGET_UDID, SIMPOOL_UDID_N) rather than anything visible in
// `ps`, which is exactly how simpool hands off a simulator (see design doc
// §5) and therefore exactly the case a pgrep-based check cannot see.
func PGIDAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	return syscall.Kill(-pgid, 0) == nil
}

// LiveConsumers returns the PIDs of live host processes that reference udid
// on their command line and are NOT the simulator's own runtime (its
// `launchd_sim` process and everything launchd_sim spawns inside the
// simulated OS — SpringBoard, backboardd, apps under test, ...). Those
// always reference paths under the device's UDID the entire time it is
// booted, so a plain `pgrep -f <udid>` match is not evidence of an external
// consumer still attached to the device — only a genuine orphan (e.g. a
// `simctl spawn <udid> log stream` MAV forgot to reap) is.
func LiveConsumers(udid string) ([]int, error) {
	matches, err := MatchingPIDs(udid)
	if err != nil || len(matches) == 0 {
		return matches, err
	}

	simRoots, err := MatchingPIDs("launchd_sim")
	if err != nil {
		return nil, err
	}
	exclude := map[int]bool{}
	for _, root := range simRoots {
		if !strings.Contains(CommandLine(root), udid) {
			continue
		}
		exclude[root] = true
		for _, d := range Descendants(root) {
			exclude[d] = true
		}
	}

	var live []int
	for _, p := range matches {
		if !exclude[p] {
			live = append(live, p)
		}
	}
	return live, nil
}

// IsSimpoolHolder reports whether pid's command line looks like a
// `simpool <subcommand>` invocation. Used before treating an `lsof`-derived
// lock "holder" as trustworthy: `lsof -t <lockfile>` reports every process
// that has the file open at all, including a `simpool status`/`doctor`/
// `reap` that merely probed it and closed it again — Darwin's lsof does not
// reliably expose which opener actually holds the flock, so command-line
// identity is the corroborating signal available.
func IsSimpoolHolder(pid int, subcommand string) bool {
	cl := CommandLine(pid)
	if cl == "" {
		return false
	}
	fields := strings.Fields(cl)
	if len(fields) < 2 {
		return false
	}
	bin := fields[0]
	if bin != "simpool" && !strings.HasSuffix(bin, "/simpool") {
		return false
	}
	return fields[1] == subcommand
}

// CommandLine returns the full command line of pid, best-effort ("" if it
// can't be read, e.g. the process already exited or belongs to another
// user).
func CommandLine(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
