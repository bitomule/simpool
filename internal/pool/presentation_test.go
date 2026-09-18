package pool

import (
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// cleanSlot is a simulator sitting in the baseline this test file uses
// everywhere, so a test only has to say what is WRONG with a slot.
func cleanSlot() Presentation {
	return Presentation{
		Language:         "es-ES",
		Region:           "es_ES",
		Force24Hour:      "",
		Appearance:       "light",
		ContentSize:      "large",
		IncreaseContrast: "disabled",
	}
}

// fakeSim is a recording stand-in for one simulator's presentation surface:
// it answers reads from a Presentation and records every write, so a test
// can assert on what was NOT done as easily as on what was.
type fakeSim struct {
	mu    sync.Mutex
	state Presentation
	// readErr, when set for a key, makes that read fail.
	readErr map[string]error

	writes         []string
	uiSets         []string
	cleared        int
	restarts       int
	restartErr     error
	missing24Hour  bool
	deleted24Hour  bool
	wroteLanguages string
}

func newFakeSim(p Presentation) *fakeSim {
	return &fakeSim{state: p, readErr: map[string]error{}, missing24Hour: p.Force24Hour == ""}
}

func (f *fakeSim) deps() presentationDeps {
	return presentationDeps{
		defaultsRead: func(_, key string, _ time.Duration) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if err := f.readErr[key]; err != nil {
				return "", err
			}
			switch key {
			case "AppleLanguages":
				return "(\n    \"" + f.state.Language + "\",\n    \"en-ES\"\n)", nil
			case "AppleLocale":
				return f.state.Region, nil
			case "AppleICUForce24HourTime":
				if f.missing24Hour {
					return "", errors.New("the domain/default pair does not exist")
				}
				return f.state.Force24Hour, nil
			}
			return "", errors.New("unexpected key " + key)
		},
		defaultsWrite: func(_, key, value string, array bool, _ time.Duration) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.writes = append(f.writes, key+"="+value)
			if key == "AppleLanguages" && array {
				f.wroteLanguages = value
			}
			return nil
		},
		defaultsDelete: func(_, key string, _ time.Duration) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if key == "AppleICUForce24HourTime" {
				f.deleted24Hour = true
			}
			return nil
		},
		ui: func(_, option, value string, _ time.Duration) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if value != "" {
				f.uiSets = append(f.uiSets, option+"="+value)
				return "", nil
			}
			if err := f.readErr[option]; err != nil {
				return "", err
			}
			switch option {
			case "appearance":
				return f.state.Appearance, nil
			case "content_size":
				return f.state.ContentSize, nil
			case "increase_contrast":
				return f.state.IncreaseContrast, nil
			}
			return "", errors.New("unexpected ui option " + option)
		},
		statusBarOverridden: func(string, time.Duration) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.state.StatusBarOverridden, nil
		},
		statusBarClear: func(string, time.Duration) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.cleared++
			return nil
		},
		restartSpringBoard: func(string, time.Duration) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.restarts++
			return f.restartErr
		},
	}
}

func (f *fakeSim) sortedWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.writes...)
	sort.Strings(out)
	return out
}

// The baseline compares the HEAD of AppleLanguages, never the whole array.
// iOS appends its own fallbacks to whatever is written, so an array
// comparison would find every slot dirty forever and rewrite it — plus a
// SpringBoard restart — on every single acquisition.
func TestPresentationComparesOnlyThePrimaryLanguage(t *testing.T) {
	sim := newFakeSim(cleanSlot())
	changed, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("a slot whose AppleLanguages is (es-ES, en-ES) is clean, got drift %v", changed)
	}
	if got := sim.sortedWrites(); len(got) != 0 {
		t.Errorf("a clean slot must not be written to, got %v", got)
	}
	if sim.restarts != 0 {
		t.Errorf("a clean slot must not pay a SpringBoard restart, got %d", sim.restarts)
	}
}

// The defect exactly as it was found: a slot left in German by a screenshot
// run, handed back to the pool with nothing saying so.
func TestRestorePutsAGermanSlotBackToTheBaseline(t *testing.T) {
	dirty := cleanSlot()
	dirty.Language = "de-DE"
	dirty.Region = "de_DE"
	sim := newFakeSim(dirty)

	changed, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	want := []string{"AppleLanguages=es-ES", "AppleLocale=es_ES"}
	if got := sim.sortedWrites(); !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	if sim.wroteLanguages != "es-ES" {
		t.Errorf("AppleLanguages must be written as a one-element array of the baseline, got %q", sim.wroteLanguages)
	}
	if sim.restarts != 1 {
		t.Errorf("a language change must restart SpringBoard exactly once, got %d", sim.restarts)
	}
	joined := strings.Join(changed, "; ")
	if !strings.Contains(joined, "language de-DE→es-ES") || !strings.Contains(joined, "region de_DE→es_ES") {
		t.Errorf("the reported drift must name both settings and both values, got %q", joined)
	}
}

// `simctl ui` settings apply live. Restarting SpringBoard for them would
// charge ~6s to a slot whose only sin was being left in dark mode.
func TestRestoreDoesNotRestartSpringBoardForUIOnlyDrift(t *testing.T) {
	dirty := cleanSlot()
	dirty.Appearance = "dark"
	dirty.ContentSize = "extra-extra-large"
	dirty.IncreaseContrast = "enabled"
	sim := newFakeSim(dirty)

	if _, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sim.restarts != 0 {
		t.Errorf("appearance/text size/contrast apply live; SpringBoard must not be restarted, got %d", sim.restarts)
	}
	want := []string{"appearance=light", "content_size=large", "increase_contrast=disabled"}
	if !reflect.DeepEqual(sim.uiSets, want) {
		t.Errorf("ui sets = %v, want %v", sim.uiSets, want)
	}
}

// "Absent" and "0" are different states of AppleICUForce24HourTime: absent
// follows the region, 0 forces a 12-hour clock whatever the region says.
// Three states, not two, so the clean one is restored by deleting the key —
// writing a 0 for "clean" would leave the slot permanently wrong.
func TestRestoreDeletesAnExplicit24HourSettingRatherThanWritingZero(t *testing.T) {
	dirty := cleanSlot()
	dirty.Force24Hour = "0"
	sim := newFakeSim(dirty)
	sim.missing24Hour = false

	changed, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !sim.deleted24Hour {
		t.Error("restoring the absent state must delete the key, not write a value into it")
	}
	if got := sim.sortedWrites(); len(got) != 0 {
		t.Errorf("nothing should be written for a delete, got %v", got)
	}
	if sim.restarts != 1 {
		t.Errorf("a clock-format change needs SpringBoard restarted, got %d", sim.restarts)
	}
	if !strings.Contains(strings.Join(changed, "; "), "24-hour clock 0→(unset)") {
		t.Errorf("drift must name the absent state as such, got %v", changed)
	}
}

// The 9:41 clock and the full battery bar every screenshot pipeline sets.
// They do not survive a reboot, but pool slots stay booted for days, so
// between two consumers of a warm slot they are as sticky as the rest.
func TestRestoreClearsStatusBarOverrides(t *testing.T) {
	dirty := cleanSlot()
	dirty.StatusBarOverridden = true
	sim := newFakeSim(dirty)

	changed, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sim.cleared != 1 {
		t.Errorf("status bar overrides must be cleared once, got %d", sim.cleared)
	}
	if !strings.Contains(strings.Join(changed, "; "), "status bar") {
		t.Errorf("drift must mention the status bar, got %v", changed)
	}
}

// A read that failed is not evidence that a slot is clean. Treating it as
// clean would make every simctl hiccup silently hand out a dirty slot —
// the same "an incomplete check must never read as confirmed-free" rule
// CheckPoison's PoisonedByCheckFailure exists for.
func TestRestoreRefusesToActOnAFailedRead(t *testing.T) {
	sim := newFakeSim(cleanSlot())
	sim.readErr["appearance"] = errors.New("simctl ui: device not booted")

	changed, err := restorePresentation("UDID", cleanSlot(), time.Second, sim.deps())
	if err == nil {
		t.Fatal("a failed read must be reported, not swallowed")
	}
	if changed != nil {
		t.Errorf("nothing may be claimed as changed when the state is unknown, got %v", changed)
	}
	if len(sim.sortedWrites()) != 0 || len(sim.uiSets) != 0 || sim.restarts != 0 {
		t.Error("nothing may be written when the current state could not be read")
	}
}

func TestFirstPlistString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"(\n    \"es-ES\",\n    \"en-ES\"\n)", "es-ES"},
		{"(\n    es\n)", "es"},
		{"es_ES", "es_ES"},
		{"\"es_ES\"", "es_ES"},
		{"", ""},
		{"(\n)", ""},
	} {
		if got := firstPlistString(tc.in); got != tc.want {
			t.Errorf("firstPlistString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBaselineHonoursTheEnvironmentOverrides(t *testing.T) {
	// BaselinePresentation memoises, so exercise the resolution rule
	// itself rather than the cached value.
	t.Setenv(EnvLanguage, "en-US")
	t.Setenv(EnvRegion, "en_US")
	got := Presentation{
		Language: firstNonEmpty(os.Getenv(EnvLanguage), "es-ES"),
		Region:   firstNonEmpty(os.Getenv(EnvRegion), "es_ES"),
	}
	if got.Language != "en-US" || got.Region != "en_US" {
		t.Errorf("the overrides must win over the host, got %+v", got)
	}
}

func TestRestoreIsOnByDefaultAndDisablableWithOneVariable(t *testing.T) {
	t.Setenv(EnvRestore, "")
	if !RestoreEnabled() {
		t.Error("a pool that does not restore its slots is the defect, so this must default to on")
	}
	t.Setenv(EnvRestore, "0")
	if RestoreEnabled() {
		t.Error("SIMPOOL_RESTORE=0 must turn it off")
	}
}

func TestLiveProvisionDepsRestore(t *testing.T) {
	if liveProvisionDeps.restore == nil {
		t.Fatal("a nil restore in the live deps is silently the bug this exists to fix")
	}
}

// The hand-out reconcile: it runs when a slot changes hands, and it must
// NOT run on a sticky lease renewal. A renewal is the same consumer coming
// back for its next command — `mav sim language set de-DE` followed by
// `mav screenshot`, each of which goes through its own `simpool lease` —
// so a reconcile there would put the slot back into Spanish between the two
// and a fourteen-language matrix would silently shoot fourteen Spanish
// captures.
func TestEnsureProvisionedRestoresOnlyWhenTheSlotChangesHands(t *testing.T) {
	for _, tc := range []struct {
		name        string
		renewed     bool
		wantRestore bool
	}{
		{"fresh claim", false, true},
		{"sticky lease renewal", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvSlim, "0")
			t.Setenv(EnvRestore, "")
			s := fakeSlot(t, "iPhone 17 Pro", "26.3", "")
			s.Renewed = tc.renewed
			restored := false
			deps := provisionDeps{
				find:           func(string) (simctl.DeviceEntry, bool, error) { return simctl.DeviceEntry{}, false, nil },
				listDevices:    func() ([]simctl.DeviceEntry, error) { return nil, nil },
				resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
				create:         func(string, string, string) (string, error) { return "UDID-1", nil },
				bootAndWait:    func(string, time.Duration) error { return nil },
				shutdown:       neverShutdown(t),
				delete:         neverDelete(t),
				restore: func(string, Presentation, time.Duration) ([]string, error) {
					restored = true
					return nil, nil
				},
			}
			if err := ensureProvisioned(s, "test", "lease", "k", deps, time.Minute); err != nil {
				t.Fatalf("provision: %v", err)
			}
			if restored != tc.wantRestore {
				t.Errorf("restore called = %v, want %v", restored, tc.wantRestore)
			}
		})
	}
}

// The reconcile must never take an acquisition down with it, for the same
// reason the slim one doesn't: a slot whose appearance could not be read is
// still a perfectly usable simulator, and a screenshot in the wrong
// language is a smaller outage than a test run that cannot start at all.
func TestEnsureProvisionedSurvivesARestoreFailure(t *testing.T) {
	t.Setenv(EnvSlim, "0")
	t.Setenv(EnvRestore, "")
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
		restore: func(string, Presentation, time.Duration) ([]string, error) {
			return nil, errors.New("simctl ui said no")
		},
	}
	if err := ensureProvisioned(s, "test", "with", "", deps, time.Minute); err != nil {
		t.Fatalf("a failed restore must not fail provisioning: %v", err)
	}
	if !booted {
		t.Error("the device must still be booted and handed out")
	}
}

func TestEnsureProvisionedSkipsRestoreWhenDisabled(t *testing.T) {
	t.Setenv(EnvSlim, "0")
	t.Setenv(EnvRestore, "0")
	s := fakeSlot(t, "iPhone 17 Pro", "26.3", "")
	deps := provisionDeps{
		find:           func(string) (simctl.DeviceEntry, bool, error) { return simctl.DeviceEntry{}, false, nil },
		listDevices:    func() ([]simctl.DeviceEntry, error) { return nil, nil },
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         func(string, string, string) (string, error) { return "UDID-1", nil },
		bootAndWait:    func(string, time.Duration) error { return nil },
		shutdown:       neverShutdown(t),
		delete:         neverDelete(t),
		restore: func(string, Presentation, time.Duration) ([]string, error) {
			t.Error("SIMPOOL_RESTORE=0 must skip the reconcile entirely")
			return nil, nil
		},
	}
	if err := ensureProvisioned(s, "test", "with", "", deps, time.Minute); err != nil {
		t.Fatalf("provision: %v", err)
	}
}
