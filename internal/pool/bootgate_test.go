package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultBootConcurrency_EnvOverride(t *testing.T) {
	t.Setenv(EnvBootConcurrency, "7")
	if got := DefaultBootConcurrency(); got != 7 {
		t.Fatalf("DefaultBootConcurrency() = %d, want 7", got)
	}
}

func TestDefaultBootConcurrency_IgnoresGarbageOverride(t *testing.T) {
	t.Setenv(EnvBootConcurrency, "not-a-number")
	if got := DefaultBootConcurrency(); got <= 0 {
		t.Fatalf("DefaultBootConcurrency() = %d with a garbage override, want a positive fallback", got)
	}
}

// TestAcquireBootGate_LimitsConcurrency proves the gate actually caps how
// many holders can be inside it at once: with size 2, a third concurrent
// acquirer must never observe more than 2 simultaneous holders.
func TestAcquireBootGate_LimitsConcurrency(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvBootConcurrency, "2")

	var inFlight int32
	var maxObserved int32

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock, err := AcquireBootGate(root, 5*time.Second)
			if err != nil {
				t.Errorf("AcquireBootGate: %v", err)
				return
			}
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxObserved)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxObserved, prev, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			if err := lock.Release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxObserved); got > 2 {
		t.Fatalf("observed %d simultaneous boot-gate holders, want <= 2", got)
	}
}

// TestAcquireBootGate_TimesOutWhenExhausted proves a caller that can never
// get a slot fails with a clear, bounded error instead of blocking forever.
func TestAcquireBootGate_TimesOutWhenExhausted(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvBootConcurrency, "1")

	held, err := AcquireBootGate(root, time.Second)
	if err != nil {
		t.Fatalf("first AcquireBootGate: %v", err)
	}
	defer held.Release()

	start := time.Now()
	_, err = AcquireBootGate(root, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected AcquireBootGate to time out with the one slot already held")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("AcquireBootGate took %v to give up, want close to its own timeout", elapsed)
	}
}

// TestAcquireBootGate_ReleasedSlotIsReusable proves releasing a slot makes
// it immediately available to the next waiter — this is what keeps the
// gate from ever wedging the pool shut once boots finish.
func TestAcquireBootGate_ReleasedSlotIsReusable(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvBootConcurrency, "1")

	first, err := AcquireBootGate(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		l, err := AcquireBootGate(root, 2*time.Second)
		if err == nil {
			_ = l.Release()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second AcquireBootGate should have succeeded once the first released, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second AcquireBootGate never returned after the first slot was released")
	}
}

func TestBootGatePaths_AreStableAndDistinctPerSlot(t *testing.T) {
	root := t.TempDir()
	seen := map[string]bool{}
	for n := 0; n < 5; n++ {
		p := bootGateSlotPath(root, n)
		if seen[p] {
			t.Fatalf("slot %d path %q collides with another slot's path", n, p)
		}
		seen[p] = true
		if p2 := bootGateSlotPath(root, n); p2 != p {
			t.Fatalf("bootGateSlotPath(%d) not stable: %q != %q", n, p, p2)
		}
	}
}

func TestBootGateSlotPath_UsesFixedDirNotPerGroup(t *testing.T) {
	root := t.TempDir()
	p := bootGateSlotPath(root, 0)
	want := bootGateDir(root) + "/slot-0.lock"
	if p != want {
		t.Fatalf("bootGateSlotPath(root, 0) = %q, want %q", p, want)
	}
}
