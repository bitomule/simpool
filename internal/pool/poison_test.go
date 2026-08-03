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
// its real parent here — never collects it, and an unreaped zombie still
// answers kill(-pgid, 0) with EPERM (not ESRCH) on Darwin, which
// PGIDAlive/VerifyConsumerIdentity correctly (see their doc comments) read
// as "still alive" — making a successful recovery look like a failed one.
// In real usage this isn't an issue: `simpool with` itself is already dead
// by the time this scenario happens, so the orphan reparents to launchd,
// which reaps it immediately once killed.
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
	go func() { _ = cmd.Wait() }()
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

// testRoot/testSlotN stand in for AttemptRecovery's new root/n/device/
// osVersion identity parameters (see deviceBelongsToSlot) across every test
// in this file: none of them use a real simctl device (they all use a bare
// placeholder UDID string), so simctl.Find always reports "not found" for
// it regardless of what identity parameters are passed — deviceBelongsToSlot
// returns false either way, and AttemptRecovery's Shutdown branch is
// correctly skipped. The kill-then-clear-metadata behavior these tests
// actually exercise never depends on these values. The cross-slot identity
// guard itself (Shutdown only fires when the UDID's real device is named
// exactly for this slot) is proved with a real simctl device in
// internal/cli/integration_test.go, where creating one is already the
// established, SIMPOOL_RUN_INTEGRATION-gated convention.
const (
	testRoot      = "/tmp/simpool-poison-test-root"
	testSlotN     = 0
	testSlotDev   = "TestDevice"
	testSlotOSVer = "1.0"
)

func fingerprint(t *testing.T, pgid int) string {
	t.Helper()
	startedAt, err := procs.ProcessStartTime(pgid)
	if err != nil {
		t.Fatalf("ProcessStartTime: %v", err)
	}
	return startedAt
}

// TestAttemptRecovery_SuccessfulReclaim proves the primary, mandated
// contract: a poisoned `with` slot whose recorded fingerprint (the
// process-group leader's own start time) matches the still-alive process
// under ConsumerPGID is killed and the slot is reclaimed.
func TestAttemptRecovery_SuccessfulReclaim(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:              "simpool-test-udid-reclaim",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: fingerprint(t, pgid),
	}

	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByConsumerPGID {
		t.Fatalf("expected PoisonedByConsumerPGID, got %v", poison.Reason)
	}

	if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery should have succeeded with a correct fingerprint")
	}
	if meta.ConsumerPGID != 0 || meta.ConsumerStartedAt != "" {
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
// never be killed. This is also the "identidad no verificable -> cuarentena,
// proceso sigue vivo" mandated test.
func TestAttemptRecovery_RefusesOnRecycledPidFingerprint(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:              "simpool-test-udid-recycled",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: "Mon Jan  1 00:00:00 1999", // deliberately wrong
	}

	poison := CheckPoison(meta)
	if !poison.Poisoned() {
		t.Fatal("test setup broken: expected poisoned")
	}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must refuse when the recorded fingerprint doesn't match — the pid may have been recycled")
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive: an unverified identity must never be killed")
	}
}

// TestAttemptRecovery_RefusesWhenNoFingerprintRecorded covers a meta.json
// predating this feature (or written by a build that never captured a
// fingerprint): ConsumerPGID is set and genuinely alive, but there is
// nothing to verify it against, so recovery must refuse rather than trust
// the bare pgid number alone.
func TestAttemptRecovery_RefusesWhenNoFingerprintRecorded(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:         "simpool-test-udid-no-fingerprint",
		Mode:         "with",
		ConsumerPGID: pgid,
		// Deliberately no ConsumerStartedAt.
	}

	poison := CheckPoison(meta)
	if !poison.Poisoned() {
		t.Fatal("test setup broken: expected poisoned")
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must refuse when no fingerprint was ever recorded")
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive")
	}
}

// TestAttemptRecovery_RefusesWhenLeaderAlreadyExited is the deliberate,
// documented scope limit (see VerifyConsumerIdentity's doc comment and the
// README's "Architecture" section): if the recorded process-group leader
// has already exited but a descendant it spawned survives under the same
// pgid, PGIDAlive still reports the group as poisoned, but there is no
// live leader left to re-identify against the recorded fingerprint — so
// recovery must refuse rather than trust bare pgid membership alone.
func TestAttemptRecovery_RefusesWhenLeaderAlreadyExited(t *testing.T) {
	dir := t.TempDir()

	// A leader that spawns a grandchild in its own group and then exits
	// immediately, leaving the grandchild as the only surviving member of
	// the pgid.
	script := `#!/bin/sh
sleep 300 &
exit 0
`
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "leader.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	// Fingerprint the leader BEFORE it exits.
	startedAt := fingerprint(t, pgid)
	go func() { _ = cmd.Wait() }()

	// Wait for the leader itself to be gone while the group (via the
	// grandchild) is still alive.
	deadline := time.Now().Add(3 * time.Second)
	for {
		leaderGone := syscall.Kill(pgid, 0) != nil || func() bool {
			_, err := procs.ProcessStartTime(pgid)
			return err != nil
		}()
		if leaderGone && procs.PGIDAlive(pgid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test setup broken: leader never exited while its grandchild kept the group alive")
		}
		time.Sleep(20 * time.Millisecond)
	}

	meta := Meta{
		UDID:              "simpool-test-udid-leader-gone",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: startedAt,
	}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByConsumerPGID {
		t.Fatalf("expected PoisonedByConsumerPGID (group still alive via grandchild), got %v", poison.Reason)
	}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must refuse to act when the recorded leader has already exited, even though its group survives — this is the documented scope limit, not a bug to route around")
	}
	if !procs.PGIDAlive(pgid) {
		t.Fatal("the surviving grandchild must not have been touched")
	}
}

// TestAttemptRecovery_NeverKillsOnLiveConsumersSignal is the most important
// safety rule: a live process referencing the UDID on its own command line
// is the HEALTHY state for a leased slot (a legitimate axe/simctl/mav
// session), never an orphan, and must never be a kill candidate — even if
// Mode happens to be "with". Covers with/lease/acquire modes.
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

	for _, mode := range []string{"lease", "with", "acquire"} {
		meta := Meta{UDID: token, Mode: mode}
		poison := CheckPoison(meta)
		if poison.Reason != PoisonedByLiveConsumers {
			t.Fatalf("mode=%q: expected PoisonedByLiveConsumers, got %v", mode, poison.Reason)
		}

		if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
			t.Fatalf("mode=%q: AttemptRecovery must never act on a LiveConsumers-only signal", mode)
		}
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the live consumer process must never be touched")
	}
}

// TestAttemptRecovery_NeverActsOnCheckFailure is the regression test for
// "fallo al comprobar -> tratado como ocupado": if the liveness check
// itself could not complete, that must never be read as grounds for a kill
// decision (nor as "free"), regardless of what CheckPoison's PGID branch
// alone might otherwise suggest.
func TestAttemptRecovery_NeverActsOnCheckFailure(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:              "simpool-test-udid-check-failure",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: fingerprint(t, pgid),
	}
	// Simulate the check itself failing (as CheckPoison would report if
	// LiveConsumers' pgrep failed to run) rather than actually asking
	// CheckPoison — PoisonedByConsumerPGID would otherwise win first.
	poison := Poison{Reason: PoisonedByCheckFailure}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never act when the poison determination itself failed")
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive")
	}
}

// TestAttemptRecovery_RefusesNonWithMode proves ConsumerPGID is only ever a
// kill candidate for a slot whose previous consumer was `simpool with` —
// `acquire` never spawns a child (its ConsumerPGID should never be set,
// but this defends the invariant directly) and a `lease`'s whole point is
// that a live process is the healthy case, not something simpool spawned
// and may reap.
func TestAttemptRecovery_RefusesNonWithMode(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	for _, mode := range []string{"acquire", "lease", ""} {
		meta := Meta{
			UDID:              "simpool-test-udid-nonwith-" + mode,
			Mode:              mode,
			ConsumerPGID:      pgid,
			ConsumerStartedAt: fingerprint(t, pgid),
		}
		poison := CheckPoison(meta)
		if poison.Reason != PoisonedByConsumerPGID {
			t.Fatalf("mode=%q: expected PoisonedByConsumerPGID, got %v", mode, poison.Reason)
		}
		if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
			t.Fatalf("mode=%q: AttemptRecovery must refuse a non-with mode even with a verified ConsumerPGID", mode)
		}
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive")
	}
}
