package pool

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// None of these tests create or boot a real simulator: companionDeviceList
// (the `simctl.ListDevices` seam) and deviceBelongsToSlotFind (the
// `simctl.Find` seam) are both swapped for fakes for the duration of each
// test, exactly like withFakeRunContext does one layer down in package
// simctl.

// withCompanionDeviceList points companionDeviceList at fn for the
// duration of the test and restores the real one on cleanup.
func withCompanionDeviceList(t *testing.T, fn func() ([]simctl.DeviceEntry, error)) {
	t.Helper()
	orig := companionDeviceList
	companionDeviceList = fn
	t.Cleanup(func() { companionDeviceList = orig })
}

// fakeDeviceList returns a companionDeviceList stub reporting a POPULATED
// device set (one unrelated device, so the set is never suspiciously empty)
// that includes udid with the given state — standing in for a healthy
// `simctl list devices` that genuinely knows about this exact device.
func fakeDeviceList(udid, state string) func() ([]simctl.DeviceEntry, error) {
	return func() ([]simctl.DeviceEntry, error) {
		return []simctl.DeviceEntry{
			{UDID: "simpool-test-unrelated-device", Name: "some other simulator", State: "Booted"},
			{UDID: udid, Name: "irrelevant to companionDeviceOffline", State: state},
		}, nil
	}
}

// fakeDeviceDeleted returns a companionDeviceList stub reporting a
// POPULATED device set that specifically does NOT include udid — the
// table's deleted-device row: conclusive proof of absence, not an empty
// listing's ambiguity.
func fakeDeviceDeleted() func() ([]simctl.DeviceEntry, error) {
	return func() ([]simctl.DeviceEntry, error) {
		return []simctl.DeviceEntry{
			{UDID: "simpool-test-unrelated-device", Name: "some other simulator", State: "Booted"},
		}, nil
	}
}

// fakeDeviceListEmpty returns a companionDeviceList stub simulating the
// finding #2 probe: a successful `xcrun` call (err == nil) that reports
// ZERO devices — reproduced directly against a fake xcrun printing
// `{"devices":{}}`, exactly what a degraded or mid-restart CoreSimulator can
// produce even though every device, including the user's own, still exists.
func fakeDeviceListEmpty() func() ([]simctl.DeviceEntry, error) {
	return func() ([]simctl.DeviceEntry, error) {
		return nil, nil
	}
}

// fakeDeviceListUnreadable returns a companionDeviceList stub simulating an
// outright listing failure (the real xcrun call itself erroring).
func fakeDeviceListUnreadable(err error) func() ([]simctl.DeviceEntry, error) {
	return func() ([]simctl.DeviceEntry, error) {
		return nil, err
	}
}

// withDeviceBelongsToSlotFind points deviceBelongsToSlotFind at fn for the
// duration of the test and restores the real one on cleanup.
func withDeviceBelongsToSlotFind(t *testing.T, fn func(udid string) (simctl.DeviceEntry, bool, error)) {
	t.Helper()
	orig := deviceBelongsToSlotFind
	deviceBelongsToSlotFind = fn
	t.Cleanup(func() { deviceBelongsToSlotFind = orig })
}

// fakeSlotOwnDevice returns a deviceBelongsToSlotFind stub reporting the
// queried udid as found and named EXACTLY for testRoot/testSlotDev/
// testSlotOSVer/testSlotN (see DeviceNameForGroup) — standing in for "this
// slot's own device, positively confirmed by name" without a real
// simulator.
func fakeSlotOwnDevice(state string) func(string) (simctl.DeviceEntry, bool, error) {
	name := DeviceNameForGroup(testRoot, GroupName(testSlotDev, testSlotOSVer), testSlotN)
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{Name: name, State: state}, true, nil
	}
}

// fakeSlotForeignDevice returns a deviceBelongsToSlotFind stub reporting the
// queried udid as found, genuinely existing, and Shutdown — but named for
// something that is NOT this slot (a developer's own, entirely unrelated
// simulator, not even pool-prefixed) — the exact probe finding #3 was
// demonstrated with: "David's own iPhone 17 Pro", Shutdown, not
// pool-prefixed, not this slot's name.
func fakeSlotForeignDevice(state string) func(string) (simctl.DeviceEntry, bool, error) {
	return func(string) (simctl.DeviceEntry, bool, error) {
		return simctl.DeviceEntry{Name: "David's own iPhone 17 Pro", State: state}, true, nil
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
	withCompanionDeviceList(t, fakeDeviceList(udid, "Booted"))

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

// TestCheckPoison_CompanionOnNonShutdownNonBootedStates_NeverReclaimed is
// the ALLOWLIST regression test (finding #1, HIGH): companionDeviceOffline
// used to be `entry.State != "Booted"`, a DENYLIST under which "Booting",
// "Shutting Down", "Creating", an unrecognized future state, and even ""
// (what a JSON response omitting the field would parse as) all classified
// as reclaimable. "Booting" in particular names a device whose launchd_sim
// is already up and whose boot is actively underway — reachable in
// production via a lease-renewal race (TTL 3m, renewed only on the next
// `simpool lease` call; mav's launch recipe boots the device while a
// concurrent acquire/lease sees the expired lease, no live ConsumerPGID for
// an acquire/lease slot, and a live companion — "not Booted" under the old
// code would kill it and hand the device to a second consumer). Only
// State == "Shutdown" may ever be treated as offline.
func TestCheckPoison_CompanionOnNonShutdownNonBootedStates_NeverReclaimed(t *testing.T) {
	for _, state := range []string{"Booting", "Shutting Down", "Creating", "Unknown", ""} {
		t.Run("state="+state, func(t *testing.T) {
			dir := t.TempDir()
			// The udid itself must never contain whitespace: IsIdbCompanionFor
			// (correctly) tokenizes the companion's command line on
			// strings.Fields, so a udid embedding a literal space (as
			// "Shutting Down" would, pasted in unsanitized) breaks the exact
			// --udid flag match test fixtures rely on — real UDIDs are plain
			// hex UUIDs and never contain spaces, so this is purely a test
			// fixture concern, not a production one.
			udid := "simpool-test-companion-transitional-" + strings.ReplaceAll(state, " ", "_")
			pid, cleanup := spawnIdbCompanion(t, udid)
			defer cleanup()
			waitForLiveConsumer(t, udid)
			withCompanionDeviceList(t, fakeDeviceList(udid, state))

			meta := Meta{UDID: udid, Mode: "lease"}
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByLiveConsumers {
				t.Fatalf("state=%q: expected PoisonedByLiveConsumers (never PoisonedByOrphanedCompanions for anything but a positively confirmed Shutdown), got %v", state, poison.Reason)
			}
			if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatalf("state=%q: AttemptRecovery must never reclaim a companion whose device is not positively confirmed Shutdown", state)
			}
			if syscall.Kill(pid, 0) != nil {
				t.Fatalf("state=%q: the companion process must still be alive", state)
			}
		})
	}
}

// TestCheckPoison_CompanionOnShutdownDevice_AutoReclaimedWhenIdentityVerified
// proves the primary fix, WITH the identity guard satisfied: a companion
// pinned to a device confirmed Shutdown, AND independently confirmed (by
// name, via deviceBelongsToSlot) to be this exact slot's own device, is
// inert (idb respawns it on demand) and safe to kill automatically —
// regardless of Meta.Mode, since idb_companion residue has nothing to do
// with which subcommand held the slot.
func TestCheckPoison_CompanionOnShutdownDevice_AutoReclaimedWhenIdentityVerified(t *testing.T) {
	for _, mode := range []string{"lease", "acquire", "with", ""} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			udid := "simpool-test-companion-shutdown-verified-" + mode
			pid, cleanup := spawnIdbCompanion(t, udid)
			defer cleanup()
			waitForLiveConsumer(t, udid)
			withCompanionDeviceList(t, fakeDeviceList(udid, "Shutdown"))
			withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Shutdown"))

			meta := Meta{UDID: udid, Mode: mode}
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByOrphanedCompanions {
				t.Fatalf("mode=%q: expected PoisonedByOrphanedCompanions, got %v", mode, poison.Reason)
			}
			if len(poison.CompanionPIDs) != 1 || poison.CompanionPIDs[0] != pid {
				t.Fatalf("mode=%q: expected CompanionPIDs=[%d], got %v", mode, pid, poison.CompanionPIDs)
			}
			if !AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatalf("mode=%q: AttemptRecovery should have reclaimed the orphaned companion once identity was verified", mode)
			}
			waitForDead(t, pid)
		})
	}
}

// TestCheckPoison_CompanionOnShutdownDevice_NotAutoReclaimedWithoutIdentity
// is the finding #3 (HIGH) regression test: a companion pinned to a UDID
// that is genuinely Shutdown is NOT enough by itself — meta.UDID is
// advisory and can be stale or corrupt, so AttemptRecovery must refuse to
// kill unless deviceBelongsToSlot can positively confirm, by name, that
// this Shutdown device is actually this slot's own. Two ways identity can
// fail to confirm: the device isn't found at all under this UDID (default
// deviceBelongsToSlotFind against a synthetic UDID — mirrors the existing
// ConsumerPGID tests' own testRoot convention), and the device IS found but
// is named for something else entirely — reproducing the review's own
// probe: a companion pinned to "David's own iPhone 17 Pro", Shutdown, not
// pool-prefixed, not this slot's name.
func TestCheckPoison_CompanionOnShutdownDevice_NotAutoReclaimedWithoutIdentity(t *testing.T) {
	cases := []struct {
		name string
		fake func(string) (simctl.DeviceEntry, bool, error)
	}{
		{"udid not found at all", nil}, // deliberately leaves the real simctl.Find in place
		{"udid names a foreign, unrelated device", fakeSlotForeignDevice("Shutdown")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			udid := "simpool-test-companion-shutdown-unverified-" + strings.ReplaceAll(tc.name, " ", "-")
			pid, cleanup := spawnIdbCompanion(t, udid)
			defer cleanup()
			waitForLiveConsumer(t, udid)
			withCompanionDeviceList(t, fakeDeviceList(udid, "Shutdown"))
			if tc.fake != nil {
				withDeviceBelongsToSlotFind(t, tc.fake)
			}

			meta := Meta{UDID: udid, Mode: "lease"}
			poison := CheckPoison(meta)
			if poison.Reason != PoisonedByOrphanedCompanions {
				t.Fatalf("expected PoisonedByOrphanedCompanions (the device state condition alone is satisfied), got %v", poison.Reason)
			}
			if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
				t.Fatal("AttemptRecovery must refuse to kill a companion whose target device's identity as THIS slot's own could not be verified, even though the device is genuinely Shutdown")
			}
			if syscall.Kill(pid, 0) != nil {
				t.Fatal("the companion process must still be alive — an unverifiable kill target must never be touched")
			}

			// The explicit, operator-opt-in escape hatch must still be able
			// to reach it, exactly like the ConsumerPGID case's own
			// --disown-poisoned role.
			if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); err != nil {
				t.Fatalf("DisownPoisonedSlot should have killed the identity-unverifiable companion on explicit request: %v", err)
			}
			waitForDead(t, pid)
			if meta.UDID != "" {
				t.Errorf("DisownPoisonedSlot should have forgotten the stale UDID, got %+v", meta)
			}
		})
	}
}

// TestCheckPoison_CompanionOnDeletedDevice_NeverAutoReclaimed_DisownRequired
// proves finding #3's resolved tension for the exact real-world incident
// this whole feature exists to fix: a companion still holding the UDID of a
// simulator that no longer exists at all. Unlike the Shutdown case,
// deviceBelongsToSlot can NEVER be satisfied here — there is no device left
// to name-check — so this is now, deliberately, unreachable via the fully
// automatic AttemptRecovery path. It remains reachable only through the
// explicit, operator-opt-in `--disown-poisoned` escape hatch (see
// DisownPoisonedSlot's PoisonedByOrphanedCompanions branch), exactly the
// same role that flag already plays for an unverifiable ConsumerPGID.
func TestCheckPoison_CompanionOnDeletedDevice_NeverAutoReclaimed_DisownRequired(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-deleted"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceList(t, fakeDeviceDeleted())

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByOrphanedCompanions {
		t.Fatalf("expected PoisonedByOrphanedCompanions for a companion on a deleted device, got %v", poison.Reason)
	}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never automatically reclaim a companion on a deleted device — there is no device left to confirm this slot owns it, so this is structurally an unverifiable kill")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive after the refused automatic attempt")
	}

	if err := DisownPoisonedSlot(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison); err != nil {
		t.Fatalf("DisownPoisonedSlot should have reclaimed the deleted-device companion on explicit request: %v", err)
	}
	waitForDead(t, pid)
	if meta.UDID != "" {
		t.Errorf("DisownPoisonedSlot should have forgotten the stale (deleted) UDID, got %+v", meta)
	}
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
	withCompanionDeviceList(t, fakeDeviceList(token, "Shutdown"))

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
	withCompanionDeviceList(t, fakeDeviceList(token, "Shutdown"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Shutdown"))

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
// the device listing itself fails, that must never be read as "confirmed
// offline", even though the process is a genuine idb_companion for this
// exact udid.
func TestCheckPoison_CompanionDeviceStateUnreadable_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-unreadable"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceList(t, fakeDeviceListUnreadable(errors.New("xcrun simctl list devices -j: boom")))

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

// TestCheckPoison_CompanionDeviceListEmpty_NeverReclaimed is the finding #2
// (HIGH) regression test: a SUCCESSFUL simctl call reporting ZERO devices
// must never be read as "udid was verified deleted" — reproduced directly
// against a fake `xcrun` exiting 0 and printing `{"devices":{}}`, exactly
// what a degraded or mid-restart CoreSimulator can report even though every
// device (the user's own included) genuinely still exists. Before this fix,
// companionDeviceOffline's `!found` branch could not tell this apart from a
// populated listing that specifically excludes udid (the real deleted-device
// case, still exercised by
// TestCheckPoison_CompanionOnDeletedDevice_NeverAutoReclaimed_DisownRequired).
func TestCheckPoison_CompanionDeviceListEmpty_NeverReclaimed(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-empty-listing"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withCompanionDeviceList(t, fakeDeviceListEmpty())

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := CheckPoison(meta)
	if poison.Reason != PoisonedByLiveConsumers {
		t.Fatalf("expected PoisonedByLiveConsumers for a successful-but-empty device listing — it is not proof of deletion, got %v", poison.Reason)
	}
	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must never act on a successful-but-empty device listing")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}

// TestReclaimOrphanedCompanions_ReVerifiesDeviceStateBeforeKilling is the
// finding #7 "no re-verify before acting" regression test, applied to the
// device-offline condition: CheckPoison's own determination and
// AttemptRecovery's kill are not the same instant, so this proves the
// device state is checked again, immediately before killing, not trusted
// from the Poison value alone. A hand-built Poison claims
// PoisonedByOrphanedCompanions (as if CheckPoison had determined this
// earlier), but companionDeviceList is set to report the device BOOTED at
// the moment AttemptRecovery actually runs — simulating the device coming
// back up in the window between determination and action.
func TestReclaimOrphanedCompanions_ReVerifiesDeviceStateBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-reverify-device"
	pid, cleanup := spawnIdbCompanion(t, udid)
	defer cleanup()
	waitForLiveConsumer(t, udid)
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Booted"))
	withCompanionDeviceList(t, fakeDeviceList(udid, "Booted")) // device is back up NOW

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := Poison{Reason: PoisonedByOrphanedCompanions, CompanionPIDs: []int{pid}}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must re-check the device is still offline immediately before killing, not trust an earlier determination")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the companion process must still be alive")
	}
}

// TestReclaimOrphanedCompanions_ReVerifiesCompanionIdentityBeforeKilling is
// finding #7's re-verify rule applied to the per-PID companion-identity
// condition: a hand-built Poison lists a pid that is, at the moment
// AttemptRecovery actually runs, NOT (or no longer) a genuine idb_companion
// for udid — simulating that pid having exited and been recycled by an
// unrelated process in the window between CheckPoison and this call.
func TestReclaimOrphanedCompanions_ReVerifiesCompanionIdentityBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	token := "simpool-test-companion-reverify-identity"
	pid, cleanup := spawnLiveConsumerWithToken(t, token) // genuinely NOT idb_companion
	defer cleanup()
	waitForLiveConsumer(t, token)
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Shutdown"))
	withCompanionDeviceList(t, fakeDeviceList(token, "Shutdown"))

	meta := Meta{UDID: token, Mode: "lease"}
	poison := Poison{Reason: PoisonedByOrphanedCompanions, CompanionPIDs: []int{pid}}

	if AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison) {
		t.Fatal("AttemptRecovery must re-verify every pid is still a genuine idb_companion immediately before killing, not trust an earlier determination")
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("the non-companion process must still be alive")
	}
}

// TestReclaimOrphanedCompanions_AttemptsEveryPIDEvenIfAnEarlierOneErrors is
// the finding #7 "partial kill then return false" regression test: with
// several PIDs, an earlier version of this code returned false the instant
// procs.Kill reported an error for ANY one of them — leaving that one
// already dead while the loop never even reached the rest, which then
// stayed alive despite the function reporting "nothing changed". Three real
// companion processes are spawned; companionKill is overridden to actually
// kill each one (via the real procs.Kill) while ALSO reporting a fake EPERM
// for the first pid specifically — the one way procs.Kill itself can ever
// genuinely fail. All three must still end up dead, proving every pid gets
// a kill attempt regardless of an earlier one's reported error.
func TestReclaimOrphanedCompanions_AttemptsEveryPIDEvenIfAnEarlierOneErrors(t *testing.T) {
	dir := t.TempDir()
	udid := "simpool-test-companion-partial-kill"
	var pids []int
	for i := 0; i < 3; i++ {
		pid, cleanup := spawnIdbCompanion(t, udid)
		defer cleanup()
		pids = append(pids, pid)
	}
	// All three share the same udid deliberately: CheckPoison's own
	// allOrphanedIdbCompanions groups every live PID referencing one udid
	// together, so a real multi-PID poisoned set looks exactly like this.
	waitForLiveConsumer(t, udid)
	withCompanionDeviceList(t, fakeDeviceList(udid, "Shutdown"))
	withDeviceBelongsToSlotFind(t, fakeSlotOwnDevice("Shutdown"))

	origKill := companionKill
	defer func() { companionKill = origKill }()
	var attempted []int
	firstPID := pids[0]
	companionKill = func(pid int, sig syscall.Signal) error {
		attempted = append(attempted, pid)
		realErr := procs.Kill(pid, sig) // actually kill it regardless
		if pid == firstPID {
			return syscall.EPERM // ...but report a fake permission failure
		}
		return realErr
	}

	meta := Meta{UDID: udid, Mode: "lease"}
	poison := Poison{Reason: PoisonedByOrphanedCompanions, CompanionPIDs: pids}
	AttemptRecovery(testRoot, dir, testSlotN, GroupName(testSlotDev, testSlotOSVer), &meta, poison)

	if len(attempted) != 3 {
		t.Fatalf("expected a kill attempt for all 3 pids regardless of the first one's reported error, got attempts for %v", attempted)
	}
	for _, pid := range pids {
		waitForDead(t, pid)
	}
}
