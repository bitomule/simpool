package pool

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

// A slot can come back booted and apparently healthy while its apps lay out
// without the top safe area: 132 Triage screenshots rendered ~52 pt too high
// on one slot, while the same suite passed on another. Nothing in simpool's
// own checks (SpringBoard running, presentation defaults) sees it, because
// the defaults are right and the process is up — the layout is simply wrong.
//
// So before a slot changes hands it renders a known screen, Settings, and
// measures where its first card starts. That position is a function of the
// safe area alone, so it is compared with what the same device and OS gave
// the first time it was measured. A slot that disagrees is rebooted once and
// measured again; one that still disagrees is refused rather than handed to
// a suite that would record wrong baselines.

// EnvRenderProbe disables the render check entirely when set to "0".
const EnvRenderProbe = "SIMPOOL_RENDER_PROBE"

// RenderProbeEnabled reports whether hand-out should render a known screen.
func RenderProbeEnabled() bool { return os.Getenv(EnvRenderProbe) != "0" }

const (
	settingsBundleID = "com.apple.Preferences"
	// renderTolerancePx absorbs anti-aliasing and Dynamic Type rounding. The
	// failure being guarded against moves content by 52 pt, 156 px on a
	// 3x device, so this is far below the signal.
	renderTolerancePx = 24
	renderProbeTries  = 8
	renderProbePause  = time.Second
)

// RenderReading is where the first card of Settings starts on a slot.
type RenderReading struct {
	ContentTop int `json:"contentTop"`
	Width      int `json:"width"`
	Height     int `json:"height"`
}

func renderReferencePath(groupDir string) string {
	return filepath.Join(groupDir, "render-probe.json")
}

func readRenderReference(groupDir string) (RenderReading, bool) {
	data, err := os.ReadFile(renderReferencePath(groupDir))
	if err != nil {
		return RenderReading{}, false
	}
	var ref RenderReading
	if json.Unmarshal(data, &ref) != nil || ref.ContentTop == 0 {
		return RenderReading{}, false
	}
	return ref, true
}

func writeRenderReference(groupDir string, ref RenderReading) error {
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return os.WriteFile(renderReferencePath(groupDir), data, 0o644)
}

func brightness(img image.Image, x, y int) int {
	r, g, b, _ := img.At(x, y).RGBA()
	return int((r>>8 + g>>8 + b>>8) / 3)
}

// firstCardTop finds, down the middle of a Settings screenshot, the row where
// the grouped-list background (light grey) gives way to the first white
// card. ok is false when the image has no such edge, which means Settings
// was not drawn yet or is showing something else.
func firstCardTop(img image.Image) (int, bool) {
	b := img.Bounds()
	x := b.Min.X + b.Dx()/2
	for y := b.Min.Y + b.Dy()/20; y < b.Min.Y+b.Dy()/2; y++ {
		if brightness(img, x, y) < 253 {
			continue
		}
		onGrey := true
		for above := 1; above <= 6; above++ {
			if v := brightness(img, x, y-above); v < 200 || v > 250 {
				onGrey = false
				break
			}
		}
		if onGrey {
			return y - b.Min.Y, true
		}
	}
	return 0, false
}

func readingOf(img image.Image) (RenderReading, bool) {
	top, ok := firstCardTop(img)
	b := img.Bounds()
	return RenderReading{ContentTop: top, Width: b.Dx(), Height: b.Dy()}, ok
}

func (have RenderReading) matches(ref RenderReading) bool {
	if have.Width != ref.Width || have.Height != ref.Height {
		return true
	}
	d := have.ContentTop - ref.ContentTop
	return d >= -renderTolerancePx && d <= renderTolerancePx
}

// liveRenderProbe launches Settings and screenshots it until the first card
// shows up, then closes it again.
func liveRenderProbe(udid string, timeout time.Duration) (RenderReading, error) {
	dir, err := os.MkdirTemp("", "simpool-render-")
	if err != nil {
		return RenderReading{}, err
	}
	defer os.RemoveAll(dir)
	defer simctl.Terminate(udid, settingsBundleID, timeout)

	if err := simctl.Launch(udid, settingsBundleID, timeout); err != nil {
		return RenderReading{}, err
	}
	shot := filepath.Join(dir, "settings.png")
	for try := 0; try < renderProbeTries; try++ {
		time.Sleep(renderProbePause)
		if err := simctl.Screenshot(udid, shot, timeout); err != nil {
			return RenderReading{}, err
		}
		f, err := os.Open(shot)
		if err != nil {
			return RenderReading{}, err
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		if reading, ok := readingOf(img); ok {
			return reading, nil
		}
	}
	return RenderReading{}, fmt.Errorf("Settings never drew its first card on %s", udid)
}

// verifyRender refuses a slot whose layout differs from the reference even
// after one reboot. A probe that cannot run is a warning, not a refusal: an
// unreadable screen says nothing about the safe area.
func verifyRender(deps provisionDeps, s *Slot, udid string, bootTimeout time.Duration) error {
	have, err := deps.renderProbe(udid, DefaultPresentationTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "simpool: could not render a known screen on %s (%v) — handing it out unchecked\n", udid, err)
		return nil
	}
	ref, known := readRenderReference(s.GroupDir)
	if !known || have.Width != ref.Width || have.Height != ref.Height {
		_ = writeRenderReference(s.GroupDir, have)
		return nil
	}
	if have.matches(ref) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "simpool: %s renders Settings %d px from the top, not the %d px this device gives — it lost its top safe area; rebooting it\n", udid, have.ContentTop, ref.ContentTop)
	if err := deps.shutdown(udid); err != nil {
		return fmt.Errorf("shutting down %s to recover its layout: %w", udid, err)
	}
	if err := deps.bootAndWait(udid, bootTimeout); err != nil {
		return fmt.Errorf("booting %s to recover its layout: %w", udid, err)
	}
	again, err := deps.renderProbe(udid, DefaultPresentationTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "simpool: could not render a known screen on %s after rebooting (%v) — handing it out unchecked\n", udid, err)
		return nil
	}
	if !again.matches(ref) {
		return fmt.Errorf("%s still renders Settings %d px from the top after a reboot, not the %d px this device gives — it would produce screenshots without the top safe area; use another slot or delete it with `simpool reap`", udid, again.ContentTop, ref.ContentTop)
	}
	fmt.Fprintf(os.Stderr, "simpool: %s renders correctly again after the reboot\n", udid)
	return nil
}
