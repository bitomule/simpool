// Package procs provides small process-inspection helpers used by reap and
// doctor to tell a genuinely idle slot from one that still has live work
// attached to it.
package procs

import (
	"bufio"
	"bytes"
	"fmt"
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

// pgrepFunc runs `pgrep -f needle` and returns its raw stdout. A package
// variable, not a hardcoded exec.Command call, so tests can deterministically
// simulate the kind of failure a loaded machine can genuinely produce (pgrep
// itself failing to fork under memory/process-table pressure) — something
// otherwise close to impossible to reproduce on demand.
var pgrepFunc = func(needle string) ([]byte, error) {
	return exec.Command("pgrep", "-f", needle).Output()
}

// MatchingPIDs returns the PIDs of live processes whose command line
// contains needle (via `pgrep -f`). Used to detect orphaned consumers
// (e.g. `simctl spawn <udid> log stream`) referencing a simulator's UDID.
// An error here is a genuine "could not check" (pgrep itself failed to
// run), not "no matches" (exit code 1, which pgrep uses for that and is
// deliberately not an error) — callers (see pool.CheckPoison) must treat
// it as "assume busy", never as "assume free".
func MatchingPIDs(needle string) ([]int, error) {
	out, err := pgrepFunc(needle)
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

// processSnapshotFunc fetches every live process's (pid, ppid) pair in one
// call. A package variable so tests can inject a synthetic tree without
// spawning hundreds of real child processes to prove the traversal logic.
var processSnapshotFunc = func() ([]byte, error) {
	return exec.Command("ps", "-axo", "pid=,ppid=").Output()
}

// processTree parses `ps -axo pid=,ppid=` into a parent -> direct-children
// map for every process on the host, in a single subprocess call regardless
// of how many processes exist.
//
// Descendants used to recurse via ChildPIDs, forking one `pgrep -P` per
// process in the subtree — measured at ~8.5s wall time for a single
// LiveConsumers call against a booted iOS 26.3 simulator (281 direct
// children of launchd_sim, each triggering its own recursive pgrep), pure
// fork/exec overhead rather than CPU work. A single snapshot plus an
// in-memory walk turns that into one subprocess call total; the same
// snapshot benchmark after this change measures well under 100ms.
func processTree() (map[int][]int, error) {
	out, err := processSnapshotFunc()
	if err != nil {
		return nil, err
	}
	tree := map[int][]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		tree[ppid] = append(tree[ppid], pid)
	}
	return tree, sc.Err()
}

// Descendants returns every PID transitively spawned by pid (children,
// grandchildren, ...), best-effort: a snapshot failure just yields nil (an
// empty exclusion set) rather than failing the whole call, since this is
// only ever used to build an exclusion set, not a safety-critical list.
func Descendants(pid int) []int {
	tree, err := processTree()
	if err != nil {
		return nil
	}
	var out []int
	var walk func(int)
	walk = func(p int) {
		for _, c := range tree[p] {
			out = append(out, c)
			walk(c)
		}
	}
	walk(pid)
	return out
}

// killFunc wraps syscall.Kill so tests can deterministically simulate a
// signal-delivery failure (EPERM: the process group exists but isn't ours
// to signal) that is otherwise not reliably reproducible on demand.
var killFunc = syscall.Kill

// PGIDAlive reports whether any process still belongs to process group pgid,
// via kill(-pgid, 0): the kernel delivers a null signal to every member of
// the group and reports ESRCH only if there are none left. Unlike
// MatchingPIDs/LiveConsumers, this needs no command-line evidence at all —
// it catches a consumer whose only distinguishing trace is an environment
// variable (MAV_TARGET_UDID, SIMPOOL_UDID_N) rather than anything visible in
// `ps`, which is exactly how simpool hands off a simulator (see design doc
// §5) and therefore exactly the case a pgrep-based check cannot see.
//
// Any error other than ESRCH (most notably EPERM: the group exists but the
// caller lacks permission to signal it) is treated as "still alive" — a
// check that could not positively confirm the group is gone must fail
// toward "busy, don't touch", never toward "free, hand it out".
func PGIDAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := killFunc(-pgid, 0)
	if err == nil {
		return true
	}
	if err == syscall.ESRCH {
		return false
	}
	return true
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

// startTimeEnv is the fixed, locale/timezone-independent environment
// ProcessStartTime always runs `ps` under — never the caller's ambient
// environment.
//
// This exists to fix a critical bug in an earlier, reverted version of
// this feature: it fingerprinted a process via the *whole line* of
// `sysctl kern.boottime`, which macOS renders as a date in whatever TZ
// (and formatted per whatever LC_ALL/LANG) the invoking process happens to
// have — measured directly: the exact same boot produced
// "... Fri Jul 31 09:51:34 2026" under TZ=UTC and
// "... Fri Jul 31 18:51:34 2026" under TZ=Asia/Tokyo. A strict string
// comparison of two renderings of the same instant, captured under two
// different ambient environments, reads as "these differ" — and in that
// design, the differing branch was the destructive one. `ps -o lstart=`
// (used here) has the exact same weakness — confirmed separately: it is
// sensitive to both TZ and LC_ALL (LC_ALL=es_ES renders a Spanish month
// name). Forcing one fixed environment on every invocation — not just
// recording "whatever the ambient environment happened to be" — is what
// makes the output stable and comparable by plain string equality,
// regardless of what TZ/LC_ALL/LANG look like wherever this is called
// from, at either the recording end or the verifying end.
var startTimeEnv = []string{"TZ=UTC0", "LC_ALL=C", "LANG=C", "PATH=/bin:/usr/bin"}

// ProcessStartTime returns pid's start time (`ps -o lstart=`, rendered
// under a fixed environment — see startTimeEnv), for identity verification
// before a poisoned slot's recovery is allowed to kill anything: macOS
// recycles pids, so a live process under a recorded pgid is not by itself
// proof it's the same process that was recorded — this must match exactly
// first. The returned string is opaque: it is never parsed, only ever
// compared for equality against a value recorded earlier the same way (see
// pool.Meta.ConsumerStartedAt). Errors (no such process) are returned, not
// swallowed — the caller must treat "can't verify" as "don't kill".
func ProcessStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = startTimeEnv
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("no such process %d", pid)
	}
	return s, nil
}
