package procs

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

const runtimePrefix = "/Library/Developer/CoreSimulator/Volumes/iOS_23D8133/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.3.simruntime/Contents/Resources/RuntimeRoot"

// argvLine is what the FIRST pass sees: pid and argv, no environment. This
// is what `ps -Awwo pid=,args=` actually returns for a simulator process —
// modelling it with the environment attached would let a test pass against
// output the real command never produces.
func argvLine(pid int, binary string) string {
	return strconv.Itoa(pid) + " " + runtimePrefix + binary
}

// envLine is what the SECOND pass returns for an explicitly named pid: argv
// with the environment appended.
func envLine(pid int, binary, udid string) string {
	return argvLine(pid, binary) +
		" XPC_SIMULATOR_LAUNCHD_NAME=com.apple.CoreSimulator.SimDevice." + udid +
		" SIMULATOR_HOST_HOME=/Users/someone"
}

type fakePS struct {
	argv     string
	argvErr  error
	env      string
	envErr   error
	envCalls [][]int
}

func withFakePS(t *testing.T, f *fakePS) *fakePS {
	t.Helper()
	origArgv, origEnv := argvSnapshotFunc, envForPIDsFunc
	t.Cleanup(func() { argvSnapshotFunc, envForPIDsFunc = origArgv, origEnv })
	argvSnapshotFunc = func() ([]byte, error) { return []byte(f.argv), f.argvErr }
	envForPIDsFunc = func(pids []int) ([]byte, error) {
		f.envCalls = append(f.envCalls, append([]int(nil), pids...))
		return []byte(f.env), f.envErr
	}
	return f
}

const (
	udidA = "DD66B266-011D-4D8B-B46E-DDFAC60169FD"
	udidB = "7700650D-E715-4AF0-854A-1580B1FC3F34"
)

// The bug this exists to prevent: on Darwin `ps -A` silently drops the
// environment, so a scan that expects the device tag in the full listing
// finds nothing and calls a machine full of orphans clean. Identity must
// come from the second, explicitly-addressed pass.
func TestSimRuntimeProcesses_ReadsIdentityFromSecondPass(t *testing.T) {
	f := withFakePS(t, &fakePS{
		argv: argvLine(4318, "/usr/libexec/configd_sim") + "\n",
		env:  envLine(4318, "/usr/libexec/configd_sim", udidA) + "\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 1 || got[0].PID != 4318 || got[0].UDID != udidA {
		t.Fatalf("got %+v, want 4318 on %s", got, udidA)
	}
	if len(f.envCalls) != 1 || len(f.envCalls[0]) != 1 || f.envCalls[0][0] != 4318 {
		t.Fatalf("env pass called with %v, want exactly the one candidate pid", f.envCalls)
	}
}

// A host process that merely mentions a UDID is the most dangerous false
// positive available: `simctl spawn <udid> log stream`, a mav run, a shell
// whose title carries the path. Killing one kills a user's actual work on a
// device that is very much alive.
func TestSimRuntimeProcesses_IgnoresHostProcessMentioningUDID(t *testing.T) {
	line := "5120 /usr/bin/xcrun simctl spawn " + udidA + " log stream"
	f := withFakePS(t, &fakePS{
		argv: line + "\n",
		env:  line + " XPC_SIMULATOR_LAUNCHD_NAME=com.apple.CoreSimulator.SimDevice." + udidA + "\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing: host tooling is not simulated-OS code", got)
	}
	if len(f.envCalls) != 0 {
		t.Fatalf("env pass ran for %v; a non-runtime process is never a candidate", f.envCalls)
	}
}

// The runtime path carries no device identity — every device on one iOS
// version executes byte-identical paths — so a process whose environment
// does not name a device cannot be attributed to one, and must not be
// guessed at.
func TestSimRuntimeProcesses_IgnoresRuntimeProcessWithoutDeviceTag(t *testing.T) {
	withFakePS(t, &fakePS{
		argv: argvLine(4319, "/usr/libexec/backboardd") + "\n",
		env:  argvLine(4319, "/usr/libexec/backboardd") + " HOME=/Users/someone\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing: no device tag means no identity", got)
	}
}

// launchd_sim's executable is not under a RuntimeRoot and its argv[0] is a
// bare name, so the env pass alone would miss it — leaving alive the only
// process in a dead device's tree still able to spawn more.
func TestSimRuntimeProcesses_RecognisesLaunchdSimFromArgvAlone(t *testing.T) {
	const udid = "3A8339E4-1A6D-435F-B4AC-47599DDA6587"
	f := withFakePS(t, &fakePS{
		argv:   "3910 launchd_sim /Users/someone/Library/Developer/CoreSimulator/Devices/" + udid + "/data/var/run\n",
		envErr: errors.New("env pass must not be needed for launchd_sim"),
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 1 || got[0].PID != 3910 || got[0].UDID != udid {
		t.Fatalf("got %+v, want launchd_sim 3910 on %s", got, udid)
	}
	if len(f.envCalls) != 0 {
		t.Fatalf("env pass ran for %v; launchd_sim identifies itself from argv", f.envCalls)
	}
}

// Matching "launchd_sim" anywhere in the line rather than on argv[0] would
// make this very package's own tooling — a grep, a log tail, a ps pipeline —
// look like a simulator's init process.
func TestSimRuntimeProcesses_IgnoresProcessMerelyMentioningLaunchdSim(t *testing.T) {
	const udid = "3A8339E4-1A6D-435F-B4AC-47599DDA6587"
	withFakePS(t, &fakePS{
		argv: "7001 /usr/bin/grep launchd_sim /Users/someone/Library/Developer/CoreSimulator/Devices/" + udid + "/data/log\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing: grep is not launchd_sim", got)
	}
}

// pid 1 is the host's launchd. No parsing accident may ever put it on a kill
// list.
func TestSimRuntimeProcesses_NeverReportsPID1(t *testing.T) {
	withFakePS(t, &fakePS{
		argv: argvLine(1, "/usr/libexec/configd_sim") + "\n",
		env:  envLine(1, "/usr/libexec/configd_sim", udidA) + "\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing: pid 1 is the host's launchd", got)
	}
}

// The candidate list comes from an earlier snapshot; a pid that exits in
// between can be reassigned to something entirely unrelated before the env
// pass runs, and that new process must not inherit the candidacy.
func TestSimRuntimeProcesses_RechecksRuntimePathOnSecondPass(t *testing.T) {
	withFakePS(t, &fakePS{
		argv: argvLine(4318, "/usr/libexec/configd_sim") + "\n",
		env:  "4318 /usr/bin/vim notes.txt XPC_SIMULATOR_LAUNCHD_NAME=com.apple.CoreSimulator.SimDevice." + udidA + "\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want nothing: the pid was recycled into an unrelated process", got)
	}
}

// A scan that could not be run must surface as an error. Returning an empty
// list instead is indistinguishable from a healthy machine — which is how
// this check would fail silently rather than loudly.
func TestSimRuntimeProcesses_PropagatesArgvSnapshotFailure(t *testing.T) {
	withFakePS(t, &fakePS{argvErr: errors.New("fork: resource temporarily unavailable")})

	if got, err := SimRuntimeProcesses(); err == nil {
		t.Fatalf("got %+v with nil error, want the snapshot failure surfaced", got)
	}
}

func TestSimRuntimeProcesses_PropagatesEnvPassFailure(t *testing.T) {
	withFakePS(t, &fakePS{
		argv:   argvLine(4318, "/usr/libexec/configd_sim") + "\n",
		envErr: errors.New("fork: resource temporarily unavailable"),
	})

	if got, err := SimRuntimeProcesses(); err == nil {
		t.Fatalf("got %+v with nil error, want the env-pass failure surfaced", got)
	}
}

func TestSimRuntimeProcesses_SeparatesDevices(t *testing.T) {
	withFakePS(t, &fakePS{
		argv: strings.Join([]string{
			argvLine(101, "/usr/libexec/backboardd"),
			argvLine(102, "/usr/libexec/configd_sim"),
			argvLine(103, "/usr/libexec/notifyd"),
		}, "\n") + "\n",
		env: strings.Join([]string{
			envLine(101, "/usr/libexec/backboardd", udidA),
			envLine(102, "/usr/libexec/configd_sim", udidB),
			envLine(103, "/usr/libexec/notifyd", udidA),
		}, "\n") + "\n",
	})

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	byUDID := map[string]int{}
	for _, p := range got {
		byUDID[p.UDID]++
	}
	if byUDID[udidA] != 2 || byUDID[udidB] != 1 {
		t.Fatalf("got %+v, want 2 on %s and 1 on %s", got, udidA, udidB)
	}
}

// A machine with thousands of simulator processes must not push the pid list
// past ARG_MAX and turn the whole scan into an error.
//
// wantChunk is written out rather than read from pidChunk on purpose: a test
// that sizes its own input from the constant it is checking moves with any
// change to that constant and can never fail, whatever the value becomes.
// Changing the batch size should require deliberately changing this number
// too.
func TestSimRuntimeProcesses_ChunksLargeCandidateLists(t *testing.T) {
	const wantChunk = 500

	var argv []string
	for i := 0; i < wantChunk+7; i++ {
		argv = append(argv, argvLine(1000+i, "/usr/libexec/configd_sim"))
	}
	f := withFakePS(t, &fakePS{argv: strings.Join(argv, "\n") + "\n"})

	if _, err := SimRuntimeProcesses(); err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}
	if len(f.envCalls) != 2 {
		t.Fatalf("env pass called %d time(s), want 2 chunks of at most %d", len(f.envCalls), wantChunk)
	}
	if len(f.envCalls[0]) != wantChunk || len(f.envCalls[1]) != 7 {
		t.Fatalf("chunk sizes %d and %d, want %d and 7", len(f.envCalls[0]), len(f.envCalls[1]), wantChunk)
	}
}
