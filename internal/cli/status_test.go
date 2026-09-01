package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/procs"
)

// leaseResidueSlot builds, under home, the slot from the report that led to
// this test: last held in "lease" mode by key, lease since expired, and a
// real live process carrying its (synthetic) UDID in its own argv — the
// shape a MAV hot loop leaves behind (`simctl spawn <udid> log stream`,
// `axe describe-ui --udid <udid>`, the app itself). `simpool status` used
// to print this slot "free" while every acquisition path refused it.
func leaseResidueSlot(t *testing.T, home, key, udid string) string {
	t.Helper()
	groupDir := pool.GroupDir(home, "TestDevice", "1.0")
	dir := pool.SlotDir(groupDir, 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:     udid,
		Mode:     "lease",
		LeaseKey: key,
		LastUsed: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WriteLease(dir, pool.Lease{Key: key, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(t.TempDir(), "lease_residue.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(scriptPath, udid)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// Two consecutive sightings — see the same wait in
	// pool/availability_test.go for why one is not enough.
	deadline := time.Now().Add(5 * time.Second)
	sightings := 0
	for {
		if live, err := procs.LiveConsumers(udid); err == nil && len(live) > 0 {
			sightings++
			if sightings >= 2 {
				return dir
			}
		} else {
			sightings = 0
		}
		if time.Now().After(deadline) {
			t.Fatal("the residue process never became reliably visible to LiveConsumers")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunStatus_ReportsQuarantineInsteadOfFree is the user-facing half of
// the reported defect: `status` printed "free" for a slot `simpool lease`
// refused, and the refusal told the reader to run `status` to find out who
// held it — so the two views between them said nothing was wrong and the
// slot was unusable.
func TestRunStatus_ReportsQuarantineInsteadOfFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	leaseResidueSlot(t, home, "boxy-screenshots-ipad", "simpool-test-udid-status-quarantined")

	var stdout, stderr bytes.Buffer
	if code := RunStatus(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	if !bytes.Contains([]byte(out), []byte("quarantined")) {
		t.Errorf("status must report the slot as quarantined, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("live process still references")) {
		t.Errorf("status must say why the slot is quarantined, got:\n%s", out)
	}
}

// TestRunStatus_ReportsAPlainlyFreeSlotAsFree guards the other direction:
// the new column must not label every slot with a UDID as suspect. A slot
// with no lease, no holder and nothing alive against its device is free,
// and says so.
func TestRunStatus_ReportsAPlainlyFreeSlotAsFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	dir := pool.SlotDir(pool.GroupDir(home, "TestDevice", "1.0"), 0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A UDID, so CheckPoison actually runs its liveness check instead of
	// returning at its `meta.UDID == ""` guard: an empty slot directory
	// would exercise none of the path this test claims to guard, and would
	// go on passing however broken that path became.
	if err := pool.WriteMeta(dir, pool.Meta{
		UDID:     "simpool-test-udid-nothing-alive-against-it",
		Mode:     "lease",
		LeaseKey: "some-key",
		LastUsed: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := RunStatus(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	// On the slot's own row and in the AVAILABLE cell, not anywhere in the
	// output: "free" appears in enough prose that a whole-output substring
	// check would pass with the column saying something else entirely.
	fields := slotRowFields(t, stdout.String(), "slot-0")
	if fields[2] != "free" {
		t.Errorf("AVAILABLE for an unheld, unpoisoned slot: want free, got %q (row: %v)", fields[2], fields)
	}
	if fields[3] != "-" {
		t.Errorf("WHY for a free slot should be empty, got %q", fields[3])
	}
}

// slotRowFields returns the whitespace-separated cells of the status row
// for slot, so a test can assert on one column instead of on the whole
// output. Only usable for rows whose cells are single tokens — which is
// why the callers that check a multi-word WHY use substring matching on
// the row instead.
func slotRowFields(t *testing.T, out, slot string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, slot+" ") || strings.HasSuffix(line, slot) {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				t.Fatalf("status row for %s has too few columns: %q", slot, line)
			}
			return fields
		}
	}
	t.Fatalf("no status row for %s in:\n%s", slot, out)
	return nil
}

// TestRunLease_RefusalNamesTheRealObstacle proves the refusal a caller
// actually sees names what refused the slot. The old message asserted
// "every slot is busy or leased elsewhere" — about a slot that was neither
// busy nor leased — and then pointed at `simpool status`.
func TestRunLease_RefusalNamesTheRealObstacle(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	leaseResidueSlot(t, home, "owner-key", "simpool-test-udid-lease-refusal-names-obstacle")

	var stdout, stderr bytes.Buffer
	code := RunLease([]string{"--device", "TestDevice", "--os", "1.0", "--key", "someone-else", "--max", "1"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("a foreign key must not get a slot carrying another key's live residue, stdout:\n%s", stdout.String())
	}
	msg := stderr.String()
	for _, want := range []string{"slot-0", "quarantined", "live process still references"} {
		if !bytes.Contains([]byte(msg), []byte(want)) {
			t.Errorf("refusal must contain %q, got:\n%s", want, msg)
		}
	}
	if bytes.Contains([]byte(msg), []byte("every slot is busy or leased elsewhere")) {
		t.Errorf("refusal must not assert a reason it did not check, got:\n%s", msg)
	}
}

// TestRunRelease_SaysWhenTheSlotIsStillUnavailable closes the loop on the
// one command in this report that reported success while nothing the
// caller cared about changed: `release` removed a lease that was never what
// blocked the slot, said "released", and the next lease failed identically.
// It still releases (that part was always honest), but now says what is
// still in the way.
func TestRunRelease_SaysWhenTheSlotIsStillUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pool.EnvPoolHome, home)
	leaseResidueSlot(t, home, "owner-key", "simpool-test-udid-release-still-unavailable")

	var stdout, stderr bytes.Buffer
	if code := RunRelease([]string{"--key", "owner-key"}, &stdout, &stderr); code != 0 {
		t.Fatalf("release: want exit 0, got %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	if !bytes.Contains([]byte(out), []byte("released")) {
		t.Fatalf("expected the release confirmation, got:\n%s", out)
	}
	if !bytes.Contains([]byte(out), []byte("still quarantined")) {
		t.Errorf("release must report that the slot remains unavailable to other callers, got:\n%s", out)
	}
}
