package pool

import "testing"

// TestDeviceName_UniquePerSlot is the regression test for the finding that
// motivated including the slot number in the simulator's name at all: once
// every slot shares the default device set (design decision "opción (b)"),
// two slots of the same device+OS group producing the same simulator name
// would be a real collision — not just cosmetic, as it was when each slot
// had its own isolated device set.
func TestDeviceName_UniquePerSlot(t *testing.T) {
	a := DeviceName("iPhone 17 Pro", "26.3", 0)
	b := DeviceName("iPhone 17 Pro", "26.3", 1)
	if a == b {
		t.Fatalf("DeviceName must differ per slot number, got %q for both slot 0 and slot 1", a)
	}
	want := "SIMPOOL_iPhone-17-Pro_26.3_slot-0"
	if a != want {
		t.Errorf("DeviceName(iPhone 17 Pro, 26.3, 0) = %q, want %q", a, want)
	}
}

// TestDeviceName_HasPoolPrefix ties DeviceName and IsPoolName together:
// every name DeviceName produces must satisfy IsPoolName, or reap's guard
// against touching a non-pool-owned simulator would refuse to ever
// recognize simpool's own devices.
func TestDeviceName_HasPoolPrefix(t *testing.T) {
	name := DeviceName("iPad Pro", "18.0", 3)
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
