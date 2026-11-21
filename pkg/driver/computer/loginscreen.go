package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"regexp"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
)

// ScreenKind is what the screen check decided the connection shows.
type ScreenKind string

const (
	ScreenDesktop  ScreenKind = "desktop"   // not a login screen
	ScreenPassword ScreenKind = "password"  // lock screen, or a login window with the user's field
	ScreenUserList ScreenKind = "user-list" // login window listing users, or an idle lock screen
	// ScreenLoginNoName is a lock screen or login window (no app menu, the
	// large clock) checked without the user's full name, so the user's
	// entry on it cannot be found.
	ScreenLoginNoName ScreenKind = "login-without-name"
)

// Classification is a ScreenKind and the text box to click for it.
type Classification struct {
	Kind   ScreenKind
	Target TextBox
}

// menuBarHeight is the top fraction of the screen the menu bar takes. App
// menus sit in its left half; the clock, input menu and status items sit on
// the right, and a login screen shows only those.
const menuBarHeight = 0.03

func centred(b TextBox) bool { return b.CenterX() > 1.0/3 && b.CenterX() < 2.0/3 }

func sameText(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

func isPasswordPrompt(text string) bool {
	return sameText(text, "Enter Password") || sameText(text, "Password")
}

var clockText = regexp.MustCompile(`^\d{1,2}:\d{2}$`)

// isLockClock is the large clock a lock screen and the login window show
// near the top: an app's form, even full screen, does not draw one.
func isLockClock(b TextBox) bool {
	return clockText.MatchString(strings.TrimSpace(b.Text)) && centred(b) && b.CenterY() < 0.35 && b.H >= 0.04
}

// Classify reads a screen's text. A login screen shows no app menu and
// does show the large clock near the top and the user's full name in the
// lower half, centred; with a password prompt below the name it is the
// password screen, without one the user list. Everything else is treated
// as the desktop, so nothing is typed there — including a full-screen app or
// web page that draws its own name and password fields. The session's lock
// flag is never used: it has read "locked" while the desktop was showing.
func Classify(boxes []TextBox, fullName string) Classification {
	clock := false
	for _, b := range boxes {
		if b.CenterY() < menuBarHeight && b.CenterX() < 0.5 {
			return Classification{Kind: ScreenDesktop}
		}
		clock = clock || isLockClock(b)
	}
	if !clock {
		return Classification{Kind: ScreenDesktop}
	}
	if fullName == "" {
		return Classification{Kind: ScreenLoginNoName}
	}
	var name, prompt *TextBox
	for i := range boxes {
		b := &boxes[i]
		if sameText(b.Text, fullName) && b.CenterY() > 0.5 && centred(*b) {
			name = b
		}
	}
	if name == nil {
		return Classification{Kind: ScreenDesktop}
	}
	for i := range boxes {
		b := &boxes[i]
		if isPasswordPrompt(b.Text) && centred(*b) && b.CenterY() > name.CenterY() {
			prompt = b
		}
	}
	if prompt != nil {
		return Classification{Kind: ScreenPassword, Target: *prompt}
	}
	return Classification{Kind: ScreenUserList, Target: *name}
}

// Timings of the login, variables so tests can shorten them.
var (
	inputReady    = 8500 * time.Millisecond // after login before the first key press or click (waitInput)
	loginSettle   = 300 * time.Millisecond  // after a click or key before the next capture
	loginTimeout  = 15 * time.Second        // for the login screen to go away
	loginInterval = 500 * time.Millisecond
)

// errNothingTyped marks a login that stopped before typing anything.
var errNothingTyped = errors.New("nothing was typed")

// check captures the screen and classifies it.
func (d *Driver) check(ctx context.Context) (Classification, error) {
	img, err := d.captureScreen(ctx)
	if err != nil {
		return Classification{}, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return Classification{}, err
	}
	boxes, err := d.cfg.Recognizer.Recognize(ctx, buf.Bytes())
	if err != nil {
		return Classification{}, fmt.Errorf("text recognition unavailable: %v", err)
	}
	c := Classify(boxes, d.cfg.FullName)
	logger.Info("computer: screen check: %s", c.Kind)
	return c, nil
}

// logIn makes sure the connection shows the desktop. An account login
// usually lands there, and then nothing is typed; a login window (no one
// logged in) or a lock screen is logged in with the same account.
func (d *Driver) logIn(ctx context.Context) error {
	// Shift wakes an idle lock screen's password field and does nothing on
	// a desktop. Modifiers reach the Mac at once after a login; key presses
	// and clicks do not (waitInput).
	if err := d.client.Key(true, rfb.KeyShift); err != nil {
		return err
	}
	if err := d.client.Key(false, rfb.KeyShift); err != nil {
		return err
	}
	sleep(ctx, loginSettle)
	c, err := d.check(ctx)
	if err != nil {
		return fmt.Errorf("%w; %w", err, errNothingTyped)
	}
	if c.Kind == ScreenLoginNoName {
		return fmt.Errorf("the Mac shows a lock screen or login window, and the full name of the macOS user %s is unknown, so maestro-d cannot find the user on it; %w", d.cfg.Login.User, errNothingTyped)
	}
	if c.Kind == ScreenUserList {
		d.clickBox(c.Target)
		sleep(ctx, loginSettle)
		if c, err = d.check(ctx); err != nil {
			return fmt.Errorf("%w; %w", err, errNothingTyped)
		}
		if c.Kind != ScreenPassword {
			return fmt.Errorf("the user list did not open a password field for %s", d.cfg.Login.User)
		}
	}
	if c.Kind == ScreenDesktop {
		return nil
	}
	switch c.Kind {
	case ScreenPassword:
		d.clickBox(c.Target)
		sleep(ctx, loginSettle)
		if err := d.typeSecret(d.cfg.Login.Password); err != nil {
			return err
		}
	}
	if err := d.tapKey(0xff0d); err != nil {
		return err
	}
	deadline := time.Now().Add(loginTimeout)
	for time.Now().Before(deadline) {
		sleep(ctx, loginInterval)
		c, err := d.check(ctx)
		if err != nil {
			return fmt.Errorf("the macOS password was entered, but the login could not be confirmed: %w", err)
		}
		if c.Kind == ScreenDesktop {
			return nil
		}
	}
	return errors.New("the login screen did not accept the macOS password")
}

// clickBox clicks the centre of a recognised text box.
func (d *Driver) clickBox(b TextBox) {
	w, h := d.client.Size()
	d.click(image.Pt(int(b.CenterX()*float64(w)), int(b.CenterY()*float64(h))), maskLeft, 1)
}

// typeSecret types text without ever logging it.
func (d *Driver) typeSecret(s string) error {
	for _, r := range s {
		if err := d.tapKey(rfb.KeysymForRune(r)); err != nil {
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
