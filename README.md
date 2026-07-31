# simpool

A broker for iOS simulators shared by several agents on one machine. Every
slot gets its own simulator in the **default** device set — the same one
Xcode, the user's own simulators, MAV, axe, and idb already use with no
special flag — identified by a unique, deterministic name
(`SIMPOOL_<device>_<os>_slot-<n>`) rather than by a private device set.
Arbitration is a real `flock()` held by a live process; simpool guarantees
slots come back — no central daemon, no PID-file heuristics.

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
go build -o simpool .
```

Go 1.25+, stdlib only, single binary, macOS only (uses BSD `flock`,
`xcrun simctl`, `pgrep`, `lsof`, `ps`).

## Usage

```
simpool with [--device D] [--os V] [--count N] [--max M] [--wait D] -- <cmd>
    Acquire N slots, export the environment, run <cmd>, release on exit.
    This is the normal way to use simpool — wrap your command in it.

simpool acquire [--device D] [--os V] [--count N] [--max M] [--wait D]
    Print the environment for N slots as shell `export` lines and hold
    the lock until signaled (SIGINT/SIGTERM/SIGHUP). For scripts that
    want the UDIDs up front and will manage the workload themselves.

simpool status
    List every slot: lock state, holder (best-effort), device boot state.

simpool reap [--cold N] [--stuck-after D] [--purge N] [--prune-runs-after D] [--dry-run]
    Recycle free+cold slots. Bidirectional: never shuts down a simulator
    that still has a live process attached even if its lock is free, and
    kills a stuck `with` holder that has no live work left under it
    (never an `acquire` holder — see "Capacity" below). Also prunes old
    run directories and, with --purge, deletes long-cold simulators
    outright *and their slot directory* to reclaim disk — the only
    subcommand that actually deletes anything, by design. Never touches a
    device whose name doesn't start with `SIMPOOL_`, no matter what
    meta.json says: the default device set also holds the user's own
    simulators.

simpool doctor
    Read-only coherence check. Exits non-zero if anything looks wrong.
```

### Capacity

Each device+OS group is capped at `--max` resident slots (default 3,
override with `SIMPOOL_MAX_SLOTS`) — booting one costs ~1.75GB (design doc
§3), so an uncapped pool turns ordinary contention into the kind of jetsam
this tool exists to prevent. Once a group is at capacity, `with`/`acquire`
poll for a free slot for up to `--wait` (default 10m; 0 fails immediately)
before giving up.

### Environment `with`/`acquire` export

```
MAV_TARGET_KIND=simulator
MAV_TARGET_UDID=<udid of slot 0>
MAV_TARGET_NAME=<device>
MAV_TARGET_RUNTIME=<runtime id>
MAV_EXACT_RUN_DIR=<dir private to this invocation>
SIMPOOL_UDID_0..N-1
```

There is no `SIMPOOL_DEVICE_SET_N` — every pooled UDID lives in the
default device set, so it is already usable by a plain `xcrun simctl`
call, by MAV, by `axe`, by `idb`, with no extra flag and no code change on
their end:

```
simpool with --device "iPhone 17 Pro" --os 26.3 --count 2 -- \
    mav run flow.yaml --target "$SIMPOOL_UDID_0" --target "$SIMPOOL_UDID_1" --jobs 2
```

## Pool layout

```
~/Library/Developer/SimPool/
  <device>_<os>/
    slot-0/
      lock            <- flock() exclusive. The only thing that arbitrates.
      meta.json        <- udid, created, last used. Informative, never authoritative.
      runs/            <- one dir per invocation (MAV_EXACT_RUN_DIR)
    slot-1/
    ...
```

Slot 0's simulator, for the `iPhone 17 Pro` / `26.3` group, is named
`SIMPOOL_iPhone-17-Pro_26.3_slot-0` in the default device set (`xcrun
simctl list devices`) — deterministic, so `EnsureProvisioned` can recover
it by name alone if `meta.json` is ever lost, and unique across the whole
pool (not just within a group), since every slot now shares one device set
instead of getting its own.

The lock file is the single source of truth. `meta.json` can be lost,
corrupted, or stale without the pool becoming incorrect.

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

**(b) won.** Its own failure mode — `SIGKILL` to `simpool` specifically
releases the lock while the consumer survives as an orphan — is covered by
two things: `simpool` kills the child's whole process group on every exit
path (catches SIGTERM/SIGINT and sweeps up orphaned grandchildren for
free), and `reap` refuses to recycle a free-looking slot that still has a
live process attached to it.

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
`doctor`) only. Not included, per the design doc's own scoping (§9's A2 vs
B1/M1-3/R1-2, all separate workstreams):

- The Bazel `simpool_ios_test_runner` rule (§7b) — a thin wrapper the
  design says should live under `bazel/` in this same repo later, consumed
  via `git_override`.
- The MAV-side fixes (§7c) — global run pointer, atomic/adelanted lock,
  per-run comparison, signal handling, `SaveConfig` atomicity. None of
  these couple to simpool; they make MAV correct under concurrency on
  their own.
- App-repo changes (§7d) — removing hardcoded `simulator_udid`, dead
  scripts, `ios_test_runner` → `simpool_ios_test_runner` migration,
  `/tmp` path fixes, `+no-cache` bundling.
