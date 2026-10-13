package simctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RuntimeImage is one entry of `xcrun simctl runtime list -j`: a simulator
// runtime installed as a disk image under /Library/Developer/CoreSimulator
// (downloaded through Xcode > Settings > Components or `xcodebuild
// -downloadPlatform`), as opposed to a runtime bundled inside an Xcode.app.
// Only disk images are listed there and only they can be deleted, by hand,
// with `xcrun simctl runtime delete <Identifier>`; a bundled runtime goes
// away with its Xcode. simpool only ever reads this listing.
//
// Measured on a real host: each one is 7.8-8.4GB (SizeBytes) and is also
// mounted as its own APFS volume, so an unused one is the single largest
// thing a simulator host accumulates.
type RuntimeImage struct {
	// Identifier is the disk image's UUID, the argument `simctl runtime
	// delete` takes. Not RuntimeIdentifier.
	Identifier string `json:"identifier"`
	// RuntimeIdentifier is the CoreSimulator runtime id
	// ("com.apple.CoreSimulator.SimRuntime.iOS-27-1"), the same id devices
	// are filed under in `simctl list devices -j`.
	RuntimeIdentifier  string    `json:"runtimeIdentifier"`
	Version            string    `json:"version"`
	Build              string    `json:"build"`
	PlatformIdentifier string    `json:"platformIdentifier"`
	Kind               string    `json:"kind"`
	State              string    `json:"state"`
	Deletable          bool      `json:"deletable"`
	SizeBytes          int64     `json:"sizeBytes"`
	LastUsedAt         time.Time `json:"lastUsedAt"`
}

// ParseRuntimeImages decodes `simctl runtime list -j`, which is a JSON
// object keyed by image UUID rather than an array. Sorted by version so
// output that names them is stable.
func ParseRuntimeImages(data []byte) ([]RuntimeImage, error) {
	var doc map[string]RuntimeImage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing simctl runtime list: %w", err)
	}
	out := make([]RuntimeImage, 0, len(doc))
	for id, img := range doc {
		if img.Identifier == "" {
			img.Identifier = id
		}
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Identifier < out[j].Identifier
	})
	return out, nil
}

// ListRuntimeImages returns every simulator runtime disk image on the host.
func ListRuntimeImages() ([]RuntimeImage, error) {
	out, err := run("runtime", "list", "-j")
	if err != nil {
		return nil, err
	}
	return ParseRuntimeImages(out)
}

// SimulatorSDK is one simulator SDK shipped inside an installed Xcode: the
// OS version that Xcode builds and runs simulators against, and therefore
// the runtime that Xcode needs to be installed.
type SimulatorSDK struct {
	Xcode string
	// Platform is the CoreSimulator platform id the SDK targets, in the
	// same form RuntimeImage.PlatformIdentifier uses
	// ("com.apple.platform.iphonesimulator").
	Platform string
	Version  string
}

type sdkSettings struct {
	CanonicalName string `json:"CanonicalName"`
	Version       string `json:"Version"`
}

// SimulatorSDKs reads the simulator SDKs of every Xcode found under
// developerDirs (each an Xcode.app or its Contents/Developer). It reads each
// SDK's SDKSettings.json directly rather than running xcodebuild once per
// Xcode, which would need DEVELOPER_DIR switching and costs seconds each.
func SimulatorSDKs(developerDirs []string) []SimulatorSDK {
	seen := map[string]bool{}
	var out []SimulatorSDK
	for _, d := range developerDirs {
		dev := d
		if !strings.HasSuffix(dev, filepath.Join("Contents", "Developer")) {
			dev = filepath.Join(dev, "Contents", "Developer")
		}
		dev = filepath.Clean(dev)
		if seen[dev] {
			continue
		}
		seen[dev] = true
		xcode := strings.TrimSuffix(dev, string(filepath.Separator)+filepath.Join("Contents", "Developer"))
		matches, _ := filepath.Glob(filepath.Join(dev, "Platforms", "*Simulator.platform", "Developer", "SDKs", "*.sdk", "SDKSettings.json"))
		sdkSeen := map[string]bool{}
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			var s sdkSettings
			if json.Unmarshal(data, &s) != nil || s.Version == "" || s.CanonicalName == "" {
				continue
			}
			platform := "com.apple.platform." + strings.TrimRight(s.CanonicalName, "0123456789.")
			key := platform + "@" + s.Version
			if sdkSeen[key] {
				continue
			}
			sdkSeen[key] = true
			out = append(out, SimulatorSDK{Xcode: xcode, Platform: platform, Version: s.Version})
		}
	}
	return out
}

// InstalledXcodes returns every Xcode this host could build with: each
// /Applications/Xcode*.app, plus whatever `xcode-select -p` and
// DEVELOPER_DIR point at, which can live anywhere.
func InstalledXcodes() []string {
	dirs, _ := filepath.Glob("/Applications/Xcode*.app")
	if v := os.Getenv("DEVELOPER_DIR"); v != "" {
		dirs = append(dirs, v)
	}
	if out, err := exec.Command("xcode-select", "-p").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			dirs = append(dirs, p)
		}
	}
	return dirs
}
