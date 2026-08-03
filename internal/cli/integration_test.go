package cli_test

// Integration tests that create and boot real simulators via `xcrun
// simctl`. Gated behind SIMPOOL_RUN_INTEGRATION=1 so a plain `go test
// ./...` stays fast; the design's core exclusivity/crash-recovery
// properties are covered process-agnostically in internal/pool's tests.
//
// Run with:
//   SIMPOOL_RUN_INTEGRATION=1 go test ./internal/cli/... -run Integration -v
//
// Every simulator these tests create is torn down in t.Cleanup regardless
// of pass/fail.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

const (
	testDevice = "iPhone 17 Pro"
	testOS     = "26.3"
)

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SIMPOOL_RUN_INTEGRATION") != "1" {
		t.Skip("set SIMPOOL_RUN_INTEGRATION=1 to run tests that create real simulators")
	}
}

// buildSimpool compiles the simpool binary once per test binary run.
func buildSimpool(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "simpool")
	// -buildvcs=false: `go build` otherwise shells out to `git status` to
	// stamp VCS info, which fails with "error obtaining VCS status: exit
	// status 128" when run from inside a git worktree (this repo's normal
	// workflow) instead of the primary checkout.
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "github.com/bitomule/simpool")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building simpool: %v\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/cli -> repo root
	return filepath.Join(wd, "..", "..")
}

// cleanupPool tears down every simulator (and any straggling process
// referencing one) found under a SIMPOOL_HOME used by a test, so no
// integration test ever leaks a simulator or an orphaned process. Every
// simulator these tests create lives in the default device set (design
// decision "opción (b)"), so this — like reap — only ever acts on a UDID
// after confirming its name is pool-owned (pool.IsPoolName): a test bug
// that somehow left meta.UDID pointing at an unrelated device must not be
// able to shut down or delete one of the user's own simulators.
func cleanupPool(t *testing.T, home string) {
	t.Helper()
	groups, err := pool.ListGroupDirs(home)
	if err != nil {
		return
	}
	for _, groupDir := range groups {
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			meta := pool.ReadMeta(dir)
			if meta.UDID == "" {
				continue
			}
			if pids, _ := procs.MatchingPIDs(meta.UDID); len(pids) > 0 {
				for _, p := range pids {
					_ = procs.KillProcessGroup(p, 9) // SIGKILL
				}
				time.Sleep(200 * time.Millisecond)
			}
			entry, found, err := simctl.Find(meta.UDID)
			if err != nil || !found || !pool.IsPoolName(entry.Name) {
				continue
			}
			_ = simctl.Shutdown(meta.UDID)
			// Deleting immediately after Shutdown races CoreSimulator's own
			// asynchronous teardown of the device's process tree: `delete`
			// yanking the device's data volume out from under a
			// still-tearing-down launchd_sim orphans its entire runtime
			// (AccessibilityUIServer, healthd, dozens of others) with no
			// parent left to reap them — reproduced directly on this
			// machine while validating this change, hundreds of leaked
			// 100%-CPU processes from a single premature delete. Poll for
			// the shutdown to actually land first; if it doesn't within the
			// timeout, still attempt Delete (best-effort test cleanup) but
			// this is the one place worth spending a few seconds to avoid
			// trashing the host running the test suite.
			waitForShutdown(meta.UDID, 10*time.Second)
			_ = simctl.Delete(meta.UDID)
		}
	}
}

// waitForShutdown polls until udid reports state "Shutdown" or timeout
// elapses. See cleanupPool for why this matters: `delete` right after
// `shutdown` can orphan the device's whole process tree.
func waitForShutdown(udid string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if state, found, err := simctl.State(udid); err != nil || !found || state == "Shutdown" {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestIntegration_WithHappyPathAndOrphanSweep exercises the primary `with`
// contract end to end against a real, booted simulator: the exported
// environment matches what MAV expects (§5), and — critically — any
// grandchild the launched command backgrounds and forgets about is still
// killed when `with` exits, because we always sweep the whole process
// group on the way out (§4).
func TestIntegration_WithHappyPathAndOrphanSweep(t *testing.T) {
	requireIntegration(t)
	bin := buildSimpool(t)
	home := t.TempDir()
	t.Cleanup(func() { cleanupPool(t, home) })
	env := append(os.Environ(), "SIMPOOL_HOME="+home)

	// --- happy path: env contract, run dir, device actually booted ---
	script := `echo "UDID=$SIMPOOL_UDID_0"; echo "NAME=$SIMPOOL_NAME_0"; ` +
		`echo "MAVUDID=$MAV_TARGET_UDID"; echo "MAVRUNTIME=$MAV_TARGET_RUNTIME"; ` +
		`echo "RUNDIR=$MAV_EXACT_RUN_DIR"`
	cmd := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "sh", "-c", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("simpool with (happy path) failed: %v\n%s", err, out)
	}
	vals := parseKV(t, string(out))
	if vals["UDID"] == "" || vals["UDID"] != vals["MAVUDID"] {
		t.Fatalf("SIMPOOL_UDID_0 (%q) and MAV_TARGET_UDID (%q) should match and be non-empty", vals["UDID"], vals["MAVUDID"])
	}
	if vals["MAVRUNTIME"] == "" {
		t.Fatal("MAV_TARGET_RUNTIME was empty")
	}
	if info, err := os.Stat(vals["RUNDIR"]); err != nil || !info.IsDir() {
		t.Fatalf("MAV_EXACT_RUN_DIR %q is not a directory: %v", vals["RUNDIR"], err)
	}
	// The whole point of option (b): a pooled UDID needs no --set to be
	// usable by a plain `xcrun simctl` call — the same one MAV, axe, and
	// idb already make.
	entry, found, err := simctl.Find(vals["UDID"])
	if err != nil || !found {
		t.Fatalf("device %s not found in the default device set after with: found=%v err=%v", vals["UDID"], found, err)
	}
	if !pool.IsPoolName(entry.Name) {
		t.Fatalf("device %s name %q does not look pool-owned (want prefix %q)", vals["UDID"], entry.Name, pool.NamePrefix)
	}
	if entry.State != "Booted" {
		t.Fatalf("device state = %q, want Booted (with should boot the device for its caller)", entry.State)
	}
	// SIMPOOL_NAME_0 must be the slot's actual simulator name, so a
	// name-based consumer (rules_apple's ios_xctestrun_runner) can be
	// pointed at this exact simulator instead of creating its own.
	if vals["NAME"] == "" || vals["NAME"] != entry.Name {
		t.Fatalf("SIMPOOL_NAME_0 (%q) should equal the slot's actual simulator name (%q)", vals["NAME"], entry.Name)
	}

	// The slot must be free again immediately after `with` returns.
	groupDir := pool.GroupDir(home, testDevice, testOS)
	free, err := pool.IsSlotFree(pool.SlotDir(groupDir, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("slot-0's lock is still held after `simpool with` exited")
	}

	// --- orphan sweep: a backgrounded grandchild must not survive `with` ---
	pidFile := filepath.Join(home, "orphan.pid")
	orphanScript := fmt.Sprintf(`sleep 60 & echo $! > %q; sleep 1`, pidFile)
	cmd2 := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "sh", "-c", orphanScript)
	cmd2.Env = env
	if out, err := cmd2.CombinedOutput(); err != nil {
		t.Fatalf("simpool with (orphan sweep) failed: %v\n%s", err, out)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading orphan pid file: %v", err)
	}
	var orphanPID int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pidBytes)), "%d", &orphanPID); err != nil {
		t.Fatalf("parsing orphan pid: %v", err)
	}
	if procs.Alive(orphanPID) {
		t.Fatalf("orphaned grandchild pid %d is still alive after `simpool with` exited — process group was not swept", orphanPID)
	}
}

// TestIntegration_ReapRecoversVerifiedOrphanAfterSimpoolIsKilled reproduces
// the specific failure window the design accepts as the price of the
// parent-holds-the-lock architecture (§4): SIGKILL to `simpool` itself
// (not its child) releases the flock immediately while the consumer (here
// standing in for MAV's `log stream`) keeps running.
//
// `simpool with` fingerprints its child's process start time (under a
// fixed, locale/timezone-independent environment — see
// procs.ProcessStartTime) right after launching it (see with.go), so a
// later `reap` (or the next acquisition) can prove the still-live process
// under the recorded pgid is genuinely the one it launched — not some
// unrelated process that has since reused the same numeric pgid — and
// safely kill it, then shut down the device for reuse.
func TestIntegration_ReapRecoversVerifiedOrphanAfterSimpoolIsKilled(t *testing.T) {
	requireIntegration(t)
	bin := buildSimpool(t)
	home := t.TempDir()
	t.Cleanup(func() { cleanupPool(t, home) })
	env := append(os.Environ(), "SIMPOOL_HOME="+home)

	// Provision + boot the device first via a throwaway `with`, so the
	// slot we fight over below already has a booted simulator and we
	// don't pay two boot costs.
	warm := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "true")
	warm.Env = env
	if out, err := warm.CombinedOutput(); err != nil {
		t.Fatalf("warm-up simpool with failed: %v\n%s", out, err)
	}

	groupDir := pool.GroupDir(home, testDevice, testOS)
	slotDir := pool.SlotDir(groupDir, 0)
	meta := pool.ReadMeta(slotDir)
	if meta.UDID == "" {
		t.Fatal("warm-up did not leave a provisioned UDID in slot-0's meta.json")
	}
	udid := meta.UDID

	// Launch `simpool with` running a `log stream`-shaped consumer that
	// references the UDID in its own argv after exec — exactly how MAV's
	// `xcrun simctl spawn <udid> log stream` looks to `pgrep -f <udid>`.
	// No `--set` needed: the pooled UDID lives in the default device set,
	// the same one a bare `xcrun simctl` always talks to.
	script := `exec xcrun simctl spawn "$SIMPOOL_UDID_0" log stream`
	victim := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "sh", "-c", script)
	victim.Env = env
	if err := victim.Start(); err != nil {
		t.Fatalf("starting victim simpool with: %v", err)
	}

	// Wait for the orphan-to-be to actually be running and referencing the
	// UDID before we pull the rug. Matching on "spawn" as well as the UDID
	// is deliberate: EnsureProvisioned's own (short-lived, expected)
	// `simctl boot <udid>` call also matches a plain UDID pgrep for a
	// second or so while the victim's `with` provisions its slot, and
	// racing that instead of the actual `simctl spawn ... log stream`
	// process makes this test kill the victim before its real child even
	// starts — a false pass, not a real exercise of the scenario.
	deadline := time.Now().Add(15 * time.Second)
	var livePIDs []int
	for time.Now().Before(deadline) {
		livePIDs = nil
		matches, _ := procs.MatchingPIDs(udid)
		for _, p := range matches {
			if strings.Contains(procs.CommandLine(p), "spawn") {
				livePIDs = append(livePIDs, p)
			}
		}
		if len(livePIDs) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(livePIDs) == 0 {
		_ = victim.Process.Kill()
		t.Fatal("log stream consumer never showed up as a live process referencing the UDID")
	}

	// SIGKILL simpool itself — not the group. This is the one window the
	// design's process-group sweep cannot cover (§4): the consumer keeps
	// running as a genuine orphan.
	if err := victim.Process.Kill(); err != nil {
		t.Fatalf("killing victim simpool: %v", err)
	}
	_ = victim.Wait()

	free, err := pool.IsSlotFree(slotDir)
	if err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Fatal("slot lock should be free immediately after simpool itself was SIGKILLed")
	}
	if live, _ := procs.MatchingPIDs(udid); len(live) == 0 {
		t.Fatal("consumer should still be alive after simpool (not its group) was killed — the scenario this test exists to reproduce didn't happen")
	}

	// Now the actual assertion: reap must see the lock is free, verify the
	// live process's identity against the fingerprint `with` recorded, and
	// reclaim it — kill the process group and shut down the device.
	reapCmd := exec.Command(bin, "reap", "--cold", "0")
	reapCmd.Env = env
	reapOut, err := reapCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("simpool reap failed: %v\n%s", err, reapOut)
	}
	if !strings.Contains(string(reapOut), "RECOVER") {
		t.Fatalf("reap should report RECOVER for the verified orphan, got:\n%s", reapOut)
	}

	for _, p := range livePIDs {
		if procs.Alive(p) {
			t.Fatalf("reap's recovery should have killed the verified orphan pid %d, but it is still alive", p)
		}
	}

	// AttemptRecovery's simctl.Shutdown call is asynchronous; give
	// CoreSimulator a moment to actually settle before asserting on state
	// (mirrors waitForShutdown's rationale elsewhere in this file).
	waitForShutdown(udid, 10*time.Second)
	state, found, err := simctl.State(udid)
	if err != nil || !found {
		t.Fatalf("device disappeared after reap: found=%v err=%v", found, err)
	}
	if state != "Shutdown" {
		t.Fatalf("reap's recovery should have shut down the device, got state=%q", state)
	}
}

// TestIntegration_ReapRefusesCrossSlotDevice is the regression test for the
// HIGH finding that reap checked only pool.IsPoolName(entry.Name) — the
// SIMPOOL_ prefix — before shutting down or deleting a simulator, never that
// the name matched the specific slot doing the reaping. A stale or corrupt
// meta.json (which the design doc explicitly tolerates, §6) could then make
// reap act on a *different* slot's simulator — reproduced directly during
// review by planting a slot-1 meta.json that pointed at slot-0's live,
// booted device: reap shut it down out from under its live holder, and
// with --purge would have deleted it. No boot needed here: the guard is a
// name comparison that must refuse before ever calling Shutdown/Delete, so
// paying for a boot would only slow the test down for no extra coverage.
func TestIntegration_ReapRefusesCrossSlotDevice(t *testing.T) {
	requireIntegration(t)
	bin := buildSimpool(t)
	home := t.TempDir()
	t.Cleanup(func() { cleanupPool(t, home) })

	runtimeID, deviceTypeID, err := simctl.ResolveRuntime(testDevice, testOS)
	if err != nil {
		t.Fatalf("resolving runtime: %v", err)
	}
	name := pool.DeviceName(home, testDevice, testOS, 0)
	udid, err := simctl.Create(name, deviceTypeID, runtimeID)
	if err != nil {
		t.Fatalf("creating slot-0's device: %v", err)
	}
	t.Cleanup(func() { _ = simctl.Delete(udid) })

	// Plant a stale meta.json in slot-1 pointing at slot-0's device, idle
	// and with the lock free so both --cold 0 and --purge would otherwise
	// fire immediately.
	groupDir := pool.GroupDir(home, testDevice, testOS)
	slot1Dir := pool.SlotDir(groupDir, 1)
	if err := os.MkdirAll(slot1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(slot1Dir, pool.Meta{
		Device:    testDevice,
		OSVersion: testOS,
		UDID:      udid,
		LastUsed:  time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	reapCmd := exec.Command(bin, "reap", "--cold", "0", "--purge", "1")
	reapCmd.Env = append(os.Environ(), "SIMPOOL_HOME="+home)
	out, err := reapCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("simpool reap failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "SKIP") {
		t.Fatalf("reap should SKIP a device whose name doesn't match this slot, got:\n%s", out)
	}
	if strings.Contains(string(out), "SHUT") || strings.Contains(string(out), "PURGE") {
		t.Fatalf("reap must never shut down or purge a cross-slot device, got:\n%s", out)
	}

	entry, found, err := simctl.Find(udid)
	if err != nil || !found {
		t.Fatalf("slot-0's device should still exist after reap misfired on slot-1: found=%v err=%v", found, err)
	}
	if entry.Name != name {
		t.Fatalf("slot-0's device was renamed/altered by reap: got %q, want %q", entry.Name, name)
	}
}

// TestIntegration_AcquireRecoveryRefusesCrossSlotShutdown is the mandatory
// regression test for the HIGH finding that poison.go's AttemptRecovery
// called simctl.Shutdown(meta.UDID) unconditionally, with no simctl.Find,
// pool.IsPoolName, or pool.DeviceName check anywhere in the file — reachable
// from take() (`with`/`acquire`) and claimSlotForLease (`lease`), not just
// reap's already-guarded RunReap path (see
// TestIntegration_ReapRefusesCrossSlotDevice above, which covers reap's own,
// separate guard but never boots a device, so it can't distinguish "never
// booted" from "correctly left alone").
//
// A stale or corrupt meta.json (tolerated by design, §6) whose UDID names a
// DIFFERENT slot's live, booted device, combined with a genuinely
// verifiable ConsumerPGID fingerprint for THIS slot, used to kill the
// verified orphan (correct) AND unconditionally shut down the other slot's
// simulator out from under whoever was using it (not correct — this is the
// exact scenario a stale UDID plus a real orphan produces). Reproduced here
// by planting slot-1's meta.json with a genuine, verified orphan pgid but
// slot-0's real, booted device UDID, then acquiring through the actual
// `with` binary (not just calling AttemptRecovery directly) so the whole
// take() -> AttemptRecovery -> EnsureProvisioned path is exercised.
func TestIntegration_AcquireRecoveryRefusesCrossSlotShutdown(t *testing.T) {
	requireIntegration(t)
	bin := buildSimpool(t)
	home := t.TempDir()
	t.Cleanup(func() { cleanupPool(t, home) })
	env := append(os.Environ(), "SIMPOOL_HOME="+home)

	// Slot-0's device: created and booted directly via simctl, never through
	// a slot-0 directory simpool itself knows about — standing in for
	// "another slot's live simulator, or one of a developer's own" that a
	// corrupt meta.json in a different slot happens to reference.
	runtimeID, deviceTypeID, err := simctl.ResolveRuntime(testDevice, testOS)
	if err != nil {
		t.Fatalf("resolving runtime: %v", err)
	}
	slot0Name := pool.DeviceName(home, testDevice, testOS, 0)
	slot0UDID, err := simctl.Create(slot0Name, deviceTypeID, runtimeID)
	if err != nil {
		t.Fatalf("creating slot-0's device: %v", err)
	}
	t.Cleanup(func() {
		_ = simctl.Shutdown(slot0UDID)
		waitForShutdown(slot0UDID, 10*time.Second)
		_ = simctl.Delete(slot0UDID)
	})
	if err := simctl.Boot(slot0UDID); err != nil {
		t.Fatalf("booting slot-0's device: %v", err)
	}

	// A real, verified orphan for slot-1: a process in its own group,
	// fingerprinted exactly the way `simpool with` fingerprints its own
	// child (see with.go) — genuinely killable, so recovery succeeds for the
	// right reason (the process, not the device check) and this test can
	// isolate the device-identity guard specifically.
	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	go func() { _ = orphan.Wait() }()
	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("fingerprinting orphan: %v", err)
	}

	groupDir := pool.GroupDir(home, testDevice, testOS)
	slot1Dir := pool.SlotDir(groupDir, 1)
	if err := os.MkdirAll(slot1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(slot1Dir, pool.Meta{
		Device:            testDevice,
		OSVersion:         testOS,
		UDID:              slot0UDID, // slot-0's device, not slot-1's — the corruption
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		LastUsed:          time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Only slot-1's directory exists, so `with --count 1` tries (and must
	// recover) exactly that slot.
	acquire := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "true")
	acquire.Env = env
	if out, err := acquire.CombinedOutput(); err != nil {
		t.Fatalf("simpool with failed: %v\n%s", err, out)
	}

	// The verified orphan must have been killed — recovery happened.
	deadline := time.Now().Add(2 * time.Second)
	for procs.PGIDAlive(pgid) {
		if time.Now().After(deadline) {
			t.Fatal("the verified orphan should have been killed by recovery")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Slot-0's device — cross-slot, only nominally referenced by slot-1's
	// corrupt meta.json — must be completely untouched: still booted, still
	// named exactly what it was created as.
	entry, found, err := simctl.Find(slot0UDID)
	if err != nil || !found {
		t.Fatalf("slot-0's device should still exist: found=%v err=%v", found, err)
	}
	if entry.Name != slot0Name {
		t.Fatalf("slot-0's device was renamed/altered: got %q, want %q", entry.Name, slot0Name)
	}
	if entry.State != "Booted" {
		t.Fatalf("recovering slot-1's orphan must never shut down slot-0's device — got state=%q, want Booted", entry.State)
	}
}

func parseKV(t *testing.T, out string) map[string]string {
	t.Helper()
	vals := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[k] = v
	}
	return vals
}
