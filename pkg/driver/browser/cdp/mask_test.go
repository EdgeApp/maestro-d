package cdp

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// maskTestPage is a page taller than the viewport, with a fixed banner and a
// clock below the fold, whose texts the test changes between captures.
func maskTestPage() string {
	return `<!DOCTYPE html>
<html><body style="margin:0;height:1100px;background:#fff">
<div id="banner" style="position:fixed;left:10px;top:10px;width:300px;height:30px;background:#cde">Banner one</div>
<h1 style="position:absolute;left:20px;top:300px">Stable heading</h1>
<div id="clock" style="position:absolute;left:100.5px;top:900.25px;width:200px;height:40px;background:#edc">10:00</div>
</body></html>`
}

// capturePage takes the screenshot assertScreenshot compares.
func capturePage(t *testing.T, d *Driver) []byte {
	t.Helper()
	res := d.Execute(&flow.AssertScreenshotStep{})
	data, ok := res.Data.([]byte)
	if !res.Success || !ok || len(data) == 0 {
		t.Fatalf("capture: %+v", res)
	}
	return data
}

// differingPixels masks both images at the located elements' bounds, as the
// executor's mask does, and counts the pixels that still differ.
func differingPixels(t *testing.T, d *Driver, a, b []byte, mask ...flow.Selector) int {
	t.Helper()
	bounds := make([]core.Bounds, 0, len(mask))
	for _, sel := range mask {
		element, err := d.LocateElement(sel)
		if err != nil {
			t.Fatalf("locate %s: %v", sel.Describe(), err)
		}
		bounds = append(bounds, element.Bounds)
	}
	width, height, err := core.ScreenshotSpace(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) > 0 {
		if a, err = core.MaskImage(a, bounds, width, height); err != nil {
			t.Fatal(err)
		}
		if b, err = core.MaskImage(b, bounds, width, height); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := core.CompareImages(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return stats.DifferingPixels
}

// A mask on the web driver finds its element, with bounds in the whole-page
// screenshot's space: elements that change between captures, one fixed and
// one below the fold of a scrolled page, are left out and the rest matches.
func TestLocateElementMasksAWebScreenshot(t *testing.T) {
	ts := newIframeTestServer(maskTestPage())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	if _, err := d.page.Eval(`() => window.scrollTo(0, 300)`); err != nil {
		t.Fatal(err)
	}

	before := capturePage(t, d)
	if _, err := d.page.Eval(`() => {
		document.getElementById('banner').textContent = 'Banner two, longer';
		document.getElementById('clock').textContent = '11:59';
	}`); err != nil {
		t.Fatal(err)
	}
	after := capturePage(t, d)

	banner, clock := flow.Selector{ID: "banner"}, flow.Selector{CSS: "#clock"}
	if n := differingPixels(t, d, before, after); n == 0 {
		t.Fatal("the captures should differ unmasked")
	}
	if n := differingPixels(t, d, before, after, banner); n == 0 {
		t.Error("masking the banner alone should leave the clock's change")
	}
	if n := differingPixels(t, d, before, after, clock); n == 0 {
		t.Error("masking the clock alone should leave the banner's change")
	}
	if n := differingPixels(t, d, before, after, banner, clock); n != 0 {
		t.Errorf("with both masked, %d pixels still differ", n)
	}

	// The visibility check's fast path reports no element for this selector,
	// which is why mask needs the locator; its bounds cover the clock's
	// fractional edges in page coordinates, wherever the page is scrolled.
	element, err := d.LocateElement(clock)
	if err != nil {
		t.Fatal(err)
	}
	if want := (core.Bounds{X: 100, Y: 900, Width: 201, Height: 41}); element.Bounds != want {
		t.Errorf("clock bounds %+v, want %+v", element.Bounds, want)
	}
	if element.Text != "11:59" {
		t.Errorf("clock text %q", element.Text)
	}
}

// LocateElement fails for a selector that matches nothing, and leaves the
// viewport as it found it.
func TestLocateElementMissing(t *testing.T) {
	ts := newIframeTestServer(maskTestPage())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	d.SetFindTimeout(300)

	if _, err := d.LocateElement(flow.Selector{ID: "absent"}); err == nil {
		t.Error("a selector that matches nothing should fail")
	}
	if _, err := d.LocateElement(flow.Selector{ID: "banner"}); err != nil {
		t.Fatal(err)
	}
	size, err := d.page.Eval(`() => [window.innerWidth, window.innerHeight]`)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := size.Value.Arr()[0].Int(), size.Value.Arr()[1].Int(); w != 1024 || h != 768 {
		t.Errorf("viewport left at %dx%d, want 1024x768", w, h)
	}
}
