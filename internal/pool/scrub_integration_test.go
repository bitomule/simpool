package pool

import (
	"os"
	"testing"
)

// TestScrubDeviceIntegration exercises the scrub against a REAL simulator,
// which is why it is opt-in: `go test ./...` must never shell out to
// `xcrun simctl` (see internal/cli/integration_test.go for the same rule).
//
// Point it at a throwaway device — never a pool slot, which a scheduled
// reap or another agent may be about to claim:
//
//	udid=$(xcrun simctl create scrub-probe com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro com.apple.CoreSimulator.SimRuntime.iOS-26-3)
//	xcrun simctl boot "$udid" && sleep 60 && xcrun simctl shutdown "$udid"
//	SIMPOOL_SCRUB_TEST_UDID=$udid go test ./internal/pool -run ScrubDeviceIntegration -v
//	xcrun simctl delete "$udid"
//
// It asserts the property the feature rests on: whatever the plan says is
// reclaimable actually comes back, and the device is still there
// afterwards with nothing left to reclaim.
func TestScrubDeviceIntegration(t *testing.T) {
	udid := os.Getenv("SIMPOOL_SCRUB_TEST_UDID")
	if udid == "" {
		t.Skip("set SIMPOOL_SCRUB_TEST_UDID to a throwaway simulator to run this")
	}
	categories, err := ScrubCategories(DefaultScrubCategories)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := PlanScrub(udid, categories, DefaultScrubTimeout)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	t.Logf("planned %s", HumanBytes(planned))
	if planned == 0 {
		t.Skip("nothing to reclaim on this device; boot it and use it first")
	}

	reclaimed, err := ScrubDevice(udid, categories, DefaultScrubTimeout)
	if err != nil {
		t.Fatalf("scrubbing: %v", err)
	}
	t.Logf("reclaimed %s", HumanBytes(reclaimed))
	if reclaimed == 0 {
		t.Errorf("planned %s but reclaimed nothing", HumanBytes(planned))
	}

	remaining, err := PlanScrub(udid, categories, DefaultScrubTimeout)
	if err != nil {
		t.Fatalf("re-planning: %v", err)
	}
	t.Logf("still reclaimable %s", HumanBytes(remaining))
	if remaining >= planned {
		t.Errorf("still %s reclaimable after a scrub that planned %s — the scrub did not take", HumanBytes(remaining), HumanBytes(planned))
	}
}
