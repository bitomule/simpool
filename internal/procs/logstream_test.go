package procs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- Orphaned `simctl spawn <udid> log stream`: the second residue class ---
//
// The production state this covers, measured on 2026-08-24: pool slot
// iPhone-17-Pro@26.3/slot-0 held TWO orphans from the same dead MAV session,
// not one — the idb_companion everybody was looking at (pid 64003) AND its
// log tail (pid 52024, `simctl spawn <udid> log stream`), both reparented to
// launchd, both five days old. Because the companion branch requires the
// WHOLE live-consumer set to be verified residue, the log tail alone kept
// that slot poisoned permanently. See IsOrphanedSimctlLogStreamFor.

// fakeSimctlSource is a real binary named "simctl" with two modes.
//
// In "spawn" mode it just sleeps, standing in for the real
// `simctl spawn <udid> log stream`. argv0 has to be a genuine compiled binary
// named simctl for the same reason buildFakeIdbCompanion exists: `ps` reports
// a shell script's interpreter, not the script itself, as argv[0].
//
// In launcher mode ("--launch") it starts itself in spawn mode, prints the
// grandchild's pid and exits — which is what makes that grandchild a REAL
// orphan reparented to launchd, the exact condition
// IsOrphanedSimctlLogStreamFor tests for. Stubbing a ppid lookup instead
// would prove nothing about the real reparenting. The launcher takes the udid
// from the environment, never argv, so its own (short-lived) command line can
// never itself match a `pgrep -f <udid>`.
const fakeSimctlSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "spawn" {
		time.Sleep(5 * time.Minute)
		return
	}
	c := exec.Command(os.Args[0], "spawn", os.Getenv("FAKE_UDID"), "log", "stream", "--style", "compact", "--level", "debug")
	if err := c.Start(); err != nil {
		os.Exit(1)
	}
	fmt.Println(c.Process.Pid)
}
`

func buildFakeSimctl(t *testing.T) string {
	t.Helper()
	return buildFakeSimctlFrom(t, fakeSimctlSource)
}

func buildFakeSimctlFrom(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "simctl")
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, srcPath).CombinedOutput()
	if err != nil {
		t.Fatalf("building fake simctl binary: %v\n%s", err, out)
	}
	return bin
}

// spawnOrphanedFromLauncher runs bin's launcher mode and returns the pid of
// the grandchild it left behind, once that grandchild has actually been
// reparented to launchd.
func spawnOrphanedFromLauncher(t *testing.T, launcher *exec.Cmd) int {
	t.Helper()
	out, err := launcher.Output()
	if err != nil {
		t.Fatalf("launching orphaned fake simctl: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("launcher did not print a pid: %q", out)
	}
	t.Cleanup(func() { _ = Kill(pid, syscall.SIGKILL) })
	waitUntil(t, 5*time.Second, func() bool {
		ppid, err := ParentPID(pid)
		return err == nil && ppid == 1
	})
	if ppid, err := ParentPID(pid); err != nil || ppid != 1 {
		t.Fatalf("fake log stream never reparented to launchd (ppid=%d, err=%v)", ppid, err)
	}
	return pid
}

// spawnOrphanedSimctlLogStream reproduces the exact production shape: an
// orphaned `simctl spawn <udid> log stream …`.
func spawnOrphanedSimctlLogStream(t *testing.T, bin, udid string) int {
	t.Helper()
	launcher := exec.Command(bin, "--launch")
	launcher.Env = append(os.Environ(), "FAKE_UDID="+udid)
	return spawnOrphanedFromLauncher(t, launcher)
}

func TestIsOrphanedSimctlLogStreamFor(t *testing.T) {
	bin := buildFakeSimctl(t)
	const udid = "3A8339E4-TEST-UDID-0000-000000000000"
	pid := spawnOrphanedSimctlLogStream(t, bin, udid)

	if !IsOrphanedSimctlLogStreamFor(pid, udid) {
		t.Errorf("IsOrphanedSimctlLogStreamFor(%d, %q) = false, want true (command line: %q)", pid, udid, CommandLine(pid))
	}
	if IsOrphanedSimctlLogStreamFor(pid, "SOME-OTHER-UDID") {
		t.Error("must not match a different udid, even though this pid is genuinely an orphaned simctl log stream")
	}
	if IsOrphanedSimctlLogStreamFor(pid, "") {
		t.Error("an empty udid must never match anything")
	}
	if IsOrphanedSimctlLogStreamFor(pid+1_000_000, udid) {
		t.Error("should be false for a non-existent pid")
	}
}

// TestIsOrphanedSimctlLogStreamFor_LiveParentNeverMatches is the safety case
// this predicate exists to keep off the kill list: the SAME argv, run by a
// process that is still alive — a live MAV session's log tail, or a human
// watching a pool device's logs from their own shell. ppid is the only thing
// separating it from the orphan above, and it must be enough on its own.
func TestIsOrphanedSimctlLogStreamFor_LiveParentNeverMatches(t *testing.T) {
	bin := buildFakeSimctl(t)
	const udid = "3A8339E4-TEST-UDID-0000-000000000001"

	cmd := exec.Command(bin, "spawn", udid, "log", "stream", "--style", "compact")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd.Process.Pid, syscall.SIGKILL) })
	pid := cmd.Process.Pid
	waitUntil(t, 3*time.Second, func() bool { return Alive(pid) })

	if IsOrphanedSimctlLogStreamFor(pid, udid) {
		t.Fatalf("a log stream whose parent (this test, pid %d) is still alive must never be classified as orphaned residue (command line: %q)", os.Getpid(), CommandLine(pid))
	}
}

// TestIsOrphanedSimctlLogStreamFor_OnlyLogStreamMatches proves the argv shape
// is an allowlist, not a prefix match: `simctl spawn` can run arbitrary code
// inside the simulator, and only the read-only `log stream` invocation is in
// scope. Every process here is a genuine orphan (ppid 1), so the argv shape
// alone is what has to reject them.
func TestIsOrphanedSimctlLogStreamFor_OnlyLogStreamMatches(t *testing.T) {
	bin := buildFakeSimctlFrom(t, `package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	if os.Args[1] != "--launch" {
		time.Sleep(5 * time.Minute)
		return
	}
	c := exec.Command(os.Args[0], os.Args[2:]...)
	if err := c.Start(); err != nil {
		os.Exit(1)
	}
	fmt.Println(c.Process.Pid)
}
`)

	const udid = "3A8339E4-TEST-UDID-0000-000000000002"
	for _, argv := range [][]string{
		{"spawn", udid, "/bin/sh"},                       // arbitrary code inside the simulator
		{"spawn", "--standalone", udid, "log", "stream"}, // a flag between spawn and the udid
		{"spawn", udid, "log", "collect"},                // a different log subcommand
		{"launch", udid, "log", "stream"},                // a different simctl subcommand
		{"spawn", "OTHER-UDID", "log", "stream", udid},   // udid present, but not as the target
	} {
		pid := spawnOrphanedFromLauncher(t, exec.Command(bin, append([]string{"--launch"}, argv...)...))
		if IsOrphanedSimctlLogStreamFor(pid, udid) {
			t.Errorf("argv %v must never match: only an exact `simctl spawn <udid> log stream` does (command line: %q)", argv, CommandLine(pid))
		}
	}
}

// TestParentPID covers the plain contract: a live child reports this test
// process as its parent, and an unreadable pid is an error rather than a
// silently-plausible zero — IsOrphanedSimctlLogStreamFor reads an error as
// "cannot verify, not an orphan", so the distinction is load-bearing.
func TestParentPID(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd.Process.Pid, syscall.SIGKILL) })
	got, err := ParentPID(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ParentPID: %v", err)
	}
	if got != os.Getpid() {
		t.Errorf("ParentPID = %d, want this test process %d", got, os.Getpid())
	}
	if _, err := ParentPID(0); err == nil {
		t.Error("ParentPID(0) must be an error, never a plausible-looking value")
	}
	if _, err := ParentPID(cmd.Process.Pid + 1_000_000); err == nil {
		t.Error("ParentPID of a non-existent pid must be an error")
	}
}
