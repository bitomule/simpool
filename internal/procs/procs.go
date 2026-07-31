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
func KillProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
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
