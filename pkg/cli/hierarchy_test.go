package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

const androidXML = `<?xml version="1.0"?>
<hierarchy rotation="0">
  <node class="android.widget.FrameLayout" resource-id="" text="" bounds="[0,0][1080,2400]">
    <node class="android.widget.Button" resource-id="com.app:id/login" text="Sign In" bounds="[100,200][300,260]"/>
  </node>
</hierarchy>`

const iosXML = `<?xml version="1.0"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" x="0" y="0" width="390" height="844">
  <XCUIElementTypeButton type="XCUIElementTypeButton" name="loginBtn" label="Sign In" x="50" y="100" width="290" height="50"/>
</XCUIElementTypeApplication>`

const flatJSON = `{"nodes":[
  {"index":0,"type":"Application","identifier":"","label":"","rect":{"x":0,"y":0,"width":390,"height":844},"parentIndex":null},
  {"index":1,"type":"Button","identifier":"loginBtn","label":"Sign In","rect":{"x":50,"y":100,"width":290,"height":50},"parentIndex":0}
]}`

const treeJSON = `{"type":"View","children":[{"type":"Button","id":"login","text":"Sign In"}]}`

func TestFormatHierarchy_Android(t *testing.T) {
	out, err := formatHierarchy([]byte(androidXML), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type": "Button"`, `"id": "com.app:id/login"`, `"text": "Sign In"`, `"width": 200`, `"height": 60`} {
		if !strings.Contains(out, want) {
			t.Errorf("android JSON missing %q\n%s", want, out)
		}
	}
}

func TestFormatHierarchy_IOS(t *testing.T) {
	out, err := formatHierarchy([]byte(iosXML), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type": "Button"`, `"id": "loginBtn"`, `"text": "Sign In"`, `"width": 290`} {
		if !strings.Contains(out, want) {
			t.Errorf("ios JSON missing %q\n%s", want, out)
		}
	}
}

func TestFormatHierarchy_FlatJSON(t *testing.T) {
	out, err := formatHierarchy([]byte(flatJSON), false, "")
	if err != nil {
		t.Fatal(err)
	}
	// The Button (parentIndex 0) must be nested under the Application root.
	if !strings.Contains(out, `"id": "loginBtn"`) || !strings.Contains(out, `"children"`) {
		t.Errorf("flat JSON not normalized into a tree:\n%s", out)
	}
}

func TestFormatHierarchy_Compact(t *testing.T) {
	out, err := formatHierarchy([]byte(treeJSON), true, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "{") {
		t.Errorf("compact should be flat, not JSON:\n%s", out)
	}
	if !strings.Contains(out, "Button") || !strings.Contains(out, "id=login") || !strings.Contains(out, `text="Sign In"`) {
		t.Errorf("compact missing expected fields:\n%s", out)
	}
	// The child Button must be indented under View.
	if !strings.Contains(out, "\n  Button") {
		t.Errorf("compact should indent children:\n%s", out)
	}
}

const androidStatesXML = `<?xml version="1.0"?>
<hierarchy>
  <node class="android.widget.Button" resource-id="submit" text="Submit" enabled="false" checkable="false" bounds="[0,0][100,50]"/>
  <node class="android.widget.CheckBox" resource-id="agree" text="Agree" enabled="true" checkable="true" checked="true" bounds="[0,60][100,110]"/>
  <node class="android.widget.EditText" resource-id="name" text="" enabled="true" focused="true" bounds="[0,120][100,170]"/>
</hierarchy>`

func TestFormatHierarchy_States(t *testing.T) {
	// JSON: disabled button, checked checkbox, focused field.
	out, err := formatHierarchy([]byte(androidStatesXML), false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enabled": false`, `"checked": true`, `"focused": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("states JSON missing %q\n%s", want, out)
		}
	}
	// An enabled, non-checkable element must not emit noise fields.
	if strings.Contains(out, `"checked": false`) && !strings.Contains(out, "agree") {
		t.Errorf("unexpected checked:false on non-checkable element")
	}

	// Compact: state flags appear as tags.
	comp, _ := formatHierarchy([]byte(androidStatesXML), true, "")
	for _, want := range []string{"[disabled]", "[checked]", "[focused]"} {
		if !strings.Contains(comp, want) {
			t.Errorf("compact missing state tag %q\n%s", want, comp)
		}
	}
}

func TestFormatHierarchy_Find(t *testing.T) {
	out, err := formatHierarchy([]byte(androidXML), false, "sign in")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Button") || !strings.Contains(out, "Sign In") {
		t.Errorf("find should surface the matching Button:\n%s", out)
	}
	// The non-matching FrameLayout should be filtered out.
	if strings.Contains(out, "FrameLayout") {
		t.Errorf("find should exclude non-matching elements:\n%s", out)
	}

	none, _ := formatHierarchy([]byte(androidXML), false, "nonexistent-xyz")
	if !strings.Contains(none, "no elements matching") {
		t.Errorf("find with no match should report it, got:\n%s", none)
	}
}

// fakeScreenshotDriver is a core.Driver stub for the screenshot side-channel of
// the hierarchy command; only Screenshot() is exercised.
type fakeScreenshotDriver struct {
	core.Driver
	data []byte
	err  error
}

func (f *fakeScreenshotDriver) Screenshot() ([]byte, error) { return f.data, f.err }

func TestCaptureHierarchyScreenshot(t *testing.T) {
	t.Run("writes the image and creates parent directories", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "shot.png")
		d := &fakeScreenshotDriver{data: []byte("\x89PNG fake")}

		if err := captureHierarchyScreenshot(d, path); err != nil {
			t.Fatalf("captureHierarchyScreenshot() error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read written screenshot: %v", err)
		}
		if string(got) != "\x89PNG fake" {
			t.Errorf("wrote %q, want the driver's bytes", got)
		}
	})

	t.Run("reports a driver failure", func(t *testing.T) {
		d := &fakeScreenshotDriver{err: errors.New("device gone")}
		if err := captureHierarchyScreenshot(d, filepath.Join(t.TempDir(), "s.png")); err == nil {
			t.Error("expected the driver error to surface")
		}
	})

	// An empty capture would otherwise write a 0-byte PNG that looks like a
	// successful screenshot until someone opens it.
	t.Run("rejects an empty capture", func(t *testing.T) {
		d := &fakeScreenshotDriver{data: nil}
		path := filepath.Join(t.TempDir(), "s.png")
		if err := captureHierarchyScreenshot(d, path); err == nil {
			t.Error("expected an error when the driver returned no image data")
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Error("no file should be written when the capture was empty")
		}
	})
}

// collect walks a normalized tree in document order.
func collect(n hNode, out *[]hNode) {
	*out = append(*out, n)
	for _, c := range n.Children {
		collect(c, out)
	}
}

func findByID(t *testing.T, root hNode, id string) hNode {
	t.Helper()
	var all []hNode
	collect(root, &all)
	for _, n := range all {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node with id %q", id)
	return hNode{}
}

// Every driver reports enabled on every element, true or false, so a
// physical phone's tree carries enabled:true the way the simulator's does.
func TestParseHierarchy_EnabledOnEveryElement(t *testing.T) {
	const wdaXML = `<?xml version="1.0"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" enabled="true" x="0" y="0" width="390" height="844">
  <XCUIElementTypeButton type="XCUIElementTypeButton" name="send" label="Send" enabled="true" x="0" y="0" width="44" height="44"/>
  <XCUIElementTypeButton type="XCUIElementTypeButton" name="copy" label="Copy" enabled="false" x="50" y="0" width="44" height="44"/>
</XCUIElementTypeApplication>`
	const flat = `{"nodes":[
  {"index":0,"type":"Application","identifier":"App","rect":{"x":0,"y":0,"width":390,"height":844},"enabled":true},
  {"index":1,"type":"Button","identifier":"send","label":"Send","rect":{"x":0,"y":0,"width":44,"height":44},"enabled":true,"parentIndex":0},
  {"index":2,"type":"Button","identifier":"copy","label":"Copy","rect":{"x":50,"y":0,"width":44,"height":44},"enabled":false,"parentIndex":0}
]}`
	cases := []struct {
		name, raw, on, off string
	}{
		{"android", androidStatesXML, "agree", "submit"},
		{"wda", wdaXML, "send", "copy"},
		{"devicelab flat JSON", flat, "send", "copy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, err := parseHierarchy([]byte(c.raw))
			if err != nil {
				t.Fatal(err)
			}
			on := findByID(t, root, c.on)
			if on.Enabled == nil || !*on.Enabled {
				t.Errorf("%s: enabled = %v, want true", c.on, on.Enabled)
			}
			off := findByID(t, root, c.off)
			if off.Enabled == nil || *off.Enabled {
				t.Errorf("%s: enabled = %v, want false", c.off, off.Enabled)
			}
		})
	}
}

// A node with no enabled attribute stays silent rather than claiming a state.
func TestParseHierarchy_EnabledAbsentStaysAbsent(t *testing.T) {
	for _, raw := range []string{androidXML, iosXML, flatJSON} {
		root, err := parseHierarchy([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		var all []hNode
		collect(root, &all)
		for _, n := range all {
			if n.Enabled != nil {
				t.Errorf("node %q: enabled = %v, want absent", n.ID, *n.Enabled)
			}
		}
	}
}

// WebDriverAgent lists a React Native Text nested in a Text as a StaticText
// inside a StaticText with the same label and frame. The normalized tree lists
// it once, as the devicelab runner's simulator snapshot does, and keeps the
// inner element's children.
func TestParseHierarchy_WDANestedDuplicateListedOnce(t *testing.T) {
	const raw = `<?xml version="1.0"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" label="App" enabled="true" x="0" y="0" width="390" height="844">
  <XCUIElementTypeOther type="XCUIElementTypeOther" name="chat-bubble-1" enabled="true" x="48" y="400" width="281" height="96">
    <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" label="Hello there" enabled="true" x="60" y="410" width="200" height="40">
      <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" label="Hello there" enabled="true" x="60" y="410" width="200" height="40">
        <XCUIElementTypeLink type="XCUIElementTypeLink" label="there" enabled="true" x="100" y="410" width="40" height="20"/>
      </XCUIElementTypeStaticText>
    </XCUIElementTypeStaticText>
    <XCUIElementTypeStaticText type="XCUIElementTypeStaticText" label="Hello there" enabled="true" x="60" y="460" width="200" height="40"/>
  </XCUIElementTypeOther>
</XCUIElementTypeApplication>`
	root, err := parseHierarchy([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	bubble := findByID(t, root, "chat-bubble-1")
	if len(bubble.Children) != 2 {
		t.Fatalf("bubble children = %d, want 2 (the outer text and the text below it)\n%+v", len(bubble.Children), bubble.Children)
	}
	outer := bubble.Children[0]
	if outer.Type != "StaticText" || outer.Text != "Hello there" || outer.Bounds.Y != 410 {
		t.Errorf("first child = %+v, want the outer StaticText at y=410", outer)
	}
	if len(outer.Children) != 1 || outer.Children[0].Type != "Link" {
		t.Errorf("outer StaticText children = %+v, want the inner text's Link moved up", outer.Children)
	}
	// Same words at another origin are a different element and stay.
	if below := bubble.Children[1]; below.Type != "StaticText" || below.Bounds.Y != 460 {
		t.Errorf("second child = %+v, want the StaticText at y=460", below)
	}

	var all []hNode
	collect(root, &all)
	count := 0
	for _, n := range all {
		if n.Type == "StaticText" && n.Text == "Hello there" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("StaticText %q listed %d times, want 2 (one per origin)", "Hello there", count)
	}
}

// Elements that differ in type, label, identifier or origin are all kept,
// and Android trees are never de-duplicated.
func TestParseHierarchy_DistinctElementsKept(t *testing.T) {
	const raw = `<?xml version="1.0"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" x="0" y="0" width="390" height="844">
  <XCUIElementTypeOther type="XCUIElementTypeOther" name="row" label="Send" x="10" y="10" width="100" height="40">
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="row" label="Send" x="10" y="10" width="100" height="40"/>
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="other" label="Send" x="10" y="10" width="100" height="40"/>
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="row" label="Sent" x="10" y="10" width="100" height="40"/>
  </XCUIElementTypeOther>
</XCUIElementTypeApplication>`
	root, err := parseHierarchy([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var all []hNode
	collect(root, &all)
	if len(all) != 5 {
		t.Errorf("node count = %d, want 5 (nothing dropped)", len(all))
	}

	const android = `<?xml version="1.0"?>
<hierarchy>
  <node class="android.widget.TextView" resource-id="t" text="Hi" bounds="[0,0][10,10]">
    <node class="android.widget.TextView" resource-id="t" text="Hi" bounds="[0,0][10,10]"/>
  </node>
</hierarchy>`
	aroot, err := parseHierarchy([]byte(android))
	if err != nil {
		t.Fatal(err)
	}
	if len(aroot.Children) != 1 {
		t.Errorf("android nested TextView dropped: %+v", aroot)
	}
}

// XCUITest frames are often fractional. A simulator snapshot with one must
// still normalize, or the daemon hands back the raw flat list instead of the
// tree every other device gets.
func TestParseHierarchy_FlatJSONFractionalFrames(t *testing.T) {
	const raw = `{"nodes":[
  {"index":0,"type":"Application","identifier":"App","rect":{"x":0,"y":0,"width":390,"height":844},"enabled":true},
  {"index":1,"type":"Other","identifier":"chat-composer-band","rect":{"x":16,"y":700.6666870117188,"width":370,"height":54.4},"enabled":true,"parentIndex":0}
]}`
	root, err := parseHierarchy([]byte(raw))
	if err != nil {
		t.Fatalf("parseHierarchy: %v", err)
	}
	band := findByID(t, root, "chat-composer-band")
	want := hBounds{X: 16, Y: 701, Width: 370, Height: 54}
	if band.Bounds == nil || *band.Bounds != want {
		t.Errorf("bounds = %+v, want %+v", band.Bounds, want)
	}

	tree, err := NormalizeHierarchy([]byte(raw))
	if err != nil {
		t.Fatalf("NormalizeHierarchy: %v", err)
	}
	if _, ok := tree.(hNode); !ok {
		t.Errorf("NormalizeHierarchy returned %T, want the normalized tree", tree)
	}
}

// A snapshot the runner cut off at its node cap says so on the root, in the
// compact listing, and when --find comes up empty.
func TestParseHierarchy_TruncatedSnapshotIsMarked(t *testing.T) {
	const raw = `{"truncated":true,"nodes":[
  {"index":0,"type":"Application","identifier":"App","rect":{"x":0,"y":0,"width":390,"height":844},"enabled":true},
  {"index":1,"type":"Cell","identifier":"row-1","rect":{"x":0,"y":0,"width":390,"height":40},"enabled":true,"parentIndex":0}
]}`
	root, err := parseHierarchy([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if root.Truncated == nil || !*root.Truncated {
		t.Errorf("root.Truncated = %v, want true", root.Truncated)
	}
	if child := findByID(t, root, "row-1"); child.Truncated != nil {
		t.Errorf("only the root carries truncated, row-1 has %v", *child.Truncated)
	}
	compact, _ := formatHierarchy([]byte(raw), true, "")
	if !strings.Contains(compact, "[truncated]") {
		t.Errorf("compact listing should mark the truncation:\n%s", compact)
	}
	none, _ := formatHierarchy([]byte(raw), false, "chat-composer")
	if !strings.Contains(none, "truncated") {
		t.Errorf("an empty --find on a truncated tree should say so, got %q", none)
	}

	whole, err := parseHierarchy([]byte(flatJSON))
	if err != nil {
		t.Fatal(err)
	}
	if whole.Truncated != nil {
		t.Errorf("a complete snapshot must not be marked truncated")
	}
}
