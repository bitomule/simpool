package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
)

// spawnUDIDCarryingProcess starts a real process whose OWN command line
// carries udid, so procs.LiveConsumers (pgrep -f) genuinely sees it, and
// waits until it does. Deliberately NOT one of the two reclaimable residue
// classes (idb_companion / orphaned `simctl spawn <udid> log stream`), so
// pool.CheckPoison reaches PoisonedByLiveConsumers — the never-a-kill-
// candidate reason both tests below are about. Mirrors
// availability_test.go's spawnResidueProcess in the pool package.
func spawnUDIDCarryingProcess(t *testing.T, udid string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "live_consumer.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, udid)
	// Its own process group, so no test in this package that kills a
	// process GROUP as part of a recovery can reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if live, _ := procs.LiveConsumers(udid); len(live) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the spawned process never became visible to procs.LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunPreboot_AtCapacityEnumeratesWhyEachSlotWasRefused is CORR-R2-1's
// regression test. The group's only slot has a FREE flock and no lease at
// all — it is refused because a live consumer quarantines it — so the old
// message ("all busy or already warm") asserted two things about it that
// were both false, and did so while exiting 0, i.e. while reporting
// success. pool.atCapacityError already enumerates the real per-slot
// reason; preboot must pass it through instead of glossing it.
//
// Pinned to the per-slot enumeration ("slot-0:" plus the state) AND to the
// absence of the old blanket claim: asserting only that the output changed
// would pass on any rewording.
func TestRunPreboot_AtCapacityEnumeratesWhyEachSlotWasRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	udid := "simpool-test-udid-preboot-capacity-enumeration"
	if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, Mode: "lease", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	spawnUDIDCarryingProcess(t, udid)

	var stdout, stderr bytes.Buffer
	code := RunPreboot([]string{"--device", "TestDevice", "--os", "1.0", "--max", "1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("preboot at capacity should exit 0 (it never blocks on capacity), got %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	if !bytes.Contains(stdout.Bytes(), []byte("nothing to do")) {
		t.Errorf("preboot must still say there is nothing to do, got:\n%s", out)
	}
	if bytes.Contains(stdout.Bytes(), []byte("all busy or already warm")) {
		t.Errorf("preboot must not assert the group is all busy or already warm about a slot that is neither, got:\n%s", out)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("slot-0: "+pool.SlotQuarantined.String())) {
		t.Errorf("preboot must pass through the per-slot refusal enumeration naming slot-0 as quarantined, got:\n%s", out)
	}
}

// TestRunDoctor_OwnLeaseResidueMatchesStatus is CORR-R2-2's regression
// test: the slot the OwnLeaseResidue exemption exists for — lease mode,
// lease.json already removed by `simpool release`, its own key's live
// residue still on the device — is the exact slot doctor used to
// contradict `status` about. status names the one key that can still claim
// it; doctor announced it would "be reclaimed automatically ... if its
// identity can still be verified", which is structurally unreachable
// (pool.AttemptRecovery declines every non-"with" slot before identity is
// ever considered) and omits the only fact that resolves the slot.
func TestRunDoctor_OwnLeaseResidueMatchesStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	const key = "hot-repo-lease-key"
	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	udid := "simpool-test-udid-doctor-own-lease-residue"
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:     udid,
		Mode:     "lease",
		LeaseKey: key,
		LastUsed: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	spawnUDIDCarryingProcess(t, udid)

	if av := pool.SlotAvailability(dir, ""); !av.OwnLeaseResidue {
		t.Fatalf("test setup broken: this slot must be the own-lease-residue case, got state=%v poison=%v", av.State, av.Poison)
	}

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should still flag a slot carrying live residue, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	doctorOut := stdout.String()
	if !bytes.Contains(stdout.Bytes(), []byte(key)) {
		t.Errorf("doctor must name the one lease key that can still claim this slot, got:\n%s", doctorOut)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("no reclaim is needed or will happen automatically")) {
		t.Errorf("doctor must say no automatic reclaim applies here, got:\n%s", doctorOut)
	}
	if bytes.Contains(stdout.Bytes(), []byte("will be reclaimed automatically")) {
		t.Errorf("doctor must not promise automatic reclaim for a lease-mode slot pool.AttemptRecovery refuses outright, got:\n%s", doctorOut)
	}

	var statusOut, statusErr bytes.Buffer
	RunStatus(nil, &statusOut, &statusErr)
	if !bytes.Contains(statusOut.Bytes(), []byte(key)) {
		t.Fatalf("test setup broken: status is supposed to name the key here:\n%s", statusOut.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("can still claim it")) {
		t.Errorf("doctor and status must agree on this slot; status says %q, doctor said:\n%s", statusOut.String(), doctorOut)
	}
}

// TestRunDoctor_NeverPromisesAutoReclaimForUnrecoverablePoison covers the
// rest of the arm CORR-R2-2 narrowed: a lease-mode slot with live
// consumers and NO owning key to hand it back to. pool.AttemptRecovery
// returns false for it at the Mode != "with" gate, so the old blanket
// "will be reclaimed automatically" promise was false here too.
func TestRunDoctor_NeverPromisesAutoReclaimForUnrecoverablePoison(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)

	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	udid := "simpool-test-udid-doctor-unrecoverable-live-consumer"
	if err := pool.WriteMeta(dir, pool.Meta{UDID: udid, Mode: "lease", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	spawnUDIDCarryingProcess(t, udid)

	var stdout, stderr bytes.Buffer
	code := RunDoctor(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doctor should flag a slot whose consumer is still alive, got exit 0:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("will be reclaimed automatically")) {
		t.Errorf("doctor must not promise automatic reclaim for a slot pool.AttemptRecovery refuses at the Mode != \"with\" gate, got:\n%s", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("nothing reclaims this automatically")) {
		t.Errorf("doctor should say plainly that nothing reclaims this slot, got:\n%s", stdout.String())
	}
}
