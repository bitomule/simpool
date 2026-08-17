package pool

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
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

// TestDisownPoisonedSlot_ForgetsFingerprintWithoutKilling proves the manual
// escape hatch's core promise (`simpool reap --disown-poisoned`): it clears
// a with-mode slot's poisoned ConsumerPGID fingerprint, UDID, and Mode
// WITHOUT ever sending the process behind it any signal. Unlike
// AttemptRecovery it must never require — or even attempt — identity
// verification: the entire reason this exists is for the case where
// identity can never be verified (a recycled pid, or a process group EPERM
// makes unkillable outright), so the deliberately wrong fingerprint below
// stands in for exactly that permanently-unverifiable case.
func TestDisownPoisonedSlot_ForgetsFingerprintWithoutKilling(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:              "simpool-test-udid-disown",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: "Mon Jan  1 00:00:00 1999", // deliberately wrong/unverifiable
	}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByConsumerPGID {
		t.Fatalf("test setup broken: expected PoisonedByConsumerPGID, got %v", poison.Reason)
	}

	if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); err != nil {
		t.Fatalf("DisownPoisonedSlot: %v", err)
	}
	if meta.ConsumerPGID != 0 || meta.ConsumerStartedAt != "" || meta.UDID != "" || meta.Mode != "" {
		t.Errorf("expected fully forgotten meta, got %+v", meta)
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("disown must never signal the process — it should still be alive")
	}

	persisted := ReadMeta(dir)
	if persisted.ConsumerPGID != 0 || persisted.UDID != "" {
		t.Errorf("disown should have persisted the forgotten identity, got %+v", persisted)
	}
}

// TestDisownPoisonedSlot_RefusesLiveConsumersReason proves disown enforces
// the same restraint as AttemptRecovery: a live process referencing the
// slot's UDID on its own command line is the healthy case for a leased
// slot, never something to disown (which would delete its device) out from
// under it.
func TestDisownPoisonedSlot_RefusesLiveConsumersReason(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-udid-disown-live-consumer"
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

	meta := Meta{UDID: token, Mode: "with"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers, got %v", poison.Reason)
	}

	if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); !errors.Is(err, ErrNotDisownable) {
		t.Fatalf("expected ErrNotDisownable for a LiveConsumers-only signal, got %v", err)
	}
	if meta.UDID != token {
		t.Errorf("a refused disown must never mutate meta, got %+v", meta)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the live consumer process must never be touched")
	}
}

// TestDisownPoisonedSlot_RefusesCheckFailureReason proves an incomplete
// liveness check is never grounds to disown a slot either — a check that
// didn't run has proven nothing is actually wrong.
func TestDisownPoisonedSlot_RefusesCheckFailureReason(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	meta := Meta{
		UDID:              "simpool-test-udid-disown-check-failure",
		Mode:              "with",
		ConsumerPGID:      pgid,
		ConsumerStartedAt: fingerprint(t, pgid),
	}
	poison := Poison{Reason: PoisonedByCheckFailure}

	if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); !errors.Is(err, ErrNotDisownable) {
		t.Fatalf("expected ErrNotDisownable when the check itself failed, got %v", err)
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive")
	}
}

// TestDisownPoisonedSlot_RefusesNonWithMode proves disown, like
// AttemptRecovery, only ever applies to a `with`-launched slot: `acquire`
// never spawns a child and a `lease`'s whole point is that a live process
// is the healthy case, never something simpool spawned and may disown.
func TestDisownPoisonedSlot_RefusesNonWithMode(t *testing.T) {
	dir := t.TempDir()
	pgid, cleanup := spawnRealOrphan(t)
	defer cleanup()

	for _, mode := range []string{"acquire", "lease", ""} {
		meta := Meta{
			UDID:              "simpool-test-udid-disown-nonwith-" + mode,
			Mode:              mode,
			ConsumerPGID:      pgid,
			ConsumerStartedAt: fingerprint(t, pgid),
		}
		poison := CheckPoison(meta)
		if poison.Reason != PoisonedByConsumerPGID {
			t.Fatalf("mode=%q: expected PoisonedByConsumerPGID, got %v", mode, poison.Reason)
		}
		if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); !errors.Is(err, ErrNotDisownable) {
			t.Fatalf("mode=%q: expected ErrNotDisownable, got %v", mode, err)
		}
	}
	if syscall.Kill(pgid, 0) != nil {
		t.Fatal("process must still be alive")
	}
}

// --- Orphaned idb_companion reclaim ---
//
// The scenario this covers: `idb_companion --udid <udid> --grpc-domain-sock
// /tmp/idb/<udid>` outlives its simulator's shutdown (or deletion)
// indefinitely — confirmed directly against the real binary's own --help
// text ("Terminate if the target goes offline" defaults to false) — so a
// bare `pgrep -f <udid>` match against it used to poison the slot forever,
// with nothing ever able to reclaim it (the real production incident this
// exists to fix: three production pool slots stuck FAIL, one companion
// still holding the UDID of a simulator deleted nine days earlier).
//
// None of these tests create or boot a real simulator: companionDeviceFind
// (the `simctl.Find` seam) is swapped for a fake for the duration of each
// test, exactly like withFakeRunContext does one layer down in package
// simctl.

// withCompanionDeviceFind points companionDeviceFind at fn for the
// duration of the test and restores the real one on cleanup.
func withCompanionDeviceFind(t *testing.T, fn func(udid string) (simctl.DeviceEntry, bool, error)) {
	t.Helper()
	orig := companionDeviceFind
	companionDeviceFind = fn
	t.Cleanup(func() { companionDeviceFind = orig })
}

func fakeBooted(name string) func(string) (simctl.DeviceEntry, bool, error) {
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{Name: name, State: "Booted"}, true, nil
	}
}

func fakeShutdown(name string) func(string) (simctl.DeviceEntry, bool, error) {
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{Name: name, State: "Shutdown"}, true, nil
	}
}

func fakeDeleted() func(string) (simctl.DeviceEntry, bool, error) {
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{}, false, nil
	}
}

func fakeUnreadable(err error) func(string) (simctl.DeviceEntry, bool, error) {
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{}, false, err
	}
}

// buildFakeIdbCompanionBinary compiles a trivial real binary named
// "idb_companion" that just sleeps — a shell script won't do (see
// procs_test.go's buildFakeSimpool: `ps` reports a script's *interpreter*,
// not the script itself, as argv[0], which IsIdbCompanionFor's binary-name
// check must genuinely tell apart).
func buildFakeIdbCompanionBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(5 * time.Minute) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "idb_companion")
	out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("building fake idb_companion binary: %v\n%s", err, out)
	}
	return bin
}

// spawnIdbCompanion starts a real process whose own argv0 is a compiled
// "idb_companion" binary and whose --udid flag is exactly udid — the same
// shape MatchingPIDs/LiveConsumers and IsIdbCompanionFor see from the real
// daemon.
func spawnIdbCompanion(t *testing.T, udid string) (pid int, cleanup func()) {
	t.Helper()
	bin := buildFakeIdbCompanionBinary(t)
	cmd := exec.Command(bin, "--udid", udid, "--grpc-domain-sock", "/tmp/idb/"+udid)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, func() { _ = syscall.Kill(pid, syscall.SIGKILL) }
}

func waitForLiveConsumer(t *testing.T, udid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		live, _ := procs.LiveConsumers(udid)
		if len(live) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("consumer process never became visible to LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCheckPoison_CompanionOnBootedDevice_NeverReclaimed proves the table's
// first row: a companion attached to a device that is genuinely Booted may
// be doing real work (or about to), so it must classify as the ordinary,
// never-a-kill-candidate PoisonedByLiveConsumers — never
// PoisonedByOrphanedCompanions — and AttemptRecovery must leave it running.
func TestCheckPoison_CompanionOnBootedDevice_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-booted"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceFind(t, fakeBooted("irrelevant"))

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers for a companion on a Booted device, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a companion attached to a Booted device")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}

// TestCheckPoison_CompanionOnShutdownDevice_Reclaimed proves the table's
// second row and the primary fix: a companion pinned to a device confirmed
// Shutdown is inert (idb respawns it on demand) and safe to kill —
// regardless of Meta.Mode, since idb_companion residue has nothing to do
// with which subcommand held the slot.
func TestCheckPoison_CompanionOnShutdownDevice_Reclaimed(t *testing.T) {
	for _, mode := range []string{"lease", "acquire", "with", ""} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			udid := "simpool-test-companion-shutdown-" + mode
			pid, cleanup := spawnIdbCompanion(t, udid)
			defer cleanup()
			waitForLiveConsumer(t, udid)
			withCompanionDeviceFind(t, fakeShutdown("irrelevant"))

			meta := Meta{UDID: udid, Mode: mode}
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByOrphanedCompanions {
				t.Fatalf("mode=%q: expected PoisonedByOrphanedCompanions, got %v", mode, poison.Reason)
			}
			if len(poison.CompanionPIDs) != 1 || poison.CompanionPIDs[0] != pid {
				t.Fatalf("mode=%q: expected CompanionPIDs=[%d], got %v", mode, pid, poison.CompanionPIDs)
			}
			if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatalf("mode=%q: AttemptRecovery should have reclaimed the orphaned companion", mode)
			}
			waitForDead(t, pid)
		})
	}
}

// TestCheckPoison_CompanionOnDeletedDevice_Reclaimed proves the table's
// deleted-device row — the exact real-world incident this exists to fix: a
// companion still holding the UDID of a simulator that no longer exists at
// all cannot even be asked its boot state, but "no device by this UDID
// exists" is itself conclusive proof it isn't Booted.
func TestCheckPoison_CompanionOnDeletedDevice_Reclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-deleted"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceFind(t, fakeDeleted())

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedCompanions {
		t.Fatalf("expected PoisonedByOrphanedCompanions for a companion on a deleted device, got %v", poison.Reason)
	}
	if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery should have reclaimed the companion attached to a deleted device")
	}
	waitForDead(t, pid)
}

// TestCheckPoison_NonCompanionOnShutdownDevice_NeverReclaimed proves the
// table's non-companion row: a process holding the UDID that is NOT
// idb_companion must never be reclaimed just because the device happens to
// be Shutdown — only the specific, known-respawnable daemon is narrowly
// safe here, never a generic "device is off, so this must be idle" guess.
func TestCheckPoison_NonCompanionOnShutdownDevice_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-noncompanion-shutdown"
	pid, cleanup := spawnLiveConsumerWithToken(t, token)
	defer cleanup()
	waitForLiveConsumer(t, token)
	withCompanionDeviceFind(t, fakeShutdown("irrelevant"))

	meta := Meta{UDID: token, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers for a non-companion process even on a Shutdown device, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never reclaim a non-companion process, regardless of device state")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the non-companion process must still be alive")
	}
}

// TestCheckPoison_MixedCompanionAndNonCompanion_NeverReclaimed proves a
// single non-companion PID mixed in with a real, otherwise-reclaimable
// companion poisons the whole slot back to the ordinary, untouched
// PoisonedByLiveConsumers reason — the set is only narrowly explained when
// EVERY live PID is verified companion residue.
func TestCheckPoison_MixedCompanionAndNonCompanion_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-mixed-companion"
	companionPID, cleanupCompanion := spawnIdbCompanion(t, token)
	defer cleanupCompanion()
	otherPID, cleanupOther := spawnLiveConsumerWithToken(t, token)
	defer cleanupOther()
	deadline := time.Now().Add(3 * time.Second)
	for {
		live, _ := procs.LiveConsumers(token)
		if len(live) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both processes never became visible to LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}
	withCompanionDeviceFind(t, fakeShutdown("irrelevant"))

	meta := Meta{UDID: token, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers when a non-companion is mixed in, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never act when even one live PID isn't a verified companion")
	}
	if syscall.Kill(companionPID, 0) != nil || syscall.Kill(otherPID, 0) != nil {
		t.Fatal("neither process should have been touched")
	}
}

// TestCheckPoison_CompanionDeviceStateUnreadable_NeverReclaimed proves the
// "couldn't verify must read as busy, don't touch" rule applies here too: if
// simctl.Find itself fails, that must never be read as "confirmed offline",
// even though the process is a genuine idb_companion for this exact udid.
func TestCheckPoison_CompanionDeviceStateUnreadable_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-unreadable"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceFind(t, fakeUnreadable(errors.New("xcrun simctl list devices -j: boom")))

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers when device state can't be determined, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never act when the device state check itself failed")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}
