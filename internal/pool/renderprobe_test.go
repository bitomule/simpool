package pool

import (
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/bitomule/simpool/internal/simctl"
)

func settingsLikeImage(width, height, cardTop int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		shade := uint8(243)
		if y >= cardTop {
			shade = 255
		}
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{shade, shade, shade, 255})
		}
	}
	return img
}

func TestFirstCardTopFindsTheEdgeBelowTheGreyBackground(t *testing.T) {
	top, ok := firstCardTop(settingsLikeImage(1206, 2622, 504))
	if !ok || top != 504 {
		t.Fatalf("firstCardTop = %d, %v; want 504, true", top, ok)
	}
}

func TestFirstCardTopReportsNothingWhenNoCardIsDrawn(t *testing.T) {
	if top, ok := firstCardTop(settingsLikeImage(1206, 2622, 99999)); ok {
		t.Fatalf("firstCardTop = %d, true on an image without a card", top)
	}
}

type probeScript struct {
	readings []RenderReading
	errs     []error
	calls    int
}

func (p *probeScript) probe(string, time.Duration) (RenderReading, error) {
	i := p.calls
	p.calls++
	if i < len(p.errs) && p.errs[i] != nil {
		return RenderReading{}, p.errs[i]
	}
	return p.readings[i], nil
}

func healthy() RenderReading { return RenderReading{ContentTop: 504, Width: 1206, Height: 2622} }
func noSafeArea() RenderReading {
	return RenderReading{ContentTop: 348, Width: 1206, Height: 2622}
}

type renderHarness struct {
	slot      *Slot
	deps      provisionDeps
	shutdowns int
	boots     int
}

func newRenderHarness(t *testing.T, probe *probeScript) *renderHarness {
	t.Helper()
	udid := "render-udid"
	s := fakeSlot(t, "iPhone 17 Pro", "26.3", udid)
	h := &renderHarness{slot: s}
	h.deps = provisionDeps{
		find: func(string) (simctl.DeviceEntry, bool, error) {
			return matchingEntry(udid, s.DeviceName(), "Booted"), true, nil
		},
		listDevices:    neverListDevices(t),
		resolveRuntime: stubResolveRuntime("runtime-1", "devicetype-1"),
		create:         neverCreate(t),
		bootAndWait:    func(string, time.Duration) error { h.boots++; return nil },
		shutdown:       func(string) error { h.shutdowns++; return nil },
		delete:         neverDelete(t),
		renderProbe:    probe.probe,
	}
	return h
}

func (h *renderHarness) provision() error {
	return ensureProvisioned(h.slot, "lease (key test)", "lease", "test", h.deps, time.Second)
}

func TestEnsureProvisioned_FirstReadingBecomesTheReference(t *testing.T) {
	h := newRenderHarness(t, &probeScript{readings: []RenderReading{healthy()}})
	if err := h.provision(); err != nil {
		t.Fatal(err)
	}
	if ref, ok := readRenderReference(h.slot.GroupDir); !ok || ref != healthy() {
		t.Fatalf("reference = %+v, %v; want %+v", ref, ok, healthy())
	}
	if h.boots != 0 || h.shutdowns != 0 {
		t.Fatalf("a healthy slot was rebooted (%d shutdowns, %d boots)", h.shutdowns, h.boots)
	}
}

func TestEnsureProvisioned_SlotWithoutSafeAreaIsRebootedBeforeHandOut(t *testing.T) {
	probe := &probeScript{readings: []RenderReading{noSafeArea(), healthy()}}
	h := newRenderHarness(t, probe)
	if err := writeRenderReference(h.slot.GroupDir, healthy()); err != nil {
		t.Fatal(err)
	}
	if err := h.provision(); err != nil {
		t.Fatalf("a slot that rendered correctly after the reboot was refused: %v", err)
	}
	if h.shutdowns != 1 || h.boots != 1 {
		t.Fatalf("rebooted %d shutdowns, %d boots; want 1 and 1", h.shutdowns, h.boots)
	}
	if probe.calls != 2 {
		t.Fatalf("probed %d times; want 2 (before and after the reboot)", probe.calls)
	}
}

func TestEnsureProvisioned_SlotStillWithoutSafeAreaAfterRebootIsRefused(t *testing.T) {
	h := newRenderHarness(t, &probeScript{readings: []RenderReading{noSafeArea(), noSafeArea()}})
	if err := writeRenderReference(h.slot.GroupDir, healthy()); err != nil {
		t.Fatal(err)
	}
	err := h.provision()
	if err == nil || !strings.Contains(err.Error(), "top safe area") {
		t.Fatalf("err = %v; want a refusal naming the missing top safe area", err)
	}
}

func TestEnsureProvisioned_UnrunnableProbeNeverBlocksHandOut(t *testing.T) {
	h := newRenderHarness(t, &probeScript{
		readings: []RenderReading{{}},
		errs:     []error{errors.New("simctl timed out")},
	})
	if err := h.provision(); err != nil {
		t.Fatalf("a probe that could not run refused the slot: %v", err)
	}
}

func TestEnsureProvisioned_RenewedLeaseIsNotProbed(t *testing.T) {
	probe := &probeScript{readings: []RenderReading{healthy()}}
	h := newRenderHarness(t, probe)
	h.slot.Renewed = true
	if err := h.provision(); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 0 {
		t.Fatalf("a sticky renewal was probed %d times", probe.calls)
	}
}

func TestEnsureProvisioned_RenderProbeEscapeHatch(t *testing.T) {
	t.Setenv(EnvRenderProbe, "0")
	probe := &probeScript{readings: []RenderReading{healthy()}}
	h := newRenderHarness(t, probe)
	if err := h.provision(); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 0 {
		t.Fatalf("probed %d times with %s=0", probe.calls, EnvRenderProbe)
	}
}
