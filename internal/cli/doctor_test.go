package cli

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
)

// TestRunDoctor_FlagsSlotWithLivePGIDEvenWithoutUDIDInArgv is the doctor-side
// counterpart to reap_test.go's
// TestReapSlot_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv: doctor.go
// duplicates the exact same poisoned-slot check (meta.ConsumerPGID via
// procs.PGIDAlive, falling back to procs.LiveConsumers's pgrep-based scan
// only when there's no recorded pgid) and must not silently diverge from
// it. A consumer that only ever receives its UDID by environment variable
// (MAV_TARGET_UDID, SIMPOOL_UDID_N — simpool's own handoff contract, design
// doc §5, and exactly how `mav run` receives it) must still be flagged by
// `simpool doctor`, not just missed.
//
// Like the reap test, the process below stands in for `simpool with`'s
// child: leader of its own process group (Setpgid, mirroring with.go),
// nothing in its command line a UDID-based pgrep could ever match.
func TestRunDoctor_FlagsSlotWithLivePGIDEvenWithoutUDIDInArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
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

	fakeUDID := "simpool-test-udid-not-in-any-argv"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:         fakeUDID,
		Mode:         "with",
		ConsumerPGID: pgid,
	}); err != nil {
		t.Fatal(err)
	}
	// Slot-0's lock starts free (never taken): simulates `simpool` itself
	// being SIGKILLed — the kernel released its flock immediately, but the
	// child it launched, recorded by pgid and not by anything pgrep-able,
	// is still running.

	// Sanity check that this really is invisible to the old mechanism.
	if live, _ := procs.LiveConsumers(fakeUDID); len(live) != 0 {
		t.Fatalf("test setup broken: fakeUDID must not be visible to a pgrep-based check, got live=%v", live)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should report a problem for a slot whose consumer is alive only via ConsumerPGID, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("still alive")) {
		t.Fatalf("doctor should flag the live-but-invisible consumer, got:\n%s", stdout.String())
	}
}

// TestRunDoctor_FlagsLiveLeaseWithBusyFlock proves doctor catches the one
// invariant violation that must never happen given the lease/flock
// exclusion design (see lease.go's AcquireLease and slot.go's take()): a
// slot whose flock is currently held while it also carries a live lease
// for some key. Either path alone is fine; both together means two
// consumers may be sharing one simulator.
func TestRunDoctor_FlagsLiveLeaseWithBusyFlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.TryLock(pool.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := pool.WriteLease(dir, pool.Lease{Key: "hot-repo", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should report a problem for a busy slot with a live lease, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("hot-repo")) {
		t.Fatalf("doctor should name the conflicting lease key, got:\n%s", stdout.String())
	}
}
