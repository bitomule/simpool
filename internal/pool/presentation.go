package pool

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A slot's *presentation* is the device-wide state that decides what any
// screenshot of any app on it looks like: its language, its region, its
// appearance, its text size. None of it belongs to the app under test and
// none of it is reset by anything iOS does on its own — a consumer that
// sets one sets it for every consumer that comes after, and nothing
// anywhere says so.
//
// That is not hypothetical. A node capturing App Store screenshots put a
// slot into German, the slot went back to the pool in German, and the next
// snapshot suite would have recorded German baselines without a word of
// warning; the node reset it to es-ES by hand, which fixes that one slot
// once. The pressure is about to get much worse: `mav sim language` exists
// precisely so a screenshot matrix can walk fourteen App Store languages,
// so from now on slots get their language changed deliberately and many
// times a day.
//
// It is also the same defect this project has now hit four times in a
// month from four directions — slots whose renderer differs, a corpus
// shared by absolute path, a bazel symlink that repointed, a debug build
// sharing an App Group with production. A worktree isolates files and
// nothing else. A slot carrying the previous tenant's settings is a green
// or a red that depends on who had it before you, which is the one thing a
// snapshot suite cannot survive.
//
// What is deliberately NOT here: installed apps, their containers, the
// keychain, added media. Those are stored data, not presentation — they
// change what a screenshot of *that app* looks like, never what a
// screenshot of an unrelated app looks like, and a suite installs its own
// build anyway. Wiping them would also throw away the reuse that makes a
// warm slot worth having (the same argument scrub.go makes for leaving
// stored data alone), and cost a reinstall on every acquisition.
type Presentation struct {
	// Language is AppleLanguages[0] — the primary language, e.g. "es-ES".
	// Only the head of the array is ever compared: iOS appends its own
	// fallbacks ("es-ES" gains "en-ES" on its own), so comparing the whole
	// array would call every slot dirty forever and rewrite it on every
	// acquisition.
	Language string `json:"language,omitempty"`
	// Region is AppleLocale, e.g. "es_ES". Decides date order, decimal
	// separator, currency and measurement units independently of Language.
	Region string `json:"region,omitempty"`
	// Force24Hour is AppleICUForce24HourTime: "1", "0", or "" when the key
	// is absent, which is not the same as "0" — absent means "follow the
	// region", 0 means "12-hour, whatever the region says". A slot that has
	// been through a language change carries an explicit value where a
	// fresh one carries none, which is exactly how it was found.
	Force24Hour string `json:"force24Hour,omitempty"`
	// Appearance is simctl's ui appearance: "light" or "dark".
	Appearance string `json:"appearance,omitempty"`
	// ContentSize is simctl's ui content_size — the Dynamic Type category,
	// "large" on a fresh device. Changes the size of every string on screen.
	ContentSize string `json:"contentSize,omitempty"`
	// IncreaseContrast is simctl's ui increase_contrast: "enabled" or
	// "disabled". Changes borders and system colours.
	IncreaseContrast string `json:"increaseContrast,omitempty"`
	// StatusBarOverridden reports whether any `simctl status_bar override`
	// is in effect — the 9:41 clock and full battery every screenshot
	// pipeline sets. Unlike everything else here it does NOT survive a
	// reboot (measured: the override list is empty immediately after a
	// shutdown+boot), but pool slots stay booted for days, so between two
	// consumers of a warm slot it is as sticky as the rest.
	StatusBarOverridden bool `json:"statusBarOverridden,omitempty"`
}

// EnvRestore disables the presentation reconcile entirely when set to "0",
// mirroring SIMPOOL_SLIM. An escape hatch for a machine where the reconcile
// itself is the problem, not a setting anyone should need.
const EnvRestore = "SIMPOOL_RESTORE"

// EnvLanguage and EnvRegion override the baseline the pool restores slots
// to. Unset, the baseline is the host Mac's own language and region, which
// is exactly what a hand-made `simctl create` produces — so turning this on
// changes no existing snapshot baseline in any consuming repo, it only
// stops them drifting.
const (
	EnvLanguage = "SIMPOOL_LANGUAGE"
	EnvRegion   = "SIMPOOL_REGION"
)

// RestoreEnabled reports whether hand-out should reconcile presentation.
func RestoreEnabled() bool { return os.Getenv(EnvRestore) != "0" }

var (
	baselineOnce sync.Once
	baseline     Presentation
)

// BaselinePresentation is the state a slot is handed out in: the host's own
// language and region, plus iOS's factory defaults for everything else.
//
// Uniform across the whole pool on purpose. A per-slot baseline — whatever
// each simulator happened to be created with, say — would let two slots in
// the same group disagree, and "the same suite gives the same result
// whichever slot you got" is the entire point. Resolved once per process:
// reading the host's defaults is a subprocess, and nothing about it changes
// while a command runs.
func BaselinePresentation() Presentation {
	baselineOnce.Do(func() {
		baseline = Presentation{
			Language:         firstNonEmpty(os.Getenv(EnvLanguage), hostDefault("AppleLanguages"), "en-US"),
			Region:           firstNonEmpty(os.Getenv(EnvRegion), hostDefault("AppleLocale"), "en_US"),
			Force24Hour:      "",
			Appearance:       "light",
			ContentSize:      "large",
			IncreaseContrast: "disabled",
		}
	})
	return baseline
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// hostDefault reads one key out of the host Mac's global domain, returning
// "" for anything it cannot read. AppleLanguages comes back as a plist
// array; only its head is wanted (see Presentation.Language).
func hostDefault(key string) string {
	out, err := exec.Command("defaults", "read", "-g", key).Output()
	if err != nil {
		return ""
	}
	return firstPlistString(string(out))
}

// plistHead matches the first string in `defaults`' plist-array rendering,
// quoted or bare:
//
//	(
//	    "es-ES",
//	    "en-ES"
//	)
var plistHead = regexp.MustCompile(`\(\s*"?([A-Za-z0-9_@\-]+)"?`)

// firstPlistString reduces one `defaults read` output to a single value: the
// head of an array, or the scalar itself.
func firstPlistString(out string) string {
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "(") {
		if m := plistHead.FindStringSubmatch(out); m != nil {
			return m[1]
		}
		return ""
	}
	return strings.Trim(out, "\"")
}

// presentationDeps is the simctl surface ReadPresentation and
// RestorePresentation use, faked in tests for the same reason
// provisionDeps is: the real one shells out to `xcrun simctl` against a
// booted simulator, which `go test ./...` must never do.
type presentationDeps struct {
	// defaultsRead reads one key of the simulator's global domain.
	defaultsRead func(udid, key string, timeout time.Duration) (string, error)
	// defaultsWrite writes one key. array=true writes a one-element array
	// (AppleLanguages), false a string.
	defaultsWrite func(udid, key, value string, array bool, timeout time.Duration) error
	// defaultsDelete removes a key, for restoring "absent" (Force24Hour).
	defaultsDelete func(udid, key string, timeout time.Duration) error
	// ui reads (value == "") or sets one `simctl ui` option.
	ui func(udid, option, value string, timeout time.Duration) (string, error)
	// statusBarOverridden reports whether any override is in effect.
	statusBarOverridden func(udid string, timeout time.Duration) (bool, error)
	// statusBarClear removes every override.
	statusBarClear func(udid string, timeout time.Duration) error
	// restartSpringBoard stops SpringBoard and waits for it to come back,
	// which is the only way a language change reaches the status bar.
	restartSpringBoard func(udid string, timeout time.Duration) error
}

var livePresentationDeps = presentationDeps{
	defaultsRead:        simDefaultsRead,
	defaultsWrite:       simDefaultsWrite,
	defaultsDelete:      simDefaultsDelete,
	ui:                  simUI,
	statusBarOverridden: simStatusBarOverridden,
	statusBarClear:      simStatusBarClear,
	restartSpringBoard:  simRestartSpringBoard,
}

// DefaultPresentationTimeout bounds one simctl call made on behalf of the
// presentation reconcile. Measured on this pool: `simctl spawn … defaults
// read` 1.3s, `simctl ui` 0.8s, `simctl status_bar list` 0.8s. Thirty
// seconds is far above any of them and still turns a wedged simulator into
// an error rather than a hung acquisition.
const DefaultPresentationTimeout = 30 * time.Second

// ReadPresentation reports the state udid is currently in. Every read is an
// independent subprocess, so they run at once: serially this is ~5s on the
// hand-out path, in parallel it is one call's latency.
func ReadPresentation(udid string, timeout time.Duration) (Presentation, error) {
	return readPresentation(udid, timeout, livePresentationDeps)
}

func readPresentation(udid string, timeout time.Duration, deps presentationDeps) (Presentation, error) {
	var (
		p    Presentation
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)
	record := func(name string, err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			mu.Unlock()
		}
	}
	read := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record(name, fn())
		}()
	}

	read("AppleLanguages", func() error {
		v, err := deps.defaultsRead(udid, "AppleLanguages", timeout)
		mu.Lock()
		p.Language = firstPlistString(v)
		mu.Unlock()
		return err
	})
	read("AppleLocale", func() error {
		v, err := deps.defaultsRead(udid, "AppleLocale", timeout)
		mu.Lock()
		p.Region = strings.TrimSpace(v)
		mu.Unlock()
		return err
	})
	// A missing key is the answer, not a failure: AppleICUForce24HourTime
	// is absent on a fresh device and present on one a language change has
	// been through, and conflating "absent" with "could not read" would
	// make the fresh state look unreadable.
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, err := deps.defaultsRead(udid, "AppleICUForce24HourTime", timeout)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			p.Force24Hour = ""
			return
		}
		p.Force24Hour = strings.TrimSpace(v)
	}()

	for _, opt := range []struct {
		name  string
		field *string
	}{
		{"appearance", &p.Appearance},
		{"content_size", &p.ContentSize},
		{"increase_contrast", &p.IncreaseContrast},
	} {
		opt := opt
		read(opt.name, func() error {
			v, err := deps.ui(udid, opt.name, "", timeout)
			mu.Lock()
			*opt.field = strings.TrimSpace(v)
			mu.Unlock()
			return err
		})
	}

	read("status_bar", func() error {
		v, err := deps.statusBarOverridden(udid, timeout)
		mu.Lock()
		p.StatusBarOverridden = v
		mu.Unlock()
		return err
	})

	wg.Wait()
	if len(errs) > 0 {
		return p, fmt.Errorf("reading presentation of %s: %s", udid, strings.Join(errs, "; "))
	}
	return p, nil
}

// Drift lists, in human words, every way have differs from want. Empty
// means the slot is clean. The strings are what the reconcile prints and
// what the tests assert on, so they name the setting and both values.
func (want Presentation) Drift(have Presentation) []string {
	var out []string
	cmp := func(name, got, expected string) {
		if got != expected {
			out = append(out, fmt.Sprintf("%s %s→%s", name, orNone(got), orNone(expected)))
		}
	}
	cmp("language", have.Language, want.Language)
	cmp("region", have.Region, want.Region)
	cmp("24-hour clock", have.Force24Hour, want.Force24Hour)
	cmp("appearance", have.Appearance, want.Appearance)
	cmp("text size", have.ContentSize, want.ContentSize)
	cmp("increase contrast", have.IncreaseContrast, want.IncreaseContrast)
	if have.StatusBarOverridden != want.StatusBarOverridden {
		out = append(out, "status bar overrides set→cleared")
	}
	return out
}

func orNone(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

// RestorePresentation brings udid back to want and reports what it had to
// change. An already-clean slot costs the reads and nothing else — no
// writes, and above all no SpringBoard restart, which is the only expensive
// step here (~6s measured) and the reason this compares before it writes
// rather than setting everything unconditionally.
//
// A read that fails is not treated as "clean". It returns the error, and
// the caller decides: the hand-out path warns loudly and hands the slot
// over anyway, because refusing to hand out a usable simulator over a
// failed `simctl ui` would be a worse outage than the drift it is guarding
// against.
func RestorePresentation(udid string, want Presentation, timeout time.Duration) ([]string, error) {
	return restorePresentation(udid, want, timeout, livePresentationDeps)
}

func restorePresentation(udid string, want Presentation, timeout time.Duration, deps presentationDeps) ([]string, error) {
	have, readErr := readPresentation(udid, timeout, deps)
	if readErr != nil {
		return nil, readErr
	}
	drift := want.Drift(have)
	if len(drift) == 0 {
		return nil, nil
	}

	// SpringBoard draws the status bar, and it reads the language once at
	// launch — which is how an English iPad screenshot shipped to the App
	// Store with "Lunes 7 de septiembre" in its status bar. Writing the
	// defaults is not enough; only the three keys below need the restart,
	// so a slot whose only drift is `simctl ui` (those apply live) never
	// pays for one.
	restart := false

	if have.Language != want.Language {
		if err := deps.defaultsWrite(udid, "AppleLanguages", want.Language, true, timeout); err != nil {
			return drift, err
		}
		restart = true
	}
	if have.Region != want.Region {
		if err := deps.defaultsWrite(udid, "AppleLocale", want.Region, false, timeout); err != nil {
			return drift, err
		}
		restart = true
	}
	if have.Force24Hour != want.Force24Hour {
		var err error
		if want.Force24Hour == "" {
			err = deps.defaultsDelete(udid, "AppleICUForce24HourTime", timeout)
		} else {
			err = deps.defaultsWrite(udid, "AppleICUForce24HourTime", want.Force24Hour, false, timeout)
		}
		if err != nil {
			return drift, err
		}
		restart = true
	}

	for _, opt := range []struct{ name, have, want string }{
		{"appearance", have.Appearance, want.Appearance},
		{"content_size", have.ContentSize, want.ContentSize},
		{"increase_contrast", have.IncreaseContrast, want.IncreaseContrast},
	} {
		if opt.have == opt.want {
			continue
		}
		if _, err := deps.ui(udid, opt.name, opt.want, timeout); err != nil {
			return drift, err
		}
	}

	if have.StatusBarOverridden && !want.StatusBarOverridden {
		if err := deps.statusBarClear(udid, timeout); err != nil {
			return drift, err
		}
	}

	if restart {
		if err := deps.restartSpringBoard(udid, SpringBoardRestartTimeout); err != nil {
			return drift, err
		}
	}
	return drift, nil
}
