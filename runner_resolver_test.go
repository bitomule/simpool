package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the Python simulator resolver the template writes out and runs,
// extracted verbatim from its heredoc and run against a fake `xcrun` that
// reports this machine's real shape: the 26.3.1 patch runtime lives under the
// identifier "...iOS-26-3", not "...iOS-26-3-1".

const (
	resolverBegin = "cat > \"$simpool_resolver\" <<'PY'\n"
	resolverEnd   = "\nPY\n"
)

const fakeRuntimesJSON = `{"runtimes":[
 {"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-3","version":"26.3.1","platform":"iOS","isAvailable":true},
 {"identifier":"com.apple.CoreSimulator.SimRuntime.iOS-27-1","version":"27.1","platform":"iOS","isAvailable":true}
]}`

const fakeDevicesJSON = `{"devices":{
 "com.apple.CoreSimulator.SimRuntime.iOS-26-3":[{"udid":"UDID-26","name":"slot-0","state":"Booted"}],
 "com.apple.CoreSimulator.SimRuntime.iOS-27-1":[{"udid":"UDID-27","name":"slot-0","state":"Booted"}]
}}`

func extractResolver(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the shipped template: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, resolverBegin)
	if begin < 0 {
		t.Fatalf("the resolver heredoc is missing from %s", templatePath)
	}
	body := src[begin+len(resolverBegin):]
	end := strings.Index(body, resolverEnd)
	if end < 0 {
		t.Fatalf("the resolver heredoc in %s is not terminated", templatePath)
	}
	return body[:end]
}

func runResolver(t *testing.T, osVersion string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runtimes.json"), []byte(fakeRuntimesJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devices.json"), []byte(fakeDevicesJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	xcrun := "#!/bin/bash\ncase \"$*\" in\n" +
		"  'simctl list runtimes -j') /bin/cat \"" + filepath.Join(dir, "runtimes.json") + "\" ;;\n" +
		"  'simctl list devices -j') /bin/cat \"" + filepath.Join(dir, "devices.json") + "\" ;;\n" +
		"  *) echo \"unexpected xcrun $*\" >&2; exit 99 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(xcrun), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "resolve.py")
	if err := os.WriteFile(script, []byte(extractResolver(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/usr/bin/python3", "-I", script)
	cmd.Env = []string{
		"PATH=" + dir,
		"SIMULATOR_NAME=slot-0",
		"SIMULATOR_DEVICE_TYPE=iPhone 17 Pro",
		"SIMULATOR_OS_VERSION=" + osVersion,
		"SIMULATOR_REUSE_SIMULATOR=true",
		"SIMPOOL_SLOT_NAME=true",
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running the resolver: %v", err)
		}
		code = ee.ExitCode()
	}
	return strings.TrimSpace(stdout.String()), stderr.String(), code
}

func TestRunnerResolverFindsTheSlotOnAPatchRuntime(t *testing.T) {
	cases := map[string]string{
		"26.3":   "UDID-26",
		"26.3.1": "UDID-26",
		"27.1":   "UDID-27",
	}
	for osVersion, want := range cases {
		t.Run(osVersion, func(t *testing.T) {
			got, stderr, code := runResolver(t, osVersion)
			if code != 0 || got != want {
				t.Errorf("os_version %q: got %q (exit %d), want %q\n%s", osVersion, got, code, want, stderr)
			}
		})
	}
}

func TestRunnerResolverRefusesAVersionNoRuntimeHas(t *testing.T) {
	got, stderr, code := runResolver(t, "26.4")
	if code == 0 {
		t.Fatalf("os_version 26.4 resolved to %q; no runtime has that version", got)
	}
	if !strings.Contains(stderr, "no available iOS simulator runtime matches os_version '26.4'") {
		t.Errorf("the refusal does not name the version it could not match:\n%s", stderr)
	}
}
