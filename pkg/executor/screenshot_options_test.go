package executor

import (
	"bytes"
	"image"
	imagecolor "image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// tenByTen makes a 10x10 PNG, white but for the listed black pixels.
func tenByTen(t *testing.T, black ...image.Point) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, imagecolor.White)
		}
	}
	for _, p := range black {
		img.Set(p.X, p.Y, imagecolor.Black)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// screenshotRunner answers captures with capture, and finds elements only when
// the selector's id is "found", at the top-left quarter of a 5x5 point screen.
func screenshotRunner(dir string, capture []byte) *FlowRunner {
	driver := &mockDriver{
		executeFunc: func(step flow.Step) *core.CommandResult {
			switch s := step.(type) {
			case *flow.AssertScreenshotStep:
				return &core.CommandResult{Success: true, Data: capture}
			case *flow.AssertVisibleStep:
				if s.Selector.ID == "found" {
					return &core.CommandResult{Success: true, Element: &core.ElementInfo{
						Bounds: core.Bounds{X: 0, Y: 0, Width: 2, Height: 2},
					}}
				}
				return &core.CommandResult{Success: false, Message: "not there"}
			}
			return &core.CommandResult{Success: true}
		},
		platformFunc: func() *core.PlatformInfo {
			return &core.PlatformInfo{ScreenWidth: 5, ScreenHeight: 5}
		},
	}
	script := NewScriptEngine()
	script.SetFlowDir(dir)
	return &FlowRunner{driver: driver, script: script, config: RunnerConfig{}}
}

func TestAssertScreenshotMask(t *testing.T) {
	dir := t.TempDir()
	reference := filepath.Join(dir, "reference.png")
	if err := os.WriteFile(reference, tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	// One pixel differs inside the masked quarter of the image.
	fr := screenshotRunner(dir, tenByTen(t, image.Pt(1, 1)))

	masked := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
	})
	if !masked.Success {
		t.Errorf("a masked difference should pass: %s", masked.Message)
	}

	unmasked := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, CaptureOut: "capture",
	})
	if unmasked.Success {
		t.Fatal("the same difference unmasked should fail")
	}
	if _, err := os.Stat(core.DiffScreenshotPath(reference)); err != nil {
		t.Errorf("a mismatch should write a diff: %v", err)
	}
	outcome, ok := unmasked.Data.(core.ScreenshotOutcome)
	if !ok {
		t.Fatalf("result data: %#v", unmasked.Data)
	}
	if outcome.DifferingPixels != 1 || outcome.TotalPixels != 100 || outcome.DiffPath == "" {
		t.Errorf("outcome: %+v", outcome)
	}

	outside := screenshotRunner(dir, tenByTen(t, image.Pt(9, 9)))
	if result := outside.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
	}); result.Success {
		t.Error("a difference outside the mask should still fail")
	}
}

func TestAssertScreenshotMaskFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reference.png"), tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := screenshotRunner(dir, tenByTen(t))

	missing := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "absent"}},
	})
	if missing.Success || !strings.Contains(missing.Message, "element not found") {
		t.Errorf("a mask matching nothing must fail: %+v", missing)
	}

	both := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100,
		Mask: []flow.Selector{{ID: "found"}}, CropOn: &flow.Selector{ID: "found"},
	})
	if both.Success || !strings.Contains(both.Message, "cropOn") {
		t.Errorf("mask with cropOn must fail: %+v", both)
	}

	noSize := screenshotRunner(dir, tenByTen(t))
	noSize.driver.(*mockDriver).platformFunc = func() *core.PlatformInfo { return &core.PlatformInfo{} }
	unsized := noSize.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
	})
	if unsized.Success || !strings.Contains(unsized.Message, "screen dimensions") {
		t.Errorf("a mask needs a screen size: %+v", unsized)
	}
}

func TestAssertScreenshotRequireBaseline(t *testing.T) {
	dir := t.TempDir()
	fr := screenshotRunner(dir, tenByTen(t))

	result := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "new", ThresholdPercentage: 100, RequireBaseline: true, CaptureOut: "capture",
	})
	if result.Success {
		t.Fatal("a missing baseline must fail")
	}
	outcome, ok := result.Data.(core.ScreenshotOutcome)
	if !ok || outcome.Code != core.BaselineMissingCode {
		t.Fatalf("result data: %#v", result.Data)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.png")); !os.IsNotExist(err) {
		t.Error("no reference may be created")
	}
	if _, err := os.Stat(outcome.CapturePath); err != nil {
		t.Errorf("the settled frame should be saved: %v", err)
	}

	seeded := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{Path: "seeded", ThresholdPercentage: 100})
	if !seeded.Success {
		t.Errorf("without the field a missing baseline still seeds: %s", seeded.Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "seeded.png")); err != nil {
		t.Errorf("baseline should exist: %v", err)
	}
	if _, ok := seeded.Data.([]byte); !ok {
		t.Errorf("without the fields, data stays the capture: %T", seeded.Data)
	}
}

func TestAssertScreenshotCaptureOnPass(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reference.png"), tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := screenshotRunner(dir, tenByTen(t))
	result := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, CaptureOut: "sub/capture.png",
	})
	if !result.Success {
		t.Fatal(result.Message)
	}
	outcome := result.Data.(core.ScreenshotOutcome)
	if outcome.MatchPercentage != 100 || outcome.CapturePath != filepath.Join(dir, "sub", "capture.png") {
		t.Errorf("outcome: %+v", outcome)
	}
	escaped := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, CaptureOut: "../../escape",
	})
	if escaped.Success {
		t.Error("a capture outside the workspace must be refused")
	}
}

func TestAssertScreenshotCaptureErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := screenshotRunner(dir, tenByTen(t))
	blocked := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, CaptureOut: "blocker/capture",
	})
	if blocked.Success {
		t.Error("a capture path that cannot be created must fail")
	}
	if err := os.Mkdir(filepath.Join(dir, "taken.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	unwritable := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, CaptureOut: "taken",
	})
	if unwritable.Success {
		t.Error("a capture that cannot be written must fail")
	}
	noInfo := screenshotRunner(dir, tenByTen(t))
	noInfo.driver.(*mockDriver).platformFunc = func() *core.PlatformInfo { return nil }
	if err := os.WriteFile(filepath.Join(dir, "reference.png"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := noInfo.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
	})
	if result.Success {
		t.Error("a mask with no platform info must fail")
	}
	badReference := screenshotRunner(dir, tenByTen(t))
	corrupt := badReference.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
	})
	if corrupt.Success {
		t.Error("masking a corrupt reference must fail")
	}
}

// locatingDriver finds elements through LocateElement, as the Android drivers do,
// and returns empty bounds from its visibility check, as they do too.
type locatingDriver struct {
	*mockDriver
	element *core.ElementInfo
	err     error
}

func (d *locatingDriver) LocateElement(flow.Selector) (*core.ElementInfo, error) {
	return d.element, d.err
}

func TestAssertScreenshotMaskUsesARealLocator(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reference.png"), tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	base := screenshotRunner(dir, tenByTen(t, image.Pt(1, 1)))
	mock := base.driver.(*mockDriver)
	run := func(d core.Driver) *core.CommandResult {
		fr := &FlowRunner{driver: d, script: base.script, config: RunnerConfig{}}
		return fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
			Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}},
		})
	}
	located := &locatingDriver{mockDriver: mock, element: &core.ElementInfo{Bounds: core.Bounds{Width: 2, Height: 2}}}
	if result := run(located); !result.Success {
		t.Errorf("the locator's bounds should mask the difference: %s", result.Message)
	}
	empty := &locatingDriver{mockDriver: mock, element: &core.ElementInfo{}}
	if result := run(empty); result.Success || !strings.Contains(result.Message, "empty bounds") {
		t.Errorf("empty bounds must fail, not mask nothing: %+v", result)
	}
	gone := &locatingDriver{mockDriver: mock}
	if result := run(gone); result.Success {
		t.Error("no element must fail")
	}
	failed := &locatingDriver{mockDriver: mock, err: os.ErrNotExist}
	if result := run(failed); result.Success {
		t.Error("a failed locate must fail")
	}
}

func TestAssertScreenshotDiffsAScreenThatGrew(t *testing.T) {
	dir := t.TempDir()
	reference := filepath.Join(dir, "golden.png")
	if err := os.WriteFile(reference, tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 10, 14))
	for y := 0; y < 14; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, imagecolor.White)
		}
	}
	var grown bytes.Buffer
	if err := png.Encode(&grown, img); err != nil {
		t.Fatal(err)
	}

	result := screenshotRunner(dir, grown.Bytes()).executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path:                reference,
		RequireBaseline:     true,
		ThresholdPercentage: 99.9,
	})
	if result.Success {
		t.Fatal("a screen that grew must not match")
	}
	outcome, ok := result.Data.(core.ScreenshotOutcome)
	if !ok {
		t.Fatalf("data = %T", result.Data)
	}
	if outcome.DiffPath == "" || outcome.TotalPixels != 140 || outcome.DifferingPixels != 40 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err := os.Stat(outcome.DiffPath); err != nil {
		t.Fatalf("no diff written: %v", err)
	}

	// Without the outcome options the size mismatch is still the message.
	plain := screenshotRunner(dir, grown.Bytes()).executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path:                reference,
		ThresholdPercentage: 99.9,
	})
	if plain.Success || !strings.Contains(plain.Message, "size mismatch") {
		t.Fatalf("plain = %+v", plain)
	}
}
