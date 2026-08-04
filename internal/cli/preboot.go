package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/bitomule/simpool/internal/pool"
)

// RunPreboot implements `simpool preboot [--device D] [--os V] [--count N]
// [--max M]`: a desired-state warm-up that provisions N slots for a
// device+OS group — booting their simulators — and releases every one of
// them immediately, so a session that follows (a `with`/`acquire`/`lease`
// call, a `bazel test` run) finds a warm slot instead of paying the ~110s
// cold-boot cost itself on its first acquisition.
//
// Ownership while warming: each slot's meta.json records Mode "preboot" and
// ownerCmd "preboot (pid N)" for the brief window it is actually being
// provisioned, exactly the way `with`/`acquire` record their own mode —
// reap/doctor already treat any Mode other than "with" as legitimately
// child-less (see reapHeldSlot/RunDoctor), so a preboot slot observed
// mid-provisioning is reported BUSY, never killed. Once provisioning
// finishes, the lock is released immediately: preboot owns nothing beyond
// the call itself, and the freed slot is indistinguishable from any other
// warm, freed slot — the next caller of any kind can pick it up.
//
// Never waits for capacity (unlike `with`/`acquire`'s own --wait): a group
// already at --max is left exactly as it is, warm or not, rather than
// blocking.
//
// Provisions ONE slot at a time — never holds a flock across the ~110s a
// cold boot can take — which is what actually keeps preboot from starving a
// real acquisition. An earlier version of this comment claimed the same
// thing while the code acquired all --count flocks up front through
// AcquireSlots and held every one of them for the entire boot loop
// (--count 3 held three slots hostage for ~5.5 minutes): a concurrent
// `simpool lease` — mav's target_command, which by design never waits for
// capacity — got ErrAtCapacity immediately in that window even though most
// of the group was only "busy" with preboot's own warm-up, not real work.
//
// Getting this right took two attempts, not one: the naive fix — loop
// AcquireSlots(..., 1, max, 0) once per slot, provisioning and releasing
// each before requesting the next — never holds more than one flock, but
// AcquireSlots itself prefers the most-recently-used FREE slot (the right
// policy for `with`/`acquire`/`lease`, which want a warm slot handed back).
// The instant that loop releases the slot it just warmed, it becomes the
// group's most-recently-used free slot — so the very next AcquireSlots(1)
// call hands preboot that exact same slot right back, forever. --count 3
// would silently rewarm slot-0 three times and never touch slots 1 or 2.
// So instead: a single AcquireSlots(count) call reserves --count DISTINCT
// slot numbers up front (the only moment more than one of this group's
// flocks is ever held at once — and only for the near-instant directory-
// bookkeeping AcquireSlots itself does, not for a boot), every one of them
// is released again immediately, and then each reserved number is re-locked
// one at a time, by number (pool.AcquireSlotByNumber, which bypasses the
// most-recently-used selection entirely), immediately before that slot's
// own provisioning step. A number that's gone busy again in that narrow
// window (something else claimed it first) is simply skipped, not retried
// or substituted — count is a ceiling on how many get warmed, not a
// guarantee.
//
// Deliberately has no --reconcile: it never inspects history, prior calls,
// or "how many slots have been used lately" to infer a desired count on its
// own — --count is the caller's own explicit ask, every time, nothing
// remembered or guessed.
//
// provisionForPreboot is pool.EnsureProvisioned by default, exposed as a
// package-level var — the same seam reap.go's listPoolDevices/shutdownOrphan
// use — so tests can observe/control what happens during a slot's
// provisioning window (which slot is currently held, what a concurrent
// acquisition can see) without shelling out to real `xcrun simctl`.
var provisionForPreboot = pool.EnsureProvisioned

func RunPreboot(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("preboot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	device := fs.String("device", "", "simulator device type, e.g. \"iPhone 17 Pro\" (required)")
	osVersion := fs.String("os", "", "simulator OS version, e.g. \"26.3\" (required)")
	count := fs.Int("count", 1, "number of slots to warm up")
	max := fs.Int("max", pool.MaxSlotsPerGroup(), "maximum resident slots for this device+OS group, across all callers (env "+pool.EnvMaxSlots+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *device == "" {
		fmt.Fprintln(stderr, "simpool preboot: --device is required")
		return 2
	}
	if *osVersion == "" {
		fmt.Fprintln(stderr, "simpool preboot: --os is required")
		return 2
	}
	if *count < 1 {
		fmt.Fprintln(stderr, "simpool preboot: --count must be >= 1")
		return 2
	}
	if *max < *count {
		fmt.Fprintf(stderr, "simpool preboot: --max (%d) must be >= --count (%d)\n", *max, *count)
		return 2
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool preboot:", err)
		return 1
	}

	// wait=0: never poll for capacity. This is the only call that can ever
	// hold more than one of this group's flocks at once — and only for the
	// near-instant directory-bookkeeping AcquireSlots itself does, released
	// again below before any provisioning starts. See the doc comment above
	// for why a naive one-AcquireSlots-call-per-slot loop doesn't work.
	slots, err := pool.AcquireSlots(root, *device, *osVersion, *count, *max, 0)
	if err != nil {
		if errors.Is(err, pool.ErrAtCapacity) {
			fmt.Fprintf(stdout, "preboot: %s already has %d slot(s), all busy or already warm — nothing to do\n", pool.GroupName(*device, *osVersion), *max)
			return 0
		}
		fmt.Fprintln(stderr, "simpool preboot:", err)
		return 1
	}
	numbers := make([]int, len(slots))
	for i, s := range slots {
		numbers[i] = s.Number
	}
	for _, s := range slots {
		s.Release()
	}

	ownerCmd := "preboot (pid " + strconv.Itoa(os.Getpid()) + ")"
	warmed := 0
	for _, n := range numbers {
		s, err := pool.AcquireSlotByNumber(root, *device, *osVersion, n)
		if err != nil {
			fmt.Fprintln(stderr, "simpool preboot:", err)
			return 1
		}
		if s == nil {
			// Something else claimed this exact slot number in the window
			// between reserving it and reaching it here — not preboot's to
			// warm anymore; move on rather than retry or substitute.
			continue
		}
		if err := provisionForPreboot(s, ownerCmd, "preboot", ""); err != nil {
			s.Release()
			fmt.Fprintf(stderr, "simpool preboot: slot %s: %v\n", s.Dir, err)
			return 1
		}
		fmt.Fprintf(stdout, "preboot: warmed %s/slot-%d (udid %s)\n", filepath.Base(s.GroupDir), s.Number, s.Meta.UDID)
		s.Release()
		warmed++
	}
	if warmed < *count {
		fmt.Fprintf(stdout, "preboot: warmed %d/%d requested slot(s) for %s\n", warmed, *count, pool.GroupName(*device, *osVersion))
	}
	return 0
}
