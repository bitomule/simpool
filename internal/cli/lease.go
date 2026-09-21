package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
)

type leaseFlags struct {
	device string
	os     string
	key    string
	ttl    time.Duration
	max    int
	need   string
}

func parseLeaseFlags(fs *flag.FlagSet, f *leaseFlags) {
	fs.StringVar(&f.device, "device", "", "simulator device type, e.g. \"iPhone 17 Pro\" (required)")
	fs.StringVar(&f.os, "os", "", "simulator OS version, e.g. \"26.3\" (required)")
	fs.StringVar(&f.key, "key", "", "sticky lease key; defaults to "+pool.EnvLeaseKey+", else the current git worktree's root, else the working directory. Two concurrent callers sharing a key share a simulator — give each its own")
	fs.DurationVar(&f.ttl, "ttl", pool.LeaseTTL(), "how long the lease lasts before it's considered abandoned; renewed on every call made with the same key (env "+pool.EnvLeaseTTL+")")
	fs.IntVar(&f.max, "max", pool.MaxSlotsPerGroup(), "maximum resident slots for this device+OS group, across all callers (env "+pool.EnvMaxSlots+")")
	fs.StringVar(&f.need, "need", "", "comma-separated capabilities this slot must have, e.g. \"photos\" or \"spotlight\" (env "+pool.EnvNeed+"); see `simpool with --help`")
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
	return applyNeed(f.need)
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
		// No "every slot is busy or leased elsewhere" gloss any more, and
		// no pointing at `simpool status` for an explanation: the error
		// itself now names, per slot, the reason that slot was refused
		// (see pool.atCapacityError), and status reports the same verdict
		// this path computed rather than a flock-only "free" that
		// contradicted it.
		fmt.Fprintln(stderr, "simpool lease:", err)
		return 1
	}
	fmt.Fprintf(stderr, "simpool: leased %s/slot-%d for key %q in %s\n", pool.GroupName(lf.device, lf.os), slot.Number, key, time.Since(acquireStart).Round(time.Millisecond))
	// The one thing simpool can say about the failure that is invisible
	// from inside it: another live session is already driving this slot
	// under this same key, so both are about to write to one simulator and
	// read each other's state back. Stickiness is per key, the default key
	// is the git repo root, and two agents launched from one checkout land
	// here — with --max having nothing to say about it, because the sticky
	// renewal returns the key's own slot before capacity is counted.
	//
	// A warning and not a refusal: sharing a key on purpose is exactly what
	// stickiness is for, and simpool cannot tell one agent's two tools from
	// two agents. It is deliberately quiet in every normal case — see
	// pool.ConcurrentLeaseDriver for the four of them.
	if slot.SharedWith != nil {
		// The UDID as read from disk, and only if it is there: this runs
		// before EnsureProvisioned, so a slot that has never been
		// provisioned has none yet, and "simulator " with nothing after it
		// sends the reader looking for a name that does not exist.
		what := "this slot"
		if slot.Meta.UDID != "" {
			what = "simulator " + slot.Meta.UDID
		}
		fmt.Fprintf(stderr, "simpool: warning: another live session (%s) is already leasing this slot under key %q — both of you are driving %s and will read each other's state\n", slot.SharedWith, key, what)
		fmt.Fprintln(stderr, "simpool:   if that is not deliberate, give each caller its own --key (its worktree path, its agent name, anything unique)")
	}

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
	key := fs.String("key", "", "lease key to release; defaults to "+pool.EnvLeaseKey+", else the current git worktree's root, else the working directory")
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
		// Dropping the lease does not end the session's side effects: a
		// complete, successful `mav run` leaves `idb`'s own companion
		// daemon attached to the slot's device (its "terminate if the
		// target goes offline" default is false), and that alone
		// quarantines the slot against every other caller. Nothing else
		// runs at this moment — the next acquisition and `reap` are both
		// observers that arrive later, if at all — so the reclaim happens
		// here, for exactly the key that left it. Best-effort: a refusal
		// leaves the slot as it was and falls through to the note below.
		// It reports what it killed on stderr itself, in the same words the
		// acquisition paths use for the same act — printing it again here
		// would only say it twice.
		pool.ReclaimReleasedResidue(root, dir, k)
		// Dropping the lease is not the same as making the slot available,
		// and saying only the first while the caller reads it as the second
		// is what made this command look like it succeeded without changing
		// anything: the reported failure that led here was a slot no lease
		// held at all — it was quarantined by a live process still
		// referencing its device — so releasing it truthfully reported
		// success and the next lease was refused identically. Report what
		// still stands in the way, from the same computation the
		// acquisition paths use (keyless: what any OTHER caller now sees).
		if av := pool.SlotAvailability(dir, ""); av.State != pool.SlotFree {
			fmt.Fprintf(stdout, "  note: the slot is still %s for other callers — %s\n", av.State, av.Detail())
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "simpool release:", err)
		return 1
	}
	if len(released) == 0 {
		fmt.Fprintf(stdout, "no lease to release for key %q\n", k)
		reportFlockHeldSlots(root, stdout)
	}
	return 0
}

// reportFlockHeldSlots explains, after a release that found no lease, the
// slots that ARE held right now and by what — because the bare "no active
// lease for key …" this replaces was read, twice in one day, as `release`
// failing.
//
// It is not failing, and the confusion is structural rather than careless:
// `with` and `acquire` hold a slot with a kernel flock, which carries no
// key at all, while `release` is key-scoped because a lease is all it can
// address. So a session that took its slot with `acquire` and then called
// `release` got a message that is perfectly true and reads as an error —
// one node reported a slot orphaned that was not, another spent its time
// deciding `release` was broken. Nothing was wrong with either.
//
// Deliberately not an error and deliberately not a fix: a flock-held slot
// is not `release`'s to take, and the kernel frees it the instant its
// holder exits, with no cleanup step. The only thing missing was saying
// so. It runs only on the nothing-released path, where the reader is
// already asking "so why did nothing happen?", and never widens an actual
// release into a pool-wide scan.
func reportFlockHeldSlots(root string, stdout io.Writer) {
	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		return
	}
	var lines []string
	for _, groupDir := range groups {
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			if free, err := pool.IsSlotFree(dir); err != nil || free {
				continue
			}
			label := fmt.Sprintf("%s/slot-%d", filepath.Base(groupDir), n)
			// The holder by pid from the kernel (lsof on the lock file),
			// not from meta.json: meta is advisory and can name a pid that
			// exited, which is exactly the kind of near-miss that sent
			// someone hunting for a process that was not there. Fall back
			// to meta only when lsof says nothing, and label it as the
			// guess it is — the same distinction `simpool status` draws.
			who := ""
			if holders, _ := procs.LockHolders(pool.LockPath(dir)); len(holders) > 0 {
				var parts []string
				for _, h := range holders {
					parts = append(parts, fmt.Sprintf("pid %d", h))
				}
				who = strings.Join(parts, ", ")
			} else if meta := pool.ReadMeta(dir); meta.OwnerPID != 0 {
				who = fmt.Sprintf("pid %d (from meta.json, unverified)", meta.OwnerPID)
			}
			if who == "" {
				lines = append(lines, fmt.Sprintf("  %s is held, by a process this command could not identify", label))
				continue
			}
			lines = append(lines, fmt.Sprintf("  %s is held by a live process — %s", label, who))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(stdout, "note: nothing here is wrong. `release` drops leases, and these slots are held by a kernel lock instead:")
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	fmt.Fprintln(stdout, "  A lock like that has no key and cannot be released from outside. It is `simpool with`/`acquire` holding its own slot, and the kernel drops it the moment that process exits — including on SIGKILL, with no cleanup step. Signal the process (an `acquire` exits on SIGINT/SIGTERM/SIGHUP); do not go looking for a lease.")
}
