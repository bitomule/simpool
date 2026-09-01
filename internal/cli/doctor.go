package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
	"github.com/bitomule/simpool/internal/simctl"
)

// stuckGrace mirrors reap's default --stuck-after: a `with` holder that has
// had zero live children for less than this is presumed to be mid-startup
// (booting a fresh simulator, resolving a runtime), not stuck.
const stuckGrace = 3 * time.Minute

// findDevice abstracts simctl.Find so tests can inject device-set state
// (a meta.json pointing at some other slot's live device, a device under an
// unexpected name) without touching the real device set — mirrors
// pool.liveProvisionDeps' seam.
var findDevice = simctl.Find

// RunDoctor implements `simpool doctor`: a read-only coherence check.
// Non-zero exit means something is wrong; it never modifies the pool
// itself (that's reap's job).
func RunDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root, err := pool.Root()
	if err != nil {
		fmt.Fprintln(stderr, "simpool doctor: FAIL:", err)
		return 1
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		fmt.Fprintln(stdout, "FAIL  pool root missing or not a directory:", root)
		return 1
	}

	var problems []string
	note := func(format string, a ...any) {
		problems = append(problems, fmt.Sprintf(format, a...))
	}

	groups, err := pool.ListGroupDirs(root)
	if err != nil {
		fmt.Fprintln(stderr, "simpool doctor:", err)
		return 1
	}

	for _, groupDir := range groups {
		group := filepath.Base(groupDir)
		for _, n := range pool.ListSlotNumbers(groupDir) {
			dir := pool.SlotDir(groupDir, n)
			label := fmt.Sprintf("%s/slot-%d", group, n)

			meta := pool.ReadMeta(dir)
			free, err := pool.IsSlotFree(dir)
			if err != nil {
				note("%s: could not check lock: %v", label, err)
				continue
			}

			if lease, err := pool.ReadLease(dir); err != nil {
				// doctor is read-only and diagnostic, not a gate on
				// handout — surface the failure instead of silently
				// dropping it (see ReadLease's doc comment on why this
				// must never be read as "no lease" by a caller that hands
				// slots out).
				note("%s: could not read lease.json: %v — treat as occupied until this is resolved", label, err)
			} else if lease.Alive() && !free {
				// Should never happen: `with`/`acquire` refuse a slot with
				// a live lease (see AcquireSlots' take()), and a lease
				// claim refuses a slot whose flock is held (see
				// claimSlotForLease) — both routed through the same group
				// allocation lock. Seeing both at once means two consumers
				// may be sharing one simulator right now, exactly what
				// simpool exists to prevent.
				note("%s: lock is busy while lease key %q is also alive on this slot — two consumers may be sharing one simulator, this should never happen", label, lease.Key)
			}

			if meta.UDID != "" {
				if entry, found, err := findDevice(meta.UDID); err == nil {
					if !found {
						note("%s: meta.json references device %s which no longer exists", label, meta.UDID)
					} else if !pool.IsPoolName(entry.Name) {
						// The default device set also holds the user's own
						// simulators; meta.json pointing at one of those
						// would be a serious bug elsewhere in simpool, not
						// something to paper over here.
						note("%s: meta.json references device %s (name %q) that is NOT pool-owned — this should never happen", label, meta.UDID, entry.Name)
					} else if want := pool.DeviceNameForGroup(root, group, n); entry.Name != want {
						// The expected name is derived from this slot's own
						// directory (root, group, slot number), never from
						// meta.Device/meta.OSVersion — see DeviceNameForGroup's
						// doc comment in paths.go for why deriving it from the
						// meta.json under audit would make this check
						// self-referential and blind to a meta.json that is
						// internally coherent but points at another group's
						// (or another pool root's) live simulator.
						if tag, ok := poolDeviceTag(entry.Name); ok && tag != pool.RootTag(root) {
							note("%s: meta.json's device %s (name %q) belongs to another simpool root %s, not this pool's root %s — two consumers may be pointed at one simulator", label, meta.UDID, entry.Name, tag, pool.RootTag(root))
						} else {
							note("%s: meta.json's device %s is named %q, expected %q", label, meta.UDID, entry.Name, want)
						}
					}
				}
			}

			if free {
				if poison := pool.CheckPoison(meta); poison.Poisoned() {
					// The one shared computation `status` reports from (see
					// pool.SlotState): doctor is named in that contract as a
					// consumer of it and must not reach its own, contradicting
					// verdict about the same slot. Only av.OwnLeaseResidue is
					// taken from it here — the arms below say things
					// SlotAvailability deliberately does not model (which of
					// the residue recovery paths can act on this slot).
					av := pool.SlotAvailability(dir, "")
					switch {
					case av.State == pool.SlotLeased:
						// A live lease holds this slot right now. The
						// UDID-carrying processes CheckPoison sees are that
						// session's own tools at work (`simctl spawn <udid>
						// log stream`, `axe --udid`, the booted app itself) —
						// AttemptRecovery's own doc calls a user-owned process
						// referencing the UDID the healthy case. The shared
						// computation reports this slot as leased; calling it
						// quarantined here was doctor contradicting `status`
						// about a slot with nothing wrong in it.
					case av.OwnLeaseResidue:
						// Quarantined against everyone EXCEPT the key whose
						// own previous lease session left these processes
						// behind — the exemption pool.ownLeaseResidue exists
						// for, which covers BOTH consumer-residue reasons
						// (LiveConsumers and OrphanedResidue), so this arm
						// must be consulted before the per-reason arms below:
						// sending the operator to manual surgery (or
						// `--disown-poisoned`) about a slot the owning key's
						// next `simpool lease` claims as-is was doctor
						// contradicting `status` (which names the key).
						if poison.Reason == pool.PoisonedByOrphanedResidue && pool.ResidueDeviceVerified(root, group, n, meta) {
							// Here the residue itself IS reclaimed
							// automatically: the device is independently
							// confirmed, by name, to be this exact slot's
							// own, so the owning key's next `simpool lease`
							// (via claimSlotForLease's own AttemptRecovery
							// call), any other acquisition, or `simpool reap`
							// kills the residue pid(s) without operator
							// action.
							note("%s: lock is free but its consumer is still alive (device %s) — %s; device confirmed this slot's own, so the residue is reclaimed automatically on that key's next `simpool lease`, any other acquisition, or `simpool reap` — no operator action is needed", label, meta.UDID, av.Detail())
						} else {
							note("%s: lock is free but its consumer is still alive (device %s) — %s; no reclaim is needed or will happen automatically — the owning key's next `simpool lease` claims it as-is, no operator action is needed", label, meta.UDID, av.Detail())
						}
					case poison.Reason == pool.PoisonedByOrphanedResidue && pool.ResidueDeviceVerified(root, group, n, meta):
						// The automatic path (see reap.go's identical
						// branching) CAN reclaim this one: the device is
						// independently confirmed, by name, to be this
						// exact slot's own — reap's next run (or the next
						// acquisition) will kill the residue process(es)
						// without any operator action.
						note("%s: lock is free but its consumer is still alive (device %s, %s) — device confirmed this slot's own; will be reclaimed automatically on the next acquisition or `simpool reap`", label, meta.UDID, poison)
					case poison.Reason == pool.PoisonedByOrphanedResidue && !poison.ResidueDisownable():
						// Running device, identity unconfirmed: neither the
						// automatic path (which needs deviceBelongsToSlot)
						// nor --disown-poisoned (which needs a device
						// confirmed not running — see
						// pool.Poison.ResidueDisownable) can act. Say so
						// rather than sending the operator to a flag that
						// will refuse.
						note("%s: lock is free but its consumer is still alive (device %s, %s) — device is running but could not be confirmed to be this slot's own, and `--disown-poisoned` deliberately will not act on a running device either; fix or clear meta.json's device reference for this slot", label, meta.UDID, poison)
					case poison.Reason == pool.PoisonedByOrphanedResidue:
						// deviceBelongsToSlot can only ever succeed against a
						// device that still exists (see
						// reclaimOrphanedResidue' own doc comment) — for a
						// deleted device (the real production incident this
						// whole feature exists to fix) that guard can NEVER
						// pass, so the generic "will be reclaimed
						// automatically" message above would be structurally
						// false here: automatic recovery is permanently
						// unreachable for this exact slot, not merely
						// unlucky on this particular run. Point at the one
						// path that actually can, `--disown-poisoned` — the
						// same escape hatch `reap`'s own SKIP message names.
						note("%s: lock is free but its consumer is still alive (device %s, %s) — device's identity as this slot's own could not be confirmed (deleted, or named for something else), so this will NEVER be reclaimed automatically; run `simpool reap --disown-poisoned` to kill the residue pid(s) and forget this slot's stale device reference", label, meta.UDID, poison)
					case meta.Mode == "with" && poison.Reason == pool.PoisonedByConsumerPGID:
						// The ONLY combination pool.AttemptRecovery's
						// ConsumerPGID branch can act on: a `with`-launched
						// process group simpool itself spawned. Even here it
						// isn't guaranteed (an unverifiable fingerprint or a
						// kill that doesn't stick leaves it quarantined), so
						// it is still worth flagging.
						note("%s: lock is free but its consumer is still alive (device %s, %s) — will be reclaimed automatically on the next acquisition or `simpool reap` if its identity can still be verified", label, meta.UDID, poison)
					default:
						// Everything else — a non-"with" slot, or a `with`
						// slot poisoned by LiveConsumers or by a check that
						// failed — is refused by pool.AttemptRecovery before
						// identity is ever considered, so promising
						// eventual automatic reclaim here would be false for
						// every one of them.
						note("%s: lock is free but its consumer is still alive (device %s, %s) — nothing reclaims this automatically (only a `with`-mode slot poisoned by its own process group is ever recovered); it stays quarantined until whatever still holds the device exits", label, meta.UDID, poison)
					}
				}
				continue
			}

			if meta.Mode != "with" {
				// `acquire` holders are supposed to have zero children for
				// their entire lifetime; that is not a coherence problem
				// (mirrors reap.go's reapHeldSlot — keep both in sync).
				continue
			}

			holders, err := procs.LockHolders(pool.LockPath(dir))
			if err != nil {
				note("%s: could not determine lock holder: %v", label, err)
				continue
			}
			if len(holders) == 0 {
				continue // released between our check and lsof; not a real problem
			}
			for _, h := range holders {
				// lsof reports every process with the lock file open, not
				// just whichever one holds the flock (Darwin doesn't
				// reliably distinguish the two) — a concurrent `status`/
				// `doctor`/`reap` probe that briefly opened the same path
				// would otherwise be misread as a stuck holder. meta.OwnerPID
				// is the pid EnsureProvisioned recorded for whichever process
				// actually completed provisioning while holding this lock,
				// so requiring an exact match is the corroborating signal
				// (mirrors reap.go's reapHeldSlot — keep both in sync).
				if h != meta.OwnerPID || !procs.Alive(h) || !procs.IsSimpoolHolder(h, "with") {
					continue
				}
				children, _ := procs.ChildPIDs(h)
				if len(children) == 0 && time.Since(meta.LastUsed) >= stuckGrace {
					note("%s: held by pid %d with no live child work for %s — stuck, run `simpool reap`", label, h, time.Since(meta.LastUsed).Round(time.Second))
				}
			}
		}
	}

	// Reported, but deliberately not folded into `problems`: the exit code
	// documents pool coherence, and a deleted device's surviving userland
	// belongs to no slot and no pool root — it is machine-wide debris that
	// says nothing about whether this pool is consistent. Failing on it
	// would make `doctor` start returning non-zero for something no caller
	// of it is asking about, which is how a useful signal gets suppressed
	// with `|| true`.
	if orphaned, ok := findOrphanedRuntimes(stderr); ok {
		total := 0
		for _, g := range orphaned {
			total += len(g.PIDs)
		}
		if total > 0 {
			fmt.Fprintf(stdout, "WARN %d process(es) from %d deleted simulator(s) are still running — they belong to no device any more and nothing else will ever collect them; run `simpool reap --purge-orphan-runtimes` to kill them\n", total, len(orphaned))
		}
	}

	if len(problems) == 0 {
		fmt.Fprintln(stdout, "OK   pool is coherent:", root)
		return 0
	}
	for _, p := range problems {
		fmt.Fprintln(stdout, "FAIL ", p)
	}
	return 1
}

// poolDeviceTag extracts the RootTag embedded in a pool-owned simulator
// name (pool.NamePrefix + 8 lowercase-hex chars + "_" + group + "_slot-N"),
// reporting ok=false for anything that doesn't have that shape — including
// names that merely start with the prefix but aren't well-formed pool names.
func poolDeviceTag(name string) (tag string, ok bool) {
	rest := strings.TrimPrefix(name, pool.NamePrefix)
	if rest == name || len(rest) < 9 || rest[8] != '_' {
		return "", false
	}
	tag = rest[:8]
	for _, c := range tag {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", false
		}
	}
	return tag, true
}
