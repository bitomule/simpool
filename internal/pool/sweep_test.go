package pool

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// TestExitSweepGroupDir_RecoversVerifiedOrphan proves the exit sweep
// (wired into `simpool with`'s and `simpool release`'s exit paths) reclaims
// a verified orphan just like an on-demand acquisition would — no real
// simulator needed since meta.UDID is never a real device here and
// AttemptRecovery's simctl.Shutdown call is best-effort.
func TestExitSweepGroupDir_RecoversVerifiedOrphan(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	orphan := exec.Command("sleep", "300")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := orphan.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	go func() { _ = orphan.Wait() }() // see spawnRealOrphan's comment in poison_test.go

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := procs.MachineBootTime()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{
		UDID:              "simpool-test-udid-sweep-recover",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		ConsumerBootID:    bootID,
		LastUsed:          time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	ExitSweepGroupDir(root, groupDir)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if syscall.Kill(pgid, 0) != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ExitSweep should have recovered (killed) the verified orphan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := ReadMeta(dir).ConsumerPGID; got != 0 {
		t.Errorf("ConsumerPGID should be cleared after sweep recovery, got %d", got)
	}
}

// TestExitSweepGroupDir_SkipsActiveLease proves the sweep never touches a
// slot currently reserved by a live `simpool lease` — a lease deliberately
// never holds the flock, so without this check the sweep would treat an
// actively hot-looped MAV slot exactly like any other idle free slot.
func TestExitSweepGroupDir_SkipsActiveLease(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{UDID: "simpool-test-udid-leased", LastUsed: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLease(dir, Lease{Key: "hot-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	ExitSweepGroupDir(root, groupDir)

	if lease := ReadLease(dir); lease.Key != "hot-repo" {
		t.Fatalf("active lease must survive ExitSweep untouched, got %+v", lease)
	}
}

// TestExitSweepGroupDir_IgnoresNeverProvisionedSlot is a defensive test
// against a panic/crash on the (never-provisioned, UDID == "") free-slot
// path, which involves no simctl call at all.
func TestExitSweepGroupDir_IgnoresNeverProvisionedSlot(t *testing.T) {
	root := t.TempDir()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ExitSweepGroupDir(root, groupDir) // must not panic
}
