package main

// Tests for the simpool-specific part of the Bazel test-runner template
// (bazel/simpool_ios_test_runner.template.sh).
//
// The template is a shell script Bazel fills in at analysis time, so it
// cannot be run whole from here. What can be run is the block that decides
// whether this machine has a simpool binary and what to do when it does
// not — which is the block that matters, because the old answer was "build
// your own simulator and say nothing", and that is how a BAZEL_TEST_*
// simulator nobody reclaims comes into existence.
//
// The block is extracted verbatim between two markers rather than copied
// here. A copy would pass forever while the shipped template drifted away
// from it, which is the failure mode these tests exist to prevent.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	templatePath = "bazel/simpool_ios_test_runner.template.sh"
	beginMarker  = "# simpool: >>> resolve-begin"
	endMarker    = "# simpool: <<< resolve-end"
)

// extractResolveBlock returns the shell between the two markers.
func extractResolveBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the shipped template: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, beginMarker)
	end := strings.Index(src, endMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("the %s / %s markers are missing or out of order in %s — these tests extract the block between them, and without the markers they would silently stop testing anything", beginMarker, endMarker, templatePath)
	}
	return src[begin+len(beginMarker) : end]
}

// The extracted block has to stand on its own: a Bazel %(...)s placeholder
// inside it would be unsubstituted shell here and, worse, would mean the
// block could only be tested by faking the substitution — which is how a
// test stops resembling what ships.
func TestRunnerTemplateResolveBlockHasNoPlaceholders(t *testing.T) {
	block := extractResolveBlock(t)
	if strings.Contains(block, "%"+"(") {
		t.Errorf("the extractable block contains a Bazel placeholder; pass what the message needs through an environment variable the caller sets instead:\n%s", block)
	}
}

// runBlock runs the extracted block plus `script` under bash and returns
// stdout+stderr with the exit code.
//
// Two things are deliberately hostile here. PATH is emptied, which is what a
// Bazel test action's environment actually looks like — it is how these
// tests caught the block reaching for `seq` and `sleep`, neither of which a
// resolver should need to decide whether a file exists. And the search
// prefixes are pointed at an empty directory, because this machine has
// simpool installed at /opt/homebrew/bin: without that, every "no binary
// anywhere" test would pass or fail for reasons having nothing to do with
// the code.
func runBlock(t *testing.T, env []string, script string) (string, int) {
	t.Helper()
	full := "set -euo pipefail\n" + extractResolveBlock(t) + "\n" + script
	cmd := exec.Command("/bin/bash", "-c", full)
	base := map[string]string{
		"PATH":                    "/var/empty",
		"SIMPOOL_SEARCH_PREFIXES": t.TempDir(),
		"SIMPOOL_FALLBACK_NAME":   "BAZEL_TEST_iPhone-17-Pro_26.3",
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}
	// Merged through a map rather than appended: a duplicate key in an
	// exec environment resolves differently depending on the libc, and a
	// test whose PATH override may or may not take effect is worse than no
	// test.
	cmd.Env = nil
	for k, v := range base {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the extracted block: %v\n%s", err, out)
	}
	return string(out), code
}

// fakeSimpool writes an executable file to stand in for the binary.
func fakeSimpool(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "simpool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunnerTemplateFindsTheBinaryAtSimpoolBin(t *testing.T) {
	bin := fakeSimpool(t, t.TempDir())
	out, code := runBlock(t, []string{"SIMPOOL_BIN=" + bin}, `simpool_require_bin`)
	if code != 0 {
		t.Fatalf("want exit 0 when the binary is right there, got %d:\n%s", code, out)
	}
	if strings.TrimSpace(out) != bin {
		t.Errorf("want the resolved path %q printed, got %q", bin, out)
	}
}

// The change this whole commit is about. A missing binary used to mean a
// silent fallback that built a BAZEL_TEST_<device>_<os> simulator with no
// lease and no reaper; two such simulators were found still booted days
// after the sessions that made them had been closed.
func TestRunnerTemplateRefusesToRunWithoutAPool(t *testing.T) {
	out, code := runBlock(t, []string{"SIMPOOL_BIN=/nonexistent/simpool"}, `simpool_require_bin`)
	if code != 1 {
		t.Fatalf("a missing simpool must fail the test action (exit 1), got %d:\n%s", code, out)
	}
	// The next person must not have to deduce where it looked, which is
	// exactly what this session had to do to find the stray simulator.
	for _, want := range []string{
		"$SIMPOOL_BIN",
		"$PATH",
		"brew install bitomule/tap/simpool",
		"SIMPOOL_OPTIONAL=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal must name %q so nobody has to guess; got:\n%s", want, out)
		}
	}
}

// The message renders the prefixes it actually searched, so what it names
// and what it checked cannot drift — but the shipped defaults still have to
// be the real ones, and no runtime test can assert that while overriding
// them. This reads the block itself.
func TestRunnerTemplateShipsTheRealSearchPrefixes(t *testing.T) {
	block := extractResolveBlock(t)
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(block, want) {
			t.Errorf("the shipped default search prefixes must include %q", want)
		}
	}
}

// And the rendered message must list whatever prefixes were in play, not a
// second hardcoded list beside the loop — which is what it used to be.
func TestRunnerTemplateMessageNamesThePrefixesItSearched(t *testing.T) {
	dir := t.TempDir()
	out, _ := runBlock(t, []string{
		"SIMPOOL_SEARCH_PREFIXES=" + dir,
		"SIMPOOL_BIN=/nonexistent/simpool",
	}, `simpool_require_bin`)
	if !strings.Contains(out, dir+"/simpool") {
		t.Errorf("the refusal must name the prefix it actually searched (%s), got:\n%s", dir, out)
	}
}

// Running without a pool stays possible — a machine with no simpool, CI, a
// fresh checkout — but as somebody's decision rather than as what happens
// when a lookup fails.
func TestRunnerTemplateRunsUnpooledOnlyWhenAskedTo(t *testing.T) {
	out, code := runBlock(t, []string{
		"SIMPOOL_BIN=/nonexistent/simpool",
		"SIMPOOL_OPTIONAL=1",
	}, `simpool_require_bin || true`)
	if code != 0 {
		t.Fatalf("SIMPOOL_OPTIONAL=1 must not fail the action, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "warning:") {
		t.Errorf("the opt-out must still warn — it is accepting an unmanaged simulator, not a free pass; got:\n%s", out)
	}
	if !strings.Contains(out, "BAZEL_TEST_iPhone-17-Pro_26.3") {
		t.Errorf("the warning must name the simulator that will be shared, got:\n%s", out)
	}
}

func TestRunnerTemplateOptionalIsExactlyOne(t *testing.T) {
	// Not "any non-empty value": SIMPOOL_OPTIONAL=0 reads as "no" to
	// everyone who writes it, and silently meaning "yes" would give away
	// the pool to someone who thought they had asked for the opposite.
	for _, v := range []string{"0", "false", "no", ""} {
		out, code := runBlock(t, []string{
			"SIMPOOL_BIN=/nonexistent/simpool",
			"SIMPOOL_OPTIONAL=" + v,
		}, `simpool_require_bin`)
		if code != 1 {
			t.Errorf("SIMPOOL_OPTIONAL=%q must not enable the unpooled path (want exit 1, got %d):\n%s", v, code, out)
		}
	}
}

// The case that is believed to have produced the one stray simulator on
// this machine: `brew upgrade` unlinks the binary and relinks it a moment
// later, and a test action that looks inside that window sees nothing. A
// miss is not proof of absence.
func TestRunnerTemplateSurvivesTheBinaryBlinkingDuringABrewUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "simpool")
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}()
	start := time.Now()
	out, code := runBlock(t, []string{"SIMPOOL_BIN=" + path}, `simpool_require_bin`)
	if code != 0 {
		t.Fatalf("a binary that reappears within the retry window must be found, got %d after %s:\n%s", code, time.Since(start), out)
	}
	if strings.TrimSpace(out) != path {
		t.Errorf("want %q, got %q", path, out)
	}
}

// And the other half of that: the retry must not cost anything when the
// binary is simply there, which is every run on a healthy machine.
func TestRunnerTemplateDoesNotSleepWhenTheBinaryIsPresent(t *testing.T) {
	bin := fakeSimpool(t, t.TempDir())
	start := time.Now()
	if _, code := runBlock(t, []string{"SIMPOOL_BIN=" + bin}, `simpool_require_bin`); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("the happy path must not sleep; took %s", elapsed)
	}
}

// A non-executable file at the path is not a usable binary, and treating it
// as one would turn a clear refusal into an exec failure much further down.
func TestRunnerTemplateIgnoresANonExecutableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "simpool")
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runBlock(t, []string{"SIMPOOL_BIN=" + path}, `simpool_require_bin`)
	if code != 1 {
		t.Fatalf("a non-executable file must not count as the binary, got %d:\n%s", code, out)
	}
}

func TestRunnerTemplateFindsTheBinaryOnPath(t *testing.T) {
	dir := t.TempDir()
	fakeSimpool(t, dir)
	out, code := runBlock(t, []string{"PATH=" + dir}, `simpool_require_bin`)
	if code != 0 {
		t.Fatalf("a simpool on $PATH must be found, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("want the $PATH copy resolved, got %q", out)
	}
}
