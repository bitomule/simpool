package procs

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// SimRuntimeProcess is one live host process that belongs to a simulator's
// *simulated* operating system — the launchd_sim tree CoreSimulator starts
// when a device boots (SpringBoard, backboardd, configd_sim, the app under
// test, ...) — together with the UDID of the device it was started for.
//
// These are ordinary host processes; the simulator is not a VM and has no
// PID namespace. Nothing outside CoreSimulator has any reason to know they
// exist, and nothing here should treat one as an external consumer of a
// slot: they are the device, not a user of it.
type SimRuntimeProcess struct {
	PID  int
	UDID string
}

// runtimeRootMarker appears in the executable path of every binary running
// inside a simulated OS, e.g.
//
//	/Library/Developer/CoreSimulator/Volumes/iOS_23D8133/Library/Developer/
//	CoreSimulator/Profiles/Runtimes/iOS 26.3.simruntime/Contents/Resources/
//	RuntimeRoot/usr/libexec/backboardd
//
// It is what separates simulated-OS code from the host-side CoreSimulator
// daemons (SimRenderServer, CoreSimulatorBridge, simdiskimaged) that live
// under CoreSimulator.framework instead and must never be swept: those are
// shared infrastructure, not per-device processes.
const runtimeRootMarker = ".simruntime/Contents/Resources/RuntimeRoot/"

// launchdSimUDIDRe recovers a device UDID from a launchd_sim command line,
// which names the device's data directory rather than a RuntimeRoot path
// (its argv[0] is the bare string "launchd_sim"). launchd_sim is the root of
// the tree, so missing it would leave running the one process that can still
// spawn new members of a dead device's userland.
var launchdSimUDIDRe = regexp.MustCompile(`/Devices/([0-9A-Fa-f-]{36})/`)

// simDeviceEnvTag is the environment variable CoreSimulator stamps on every
// process it starts inside a simulated OS, naming the device's XPC domain:
//
//	XPC_SIMULATOR_LAUNCHD_NAME=com.apple.CoreSimulator.SimDevice.<UDID>
//
// This is the only trustworthy device identity such a process carries. Its
// command line is just a path inside the runtime — shared byte-for-byte by
// every device on the same iOS version — so command-line matching cannot
// tell one device's backboardd from another's. Reading the tag CoreSimulator
// itself wrote is what makes "which device does this process belong to?" a
// question with an answer rather than a guess.
const simDeviceEnvTag = "XPC_SIMULATOR_LAUNCHD_NAME=com.apple.CoreSimulator.SimDevice."

// argvSnapshotFunc lists every live process's pid and argv. No environment:
// see envForPIDsFunc for why that needs a second, different call.
//
// startTimeEnv is reused deliberately — it pins PATH and forces a
// locale- and timezone-independent rendering, and `/bin/ps` is spelled
// absolutely, for the same reason ProcessStartTime does it: this output
// decides whether processes get killed.
var argvSnapshotFunc = func() ([]byte, error) {
	cmd := exec.Command("/bin/ps", "-Awwo", "pid=,args=")
	cmd.Env = startTimeEnv
	return cmd.Output()
}

// envForPIDsFunc returns pid + argv + environment for an explicit list of
// pids.
//
// This exists as a second pass, rather than one `ps -Aewwo` over everything,
// because on Darwin `-E` and `-A` do not compose: a full listing silently
// drops the environment and prints argv alone. Measured on this machine with
// 804 live simulator-runtime processes — a bulk `ps -Aewwo pid=,command=`
// reported the device tag for 1 of them, while querying the same pids
// explicitly reported it for 40 of 40. A scan built on the bulk form finds
// essentially nothing and reports a machine full of orphans as clean, which
// is exactly the way this check would fail silently rather than loudly.
//
// Explicit pids also keep the cost at one call: `ps` accepts the whole list
// in a single `-p`, so the second pass is one more fork, not one per
// process.
var envForPIDsFunc = func(pids []int) ([]byte, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	list := make([]string, 0, len(pids))
	for _, p := range pids {
		list = append(list, strconv.Itoa(p))
	}
	cmd := exec.Command("/bin/ps", "-Ewwo", "pid=,command=", "-p", strings.Join(list, ","))
	cmd.Env = startTimeEnv
	// A pid that exited between the two passes makes ps exit non-zero even
	// though it printed every other pid perfectly well, so output is used
	// whenever there is any, and the error only matters when there is none.
	out, err := cmd.Output()
	if len(out) > 0 {
		return out, nil
	}
	return out, err
}

// pidChunk bounds how many pids go into one `ps -p` invocation, so a machine
// with pathologically many simulator processes cannot push the argument list
// past ARG_MAX and turn the whole scan into an error.
const pidChunk = 500

// SimRuntimeProcesses returns every live process belonging to some
// simulator's simulated OS, tagged with the device UDID it was started for.
//
// It reports what exists, and deliberately says nothing about whether any of
// those devices still exist — that judgement needs a device listing, and
// folding the two together here would let a failed or empty listing silently
// turn into "everything is an orphan". The caller pairs this with simctl's
// own list; see cli.findOrphanedRuntimes.
//
// An error means the scan itself could not be run (ps failing to fork on a
// loaded machine, say), never "found nothing" — the same fail-toward-busy
// contract MatchingPIDs already has.
func SimRuntimeProcesses() ([]SimRuntimeProcess, error) {
	out, err := argvSnapshotFunc()
	if err != nil {
		return nil, err
	}

	var found []SimRuntimeProcess
	var needEnv []int
	for _, line := range strings.Split(string(out), "\n") {
		pid, rest, ok := splitPIDLine(line)
		if !ok {
			continue
		}
		// launchd_sim carries the device's data directory on its own
		// command line, so it needs no second pass — and must not be
		// skipped if that pass fails, since it is the one process that can
		// still spawn new members of the tree.
		if isLaunchdSim(rest) {
			if m := launchdSimUDIDRe.FindStringSubmatch(rest); m != nil {
				found = append(found, SimRuntimeProcess{PID: pid, UDID: m[1]})
			}
			continue
		}
		if strings.Contains(rest, runtimeRootMarker) {
			needEnv = append(needEnv, pid)
		}
	}

	for start := 0; start < len(needEnv); start += pidChunk {
		end := start + pidChunk
		if end > len(needEnv) {
			end = len(needEnv)
		}
		envOut, err := envForPIDsFunc(needEnv[start:end])
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(envOut), "\n") {
			pid, rest, ok := splitPIDLine(line)
			if !ok {
				continue
			}
			// Re-checking the runtime path on the second pass is not
			// redundant: the pid list was built from an earlier snapshot,
			// and a pid that exited in between can be handed to a brand new,
			// entirely unrelated process before this call runs.
			if !strings.Contains(rest, runtimeRootMarker) {
				continue
			}
			if udid, ok := deviceTagUDID(rest); ok {
				found = append(found, SimRuntimeProcess{PID: pid, UDID: udid})
			}
		}
	}
	return found, nil
}

// splitPIDLine parses one `pid <rest>` line of ps output. pid 1 is the
// host's launchd, which can never be simulated-OS code; rejecting it here
// means no parsing accident can put the host's init process on a kill list.
func splitPIDLine(line string) (int, string, bool) {
	line = strings.TrimSpace(line)
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(line[:sp])
	if err != nil || pid <= 1 {
		return 0, "", false
	}
	return pid, line[sp+1:], true
}

// deviceTagUDID reads the device UDID out of the environment CoreSimulator
// stamped on a process. Requiring the tag is what stops a runtime path alone
// from being read as an identity it does not carry: every device on one iOS
// version executes byte-identical paths.
func deviceTagUDID(text string) (string, bool) {
	i := strings.Index(text, simDeviceEnvTag)
	if i < 0 {
		return "", false
	}
	tail := text[i+len(simDeviceEnvTag):]
	end := strings.IndexAny(tail, " \t")
	if end < 0 {
		end = len(tail)
	}
	if udid := strings.TrimSpace(tail[:end]); udid != "" {
		return udid, true
	}
	return "", false
}

// isLaunchdSim reports whether a command line is CoreSimulator's per-device
// init process. Matched on the argv[0] token alone rather than anywhere in
// the line, so a process that merely mentions launchd_sim — this binary's
// own `ps` pipeline, a log tail, a grep — is never mistaken for one.
func isLaunchdSim(text string) bool {
	first := text
	if sp := strings.IndexAny(first, " \t"); sp >= 0 {
		first = first[:sp]
	}
	if i := strings.LastIndexByte(first, '/'); i >= 0 {
		first = first[i+1:]
	}
	return first == "launchd_sim"
}
