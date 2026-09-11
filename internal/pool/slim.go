package pool

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mobai-app/simslim"
)

// A stock simulator boots ~265 processes, most of them daemons no test
// runner will ever talk to: Siri, Spotlight, iCloud sync, Wallet, Health,
// PosterBoard. Measured on this project's own hardware (iPhone 17 Pro,
// iOS 26.3, an `xcrun simctl create`d device outside the pool):
//
//	stock  265 processes  4.90 GB phys_footprint
//	slim    73 processes  1.09 GB phys_footprint
//
// That 3.8GB is what caps the pool: DefaultMaxSlotsPerGroup exists purely
// because booting slots is how this machine reaches jetsam, and jetsam is
// what creates the orphaned userlands reap spends its life collecting. A
// slim slot costs roughly a quarter of a stock one, so the same memory
// budget buys several times the concurrency.
//
// The disables live in the simulator's own launchd overrides database and
// survive reboot on iOS 18.5+ (see simslim.PersistentOverridesSupported),
// so a slot pays the reconfigure exactly once in its life: every later
// acquisition of that same slot finds it already slim and does nothing.
const (
	// EnvSlim disables slimming when set to "0". On by default: a pool
	// whose slots are each holding 3.8GB of daemons nothing tests is the
	// problem, not the safe baseline.
	EnvSlim = "SIMPOOL_SLIM"

	// EnvSlimExcept is a comma-separated list of simslim category IDs to
	// leave fully enabled, for a repo whose tests genuinely need one of
	// them (`push` for APNs, `store` for StoreKit, `photos` for a picker
	// flow). `simslim profiles` lists them, and `simslim doctor --list`
	// maps a feature back to the daemons it needs.
	EnvSlimExcept = "SIMPOOL_SLIM_EXCEPT"

	// EnvSlimKeep is a comma-separated list of individual launchd labels
	// to leave running, for the case where a whole category is too blunt.
	EnvSlimKeep = "SIMPOOL_SLIM_KEEP"

	// EnvSlimTimeout overrides DefaultSlimTimeout.
	EnvSlimTimeout = "SIMPOOL_SLIM_TIMEOUT"
)

// DefaultSlimTimeout bounds the one-off reconfigure: boot, ~170 launchctl
// transitions, reboot, and a read-back to prove the overrides survived it.
// Measured here at roughly three minutes on an idle machine and longer
// under load, which is why this is minutes rather than the seconds a plain
// boot gets. It is not a per-acquisition cost — only the first acquisition
// of a slot ever pays it.
const DefaultSlimTimeout = 10 * time.Minute

// SlimTimeout resolves the effective slim budget: SIMPOOL_SLIM_TIMEOUT if
// set to a valid positive duration, else DefaultSlimTimeout.
func SlimTimeout() time.Duration {
	if v := os.Getenv(EnvSlimTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultSlimTimeout
}

// SlimEnabled reports whether newly provisioned slots should be slimmed.
func SlimEnabled() bool { return os.Getenv(EnvSlim) != "0" }

// SlimProfile resolves the profile applied to every slot this process
// provisions, from EnvSlimExcept/EnvSlimKeep. An unknown category ID is an
// error rather than a silent no-op: a repo that asks to keep `storekit`
// and quietly gets a fully slim simulator would fail its purchase tests
// with no indication that the flag it set never applied.
func SlimProfile() (simslim.Profile, error) {
	p := simslim.Profile{
		ExceptCategories: map[string]bool{},
		Keep:             map[string]bool{},
	}
	for _, id := range splitEnvList(os.Getenv(EnvSlimExcept)) {
		if _, ok := simslim.CategoryByID(id); !ok {
			return p, fmt.Errorf("%s: %q is not a simslim category (run `simslim profiles` for the list)", EnvSlimExcept, id)
		}
		p.ExceptCategories[id] = true
	}
	for _, label := range splitEnvList(os.Getenv(EnvSlimKeep)) {
		p.Keep[label] = true
	}
	return p, nil
}

func splitEnvList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SlimDevice brings udid to the profile's slim state, booting it in the
// process. It is idempotent by construction: simslim reads the device's
// current overrides and only reboots when the desired state differs from
// what is already there, so the common case — a slot provisioned slim days
// ago — costs one launchctl read and no reboot at all.
//
// Returns whether anything changed, so the caller can tell a first-time
// reconfigure (which rebooted the device, and is therefore worth a line on
// stderr) apart from the steady state.
//
// A runtime too old to persist overrides is reported as an error rather
// than silently slimmed for one boot: `with` would hand out a simulator
// that quietly goes stock again at its next boot, and a capacity decision
// made on the assumption that slots are slim would then be wrong in the
// one direction that ends in jetsam.
func SlimDevice(udid string, timeout time.Duration) (bool, error) {
	p, err := SlimProfile()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The empty device set is the default one — the same set simpool has
	// always provisioned into (see the README's "Why not a private device
	// set"), and the only one any slot's simulator ever lives in.
	return simslim.EnableSlim(ctx, "", udid, p, nil)
}

// SlimState describes how slim a booted device currently is, for `status`
// and `doctor`. A device that is not booted has no launchd to ask, so
// Booted is false and the counts are zero — never an error, since most of
// a pool is shut down most of the time.
type SlimState struct {
	Booted   bool
	Disabled int
	Total    int
}

// Slim reports whether every managed daemon this runtime knows about is
// disabled. A partially slim device (an interrupted reconfigure, a device
// erased out from under its overrides) is deliberately not "slim": it is
// holding memory the capacity maths already spent.
func (s SlimState) Slim() bool { return s.Booted && s.Total > 0 && s.Disabled == s.Total }

func (s SlimState) String() string {
	if !s.Booted {
		return "-"
	}
	if s.Slim() {
		return "slim"
	}
	return fmt.Sprintf("%d/%d", s.Disabled, s.Total)
}

// ReadSlimState reports udid's current slim state. Read-only: it never
// boots, reconfigures or reboots anything, so `status` and `doctor` stay
// as cheap and side-effect-free as they have always been.
func ReadSlimState(udid string, timeout time.Duration) (SlimState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st, _, err := simslim.ReadStatus(ctx, udid)
	if err != nil {
		return SlimState{}, err
	}
	return SlimState{Booted: st.Booted, Disabled: st.ManagedDisabled, Total: st.ManagedTotal}, nil
}
