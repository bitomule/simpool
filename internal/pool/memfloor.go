package pool

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// FreeMemoryFraction reports how much of physical memory is currently free,
// as 0..1, and whether it could be determined at all.
//
// A package-level var so tests can drive both sides of a threshold without
// a machine in a particular state.
//
// "Free" here is free + inactive + speculative pages, not just free: macOS
// keeps very little genuinely free at any time, and inactive pages are
// reclaimable on demand, so counting only free pages would report a healthy
// machine as nearly full and every floor built on this would refuse
// everything.
//
// It lives in this package rather than in internal/cli because two
// different floors now read the same number: `reap --warm`, which declines
// to boot a simulator nobody asked for yet, and acquisition's cold-boot
// branch below. One measurement, two consumers, same seam for tests.
var FreeMemoryFraction = liveFreeMemoryFraction

func liveFreeMemoryFraction() (float64, bool) {
	total, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, false
	}
	totalBytes, err := strconv.ParseFloat(strings.TrimSpace(string(total)), 64)
	if err != nil || totalBytes <= 0 {
		return 0, false
	}

	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, false
	}
	pageSize := 4096.0
	var free, inactive, speculative float64
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "... (page size of 16384 bytes)"
			if i := strings.Index(line, "page size of "); i >= 0 {
				if v, err := strconv.ParseFloat(strings.Fields(line[i+len("page size of "):])[0], 64); err == nil && v > 0 {
					pageSize = v
				}
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(value), "."), 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(name) {
		case "Pages free":
			free = v
		case "Pages inactive":
			inactive = v
		case "Pages speculative":
			speculative = v
		}
	}
	if free == 0 && inactive == 0 && speculative == 0 {
		// Parsed nothing usable — never read that as "plenty free".
		return 0, false
	}
	return (free + inactive + speculative) * pageSize / totalBytes, true
}

// MinFreeFraction reads an integer-percentage override out of the
// environment, falling back to def.
//
// An INTEGER percentage, deliberately: "0.35" is a valid float in range and
// would mean 0.35%, which all but disables the floor while reading like
// someone carefully setting 35%. Refusing it is the difference between a
// typo that falls back to the default and a typo that quietly removes the
// protection. A value outside 0-100, or one that does not parse at all, is
// ignored in favour of def rather than treated as 0.
func MinFreeFraction(env string, def float64) float64 {
	raw := os.Getenv(env)
	if raw == "" {
		return def
	}
	pct, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || pct < 0 || pct > 100 {
		return def
	}
	return float64(pct) / 100
}
