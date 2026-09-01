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
)

// leaseResidueFixture builds the exact situation reported from a MAV hot
// loop and reproduced by hand against a real pool before this file existed:
// one slot, held last in "lease" mode by key, whose lease has since
// expired, with a real live process carrying the slot's UDID in its own
// argv — the shape `simctl spawn <udid> log stream`, `axe describe-ui
// --udid <udid>` and the booted app itself all have. Returns the slot dir.
//
// The UDID is synthetic (never a real device): CheckPoison only ever
// pgreps for it, and everything this file asserts happens strictly before
// any provisioning, so no simulator is involved.
func leaseResidueFixture(t *testing.T, root, key, udid string) string {
	t.Helper()
	groupDir := GroupDir(root, "TestDevice", "1.0")
	dir := SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{
		UDID:     udid,
		Mode:     "lease",
		LeaseKey: key,
		LastUsed: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLease(dir, Lease{Key: key, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	spawnResidueProcess(t, udid)
	return dir
}

// spawnResidueProcess starts a real process whose OWN command line carries
// token, so procs.LiveConsumers (pgrep -f) genuinely sees it — the same
// technique poison_test.go's spawnLiveConsumerWithToken uses, repeated here
// with a wait so a test never races the process becoming visible.
func spawnResidueProcess(t *testing.T, token string) {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "lease_residue.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scriptPath, token)
	// Its own process group, mirroring poison_test.go's spawnRealOrphan:
	// nothing here needs a group of its own to be found, but it keeps this
	// process out of reach of any other test in this package that kills a
	// process GROUP as part of a recovery it is exercising.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// Two consecutive sightings, not one: `pgrep -f` is a sampling of the
	// process table, and a single positive sample taken the instant a
	// process appears is a thinner precondition than these tests deserve —
	// every one of them asserts what simpool does WHILE that process is
	// visible, so a fixture that returned on a first flicker would make a
	// missed sample look like a behaviour change.
	deadline := time.Now().Add(5 * time.Second)
	sightings := 0
	for {
		if live, err := procs.LiveConsumers(token); err == nil && len(live) > 0 {
			sightings++
			if sightings >= 2 {
				return
			}
		} else {
			sightings = 0
		}
		if time.Now().After(deadline) {
			t.Fatal("the residue process never became reliably visible to LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAcquireLease_ReclaimsOwnLeaseResidueAtMaxOne is reproduction #1 of
// the report this work came from, as a test: a key that leased a slot,
// left the usual live processes behind on it, and let its TTL lapse must
// be able to lease that same slot again. Before, CheckPoison saw those
// processes as PoisonedByLiveConsumers — never a kill candidate, so never
// recoverable — and quarantined the slot against everyone including its
// own key; with --max 1 there was no second slot to fall back to, so the
// key could never lease again at all.
func TestAcquireLease_ReclaimsOwnLeaseResidueAtMaxOne(t *testing.T) {
	root := t.TempDir()
	const key = "boxy-screenshots-ipad"
	dir := leaseResidueFixture(t, root, key, "simpool-test-udid-own-residue-max-one")

	slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1)
	if err != nil {
		t.Fatalf("re-leasing a key's own residue at --max 1: want success, got %v", err)
	}
	if slot.Dir != dir {
		t.Fatalf("expected the key's own slot-0 back, got %s", slot.Dir)
	}
	lease, err := ReadLease(slot.Dir)
	if err != nil {
		t.Fatalf("reading the renewed lease: %v", err)
	}
	if lease.Key != key || !lease.Alive() {
		t.Fatalf("expected a live lease for %q, got %+v", key, lease)
	}
}

// TestAcquireLease_ReclaimsOwnResidueAfterRelease covers the second half of
// the same report: `simpool release` succeeded, removed lease.json, and the
// next lease was refused identically — because the lease was never what
// blocked it. With lease.json gone, Meta.LeaseKey is the only thing left
// that can say whose residue the live processes are.
func TestAcquireLease_ReclaimsOwnResidueAfterRelease(t *testing.T) {
	root := t.TempDir()
	const key = "boxy-screenshots-ipad"
	dir := leaseResidueFixture(t, root, key, "simpool-test-udid-own-residue-after-release")

	released, err := ReleaseLease(root, key)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(released) != 1 || released[0] != dir {
		t.Fatalf("expected release to clear slot-0's lease, got %v", released)
	}
	if lease, _ := ReadLease(dir); lease.Key != "" {
		t.Fatalf("lease.json should be gone after release, got %+v", lease)
	}

	slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1)
	if err != nil {
		t.Fatalf("re-leasing after release, on the key's own residue: want success, got %v", err)
	}
	if slot.Dir != dir {
		t.Fatalf("expected the key's own slot-0 back, got %s", slot.Dir)
	}
}

// TestAcquireLease_ForeignKeyStaysQuarantined pins the boundary of the
// exemption: it is only ever for the key whose own session left the
// residue. Another key must still be refused — those processes are someone
// else's live work, and handing the slot over would be exactly the
// two-consumers-on-one-simulator harm the quarantine exists for.
func TestAcquireLease_ForeignKeyStaysQuarantined(t *testing.T) {
	root := t.TempDir()
	leaseResidueFixture(t, root, "owner-key", "simpool-test-udid-foreign-key-quarantined")

	_, err := AcquireLease(root, "TestDevice", "1.0", "someone-else", time.Hour, 1)
	if err == nil {
		t.Fatal("a foreign key must not be handed a slot carrying another key's live residue")
	}
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("want ErrAtCapacity, got %v", err)
	}
}

// TestAcquireLease_CapacityErrorNamesEachSlotsRealReason is the reporting
// half of the defect. The old message asserted one blanket reason — "every
// slot is busy or leased elsewhere" — about a slot that was neither, then
// sent the reader to `simpool status`, which showed that same slot free.
// The failure must now name what actually refused each slot.
func TestAcquireLease_CapacityErrorNamesEachSlotsRealReason(t *testing.T) {
	root := t.TempDir()
	leaseResidueFixture(t, root, "owner-key", "simpool-test-udid-capacity-error-names-reason")

	_, err := AcquireLease(root, "TestDevice", "1.0", "someone-else", time.Hour, 1)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"slot-0", SlotQuarantined.String(), "live process still references"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message must contain %q, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "every slot is busy or leased elsewhere") {
		t.Errorf("refusal must not assert a reason it did not check, got:\n%s", msg)
	}
}

// TestSlotAvailability_AgreesWithTheLeasePath is the defect stated as one
// assertion: the view `simpool status` prints and the verdict the leasing
// path acts on are the same computation. Before, status called this slot
// "free" (its flock is genuinely uncontended) while every acquisition path
// refused it.
func TestSlotAvailability_AgreesWithTheLeasePath(t *testing.T) {
	root := t.TempDir()
	const key = "owner-key"
	dir := leaseResidueFixture(t, root, key, "simpool-test-udid-status-agrees")

	if av := SlotAvailability(dir, ""); av.State != SlotQuarantined {
		t.Fatalf("keyless view of a slot every other caller is refused: want %v, got %v (%s)", SlotQuarantined, av.State, av.Detail())
	}
	if _, err := AcquireLease(root, "TestDevice", "1.0", "someone-else", time.Hour, 1); err == nil {
		t.Fatal("the leasing path must agree with that view and refuse a foreign key")
	}

	av := SlotAvailability(dir, key)
	if av.State != SlotFree || !av.OwnLeaseResidue {
		t.Fatalf("the owning key's view: want free with OwnLeaseResidue, got %v (residue=%v)", av.State, av.OwnLeaseResidue)
	}
	if _, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1); err != nil {
		t.Fatalf("the leasing path must agree with that view too and hand the slot back: %v", err)
	}
}

// TestSlotAvailability_KeylessViewStillNamesTheOwner proves the keyless
// view does not merely say "quarantined" and stop: it names the key that
// can still claim the slot, which is the difference between a status
// output someone can act on and one that sends them looking for a holder
// that does not exist.
func TestSlotAvailability_KeylessViewStillNamesTheOwner(t *testing.T) {
	root := t.TempDir()
	const key = "boxy-screenshots-ipad"
	dir := leaseResidueFixture(t, root, key, "simpool-test-udid-keyless-names-owner")

	av := SlotAvailability(dir, "")
	if av.State != SlotQuarantined || !av.OwnLeaseResidue {
		t.Fatalf("keyless view: want quarantined with the residue owner known, got %v (residue=%v)", av.State, av.OwnLeaseResidue)
	}
	detail := av.Detail()
	if !strings.Contains(detail, "live process still references") {
		t.Errorf("keyless detail should carry the poison reason, got %q", detail)
	}
	if !strings.Contains(detail, key) {
		t.Errorf("keyless detail should name the key that can still claim the slot, got %q", detail)
	}
}

// TestOwnLeaseResidue_NeverExemptsUnsafeReasons pins the three cases the
// exemption must never widen to, each of which would turn a deliberate
// safety rule into a hole: a `with`-spawned process group still alive (a
// real orphan, with its own verified recovery path), a liveness check that
// did not complete (never read as free, for anyone), and a slot whose last
// holder was not a lease at all.
func TestOwnLeaseResidue_NeverExemptsUnsafeReasons(t *testing.T) {
	const key = "repo-a"
	leaseMeta := Meta{Mode: "lease", LeaseKey: key, UDID: "u"}
	expired := Lease{Key: key, ExpiresAt: time.Now().Add(-time.Minute)}

	cases := []struct {
		name   string
		meta   Meta
		lease  Lease
		poison Poison
		key    string
	}{
		{"live with-spawned process group", leaseMeta, expired, Poison{Reason: PoisonedByConsumerPGID, PGID: 1}, key},
		{"liveness check failed", leaseMeta, expired, Poison{Reason: PoisonedByCheckFailure, Err: errors.New("pgrep: fork failed")}, key},
		{"last held by with", Meta{Mode: "with", LeaseKey: key, UDID: "u"}, expired, Poison{Reason: PoisonedByLiveConsumers}, key},
		{"different key", leaseMeta, expired, Poison{Reason: PoisonedByLiveConsumers}, "other-key"},
		{"keyless viewer", leaseMeta, expired, Poison{Reason: PoisonedByLiveConsumers}, ""},
		{"live lease of another key, meta says ours", leaseMeta, Lease{Key: "other-key", ExpiresAt: time.Now().Add(time.Hour)}, Poison{Reason: PoisonedByLiveConsumers}, key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ownLeaseResidue(tc.meta, tc.lease, tc.poison, tc.key) {
				t.Fatal("this case must never be exempt from quarantine")
			}
		})
	}
}

// TestAcquireSlots_NeverExemptsLeaseResidue proves the exemption is
// scoped to the lease path alone: `with`/`acquire` hold no lease key of
// their own, so a slot carrying a lease session's live residue stays
// quarantined against them exactly as before — they would be a second,
// genuinely different consumer on that simulator.
func TestAcquireSlots_NeverExemptsLeaseResidue(t *testing.T) {
	root := t.TempDir()
	leaseResidueFixture(t, root, "owner-key", "simpool-test-udid-with-never-exempt")

	_, err := AcquireSlots(root, "TestDevice", "1.0", 1, 1, 0)
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("`with`/`acquire` against a lease session's residue at max=1: want ErrAtCapacity, got %v", err)
	}
	if !strings.Contains(err.Error(), SlotQuarantined.String()) {
		t.Errorf("the refusal should name the quarantine rather than assert \"all busy\", got:\n%s", err)
	}
}
