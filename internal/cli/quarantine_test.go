package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// quarantinedPool builds slot 0 used a second ago, its device in state, and
// quarantined by a live `with` process group whose identity was never
// fingerprinted, so no recovery path may kill it.
func quarantinedPool(t *testing.T, state string) (f *idleFakes, root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = pool.SlotDir(pool.GroupDir(root, "TestDevice", "1.0"), 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	consumer := exec.Command("sleep", "300")
	consumer.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := consumer.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := consumer.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = consumer.Wait()
	})
	udid := "quarantine-udid-a"
	if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, Mode: "with", ConsumerPGID: pgid, LastUsed: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if !pool.CheckPoison(pool.ReadMeta(dir)).Poisoned() {
		t.Fatal("fixture slot is not quarantined")
	}
	f = installIdleFakes(t, root, "TestDevice@1.0", map[string]int{udid: 0}, map[string]string{udid: state})
	return f, root, dir
}

func reapQuarantined(root, dir string, dryRun bool) (string, string) {
	var stdout, stderr bytes.Buffer
	reapSlot(root, dir, 0, 5*time.Minute, 0, time.Hour, 3*time.Minute, dryRun, false, 0, nil, false, &stdout, &stderr)
	return stdout.String(), stderr.String()
}

func TestReapSlot_ShutsDownABootedQuarantinedSlotAtOnce(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")

	out, errOut := reapQuarantined(root, dir, false)

	if len(f.shut) != 1 || f.shut[0] != "quarantine-udid-a" {
		t.Fatalf("a booted quarantined slot must not survive a reap, used 1s ago or not; shut=%v\nstdout:\n%s\nstderr:\n%s", f.shut, out, errOut)
	}
	if !strings.Contains(out, "SHUT  TestDevice@1.0/slot-0  quarantined") {
		t.Fatalf("want a SHUT line naming the quarantine, got:\n%s", out)
	}
	if !f.markerOn[0] {
		t.Fatal("the shutdown ran without the marker that tells a lease to wait for it")
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a quarantine shutdown must never delete, deleted=%v", f.deleted)
	}
}

func TestReapSlot_RetriesAFailedQuarantineShutdownOnTheNextPass(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")
	recorded := shutdownIdle
	attempts := 0
	shutdownIdle = func(udid string) error {
		attempts++
		if attempts == 1 {
			return errors.New("simctl timed out")
		}
		return recorded(udid)
	}

	_, errOut := reapQuarantined(root, dir, false)
	if !strings.Contains(errOut, "could not shut down quarantined device quarantine-udid-a: simctl timed out — the next reap retries") {
		t.Fatalf("a failed shutdown must be logged, stderr:\n%s", errOut)
	}
	if len(f.shut) != 0 {
		t.Fatalf("the first attempt failed, shut=%v", f.shut)
	}

	reapQuarantined(root, dir, false)
	if len(f.shut) != 1 {
		t.Fatalf("the next reap must try again, attempts=%d shut=%v", attempts, f.shut)
	}
}

func TestReapSlot_LeavesAShutDownQuarantinedSlotAlone(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Shutdown")

	out, _ := reapQuarantined(root, dir, false)

	if len(f.shut) != 0 {
		t.Fatalf("nothing to shut down, shut=%v", f.shut)
	}
	if !strings.Contains(out, "SKIP") {
		t.Fatalf("want the usual quarantine SKIP, got:\n%s", out)
	}
}

func TestReapSlot_NeverShutsDownADeviceThatIsNotTheQuarantinedSlotsOwn(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")
	meta := pool.ReadMeta(dir)
	meta.UDID = "someone-elses-udid"
	if err := pool.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	fakeDeviceStates(t, root, "TestDevice@1.0", map[string]int{"someone-elses-udid": 1}, map[string]string{"someone-elses-udid": "Booted"})

	reapQuarantined(root, dir, false)

	if len(f.shut) != 0 {
		t.Fatalf("a device named for another slot must never be shut down, shut=%v", f.shut)
	}
}

func TestReapSlot_DryRunOnlyReportsTheQuarantineShutdown(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")

	out, _ := reapQuarantined(root, dir, true)

	if len(f.shut) != 0 {
		t.Fatalf("dry-run must not shut anything down, shut=%v", f.shut)
	}
	if !strings.Contains(out, "SHUT  TestDevice@1.0/slot-0  quarantined") {
		t.Fatalf("dry-run must say what it would shut down, got:\n%s", out)
	}
}

func TestShutdownReleasedQuarantine_ShutsDownASlotStillQuarantinedAfterRelease(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")
	var stdout, stderr bytes.Buffer

	if !shutdownReleasedQuarantine(root, dir, &stdout, &stderr) {
		t.Fatalf("want a shutdown, stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if len(f.shut) != 1 || f.shut[0] != "quarantine-udid-a" {
		t.Fatalf("shut=%v", f.shut)
	}
}

func TestShutdownReleasedQuarantine_LeavesARenewedLeaseAlone(t *testing.T) {
	f, root, dir := quarantinedPool(t, "Booted")
	if err := pool.WriteLease(dir, pool.Lease{Key: "k", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer

	if shutdownReleasedQuarantine(root, dir, &stdout, &stderr) || len(f.shut) != 0 {
		t.Fatalf("a slot leased again must be left to its new holder, shut=%v", f.shut)
	}
}

func TestReapSlot_IdleClockStopsJustShortOfColdAndShutsAtIt(t *testing.T) {
	root := t.TempDir()
	f, groupDir := idlePool(t, root, 0)
	dir := pool.SlotDir(groupDir, 0)
	used := pool.ReadMeta(dir).LastUsed
	orig := reapNow
	t.Cleanup(func() { reapNow = orig })

	reapNow = func() time.Time { return used.Add(5*time.Minute - time.Second) }
	if out, _ := reapIdle(root, dir, 0, 5*time.Minute, false); len(f.shut) != 0 || !strings.Contains(out, "KEEP") {
		t.Fatalf("4m59s after its last use the slot stays booted, shut=%v\n%s", f.shut, out)
	}

	reapNow = func() time.Time { return used.Add(5 * time.Minute) }
	if out, _ := reapIdle(root, dir, 0, 5*time.Minute, false); len(f.shut) != 1 {
		t.Fatalf("5m00s after its last use the slot is shut down, shut=%v\n%s", f.shut, out)
	}
}
