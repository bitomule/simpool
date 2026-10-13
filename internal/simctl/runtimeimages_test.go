package simctl

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSDK(t *testing.T, xcode, platform, sdk, canonical, version string) {
	t.Helper()
	dir := filepath.Join(xcode, "Contents", "Developer", "Platforms", platform, "Developer", "SDKs", sdk)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"CanonicalName":"` + canonical + `","Version":"` + version + `","DisplayName":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "SDKSettings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two Xcodes, the versioned SDK alongside its unversioned alias (as Xcode
// ships them), a device-only platform that must be ignored, a Developer dir
// given the way xcode-select prints it, and an app matching Xcode*.app that
// is not an Xcode at all (Xcodes.app).
func TestSimulatorSDKs_ReadsEveryInstalledXcode(t *testing.T) {
	apps := t.TempDir()
	x271 := filepath.Join(apps, "Xcode-27.1.0-Release.Candidate.app")
	x263 := filepath.Join(apps, "Xcode-26.3.app")
	writeSDK(t, x271, "iPhoneSimulator.platform", "iPhoneSimulator27.1.sdk", "iphonesimulator27.1", "27.1")
	writeSDK(t, x271, "iPhoneSimulator.platform", "iPhoneSimulator.sdk", "iphonesimulator27.1", "27.1")
	writeSDK(t, x271, "WatchSimulator.platform", "WatchSimulator27.0.sdk", "watchsimulator27.0", "27.0")
	writeSDK(t, x271, "iPhoneOS.platform", "iPhoneOS27.1.sdk", "iphoneos27.1", "27.1")
	writeSDK(t, x263, "iPhoneSimulator.platform", "iPhoneSimulator26.3.sdk", "iphonesimulator26.3", "26.3")
	notXcode := filepath.Join(apps, "Xcodes.app")
	if err := os.MkdirAll(notXcode, 0o755); err != nil {
		t.Fatal(err)
	}

	got := SimulatorSDKs([]string{x271, filepath.Join(x263, "Contents", "Developer"), x263, notXcode})

	want := map[string]string{
		"com.apple.platform.iphonesimulator@27.1": x271,
		"com.apple.platform.watchsimulator@27.0":  x271,
		"com.apple.platform.iphonesimulator@26.3": x263,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d SDKs %+v, want %d", len(got), got, len(want))
	}
	for _, s := range got {
		if xcode, ok := want[s.Platform+"@"+s.Version]; !ok || xcode != s.Xcode {
			t.Errorf("unexpected SDK %+v", s)
		}
	}
}

func TestParseRuntimeImages_KeyedByUUID(t *testing.T) {
	data := `{"AAA":{"build":"24A94401","deletable":true,"runtimeIdentifier":"com.apple.CoreSimulator.SimRuntime.iOS-27-1","platformIdentifier":"com.apple.platform.iphonesimulator","sizeBytes":7873719649,"state":"Ready","version":"27.1","lastUsedAt":"2026-10-12T16:46:03Z"}}`
	imgs, err := ParseRuntimeImages([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("got %d images", len(imgs))
	}
	img := imgs[0]
	if img.Identifier != "AAA" || img.Version != "27.1" || img.SizeBytes != 7873719649 || !img.Deletable || img.State != "Ready" || img.LastUsedAt.IsZero() {
		t.Errorf("parsed %+v", img)
	}
}
