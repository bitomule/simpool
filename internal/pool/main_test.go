package pool

import (
	"os"
	"testing"
)

// TestMain pins the machine measurement the cold-boot memory floor reads, so
// that acquisition tests test acquisition and not whatever this machine's
// memory happens to be doing while they run.
//
// Not a tidiness choice — three tests went red on the run that introduced the
// floor, all of them tests of flock arbitration, --need dispatch and lease
// skipping that simply expect a group to grow. Every one of them was
// CORRECT to go red: the machine was sitting at 20% free, below the 35%
// floor, so acquisition queued instead of adding a simulator, which is
// exactly what the floor is for. A suite whose verdict depends on the
// machine's free memory is a suite that passes on a quiet laptop and fails in
// the afternoon, and the failure would read as a lock bug.
//
// 0.90 rather than "disabled", so the floor's real code path still runs on
// every acquisition test — a stub that removed the check entirely would hide
// a floor that fired unconditionally. Tests that are ABOUT the floor
// (coldfloor_test.go) call stubFreeMemory themselves and take precedence for
// their own duration.
//
// It applies to the re-executed helper subprocesses too (slot_test.go and
// lock_test.go spawn this same binary with -test.run), which is what keeps
// two real processes racing for slots from being arbitrated by memory.
func TestMain(m *testing.M) {
	FreeMemoryFraction = func() (float64, bool) { return 0.90, true }
	os.Exit(m.Run())
}
