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
// blocking. Combined with releasing every slot the instant it's
// provisioned, this is what keeps preboot from ever starving a real
// acquisition — it can only ever create slots up to the same --max every
// other caller respects, and a concurrent real acquisition racing for the
// very same slot number is on entirely equal footing with it, never
// blocked behind it.
//
// Deliberately has no --reconcile: it never inspects history, prior calls,
// or "how many slots have been used lately" to infer a desired count on its
// own — --count is the caller's own explicit ask, every time, nothing
// remembered or guessed.
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

	// wait=0: never poll for capacity — see the doc comment above for why
	// that's what keeps preboot from ever starving a real acquisition.
	slots, err := pool.AcquireSlots(root, *device, *osVersion, *count, *max, 0)
	if err != nil {
		if errors.Is(err, pool.ErrAtCapacity) {
			fmt.Fprintf(stdout, "preboot: %s already has %d slot(s), all busy or already warm — nothing to do\n", pool.GroupName(*device, *osVersion), *max)
			return 0
		}
		fmt.Fprintln(stderr, "simpool preboot:", err)
		return 1
	}

	release := func() {
		for _, s := range slots {
			s.Release()
		}
	}

	ownerCmd := "preboot (pid " + strconv.Itoa(os.Getpid()) + ")"
	for _, s := range slots {
		if err := pool.EnsureProvisioned(s, ownerCmd, "preboot", ""); err != nil {
			release()
			fmt.Fprintf(stderr, "simpool preboot: slot %s: %v\n", s.Dir, err)
			return 1
		}
		fmt.Fprintf(stdout, "preboot: warmed %s/slot-%d (udid %s)\n", filepath.Base(s.GroupDir), s.Number, s.Meta.UDID)
	}
	release()
	return 0
}
