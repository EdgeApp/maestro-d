package computer

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
)

// PointerEvent button bits.
const (
	maskLeft   uint8 = 1
	maskMiddle uint8 = 2
	maskRight  uint8 = 4
	wheelUp    uint8 = 8
	wheelDown  uint8 = 16
	wheelLeft  uint8 = 32
	wheelRight uint8 = 64
)

// Input pacing, variables so tests can shorten them.
var (
	clickGap      = 50 * time.Millisecond   // between the clicks of a double click
	defaultHold   = 1000 * time.Millisecond // a long press
	keyGap        = 5 * time.Millisecond    // between typed characters
	dragStep      = 16 * time.Millisecond   // between the moves of a drag
	defaultDrag   = 400 * time.Millisecond
	directionDrag = 0.6 // fraction of the screen a direction swipe crosses
	scrollTicks   = 5
)

var buttonMasks = map[string]uint8{"left": maskLeft, "middle": maskMiddle, "right": maskRight}

// point parses "x, y" or "x%, y%" against the screen and clamps it inside.
func (d *Driver) point(s string) (image.Point, error) {
	w, h := d.client.Size()
	x, y, err := core.ParsePointCoords(s, w, h)
	if err != nil {
		return image.Point{}, err
	}
	return clampPoint(image.Pt(x, y), w, h), nil
}

func clampPoint(p image.Point, w, h int) image.Point {
	p.X = min(max(p.X, 0), w-1)
	p.Y = min(max(p.Y, 0), h-1)
	return p
}

// pointerAt sends the pointer to p with buttons held and records both.
// Like every input, it waits out the gate after login (waitInput) first; a
// step refused before it sends anything answers at once.
func (d *Driver) pointerAt(p image.Point, buttons uint8) error {
	d.waitInput(d.context())
	d.mu.Lock()
	d.pointer, d.buttons = p, buttons
	d.mu.Unlock()
	return d.client.Pointer(buttons, p.X, p.Y)
}

func (d *Driver) held() (image.Point, uint8) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pointer, d.buttons
}

func (d *Driver) move(p image.Point) error {
	_, buttons := d.held()
	return d.pointerAt(p, buttons)
}

// click moves to p and presses mask count times; hold keeps the last press
// down that long.
func (d *Driver) click(p image.Point, mask uint8, count int) error {
	return d.press(p, mask, count, 0)
}

func (d *Driver) press(p image.Point, mask uint8, count int, hold time.Duration) error {
	_, base := d.held()
	if err := d.pointerAt(p, base); err != nil {
		return err
	}
	ctx := d.context()
	for i := 0; i < count; i++ {
		if i > 0 {
			sleep(ctx, clickGap)
		}
		if err := d.pointerAt(p, base|mask); err != nil {
			return err
		}
		if hold > 0 {
			sleep(ctx, hold)
		}
		if err := d.pointerAt(p, base&^mask); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) tapAt(point string, long bool, durationMs int) *core.CommandResult {
	p, err := d.point(point)
	if err != nil {
		return fail(err)
	}
	return d.tapPoint(p, long, durationMs)
}

func (d *Driver) tapPoint(p image.Point, long bool, durationMs int) *core.CommandResult {
	var hold time.Duration
	switch {
	case durationMs > 0:
		hold = time.Duration(durationMs) * time.Millisecond
	case long:
		hold = defaultHold
	}
	return result(d.press(p, maskLeft, 1, hold), "tapped %d, %d", p.X, p.Y)
}

func (d *Driver) clickOn(s *flow.ClickOnStep) *core.CommandResult {
	p, _ := d.held()
	if s.Point != "" {
		var err error
		if p, err = d.point(s.Point); err != nil {
			return fail(err)
		}
	}
	mask, okb := buttonMasks[s.Button]
	if !okb {
		mask = maskLeft
	}
	count := s.Count
	if count < 1 {
		count = 1
	}
	return result(d.click(p, mask, count), "%s click ×%d at %d, %d", s.Button, count, p.X, p.Y)
}

// drag holds the left button from a to b over dur.
func (d *Driver) drag(a, b image.Point, dur time.Duration) error {
	if err := d.pointerAt(a, 0); err != nil {
		return err
	}
	if err := d.pointerAt(a, maskLeft); err != nil {
		return err
	}
	ctx := d.context()
	steps := max(int(dur/dragStep), 1)
	for i := 1; i <= steps; i++ {
		sleep(ctx, dragStep)
		p := image.Pt(a.X+(b.X-a.X)*i/steps, a.Y+(b.Y-a.Y)*i/steps)
		if err := d.pointerAt(p, maskLeft); err != nil {
			return err
		}
	}
	return d.pointerAt(b, 0)
}

func (d *Driver) swipe(s *flow.SwipeStep) *core.CommandResult {
	if s.Selector != nil && !s.Selector.IsEmpty() {
		return fail(fmt.Errorf(noHierarchy))
	}
	w, h := d.client.Size()
	var a, b image.Point
	switch {
	case s.Start != "" || s.End != "":
		var err error
		if a, err = d.point(s.Start); err != nil {
			return fail(fmt.Errorf("swipe start: %w", err))
		}
		if b, err = d.point(s.End); err != nil {
			return fail(fmt.Errorf("swipe end: %w", err))
		}
	case s.StartX != 0 || s.StartY != 0 || s.EndX != 0 || s.EndY != 0:
		a, b = clampPoint(image.Pt(s.StartX, s.StartY), w, h), clampPoint(image.Pt(s.EndX, s.EndY), w, h)
	default:
		dir, err := core.NormalizeSwipeDirection(s.Direction)
		if err != nil {
			return fail(err)
		}
		dist := s.Distance
		if dist <= 0 {
			dist = directionDrag
		}
		x0, y0, x1, y1, err := core.DirectionSwipeScreenCoords(dir, w, h, dist)
		if err != nil {
			return fail(err)
		}
		a, b = clampPoint(image.Pt(x0, y0), w, h), clampPoint(image.Pt(x1, y1), w, h)
	}
	dur := defaultDrag
	if s.Duration > 0 {
		dur = time.Duration(s.Duration) * time.Millisecond
	}
	return result(d.drag(a, b, dur), "dragged %d, %d to %d, %d", a.X, a.Y, b.X, b.Y)
}

// wheel turns the scroll wheel x ticks right (negative left) and y ticks
// down (negative up) at p.
func (d *Driver) wheel(p image.Point, x, y int) error {
	if err := d.move(p); err != nil {
		return err
	}
	_, base := d.held()
	tick := func(bit uint8, n int) error {
		for i := 0; i < n; i++ {
			d.waitInput(d.context())
			if err := d.client.Pointer(base|bit, p.X, p.Y); err != nil {
				return err
			}
			if err := d.client.Pointer(base, p.X, p.Y); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if y > 0 {
		err = tick(wheelDown, y)
	} else if y < 0 {
		err = tick(wheelUp, -y)
	}
	if err != nil {
		return err
	}
	if x > 0 {
		return tick(wheelRight, x)
	} else if x < 0 {
		return tick(wheelLeft, -x)
	}
	return nil
}

// scroll is Maestro's scroll: DOWN by default, at the screen's centre.
func (d *Driver) scroll(direction string) *core.CommandResult {
	w, h := d.client.Size()
	c := image.Pt(w/2, h/2)
	var x, y int
	switch strings.ToUpper(strings.TrimSpace(direction)) {
	case "", "DOWN":
		y = scrollTicks
	case "UP":
		y = -scrollTicks
	case "LEFT":
		x = -scrollTicks
	case "RIGHT":
		x = scrollTicks
	default:
		return fail(fmt.Errorf("scroll direction %q: use UP, DOWN, LEFT or RIGHT", direction))
	}
	return result(d.wheel(c, x, y), "scrolled %s", strings.ToLower(strings.TrimSpace(direction)))
}

func (d *Driver) tapKey(keysym uint32) error {
	d.waitInput(d.context())
	if err := d.client.Key(true, keysym); err != nil {
		return err
	}
	return d.client.Key(false, keysym)
}

func (d *Driver) typeText(text string) error {
	for _, r := range text {
		if r > 0xffff {
			// Measured on macOS 26: Screen Sharing drops a keysym beyond
			// the Basic Multilingual Plane without a trace.
			return fmt.Errorf("Screen Sharing cannot type %q (U+%04X): only characters up to U+FFFF reach the Mac; nothing was typed", r, r)
		}
	}
	// A Windows line break is one Return, not two.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	ctx := d.context()
	for i, r := range text {
		if i > 0 {
			sleep(ctx, keyGap)
		}
		if err := d.tapKey(rfb.KeysymForRune(r)); err != nil {
			return err
		}
	}
	return nil
}

// pressKey presses a key or a combination: modifiers down in order, the key,
// modifiers up in reverse.
func (d *Driver) pressKey(key string) *core.CommandResult {
	combo, err := rfb.ParseCombo(key)
	if err != nil {
		return fail(err)
	}
	if combo.Key >= 0x01010000 {
		return fail(fmt.Errorf("pressKey %q: Screen Sharing cannot type characters beyond U+FFFF", key))
	}
	for _, m := range combo.Modifiers {
		if m == rfb.KeyAlt {
			// Measured on macOS 26: Screen Sharing turns Alt (and Super and
			// Hyper) into Command, and no keysym reaches Option, so Alt+Q
			// would quit an app.
			return fail(fmt.Errorf("pressKey %q: Screen Sharing gives a VNC client no Option key (Alt arrives as Command); type the character itself with inputText (\"é\") instead", key))
		}
	}
	// A letter after modifiers is the key, not its capital: an uppercase
	// keysym makes Screen Sharing add Shift on its own.
	if len(combo.Modifiers) > 0 && combo.Key >= 'A' && combo.Key <= 'Z' {
		combo.Key += 'a' - 'A'
	}
	release := func() {
		d.mu.Lock()
		mods := d.modifiers
		d.modifiers = nil
		d.mu.Unlock()
		for i := len(mods) - 1; i >= 0; i-- {
			_ = d.client.Key(false, mods[i])
		}
	}
	defer release()
	d.waitInput(d.context())
	for _, m := range combo.Modifiers {
		if err := d.client.Key(true, m); err != nil {
			return fail(err)
		}
		d.mu.Lock()
		d.modifiers = append(d.modifiers, m)
		d.mu.Unlock()
	}
	return result(d.tapKey(combo.Key), "pressed %s", key)
}
