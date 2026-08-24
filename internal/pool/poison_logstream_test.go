package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// --- The mixed orphan set a dead MAV session actually leaves behind ---
//
// Measured on 2026-08-24, on the one pool slot of eight that survived every
// reclaim path PR #5 added: iPhone-17-Pro@26.3/slot-0, flock free, no lease,
// meta.LastUsed five days old, device SIMPOOL_…_slot-0 verified Booted and
// name-matched to the slot. procs.LiveConsumers returned TWO pids, not one:
//
//	52024  simctl spawn <udid> log stream --style compact --level debug …   ppid 1
//	64003  idb_companion --udid <udid> --grpc-domain-sock /tmp/idb/…        ppid 1
//
// Both five days old, both orphaned to launchd, both left by the same dead
// `simpool with` consumer (a BoxySnapshotTests run whose meta.json is still
// on disk). CheckPoison returned PoisonedByLiveConsumers — instrumented
// against those exact pids, not inferred — because allReclaimableResidue
// requires EVERY live pid to be residue and the log stream was not yet a
// recognised class. Every other precondition PR #5 added was satisfied:
// residueSlotLongIdle=true, deviceBelongsToSlot=true. The companion branch
// was simply never reached.
//
// So the slot was unreclaimable in both directions: `reap` skipped it and
// `--disown-poisoned` refuses PoisonedByLiveConsumers outright. One of eight
// slots, lost permanently. These tests cover the fix and, more importantly,
// the shapes it must still refuse.

// buildFakeSimctlBinary compiles a real binary named "simctl" that can
// double-fork itself into a genuine ppid-1 orphan. Same rationale as
// buildFakeIdbCompanionBinary: `ps` reports a shell script's interpreter as
// argv[0], and both the binary name and the real reparenting are exactly what
// procs.IsOrphanedSimctlLogStreamFor checks, so neither may be faked.
func buildFakeSimctlBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main

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
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "simctl")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("building fake simctl binary: %v\n%s", err, out)
	}
	return bin
}

// spawnOrphanedLogStream starts `simctl spawn <udid> log stream …` and leaves
// it reparented to launchd, exactly like production pid 52024.
func spawnOrphanedLogStream(t *testing.T, udid string) (pid int, cleanup func()) {
	t.Helper()
	bin := buildFakeSimctlBinary(t)
	out, err := exec.Command(bin, "--launch", "spawn", udid, "log", "stream", "--style", "compact", "--level", "debug").Output()
	if err != nil {
		t.Fatalf("launching orphaned fake log stream: %v", err)
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("launcher did not print a pid: %q", out)
	}
	cleanup = func() { _ = syscall.Kill(pid, syscall.SIGKILL) }
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ppid, err := procs.ParentPID(pid); err == nil && ppid == 1 {
			return pid, cleanup
		}
		if time.Now().After(deadline) {
			cleanup()
			t.Fatal("fake log stream never reparented to launchd")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// spawnParentedLogStream starts the SAME argv as a direct child of this test
// process, so its parent is alive — a live MAV session's log tail, or a human
// watching a pool device's logs. This is the negative case the whole ppid
// condition exists for.
func spawnParentedLogStream(t *testing.T, udid string) (pid int, cleanup func()) {
	t.Helper()
	bin := buildFakeSimctlBinary(t)
	cmd := exec.Command(bin, "spawn", udid, "log", "stream", "--style", "compact", "--level", "debug")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, func() { _ = syscall.Kill(pid, syscall.SIGKILL) }
}

// TestCheckPoison_CompanionAndOrphanedLogStreamOnWarmSlot_Reclaimed is the
// primary regression test for the production slot described at the top of
// this file: a companion AND its orphaned log tail, together, on a warm
// long-idle slot whose device is confirmed this slot's own, must now be
// reclaimed — the whole set, in one pass.
//
// Before the fix this failed on its first assertion with
// PoisonedByLiveConsumers, exactly as the real binary did against pids 52024
// and 64003.
func TestCheckPoison_CompanionAndOrphanedLogStreamOnWarmSlot_Reclaimed(t *testing.T) {
	for _, mode := range []string{"lease", "acquire", "with", ""} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			udid := "simpool-test-mixed-residue-warm-" + mode
			companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
			defer cleanupCompanion()
			logPID, cleanupLog := spawnOrphanedLogStream(t, udid)
			defer cleanupLog()
			waitForConsumerCount(t, udid, 2)
			withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
			withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

			meta := longIdleMeta(udid, mode)
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByOrphanedResidue {
				t.Fatalf("mode=%q: a companion plus its orphaned log tail is residue, expected PoisonedByOrphanedResidue, got %v", mode, poison.Reason)
			}
			if poison.ResidueEvidence != ResidueSlotLongIdle {
				t.Fatalf("mode=%q: expected ResidueSlotLongIdle evidence (the device is up), got %v", mode, poison.ResidueEvidence)
			}
			if len(poison.ResiduePIDs) != 2 {
				t.Fatalf("mode=%q: expected both pids in ResiduePIDs, got %v", mode, poison.ResiduePIDs)
			}
			if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatalf("mode=%q: AttemptRecovery must reclaim a warm long-idle slot whose whole live set is verified residue", mode)
			}
			waitForDead(t, companionPID)
			waitForDead(t, logPID)
		})
	}
}

// TestCheckPoison_OrphanedLogStreamAloneOnWarmSlot_Reclaimed covers the same
// slot after its companion has already been reclaimed by an earlier pass (or
// never existed): the log tail on its own must not re-poison it.
func TestCheckPoison_OrphanedLogStreamAloneOnWarmSlot_Reclaimed(t *testing.T) {
	dir := t.TempDir()
	const udid = "simpool-test-logstream-alone-warm"
	logPID, cleanupLog := spawnOrphanedLogStream(t, udid)
	defer cleanupLog()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := longIdleMeta(udid, "with")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedResidue {
		t.Fatalf("expected PoisonedByOrphanedResidue for a lone orphaned log tail on a long-idle warm slot, got %v", poison.Reason)
	}
	if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must reclaim a warm long-idle slot whose only live consumer is an orphaned log tail")
	}
	waitForDead(t, logPID)
}

// TestCheckPoison_ParentedLogStreamAlongsideCompanion_NeverReclaimed is the
// safety bar. The argv is byte-identical to the reclaimed case above and
// every other condition is identical too — long-idle slot, device verified
// this slot's own, a genuine companion beside it. The ONLY difference is that
// this log stream's parent is still alive, which is what a live MAV session
// (or a human watching logs) looks like. Nothing may be signalled.
func TestCheckPoison_ParentedLogStreamAlongsideCompanion_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	const udid = "simpool-test-logstream-parented-warm"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, udid)
	defer cleanupCompanion()
	logPID, cleanupLog := spawnParentedLogStream(t, udid)
	defer cleanupLog()
	waitForConsumerCount(t, udid, 2)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := longIdleMeta(udid, "lease")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("a log stream with a live parent is not residue: expected PoisonedByLiveConsumers, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a slot whose log stream still has a live parent")
	}
	if syscall.Kill(companionPID, 0) != nil || syscall.Kill(logPID, 0) != nil {
		t.Fatal("both processes must still be alive")
	}
}

// TestCheckPoison_OrphanedLogStreamWithinGrace_NeverReclaimed proves the
// widened residue class did NOT widen the evidence bar with it: a slot used
// three minutes ago is a session that may merely be quiet, and its orphaned
// log tail stays untouched exactly like a companion's would.
func TestCheckPoison_OrphanedLogStreamWithinGrace_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	const udid = "simpool-test-logstream-within-grace"
	logPID, cleanupLog := spawnOrphanedLogStream(t, udid)
	defer cleanupLog()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))

	meta := Meta{UDID: udid, Mode: "lease", LastUsed: time.Now().Add(-3 * time.Minute)}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("within ResidueIdleGrace nothing is residue: expected PoisonedByLiveConsumers, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a slot used within ResidueIdleGrace")
	}
	if syscall.Kill(logPID, 0) != nil {
		t.Fatal("the log stream must still be alive")
	}
}

// TestCheckPoison_OrphanedLogStreamOnForeignDevice_NeverReclaimed proves the
// deviceBelongsToSlot identity guard still stands in front of the new class:
// a stale meta.UDID naming one of the developer's own booted simulators must
// not get that simulator's log tail killed just because this slot is idle.
func TestCheckPoison_OrphanedLogStreamOnForeignDevice_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	const udid = "simpool-test-logstream-foreign-device"
	logPID, cleanupLog := spawnOrphanedLogStream(t, udid)
	defer cleanupLog()
	waitForLiveConsumer(t, udid)
	withResidueDeviceList(t, fakeDeviceList(udid, "Booted"))
	withDeviceBelongsToSlotFind(t, fakeSlotForeignDevice("Booted"))

	meta := longIdleMeta(udid, "lease")
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedResidue {
		t.Fatalf("setup: expected PoisonedByOrphanedResidue, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must refuse once the device cannot be confirmed to be this slot's own")
	}
	if syscall.Kill(logPID, 0) != nil {
		t.Fatal("the log stream must still be alive")
	}
}
