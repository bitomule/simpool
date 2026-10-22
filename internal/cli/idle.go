package cli

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/pool"
	"github.com/bitomule/simpool/internal/simctl"
)

// EnvIdleShutdown is --cold's default when the flag is not passed: how long
// a free simulator stays booted before reap shuts it down.
const EnvIdleShutdown = "SIMPOOL_IDLE_SHUTDOWN"

// idleDuration is --cold's value. A bare number is minutes, which is what
// --cold has always meant (`--cold 60`); anything else is a Go duration
// (`5m`, `90s`), because a few minutes is now the useful range.
type idleDuration time.Duration

func parseIdleDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("%q: must not be negative", s)
		}
		return time.Duration(n) * time.Minute, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q: want minutes (60) or a duration (5m, 90s)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q: must not be negative", s)
	}
	return d, nil
}

func (d *idleDuration) Set(s string) error {
	v, err := parseIdleDuration(s)
	if err != nil {
		return err
	}
	*d = idleDuration(v)
	return nil
}

func (d *idleDuration) String() string { return time.Duration(*d).String() }

// defaultIdleShutdown resolves --cold when the flag is absent: the
// environment's value, or 0, which is what a hand-typed `simpool reap` has
// always used.
func defaultIdleShutdown() (time.Duration, error) {
	v := os.Getenv(EnvIdleShutdown)
	if v == "" {
		return 0, nil
	}
	d, err := parseIdleDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%w", EnvIdleShutdown, err)
	}
	return d, nil
}

const fullPassStampFile = ".reap-full-pass"

// fullPassDue reports whether this reap should run its expensive passes
// (--max, --warm, --scrub, orphans, ghosts). The scheduled service runs reap
// every minute so an idle simulator goes down within a minute of its idle
// time; those passes take ~25s and only need to run every half hour.
func fullPassDue(root string, every time.Duration, now time.Time) bool {
	if every <= 0 {
		return true
	}
	info, err := os.Stat(filepath.Join(root, fullPassStampFile))
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) >= every
}

func stampFullPass(root string, now time.Time) error {
	p := filepath.Join(root, fullPassStampFile)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		return err
	}
	return os.Chtimes(p, now, now)
}

// warmKept returns the slot numbers of groupDir that `--warm n` keeps
// booted: the n most recently used free, provisioned slots, which is the
// order both enforceWarmCap and enforceWarmFloor rank by. --cold leaves
// them alone; shutting them down only for the floor to boot them again is
// a cold boot's CPU and a simulator's memory spent for nothing.
func warmKept(groupDir string, n int) map[int]bool {
	if n <= 0 {
		return nil
	}
	type cand struct {
		n    int
		used time.Time
	}
	var cands []cand
	for _, num := range pool.ListSlotNumbers(groupDir) {
		dir := pool.SlotDir(groupDir, num)
		if lease, err := pool.ReadLease(dir); err != nil || lease.Alive() {
			continue
		}
		meta := pool.ReadMeta(dir)
		if meta.UDID == "" {
			continue
		}
		cands = append(cands, cand{num, meta.LastUsed})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].used.After(cands[j].used) })
	kept := map[int]bool{}
	for i := 0; i < len(cands) && i < n; i++ {
		kept[cands[i].n] = true
	}
	return kept
}

// snapshotDevices replaces findDevice with one `simctl list` for the rest
// of the per-slot pass, instead of one per slot: that listing was most of a
// pass's 5.8s on a 20-slot pool, and the pass now runs every minute. Each
// slot owns a different device, so a slot's own shutdown never makes the
// snapshot wrong for the next one. A failed listing keeps per-slot lookups.
func snapshotDevices() (restore func()) {
	orig := findDevice
	devices, err := listPoolDevices()
	if err != nil {
		return func() {}
	}
	byUDID := make(map[string]simctl.DeviceEntry, len(devices))
	for _, d := range devices {
		byUDID[d.UDID] = d
	}
	findDevice = func(udid string) (simctl.DeviceEntry, bool, error) {
		d, ok := byUDID[udid]
		return d, ok, nil
	}
	return func() { findDevice = orig }
}

// quietWriter drops the per-slot "nothing to do" lines of a fast pass, so a
// reap scheduled every minute logs what it did rather than twenty lines of
// status per run.
type quietWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

var quietPrefixes = []string{"KEEP ", "COLD ", "IDLE ", "SKIP "}

func (q *quietWriter) Write(p []byte) (int, error) {
	q.buf.Write(p)
	for {
		line, err := q.buf.ReadBytes('\n')
		if err != nil {
			q.buf.Write(line)
			return len(p), nil
		}
		if !hasQuietPrefix(line) {
			if _, err := q.w.Write(line); err != nil {
				return len(p), err
			}
		}
	}
}

func (q *quietWriter) Flush() {
	if rest := q.buf.Bytes(); len(rest) > 0 && !hasQuietPrefix(rest) {
		_, _ = q.w.Write(rest)
	}
	q.buf.Reset()
}

func hasQuietPrefix(line []byte) bool {
	for _, p := range quietPrefixes {
		if bytes.HasPrefix(line, []byte(p)) {
			return true
		}
	}
	return false
}

func flagPassed(fs *flag.FlagSet, name string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}
