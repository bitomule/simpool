# simpool

A broker for iOS simulators shared by several agents on one machine. It
hands out isolated `xcrun simctl` device sets, arbitrates them with a real
`flock()` held by a live process, and guarantees they come back — no
central daemon, no PID-file heuristics.

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
simpool with [--device D] [--os V] [--count N] -- <cmd>
    Acquire N slots, export the environment, run <cmd>, release on exit.
    This is the normal way to use simpool — wrap your command in it.

simpool acquire [--device D] [--os V] [--count N]
    Print the environment for N slots as shell `export` lines and hold
    the lock until signaled (SIGINT/SIGTERM/SIGHUP). For scripts that
    want the UDIDs up front and will manage the workload themselves.

simpool status
    List every slot: lock state, holder (best-effort), device boot state.

simpool reap [--cold N] [--dry-run]
    Recycle free+cold slots. Bidirectional: never shuts down a simulator
    that still has a live process attached even if its lock is free, and
    kills stuck lock-holders that have no live work left under them.

simpool doctor
    Read-only coherence check. Exits non-zero if anything looks wrong.
```

### Environment `with`/`acquire` export

```
MAV_TARGET_KIND=simulator
MAV_TARGET_UDID=<udid of slot 0>
MAV_TARGET_NAME=<device>
MAV_TARGET_RUNTIME=<runtime id>
MAV_EXACT_RUN_DIR=<dir private to this invocation>
SIMPOOL_UDID_0..N-1
SIMPOOL_DEVICE_SET_0..N-1
```

### Example

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
      set/             <- private device set: xcrun simctl --set <here>
      meta.json        <- udid, created, last used. Informative, never authoritative.
    slot-1/
    ...
```

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

```
SIMPOOL_RUN_INTEGRATION=1 go test ./internal/cli/... -run Integration -v
```

Slow (~20s), creates and boots real simulators under an isolated
`SIMPOOL_HOME` (never touches `~/Library/Developer/SimPool`), and tears
every simulator and stray process down in `t.Cleanup` regardless of
pass/fail:

- `TestIntegration_WithHappyPathAndOrphanSweep` — the full `with` pipeline
  against a real booted device (env contract, run dir, device state), plus
  proof that a grandchild the command backgrounds and forgets about does
  not survive `with` exiting.
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
