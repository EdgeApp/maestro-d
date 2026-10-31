package executor

import (
	"bytes"
	"errors"
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

// sizedScreenshotDriver is a mock whose screenshots cover an area other than
// its screen, as a web page's do.
type sizedScreenshotDriver struct {
	*mockDriver
	width, height int
	err           error
}

func (d *sizedScreenshotDriver) ScreenshotSize() (int, int, error) { return d.width, d.height, d.err }

// A driver that reports its screenshots' size has its mask bounds scaled by
// that size, not its screen's.
func TestAssertScreenshotMaskUsesScreenshotSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reference.png"), tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	// The mask's 2x2 bounds cover pixels 0-3 of the 10x10 image in the 5x5
	// screen's space, but only pixels 0-1 in a 10x10 screenshot space.
	fr := screenshotRunner(dir, tenByTen(t, image.Pt(3, 3)))
	step := &flow.AssertScreenshotStep{Path: "reference", ThresholdPercentage: 100, Mask: []flow.Selector{{ID: "found"}}}
	if result := fr.executeAssertScreenshot(step); !result.Success {
		t.Fatalf("in the screen's space the mask covers the difference: %s", result.Message)
	}

	sized := &sizedScreenshotDriver{mockDriver: fr.driver.(*mockDriver), width: 10, height: 10}
	fr.driver = sized
	if result := fr.executeAssertScreenshot(step); result.Success {
		t.Error("in the screenshot's space the mask should leave the difference")
	}

	sized.err = errors.New("no layout metrics")
	if result := fr.executeAssertScreenshot(step); result.Success || !strings.Contains(result.Message, "no layout metrics") {
		t.Errorf("a screenshot size error should fail the mask: %+v", result)
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

// A step that wrote its reference rather than comparing says so in its code, so
// a caller reading data cannot take it for a comparison that passed at 0%.
func TestAssertScreenshotBaselineWritesHaveCodes(t *testing.T) {
	dir := t.TempDir()
	fr := screenshotRunner(dir, tenByTen(t))
	step := func() *flow.AssertScreenshotStep {
		return &flow.AssertScreenshotStep{Path: "baseline", ThresholdPercentage: 100, CaptureOut: "capture"}
	}
	code := func(name string, result *core.CommandResult) string {
		t.Helper()
		if !result.Success {
			t.Fatalf("%s: %s", name, result.Message)
		}
		outcome, ok := result.Data.(core.ScreenshotOutcome)
		if !ok {
			t.Fatalf("%s: data = %T", name, result.Data)
		}
		return outcome.Code
	}

	if got := code("created", fr.executeAssertScreenshot(step())); got != core.BaselineCreatedCode {
		t.Errorf("a written missing reference: code %q, want %q", got, core.BaselineCreatedCode)
	}
	if got := code("compared", fr.executeAssertScreenshot(step())); got != "" {
		t.Errorf("a comparison that passed: code %q, want none", got)
	}
	fr.config.UpdateScreenshots = true
	if got := code("updated", fr.executeAssertScreenshot(step())); got != core.BaselineUpdatedCode {
		t.Errorf("a rewritten reference: code %q, want %q", got, core.BaselineUpdatedCode)
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

// With captureOut, every exit carries the outcome and its capturePath, not the
// raw PNG: a seeded or updated baseline as well as each kind of failure.
func TestAssertScreenshotOutcomeOnEveryExit(t *testing.T) {
	dir := t.TempDir()
	fr := screenshotRunner(dir, tenByTen(t))
	wantOutcome := func(name string, result *core.CommandResult, success bool) {
		t.Helper()
		if result.Success != success {
			t.Errorf("%s: success = %v, want %v (%s)", name, result.Success, success, result.Message)
		}
		outcome, ok := result.Data.(core.ScreenshotOutcome)
		if !ok {
			t.Errorf("%s: data = %T, want the outcome", name, result.Data)
			return
		}
		if outcome.CapturePath != filepath.Join(dir, "capture.png") {
			t.Errorf("%s: capturePath = %q", name, outcome.CapturePath)
		}
	}
	step := func() *flow.AssertScreenshotStep {
		return &flow.AssertScreenshotStep{Path: "seed", ThresholdPercentage: 100, CaptureOut: "capture"}
	}

	wantOutcome("created", fr.executeAssertScreenshot(step()), true)

	fr.config.UpdateScreenshots = true
	wantOutcome("updated", fr.executeAssertScreenshot(step()), true)
	fr.config.UpdateScreenshots = false

	masked := step()
	masked.Mask = []flow.Selector{{ID: "absent"}}
	wantOutcome("mask error", fr.executeAssertScreenshot(masked), false)

	cropped := step()
	cropped.Mask = []flow.Selector{{ID: "found"}}
	cropped.CropOn = &flow.Selector{ID: "found"}
	wantOutcome("mask with cropOn", fr.executeAssertScreenshot(cropped), false)

	if err := os.WriteFile(filepath.Join(dir, "seed.png"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantOutcome("compare error", fr.executeAssertScreenshot(step()), false)

	if err := os.Remove(filepath.Join(dir, "seed.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "seed.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantOutcome("reference read error", fr.executeAssertScreenshot(step()), false)

	escaped := step()
	escaped.Path = "../../../../../../escape"
	wantOutcome("reference outside the workspace", fr.executeAssertScreenshot(escaped), false)
}

// A suite that always sets requireBaseline records a new baseline with
// --update-screenshots rather than failing BASELINE_MISSING.
func TestAssertScreenshotRequireBaselineWithUpdateScreenshots(t *testing.T) {
	dir := t.TempDir()
	fr := screenshotRunner(dir, tenByTen(t))
	fr.config.UpdateScreenshots = true
	result := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "fresh", ThresholdPercentage: 100, RequireBaseline: true,
	})
	if !result.Success {
		t.Fatalf("--update-screenshots should write the missing baseline: %s", result.Message)
	}
	if outcome, ok := result.Data.(core.ScreenshotOutcome); !ok || outcome.Code != core.BaselineCreatedCode {
		t.Errorf("data = %#v", result.Data)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.png")); err != nil {
		t.Errorf("the baseline should be written: %v", err)
	}
}

// Padding a screen that changed size only produces the diff and the numbers:
// the step fails even when the padded match clears the threshold, as it does
// without captureOut or requireBaseline.
func TestAssertScreenshotSizeMismatchAlwaysFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "golden.png"), tenByTen(t), 0o644); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 10, 11))
	for y := 0; y < 11; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, imagecolor.White)
		}
	}
	var grown bytes.Buffer
	if err := png.Encode(&grown, img); err != nil {
		t.Fatal(err)
	}
	for _, step := range []*flow.AssertScreenshotStep{
		{Path: "golden", ThresholdPercentage: 80, CaptureOut: "capture"},
		{Path: "golden", ThresholdPercentage: 80, RequireBaseline: true},
	} {
		result := screenshotRunner(dir, grown.Bytes()).executeAssertScreenshot(step)
		if result.Success {
			t.Fatalf("a padded match above the threshold must still fail: %s", result.Message)
		}
		if !strings.Contains(result.Message, "size mismatch") || !strings.Contains(result.Message, "10x10") {
			t.Errorf("message = %q", result.Message)
		}
		outcome, ok := result.Data.(core.ScreenshotOutcome)
		if !ok || outcome.Code != core.SizeMismatchCode {
			t.Fatalf("data = %#v", result.Data)
		}
		if outcome.TotalPixels != 110 || outcome.DifferingPixels != 10 || outcome.DiffPath == "" {
			t.Errorf("outcome = %+v", outcome)
		}
	}

	// With cropOn nothing is padded, and the code still names the mismatch.
	cropped := screenshotRunner(dir, grown.Bytes()).executeAssertScreenshot(&flow.AssertScreenshotStep{
		Path: "golden", ThresholdPercentage: 80, RequireBaseline: true, CropOn: &flow.Selector{ID: "found"},
	})
	if outcome, ok := cropped.Data.(core.ScreenshotOutcome); cropped.Success || !ok || outcome.Code != core.SizeMismatchCode {
		t.Errorf("cropped = %+v", cropped)
	}
}

// A captureOut that names the step's own reference (or its diff) is refused
// before anything is written: the live frame would overwrite the baseline and
// then be compared with itself, a 100% match.
func TestAssertScreenshotCaptureOutCannotBeTheReference(t *testing.T) {
	dir := t.TempDir()
	referenceBytes := tenByTen(t)
	reference := filepath.Join(dir, "reference.png")
	if err := os.WriteFile(reference, referenceBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(reference, filepath.Join(dir, "alias.png")); err != nil {
		t.Fatal(err)
	}
	// The live screen differs from the baseline in one pixel.
	fr := screenshotRunner(dir, tenByTen(t, image.Pt(4, 4)))
	for _, captureOut := range []string{"reference", "reference.png", "./reference", "alias", "reference_diff"} {
		result := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
			Path: "reference", ThresholdPercentage: 100, CaptureOut: captureOut,
		})
		if result.Success {
			t.Errorf("captureOut %q: a capture over the step's own files must fail, got %q", captureOut, result.Message)
		}
		if !strings.Contains(result.Message, "captureOut") {
			t.Errorf("captureOut %q: the error should name captureOut, got %q", captureOut, result.Message)
		}
		if got, err := os.ReadFile(reference); err != nil || !bytes.Equal(got, referenceBytes) {
			t.Fatalf("captureOut %q overwrote the baseline", captureOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "reference_diff.png")); !os.IsNotExist(err) {
		t.Errorf("a refused capture should not reach the comparison or its diff: %v", err)
	}
}

// caseFolds reports whether dir's file system folds case.
func caseFolds(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casefold-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "CASEFOLD-PROBE"))
	return err == nil
}

// A captureOut that names the reference only on this file system, when there
// is no baseline yet, is refused too and leaves the baseline missing: the
// capture is never read back as the reference and compared with itself, and
// is not left behind as an unreviewed baseline.
func TestAssertScreenshotCaptureOutCannotBecomeAMissingReference(t *testing.T) {
	cases := []struct{ name, path, captureOut string }{
		// Through a linked directory, on any file system.
		{"linked directory", "real/home", "link/home"},
		// By case alone, on a file system that folds case (macOS's default).
		{"case", "screens/home", "screens/Home"},
	}
	for _, c := range cases {
		for _, requireBaseline := range []bool{false, true} {
			dir := t.TempDir()
			if c.name == "case" && !caseFolds(t, dir) {
				t.Logf("%s: the file system does not fold case", c.name)
				continue
			}
			if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			fr := screenshotRunner(dir, tenByTen(t))
			result := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{
				Path: c.path, ThresholdPercentage: 100, CaptureOut: c.captureOut, RequireBaseline: requireBaseline,
			})
			if result.Success {
				t.Errorf("%s, requireBaseline %v: a capture that is the reference must fail, got %q", c.name, requireBaseline, result.Message)
			}
			if !strings.Contains(result.Message, "captureOut") {
				t.Errorf("%s, requireBaseline %v: the error should name captureOut, got %q", c.name, requireBaseline, result.Message)
			}
			if _, err := os.Stat(filepath.Join(dir, c.path+".png")); !os.IsNotExist(err) {
				t.Errorf("%s, requireBaseline %v: the baseline should still be missing: %v", c.name, requireBaseline, err)
			}
		}
	}
}

// The capture's own failures, before any frame is in hand, still return the
// outcome once a caller opted into it.
func TestAssertScreenshotOutcomeOnCaptureFailure(t *testing.T) {
	dir := t.TempDir()
	for name, answer := range map[string]*core.CommandResult{
		"failed capture": {Success: false, Message: "screenshot failed", Data: "driver data"},
		"empty capture":  {Success: true, Data: []byte{}},
	} {
		fr := screenshotRunner(dir, nil)
		fr.driver.(*mockDriver).executeFunc = func(flow.Step) *core.CommandResult { return answer }
		for _, step := range []*flow.AssertScreenshotStep{
			{Path: "reference", RequireBaseline: true},
			{Path: "reference", CaptureOut: "capture"},
		} {
			result := fr.executeAssertScreenshot(step)
			if result.Success {
				t.Errorf("%s: must fail", name)
			}
			if _, ok := result.Data.(core.ScreenshotOutcome); !ok {
				t.Errorf("%s: data = %#v, want the outcome", name, result.Data)
			}
		}
		if !answer.Success && answer.Data != "driver data" {
			t.Errorf("%s: the driver's own result was changed: %#v", name, answer.Data)
		}
		plain := fr.executeAssertScreenshot(&flow.AssertScreenshotStep{Path: "reference"})
		if _, ok := plain.Data.(core.ScreenshotOutcome); ok || plain.Success {
			t.Errorf("%s: without the options the result is unchanged: %+v", name, plain)
		}
	}
}
