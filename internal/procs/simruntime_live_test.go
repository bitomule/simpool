package procs

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSimRuntimeProcesses_Live runs the real scan against the real machine.
//
// It exists because every other test in this file feeds SimRuntimeProcesses
// synthetic `ps` output, and the first implementation passed all of them
// while finding almost nothing in reality: it read the device tag from a
// bulk `ps -Aewwo`, and on Darwin `-E` and `-A` do not compose — the full
// listing drops the environment. The tag showed up for 1 process out of 804.
// Nothing that mocks `ps` can catch that; only running it can.
//
// Opt-in, because it needs booted simulators to be meaningful:
//
//	SIMPOOL_RUN_LIVE_TESTS=1 go test ./internal/procs/ -run Live -v
func TestSimRuntimeProcesses_Live(t *testing.T) {
	if os.Getenv("SIMPOOL_RUN_LIVE_TESTS") != "1" {
		t.Skip("set SIMPOOL_RUN_LIVE_TESTS=1 to run against the real machine")
	}

	out, err := exec.Command("/bin/ps", "-Awwo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("listing processes: %v", err)
	}
	expected := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, runtimeRootMarker) {
			expected++
		}
	}
	if expected == 0 {
		t.Skip("no simulator is booted; nothing to attribute")
	}

	got, err := SimRuntimeProcesses()
	if err != nil {
		t.Fatalf("SimRuntimeProcesses: %v", err)
	}

	// The bar is deliberately "most of them", not "all": processes exit
	// between the two passes, and a handful of runtime binaries genuinely
	// carry no device tag. A scan reading the environment from the wrong
	// kind of `ps` call lands near zero, which is what this catches.
	if len(got) < expected/2 {
		t.Fatalf("attributed %d of %d live simulator-runtime processes to a device — the scan is not reading identity correctly", len(got), expected)
	}

	devices := map[string]int{}
	for _, p := range got {
		if p.UDID == "" {
			t.Fatalf("pid %d attributed to an empty UDID", p.PID)
		}
		devices[p.UDID]++
	}
	t.Logf("attributed %d of %d runtime processes across %d device(s)", len(got), expected, len(devices))
}
