package cdp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// topFramePageWithOverlay is a top-frame button under a fixed overlay that
// never moves, which Rod's Click would wait on forever.
func topFramePageWithOverlay() string {
	return `<!DOCTYPE html>
<html><body>
<button id="b" style="position:absolute;left:40px;top:40px;width:120px;height:40px">Delete</button>
<div id="overlay" style="position:fixed;left:0;top:0;width:100%;height:100%;background:rgba(0,0,0,0.5);z-index:10">Modal</div>
<script>
document.getElementById("b").addEventListener("click", function() { document.title = "deleted"; });
</script>
</body></html>`
}

func topFramePageWithButton() string {
	return `<!DOCTYPE html>
<html><body>
<button id="b" style="position:absolute;left:40px;top:40px;width:120px;height:40px">Save</button>
<script>
document.getElementById("b").addEventListener("click", function() { document.title = "saved"; });
</script>
</body></html>`
}

// topFramePageCountingClicks is a button whose clicks are counted in the title.
func topFramePageCountingClicks() string {
	return `<!DOCTYPE html>
<html><body>
<button id="b" style="position:absolute;left:40px;top:40px;width:120px;height:40px">Submit</button>
<script>
var clicks = 0;
document.getElementById("b").addEventListener("click", function() { clicks++; document.title = "clicks:" + clicks; });
</script>
</body></html>`
}

// stubPressAndRelease stands in for a native click's press and release for one test.
func stubPressAndRelease(t *testing.T, press func(*rod.Mouse) error) {
	t.Helper()
	prev := pressAndRelease
	pressAndRelease = press
	t.Cleanup(func() { pressAndRelease = prev })
}

// A press whose call fails after Chrome received it (here, the click goes
// through and then the call reports a timeout) fails the tap with that error.
// It is never followed by a JS click, which would activate the element twice.
func TestTapOn_FailedPressIsNotClickedAgain(t *testing.T) {
	ts := newIframeTestServer(topFramePageCountingClicks())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	stubPressAndRelease(t, func(m *rod.Mouse) error {
		if err := m.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return err
		}
		return context.DeadlineExceeded
	})

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	if res.Success {
		t.Fatalf("a press that failed must fail the tap, got %q", res.Message)
	}
	if !strings.Contains(res.Message, "sent but did not complete") || strings.Contains(res.Message, "JS click") {
		t.Errorf("the tap should report the press error, got %q", res.Message)
	}
	if got := pageTitle(t, d); got != "clicks:1" {
		t.Errorf("the button should be clicked once, title %q", got)
	}
}

// The bound covers only the waits: a press and release that run past it
// still complete, and the click counts once.
func TestNativeClickBoundsOnlyTheWaits(t *testing.T) {
	ts := newIframeTestServer(topFramePageCountingClicks())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	const bound = 100 * time.Millisecond
	stubPressAndRelease(t, func(m *rod.Mouse) error {
		time.Sleep(3 * bound)
		return m.Click(proto.InputMouseButtonLeft, 1)
	})
	elem, err := d.page.Element("#b")
	if err != nil {
		t.Fatal(err)
	}
	if err := nativeClick(elem, bound); err != nil {
		t.Fatalf("a press after the bound should still complete: %v", err)
	}
	if got := pageTitle(t, d); got != "clicks:1" {
		t.Errorf("the button should be clicked once, title %q", got)
	}
}

// stubNativeClick stands in for the bounded native click for one test.
func stubNativeClick(t *testing.T, click func(*rod.Element, time.Duration) error) {
	t.Helper()
	prev := nativeClick
	nativeClick = click
	t.Cleanup(func() { nativeClick = prev })
}

// A covered top-frame element fails the tap with the iframe path's occlusion
// error, within about the click's bound, and its handler never runs.
func TestTapOn_CoveredElementFailsAsOccluded(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithOverlay())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()

	start := time.Now()
	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	elapsed := time.Since(start)
	if res.Success {
		t.Fatalf("a tap on a covered button must fail, got %q", res.Message)
	}
	if !strings.Contains(res.Message, "blocked by overlay") || !strings.Contains(res.Message, "overlay") {
		t.Errorf("expected the iframe path's occlusion error, got %q", res.Message)
	}
	if elapsed > defaultClickTimeout+3*time.Second {
		t.Errorf("the tap took %v, past its %v bound", elapsed, defaultClickTimeout)
	}
	if got := pageTitle(t, d); got == "deleted" {
		t.Error("the covered button's click handler ran")
	}
}

// An id: selector (whose finder returns an element with a sleeper that gives
// up at once) waits, like a text: one, for an overlay that is leaving: the tap
// lands natively once it has gone, not as blocked on the first covered check.
func TestTapOn_WaitsForALeavingOverlay(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithOverlay())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	if _, err := d.page.Eval(`() => setTimeout(() => document.getElementById("overlay").remove(), 500)`); err != nil {
		t.Fatal(err)
	}

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	if !res.Success {
		t.Fatalf("a tap behind an overlay that leaves should land: %s", res.Message)
	}
	if strings.Contains(res.Message, "JS click") {
		t.Errorf("the tap should be the native click, got %q", res.Message)
	}
	if got := pageTitle(t, d); got != "deleted" {
		t.Errorf("the button's click handler should have run, title %q", got)
	}
}

// A native click that runs to its timeout on a covered element (Rod's wait
// with its default sleeper) fails as occluded too, not with a JS click.
func TestTapOn_TimedOutClickOnACoveredElementFailsAsOccluded(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithOverlay())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	stubNativeClick(t, func(*rod.Element, time.Duration) error { return context.DeadlineExceeded })

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	if res.Success || !strings.Contains(res.Message, "blocked by overlay") {
		t.Fatalf("expected the occlusion error, got %+v", res)
	}
	if got := pageTitle(t, d); got == "deleted" {
		t.Error("the covered button's click handler ran")
	}
}

// A native click that times out on an element in the clear falls back to a
// JS click, and the result says so.
func TestTapOn_TimedOutClickInTheClearFallsBack(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithButton())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	stubNativeClick(t, func(*rod.Element, time.Duration) error { return context.DeadlineExceeded })

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	if !res.Success {
		t.Fatalf("an uncovered element should fall back to a JS click: %s", res.Message)
	}
	if !strings.Contains(res.Message, "JS click") {
		t.Errorf("the result should report the fallback, got %q", res.Message)
	}
	if got := pageTitle(t, d); got != "saved" {
		t.Errorf("the JS click should have run the handler, title %q", got)
	}
}

// Any other native click error keeps the JS fallback it always had.
func TestTapOn_OtherClickErrorFallsBack(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithButton())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	stubNativeClick(t, func(*rod.Element, time.Duration) error { return errors.New("no input events") })

	res := d.Execute(&flow.TapOnStep{Selector: flow.Selector{ID: "b"}})
	if !res.Success || !strings.Contains(res.Message, "JS click") {
		t.Fatalf("got %+v", res)
	}
	if got := pageTitle(t, d); got != "saved" {
		t.Errorf("title %q", got)
	}
}

// The native click is bounded: an element whose sleeper retries (Rod's
// default), covered by something that never moves, gives up at the bound
// with a timeout instead of holding the command forever.
func TestNativeClickIsBounded(t *testing.T) {
	ts := newIframeTestServer(topFramePageWithOverlay())
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()
	elem, err := d.page.Element("#b")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = nativeClick(elem, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a click on a covered element should time out, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the click held on for %v past its bound", elapsed)
	}
	if blocker := coveringElement(elem); !strings.Contains(blocker, "overlay") {
		t.Errorf("the overlay should be named as the blocker, got %q", blocker)
	}
}

// notFoundAs reports itself as Rod's not-found through As alone, the way a
// wrapper of a not-found with fields of its own would.
type notFoundAs struct{}

func (notFoundAs) Error() string { return "element not found (wrapped)" }

func (notFoundAs) As(target any) bool {
	if p, ok := target.(**rod.ElementNotFoundError); ok {
		*p = &rod.ElementNotFoundError{}
		return true
	}
	return false
}

// The not-found is matched by type, not by comparing pointers to Rod's empty
// struct, which only happen to be equal in gc.
func TestClickWaitGaveUp(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", context.DeadlineExceeded, true},
		{"wrapped timeout", fmt.Errorf("click: %w", context.DeadlineExceeded), true},
		{"not found", &rod.ElementNotFoundError{}, true},
		{"wrapped not found", fmt.Errorf("click: %w", &rod.ElementNotFoundError{}), true},
		{"not found through As", notFoundAs{}, true},
		{"other", errors.New("no input events"), false},
	}
	for _, c := range cases {
		if got := clickWaitGaveUp(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
