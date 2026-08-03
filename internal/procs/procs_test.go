package procs

import (
	"bufio"
	"fmt"
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

// TestPGIDAlive is the unit-level regression test for the CRITICAL finding
// that the poisoned-slot check relied entirely on argv (MatchingPIDs/
// LiveConsumers), which cannot see a consumer that only ever receives its
// UDID by environment variable — simpool's own handoff contract (design
// doc §5). PGIDAlive replaces that: it inspects process-group membership
// via kill(-pgid, 0), not command-line text, so it is correct whether or
// not the UDID appears anywhere in argv.
func TestPGIDAlive(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	// Reap it the instant it exits (by any means): this test binary is its
	// real parent, and an un-Wait()'d zombie still answers kill(-pgid, 0)
	// with EPERM — not ESRCH — on Darwin, which PGIDAlive now correctly (see
	// its doc comment: a non-ESRCH error must fail toward "still alive")
	// reports as true until the zombie is actually collected. Not an issue
	// in real usage — an orphan's original parent (`simpool with`) is
	// already dead by the time recovery runs, so the kernel reparents it to
	// launchd, which reaps it immediately once killed.
	go func() { _ = cmd.Wait() }()

	waitUntil(t, 3*time.Second, func() bool { return PGIDAlive(pgid) })

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, func() bool { return !PGIDAlive(pgid) })

	if PGIDAlive(0) {
		t.Error("PGIDAlive(0) should be false")
	}
	if PGIDAlive(-1) {
		t.Error("PGIDAlive(-1) should be false")
	}
}

// TestPGIDAlive_TreatsNonESRCHErrorAsAlive is the regression test for the
// HIGH finding that a kill(-pgid, 0) failure other than ESRCH (most
// notably EPERM: the process group exists but the caller lacks permission
// to signal it — reproducible without any special setup, see the
// unreaped-zombie case TestPGIDAlive's own Wait() goroutine works around)
// used to be read as "not alive", handing a poisoned slot's flock check
// the exact wrong answer: a check that cannot positively confirm a group
// is gone must fail toward "still alive", never toward "confirmed free".
// killFunc is overridden here for determinism — EPERM from a real
// permission conflict requires a process owned by a different user, which
// a test environment cannot reliably arrange on demand.
func TestPGIDAlive_TreatsNonESRCHErrorAsAlive(t *testing.T) {
	orig := killFunc
	defer func() { killFunc = orig }()

	killFunc = func(pid int, sig syscall.Signal) error { return syscall.EPERM }
	if !PGIDAlive(42) {
		t.Error("PGIDAlive should treat EPERM as still alive, not as gone")
	}

	killFunc = func(pid int, sig syscall.Signal) error { return syscall.EINVAL }
	if !PGIDAlive(42) {
		t.Error("PGIDAlive should treat an unexpected errno as still alive, not as gone")
	}

	killFunc = func(pid int, sig syscall.Signal) error { return syscall.ESRCH }
	if PGIDAlive(42) {
		t.Error("PGIDAlive should still treat ESRCH (genuinely gone) as not alive")
	}
}

// TestMatchingPIDs_PropagatesCheckFailure and
// TestLiveConsumers_PropagatesCheckFailure are the regression tests for the
// HIGH finding that CheckPoison used to discard MatchingPIDs/LiveConsumers'
// error entirely (`live, _ := procs.LiveConsumers(...)`), so a `pgrep` that
// failed to even run — reproduced at load 239 during review — was read as
// "no live consumer", the exact wrong direction: a failed check must be
// treated as "busy, don't touch", never as "confirmed free". pgrepFunc is
// overridden here because reliably forcing `pgrep` itself to fail (as
// opposed to "ran and found nothing") is not something a test can arrange
// on demand.
func TestMatchingPIDs_PropagatesCheckFailure(t *testing.T) {
	orig := pgrepFunc
	defer func() { pgrepFunc = orig }()

	wantErr := fmt.Errorf("fork: resource temporarily unavailable")
	pgrepFunc = func(needle string) ([]byte, error) { return nil, wantErr }

	_, err := MatchingPIDs("anything")
	if err == nil {
		t.Fatal("MatchingPIDs should propagate a pgrep execution failure, not swallow it")
	}
}

func TestLiveConsumers_PropagatesCheckFailure(t *testing.T) {
	orig := pgrepFunc
	defer func() { pgrepFunc = orig }()

	calls := 0
	pgrepFunc = func(needle string) ([]byte, error) {
		calls++
		if calls == 1 {
			// The udid lookup itself finds a match...
			return []byte("12345\n"), nil
		}
		// ...but the second pgrep (excluding launchd_sim's own tree) fails.
		return nil, fmt.Errorf("fork: resource temporarily unavailable")
	}

	_, err := LiveConsumers("some-udid")
	if err == nil {
		t.Fatal("LiveConsumers should propagate a pgrep execution failure, not swallow it")
	}
}

// TestDescendants_SingleSnapshotCall proves the fix for the HIGH finding
// that checking a single booted simulator's live consumers cost ~8-12s wall
// time — Descendants used to recurse via one `pgrep -P` fork per process in
// the subtree (281 direct children of launchd_sim on an iOS 26.3
// simulator). processSnapshotFunc is overridden with a synthetic
// multi-level tree and an invocation counter: the whole traversal — no
// matter how deep or wide the tree — must cost exactly one call.
func TestDescendants_SingleSnapshotCall(t *testing.T) {
	orig := processSnapshotFunc
	defer func() { processSnapshotFunc = orig }()

	calls := 0
	processSnapshotFunc = func() ([]byte, error) {
		calls++
		// 1 -> 2,3 ; 2 -> 4,5 ; 4 -> 6 ; plus unrelated noise (100 -> 101).
		return []byte("2 1\n3 1\n4 2\n5 2\n6 4\n101 100\n"), nil
	}

	got := Descendants(1)
	calls1 := calls
	if calls1 != 1 {
		t.Fatalf("Descendants should fetch the process snapshot exactly once, got %d calls", calls1)
	}

	want := map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true}
	if len(got) != len(want) {
		t.Fatalf("Descendants(1) = %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("Descendants(1) included unexpected pid %d", p)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Errorf("Descendants(1) missing pids %v", want)
	}
}

// TestDescendants_CyclicSnapshotDoesNotOverflow is the regression test for
// the finding that Descendants' walk had no visited-set: a well-formed
// process tree can never contain a cycle (nothing is its own ancestor), but
// this snapshot comes from parsing `ps` output, not from a source that
// enforces that invariant — a malformed or racily-read snapshot must not be
// able to turn a bounded exclusion-set walk into unbounded recursion.
// Reproduced directly with the exact two-line snapshot that crashed the
// process with an uncatchable stack overflow before the seen-map guard
// existed: "10 20\n20 10\n" (pid 10's parent is 20, and pid 20's parent is
// 10). Without the guard, `go test -run TestDescendants_CyclicSnapshotDoesNotOverflow`
// itself never returns — it crashes the whole test binary.
func TestDescendants_CyclicSnapshotDoesNotOverflow(t *testing.T) {
	orig := processSnapshotFunc
	defer func() { processSnapshotFunc = orig }()

	processSnapshotFunc = func() ([]byte, error) {
		return []byte("10 20\n20 10\n"), nil
	}

	done := make(chan []int, 1)
	go func() { done <- Descendants(10) }()

	select {
	case got := <-done:
		// tree[10] = [20], tree[20] = [10]; walking from 10 visits 20 once,
		// then refuses to revisit 10 (already seen) — exactly one descendant.
		if len(got) != 1 || got[0] != 20 {
			t.Fatalf("Descendants(10) on a 2-cycle = %v, want exactly [20]", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Descendants did not return within 3s on a cyclic snapshot — the visited-set guard regressed")
	}
}

// TestProcessStartTime_StableAcrossAmbientLocaleAndTZ is the regression
// test for the CRITICAL finding: an earlier, reverted version of this
// feature fingerprinted a process via a rendering (`sysctl kern.boottime`'s
// full line) that is sensitive to the invoking process's ambient TZ and
// locale, so the exact same instant produced different strings depending
// on what happened to be set — and the design's one destructive branch
// fired on any mismatch. `ps -o lstart=` (used here) has the identical
// weakness on its own; ProcessStartTime must force a fixed environment so
// its output is stable regardless of the ambient TZ/LC_ALL/LANG at either
// the recording or the verifying end.
func TestProcessStartTime_StableAcrossAmbientLocaleAndTZ(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd.Process.Pid, syscall.SIGKILL) })
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid

	waitUntil(t, 3*time.Second, func() bool { return Alive(pid) })

	envs := []struct{ tz, lc string }{
		{"UTC", "C"},
		{"Asia/Tokyo", "C"},
		{"America/Argentina/Buenos_Aires", "es_ES.UTF-8"},
	}
	var first string
	for i, e := range envs {
		t.Setenv("TZ", e.tz)
		t.Setenv("LC_ALL", e.lc)
		got, err := ProcessStartTime(pid)
		if err != nil {
			t.Fatalf("ProcessStartTime under TZ=%s LC_ALL=%s: %v", e.tz, e.lc, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("ProcessStartTime is sensitive to the calling process's ambient TZ/LC_ALL: TZ=%s LC_ALL=%s got %q, want %q (same instant as the first call)", e.tz, e.lc, got, first)
		}
	}
}

// TestProcessStartTime_IgnoresAmbientPATH is the regression test for the
// finding that ProcessStartTime set PATH=/bin:/usr/bin in cmd.Env but still
// invoked the bare name "ps" — exec.Command resolves a bare name against
// the *calling* process's own ambient $PATH (via exec.LookPath) before
// cmd.Env is ever consulted, so cmd.Env's PATH only ever hardens what the
// child sees, never which binary actually gets resolved and executed. A
// malicious or merely careless ambient PATH with an earlier "ps" could
// substitute a fake binary that fabricates whatever start-time string it
// likes, defeating the exact fingerprint this function exists to produce
// trustworthy output for. Reproduced directly here: a fake "ps" placed
// first on the test's own ambient PATH always prints a fixed, wrong lstart
// string regardless of which pid it's asked about — if ProcessStartTime
// ever resolved against ambient PATH instead of the hardcoded /bin/ps, this
// test would observe that wrong string instead of the real one.
func TestProcessStartTime_IgnoresAmbientPATH(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(cmd.Process.Pid, syscall.SIGKILL) })
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	waitUntil(t, 3*time.Second, func() bool { return Alive(pid) })

	want, err := ProcessStartTime(pid)
	if err != nil {
		t.Fatalf("ProcessStartTime under the real PATH: %v", err)
	}

	fakeDir := t.TempDir()
	fakePS := writeScript(t, fakeDir, "ps", `echo "Thu Jan  1 00:00:00 1970"`)
	if err := os.Chmod(fakePS, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Sanity check the fake actually shadows "ps" for a bare exec.Command
	// lookup — otherwise this test would pass for the wrong reason.
	if out, err := exec.Command("ps").Output(); err != nil || strings.TrimSpace(string(out)) != "Thu Jan  1 00:00:00 1970" {
		t.Fatalf("test setup broken: fake ps did not shadow the real one on PATH, got %q err=%v", out, err)
	}

	got, err := ProcessStartTime(pid)
	if err != nil {
		t.Fatalf("ProcessStartTime with a fake ps shadowing PATH: %v", err)
	}
	if got != want {
		t.Fatalf("ProcessStartTime resolved against ambient PATH instead of /bin/ps: got %q, want %q (the real /bin/ps output)", got, want)
	}
}

func TestProcessStartTime_ErrorsForInvalidOrDeadPid(t *testing.T) {
	if _, err := ProcessStartTime(0); err == nil {
		t.Error("ProcessStartTime(0) should error")
	}
	if _, err := ProcessStartTime(-1); err == nil {
		t.Error("ProcessStartTime(-1) should error")
	}
	// A pid extremely unlikely to be in use.
	if _, err := ProcessStartTime(1_999_999); err == nil {
		t.Error("ProcessStartTime should error for a pid that doesn't exist")
	}
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
