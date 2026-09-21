package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAcquireLease_DistinctKeysNeverShareASlot is the in-process half of
// the reproduction of the report that two concurrent agents, each asking
// with --max 1, were handed the same slot. Different keys are different
// consumers: with max=1 exactly one of them may win, and every other must
// be refused at capacity. Two winners on one slot number is the defect.
func TestAcquireLease_DistinctKeysNeverShareASlot(t *testing.T) {
	const (
		max       = 1
		claimants = 16
	)
	root := t.TempDir()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := map[int][]string{}

	for i := 0; i < claimants; i++ {
		key := fmt.Sprintf("/agent/%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Minute, max)
			if err != nil {
				if !errors.Is(err, ErrAtCapacity) {
					t.Errorf("AcquireLease(%s): unexpected error %v", key, err)
				}
				return
			}
			mu.Lock()
			won[slot.Number] = append(won[slot.Number], key)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	assertOneWinnerPerSlot(t, won, max)
}

// TestLeaseRaceHelperProcess is not a real test: it is re-exec'd as a
// subprocess by the across-processes reproduction below, the same trick
// TestHelperProcess uses in lock_test.go. Guarded by an env var so a
// normal `go test` run of it is a no-op.
//
// Separate PROCESSES, not goroutines, are what the report is about, and
// the two are not interchangeable here: flock() exclusion is scoped to
// the open file description, so a goroutine test exercises a different
// (and weaker) arrangement of the same primitive than two `simpool lease`
// invocations do. It stays entirely on the filesystem — no simctl, no
// simulator boots — because AcquireLease never touches one: provisioning
// happens after it returns.
func TestLeaseRaceHelperProcess(t *testing.T) {
	if os.Getenv("SIMPOOL_HELPER_LEASE_RACE") != "1" {
		return
	}
	root := os.Getenv("SIMPOOL_HELPER_LEASE_ROOT")
	key := os.Getenv("SIMPOOL_HELPER_LEASE_KEY")
	max, _ := strconv.Atoi(os.Getenv("SIMPOOL_HELPER_LEASE_MAX"))
	gate := os.Getenv("SIMPOOL_HELPER_LEASE_GATE")

	// Spin until the parent drops the gate file, so every claimant is
	// already running when the first one starts claiming. Without this the
	// children are serialised by however long each `exec` happens to take,
	// and the window the report is about never opens.
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	res := leaseRaceResult{Slot: -1}
	slot, err := AcquireLease(root, "TestDevice", "1.0", key, time.Minute, max)
	switch {
	case err == nil:
		res.Slot = slot.Number
	case errors.Is(err, ErrAtCapacity):
		res.Err = "at-capacity"
	default:
		res.Err = err.Error()
	}
	enc, _ := json.Marshal(res)
	os.Stdout.WriteString(string(enc) + "\n")
	os.Stdout.Sync()
	os.Exit(0)
}

type leaseRaceResult struct {
	Slot int    `json:"slot"`
	Err  string `json:"err"`
}

// TestAcquireLease_DistinctKeysNeverShareASlot_AcrossProcesses is the
// reproduction proper: N real processes asking for a lease at the same
// instant with --max 1 and a different key each.
func TestAcquireLease_DistinctKeysNeverShareASlot_AcrossProcesses(t *testing.T) {
	const (
		max       = 1
		claimants = 12
	)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "pool")
	gate := filepath.Join(tmp, "go")

	cmds := make([]*exec.Cmd, claimants)
	outs := make([]*strings.Builder, claimants)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=TestLeaseRaceHelperProcess")
		cmd.Env = append(os.Environ(),
			"SIMPOOL_HELPER_LEASE_RACE=1",
			"SIMPOOL_HELPER_LEASE_ROOT="+root,
			"SIMPOOL_HELPER_LEASE_KEY="+fmt.Sprintf("/agent/%d", i),
			"SIMPOOL_HELPER_LEASE_MAX="+strconv.Itoa(max),
			"SIMPOOL_HELPER_LEASE_GATE="+gate,
		)
		out := &strings.Builder{}
		cmd.Stdout = out
		cmds[i], outs[i] = cmd, out
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting claimant %d: %v", i, err)
		}
	}
	// Everyone is up and spinning; drop the gate.
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatalf("opening the gate: %v", err)
	}

	won := map[int][]string{}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("claimant %d: %v (output %q)", i, err, outs[i].String())
		}
		res, err := parseLeaseRaceResult(outs[i].String())
		if err != nil {
			t.Fatalf("claimant %d: %v", i, err)
		}
		if res.Slot < 0 {
			if res.Err != "at-capacity" {
				t.Errorf("claimant %d refused for something other than capacity: %s", i, res.Err)
			}
			continue
		}
		won[res.Slot] = append(won[res.Slot], fmt.Sprintf("/agent/%d", i))
	}

	assertOneWinnerPerSlot(t, won, max)
}

// parseLeaseRaceResult picks the verdict line out of the helper's stdout,
// which also carries `go test`'s own PASS/ok chatter.
func parseLeaseRaceResult(out string) (leaseRaceResult, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var res leaseRaceResult
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			return res, fmt.Errorf("unreadable verdict %q: %w", line, err)
		}
		return res, nil
	}
	return leaseRaceResult{}, fmt.Errorf("no verdict in helper output %q", out)
}

// TestAcquireLease_SameKeyDeliberatelySharesOneSlot pins the behaviour
// that explains the report this file was written for, and that is NOT a
// defect: stickiness is per KEY, not per process, and the default key is
// the caller's git repo root (see cli.defaultLeaseKey). Two concurrent
// callers run from the same checkout therefore ask under the same key and
// are handed the same slot — and so the same UDID — whatever --max says,
// because the sticky renewal path returns the key's own slot before any
// capacity accounting happens.
//
// Two agents driving one simulator is exactly the contaminated-reading
// shape that was reported as a lease exclusion failure. The exclusion
// that failed there is the one between two AGENTS; simpool only ever
// promised exclusion between two KEYS.
func TestAcquireLease_SameKeyDeliberatelySharesOneSlot(t *testing.T) {
	root := t.TempDir()
	const key = "/Users/someone/Projects/App"

	first, err := AcquireLease(root, "TestDevice", "1.0", key, time.Minute, 1)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := AcquireLease(root, "TestDevice", "1.0", key, time.Minute, 1)
	if err != nil {
		t.Fatalf("second claim under the same key: %v", err)
	}
	if first.Number != second.Number {
		t.Fatalf("stickiness broken: same key got slot-%d then slot-%d", first.Number, second.Number)
	}
	if !second.Renewed {
		t.Errorf("the second claim under one key should be a renewal, not a fresh hand-out")
	}
}

func assertOneWinnerPerSlot(t *testing.T, won map[int][]string, max int) {
	t.Helper()
	for n, keys := range won {
		if len(keys) > 1 {
			t.Errorf("slot-%d handed to %d different keys at once: %v", n, len(keys), keys)
		}
	}
	if len(won) > max {
		t.Errorf("%d slots handed out with --max %d: %v", len(won), max, won)
	}
	// The control on the other side: exclusion that refuses everybody is
	// not exclusion, it is an outage.
	if len(won) == 0 {
		t.Errorf("nobody got a slot: a legitimate lease must still be granted")
	}
}
