package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the block that uninstalls, before xcodebuild runs, every app
// xcodebuild is about to install. Extracted verbatim from the shipped
// template, for the reason the resolve-block tests give.
//
// The bug it fixes: Bazel's zips date every file 1980-01-01, and the
// simulator reinstalls an app it already has as a delta patch that keeps
// any file whose size and modification time match the installed copy. A
// resource whose text changed but not its length stayed from the previous
// build — a snapshot suite then rendered last build's strings.
const (
	staleBeginMarker = "# simpool: >>> stale-install-begin"
	staleEndMarker   = "# simpool: <<< stale-install-end"
	staleBundleID    = "com.simpool.staleinstalltest"
)

func extractStaleInstallBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the shipped template: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, staleBeginMarker)
	end := strings.Index(src, staleEndMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("the %s / %s markers are missing or out of order in %s", staleBeginMarker, staleEndMarker, templatePath)
	}
	return src[begin+len(staleBeginMarker) : end]
}

func TestRunnerTemplateStaleInstallBlockHasNoPlaceholders(t *testing.T) {
	if strings.Contains(extractStaleInstallBlock(t), "%"+"(") {
		t.Error("the stale-install block contains a Bazel placeholder")
	}
}

// runStaleInstallBlock runs the block under the template's own shell
// options, with simpool_run_bounded reduced to running its command, and
// `script` after it.
func runStaleInstallBlock(t *testing.T, env []string, script string) (string, error) {
	t.Helper()
	src := "set -euo pipefail\nsimpool_run_bounded() { shift; \"$@\"; }\n" + extractStaleInstallBlock(t) + "\n" + script
	cmd := exec.Command("/bin/bash", "-c", src)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeAppWithBundleID(t *testing.T, dir, name, bundleID string) string {
	t.Helper()
	app := filepath.Join(dir, name+".app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + bundleID + `</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-convert", "binary1", filepath.Join(app, "Info.plist")).CombinedOutput(); err != nil {
		t.Fatalf("converting %s's Info.plist to binary: %v\n%s", name, err, out)
	}
	return app
}

// fakeXcrun puts an xcrun on PATH that records its arguments and exits
// with $FAKE_XCRUN_EXIT.
func fakeXcrun(t *testing.T) (pathEnv, logPath string) {
	t.Helper()
	bin := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "xcrun.log")
	script := "#!/bin/bash\necho \"$*\" >> " + logPath + "\nexit ${FAKE_XCRUN_EXIT:-0}\n"
	if err := os.WriteFile(filepath.Join(bin, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + bin + ":" + os.Getenv("PATH"), logPath
}

func TestRunnerUninstallsEveryAppXcodebuildInstalls(t *testing.T) {
	dir := t.TempDir()
	host := writeAppWithBundleID(t, dir, "Host", "com.example.host")
	runner := writeAppWithBundleID(t, dir, "AppUITests-Runner", "com.apple.test.AppUITests-Runner")
	pathEnv, logPath := fakeXcrun(t)

	out, err := runStaleInstallBlock(t, []string{pathEnv},
		`simpool_uninstall_previous_install UDID-1 60 "`+host+`" "`+runner+`"`)
	if err != nil {
		t.Fatalf("the block failed: %v\n%s", err, out)
	}
	calls, _ := os.ReadFile(logPath)
	for _, want := range []string{
		"simctl uninstall UDID-1 com.example.host",
		"simctl uninstall UDID-1 com.apple.test.AppUITests-Runner",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("xcrun was not called with %q; calls:\n%s", want, calls)
		}
	}
}

// A failed uninstall leaves the old delta behaviour, not a broken run: the
// test still runs, with a warning saying why its resources may be stale.
func TestRunnerStaleInstallSurvivesAFailedUninstall(t *testing.T) {
	host := writeAppWithBundleID(t, t.TempDir(), "Host", "com.example.host")
	pathEnv, _ := fakeXcrun(t)

	out, err := runStaleInstallBlock(t, []string{pathEnv, "FAKE_XCRUN_EXIT=1"},
		`simpool_uninstall_previous_install UDID-1 60 "`+host+`"; echo reached`)
	if err != nil || !strings.Contains(out, "reached") {
		t.Fatalf("a failed uninstall stopped the run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning:") {
		t.Errorf("a failed uninstall said nothing:\n%s", out)
	}
}

// TestRunnerStaleInstallLive installs, on a real simulator, two builds of an
// app whose s.txt has the same length and different text, both dated
// 1980-01-01 as Bazel's zips leave them, and checks the second build's text
// is the one installed. Without the uninstall the simulator keeps "AAAA".
//
// Opt-in, because it needs a booted simulator:
//
//	simpool with --device "iPhone 17 Pro" --os 26.3 -- \
//	  sh -c 'SIMPOOL_STALE_INSTALL_TEST_UDID=$SIMPOOL_UDID_0 go test -run StaleInstallLive -v .'
func TestRunnerStaleInstallLive(t *testing.T) {
	udid := os.Getenv("SIMPOOL_STALE_INSTALL_TEST_UDID")
	if udid == "" {
		t.Skip("set SIMPOOL_STALE_INSTALL_TEST_UDID to a booted simulator to run this")
	}
	t.Cleanup(func() { _ = exec.Command("xcrun", "simctl", "uninstall", udid, staleBundleID).Run() })

	main := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(main, []byte("int main(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"AAAA", "BBBB"} {
		app := extractedBazelApp(t, main, text)
		out, err := runStaleInstallBlock(t, nil, `simpool_uninstall_previous_install "`+udid+`" 60 "`+app+`"`)
		if err != nil {
			t.Fatalf("the block failed: %v\n%s", err, out)
		}
		if out, err := exec.Command("xcrun", "simctl", "install", udid, app).CombinedOutput(); err != nil {
			t.Fatalf("installing the %s build: %v\n%s", text, err, out)
		}
		container, err := exec.Command("xcrun", "simctl", "get_app_container", udid, staleBundleID).Output()
		if err != nil {
			t.Fatalf("finding the installed app: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(container)), "s.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != text {
			t.Errorf("installed the %s build but the simulator has s.txt = %q", text, got)
		}
	}
}

// extractedBazelApp builds a simulator app whose s.txt holds text, zips it
// with every file dated 1980-01-01 as Bazel does, and unzips it the way the
// runner does. It returns the extracted .app.
func extractedBazelApp(t *testing.T, mainC, text string) string {
	t.Helper()
	stage := t.TempDir()
	app := filepath.Join(stage, "Payload", "Fix.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	run("xcrun", "-sdk", "iphonesimulator", "clang", "-target", "arm64-apple-ios26.0-simulator", "-o", filepath.Join(app, "Fix"), mainC)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>` + staleBundleID + `</string>
<key>CFBundleExecutable</key><string>Fix</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleSupportedPlatforms</key><array><string>iPhoneSimulator</string></array>
<key>MinimumOSVersion</key><string>26.0</string>
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "s.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	run("codesign", "-f", "-s", "-", app)
	run("find", filepath.Join(stage, "Payload"), "-exec", "touch", "-h", "-t", "198001010000", "{}", "+")
	ipa := filepath.Join(t.TempDir(), "app.ipa")
	zip := exec.Command("zip", "-qrX", ipa, "Payload")
	zip.Dir = stage
	if out, err := zip.CombinedOutput(); err != nil {
		t.Fatalf("zipping: %v\n%s", err, out)
	}
	extracted := t.TempDir()
	run("unzip", "-qq", "-d", extracted, ipa)
	return filepath.Join(extracted, "Payload", "Fix.app")
}
