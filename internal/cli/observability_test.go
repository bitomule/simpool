package cli

import (
	"bytes"
	"os"
	"testing"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// TestReportForeignRootDevices_FlagsOnlyForeignTaggedPoolDevices proves the
// scan reports a pool-named device belonging to a different pool root's
// tag, leaves a device belonging to THIS root's own tag alone, and never
// mentions a non-pool-named device at all.
func TestReportForeignRootDevices_FlagsOnlyForeignTaggedPoolDevices(t *testing.T) {
	root := t.TempDir()
	ownTag := pool.RootTag(root)

	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-own", Name: "SIMPOOL_" + ownTag + "_TestDevice@1.0_slot-0"},
		{UDID: "udid-foreign", Name: "SIMPOOL_deadbeef_TestDevice@1.0_slot-3"},
		{UDID: "udid-not-pool", Name: "My Own iPhone"},
	})

	var stderr bytes.Buffer
	reportForeignRootDevices(root, &stderr)

	out := stderr.String()
	if bytes.Contains([]byte(out), []byte("udid-own")) {
		t.Errorf("a device belonging to this root's own tag must never be reported, got:\n%s", out)
	}
	if bytes.Contains([]byte(out), []byte("udid-not-pool")) {
		t.Errorf("a non-pool-named device must never be reported, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("udid-foreign")) {
		t.Errorf("a device tagged for a different pool root should be reported, got:\n%s", out)
	}
}

// TestReportForeignRootDevices_SilentWhenNoneFound proves this never prints
// anything when there is nothing to report — the diagnostic line is only
// noise-worthy signal, never a routine status line.
func TestReportForeignRootDevices_SilentWhenNoneFound(t *testing.T) {
	root := t.TempDir()
	ownTag := pool.RootTag(root)
	withFakeDevices(t, []simctl.DeviceEntry{
		{UDID: "udid-own", Name: "SIMPOOL_" + ownTag + "_TestDevice@1.0_slot-0"},
	})

	var stderr bytes.Buffer
	reportForeignRootDevices(root, &stderr)

	if stderr.Len() != 0 {
		t.Errorf("expected no output when every pool-named device belongs to this root, got:\n%s", stderr.String())
	}
}

// TestAcquireAndProvision_ReportsPoolRootOnStderr proves the acquisition
// path used by `with`/`acquire` always names the resolved pool root on
// stderr, even when it fails fast at capacity — the whole point being that
// a slow or wrong acquisition is diagnosable from a log without re-running
// anything.
func TestAcquireAndProvision_ReportsPoolRootOnStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	withFakeDevices(t, nil)

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

	var stderr bytes.Buffer
	_, _, err = acquireAndProvision("TestDevice", "1.0", 1, 1, 0, "test-owner", "acquire", &stderr)
	if err == nil {
		t.Fatal("expected acquireAndProvision to fail at capacity with the one slot already held")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("pool root "+home)) {
		t.Errorf("expected the resolved pool root on stderr, got:\n%s", stderr.String())
	}
}
