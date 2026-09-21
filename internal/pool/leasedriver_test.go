package pool

import (
	"fmt"
	"testing"
	"time"
)

// stageLeaseDriver replaces the three process lookups ConcurrentLeaseDriver
// makes with a staged world: who this call's parent is, and which pids are
// alive with what start time. Restored when the test ends.
func stageLeaseDriver(t *testing.T, ownPID int, world map[int]string) {
	t.Helper()
	oldPID, oldAlive, oldStart := leaseDriverPID, leaseDriverAlive, leaseDriverStartTime
	t.Cleanup(func() {
		leaseDriverPID, leaseDriverAlive, leaseDriverStartTime = oldPID, oldAlive, oldStart
	})
	leaseDriverPID = func() int { return ownPID }
	leaseDriverAlive = func(pid int) bool { _, ok := world[pid]; return ok }
	leaseDriverStartTime = func(pid int) (string, error) {
		started, ok := world[pid]
		if !ok {
			return "", fmt.Errorf("no such process %d", pid)
		}
		return started, nil
	}
}

// TestConcurrentLeaseDriver_WarnsAndStaysQuiet is both directions of the
// warning in one table, and the quiet rows are the ones that matter: a
// warning that cannot fail to fire is noise, and noise stops being read
// within a week.
func TestConcurrentLeaseDriver_WarnsAndStaysQuiet(t *testing.T) {
	const (
		mine   = 100
		theirs = 200
	)
	cases := []struct {
		name  string
		meta  Meta
		own   int
		world map[int]string
		want  bool
	}{
		{
			// Two agents launched from one checkout: different mav
			// processes, both alive, one key.
			name:  "two live drivers under one key",
			meta:  Meta{LeaseDriverPID: theirs, LeaseDriverStartedAt: "T-theirs"},
			own:   mine,
			world: map[int]string{mine: "T-mine", theirs: "T-theirs"},
			want:  true,
		},
		{
			// One agent's hot loop. Every `mav tap` is a fresh lease
			// process with a new pid, but they all hang off one mav.
			name:  "the same driver renewing its own slot",
			meta:  Meta{LeaseDriverPID: mine, LeaseDriverStartedAt: "T-mine"},
			own:   mine,
			world: map[int]string{mine: "T-mine"},
			want:  false,
		},
		{
			// The previous session ended; its mav is gone.
			name:  "the recorded driver has exited",
			meta:  Meta{LeaseDriverPID: theirs, LeaseDriverStartedAt: "T-theirs"},
			own:   mine,
			world: map[int]string{mine: "T-mine"},
			want:  false,
		},
		{
			// macOS recycled the pid onto something unrelated. Alive is
			// not enough; the fingerprint is what decides.
			name:  "the recorded pid was reused by another process",
			meta:  Meta{LeaseDriverPID: theirs, LeaseDriverStartedAt: "T-theirs"},
			own:   mine,
			world: map[int]string{mine: "T-mine", theirs: "T-somebody-else"},
			want:  false,
		},
		{
			name:  "nothing was ever recorded",
			meta:  Meta{},
			own:   mine,
			world: map[int]string{mine: "T-mine"},
			want:  false,
		},
		{
			// A pid with no fingerprint cannot be told apart from a reused
			// one, so it is not reported.
			name:  "a recorded driver with no fingerprint",
			meta:  Meta{LeaseDriverPID: theirs},
			own:   mine,
			world: map[int]string{mine: "T-mine", theirs: "T-theirs"},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageLeaseDriver(t, tc.own, tc.world)
			driver, got := ConcurrentLeaseDriver(tc.meta)
			if got != tc.want {
				t.Fatalf("ConcurrentLeaseDriver = %v, want %v", got, tc.want)
			}
			if got && driver.PID != tc.meta.LeaseDriverPID {
				t.Errorf("reported pid %d, want the recorded driver %d", driver.PID, tc.meta.LeaseDriverPID)
			}
		})
	}
}

// TestAcquireLease_RenewalNamesTheOtherLiveSession is the same two
// directions through the real acquisition path, which is where the warning
// is actually produced: a sticky renewal on a slot whose recorded driver is
// somebody else's, and the hot loop's own renewal right next to it.
func TestAcquireLease_RenewalNamesTheOtherLiveSession(t *testing.T) {
	const (
		mine   = 100
		theirs = 200
		key    = "/Users/someone/Projects/App"
	)

	seed := func(t *testing.T, driverPID int, started string) (root string, dir string) {
		t.Helper()
		root = t.TempDir()
		g := seedSlotDirs(t, root, "TestDevice", "1.0", 1)
		dir = SlotDir(g, 0)
		if err := WriteLease(dir, Lease{Key: key, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if err := WriteMeta(dir, Meta{
			Mode:                 "lease",
			LeaseKey:             key,
			UDID:                 "UDID-UNDER-TEST",
			LeaseDriverPID:       driverPID,
			LeaseDriverStartedAt: started,
		}); err != nil {
			t.Fatal(err)
		}
		return root, dir
	}

	t.Run("another live session is named", func(t *testing.T) {
		root, _ := seed(t, theirs, "T-theirs")
		stageLeaseDriver(t, mine, map[int]string{mine: "T-mine", theirs: "T-theirs"})

		slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1)
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if slot.SharedWith == nil {
			t.Fatal("two live sessions share this key and this slot, and the claim said nothing")
		}
		if slot.SharedWith.PID != theirs {
			t.Errorf("named pid %d, want the other session's %d", slot.SharedWith.PID, theirs)
		}
		// Still handed over: this is information, never a refusal.
		if slot.Number != 0 {
			t.Errorf("the renewal must still land on slot-0, got slot-%d", slot.Number)
		}
	})

	t.Run("a hot loop renewing its own slot is quiet", func(t *testing.T) {
		root, _ := seed(t, mine, "T-mine")
		stageLeaseDriver(t, mine, map[int]string{mine: "T-mine"})

		slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1)
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if slot.SharedWith != nil {
			t.Fatalf("warned about %s on a hot loop renewing its own slot", slot.SharedWith)
		}
	})

	t.Run("a different key's leftover driver is not this key's business", func(t *testing.T) {
		root, dir := seed(t, theirs, "T-theirs")
		meta := ReadMeta(dir)
		meta.LeaseKey = "/Users/someone/Projects/SomethingElse"
		if err := WriteMeta(dir, meta); err != nil {
			t.Fatal(err)
		}
		if err := RemoveLease(dir); err != nil {
			t.Fatal(err)
		}
		stageLeaseDriver(t, mine, map[int]string{mine: "T-mine", theirs: "T-theirs"})

		slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Hour, 1)
		if err != nil {
			t.Fatalf("AcquireLease: %v", err)
		}
		if slot.SharedWith != nil {
			t.Fatalf("warned about %s, whose session holds a different key entirely", slot.SharedWith)
		}
	})
}
