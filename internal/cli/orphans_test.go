package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// withFakeDevices overrides listPoolDevices for the duration of the test,
// restoring it on cleanup.
func withFakeDevices(t *testing.T, devices []simctl.DeviceEntry) {
	t.Helper()
	orig := listPoolDevices
	t.Cleanup(func() { listPoolDevices = orig })
	listPoolDevices = func() ([]simctl.DeviceEntry, error) {
		return devices, nil
	}
}

// TestReapOrphans_ReportsOnlyUnreferencedPoolNamedDevices proves the scan
// distinguishes a device a real slot still references (by name, derived
// from the slot's own directory — never from meta.json) from one nothing
// references anymore, and leaves the user's own, non-pool-named
// simulators alone entirely.
func TestReapOrphans_ReportsOnlyUnreferencedPoolNamedDevices(t *testing.T) {
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	if err := os.MkdirAll(pool.SlotDir(groupDir, 0), 0o755); err != nil {
		t.Fatal(err)
	}
	group := filepath.Base(groupDir)
	referencedName := pool.DeviceNameForGroup(root, group, 0)

	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-referenced", Name: referencedName},
		{UDID: "udid-orphan", Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-7"},
		{UDID: "udid-not-mine", Name: "My Own iPhone"},
	})

	var stdout, stderr bytes.Buffer
	reapOrphans(root, false /*purge*/, false /*dryRun*/, &stdout, &stderr)

	out := stdout.String()
	if bytes.Contains([]byte(out), []byte("udid-referenced")) {
		t.Errorf("a device a real slot references must never be reported, got:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("udid-not-mine")) {
		t.Errorf("a non-pool-named device must never be reported, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("udid-orphan")) {
		t.Errorf("the unreferenced pool-named device should be reported ORPHAN, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("ORPHAN")) {
		t.Errorf("expected an ORPHAN report line, got:\n%s", out)
	}
}

// TestReapOrphans_ReportOnlyNeverDeletesWithoutPurge proves --orphans by
// itself is purely read-only: shutdownOrphan/deleteOrphan must never be
// called unless --purge-orphans was explicitly given.
func TestReapOrphans_ReportOnlyNeverDeletesWithoutPurge(t *testing.T) {
	root := t.TempDir()
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-orphan", Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-7"},
	})

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var shutdownCalled, deleteCalled bool
	shutdownOrphan = func(udid string) error { shutdownCalled = true; return nil }
	deleteOrphan = func(udid string) error { deleteCalled = true; return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, false /*purge*/, false /*dryRun*/, &stdout, &stderr)

	if shutdownCalled || deleteCalled {
		t.Fatal("reapOrphans without --purge-orphans must never shut down or delete anything")
	}
}

// TestReapOrphans_PurgeSkipsDeviceWithLiveConsumer proves the deletion path
// re-verifies liveness right before acting and fails safe: a device a live
// process still references must survive even under --purge-orphans. Mirrors
// procs_test.go's own pattern: a real subprocess whose argv contains the
// UDID, standing in for a genuine orphan consumer (e.g. a forgotten
// `simctl spawn <udid> log stream`).
func TestReapOrphans_PurgeSkipsDeviceWithLiveConsumer(t *testing.T) {
	root := t.TempDir()
	udid := "simpool-test-orphan-udid-" + strconv.Itoa(os.Getpid())

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "log_stream_consumer.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	consumer := exec.Command(scriptPath, udid)
	if err := consumer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(consumer.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if matches, _ := procs.MatchingPIDs(udid); len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake orphan consumer never showed up as a live process referencing the UDID")
		}
		time.Sleep(50 * time.Millisecond)
	}

	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: udid, Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-7"},
	})

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var shutdownCalled bool
	shutdownOrphan = func(string) error { shutdownCalled = true; return nil }
	deleteOrphan = func(string) error { return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, true /*purge*/, false /*dryRun*/, &stdout, &stderr)

	if shutdownCalled {
		t.Fatalf("a device with a live consumer must never be shut down/deleted, got stdout:\n%s", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("SKIP")) {
		t.Errorf("expected a SKIP report for the still-referenced device, got:\n%s", stdout.String())
	}
}

// TestReapOrphans_PurgeDryRunNeverActs proves --dry-run still previews the
// PURGE decision without ever calling shutdownOrphan/deleteOrphan.
func TestReapOrphans_PurgeDryRunNeverActs(t *testing.T) {
	root := t.TempDir()
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-orphan-dry-run", Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-7"},
	})

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var called bool
	shutdownOrphan = func(string) error { called = true; return nil }
	deleteOrphan = func(string) error { called = true; return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, true /*purge*/, true /*dryRun*/, &stdout, &stderr)

	if called {
		t.Fatal("--dry-run must never actually shut down or delete an orphan")
	}
	if !bytes.Contains(stdout.Bytes(), []byte("PURGE")) {
		t.Errorf("--dry-run should still report what it would purge, got:\n%s", stdout.String())
	}
}

// TestReapOrphans_PurgeDeletesVerifiedOrphan proves the full happy path: no
// live consumer, --purge-orphans set, not dry-run — shutdown then delete
// both run, in that order.
func TestReapOrphans_PurgeDeletesVerifiedOrphan(t *testing.T) {
	root := t.TempDir()
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-orphan-real-purge", Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-7"},
	})

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var order []string
	shutdownOrphan = func(udid string) error { order = append(order, "shutdown:"+udid); return nil }
	deleteOrphan = func(udid string) error { order = append(order, "delete:"+udid); return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, true /*purge*/, false /*dryRun*/, &stdout, &stderr)

	want := []string{"shutdown:udid-orphan-real-purge", "delete:udid-orphan-real-purge"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("got call order %v, want %v", order, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr:\n%s", stderr.String())
	}
}

// TestReapOrphans_UnreadableGroupListingAbortsPurge proves that when a
// group's slot listing cannot be read (EACCES, EMFILE, ...), reapOrphans
// treats that exactly like every other "couldn't verify" case in this
// codebase — as still-referenced, not as confirmed-free — and aborts the
// whole pass rather than purge against a `known` set that might be missing
// that group's live slot. Before this fix, ListSlotNumbers silently
// swallowed the read error into an empty slice, so the live slot's device
// looked unreferenced and got deleted out from under it.
func TestReapOrphans_UnreadableGroupListingAbortsPurge(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses directory permission bits")
	}
	root := t.TempDir()
	groupDir := pool.GroupDir(root, "TestDevice", "1.0")
	if err := os.MkdirAll(pool.SlotDir(groupDir, 0), 0o755); err != nil {
		t.Fatal(err)
	}
	group := filepath.Base(groupDir)
	liveName := pool.DeviceNameForGroup(root, group, 0)

	if err := os.Chmod(groupDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(groupDir, 0o755) })

	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-live-slot-0", Name: liveName},
	})

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var acted bool
	shutdownOrphan = func(string) error { acted = true; return nil }
	deleteOrphan = func(string) error { acted = true; return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, true /*purge*/, false /*dryRun*/, &stdout, &stderr)

	if acted {
		t.Fatal("an unreadable group listing must never lead to a purge — the live slot's device could be the one that was missed")
	}
	if stderr.Len() == 0 {
		t.Error("expected a loud stderr message explaining the abort")
	}
}

// TestReapOrphans_GroupCreatedDuringDeviceScanIsNotPurged proves reapOrphans
// re-lists groups and slots strictly after its own listPoolDevices() call,
// not from a slice snapshotted before it — a group/slot created in that
// window (e.g. mav leasing a brand-new device+OS pair while a slow reap
// pass is still running) must never be purged as an orphan.
func TestReapOrphans_GroupCreatedDuringDeviceScanIsNotPurged(t *testing.T) {
	root := t.TempDir()
	device, osVersion := "NewDevice", "2.0"
	group := pool.GroupName(device, osVersion)
	freshName := pool.DeviceNameForGroup(root, group, 0)

	orig := listPoolDevices
	t.Cleanup(func() { listPoolDevices = orig })
	listPoolDevices = func() ([]simctl.DeviceEntry, error) {
		// Simulate a lease created concurrently with (and discovered only
		// after) this device scan: the group/slot did not exist yet when
		// callers snapshot group directories before a scan like this one.
		if err := os.MkdirAll(pool.SlotDir(pool.GroupDir(root, device, osVersion), 0), 0o755); err != nil {
			return nil, err
		}
		return []simctl.DeviceEntry{{UDID: "udid-freshly-leased", Name: freshName}}, nil
	}

	origShutdown, origDelete := shutdownOrphan, deleteOrphan
	t.Cleanup(func() { shutdownOrphan, deleteOrphan = origShutdown, origDelete })
	var acted bool
	shutdownOrphan = func(string) error { acted = true; return nil }
	deleteOrphan = func(string) error { acted = true; return nil }

	var stdout, stderr bytes.Buffer
	reapOrphans(root, true /*purge*/, false /*dryRun*/, &stdout, &stderr)

	if acted {
		t.Fatalf("a slot created during the device scan must count as referenced, not be purged; stdout:\n%s", stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("PURGE")) || bytes.Contains(stdout.Bytes(), []byte("ORPHAN")) {
		t.Errorf("the freshly-leased device must not be reported as an orphan at all, got:\n%s", stdout.String())
	}
}
