package cdp

import (
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
)

// topFramePageWithPlayer is a video under its player's controls, far enough
// down the page to need scrolling: a click on the controls is counted in the
// title, one on the video itself marks it.
func topFramePageWithPlayer() string {
	return `<!DOCTYPE html>
<html><body style="height:3000px">
<div style="position:absolute;left:40px;top:1500px;width:320px;height:180px">
  <video id="v" style="position:absolute;left:0;top:0;width:320px;height:180px;background:#000"></video>
  <div id="controls" style="position:absolute;left:0;top:0;width:320px;height:180px;z-index:2"></div>
</div>
<script>
var hits = 0;
document.getElementById("controls").addEventListener("click", function() { hits++; document.title = "controls:" + hits; });
document.getElementById("v").addEventListener("click", function() { document.title = "video"; });
</script>
</body></html>`
}

// allowCovered taps the covered element's centre: the click reaches the
// controls on top, once, as a finger's would. Without it the tap is refused.
func TestTapOn_AllowCoveredTapsTheCentre(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithPlayer())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "v"}, AllowCovered: true})
	if !res.Success || !strings.Contains(res.Message, "centre") {
		t.Fatalf("allowCovered tap: %+v", res)
	}
	if got := pageTitle(t, d); got != "controls:1" {
		t.Fatalf("title %q, want the controls clicked once", got)
	}
	if res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "v"}}); res.Success || !strings.Contains(res.Message, "blocked by overlay") {
		t.Fatalf("default tap on the covered video: %+v", res)
	}
}

// A press that fails fails the allowCovered tap.
func TestTapOn_AllowCoveredPressFails(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithPlayer())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	stubPressAndRelease(t, func(*rod.Mouse) error { return errors.New("target closed") })
	if res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "v"}, AllowCovered: true}); res.Success || !strings.Contains(res.Message, "target closed") {
		t.Fatalf("got %+v", res)
	}
}
