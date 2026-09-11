package pool

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
	"github.com/mobai-app/simslim"
)

func TestSlimEnabledDefaultsOn(t *testing.T) {
	t.Setenv(EnvSlim, "")
	if !SlimEnabled() {
		t.Fatal("slimming must be on unless explicitly disabled")
	}
	t.Setenv(EnvSlim, "0")
	if SlimEnabled() {
		t.Fatal("SIMPOOL_SLIM=0 must disable slimming")
	}
}

func TestSlimProfileRejectsUnknownCategory(t *testing.T) {
	t.Setenv(EnvSlimExcept, "storekit")
	_, err := SlimProfile()
	if err == nil {
		t.Fatal("an unknown category must be an error, not a silently fully slim simulator")
	}
	// `storekit` is a doctor FEATURE id, not a profile CATEGORY id — the
	// exact confusion a caller is most likely to hit, so the error has to
	// say which vocabulary it wanted.
	if !strings.Contains(err.Error(), "simslim category") {
		t.Errorf("error should name what it expected, got %q", err)
	}
}

func TestSlimProfileKeepsExceptedCategoryEnabled(t *testing.T) {
	t.Setenv(EnvSlimExcept, "store")
	t.Setenv(EnvSlimKeep, "com.apple.apsd")
	p, err := SlimProfile()
	if err != nil {
		t.Fatal(err)
	}
	desired := p.Desired()
	if len(desired) == 0 {
		t.Fatal("profile disables nothing at all")
	}
	if desired["com.apple.apsd"] {
		t.Error("a label named in SIMPOOL_SLIM_KEEP must stay enabled")
	}
	store, ok := simslim.CategoryByID("store")
	if !ok {
		t.Fatal("simslim no longer has a `store` category; this test's premise is gone")
	}
	for _, label := range store.Labels {
		if desired[label] {
			t.Errorf("%s belongs to the excepted `store` category and must stay enabled", label)
		}
	}
}

func TestSlimTimeoutOverride(t *testing.T) {
	t.Setenv(EnvSlimTimeout, "90s")
	if got := SlimTimeout(); got != 90*time.Second {
		t.Errorf("SlimTimeout() = %s, want 90s", got)
	}
	// Garbage must fall back rather than produce a zero budget, which
	// would fail every slim instantly.
	t.Setenv(EnvSlimTimeout, "not-a-duration")
	if got := SlimTimeout(); got != DefaultSlimTimeout {
		t.Errorf("SlimTimeout() = %s, want the default %s", got, DefaultSlimTimeout)
	}
}

func TestMaxSlotsFollowsSlimming(t *testing.T) {
	t.Setenv(EnvMaxSlots, "")
	t.Setenv(EnvSlim, "")
	if got := MaxSlotsPerGroup(); got != DefaultMaxSlotsPerGroupSlim {
		t.Errorf("a slim pool's default cap = %d, want %d", got, DefaultMaxSlotsPerGroupSlim)
	}
	t.Setenv(EnvSlim, "0")
	if got := MaxSlotsPerGroup(); got != DefaultMaxSlotsPerGroup {
		t.Errorf("a stock pool's default cap = %d, want %d", got, DefaultMaxSlotsPerGroup)
	}
	// An explicit cap always wins over either default: a machine whose
	// operator has measured its own headroom must not have that overridden
	// by whether slimming happens to be on.
	t.Setenv(EnvMaxSlots, "11")
	if got := MaxSlotsPerGroup(); got != 11 {
		t.Errorf("SIMPOOL_MAX_SLOTS=11 gave %d", got)
	}
}

func TestLiveProvisionDepsSlims(t *testing.T) {
	if liveProvisionDeps.slim == nil {
		t.Fatal("the live deps must slim; a nil there is silently a stock pool")
	}
}

// The slim step must never take an acquisition down with it: a slot that
// could not be slimmed is still a perfectly usable slot.
func TestEnsureProvisionedSurvivesSlimFailure(t *testing.T) {
	t.Setenv(EnvSlim, "")
	s := fakeSlot(t, "iPhone 17 Pro", "26.3", "")
	booted := false
	deps := provisionDeps{
		find:           func(string) (simctl.DeviceEntry, bool, error) { return simctl.DeviceEntry{}, false, nil },
		listDevices:    func() ([]simctl.DeviceEntry, error) { return nil, nil },
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         func(string, string, string) (string, error) { return "UDID-1", nil },
		bootAndWait:    func(string, time.Duration) error { booted = true; return nil },
		shutdown:       neverShutdown(t),
		delete:         neverDelete(t),
		slim:           func(string, time.Duration) (bool, error) { return false, errors.New("launchctl said no") },
	}
	if err := ensureProvisioned(s, "test", "with", "", deps, time.Minute); err != nil {
		t.Fatalf("a failed slim must not fail provisioning: %v", err)
	}
	if !booted {
		t.Error("the device must still be booted and handed out")
	}
}

func TestEnsureProvisionedSkipsSlimWhenDisabled(t *testing.T) {
	t.Setenv(EnvSlim, "0")
	s := fakeSlot(t, "iPhone 17 Pro", "26.3", "")
	slimCalled := false
	deps := provisionDeps{
		find:           func(string) (simctl.DeviceEntry, bool, error) { return simctl.DeviceEntry{}, false, nil },
		listDevices:    func() ([]simctl.DeviceEntry, error) { return nil, nil },
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         func(string, string, string) (string, error) { return "UDID-1", nil },
		bootAndWait:    func(string, time.Duration) error { return nil },
		shutdown:       neverShutdown(t),
		delete:         neverDelete(t),
		slim:           func(string, time.Duration) (bool, error) { slimCalled = true; return true, nil },
	}
	if err := ensureProvisioned(s, "test", "with", "", deps, time.Minute); err != nil {
		t.Fatal(err)
	}
	if slimCalled {
		t.Error("SIMPOOL_SLIM=0 must not slim anything")
	}
}

func TestScrubCategoriesRejectsUnknown(t *testing.T) {
	if _, err := ScrubCategories("caches,not-a-category"); err == nil {
		t.Fatal("an unknown cleanup category must be rejected before any slot is walked")
	}
	if _, err := ScrubCategories(""); err == nil {
		t.Fatal("an empty selection must be rejected rather than scrubbing nothing silently")
	}
	got, err := ScrubCategories(DefaultScrubCategories)
	if err != nil {
		t.Fatalf("the default selection must be valid: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("default categories = %v, want three", got)
	}
}

// The scrub must never be pointed at a category that holds anything a slot
// cannot regenerate on its own. Installed apps, documents and user media
// are stored data, not cleanup categories, and simslim must keep refusing
// them for this feature to stay safe.
func TestScrubNeverAcceptsStoredData(t *testing.T) {
	for _, id := range []string{"installed-apps", "documents", "app-data", "user-media"} {
		if _, err := ScrubCategories(id); err == nil {
			t.Errorf("%q was accepted as a cleanup category", id)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1_500_000, "1.5 MB"},
		{1_900_000_000, "1.9 GB"},
	} {
		if got := HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
