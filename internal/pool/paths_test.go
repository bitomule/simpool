package pool

import "testing"

// TestDeviceName_UniquePerSlot is the regression test for the finding that
// motivated including the slot number in the simulator's name at all: once
// every slot shares the default device set (design decision "opción (b)"),
// two slots of the same device+OS group producing the same simulator name
// would be a real collision — not just cosmetic, as it was when each slot
// had its own isolated device set.
func TestDeviceName_UniquePerSlot(t *testing.T) {
	a := DeviceName("/tmp/pool-a", "iPhone 17 Pro", "26.3", 0)
	b := DeviceName("/tmp/pool-a", "iPhone 17 Pro", "26.3", 1)
	if a == b {
		t.Fatalf("DeviceName must differ per slot number, got %q for both slot 0 and slot 1", a)
	}
	want := "SIMPOOL_" + RootTag("/tmp/pool-a") + "_iPhone-17-Pro@26.3_slot-0"
	if a != want {
		t.Errorf("DeviceName(/tmp/pool-a, iPhone 17 Pro, 26.3, 0) = %q, want %q", a, want)
	}
}

// TestDeviceName_UniquePerRoot is the regression test for the CRITICAL
// finding that DeviceName was derived only from (device, os, slot number),
// which is unique within a single pool root but not across roots: two
// independent SIMPOOL_HOME roots produced byte-identical names for "their"
// slot-0, and EnsureProvisioned's name-based recovery path (added to close
// a lost-meta.json leak) made the second root actively adopt — and reap
// could then shut down or delete — the first root's live simulator.
// Reproduced directly: two `simpool with` runs under different SIMPOOL_HOME
// values handed out the same UDID for slot-0 of the same device+OS group.
func TestDeviceName_UniquePerRoot(t *testing.T) {
	a := DeviceName("/tmp/pool-a", "iPhone 17 Pro", "26.3", 0)
	b := DeviceName("/tmp/pool-b", "iPhone 17 Pro", "26.3", 0)
	if a == b {
		t.Fatalf("DeviceName must differ per pool root, got %q for both /tmp/pool-a and /tmp/pool-b", a)
	}
}

// TestDeviceName_HasPoolPrefix ties DeviceName and IsPoolName together:
// every name DeviceName produces must satisfy IsPoolName, or reap's guard
// against touching a non-pool-owned simulator would refuse to ever
// recognize simpool's own devices.
func TestDeviceName_HasPoolPrefix(t *testing.T) {
	name := DeviceName("/tmp/pool-a", "iPad Pro", "18.0", 3)
	if !IsPoolName(name) {
		t.Errorf("IsPoolName(%q) = false, want true — DeviceName's own output must satisfy IsPoolName", name)
	}
}

// TestIsPoolName_RejectsUnrelatedNames is the regression test for the
// finding this whole check exists for: the default device set also holds
// the user's own simulators (34 on the design machine), and none of their
// names happen to start with the pool's prefix by construction, but
// IsPoolName must not be so loose that an unrelated name could pass.
func TestIsPoolName_RejectsUnrelatedNames(t *testing.T) {
	for _, name := range []string{
		"iPhone 17 Pro",
		"My Test Device",
		"simpool-iPhone-17-Pro_26.3", // the pre-migration naming scheme
		"",
	} {
		if IsPoolName(name) {
			t.Errorf("IsPoolName(%q) = true, want false", name)
		}
	}
}

// TestGroupName_SeparatorNotAmbiguous is the regression test for the LOW
// finding that GroupName's "_" separator could also appear inside a
// sanitized part, letting two different (device, osVersion) pairs collapse
// onto the same GroupName (and therefore the same simulator name):
// sanitize("iPhone 17", "Pro_26.3") and sanitize("iPhone 17_Pro", "26.3")
// both used to end up as "iPhone-17_Pro_26.3".
func TestGroupName_SeparatorNotAmbiguous(t *testing.T) {
	a := GroupName("iPhone 17", "Pro_26.3")
	b := GroupName("iPhone 17_Pro", "26.3")
	if a == b {
		t.Errorf("GroupName(%q, %q) and GroupName(%q, %q) collided: both %q", "iPhone 17", "Pro_26.3", "iPhone 17_Pro", "26.3", a)
	}
}
