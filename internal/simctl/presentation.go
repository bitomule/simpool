package simctl

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// This file holds the simctl calls that read and write a simulator's
// device-wide presentation — language, region, appearance, text size,
// contrast, status bar. They are separate from the rest of the package
// because every one of them takes its own timeout: they are made on the
// hand-out path, where `simpool lease` promises one bound to a caller that
// has nothing to retry with, not the package-wide 120s that `create` and
// `delete` need.

// runWithTimeout is run(), with the deadline supplied by the caller rather
// than read from the environment.
func runWithTimeout(timeout time.Duration, args ...string) ([]byte, error) {
	full := append([]string{"simctl"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := runContext(ctx, full...)
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w: xcrun %s", ErrSimctlTimeout, strings.Join(full, " "))
	}
	if err != nil {
		return nil, fmt.Errorf("xcrun %s: %w", strings.Join(full, " "), err)
	}
	return out, nil
}

// DefaultsRead reads one key of udid's global preference domain, the way
// `defaults read -g <key>` renders it — a bare scalar, or a plist array
// across several lines. A key that does not exist is an error, and the
// caller is expected to tell that apart from a failure to look (see
// Presentation.Force24Hour).
func DefaultsRead(udid, key string, timeout time.Duration) (string, error) {
	out, err := runWithTimeout(timeout, "spawn", udid, "defaults", "read", "-g", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultsWriteString writes a string value into udid's global domain.
func DefaultsWriteString(udid, key, value string, timeout time.Duration) error {
	_, err := runWithTimeout(timeout, "spawn", udid, "defaults", "write", "-g", key, "-string", value)
	return err
}

// DefaultsWriteArray writes a one-element array into udid's global domain.
// One element, not the several iOS itself keeps: AppleLanguages is written
// as the single language asked for, and iOS re-derives its own fallback
// chain from it — which is exactly what `mav sim language` does, so a slot
// restored here and a slot set by mav end up in the same shape.
func DefaultsWriteArray(udid, key, value string, timeout time.Duration) error {
	_, err := runWithTimeout(timeout, "spawn", udid, "defaults", "write", "-g", key, "-array", value)
	return err
}

// DefaultsDelete removes a key from udid's global domain. Removing a key
// that is not there is not an error — the state asked for is the state
// arrived at.
func DefaultsDelete(udid, key string, timeout time.Duration) error {
	_, err := runWithTimeout(timeout, "spawn", udid, "defaults", "delete", "-g", key)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// UI reads (value == "") or sets one `simctl ui` option: appearance,
// content_size or increase_contrast.
func UI(udid, option, value string, timeout time.Duration) (string, error) {
	args := []string{"ui", udid, option}
	if value != "" {
		args = append(args, value)
	}
	out, err := runWithTimeout(timeout, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// StatusBarOverridden reports whether udid has any status bar override in
// effect. `simctl status_bar <udid> list` prints a two-line header and then
// one line per override, so "more than the header" is the answer; there is
// no machine-readable form of this command to prefer.
func StatusBarOverridden(udid string, timeout time.Duration) (bool, error) {
	out, err := runWithTimeout(timeout, "status_bar", udid, "list")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Current Status Bar Overrides") || strings.HasPrefix(line, "===") {
			continue
		}
		return true, nil
	}
	return false, nil
}

// StatusBarClear removes every status bar override from udid.
func StatusBarClear(udid string, timeout time.Duration) error {
	_, err := runWithTimeout(timeout, "status_bar", udid, "clear")
	return err
}

// springBoardPoll is how often RestartSpringBoard asks whether SpringBoard
// is back, and springBoardSettle is the margin it allows after the job
// reports itself running before the status bar has actually been repainted.
// Measured on an iPhone 17 Pro / iOS 26.3 slim slot: the job is back in
// ~4-6s, and a capture two seconds later shows the new language.
const (
	springBoardPoll   = 500 * time.Millisecond
	springBoardSettle = 2 * time.Second
)

// RestartSpringBoard stops SpringBoard and waits for launchd to bring it
// back. This is the step that makes a language change visible: SpringBoard
// draws the status bar and reads the language once, at launch, so writing
// AppleLanguages without this leaves the previous tenant's language on
// screen while every file on disk says otherwise.
//
// Polls rather than sleeping a fixed interval. A fixed sleep is either too
// long on every slot that was already clean, or too short on the one cold
// slot that matters — and the short one hands out a slot still painting the
// old language while reporting success.
func RestartSpringBoard(udid string, timeout time.Duration) error {
	if _, err := runWithTimeout(timeout, "spawn", udid, "launchctl", "stop", "com.apple.SpringBoard"); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(springBoardPoll)
		out, err := runWithTimeout(timeout, "spawn", udid, "launchctl", "print", "system/com.apple.SpringBoard")
		if err != nil {
			continue
		}
		if strings.Contains(string(out), "state = running") {
			time.Sleep(springBoardSettle)
			return nil
		}
	}
	return fmt.Errorf("SpringBoard on %s did not come back within %s after being stopped", udid, timeout)
}
