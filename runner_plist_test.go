package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the block that fills in XCTRunner.app's Info.plist for a UI
// test bundle. Extracted verbatim from the shipped template, for the reason
// the resolve-block tests give.
const (
	plistBeginMarker = "# simpool: >>> runner-plist-begin"
	plistEndMarker   = "# simpool: <<< runner-plist-end"
)

// The placeholders Xcode's own XCTRunner.app/Info.plist carries.
const runnerInfoPlistXML = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>$(WRAPPEDPRODUCTNAME)</string>
	<key>CFBundleIdentifier</key>
	<string>$(WRAPPEDPRODUCTBUNDLEIDENTIFIER)</string>
	<key>CFBundleName</key>
	<string>$(WRAPPEDPRODUCTNAME)</string>
</dict>
</plist>
`

func extractPlistBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("reading the shipped template: %v", err)
	}
	src := string(raw)
	begin := strings.Index(src, plistBeginMarker)
	end := strings.Index(src, plistEndMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("the %s / %s markers are missing or out of order in %s", plistBeginMarker, plistEndMarker, templatePath)
	}
	return src[begin+len(plistBeginMarker) : end]
}

func TestRunnerTemplatePlistBlockHasNoPlaceholders(t *testing.T) {
	if strings.Contains(extractPlistBlock(t), "%"+"(") {
		t.Error("the runner-plist block contains a Bazel placeholder")
	}
}

func writeRunnerInfoPlist(t *testing.T, format string) string {
	t.Helper()
	plist := filepath.Join(t.TempDir(), "Info.plist")
	if err := os.WriteFile(plist, []byte(runnerInfoPlistXML), 0o644); err != nil {
		t.Fatal(err)
	}
	if format != "xml1" {
		if out, err := exec.Command("/usr/bin/plutil", "-convert", format, plist).CombinedOutput(); err != nil {
			t.Fatalf("converting the fixture to %s: %v\n%s", format, err, out)
		}
	}
	return plist
}

func plistString(t *testing.T, plist, key string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist).CombinedOutput()
	if err != nil {
		t.Fatalf("reading %s from the rewritten plist: %v\n%s", key, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRunnerTemplateRewritesTheRunnerInfoPlist(t *testing.T) {
	const bundleID = "com.apple.test.AppUITests-Runner"
	for _, format := range []string{"binary1", "xml1"} {
		t.Run(format, func(t *testing.T) {
			plist := writeRunnerInfoPlist(t, format)
			script := "set -euo pipefail\n" + extractPlistBlock(t) +
				"\nsimpool_rewrite_runner_info_plist \"$1\" \"$2\"\n"
			cmd := exec.Command("/bin/bash", "-c", script, "bash", plist, bundleID)
			cmd.Env = []string{"PATH=/var/empty"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("running the runner-plist block: %v\n%s", err, out)
			}

			want := map[string]string{
				"CFBundleIdentifier": bundleID,
				"CFBundleExecutable": "XCTRunner",
				"CFBundleName":       "XCTRunner",
			}
			for key, value := range want {
				if got := plistString(t, plist, key); got != value {
					t.Errorf("%s = %q, want %q", key, got, value)
				}
			}

			raw, err := os.ReadFile(plist)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(raw), "bplist00") {
				t.Error("the rewritten plist is not binary; upstream rules_apple leaves it binary and so should this")
			}
		})
	}
}
