package pool

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// MachineLoad is what the machine was doing at one instant: the 1-minute
// load average and how much swap was free.
//
// It exists because of a question this hive could not answer three times in
// one night — "what was the load when that measurement ran?" — asked after
// the fact, about runs that had already finished, with nobody able to say.
// A batch of six UI captures came back stuck on a machine at load 66; a
// baseline of ten navigations came back clean on the same pool. Whether
// those two facts are related is still unknown, and this does not answer
// it. What it does is make the question answerable NEXT time, which is the
// part that was missing.
//
// Deliberately recorded and printed, never acted on. The only evidence
// linking load to swallowed input is a single batch with no control arm and
// two variables moving at once — n=1 — and refusing to hand out a slot on
// the strength of that would stop the hive for something nobody has
// measured. A number in the log costs nothing and can be checked against
// later; a threshold invented tonight would be a guess with the authority
// of code.
type MachineLoad struct {
	// Load1 is the 1-minute load average.
	Load1 float64
	// SwapFreeMB and SwapUsedMB are macOS's own swap accounting, in
	// megabytes, as `sysctl vm.swapusage` reports them.
	SwapFreeMB float64
	SwapUsedMB float64
	// OK is false when the reading could not be taken at all. Every
	// consumer must treat that as "unknown" and print nothing rather than
	// printing a zero, which would read as an idle machine.
	OK bool
}

// String is the form that goes in a log line: short enough to sit at the
// end of an existing line, complete enough to be worth reading back.
func (m MachineLoad) String() string {
	if !m.OK {
		return "load unknown"
	}
	return fmt.Sprintf("load %.2f, swap %.0fM free", m.Load1, m.SwapFreeMB)
}

// ReadMachineLoad is a package-level var so tests can stage a machine state
// without one, mirroring FreeMemoryFraction.
var ReadMachineLoad = liveMachineLoad

// liveMachineLoad reads both numbers from a SINGLE `sysctl` invocation.
//
// One subprocess, not two, because this runs on `simpool lease`'s hot
// path — once per mav tap/swipe/screenshot — and the lease's whole reason
// for existing is that it is cheap. Measured (BenchmarkReadMachineLoad,
// three runs of 100 on a machine at load ~10): 2.16-2.28ms per call, the
// cost of one exec. Note this is deliberately NOT built on
// liveFreeMemoryFraction, which spends two subprocesses (`sysctl` plus
// `vm_stat`) for a different number and would roughly double it.
func liveMachineLoad() MachineLoad {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg", "vm.swapusage").Output()
	if err != nil {
		return MachineLoad{}
	}
	return parseMachineLoad(string(out))
}

// parseMachineLoad is split out and pure so the two output shapes can be
// tested against captured text rather than against whatever this machine
// happens to be doing while the suite runs.
//
// Expected input, in this order:
//
//	{ 12.06 10.32 15.32 }
//	total = 9216.00M  used = 8007.31M  free = 1208.69M  (encrypted)
//
// A line that does not parse leaves its own field alone rather than
// failing the whole reading: a kernel that stops reporting swap should not
// also cost us the load average. OK is set if EITHER half was understood,
// and String only omits the half that is genuinely missing.
func parseMachineLoad(out string) MachineLoad {
	var m MachineLoad
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "{"):
			fields := strings.Fields(strings.Trim(line, "{} "))
			if len(fields) == 0 {
				continue
			}
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				m.Load1 = v
				m.OK = true
			}
		case strings.Contains(line, "total ="):
			for _, part := range strings.Split(line, "  ") {
				name, value, ok := strings.Cut(part, "=")
				if !ok {
					continue
				}
				v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "M"), 64)
				if err != nil {
					continue
				}
				switch strings.TrimSpace(name) {
				case "free":
					m.SwapFreeMB = v
					m.OK = true
				case "used":
					m.SwapUsedMB = v
					m.OK = true
				}
			}
		}
	}
	return m
}
