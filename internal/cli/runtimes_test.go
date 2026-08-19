package cli

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

type killRecorder struct {
	mu   sync.Mutex
	pids []int
}

func (k *killRecorder) record(pid int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.pids = append(k.pids, pid)
	return nil
}

func (k *killRecorder) killed() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int(nil), k.pids...)
}

func withFakeRuntimes(t *testing.T, found []procs.SimRuntimeProcess, err error) {
	t.Helper()
	orig := simRuntimeSnapshot
	t.Cleanup(func() { simRuntimeSnapshot = orig })
	simRuntimeSnapshot = func() ([]procs.SimRuntimeProcess, error) { return found, err }
}

func withKillRecorder(t *testing.T) *killRecorder {
	t.Helper()
	rec := &killRecorder{}
	orig := killRuntimeProc
	t.Cleanup(func() { killRuntimeProc = orig })
	killRuntimeProc = rec.record
	return rec
}

// withFailingDeviceList returns devices AND an error, which is the failure
// mode that actually matters: simctl printing part of its output before
// dying leaves a listing that looks perfectly usable. A fake returning
// (nil, err) would be caught by the separate empty-listing guard, so it
// could not tell whether the error itself was ever honoured.
func withFailingDeviceList(t *testing.T, partial []simctl.DeviceEntry, err error) {
	t.Helper()
	orig := listPoolDevices
	t.Cleanup(func() { listPoolDevices = orig })
	listPoolDevices = func() ([]simctl.DeviceEntry, error) { return partial, err }
}

const (
	liveUDID = "3A8339E4-1A6D-435F-B4AC-47599DDA6587"
	deadUDID = "DD66B266-011D-4D8B-B46E-DDFAC60169FD"
)

// The whole feature rests on this: a process is swept only because its
// device is ABSENT from a listing that worked. If the listing itself failed,
// every simulator on the machine looks deleted.
func TestReapOrphanRuntimes_DeviceListingFailureKillsNothing(t *testing.T) {
	withFailingDeviceList(t,
		[]simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}},
		errors.New("simctl: connection interrupted"))
	withFakeRuntimes(t, []procs.SimRuntimeProcess{{PID: 4318, UDID: deadUDID}}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, false, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v after a failed device listing; nothing may be killed on an unverifiable listing", got)
	}
	if !strings.Contains(stderr.String(), "not touching any process") {
		t.Fatalf("stderr = %q, want it to say nothing was touched", stderr.String())
	}
}

// An empty listing is not proof that every device was deleted — it is
// indistinguishable from a listing that reported nothing because it could
// not. Same rule the poison checks already apply.
func TestReapOrphanRuntimes_EmptyDeviceListingKillsNothing(t *testing.T) {
	withFakeDevices(t, nil)
	withFakeRuntimes(t, []procs.SimRuntimeProcess{{PID: 4318, UDID: deadUDID}}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, false, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v on an empty device listing; empty is unverifiable, not proof of deletion", got)
	}
}

// A live device's userland is the healthy case. Sweeping it would kill the
// simulator someone is using right now.
func TestReapOrphanRuntimes_NeverTouchesLiveDevice(t *testing.T) {
	withFakeDevices(t, []simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}})
	withFakeRuntimes(t, []procs.SimRuntimeProcess{
		{PID: 3910, UDID: liveUDID},
		{PID: 3952, UDID: liveUDID},
	}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, false, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v belonging to a device present in the listing", got)
	}
}

// Reporting has to happen without being asked — this accumulated unnoticed
// for 18 days precisely because nothing ever mentioned it — while killing
// still has to be asked for.
func TestReapOrphanRuntimes_ReportsWithoutPurging(t *testing.T) {
	withFakeDevices(t, []simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}})
	withFakeRuntimes(t, []procs.SimRuntimeProcess{
		{PID: 4318, UDID: deadUDID},
		{PID: 4319, UDID: deadUDID},
	}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(false, false, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v without --purge-orphan-runtimes", got)
	}
	out := stdout.String()
	if !strings.Contains(out, deadUDID) || !strings.Contains(out, "--purge-orphan-runtimes") {
		t.Fatalf("stdout = %q, want the dead device reported with the flag that kills it", out)
	}
}

func TestReapOrphanRuntimes_DryRunKillsNothing(t *testing.T) {
	withFakeDevices(t, []simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}})
	withFakeRuntimes(t, []procs.SimRuntimeProcess{{PID: 4318, UDID: deadUDID}}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, true, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v during --dry-run", got)
	}
	if !strings.Contains(stdout.String(), "would kill") {
		t.Fatalf("stdout = %q, want a dry-run preview", stdout.String())
	}
}

func TestReapOrphanRuntimes_PurgeKillsOnlyTheDeadDevicesProcesses(t *testing.T) {
	withFakeDevices(t, []simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}})
	withFakeRuntimes(t, []procs.SimRuntimeProcess{
		{PID: 3952, UDID: liveUDID},
		{PID: 4400, UDID: deadUDID},
		{PID: 4318, UDID: deadUDID},
	}, nil)
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, false, &stdout, &stderr)

	got := rec.killed()
	want := []int{4318, 4400}
	sorted := append([]int(nil), got...)
	sort.Ints(sorted)
	if len(got) != len(want) {
		t.Fatalf("killed %v, want exactly %v", got, want)
	}
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("killed %v, want exactly %v", got, want)
		}
	}
	// launchd_sim is the lowest pid of the tree it started and is the only
	// member able to spawn more; signalling it first is what stops it
	// re-populating the leaves mid-sweep.
	if got[0] != 4318 {
		t.Fatalf("killed in order %v, want the lowest pid (the tree's leader) first", got)
	}
}

// A snapshot that could not be taken is not an empty machine.
func TestReapOrphanRuntimes_SnapshotFailureKillsNothing(t *testing.T) {
	withFakeDevices(t, []simctl.DeviceEntry{{UDID: liveUDID, Name: "SIMPOOL_x_iPhone@26.3_slot-0", State: "Booted"}})
	// Partial output plus an error, for the same reason as the device
	// listing above: a fake returning nothing would be indistinguishable
	// from a machine that simply has no simulator processes.
	withFakeRuntimes(t, []procs.SimRuntimeProcess{{PID: 4318, UDID: deadUDID}},
		errors.New("fork: resource temporarily unavailable"))
	rec := withKillRecorder(t)

	var stdout, stderr bytes.Buffer
	reapOrphanRuntimes(true, false, &stdout, &stderr)

	if got := rec.killed(); len(got) != 0 {
		t.Fatalf("killed %v after the process snapshot failed", got)
	}
	if !strings.Contains(stderr.String(), "not touching any process") {
		t.Fatalf("stderr = %q, want it to say nothing was touched", stderr.String())
	}
}
