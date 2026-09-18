package pool

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// globalPreferences is where iOS keeps AppleLanguages and AppleLocale
// inside a simulator's data container.
const globalPreferences = "Library/Preferences/.GlobalPreferences.plist"

// diskReadTimeout bounds one plutil call. Reading a 900-byte plist is
// microseconds; this only exists so a stalled filesystem cannot hang
// `simpool status`.
const diskReadTimeout = 5 * time.Second

// LocaleOnDisk reports the language and region recorded in a simulator's
// own data container, without booting it and without spawning anything
// inside it.
//
// This is what `simpool status` reports, for two reasons. It works on a
// shut-down slot, which `simctl spawn defaults read` cannot — and a
// shut-down slot carrying the previous consumer's language is exactly the
// one nobody would otherwise look at. And it is live rather than
// remembered: recording the value in meta.json at hand-out would be
// cheaper and would read correctly right up until the moment it mattered,
// because the whole failure being reported is a consumer changing this
// under a slot the pool already handed out.
//
// It is safe to read this rather than asking the running simulator:
// measured on a booted slot, a `defaults write` through `simctl spawn` is
// visible in this file immediately afterwards — cfprefsd inside the
// simulator writes through rather than holding the value in memory.
//
// Anything unreadable comes back as empty strings and no error. A slot
// that has never been booted has no such file at all, and a status column
// is not the place to turn that into a failure.
func LocaleOnDisk(dataPath string) (language, region string) {
	if dataPath == "" {
		return "", ""
	}
	plist := filepath.Join(dataPath, globalPreferences)
	return plutilExtract(plist, "AppleLanguages.0"), plutilExtract(plist, "AppleLocale")
}

func plutilExtract(plist, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), diskReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "plutil", "-extract", key, "raw", "-o", "-", plist).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
