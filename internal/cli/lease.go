package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

type leaseFlags struct {
	device string
	os     string
	key    string
	ttl    time.Duration
	max    int
}

func parseLeaseFlags(fs *flag.FlagSet, f *leaseFlags) {
	fs.StringVar(&f.device, "device", "", "simulator device type, e.g. \"iPhone 17 Pro\" (required)")
	fs.StringVar(&f.os, "os", "", "simulator OS version, e.g. \"26.3\" (required)")
	fs.StringVar(&f.key, "key", "", "sticky lease key; defaults to the current git repo's root, or the working directory if there is none")
	fs.DurationVar(&f.ttl, "ttl", pool.DefaultLeaseTTL, "how long the lease lasts before it's considered abandoned; renewed on every call made with the same key")
	fs.IntVar(&f.max, "max", pool.MaxSlotsPerGroup(), "maximum resident slots for this device+OS group, across all callers (env "+pool.EnvMaxSlots+")")
}

func (f *leaseFlags) validate() error {
	if f.device == "" {
		return fmt.Errorf("--device is required")
	}
	if f.os == "" {
		return fmt.Errorf("--os is required")
	}
	if f.ttl <= 0 {
		return fmt.Errorf("--ttl must be > 0")
	}
	if f.max < 1 {
		return fmt.Errorf("--max must be >= 1")
	}
	return nil
}

// RunLease implements `simpool lease`: a fast, sticky, key-scoped
// reservation meant to be called once per short-lived command in a hot
// loop (`mav tap`, `mav swipe`, `mav screenshot`, ...) — typically wired
// up as MAV's `target_command`, not invoked directly by hand. It prints
// exactly one line, the slot's UDID, and exits; it never wraps or waits
// for a child process the way `with` does, and it never blocks holding
// the lock the way `acquire` does.
//
// A lease is NOT a flock. It carries no live-process guarantee — it
// expires purely by wall-clock TTL, renewed on every call with the same
// key. If the caller's whole session dies without ever calling `simpool
// release`, the slot simply becomes reusable once the TTL elapses; unlike
// `with`/`acquire`, the kernel is not involved and there is no crash
// detection. See README "MAV in the hot loop" for the honest tradeoff.
//
// Never waits for capacity: if the device+OS group is full and no slot is
// free or leaseable, this fails immediately with an actionable message —
// the caller here is an interactive command inside an agent's tool loop,
// not a test that can afford to block.
func RunLease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var lf leaseFlags
	parseLeaseFlags(fs, &lf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := lf.validate(); err != nil {
		fmt.Fprintln(stderr, "simpool lease:", err)
		return 2
	}

	key := lf.key
	if key == "" {
		k, err := defaultLeaseKey()
		if err != nil {
			fmt.Fprintln(stderr, "simpool lease: resolving default --key:", err)
			return 1
		}
		key = k
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool lease:", err)
		return 1
	}
	fmt.Fprintf(stderr, "simpool: pool root %s\n", root)

	// Deliberately no reportForeignRootDevices scan here (see
	// acquireAndProvision's doc comment in provision.go): that costs an
	// extra `simctl list devices` subprocess, which this hot path — called
	// roughly once per mav tap/swipe/screenshot — cannot afford to pay on
	// every single call. Root + per-phase timings below are free (no extra
	// syscalls beyond what AcquireLease/EnsureProvisioned already make).
	acquireStart := time.Now()
	slot, err := pool.AcquireLease(root, lf.device, lf.os, key, lf.ttl, lf.max)
	if err != nil {
		if errors.Is(err, pool.ErrAtCapacity) {
			fmt.Fprintf(stderr, "simpool lease: %v for key %q — every slot is busy or leased elsewhere; run `simpool status` to see who holds them, or raise --max/%s\n", err, key, pool.EnvMaxSlots)
		} else {
			fmt.Fprintln(stderr, "simpool lease:", err)
		}
		return 1
	}
	fmt.Fprintf(stderr, "simpool: leased %s/slot-%d for key %q in %s\n", pool.GroupName(lf.device, lf.os), slot.Number, key, time.Since(acquireStart).Round(time.Millisecond))

	ownerCmd := "lease (key " + key + ")"
	provisionStart := time.Now()
	if err := pool.EnsureProvisioned(slot, ownerCmd, "lease", key); err != nil {
		fmt.Fprintln(stderr, "simpool lease:", err)
		return 1
	}
	fmt.Fprintf(stderr, "simpool: slot-%d provisioned (udid %s) in %s\n", slot.Number, slot.Meta.UDID, time.Since(provisionStart).Round(time.Millisecond))

	fmt.Fprintln(stdout, slot.Meta.UDID)
	return 0
}

// RunRelease implements `simpool release [--key K]`: drops key's lease
// wherever it is currently held, freeing the slot for reuse immediately
// instead of waiting out its TTL.
func RunRelease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	key := fs.String("key", "", "lease key to release; defaults to the current git repo's root, or the working directory if there is none")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	k := *key
	if k == "" {
		dk, err := defaultLeaseKey()
		if err != nil {
			fmt.Fprintln(stderr, "simpool release: resolving default --key:", err)
			return 1
		}
		k = dk
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool release:", err)
		return 1
	}

	// ReleaseLease can return a non-empty released alongside a non-nil err:
	// some slots' lease.json could not be verified (never guess-removed —
	// see ReleaseLease's doc comment), but that must not hide whatever was
	// successfully released elsewhere.
	released, err := pool.ReleaseLease(root, k)
	for _, dir := range released {
		fmt.Fprintf(stdout, "released %s (key %q)\n", dir, k)
	}
	if err != nil {
		fmt.Fprintln(stderr, "simpool release:", err)
		return 1
	}
	if len(released) == 0 {
		fmt.Fprintf(stdout, "no active lease for key %q\n", k)
	}
	return 0
}
