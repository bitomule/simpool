package procs

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitUntil polls fn every 20ms until it returns true or timeout elapses.
func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeScript writes an executable shell script at dir/name.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDescendants(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "parent.sh", `sleep 300 & echo $!; wait`)

	cmd := exec.Command(script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	parentPID := cmd.Process.Pid
	t.Cleanup(func() { _ = KillProcessGroup(parentPID, syscall.SIGKILL) })

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		t.Fatalf("script produced no output: %v", sc.Err())
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil {
		t.Fatalf("parsing child pid: %v", err)
	}
	t.Cleanup(func() { _ = Kill(childPID, syscall.SIGKILL) })

	var desc []int
	waitUntil(t, 3*time.Second, func() bool {
		desc = Descendants(parentPID)
		for _, d := range desc {
			if d == childPID {
				return true
			}
		}
		return false
	})
}

// TestLiveConsumers_ExcludesSimulatorRuntimeButNotGenuineOrphans is the
// regression test for the critical finding that `pgrep -f <udid>` always
// matches the booted device's own `launchd_sim` (its data-directory paths
// contain the UDID for as long as it's booted), so a naive MatchingPIDs
// check made `reap` refuse to recycle any booted slot, ever. A process
// named/pathed "launchd_sim" that references the token must be excluded; an
// unrelated process referencing the same token must not be.
func TestLiveConsumers_ExcludesSimulatorRuntimeButNotGenuineOrphans(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-udid-C9F1" + strconv.Itoa(os.Getpid())

	// Stands in for the booted device's launchd_sim: a process whose own
	// path ends in "launchd_sim" and whose argv references the UDID, the
	// same way launchd_sim's real bootstrap plist path does.
	simScript := writeScript(t, dir, "launchd_sim", `sleep 300`)
	simCmd := exec.Command(simScript, token)
	if err := simCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(simCmd.Process.Pid, syscall.SIGKILL) })

	// Stands in for a genuine orphan: MAV's `simctl spawn <udid> log
	// stream`, unrelated to the simulator's own process tree.
	orphanScript := writeScript(t, dir, "log_stream_consumer.sh", `sleep 300`)
	orphanCmd := exec.Command(orphanScript, token)
	if err := orphanCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(orphanCmd.Process.Pid, syscall.SIGKILL) })

	waitUntil(t, 3*time.Second, func() bool {
		matches, _ := MatchingPIDs(token)
		return len(matches) >= 2
	})

	live, err := LiveConsumers(token)
	if err != nil {
		t.Fatal(err)
	}
	var gotOrphan, gotSim bool
	for _, p := range live {
		if p == orphanCmd.Process.Pid {
			gotOrphan = true
		}
		if p == simCmd.Process.Pid {
			gotSim = true
		}
	}
	if !gotOrphan {
		t.Errorf("LiveConsumers should include the genuine orphan pid %d, got %v", orphanCmd.Process.Pid, live)
	}
	if gotSim {
		t.Errorf("LiveConsumers should exclude the launchd_sim-shaped pid %d (device's own runtime), got %v", simCmd.Process.Pid, live)
	}
}

// buildFakeSimpool compiles a trivial real binary named "simpool" that just
// sleeps. A shell script won't do here: when a script runs, ps reports its
// *interpreter* as argv[0] (e.g. "/bin/sh /path/to/simpool with ..."), which
// would make this test pass for the wrong reason — IsSimpoolHolder must
// recognize a real `simpool` binary's own argv[0], not "/bin/sh".
func buildFakeSimpool(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(5 * time.Minute) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "simpool")
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("building fake simpool binary: %v\n%s", err, out)
	}
	return bin
}

func TestIsSimpoolHolder(t *testing.T) {
	bin := buildFakeSimpool(t)

	cmd := exec.Command(bin, "with", "--device", "iPhone 17 Pro")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd.Process.Pid, syscall.SIGKILL) })
	pid := cmd.Process.Pid

	waitUntil(t, 3*time.Second, func() bool { return Alive(pid) })

	if !IsSimpoolHolder(pid, "with") {
		t.Errorf("IsSimpoolHolder(%d, \"with\") = false, want true (command line: %q)", pid, CommandLine(pid))
	}
	if IsSimpoolHolder(pid, "status") {
		t.Errorf("IsSimpoolHolder(%d, \"status\") = true, want false — process is running `with`, not `status`", pid)
	}
	if IsSimpoolHolder(pid+1_000_000, "with") {
		t.Errorf("IsSimpoolHolder should be false for a non-existent pid")
	}
}
