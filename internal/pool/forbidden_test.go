package pool

import (
	"os"
	"path/filepath"
	"testing"
)

// SIMPOOL_HOME is honoured verbatim. This is the test that would have to be
// deleted to reintroduce a special-cased location, which is the point of
// writing it: the previous guard refused "/Volumes/BazelCache" on the
// grounds that a pool of multi-GB simulators would starve the Bazel cache
// sharing that volume, and no simulator has ever lived in the pool root
// (see Root's doc comment for the measurement).
func TestRoot_HonoursAnyPoolHome(t *testing.T) {
	for _, name := range []string{"BazelCache", "Volumes", "pool", "weird name"} {
		sub := filepath.Join(t.TempDir(), name, "SimPool")
		t.Setenv(EnvPoolHome, sub)

		got, err := Root()
		if err != nil {
			t.Fatalf("Root() with SIMPOOL_HOME=%q: %v — no location is special to simpool", sub, err)
		}
		if got != sub {
			t.Fatalf("Root() = %q, want %q", got, sub)
		}
		if info, err := os.Stat(sub); err != nil || !info.IsDir() {
			t.Fatalf("Root() did not create %q: %v", sub, err)
		}
	}
}

// The pool root is bookkeeping, not storage: locks (empty — flock lives on
// the inode) and small meta.json files. Anything that starts writing
// something substantial here would invalidate the reasoning that removed
// the volume guard, so it is worth failing loudly rather than discovering
// it on a full disk.
func TestPoolRootStaysSmall(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvPoolHome, root)
	if _, err := Root(); err != nil {
		t.Fatalf("Root(): %v", err)
	}

	groupDir := GroupDir(root, "iPhone 17 Pro", "26.3")
	for n := 0; n < 8; n++ {
		dir := SlotDir(groupDir, n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(LockPath(dir), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WriteMeta(dir, Meta{
			UDID:      "3A8339E4-1A6D-435F-B4AC-47599DDA6587",
			Device:    "iPhone 17 Pro",
			OSVersion: "26.3",
		}); err != nil {
			t.Fatal(err)
		}
	}

	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Eight fully-populated slots measured well under 8 KB of file content;
	// 256 KB is a ceiling that only a change in kind could cross.
	const ceiling = 256 * 1024
	if total > ceiling {
		t.Fatalf("eight populated slots hold %d bytes, over the %d-byte ceiling — the pool root is storing something substantial now, which is exactly the assumption Root's doc comment relies on", total, ceiling)
	}
}
