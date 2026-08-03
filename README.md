# simpool

A broker for iOS simulators shared by several agents on one machine. Every
slot gets its own simulator in the **default** device set — the same one
Xcode, the user's own simulators, MAV, axe, and idb already use with no
special flag — identified by a unique, deterministic name
(`SIMPOOL_<roottag>_<device>@<os>_slot-<n>`, see "Pool layout" below) rather
than by a private device set. Arbitration is a real `flock()` held by a
live process, not a PID-file heuristic or a central daemon: the kernel
always releases it the instant its holder dies, with no cleanup step of
its own required. The one gap that leaves is a SIGKILL to `simpool` itself
(not its whole process group) — the lock frees but the child survives as
an orphan — and that is not silently unsafe: nothing hands a lock-free slot
to a new consumer while its old one is provably still alive, so at worst
the slot sits quarantined, never corrupted. Recovery is automatic where it
can be proven safe: `simpool with`/`acquire`/`lease` reclaim a poisoned
slot the moment anything next tries to acquire it, and `simpool reap` does
the same for anything it walks — all provided the old consumer's identity
(its process-group leader's own start time, fingerprinted under a fixed,
locale/timezone-independent environment when it was launched) can still be
verified, which is what makes killing it safe despite macOS recycling pids
(see "Architecture" below). There is no automatic idle-simulator sweep —
shutting one down is still `simpool reap --cold N`'s job, run by a human,
cron, or CI; see "Architecture" for why that boundary is deliberate.

Isolation by private device set was the original design (see git history)
and was dropped after implementing and running it: `Simulator.app` is
single-instance and tied to one set at a time, quitting it kills every
simulator booted from that set, and `axe` — MAV's accessibility-tree and
tap provider — cannot see a device in a non-default set at all
(`Simulator with UDID ... not found in set`). None of that is compatible
with a human occasionally taking the wheel of an agent's simulator, which
this tool has to support. Isolation is now by name only, enforced by the
`SIMPOOL_` prefix: reap and doctor refuse to shut down, delete, or
otherwise act on any device in the default set whose name doesn't start
with it — the user's own simulators live in that same set and must never
be touched.

See the design doc (§1–§10) for the full rationale. In short: this tool
exists because five different repos on this machine had the same simulator
UDID hardcoded, and two agents launching at once would install over each
other silently.

## Build

```
brew install bitomule/tap/simpool
```

or from source:

```
go build -o simpool .
```

Go 1.25+, stdlib only, single binary, macOS only (uses BSD `flock`,
`xcrun simctl`, `pgrep`, `lsof`, `ps`). `simpool_ios_test_runner` (see
Bazel, below) looks for the Homebrew install path first
(`/opt/homebrew/bin/simpool` or `/usr/local/bin/simpool`), so that's the
path of least resistance for CI/agent machines too.

## Usage

```
simpool with [--device D] [--os V] [--count N] [--max M] [--wait D] -- <cmd>
    Acquire N slots, export the environment, run <cmd>, release on exit.
    This is the normal way to use simpool — wrap your command in it.

simpool acquire [--device D] [--os V] [--count N] [--max M] [--wait D]
    Print the environment for N slots as shell `export` lines and hold
    the lock until signaled (SIGINT/SIGTERM/SIGHUP). For scripts that
    want the UDIDs up front and will manage the workload themselves.

simpool lease --device D --os V [--key K] [--ttl D] [--max M]
    Print just a UDID and exit — for many short, independent commands
    (mav tap/swipe/screenshot) that have nothing to hold `with`'s lock
    across. Sticky per --key (default: git repo root, else cwd); NOT a
    flock — see "MAV in the hot loop" below.

simpool release [--key K]
    Drop --key's lease immediately instead of waiting out its TTL.

simpool status
    List every slot: lock state, holder (best-effort), lease, device
    boot state.

simpool reap [--cold N] [--stuck-after D] [--purge N] [--prune-runs-after D] [--disown-poisoned] [--dry-run]
    Recycle free+cold slots. A free slot whose previous consumer still has
    a live process attached is reclaimed — killed and shut down — if its
    recorded identity (the process-group leader's own start time) still
    checks out, and left alone (quarantined) otherwise; never based on a
    bare UDID-in-argv match alone (see "Architecture" below). Also kills a
    stuck `with` holder that has no live work left under it (never an
    `acquire` holder — see "Capacity" below), prunes old run directories,
    clears expired lease files, and, with --purge, deletes long-cold
    simulators outright *and their slot directory* to reclaim disk — the
    only subcommand that actually deletes anything, by design. This is
    also the only command that ever shuts down an otherwise-idle simulator
    (`--cold N`) — schedule it (cron/launchd) if you want that to happen
    without a human running it by hand; see "Architecture" for why `with`
    doesn't do this on its own exit. Never touches a device whose name
    doesn't start with `SIMPOOL_`, no matter what meta.json says: the
    default device set also holds the user's own simulators.

    --disown-poisoned is the manual, explicit-only escape from a
    *permanent* quarantine: a slot whose recorded consumer keeps testing
    "alive" forever because its pid was recycled by an unrelated process,
    or because it belongs to another user entirely (`kill(-pgid, 0)` fails
    with EPERM) — neither of which the identity fingerprint can ever
    resolve, so ordinary `reap`/the next acquisition would SKIP it forever.
    It never signals the process behind that pgid — if it's still alive,
    it is left running, untouched, just no longer tracked by simpool — it
    only forgets this slot's identity and deletes its device (verified
    first to really be this slot's own) so the next acquisition provisions
    a genuinely new simulator. Only ever eligible for a `with` slot whose
    poison reason is the unverifiable process-group fingerprint itself —
    never a live lease/`acquire` consumer, never a liveness check that
    merely failed to run. See "Architecture" for the full reasoning,
    including why this forgets rather than force-kills.

simpool doctor
    Read-only coherence check. Exits non-zero if anything looks wrong.
```

### Provisioning: a UDID is only handed out once it's usable

Every path that hands out a slot (`with`, `acquire`, `lease`) routes through
the same provisioning step before returning: it makes sure the slot's
simulator exists, then blocks until `xcrun simctl bootstatus <udid> -b`
reports the device finished booting, plus a fixed 3s settle margin —
bootstatus's own "done" signal isn't quite enough by itself (rules_apple's
`simulator_creator.py`, the runner Bazel's own `apple_test` rules use, hits
the identical gap independently and works around it the same way). A
simulator that's slow to start makes the caller **wait**, it never fails
and it never hands back a UDID for a device that isn't actually ready yet —
this used to be a real, reproduced bug (`simpool lease` returning a UDID
whose simulator was still mid-boot, so the very next command against it
failed).

That wait is bounded: a device that never finishes booting fails with a
clear, actionable error instead of hanging forever (default 3 minutes,
override with `SIMPOOL_BOOT_TIMEOUT`, e.g. `SIMPOOL_BOOT_TIMEOUT=5m`). It's
also free when it can be: a slot whose simulator is already booted (the
common case for `lease` in a hot loop, or `with`/`acquire` reusing a warm
slot) skips the wait entirely instead of re-confirming a device that's
already known-ready.

This never holds the pool's group-wide allocation lock while it waits —
only the one slot being provisioned is affected, so a slow cold boot for
one slot never blocks acquisition attempts for any other slot, in this
group or any other.

### Capacity

Each device+OS group is capped at `--max` resident slots (default 3,
override with `SIMPOOL_MAX_SLOTS`) — booting one costs ~1.75GB (design doc
§3), so an uncapped pool turns ordinary contention into the kind of jetsam
this tool exists to prevent. Once a group is at capacity, `with`/`acquire`
poll for a free slot for up to `--wait` (default 10m; 0 fails immediately)
before giving up. `lease` counts against the same `--max` (a leased slot is
just as resident as a locked one) but never polls — see below.

### MAV in the hot loop

`with` holds its lock for the lifetime of one process — perfect for
`bazel test` or `mav run`, which are single long-lived commands. It is the
wrong shape for how MAV is actually driven interactively: an agent calls
`mav tap`, `mav swipe`, `mav screenshot`, one short independent process at
a time, dozens of them, with nothing to hold a lock across. `acquire`
doesn't fit either — it prints the environment but then blocks holding
the lock until signaled, so something still has to stay alive to release
it.

`simpool lease` is the third shape, built for exactly this. It prints one
line — the slot's UDID — and exits immediately. Point MAV's
`target_command` (`.mav/config.yaml`, MAV 0.9.1+) at it:

```yaml
target_command: simpool lease --device "iPhone 17 Pro" --os 26.3
```

Every `mav tap`/`mav swipe`/`mav screenshot` MAV runs will now call this
before each action to resolve its target. Leases are **sticky by key** —
default `--key` is the current git repo's root (or the working directory
if there's no repo) — so every call from the same repo lands on the same
slot, renewing a TTL (default ~3 minutes) each time. Two worktrees of the
same repo get two different keys, and therefore two different simulators,
automatically. Release explicitly with `simpool release` once a session is
done, or just let the TTL lapse.

The TTL is deliberately short: it only has to cover the gap between
consecutive hot-loop calls, which is seconds, not the gap a long-running
`mav run` step (a build) can leave between calls, which can be minutes. A
short TTL is what lets an idle repo give its slot back within a few
minutes instead of camping on it for half an hour, which matters when
there are more repos than slots. Covering that longer gap is deliberately
not this TTL's job: a `mav run` reinvokes `target_command` periodically on
its own as a liveness signal for as long as it runs, independent of what
TTL the pool manager behind it happens to use — see MAV's README
(`target_command`) for that half of the story. A workload with a real
long-lived process of its own to attach a lock to (`bazel test`, or `mav
run` invoked directly rather than through `target_command`) should still
use `with`, not `lease` — see "Environment `with`/`acquire` export" and
the top of this section for that distinction.

**A lease is not a flock, and it does not pretend to be one.** `with` and
`acquire` are backed by a real kernel `flock()`: the instant the holding
process dies, for any reason including SIGKILL, the lock is gone — no
cleanup step required. A lease has no equivalent. It is a plain
timestamped reservation file (`lease.json`) that expires by wall-clock
time; if the agent session holding it vanishes, the slot simply stays
reserved (and unusable by anyone else) until the TTL runs out, however
that compares to how quickly the session actually finished. That is a
real, accepted weakness in exchange for fitting a workload that has no
long-lived process to attach a lock to at all — accept it consciously, and
call `simpool release` when a session ends if you want the slot back
sooner than the TTL.

The two mechanisms are mutually exclusive over the *same* slot: `with`/
`acquire` refuse a slot with a live lease even though the lease never
holds the flock, and `lease` refuses a slot whose flock is currently held,
even though holding a lease doesn't require one. Both paths funnel through
the same allocation lock that already arbitrates slot creation, so a
`with` and a `lease` racing for the same slot can never both win it.

### Environment `with`/`acquire` export

```
MAV_TARGET_KIND=simulator
MAV_TARGET_UDID=<udid of slot 0>
MAV_TARGET_NAME=<device>
MAV_TARGET_RUNTIME=<runtime id>
MAV_EXACT_RUN_DIR=<dir private to this invocation>
SIMPOOL_UDID_0..N-1
SIMPOOL_NAME_0..N-1
```

`SIMPOOL_NAME_N` is slot N's simulator *name* in the default device set
(`SIMPOOL_<roottag>_<device>@<os>_slot-N`) — for a consumer that creates or
reuses simulators by name rather than UDID, such as `rules_apple`'s
`ios_xctestrun_runner` (`simulator_creator.py --name`, reuse-by-name on by
default), this is what lets it be pointed at the pooled simulator instead of
creating a brand-new `New-<device>-<os>` one every run.

There is no `SIMPOOL_DEVICE_SET_N` — every pooled UDID lives in the
default device set, so it is already usable by a plain `xcrun simctl`
call, by MAV, by `axe`, by `idb`, with no extra flag and no code change on
their end:

```
simpool with --device "iPhone 17 Pro" --os 26.3 --count 2 -- \
    mav run flow.yaml --target "$SIMPOOL_UDID_0" --target "$SIMPOOL_UDID_1" --jobs 2
```

## Bazel: `simpool_ios_test_runner`

`ios_xctestrun_runner`'s stock simulator creation (`rules_apple`'s own
`simulator_creator.py`) already reuses one fixed-name `BAZEL_TEST_<type>_<os>`
simulator by default rather than creating a fresh one per action — but
that's the actual problem, not the fix: with more than one test action
running at once (the normal case with `--local_test_jobs` > 1, or several
repos/agents on the same machine), they all resolve and reuse that *same*
name, so concurrent test actions collide on one shared simulator. (A
different rule, Google's `xctestrunner`/`ios_test_runner`, does create a
fresh `New-<device>-<os>` simulator per action and delete it in a Python
`finally` that a `SIGKILL` — a Bazel test timeout, an interrupted CI job —
skips entirely, leaking 1-3GB per skipped run; that is a real bug, just not
this rule's.) Either way, nothing in the stock behavior gives concurrent
test actions their own simulator safely — which is what a `simpool` pool
actually provides.

`simpool_ios_test_runner` is a full replacement for `ios_xctestrun_runner`
that resolves its simulator from a `simpool` pool instead: no per-run
simulator, no leak, and — because it owns its own `test_runner_template`
rather than hooking `pre_action`/`post_action` on top of the stock
rule — it holds the pool slot's flock for the *entire* test action
(simulator resolution through cleanup), not just a short setup/teardown
step that finishes before the test itself even starts. That's what makes
two test targets running concurrently land on two different slots instead
of serializing behind one lock: each test action's `simpool with` call
races the others for a free slot exactly the way two independent shell
invocations would.

Zero configuration: no `.bazelrc` `--run_under` prefix, no `--test_env`.
Each test action resolves the `simpool` binary and the pool's real `$HOME`
for itself (a Bazel test action's environment is sanitized — no inherited
`PATH`, no `$HOME` — so nothing here can be assumed to arrive from the
caller), and falls back to stock `ios_xctestrun_runner`-equivalent
behavior (reuse-or-create a fixed-name simulator) when `simpool` isn't
installed at all, so this rule always *builds and runs* whether or not the
host machine has `simpool`. That fallback is not equally *safe*, though:
every concurrent test action missing `simpool` shares that one fixed-name
simulator — exactly the collision this rule exists to prevent — and the
runner warns on stderr when it takes that path.

`max_slots`/`wait` attributes on the rule forward to `simpool with
--max`/`--wait` for its device+OS group; both default to unset, which
defers to `simpool`'s own defaults (3 resident slots, 10 minute wait — see
"Capacity" above). Mind `--local_test_jobs` when raising `max_slots`: more
concurrently-scheduled test actions than resident slots means the excess
actions block inside `simpool with`'s wait instead of running, and a
blocked action that outlives its test's Bazel `size`/`timeout` (300s for
`size = "medium"`, well under `simpool`'s own 600s default wait) is
reported as a plain TIMEOUT with nothing pointing at pool contention as the
cause.

```bzl
# MODULE.bazel
bazel_dep(name = "simpool", version = "0.3.0")
git_override(
    module_name = "simpool",
    remote = "https://github.com/bitomule/simpool.git",
    commit = "<pinned commit>",
    strip_prefix = "bazel",
)
```

```bzl
# BUILD.bazel
load("@simpool//:simpool_ios_test_runner.bzl", "simpool_ios_test_runner")

simpool_ios_test_runner(
    name = "iphone_17_pro_test_runner",
    device_type = "iPhone 17 Pro",  # must match `simpool ... --device`
    os_version = "26.3",            # must match `simpool ... --os`
)

ios_unit_test(
    name = "MyTests",
    minimum_os_version = "17.0",
    runner = ":iphone_17_pro_test_runner",
    deps = [":MyTestsLib"],
)
```

Then just `bazel test //:MyTests` — no wrapper, no flags. If a `simpool`
pool for that device/OS group doesn't exist yet, the rule's fallback path
creates and reuses a plain `BAZEL_TEST_<device>_<os>` simulator, same as
`ios_xctestrun_runner` always has; install `simpool` (see Build, below) and
subsequent runs pick up the pool automatically.

## Pool layout

```
~/Library/Developer/SimPool/
  <device>_<os>/
    slot-0/
      lock            <- flock() exclusive. The only thing that arbitrates.
      meta.json        <- udid, created, last used. Informative, never authoritative.
      lease.json       <- present only while `simpool lease` reserves this
                           slot: {key, expiresAt}. Advisory + time-bounded,
                           not flock-backed — see "MAV in the hot loop".
      runs/            <- one dir per invocation (MAV_EXACT_RUN_DIR)
    slot-1/
    ...
```

Slot 0's simulator, for the `iPhone 17 Pro` / `26.3` group, is named
`SIMPOOL_<roottag>_iPhone-17-Pro@26.3_slot-0` in the default device set
(`xcrun simctl list devices`) — deterministic, so `EnsureProvisioned` can recover
it by name alone if `meta.json` is ever lost, and unique across the whole
pool (not just within a group), since every slot now shares one device set
instead of getting its own.

The lock file is the single source of truth. `meta.json` can be lost,
corrupted, or stale without the pool becoming incorrect. `lease.json` sits
one level further down the trust chain still: an *absent* lease never
blocks anything, but a *live* one is honored by `with`/`acquire`/`reap`
exactly like a real reservation, right up until it expires. Unlike
`meta.json`, though, a lease is the sole authority for whether a slot is
available in `lease`'s flock-free reservation path, so it does not get
`meta.json`'s tolerant treatment of a read failure: an *unreadable*
`lease.json` (a permission error, a truncated file, `EMFILE` under load —
as opposed to one that's simply absent) is never read as "no lease" by
anything that hands a slot out. It's treated as busy until it can actually
be read, so a transient I/O error can never let a second consumer steal an
active reservation.

Override the pool root with `SIMPOOL_HOME` (used by the test suite to
avoid touching the real pool).

## Architecture: why simpool is the parent, not exec'd away

This was resolved by experiment, not by reasoning about it (see design doc
§4). Two options were on the table:

- **(a)** `simpool` execs the consumer, which inherits the lock fd. Simple,
  but a two-binary Go experiment showed the *grandchild* (e.g. a `log
  stream` MAV spawns and never reaps) inherits the fd too and holds the
  flock forever. Not fixable — `CLOEXEC` is a property of the descriptor,
  not of one particular `exec`.
- **(b)** `simpool` stays the parent, holds the flock, launches the
  consumer as a child with `CLOEXEC` intact. Neither the child nor its
  descendants ever see the fd.

**(b) won.** Its own failure mode is a `SIGKILL` to `simpool` itself
specifically (not its process group): the kernel releases the flock
immediately, but the child — and anything it spawned — survives as an
orphan, because the process-group sweep that normally reclaims them
(`simpool` catches SIGTERM/SIGINT and kills the child's whole process
group on every exit path it's alive to run) never gets to execute; a dead
process can't run its own cleanup.

This is **not** silently unsafe, and not permanently stuck either, as long
as the orphan's identity can be verified. `simpool with` fingerprints its
child right after launching it — `ps -o lstart=` for the child's own pid
(which, thanks to `Setpgid`, is also its process-group id), rendered under
a fixed, locale/timezone-independent environment (`TZ=UTC0 LC_ALL=C
LANG=C`) rather than whatever the ambient environment happens to be — and
records it alongside its pgid in meta.json.

Recovery happens in two places:

1. **On-demand, at the next acquisition.** `with`/`acquire`/`lease` all
   check a free-looking slot's previous consumer before handing it out; if
   it's still alive (`pool.CheckPoison`), they verify the recorded
   fingerprint against reality (`pool.VerifyConsumerIdentity`) and — only
   on an exact match — kill the process group, shut down the simulator, and
   take the slot for themselves (`pool.AttemptRecovery`). If identity can't
   be verified (no fingerprint recorded, a mismatch, or the kill doesn't
   stick — e.g. the process is stuck in an uninterruptible sleep inside
   CoreSimulator), the slot is quarantined exactly as before this existed,
   and left for a human or a later `simpool reap` to sort out.
2. **`simpool reap`**, run by a human or CI, does the same verified
   recovery for every slot it walks, on top of its other jobs (stuck-`with`
   cleanup, lease expiry, `--cold`/`--purge`).

**Deliberately out of scope: no automatic exit-time sweep.** An earlier
version of this feature also wired a group-wide sweep into `with`'s and
`release`'s exit paths, to reclaim other orphans and shut down long-idle
simulators as a side effect of ordinary usage. It was cut before shipping:
checking whether a slot is safe to shut down cheaply (`kill(-pgid, 0)`,
microseconds) is not the same question as checking whether it's safe by
the fuller `LiveConsumers` scan (a live process referencing the UDID with
no recorded pgid — the healthy state for an actively-leased slot, not an
orphan), and that fuller scan is expensive enough (~8.5s measured against
one booted iOS 26.3 simulator on this machine, all fork/exec overhead —
see `internal/procs.Descendants`) that fanning it out across a whole group
on every single `with`/Bazel test action's exit is the wrong trade: real
cost on the hot path for a benefit (idle-simulator shutdown) `simpool reap
--cold N` already provides on its own schedule. Shutting down an
otherwise-idle simulator stays `reap`'s job — schedule it (cron/launchd) if
you want that automatic; nothing about acquisition-time recovery depends
on it.

A second, separate attempt at automatic idle cleanup was tried and cut
too: acquiring a slot briefly shut down *other*, idle sibling slots in the
same group as a side effect, on the theory that an expired lease reliably
proves nobody's using that sibling. That theory was false exactly where it
mattered — the TTL keepalive it leaned on only runs on MAV's `run` path,
never on the one-shot `tap`/`swipe`/`ui-tree` commands the sweep actually
had to reason about (see "MAV in the hot loop" above) — so an expired
lease only proved no command had run in the last few minutes, which is
routine in an agent's tool loop, not evidence of absence. With several
repos sharing one group, an ordinary Bazel test action could shut down a
live session's simulator this way, costing the next command a ~110s cold
boot and its app's installed state. Removed for the same reason as the
exit-time sweep above: `simpool reap --cold N`, run explicitly, is the
only path that ever shuts down an idle simulator.

**One further, deliberate scope limit** on recovery itself:
`VerifyConsumerIdentity` only ever re-identifies the process-group
*leader* — the exact pid `simpool with` launched and fingerprinted. The
dominant real orphan case is that leader still running (`simpool` died
while its direct child was actively working, e.g. mid-`mav run`), which
this handles. If the leader itself has already exited but a descendant it
spawned survives under the same pgid — `PGIDAlive` still reports the group
as poisoned, but there is no live leader left to re-identify — recovery
refuses rather than trusting bare pgid membership alone (the same class of
evidence `PGIDAlive` already provides, which this mechanism exists
specifically not to trust for a kill decision). That slot stays
quarantined for a human or `simpool reap` to inspect by hand; see
`pool.VerifyConsumerIdentity`'s doc comment for the reasoning.

A live process referencing a slot's UDID on its own command line
(`procs.LiveConsumers`, `pgrep -f <udid>`-based) is a second, purely
diagnostic signal, never a kill candidate under any mode — for a `lease`d
slot in particular, that is the *healthy* state (a legitimate
`axe`/`simctl`/MAV session against the leased device, not an orphan). Only
`ConsumerPGID` (a process group `simpool with` itself created via
`Setpgid`) is ever a kill candidate, and only when `meta.Mode == "with"`.
A check that itself fails to complete (e.g. `pgrep` failing to fork under
load) is treated the same as "still alive" — never as "confirmed free" —
and is likewise never a kill candidate.

**Every shutdown or delete validates device identity from the slot's own
directory, never from `meta.json`.** `pool.deviceBelongsToSlot` computes
the *expected* device name from `(root, groupName, n)` — the slot's actual
directory location, the one thing that can't be corrupted independently of
where the file sits on disk — and only ever shuts down or deletes a UDID
once the real device's name in the default set matches that computed name
exactly. `meta.json`'s own `Device`/`OSVersion` fields are deliberately
never consulted for this: they live in the same file whose `UDID` is what's
being validated, so comparing meta against meta detects only an
*incoherent* corruption (fields that don't even agree with each other),
never a *coherent* one — a `meta.json` that consistently, believably
claims to be some other slot's, or another user's own, live simulator.
Deriving the expected name from the directory instead means a slot's
identity is never asked to be its own witness.

**`simpool reap --disown-poisoned`** is the deliberate, explicit-only way
out of a poisoned slot that can *never* self-resolve: `meta.ConsumerPGID`
tests "alive" forever because either its pid has been recycled by an
unrelated process that happens to share the same number, or the process
group belongs to a different user outright (`kill(-pgid, 0)` returns
`EPERM`). `VerifyConsumerIdentity`'s start-time fingerprint can't clear
either case — a bare pgid match was never trusted for exactly this reason
— so without an operator's override, `DefaultMaxSlotsPerGroup = 3` means
losing a slot like this is losing a third of the pool, permanently.

A `--force`-style flag that just sends `SIGKILL` despite the failed
identity check was considered and rejected — deliberately, not for lack of
trying:

- It cannot even work for one of the two motivating cases. `EPERM` means
  the kernel itself refuses to let simpool signal that process group; a
  kill-based override would fail on precisely the scenario that most needs
  one.
- For the recycled-pid case, it would reintroduce the exact failure mode
  `VerifyConsumerIdentity` exists to rule out — a bare numeric pgid match
  is never sufficient evidence of identity — just with a human's finger on
  the trigger instead of `AttemptRecovery`'s automatic one.

`--disown-poisoned` never signals anything. It only **forgets**: it clears
this slot's recorded fingerprint and, once `deviceBelongsToSlot` confirms
the device really is this slot's own, shuts it down and deletes it outright
so the next acquisition provisions a genuinely fresh simulator rather than
risking two consumers sharing one whatever-it-is is still poking at. If
that something is in fact still running, it keeps running — it's simply no
longer simpool's problem, and no longer standing between the slot and
reuse. Scoped exactly like `AttemptRecovery`'s own kill gate (`meta.Mode ==
"with"` and `poison.Reason == PoisonedByConsumerPGID` only): a live
lease/`acquire` consumer (`PoisonedByLiveConsumers`) is the healthy case and
must never have its device pulled out from under it, and a liveness check
that merely failed to run (`PoisonedByCheckFailure`) proves nothing is
actually wrong, so disowning on the strength of it would be reckless, not
an escape hatch. Respects `--dry-run` (reports what it would forget without
touching anything).

## Testing

Two layers:

```
go test ./...
```

Fast, no simulators. This is where the load-bearing correctness proofs
live — each spawns **real, separate OS processes** (the standard Go
re-exec trick, `internal/pool/lock_test.go` and `slot_test.go`) because a
single-process test cannot demonstrate flock exclusivity or "the kernel
releases the lock on SIGKILL with no cleanup step":

- `TestFlockTwoRealProcesses` — two processes race for one lock file; the
  loser gets `ErrBusy`; SIGKILLing the winner frees the lock immediately
  for a third process, with zero code of ours running to make that happen.
- `TestAcquireSlotsTwoRealProcesses` — two processes racing the actual
  slot-allocation logic land on two different slot numbers, both held
  simultaneously; killing one frees only that one.
- `TestAcquireSlots_RefusesCapacityAboveMax` / `_SkipsSlotWithLivePGIDEvenWithoutUDIDInArgv`
  — `--max` is a real cap, not a suggestion, and a slot poisoned by a live
  orphan with an unverifiable identity (no fingerprint recorded) is skipped
  rather than handed to a second consumer.
- `TestAcquireSlots_RecoversVerifiedOrphanInstead` /
  `_RecoversPoisonedSlotExactlyOnce` — the positive case: a poisoned slot
  whose recorded fingerprint matches its still-alive process-group leader
  is reclaimed instead of quarantined, and two processes racing a single
  such slot resolve to exactly one winner (never both, never neither).
- `internal/pool/poison_test.go` covers `AttemptRecovery`/
  `VerifyConsumerIdentity` directly against real processes: a verified
  orphan is killed and the slot reclaimed; a deliberately wrong recorded
  start time (standing in for a recycled pid) is never killed; a missing
  fingerprint is never killed; a live process referencing a UDID on its
  own command line is never a kill candidate in any mode; a failed
  liveness check is never a kill candidate either; and — the deliberate
  scope limit — a process-group leader that has already exited while a
  descendant of it survives under the same pgid is left quarantined
  rather than trusted on bare pgid membership alone.
- `internal/procs/procs_test.go` proves `ProcessStartTime` produces the
  *same* string for the same instant regardless of the calling process's
  ambient `TZ`/`LC_ALL`/`LANG` (`TestProcessStartTime_StableAcrossAmbientLocaleAndTZ`)
  — the regression test for the critical finding an earlier, reverted
  version of this feature got wrong: fingerprinting via a rendering that
  is sensitive to the invoking process's environment, so the exact same
  instant could produce two different strings depending on what happened
  to be set elsewhere. It also proves `PGIDAlive` treats any signal-check
  failure other than "genuinely gone" (`ESRCH`) as still-alive — most
  notably `EPERM` — never as "confirmed free", and that `MatchingPIDs`/
  `LiveConsumers` propagate a `pgrep` execution failure as an error rather
  than silently reading it as "no live consumer". Also, real child
  processes spawned to prove `LiveConsumers` tells the simulator's own
  runtime (`launchd_sim` and everything under it) apart from a genuine
  external orphan, and that `IsSimpoolHolder` only trusts a lock "holder"
  `lsof` reports if its command line actually looks like
  `simpool <subcommand>`.
- `internal/cli`'s `reap_test.go` covers the dead-slot-directory path
  (`--purge`'s deletion of an abandoned, never-provisioned slot) purely
  against the filesystem — no simulators needed since that path never
  touches `simctl` — plus `TestReapSlot_RecoversVerifiedOrphanEvenWithoutUDIDInArgv`
  (reap now reclaims, not just detects), `_SkipsOrphanWithUnverifiableIdentity`,
  and `TestReapSlot_RecoveryNeverFallsThroughToSamePassPurge` — the
  regression test for a real bug this feature had before it shipped: a
  successful recovery must return immediately, not fall through to the
  same pass's idle/cold/`--purge` accounting. `AttemptRecovery`'s
  `simctl.Shutdown` call is measured **synchronous** — it blocks until the
  device's own reported state is genuinely "Shutdown" (5-7.5s wall time
  observed), not an async call that returns before the device is really
  down — but CoreSimulator's own teardown of the device's underlying
  process tree can still be settling for a beat after that state flip;
  falling through risked `--purge` deleting a simulator before its process
  tree finished tearing down, which orphans hundreds of runtime processes
  (see `cleanupPool` below).
- `internal/pool/lease_test.go` covers `lease`'s own contract — sticky by
  key, two keys never share a slot, an expired lease is reusable, `--max`
  is a real cap and fails immediately (no polling) — and, symmetrically,
  that `with`/`acquire` refuse a leased slot and `lease` refuses a
  flock-held one. `TestAcquireLease_ConcurrentDifferentKeysNeverCollide`
  races several goroutines' `AcquireLease` calls with distinct keys
  against one group and asserts no two land on the same slot; real
  concurrency, not a simulation, since BSD `flock()` (what the shared
  allocation lock is built on) is scoped to the open file description, so
  goroutines racing in one process exercise the same kernel exclusion
  separate `simpool` processes would. `TestAcquireLease_RecoversVerifiedOrphanLeftByWith`
  proves recovery is cross-mode: whichever command next touches a slot a
  `with` session left poisoned can reclaim it, not just `with`/`acquire`'s
  own path. `internal/cli/reap_test.go` and `doctor_test.go` cover the
  CLI-facing half: reap skips an actively leased slot and cleans up an
  expired one, and doctor flags the one invariant that must never hold —
  a slot with both a busy flock and a live lease at once.
- `ReadLease`'s own tests (`internal/pool/lease_test.go`) prove the
  three-way read it returns: no `lease.json` at all is genuinely free
  (`nil` error), a valid one parses normally, and one that exists but
  can't be read or parsed (simulated with a directory sitting where the
  file should be, and with truncated JSON) is reported as an *error* —
  never silently as "no lease". `TestAcquireSlots_SkipsSlotWithUnreadableLease`
  / `TestAcquireLease_SkipsSlotWithUnreadableLease` prove that error
  propagates all the way to both handout paths (a slot with an unreadable
  lease is skipped, not treated as free), and `TestReapSlot_UnreadableLeaseIsTreatedAsBusy`
  / `TestRunDoctor_FlagsUnreadableLease` cover reap and doctor doing the
  same. `TestReleaseLease_UnreadableSlotDoesNotBlockOthers` proves a read
  failure on one unrelated slot never stops a different, readable lease
  from being released.
- `internal/pool/poison_test.go`'s `DisownPoisonedSlot` tests cover
  `--disown-poisoned`: a with-mode slot with an unverifiable fingerprint is
  forgotten (meta cleared) *without* ever signaling the process behind it
  (`TestDisownPoisonedSlot_ForgetsFingerprintWithoutKilling`), and it's
  refused outright — `ErrNotDisownable`, meta untouched — for a live
  lease/`acquire` consumer, a failed liveness check, or any non-`with`
  mode, mirroring `AttemptRecovery`'s own gates exactly.
  `internal/cli/reap_test.go`'s `TestReapSlot_DisownPoisoned*` tests cover
  the CLI end to end: an unverifiable orphan is reclaimed without being
  killed, `--dry-run` never mutates anything, and a live lease consumer is
  never touched even when `--disown-poisoned` is explicitly requested.

```
SIMPOOL_RUN_INTEGRATION=1 go test ./internal/cli/... -run Integration -v
```

Slow (~15-20s), creates and boots one real simulator at a time under an
isolated `SIMPOOL_HOME` (never touches `~/Library/Developer/SimPool`), and
tears every simulator and stray process down in `t.Cleanup` regardless of
pass/fail. **Run at most one at a time** — booting a simulator is real
RAM/CPU/disk cost on the host, and deleting one whose process tree hasn't
finished tearing down after `shutdown` can orphan dozens of runtime
processes (`AccessibilityUIServer`, `healthd`, ...) with nothing left to
reap them; `cleanupPool` polls for an actual "Shutdown" state before ever
calling `delete`, specifically because this was reproduced on the machine
these tests were last validated on.

- `TestIntegration_WithHappyPathAndOrphanSweep` — the full `with` pipeline
  against a real booted device (env contract, run dir, device state, and
  that the booted device's name satisfies `pool.IsPoolName` — the whole
  point of the default-device-set migration is that none of this needs a
  `--set`), plus proof that a grandchild the command backgrounds and
  forgets about does not survive `with` exiting.
- `TestIntegration_ReapRecoversVerifiedOrphanAfterSimpoolIsKilled` —
  reproduces the one accepted failure window from §4 (SIGKILL to `simpool`
  itself, not its group) and proves `reap` verifies the still-live
  consumer's identity against its recorded fingerprint and recovers it:
  kills the process, shuts down the simulator.

## What's out of scope here

This repo implements the CLI (`with`, `acquire`, `status`, `reap`,
`doctor`) and the Bazel integration (`simpool_ios_test_runner`, under
`bazel/`, consumed via `git_override`). Not included, per the design doc's
own scoping (§9's B1/M1-3/R1-2, separate workstreams):

- The MAV-side fixes (§7c) — global run pointer, atomic/adelanted lock,
  per-run comparison, signal handling, `SaveConfig` atomicity. None of
  these couple to simpool; they make MAV correct under concurrency on
  their own.
- App-repo changes (§7d) — removing hardcoded `simulator_udid`, dead
  scripts, `ios_test_runner` → `simpool_ios_test_runner` migration,
  `/tmp` path fixes, `+no-cache` bundling — done per-repo as each one
  adopts the Bazel integration above.
