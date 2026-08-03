package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
)

// spawnRealOrphan starts a real, separate OS process in its own process
// group (mirroring with.go's Setpgid child) to stand in for a `simpool
// with` consumer that has survived a SIGKILL to `simpool` itself.
//
// A background Wait() reaps it the instant it exits (by any means,
// including SIGKILL from AttemptRecovery): without this, the test binary —
// its real parent here — never collects it, and a zombie process still
// answers kill(pid, 0)/kill(-pgid, 0) as "alive" on Darwin, which would
// make a successful recovery look like a failed one. In real usage this
// isn't an issue: `simpool with` itself is already dead by the time this
// scenario happens, so the orphan reparents to launchd, which reaps it
// immediately once killed.
func spawnRealOrphan(t *testing.T) (pgid int, cleanup func()) {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }
}

// spawnLiveConsumerWithToken starts a real process whose OWN command line
// (not a descendant's) carries token — mirroring the pattern used
// elsewhere in this package (see slot_test.go) for a process
// `pgrep -f <udid>` can see, standing in for a legitimate axe/simctl/mav
// session against a leased simulator.
func spawnLiveConsumerWithToken(t *testing.T, token string) (pid int, cleanup func()) {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "live_consumer.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scriptPath, token)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	return pid, func() { _ = syscall.Kill(pid, syscall.SIGKILL) }
}

func waitForDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return // ESRCH
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still alive after the deadline", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAttemptRecovery_SuccessfulReclaim proves the primary contract: a
// poisoned `with` slot whose recorded fingerprint (process start time +
// machine boot) matches the still-alive process under ConsumerPGID is
// killed and the slot is reclaimed.
func TestAttemptRecovery_SuccessfulReclaim(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("ProcessStartTime: %v", err)
	}
	bootID, err := procs.MachineBootTime()
	if err != nil {
		t.Fatalf("MachineBootTime: %v", err)
	}
	meta := Meta{
		UDID:              "simpool-test-udid-reclaim",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		ConsumerBootID:    bootID,
	}

	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByConsumerPGID {
		t.Fatalf("expected PoisonedByConsumerPGID, got %v", poison.Reason)
	}

	if !AttemptRecovery(dir, &meta, poison) {
		t.Fatal("AttemptRecovery should have succeeded with a correct fingerprint")
	}
	if meta.ConsumerPGID != 0 || meta.ConsumerStartedAt != "" || meta.ConsumerBootID != "" {
		t.Errorf("consumer identity should be fully cleared, got %+v", meta)
	}

	waitForDead(t, pgid)

	persisted := ReadMeta(dir)
	if persisted.ConsumerPGID != 0 {
		t.Errorf("recovery should have persisted the cleared ConsumerPGID, got %+v", persisted)
	}
}

// TestAttemptRecovery_RefusesOnRecycledPidFingerprint is the pid-recycling
// defense: macOS recycles pids, so a live process under the recorded pgid
// is not by itself proof it's the same process — only an exact match on
// the recorded start time is. A deliberately wrong recorded start time must
// never be killed.
func TestAttemptRecovery_RefusesOnRecycledPidFingerprint(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	bootID, err := procs.MachineBootTime()
	if err != nil {
		t.Fatalf("MachineBootTime: %v", err)
	}
	meta := Meta{
		UDID:              "simpool-test-udid-recycled",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: "Mon Jan  1 00:00:00 1999", // deliberately wrong
		ConsumerBootID:    bootID,
	}

	poison := CheckPoison(meta)
	if !poison.Poisoned() {
		t.Fatal("test setup broken: expected poisoned")
	}

	if AttemptRecovery(dir, &meta, poison) {
		t.Fatal("AttemptRecovery must refuse when the recorded fingerprint doesn't match — the pid may have been recycled")
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive: an unverified identity must never be killed")
	}
}

// TestAttemptRecovery_ReclaimsAfterRebootWithoutSignaling proves that when
// the recorded machine boot no longer matches the current one, the slot is
// reclaimed WITHOUT sending any signal — nothing survives a reboot, so
// whatever process now coincidentally shares the recorded pgid number must
// not be touched, even though the slot itself is safe to hand out again.
func TestAttemptRecovery_ReclaimsAfterRebootWithoutSignaling(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("ProcessStartTime: %v", err)
	}
	meta := Meta{
		UDID:              "simpool-test-udid-reboot",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
		ConsumerBootID:    "deliberately-different-boot-id",
	}

	poison := CheckPoison(meta)
	if !poison.Poisoned() {
		t.Fatal("test setup broken: expected poisoned")
	}

	if !AttemptRecovery(dir, &meta, poison) {
		t.Fatal("AttemptRecovery should reclaim a slot whose recorded boot differs from the current one")
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("a reboot-mismatch recovery must never send any signal — the process should still be alive")
	}
	if meta.ConsumerPGID != 0 {
		t.Errorf("ConsumerPGID should be cleared even on the no-signal reboot path, got %d", meta.ConsumerPGID)
	}
}

// TestAttemptRecovery_NeverKillsOnLiveConsumersSignal is the most important
// safety rule: a live process referencing the UDID on its own command line
// is the HEALTHY state for a leased slot (a legitimate axe/simctl/mav
// session), never an orphan, and must never be a kill candidate — even if
// Mode happens to be "with".
func TestAttemptRecovery_NeverKillsOnLiveConsumersSignal(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-udid-live-consumer"
	pid, cleanup := spawnLiveConsumerWithToken(t, token)
	defer cleanup()

	deadline := time.Now().Add(3 * time.Second)
	for {
		live, _ := procs.LiveConsumers(token)
		if len(live) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live consumer process never became visible to LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, mode := range []string{"lease", "with"} {
		meta := Meta{UDID: token, Mode: mode}
		poison := CheckPoison(meta)
		if poison.Reason != PoisonedByLiveConsumers {
			t.Fatalf("mode=%q: expected PoisonedByLiveConsumers, got %v", mode, poison.Reason)
		}

		if AttemptRecovery(dir, &meta, poison) {
			t.Fatalf("mode=%q: AttemptRecovery must never act on a LiveConsumers-only signal", mode)
		}
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the live consumer process must never be touched")
	}
}
