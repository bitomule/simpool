package pool

import (
	"fmt"
	"os"
	"path/filepath"
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
}

var liveProvisionDeps = provisionDeps{
	find:           simctl.Find,
	listDevices:    simctl.ListDevices,
	resolveRuntime: simctl.ResolveRuntime,
	create:         simctl.Create,
	bootAndWait:    simctl.BootAndWait,
	shutdown:       simctl.Shutdown,
	delete:         simctl.Delete,
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
	// bootstatus round trip (~2s measured on this machine) on top of the
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

	// Only pay for the boot-and-wait round trip when the device isn't
	// already known-booted. This is the idempotency fix for the hot path:
	// `simpool lease` on a warm slot used to call simctl.Boot unconditionally
	// (measured ~2s of pure subprocess overhead on this machine for the
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
		bootErr := deps.bootAndWait(udid, remaining)
		_ = gate.Release()
		if bootErr != nil {
			return fmt.Errorf("booting %s: %w", udid, bootErr)
		}
	}

	s.Meta.Device = s.Device
	s.Meta.OSVersion = s.OSVer
	s.Meta.UDID = udid
	s.Meta.LastUsed = time.Now()
	s.Meta.OwnerPID = os.Getpid()
	s.Meta.OwnerCmd = ownerCmd
	s.Meta.Mode = mode
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
