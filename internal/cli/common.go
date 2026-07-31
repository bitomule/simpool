// Package cli implements simpool's subcommands: with, acquire, status,
// reap, doctor.
package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bitomule/simpool/internal/pool"
)

// acquireFlags are the flags shared by `with` and `acquire`.
type acquireFlags struct {
	device string
	os     string
	count  int
}

func parseAcquireFlags(fs *flag.FlagSet, f *acquireFlags) {
	fs.StringVar(&f.device, "device", "", "simulator device type, e.g. \"iPhone 17 Pro\" (required)")
	fs.StringVar(&f.os, "os", "", "simulator OS version, e.g. \"26.3\" (required)")
	fs.IntVar(&f.count, "count", 1, "number of slots to acquire")
}

func (f *acquireFlags) validate() error {
	if f.device == "" {
		return fmt.Errorf("--device is required")
	}
	if f.os == "" {
		return fmt.Errorf("--os is required")
	}
	if f.count < 1 {
		return fmt.Errorf("--count must be >= 1")
	}
	return nil
}

// splitDoubleDash splits args into the flags portion and the command
// portion at the first literal "--" token. If there is no "--", the whole
// slice is flags and the command portion is nil.
func splitDoubleDash(args []string) (flagArgs, cmdArgs []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// envLines renders slots into the environment contract from design doc §5.
// MAV_TARGET_* and MAV_EXACT_RUN_DIR describe the primary (first) slot,
// which covers the common single-target case; SIMPOOL_UDID_N /
// SIMPOOL_DEVICE_SET_N expose every slot for multi-target invocations.
func envLines(slots []*pool.Slot, runDir string) []string {
	primary := slots[0]
	lines := []string{
		"MAV_TARGET_KIND=simulator",
		"MAV_TARGET_UDID=" + primary.Meta.UDID,
		"MAV_TARGET_NAME=" + primary.Device,
		"MAV_TARGET_RUNTIME=" + primary.Meta.RuntimeID,
		"MAV_EXACT_RUN_DIR=" + runDir,
	}
	for i, s := range slots {
		lines = append(lines,
			fmt.Sprintf("SIMPOOL_UDID_%d=%s", i, s.Meta.UDID),
			fmt.Sprintf("SIMPOOL_DEVICE_SET_%d=%s", i, s.SetDir()),
		)
	}
	return lines
}

// printEnvLines writes shell `export` statements suitable for `eval "$(...)"`.
func printEnvLines(w io.Writer, lines []string) {
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "=")
		fmt.Fprintf(w, "export %s=%q\n", k, v)
	}
}

func nowStamp() string {
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}
