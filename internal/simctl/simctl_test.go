package simctl

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fixtureDevicesJSON is a trimmed `simctl list devices -j` shape: 3 devices
// across 2 runtime buckets, covering a pool-owned device, an unavailable
// device (the drift scenario the substance-verification item needs this
// package to expose), and a plain user device under a different runtime.
const fixtureDevicesJSON = `{
  "devices": {
    "com.apple.CoreSimulator.SimRuntime.iOS-26-3": [
      {
        "udid": "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
        "name": "SIMPOOL_dbb2dfc3_iPhone-17-Pro@26.3_slot-0",
        "state": "Booted",
        "isAvailable": true,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"
      },
      {
        "udid": "BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB",
        "name": "iPhone 17 Pro",
        "state": "Shutdown",
        "isAvailable": false,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"
      }
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-26-0": [
      {
        "udid": "CCCCCCCC-CCCC-CCCC-CCCC-CCCCCCCCCCCC",
        "name": "iPhone 16",
        "state": "Shutdown",
        "isAvailable": true,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16"
      }
    ]
  }
}`

// withFakeRunContext points runContext at fn for the duration of the test
// and restores the real one on cleanup.
func withFakeRunContext(t *testing.T, fn func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	orig := runContext
	runContext = fn
	t.Cleanup(func() { runContext = orig })
}

func TestListDevices_PopulatesRuntimeIDAvailabilityAndDeviceType(t *testing.T) {
	withFakeRunContext(t, func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(fixtureDevicesJSON), nil
	})

	devices, err := ListDevices()
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("want 3 devices, got %d", len(devices))
	}

	byUDID := make(map[string]DeviceEntry, len(devices))
	for _, d := range devices {
		byUDID[d.UDID] = d
	}

	pool0, ok := byUDID["AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"]
	if !ok {
		t.Fatal("missing pool device entry")
	}
	if pool0.RuntimeID != "com.apple.CoreSimulator.SimRuntime.iOS-26-3" {
		t.Errorf("pool0.RuntimeID = %q", pool0.RuntimeID)
	}
	if !pool0.IsAvailable {
		t.Error("pool0.IsAvailable = false, want true")
	}
	if pool0.DeviceTypeID != "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro" {
		t.Errorf("pool0.DeviceTypeID = %q", pool0.DeviceTypeID)
	}

	unavailable, ok := byUDID["BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB"]
	if !ok {
		t.Fatal("missing unavailable device entry")
	}
	if unavailable.IsAvailable {
		t.Error("unavailable.IsAvailable = true, want false")
	}
	// Same runtime bucket as pool0 — proves the runtime key is applied per
	// entry within a bucket, not just to the first one.
	if unavailable.RuntimeID != "com.apple.CoreSimulator.SimRuntime.iOS-26-3" {
		t.Errorf("unavailable.RuntimeID = %q", unavailable.RuntimeID)
	}

	other, ok := byUDID["CCCCCCCC-CCCC-CCCC-CCCC-CCCCCCCCCCCC"]
	if !ok {
		t.Fatal("missing other-runtime device entry")
	}
	if other.RuntimeID != "com.apple.CoreSimulator.SimRuntime.iOS-26-0" {
		t.Errorf("other.RuntimeID = %q, want the iOS-26-0 bucket key, proving the map key (not just bucket order) drives RuntimeID", other.RuntimeID)
	}
}

// TestRun_TimesOutInsteadOfHanging proves a wedged simctl call can never
// hang `simpool lease` — it must fail fast with a distinguishable error
// instead. No real `xcrun` is invoked: runContext is redirected to `sleep 5`
// so the test itself never depends on CoreSimulator being slow or fast.
func TestRun_TimesOutInsteadOfHanging(t *testing.T) {
	withFakeRunContext(t, func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "/bin/sleep", "5")
		return cmd.Output()
	})
	t.Setenv(EnvSimctlTimeout, "100ms")

	start := time.Now()
	_, err := run("list", "devices", "-j")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("run() succeeded against a call that should have timed out")
	}
	if !errors.Is(err, ErrSimctlTimeout) {
		t.Fatalf("want errors.Is(err, ErrSimctlTimeout), got: %v", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("run() did not return before the underlying sleep would have finished (took %s) — a wedged call is not being bounded", elapsed)
	}
	if !strings.Contains(err.Error(), "simctl list devices -j") {
		t.Fatalf("error should name the command that timed out, got: %v", err)
	}
}

// TestParseSpringBoardReady covers the exact output shapes captured from a
// real device: a running SpringBoard reports a real pid in the first field,
// a known-but-not-running one reports "-", and launchctl reporting nothing
// at all (device not reachable yet) must read as not ready too — never as
// ready by default.
func TestParseSpringBoardReady(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"running", "29719\t0\tcom.apple.SpringBoard\n", true},
		{"not running", "-\t0\tcom.apple.SpringBoard\n", false},
		{"absent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSpringBoardReady([]byte(tc.output)); got != tc.want {
				t.Errorf("parseSpringBoardReady(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestSimctlTimeout_IgnoresMalformedOverride proves a bad
// SIMPOOL_SIMCTL_TIMEOUT value falls back to the default instead of
// disabling the timeout altogether (an unbounded timeout is worse than a
// wrong one).
func TestSimctlTimeout_IgnoresMalformedOverride(t *testing.T) {
	t.Setenv(EnvSimctlTimeout, "not-a-duration")
	if got := simctlTimeout(); got != DefaultSimctlTimeout {
		t.Fatalf("simctlTimeout() = %s, want default %s for a malformed override", got, DefaultSimctlTimeout)
	}

	t.Setenv(EnvSimctlTimeout, "-5s")
	if got := simctlTimeout(); got != DefaultSimctlTimeout {
		t.Fatalf("simctlTimeout() = %s, want default %s for a non-positive override", got, DefaultSimctlTimeout)
	}
}
