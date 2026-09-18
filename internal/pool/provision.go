package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// DefaultBootTimeout bounds how long EnsureProvisioned will wait for a
// simulator to finish booting (see simctl.BootAndWait) before giving up
// with a clear, actionable error instead of hanging the caller forever. A
// cold boot has been measured at ~110s on real hardware under load; 180s
// leaves headroom above that without leaving a caller stuck indefinitely on
// a device that is genuinely wedged. Override with SIMPOOL_BOOT_TIMEOUT (a
// Go duration string, e.g. "4m").
const DefaultBootTimeout = 180 * time.Second

// EnvBootTimeout overrides DefaultBootTimeout.
const EnvBootTimeout = "SIMPOOL_BOOT_TIMEOUT"

// BootTimeout resolves the effective boot-wait timeout: SIMPOOL_BOOT_TIMEOUT
// if set to a valid positive duration, else DefaultBootTimeout.
func BootTimeout() time.Duration {
	if v := os.Getenv(EnvBootTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultBootTimeout
}

// provisionDeps abstracts every simctl entry point EnsureProvisioned calls.
// Its own decision logic — skip the boot-and-wait entirely for a device
// that's already booted, propagate a timeout as a clear error rather than
// hanging, never treat a failed state check as "ready" — is exactly the
// kind of thing a previous review found this package shipped with zero
// unit coverage for. Faking these lets provision_test.go exercise that
// logic directly instead of shelling out to the real `xcrun simctl`, which
// the default `go test ./...` run must never do (see
// SIMPOOL_RUN_INTEGRATION in internal/cli/integration_test.go).
type provisionDeps struct {
	find           func(udid string) (simctl.DeviceEntry, bool, error)
	listDevices    func() ([]simctl.DeviceEntry, error)
	resolveRuntime func(device, osVersion string) (runtimeID, deviceTypeID string, err error)
	create         func(name, deviceTypeID, runtimeID string) (string, error)
	bootAndWait    func(udid string, timeout time.Duration) error
	// shutdown and delete are only ever invoked by the substance-mismatch
	// path below (see resolveSubstanceMismatch) — never by the ordinary
	// adopt-or-create flow.
	shutdown func(udid string) error
	delete   func(udid string) error
	// slim brings a device to the configured slim profile before it is
	// handed out (see slim.go). Faked in tests for the same reason as
	// everything else here: the real one boots and reboots a simulator.
	//
	// A nil slim is treated as "slimming disabled", not as a crash: the
	// fakes that predate slimming exercise the adopt/create/boot logic
	// this field has nothing to do with, and making each of them carry a
	// no-op would be noise. liveProvisionDeps always sets it, and
	// TestLiveProvisionDepsSlims guards exactly that.
	slim func(udid string, cats []string, timeout time.Duration) (bool, error)
	// restore puts a slot back into the pool's baseline presentation
	// before it changes hands (see presentation.go). Faked for the same
	// reason slim is; a nil restore means "disabled", so the fakes that
	// predate it are unaffected.
	restore func(udid string, want Presentation, timeout time.Duration) ([]string, error)
}

var liveProvisionDeps = provisionDeps{
	find:           simctl.Find,
	listDevices:    simctl.ListDevices,
	resolveRuntime: simctl.ResolveRuntime,
	create:         simctl.Create,
	bootAndWait:    simctl.BootAndWait,
	shutdown:       simctl.Shutdown,
	delete:         simctl.Delete,
	slim:           SlimDevice,
	restore:        RestorePresentation,
}

// EnvStrictSubstance, when set to "0", disables substance verification and
// falls back to the old name-only reuse — an emergency escape hatch, not a
// recommended setting: without it, a device that has drifted out from under
// its slot (a runtime Xcode replaced, one that's gone isAvailable:false)
// gets adopted and re-adopted forever, failing every boot with an opaque
// error instead of being replaced once, here.
const EnvStrictSubstance = "SIMPOOL_STRICT_SUBSTANCE"

func strictSubstanceEnabled() bool {
	return os.Getenv(EnvStrictSubstance) != "0"
}

// substanceOK reports whether entry is actually usable as the device the
// caller resolved device/osVersion to — not just named right. Matching by
// name alone lets a device survive a runtime upgrade that quietly replaced
// what it used to point at: "latest" drifts across Xcode upgrades, and a
// name match alone cannot tell a healthy device from one that has gone
// isAvailable:false or is still parked on a runtime that no longer exists.
func substanceOK(entry simctl.DeviceEntry, runtimeID, deviceTypeID string) bool {
	return entry.IsAvailable && entry.RuntimeID == runtimeID && entry.DeviceTypeID == deviceTypeID
}

// substanceMismatchReason renders a human-readable explanation of why entry
// failed substanceOK, for the log line resolveSubstanceMismatch prints and
// for actionable errors when the mismatch can't be safely acted on.
func substanceMismatchReason(entry simctl.DeviceEntry, runtimeID, deviceTypeID string) string {
	switch {
	case !entry.IsAvailable:
		return "device reports isAvailable=false"
	case entry.RuntimeID != runtimeID:
		return fmt.Sprintf("device runtime %s does not match resolved runtime %s", entry.RuntimeID, runtimeID)
	case entry.DeviceTypeID != deviceTypeID:
		return fmt.Sprintf("device type %s does not match resolved device type %s", entry.DeviceTypeID, deviceTypeID)
	default:
		return "substance mismatch"
	}
}

// resolveSubstanceMismatch decides what to do about an adopted device (udid,
// already confirmed by the caller to be exactly this slot's own by name —
// see DeviceNameForGroup) whose substance doesn't match what was requested.
// It only ever deletes and signals recreation once every guard can be
// positively confirmed: this slot's poison state is NotPoisoned (no
// evidence, and no unverifiable evidence either, of a still-alive previous
// consumer) and no live lease belonging to a DIFFERENT caller sits on the
// slot. Any guard that cannot be confirmed — a poison check that itself
// failed, an unreadable lease.json — returns an error instead of ever
// silently reusing OR silently deleting: a stale-runtime simulator handed to
// a caller is a slow, confusing failure, but destroying a simulator out from
// under an active session is worse, so when in doubt this always chooses the
// error over either extreme.
//
// ownLeaseKey is the caller's OWN lease key, if this call is happening on
// the `simpool lease` path — empty for `with`/`acquire`, which never hold a
// lease at all. It matters because AcquireLease writes lease.json for its
// own key BEFORE calling EnsureProvisioned (so the flock-free reservation is
// visible to any concurrent claimant immediately, not just once provisioning
// finishes): by the time this function runs on that path, ReadLease below is
// reading the very lease the current call itself just wrote milliseconds
// earlier, not evidence of some OTHER live consumer. Without this
// distinction every `simpool lease` substance mismatch would refuse itself
// unconditionally — the guard built for "someone else is using this slot"
// firing on "I am using this slot", turning a self-healing drift (an Xcode
// upgrade replacing a runtime) into a permanent wedge that never ages out,
// since sticky renewal keeps re-extending this exact lease's TTL on every
// subsequent call. A lease for a different key is exactly what this must
// keep refusing to touch — that is the real exclusion mechanism during
// provisioning (see AcquireLease's doc comment: the slot flock is released
// before this runs).
func resolveSubstanceMismatch(s *Slot, deps provisionDeps, udid, reason, ownLeaseKey string) error {
	group := filepath.Base(s.GroupDir)
	label := fmt.Sprintf("%s/slot-%d", group, s.Number)

	if poison := CheckPoison(s.Meta); poison.Reason == PoisonedByCheckFailure {
		return fmt.Errorf("%s: device %s does not match requested substance (%s), but its previous consumer's liveness could not be verified — refusing to delete or reuse it: %v", label, udid, reason, poison.Err)
	} else if poison.Poisoned() {
		return fmt.Errorf("%s: device %s does not match requested substance (%s), but its previous consumer still appears alive (%s) — refusing to delete or reuse it", label, udid, reason, poison)
	}

	lease, err := ReadLease(s.Dir)
	if err != nil {
		return fmt.Errorf("%s: device %s does not match requested substance (%s), but lease.json could not be read — refusing to delete or reuse it: %w", label, udid, reason, err)
	}
	if lease.Alive() && lease.Key != ownLeaseKey {
		return fmt.Errorf("%s: device %s does not match requested substance (%s), but a live lease (key %q) is on this slot — refusing to delete or reuse it", label, udid, reason, lease.Key)
	}

	fmt.Fprintf(os.Stderr, "simpool: %s device %s does not match requested substance (%s) — deleting and recreating\n", label, udid, reason)
	if err := deps.shutdown(udid); err != nil {
		return fmt.Errorf("%s: shutting down %s before recreating for a substance mismatch: %w", label, udid, err)
	}
	if err := deps.delete(udid); err != nil {
		return fmt.Errorf("%s: deleting %s for a substance mismatch: %w", label, udid, err)
	}
	return nil
}

// reconcileSlim brings udid to the slim profile this process resolved,
// whether that means disabling daemons for the first time or re-enabling
// the ones a --need asked for. simslim is idempotent by construction, so
// on a slot already in the requested state this is a launchctl read and
// nothing else.
//
// It never fails the acquisition. A slot whose profile could not be
// applied is still a usable slot, and taking a test run down over it
// would be worse than the problem — but it says so on stderr with the
// consequence spelled out, because a caller that asked for a capability
// and did not get it will otherwise read the failure as "the simulator
// cannot do this", which is the reading that ends in `simctl create`.
func reconcileSlim(deps provisionDeps, udid string, cats []string) {
	changed, err := deps.slim(udid, cats, SlimTimeout())
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "simpool: could not apply the slim profile to %s (%v) — the simulator is usable but its daemons are in whatever state they were already in, so anything you asked for with --need may still be disabled\n", udid, err)
	case changed:
		fmt.Fprintf(os.Stderr, "simpool: reconfigured %s to the requested slim profile (this reboots the simulator, and only happens when the profile actually changes)\n", udid)
	}
}

// reconcilePresentation puts a slot back into the pool's baseline
// language, region, appearance, text size, contrast and status bar before
// it is handed to a consumer that is not the one who last had it.
//
// # Why here and not on release
//
// Because the consumer that leaves a slot dirty is usually not around to
// clean it. `with`'s release is a deferred cleanup that a SIGKILL skips
// outright; `acquire` and `lease` have no release step at all in the crash
// case — a lease simply expires, with nobody running. Design for the node
// that dies halfway through, not the one that finishes politely, and
// "restore on the way out" restores nothing in exactly the case that
// produced this bug.
//
// # Why here and not in reap
//
// reap is a scheduled disk-reclaim pass over cold slots. It is not an
// admission gate and cannot be one: a slot dirtied at 10:00 and handed out
// at 10:05 meets no reap until 11:00, and reap never touches a slot that
// stayed booted, which in this pool is most of them. Only hand-out is
// guaranteed to run between "the previous consumer stopped" and "the next
// consumer sees it", whatever happened in between.
//
// # Why here specifically
//
// This is already the line where this exact shape of bug was fixed once
// this month. The slim reconcile used to live inside the cold-boot branch,
// so a warm slot came back in 1.6s with the capability it had been asked
// for still disabled. A slot handed out without what was asked for and a
// slot handed out with something nobody asked for are the same defect seen
// from its two sides, and they belong in the same place.
//
// Never fails the acquisition, for the same reason reconcileSlim doesn't: a
// slot whose appearance could not be read is still a usable simulator, and
// taking a test run down over it would be worse than the drift. It does say
// so on stderr, naming every setting it reset — the whole reason this went
// unnoticed for months is that a dirty slot looked exactly like a clean one
// until somebody read a published screenshot.
func reconcilePresentation(deps provisionDeps, udid string) []string {
	want := BaselinePresentation()
	changed, err := deps.restore(udid, want, DefaultPresentationTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "simpool: could not restore %s to the pool's baseline presentation (%v) — the simulator is usable, but it may still carry the previous consumer's language, appearance or text size, which would silently change what your screenshots look like\n", udid, err)
		return nil
	}
	if len(changed) > 0 {
		fmt.Fprintf(os.Stderr, "simpool: %s came back from its previous consumer with %s — reset to the pool baseline (%s/%s)\n", udid, strings.Join(changed, ", "), want.Language, want.Region)
	}
	return changed
}

// EnsureProvisioned makes sure s has a booted simulator matching
// s.Device/s.OSVer, in the shared default device set, named
// s.DeviceName() (see pool.DeviceName). Creates one if this is a fresh
// slot, its recorded UDID no longer exists, or its recorded UDID exists
// but under a different name than expected — meta.json is advisory, not
// authoritative, so it is never trusted blindly for something as
// consequential as "which simulator is this". It always blocks until the
// device is genuinely ready before returning, since the caller (MAV, a
// test runner) expects a usable simulator, not a UDID it has to poll or
// retry against itself — see simctl.BootAndWait.
//
// Deliberately does NOT hold any group-wide lock while it waits: callers
// (AcquireSlots' take(), AcquireLease's claimSlotForLease) already release
// the group allocation lock before invoking this — see claimSlotLock's doc
// comment — and this function only ever touches the one slot handed to it.
// A slow cold boot here therefore blocks nothing beyond this one slot's own
// caller; every other concurrent acquisition in the same (or any other)
// device+OS group proceeds unaffected.
//
// mode records which subcommand is provisioning ("with", "acquire", or
// "lease") so `reap`/`doctor` can tell a legitimately child-less `acquire`
// holder apart from a stuck `with` (see Meta.Mode).
//
// leaseKey is the caller's own sticky lease key on the `simpool lease` path
// (see RunLease) — pass "" for `with`/`acquire`, which never hold a lease.
// It is threaded through to resolveSubstanceMismatch so a substance
// mismatch on the lease path can tell "my own just-written lease" apart
// from "someone else's live lease" — see that function's doc comment.
func EnsureProvisioned(s *Slot, ownerCmd, mode, leaseKey string) error {
	return ensureProvisioned(s, ownerCmd, mode, leaseKey, liveProvisionDeps, BootTimeout())
}

func ensureProvisioned(s *Slot, ownerCmd, mode, leaseKey string, deps provisionDeps, bootTimeout time.Duration) error {
	name := s.DeviceName()
	udid := s.Meta.UDID
	// knownState carries a device's already-known state (from the Find or
	// ListDevices lookup below) into the boot decision, so a device this
	// call can already prove is "Booted" never pays for a redundant
	// bootstatus round trip (~2s measured) on top of the
	// lookup that already told us so. Left "" whenever no lookup happened
	// to report a state (the fresh-create path never has one to report,
	// since a just-created device is never anything but Shutdown), which
	// simply means the boot-and-wait step below always runs — the safe
	// default.
	knownState := ""

	// Hoisted to the top: both the reuse path (to verify substance — see
	// substanceOK) and the create path need this, and it was already being
	// called on both paths separately before this change, so this is a net
	// simplification, not an extra subprocess. resolveErr is deliberately
	// not returned here: a device this call can otherwise adopt outright
	// must not fail to hand out just because ResolveRuntime hiccuped —
	// only the create path (which has nothing else to fall back on) and
	// the substance check (which has nothing to compare against) treat it
	// as blocking, below.
	runtimeID, deviceTypeID, resolveErr := deps.resolveRuntime(s.Device, s.OSVer)

	var adopted simctl.DeviceEntry
	haveAdopted := false

	if udid != "" {
		entry, found, err := deps.find(udid)
		if err != nil {
			return fmt.Errorf("checking existing device %s: %w", udid, err)
		}
		// Require an exact name match, not just "found": meta.json can be
		// stale or corrupt, and the default device set also holds every
		// other slot's simulators plus the user's own — a UDID alone is
		// not proof this device belongs to this slot. Refusing anything
		// less than an exact match is what makes the recovery-by-name
		// path below the only way forward when in doubt, instead of ever
		// silently reusing (or shutting down/deleting elsewhere) a device
		// that might not actually be ours.
		if !found || entry.Name != name {
			udid = ""
		} else {
			knownState = entry.State
			adopted = entry
			haveAdopted = true
		}
	}

	if udid == "" {
		// meta.json is advisory and can be lost entirely (crash mid-write,
		// disk full, a human `rm`). Before creating a new simulator, look
		// for one already sitting in the default set under the
		// deterministic, root+slot-unique name we always use — otherwise a
		// lost meta.json leaks the previous device forever, since nothing
		// else in simpool ever inspects device-set contents directly.
		if existing, err := deps.listDevices(); err == nil {
			var matches []simctl.DeviceEntry
			for _, d := range existing {
				if d.Name == name {
					matches = append(matches, d)
				}
			}
			// simctl itself does not enforce unique names — verified by
			// creating two devices with the same name directly — so this is
			// not just theoretical. Recovering from an arbitrary one of
			// several matches would be non-deterministic across runs (Go's
			// map iteration order for `simctl list devices -j`'s JSON is
			// randomized) and could silently hand two different callers two
			// different simulators for what they each believe is the same
			// slot. Refuse loudly instead of guessing; this should never
			// happen given DeviceName's own uniqueness contract, so it is
			// always worth surfacing rather than working around.
			if len(matches) > 1 {
				return fmt.Errorf("%d simulators are named %q in the default device set — refusing to guess which one belongs to this slot; delete the duplicates manually", len(matches), name)
			}
			if len(matches) == 1 {
				udid = matches[0].UDID
				knownState = matches[0].State
				adopted = matches[0]
				haveAdopted = true
			}
		}
	}

	// Substance verification: a name match alone is not proof this device
	// is actually usable — see substanceOK's doc comment. Skipped (falls
	// back to the old name-only reuse) when SIMPOOL_STRICT_SUBSTANCE=0, or
	// when resolveErr means there is nothing to compare against anyway (the
	// create path below will surface that same error if it turns out there
	// really is nothing usable at all).
	if haveAdopted && resolveErr == nil && strictSubstanceEnabled() && !substanceOK(adopted, runtimeID, deviceTypeID) {
		reason := substanceMismatchReason(adopted, runtimeID, deviceTypeID)
		if err := resolveSubstanceMismatch(s, deps, udid, reason, leaseKey); err != nil {
			return err
		}
		// Guards confirmed safe and the mismatched device is now gone —
		// fall through to the create path below exactly as if nothing had
		// ever been found under this name.
		udid = ""
		knownState = ""
		haveAdopted = false
	}

	if udid == "" {
		if resolveErr != nil {
			return resolveErr
		}
		newUDID, err := deps.create(name, deviceTypeID, runtimeID)
		if err != nil {
			return fmt.Errorf("creating simulator: %w", err)
		}
		udid = newUDID
		s.Meta.Created = time.Now()
		s.Meta.RuntimeID = runtimeID
		// A device simctl.Create just minted is never anything but
		// Shutdown — no lookup ran to tell us so, but none is needed
		// either: knownState is left "" (meaning "unknown/not booted"),
		// which is exactly what makes the boot-and-wait step below run
		// unconditionally for it.
	} else if haveAdopted {
		// MAV_TARGET_RUNTIME must always describe the same device
		// MAV_TARGET_UDID does — see Meta.RuntimeID's doc comment — so this
		// is set from the adopted device's OWN actual runtime, never from
		// what ResolveRuntime would have resolved for a fresh request; the
		// two are only guaranteed to agree for a device this same call just
		// created (the branch above).
		s.Meta.RuntimeID = adopted.RuntimeID
	}

	// A slot that is already booted still has to have its slim profile
	// reconciled, and for a year it did not: the whole slim step lived
	// inside the `knownState != "Booted"` branch below, so a caller asking
	// for a capability got a warm slot back in under two seconds with the
	// daemons it asked for still disabled, and no error anywhere. Measured
	// on this pool: `SIMPOOL_SLIM_EXCEPT=photos simpool with` returned
	// slot-2 in 1.6s with com.apple.assetsd still disabled and
	// `simctl addmedia` still failing PHPhotosErrorDomain 3301 — which is
	// how two nodes concluded a slot could not do the job and went off to
	// run `simctl create` instead.
	//
	// Deliberately outside the boot-concurrency gate, unlike the cold path
	// below: this device's userland is already resident, so the reboot
	// simslim performs when the profile actually differs adds no new peak
	// for the gate to protect against. In the common case — the profile
	// already matches — it is one launchctl read and no reboot at all.
	effectiveCats := EffectiveCategories(s.Meta.Capabilities, RequestedCategories())
	if knownState == "Booted" && SlimEnabled() && deps.slim != nil {
		reconcileSlim(deps, udid, effectiveCats)
	}

	// Only pay for the boot-and-wait round trip when the device isn't
	// already known-booted. This is the idempotency fix for the hot path:
	// `simpool lease` on a warm slot used to call simctl.Boot unconditionally
	// (measured ~2s of pure subprocess overhead for the
	// no-op "already booted" case) on top of whatever lookup above already
	// told us the device's state for free. knownState is only ever "Booted"
	// here when a lookup this call already had to do anyway (deps.find or
	// deps.listDevices, both above) reported it — never a separate check
	// invented just for this — and a state-check failure (knownState left
	// "") always falls through to the wait below rather than ever being
	// read as "ready".
	//
	// Trusting a bare "Booted" here (without re-running bootstatus) is safe
	// specifically because of simpool's own exclusivity guarantee: a
	// SIMPOOL_-owned device only ever transitions into "Booted" via this
	// exact function's own bootAndWait call below (nothing else in this
	// codebase ever boots one), and only one process at a time can ever be
	// inside EnsureProvisioned for a given slot (its flock, or its lease's
	// mutual exclusion with the flock — see claimSlotForLease). So a
	// "Booted" read here can only ever be the tail of a PRIOR call's own
	// bootAndWait — margin already slept, never a boot this same call is
	// racing partway through. This is a different fast path than skipping
	// the wait mid-boot: that would reproduce the exact bug being fixed.
	if knownState != "Booted" {
		// Machine-wide boot-concurrency gate (see bootgate.go): held only
		// across the boot itself, never the rest of provisioning or the
		// slot's own lifetime, and always acquired AFTER the slot's own
		// flock (which the caller already holds at this point — see
		// AcquireBootGate's doc comment for why that fixed order can never
		// deadlock).
		//
		// bootTimeout is the caller's whole budget for "have a booted,
		// ready simulator" — the gate wait and the boot itself must share
		// ONE deadline, not each get their own full bootTimeout, or a
		// caller already at the boot-concurrency cap could wait up to
		// bootTimeout for the gate and then be handed another full
		// bootTimeout for the boot, doubling the one bound this codebase
		// promises on the path mav's target_command hits roughly once a
		// minute with nothing to retry it.
		deadline := time.Now().Add(bootTimeout)
		gate, err := AcquireBootGate(s.Root, time.Until(deadline))
		if err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining < simctl.MinBootBudget {
			_ = gate.Release()
			return fmt.Errorf("simpool: the boot-concurrency gate took long enough to free up that only %s remained of the %s boot timeout — too little to attempt a real boot; the machine may be overloaded with simultaneous boots, try again or override %s/%s", remaining.Round(time.Millisecond), bootTimeout, EnvBootTimeout, EnvBootConcurrency)
		}
		// Slimming happens inside the boot gate, before the boot-and-wait
		// below, because it boots the device itself (and reboots it, the
		// one time it has anything to change) — exactly the simultaneous
		// cold boots the gate exists to keep off a memory-constrained
		// machine. It gets its own budget rather than a share of
		// bootTimeout: reconfiguring 170 launchd labels and rebooting is
		// minutes of work the first time, and charging it to a timeout
		// sized for a single boot would fail every slot's first
		// provisioning. Later acquisitions of the same slot read the
		// overrides, find them already in place, and return in about as
		// long as a `simctl list` takes.
		if SlimEnabled() && deps.slim != nil {
			reconcileSlim(deps, udid, effectiveCats)
			// The slim step consumed its own budget, not the caller's, so
			// the boot-and-wait below starts from a full bootTimeout
			// again rather than from whatever minutes of a 180s budget
			// the reconfigure left behind (usually none). The bound this
			// codebase promises therefore becomes SlimTimeout +
			// bootTimeout, and only on the one acquisition per slot that
			// actually reconfigures anything; every other path is
			// unchanged.
			remaining = bootTimeout
		}
		bootErr := deps.bootAndWait(udid, remaining)
		_ = gate.Release()
		if bootErr != nil {
			return fmt.Errorf("booting %s: %w", udid, bootErr)
		}
	}

	// After the boot, never before it: every read and write here goes
	// through `simctl spawn` or `simctl ui`, which need a running
	// simulator. And only when the slot actually changed hands — a sticky
	// lease renewal is the same consumer coming back for its next command,
	// and resetting the language underneath it would break the very
	// screenshot matrix this exists to protect (see Slot.Renewed).
	if !s.Renewed && RestoreEnabled() && deps.restore != nil {
		reconcilePresentation(deps, udid)
	}

	s.Meta.Device = s.Device
	s.Meta.OSVersion = s.OSVer
	s.Meta.UDID = udid
	s.Meta.LastUsed = time.Now()
	s.Meta.OwnerPID = os.Getpid()
	s.Meta.OwnerCmd = ownerCmd
	s.Meta.Mode = mode
	// Recorded after the reconcile above, never before it: this is what the
	// next acquisition dispatches on, so it has to describe the profile the
	// device actually ended up in. When slimming is off entirely the field
	// is left alone — SIMPOOL_SLIM=0 says nothing about which categories a
	// slot has, and writing an empty set would make every slot look like it
	// satisfies nothing.
	if SlimEnabled() {
		s.Meta.Capabilities = effectiveCats
	}
	// Recorded on every mode, including the empty key `with`/`acquire`
	// pass: a slot moving from `lease` to `with` must not keep naming the
	// lease key that used to hold it, or ownLeaseResidue would go on
	// exempting a key whose session is no longer what this slot's residue
	// belongs to.
	s.Meta.LeaseKey = leaseKey
	// Always clear the previous consumer's identity here, regardless of
	// mode: `with` records its own child's ConsumerPGID/fingerprint AFTER
	// this call returns (see with.go, right after cmd.Start()), so this
	// never clobbers a value the current invocation is about to set — but
	// without it, a slot that moves from `with` to `acquire`/`lease` (which
	// never set these fields themselves) would otherwise carry a stale
	// pgid/fingerprint forever. Stale beyond "incorrect": if that old pgid
	// number is ever reused by an unrelated process group after this slot
	// has moved on, a poison check would misidentify it as a still-live
	// consumer of a `with` session that is long gone.
	s.Meta.ConsumerPGID = 0
	s.Meta.ConsumerStartedAt = ""
	return s.SaveMeta()
}
