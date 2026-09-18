package pool

import (
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// SpringBoardRestartTimeout bounds the one expensive step of a restore.
// Measured on a slim slot: SpringBoard is back in 4-6s. Ninety seconds
// turns a SpringBoard that never comes back into a warning on the hand-out
// path instead of a hang, and is nowhere near the normal case.
const SpringBoardRestartTimeout = 90 * time.Second

// The live presentation surface, one thin adapter per simctl primitive so
// presentation.go's dependency struct can be faked whole in tests without
// the test having to know anything about `xcrun`.

func simDefaultsRead(udid, key string, timeout time.Duration) (string, error) {
	return simctl.DefaultsRead(udid, key, timeout)
}

func simDefaultsWrite(udid, key, value string, array bool, timeout time.Duration) error {
	if array {
		return simctl.DefaultsWriteArray(udid, key, value, timeout)
	}
	return simctl.DefaultsWriteString(udid, key, value, timeout)
}

func simDefaultsDelete(udid, key string, timeout time.Duration) error {
	return simctl.DefaultsDelete(udid, key, timeout)
}

func simUI(udid, option, value string, timeout time.Duration) (string, error) {
	return simctl.UI(udid, option, value, timeout)
}

func simStatusBarOverridden(udid string, timeout time.Duration) (bool, error) {
	return simctl.StatusBarOverridden(udid, timeout)
}

func simStatusBarClear(udid string, timeout time.Duration) error {
	return simctl.StatusBarClear(udid, timeout)
}

func simRestartSpringBoard(udid string, timeout time.Duration) error {
	return simctl.RestartSpringBoard(udid, timeout)
}
