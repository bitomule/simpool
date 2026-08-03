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
an orphan — and that is not silently unsafe: `reap` refuses to hand a
lock-free slot to a new consumer while its old one is still alive, so the
slot is quarantined, not corrupted; recovering it takes an explicit
`simpool reap` (see "Architecture" below).

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

simpool reap [--cold N] [--stuck-after D] [--purge N] [--prune-runs-after D] [--dry-run]
    Recycle free+cold slots. Bidirectional: never shuts down a simulator
    that still has a live process attached even if its lock is free, and
    kills a stuck `with` holder that has no live work left under it
    (never an `acquire` holder — see "Capacity" below). Also prunes old
    run directories, clears expired lease files, and, with --purge,
    deletes long-cold simulators outright *and their slot directory* to
    reclaim disk — the only subcommand that actually deletes anything, by
    design. Never touches a device whose name doesn't start with
    `SIMPOOL_`, no matter what meta.json says: the default device set
    also holds the user's own simulators.

simpool doctor
    Read-only coherence check. Exits non-zero if anything looks wrong.
```

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
exactly like a real reservation, right up until it expires.

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
process can't run its own cleanup. This is **not** silently unsafe: `reap`
refuses to recycle a free-looking slot that still has a live process
attached, so the slot is quarantined rather than handed to a second
consumer — but quarantined is not the same as recovered. Nothing on the
`with`/Bazel path calls `reap` automatically, so an orphaned slot stays
stuck until something (a human, a cron job, CI) runs `simpool reap`
itself.

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
- `TestAcquireSlots_RefusesCapacityAboveMax` / `_SkipsSlotWithLiveOrphanConsumer`
  — `--max` is a real cap, not a suggestion, and a slot poisoned by a live
  orphan (flock free, consumer still alive) is skipped rather than handed
  to a second consumer.
- `internal/procs`'s tests spawn real child processes too, to prove
  `LiveConsumers` tells the simulator's own runtime (`launchd_sim` and
  everything under it) apart from a genuine external orphan, and that
  `IsSimpoolHolder` only trusts a lock "holder" `lsof` reports if its
  command line actually looks like `simpool <subcommand>`.
- `internal/cli`'s `reap_test.go` covers the dead-slot-directory path
  (`--purge`'s deletion of an abandoned, never-provisioned slot) purely
  against the filesystem — no simulators needed since that path never
  touches `simctl`.
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
  separate `simpool` processes would. `internal/cli/reap_test.go` and
  `doctor_test.go` cover the CLI-facing half: reap skips an actively
  leased slot and cleans up an expired one, and doctor flags the one
  invariant that must never hold — a slot with both a busy flock and a
  live lease at once.

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
- `TestIntegration_ReapProtectsLiveOrphanAfterSimpoolIsKilled` — reproduces
  the one accepted failure window from §4 (SIGKILL to `simpool` itself,
  not its group) and proves `reap` detects the still-live consumer and
  refuses to shut down its simulator.

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
