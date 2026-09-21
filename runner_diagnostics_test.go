package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Tests for the block that decides whether the runner asks Xcode to
// collect simulator diagnostics on a test failure.
//
// Extracted verbatim from the shipped template between its own markers,
// for the reason the resolve-block tests give: a copy of the shell pasted
// in here would pass forever while the template drifted away from it.
const (
	diagBeginMarker = "# simpool: >>> diagnostics-begin"
	diagEndMarker   = "# simpool: <<< diagnostics-end"
)

func extractDiagnosticsBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the shipped template: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, diagBeginMarker)
	end := strings.Index(src, diagEndMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("the %s / %s markers are missing or out of order in %s — these tests extract the block between them, and without the markers they would silently stop testing anything", diagBeginMarker, diagEndMarker, templatePath)
	}
	return src[begin+len(diagBeginMarker) : end]
}

// runDiagnosticsBlock runs the extracted block with custom_xcodebuild_args
// preset to args, then prints the two variables the block is there to
// decide, in the shape the argument list consumes them.
func runDiagnosticsBlock(t *testing.T, env []string, args []string) (value string, alreadySet string) {
	t.Helper()
	var decl strings.Builder
	decl.WriteString("custom_xcodebuild_args=(")
	for _, a := range args {
		decl.WriteString("'" + a + "' ")
	}
	decl.WriteString(")\n")

	full := "set -euo pipefail\n" + decl.String() + extractDiagnosticsBlock(t) +
		"\necho \"VALUE=$collect_test_diagnostics\"\necho \"ALREADY=$diagnostics_already_set\"\n"
	cmd := exec.Command("/bin/bash", "-c", full)
	cmd.Env = append([]string{"PATH=/var/empty"}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the extracted block: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "VALUE="); ok {
			value = v
		}
		if v, ok := strings.CutPrefix(line, "ALREADY="); ok {
			alreadySet = v
		}
	}
	return value, alreadySet
}

// TestRunnerDiagnosticsBlockHasNoPlaceholders holds this block to the same
// rule as the resolve block: a Bazel %(...)s inside it would be
// unsubstituted shell here, and the block could then only be tested by
// faking the substitution.
func TestRunnerDiagnosticsBlockHasNoPlaceholders(t *testing.T) {
	if strings.Contains(extractDiagnosticsBlock(t), "%"+"(") {
		t.Error("the extractable block contains a Bazel placeholder; pass what it needs through an environment variable instead")
	}
}

// TestRunnerDiagnosticsDefaultsToNever is the whole point: with nobody
// asking for anything, the runner turns the collection off. Left on, a red
// test costs its own 600-second timeout on top of the run.
func TestRunnerDiagnosticsDefaultsToNever(t *testing.T) {
	value, already := runDiagnosticsBlock(t, nil, nil)
	if value != "never" {
		t.Errorf("collect_test_diagnostics = %q, want \"never\"", value)
	}
	if already != "false" {
		t.Errorf("diagnostics_already_set = %q with no custom args, want \"false\"", already)
	}
}

// TestRunnerDiagnosticsEnvBringsThemBack is the escape hatch, and it is the
// half that stops this from being a silent removal: a sysdiagnose is
// exactly what you want when the simulator itself crashes or hangs, so
// there has to be a way to ask for one without editing the runner.
func TestRunnerDiagnosticsEnvBringsThemBack(t *testing.T) {
	value, _ := runDiagnosticsBlock(t, []string{"SIMPOOL_COLLECT_TEST_DIAGNOSTICS=on-failure"}, nil)
	if value != "on-failure" {
		t.Errorf("collect_test_diagnostics = %q with the env var set, want \"on-failure\"", value)
	}
}

// TestRunnerDiagnosticsEmptyEnvIsNotAnOverride: an exported-but-empty
// variable is what a shell that sets it conditionally produces, and it
// must not resolve to an empty -collect-test-diagnostics argument, which
// xcodebuild would reject.
func TestRunnerDiagnosticsEmptyEnvIsNotAnOverride(t *testing.T) {
	value, _ := runDiagnosticsBlock(t, []string{"SIMPOOL_COLLECT_TEST_DIAGNOSTICS="}, nil)
	if value != "never" {
		t.Errorf("collect_test_diagnostics = %q with the env var set to empty, want the default \"never\"", value)
	}
}

// TestRunnerDiagnosticsYieldsToAnExplicitFlag: if the caller passed
// -collect-test-diagnostics themselves, the runner must not append a
// second, contradictory copy. Theirs is the deliberate one.
func TestRunnerDiagnosticsYieldsToAnExplicitFlag(t *testing.T) {
	_, already := runDiagnosticsBlock(t, nil, []string{"-collect-test-diagnostics", "on-failure"})
	if already != "true" {
		t.Errorf("diagnostics_already_set = %q when the caller passed the flag, want \"true\"", already)
	}
}

// TestRunnerDiagnosticsIgnoresUnrelatedCustomArgs: the scan must match the
// flag and not merely notice that some custom args exist, or every target
// with any xcodebuild_args would silently keep the 600-second collection.
func TestRunnerDiagnosticsIgnoresUnrelatedCustomArgs(t *testing.T) {
	_, already := runDiagnosticsBlock(t, nil, []string{"-parallel-testing-enabled", "NO"})
	if already != "true" && already != "false" {
		t.Fatalf("unexpected value %q", already)
	}
	if already == "true" {
		t.Error("unrelated custom args were read as an explicit -collect-test-diagnostics")
	}
}

// TestRunnerDiagnosticsSurvivesSetU: the template runs under `set -u`, and
// an empty custom_xcodebuild_args array is the common case. An unguarded
// expansion of an empty array aborts the whole runner under bash 3.2,
// which is what macOS ships as /bin/bash.
func TestRunnerDiagnosticsSurvivesSetU(t *testing.T) {
	full := "set -euo pipefail\ncustom_xcodebuild_args=()\n" + extractDiagnosticsBlock(t) + "\necho OK\n"
	out, err := exec.Command("/bin/bash", "-c", full).CombinedOutput()
	if err != nil {
		t.Fatalf("the block aborts on an empty custom_xcodebuild_args under set -u: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Errorf("block did not run to completion:\n%s", out)
	}
}
