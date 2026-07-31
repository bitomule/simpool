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
	cmd := exec.Command("go", "build", "-o", bin, "github.com/bitomule/simpool")
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
// integration test ever leaks a simulator or an orphaned process.
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
			setDir := pool.SetDirFor(dir)
			_ = simctl.Shutdown(setDir, meta.UDID)
			_ = simctl.Delete(setDir, meta.UDID)
		}
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
	script := `echo "UDID=$SIMPOOL_UDID_0"; echo "SET=$SIMPOOL_DEVICE_SET_0"; ` +
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
	if info, err := os.Stat(vals["SET"]); err != nil || !info.IsDir() {
		t.Fatalf("SIMPOOL_DEVICE_SET_0 %q is not a directory: %v", vals["SET"], err)
	}
	if info, err := os.Stat(vals["RUNDIR"]); err != nil || !info.IsDir() {
		t.Fatalf("MAV_EXACT_RUN_DIR %q is not a directory: %v", vals["RUNDIR"], err)
	}
	state, found, err := simctl.State(vals["SET"], vals["UDID"])
	if err != nil || !found {
		t.Fatalf("device %s not found in its own set after with: found=%v err=%v", vals["UDID"], found, err)
	}
	if state != "Booted" {
		t.Fatalf("device state = %q, want Booted (with should boot the device for its caller)", state)
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

// TestIntegration_ReapProtectsLiveOrphanAfterSimpoolIsKilled reproduces the
// specific failure window the design accepts as the price of the
// parent-holds-the-lock architecture (§4): SIGKILL to `simpool` itself
// (not its child) releases the flock immediately while the consumer
// (here standing in for MAV's `log stream`) keeps running. `reap` must
// detect the still-live process referencing the device and refuse to
// recycle the slot.
func TestIntegration_ReapProtectsLiveOrphanAfterSimpoolIsKilled(t *testing.T) {
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
	udid, setDir := meta.UDID, pool.SetDirFor(slotDir)

	// Launch `simpool with` running a `log stream`-shaped consumer that
	// references the UDID in its own argv after exec — exactly how MAV's
	// `xcrun simctl spawn <udid> log stream` looks to `pgrep -f <udid>`.
	script := `exec xcrun simctl --set "$SIMPOOL_DEVICE_SET_0" spawn "$SIMPOOL_UDID_0" log stream`
	victim := exec.Command(bin, "with", "--device", testDevice, "--os", testOS, "--count", "1", "--", "sh", "-c", script)
	victim.Env = env
	if err := victim.Start(); err != nil {
		t.Fatalf("starting victim simpool with: %v", err)
	}

	// Wait for the orphan-to-be to actually be running and referencing the
	// UDID before we pull the rug.
	deadline := time.Now().Add(15 * time.Second)
	var livePIDs []int
	for time.Now().Before(deadline) {
		livePIDs, _ = procs.MatchingPIDs(udid)
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

	// Now the actual assertion: reap must see the lock is free AND the
	// device still has a live process attached, and must refuse to touch
	// it.
	reapCmd := exec.Command(bin, "reap", "--cold", "0")
	reapCmd.Env = env
	reapOut, err := reapCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("simpool reap failed: %v\n%s", err, reapOut)
	}
	if !strings.Contains(string(reapOut), "SKIP") {
		t.Fatalf("reap should report SKIP for the slot with a live orphan, got:\n%s", reapOut)
	}

	state, found, err := simctl.State(setDir, udid)
	if err != nil || !found {
		t.Fatalf("device disappeared after reap: found=%v err=%v", found, err)
	}
	if state != "Booted" {
		t.Fatalf("reap shut down a device that still had a live process attached to it: state=%q", state)
	}

	// Clean up the orphan ourselves; t.Cleanup will shut down/delete the
	// device afterward.
	for _, p := range livePIDs {
		_ = procs.KillProcessGroup(p, 9)
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
