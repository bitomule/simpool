package pool

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureWaitNotice redirects AcquireSlots' queue notices for one test.
// Synchronised because the notices are written from the acquiring
// goroutine while the test reads them.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureWaitNotice(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := waitNotice
	waitNotice = buf
	t.Cleanup(func() { waitNotice = prev })
	return buf
}

// TestAcquireSlots_SaysNothingWhenThereIsNothingToWaitFor is the control
// that has to come first, and it is the same discipline the measurement
// behind this change used: prove the instrument can read "fast" before
// trusting it to read "slow". An acquisition that gets its slot
// immediately — which is every acquisition on a healthy pool, measured at
// 21-79ms — must print no queue notice at all. A notice that also fires on
// the fast path would be noise on every single call and would be turned
// off within a day.
func TestAcquireSlots_SaysNothingWhenThereIsNothingToWaitFor(t *testing.T) {
	root := t.TempDir()
	buf := captureWaitNotice(t)

	slots, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, time.Minute)
	if err != nil {
		t.Fatalf("acquiring a slot in an empty pool: %v", err)
	}
	defer slots[0].Release()

	if got := buf.String(); got != "" {
		t.Errorf("an acquisition that never waited must print no queue notice, got:\n%s", got)
	}
}

// TestAcquireSlots_SaysWhyItIsWaiting is the defect. The wait loop polled
// in complete silence until it either succeeded or gave up, and the default
// --wait is ten minutes, so a node queueing for two minutes showed a still
// screen for two minutes. Several were peeked, messaged and reported as
// hung on exactly that evidence while they were merely in the queue.
//
// Two things are asserted, and the second is the one that matters: that
// something is printed while the wait is still happening, and that it
// carries the PER-SLOT reason. "Waiting" alone would not have prevented any
// of the confusion — "slot-0: busy" versus "slot-0: quarantined" is the
// difference between waiting calmly and going to look for a defect.
func TestAcquireSlots_SaysWhyItIsWaiting(t *testing.T) {
	root := t.TempDir()
	// Fill the group's one permitted slot, so a second caller has to wait.
	held, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, time.Minute)
	if err != nil {
		t.Fatalf("setting up the holder: %v", err)
	}
	defer held[0].Release()

	buf := captureWaitNotice(t)

	// Long enough to pass firstWaitNotice and print, short enough that the
	// test does not sit around: the notice is due one poll interval in.
	waitFor := firstWaitNotice + 3*acquirePollInterval
	if _, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, waitFor); err == nil {
		t.Fatal("the group is full and its one slot is held; this must not succeed")
	}

	got := buf.String()
	if got == "" {
		t.Fatal("the wait printed nothing at all — this is the defect")
	}
	if !strings.Contains(got, "waiting") {
		t.Errorf("the notice must say it is waiting, got:\n%s", got)
	}
	if !strings.Contains(got, "slot-0") {
		t.Errorf("the notice must name the slots and why each was refused, got:\n%s", got)
	}
}

// TestAcquireSlots_FirstNoticeComesEarly pins what the change is actually
// for. The silence that cost real time was the silence at the START: by the
// time a minute has gone by, somebody has already gone to look. So the
// first notice is due after one poll, not after a minute, and this fails if
// a future tidy-up raises firstWaitNotice to something that feels neater.
func TestAcquireSlots_FirstNoticeComesEarly(t *testing.T) {
	if firstWaitNotice > 5*time.Second {
		t.Fatalf("firstWaitNotice is %s; the initial silence is the whole problem, keep it short", firstWaitNotice)
	}
	if waitNoticeInterval <= firstWaitNotice {
		t.Fatalf("waitNoticeInterval (%s) must be longer than the first notice (%s): a line per poll is a log nobody reads", waitNoticeInterval, firstWaitNotice)
	}
}

// TestAcquireSlots_NamesEachSlotOnce pins a defect that was harmless while
// the refusal enumeration was only ever printed on giving up, and is not
// harmless now that the same reasons are summarised every 15s: acquisition
// walks the resident slots more than once (pass one takes only slots that
// already satisfy the request, pass three reconsiders the rest), and a slot
// refused in pass one is not in `taken`, so it was refused again and listed
// twice. Observed on the real binary as "slot-0: busy" printed twice for a
// group with exactly one slot; a group of six busy slots would report
// twelve.
func TestAcquireSlots_NamesEachSlotOnce(t *testing.T) {
	root := t.TempDir()
	held, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, time.Minute)
	if err != nil {
		t.Fatalf("setting up the holder: %v", err)
	}
	defer held[0].Release()

	_, err = AcquireSlots(root, "TestDevice", "26.3", 1, 1, 0)
	if err == nil {
		t.Fatal("expected a refusal: the group's one slot is held")
	}
	if n := strings.Count(err.Error(), "slot-0"); n != 1 {
		t.Errorf("slot-0 named %d times, want exactly 1:\n%s", n, err.Error())
	}

	var ce *capacityError
	if !errors.As(err, &ce) {
		t.Fatalf("the refusal must carry its refusals for the queue notice, got %T", err)
	}
	if !errors.Is(err, ErrAtCapacity) {
		t.Error("existing errors.Is(err, ErrAtCapacity) checks must keep working")
	}
	if n := strings.Count(ce.Summary(), "slot-0"); n != 1 {
		t.Errorf("summary names slot-0 %d times, want 1: %q", n, ce.Summary())
	}
	// The heartbeat must stay one line, and must not drag the remediation
	// advice along with it every fifteen seconds.
	if strings.Contains(ce.Summary(), "\n") {
		t.Errorf("the summary must be a single line, got %q", ce.Summary())
	}
	if strings.Contains(ce.Summary(), "raise --max") {
		t.Errorf("the summary must not repeat remediation advice, got %q", ce.Summary())
	}
}

// TestAcquireSlots_DoesNotPrintOnceItHasGivenUp keeps the notice from
// doubling the failure message. When the wait is over, the caller gets the
// full refusal as an error — the same text — and a notice printed in the
// same breath would say it twice.
func TestAcquireSlots_DoesNotPrintOnceItHasGivenUp(t *testing.T) {
	root := t.TempDir()
	held, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, time.Minute)
	if err != nil {
		t.Fatalf("setting up the holder: %v", err)
	}
	defer held[0].Release()

	buf := captureWaitNotice(t)

	// waitTimeout <= 0 means fail immediately: there is no wait, so there
	// is nothing to narrate.
	if _, err := AcquireSlots(root, "TestDevice", "26.3", 1, 1, 0); err == nil {
		t.Fatal("expected an immediate refusal")
	}
	if got := buf.String(); got != "" {
		t.Errorf("a no-wait acquisition must print no notice; the error already says everything, got:\n%s", got)
	}
}
