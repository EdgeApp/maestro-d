package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb/rfbtest"
)

const secret = "s3cr3t-pw"

// Screens the fake recogniser reports, as fractions of the screen.
var (
	desktopText  = []TextBox{{Text: "Chromium", X: 0.03, Y: 0.004, W: 0.04, H: 0.01}, {Text: "File", X: 0.08, Y: 0.004, W: 0.02, H: 0.01}}
	nameBox      = TextBox{Text: "Paul Puey", X: 0.48, Y: 0.87, W: 0.04, H: 0.014}
	promptBox    = TextBox{Text: "Enter Password", X: 0.47, Y: 0.9, W: 0.06, H: 0.015}
	passwordText = []TextBox{{Text: "7:26", X: 0.41, Y: 0.13, W: 0.18, H: 0.11}, nameBox, promptBox}
	userListText = []TextBox{{Text: "7:26", X: 0.41, Y: 0.13, W: 0.18, H: 0.11}, nameBox}
	namePassText = []TextBox{{Text: "Name", X: 0.47, Y: 0.5, W: 0.06, H: 0.02}, {Text: "Password", X: 0.47, Y: 0.55, W: 0.06, H: 0.02}}
)

// script is a recogniser that answers each call with the next screen, then
// keeps answering the last one.
type script struct {
	mu      sync.Mutex
	screens [][]TextBox
	err     error
	calls   int
}

func (s *script) Recognize(context.Context, []byte) ([]TextBox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	i := s.calls - 1
	if i >= len(s.screens) {
		i = len(s.screens) - 1
	}
	return s.screens[i], nil
}

func server(t *testing.T, cfg rfbtest.Config) *rfbtest.Server {
	if cfg.User == "" {
		cfg.User, cfg.Password = "paul", secret
	}
	if cfg.Width == 0 {
		cfg.Width, cfg.Height = 200, 100
	}
	return rfbtest.Start(t, cfg)
}

func open(t *testing.T, s *rfbtest.Server, rec Recognizer) (*Driver, error) {
	t.Helper()
	t.Cleanup(fastTimings())
	d, err := New(context.Background(), Config{
		Addr:        s.Addr(),
		Login:       rfb.Login{Method: rfb.MethodAccount, User: "paul", Password: secret, VNCPassword: "vnc-pw"},
		FullName:    "Paul Puey",
		DeviceName:  "Demo Mac",
		OSVersion:   "26.6.1",
		IdleTimeout: time.Minute,
		Recognizer:  rec,
	})
	if d != nil {
		t.Cleanup(d.Close)
	}
	return d, err
}

func mustOpen(t *testing.T, s *rfbtest.Server) *Driver {
	t.Helper()
	d, err := open(t, s, &script{screens: [][]TextBox{desktopText}})
	if err != nil {
		t.Fatal(err)
	}
	s.ResetEvents()
	return d
}

func keys(ev []rfbtest.Event) []uint32 {
	var out []uint32
	for _, e := range ev {
		if e.Key && e.Down {
			out = append(out, e.Keysym)
		}
	}
	return out
}

func typed(ev []rfbtest.Event) string {
	var b strings.Builder
	for _, k := range keys(ev) {
		if k < 0x100 {
			b.WriteRune(rune(k))
		}
	}
	return b.String()
}

func shiftOnly(t *testing.T, ev []rfbtest.Event) {
	t.Helper()
	if len(ev) != 2 || !ev[0].Key || ev[0].Keysym != rfb.KeyShift || !ev[0].Down || ev[1].Down {
		t.Errorf("want only the Shift wake, got %+v", ev)
	}
}

func TestLogin_DesktopTypesNothing(t *testing.T) {
	s := server(t, rfbtest.Config{})
	if _, err := open(t, s, &script{screens: [][]TextBox{desktopText}}); err != nil {
		t.Fatal(err)
	}
	shiftOnly(t, s.Events())
}

func TestLogin_UnrecognisedTypesNothing(t *testing.T) {
	s := server(t, rfbtest.Config{})
	// The user's name without app menus but off-centre: not a login screen.
	odd := []TextBox{{Text: "Paul Puey", X: 0.05, Y: 0.9, W: 0.04, H: 0.01}}
	if _, err := open(t, s, &script{screens: [][]TextBox{odd}}); err != nil {
		t.Fatal(err)
	}
	shiftOnly(t, s.Events())
}

// failsAfter answers screens, then fails every later call.
type failsAfter struct {
	screens [][]TextBox
	calls   int
}

func (f *failsAfter) Recognize(context.Context, []byte) ([]TextBox, error) {
	f.calls++
	if f.calls > len(f.screens) {
		return nil, errors.New("helper timed out")
	}
	return f.screens[f.calls-1], nil
}

func TestLogin_RecognizerFailsAfterThePassword(t *testing.T) {
	s := server(t, rfbtest.Config{})
	_, err := open(t, s, &failsAfter{screens: [][]TextBox{passwordText}})
	if err == nil || !strings.Contains(err.Error(), "the macOS password was entered, but the login could not be confirmed: text recognition unavailable: helper timed out") || errors.Is(err, errNothingTyped) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the error carries the password")
	}
}

func TestLogin_RecognizerFailsAfterTheUserListClick(t *testing.T) {
	s := server(t, rfbtest.Config{})
	_, err := open(t, s, &failsAfter{screens: [][]TextBox{userListText}})
	if err == nil || !errors.Is(err, errNothingTyped) {
		t.Fatalf("got %v", err)
	}
	if got := typed(s.Events()); got != "" {
		t.Errorf("typed %q", got)
	}
}

func TestLogin_PasswordScreen(t *testing.T) {
	s := server(t, rfbtest.Config{})
	if _, err := open(t, s, &script{screens: [][]TextBox{passwordText, desktopText}}); err != nil {
		t.Fatal(err)
	}
	ev := s.Events()
	if got := typed(ev); got != secret {
		t.Errorf("typed %q", got)
	}
	k := keys(ev)
	if k[0] != rfb.KeyShift || k[len(k)-1] != 0xff0d {
		t.Errorf("keys %x", k)
	}
	// The click lands on the prompt's centre: 0.5*200, 0.9075*100.
	var clicked bool
	for _, e := range ev {
		if !e.Key && e.Mask == 1 && e.X == 100 && e.Y == 90 {
			clicked = true
		}
	}
	if !clicked {
		t.Errorf("no click on the password prompt: %+v", ev)
	}
}

func TestLogin_UserList(t *testing.T) {
	s := server(t, rfbtest.Config{})
	if _, err := open(t, s, &script{screens: [][]TextBox{userListText, passwordText, desktopText}}); err != nil {
		t.Fatal(err)
	}
	if got := typed(s.Events()); got != secret {
		t.Errorf("typed %q", got)
	}
}

func TestLogin_UserListWithoutField(t *testing.T) {
	s := server(t, rfbtest.Config{})
	_, err := open(t, s, &script{screens: [][]TextBox{userListText}})
	if err == nil || !strings.Contains(err.Error(), "did not open a password field for paul") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(typed(s.Events()), "s3") {
		t.Error("typed the password into a user list")
	}
}

// A name-and-password form is never typed into: with no lock clock it may
// be any app or web page, even full screen with the menu bar hidden.
func TestLogin_NameAndPasswordFormTypesNothing(t *testing.T) {
	s := server(t, rfbtest.Config{})
	if _, err := open(t, s, &script{screens: [][]TextBox{namePassText}}); err != nil {
		t.Fatal(err)
	}
	shiftOnly(t, s.Events())
}

// Q1: attach returns at once, and the first key press or click waits
// until inputReady after the login; screenshots never wait.
func TestWaitInput(t *testing.T) {
	s := server(t, rfbtest.Config{})
	t.Cleanup(fastTimings())
	old := inputReady
	inputReady = 300 * time.Millisecond
	defer func() { inputReady = old }()
	start := time.Now()
	d, err := New(context.Background(), Config{Addr: s.Addr(), Login: rfb.Login{Method: rfb.MethodAccount, User: "paul", Password: secret},
		FullName: "Paul Puey", Recognizer: &script{screens: [][]TextBox{desktopText}}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if time.Since(start) >= 300*time.Millisecond {
		t.Errorf("attach waited for the input gate")
	}
	step, _ := flow.ParseStep([]byte("takeScreenshot: x"), "t.yaml")
	if r := d.Execute(step); !r.Success || time.Since(start) >= 300*time.Millisecond {
		t.Errorf("a screenshot waited for the input gate")
	}
	// A step refused before it sends anything answers at once.
	for _, y := range []string{"tapOn: Login", "launchApp: x", `inputText: "🙂"`} {
		step, _ = flow.ParseStep([]byte(y), "t.yaml")
		if r := d.Execute(step); r.Success || time.Since(start) >= 300*time.Millisecond {
			t.Errorf("%s waited for the input gate or succeeded", y)
		}
	}
	step, _ = flow.ParseStep([]byte(`tapOn: {point: "1, 1"}`), "t.yaml")
	if r := d.Execute(step); !r.Success {
		t.Fatal(r.Message)
	}
	if time.Since(d.loggedIn) < 300*time.Millisecond {
		t.Errorf("the first tap went out %v after login", time.Since(d.loggedIn))
	}
}

// A login screen waits for the input gate before it clicks or types.
func TestLogin_PasswordScreenWaitsForInput(t *testing.T) {
	s := server(t, rfbtest.Config{})
	t.Cleanup(fastTimings())
	old := inputReady
	inputReady = 200 * time.Millisecond
	defer func() { inputReady = old }()
	d, err := New(context.Background(), Config{Addr: s.Addr(), Login: rfb.Login{Method: rfb.MethodAccount, User: "paul", Password: secret},
		FullName: "Paul Puey", Recognizer: &script{screens: [][]TextBox{passwordText, desktopText}}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if time.Since(d.loggedIn) < 200*time.Millisecond {
		t.Errorf("typed the password before the input gate")
	}
}

func TestLogin_NotAccepted(t *testing.T) {
	s := server(t, rfbtest.Config{})
	_, err := open(t, s, &script{screens: [][]TextBox{passwordText}})
	if err == nil || err.Error() != "the login screen did not accept the macOS password" {
		t.Fatalf("got %v", err)
	}
}

func TestLogin_RecognizerFails(t *testing.T) {
	s := server(t, rfbtest.Config{})
	_, err := open(t, s, &script{err: errors.New("xcrun: not found")})
	if err == nil || !strings.Contains(err.Error(), "text recognition unavailable: xcrun: not found") || !errors.Is(err, errNothingTyped) {
		t.Fatalf("got %v", err)
	}
	shiftOnly(t, s.Events())
}

func TestLogin_AccountOnly(t *testing.T) {
	s := server(t, rfbtest.Config{Types: []byte{2}, VNCPassword: "vnc-pw"})
	_, err := open(t, s, &script{screens: [][]TextBox{desktopText}})
	var ua *rfb.AuthUnavailable
	if !errors.As(err, &ua) {
		t.Fatalf("got %v", err)
	}
	if s.Logins() != 0 {
		t.Error("logged in with the VNC password")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	s := server(t, rfbtest.Config{User: "paul", Password: "other"})
	_, err := open(t, s, &script{screens: [][]TextBox{desktopText}})
	var af *rfb.AuthFailed
	if !errors.As(err, &af) || strings.Contains(err.Error(), secret) {
		t.Fatalf("got %v", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		boxes []TextBox
		want  ScreenKind
	}{
		{"desktop", desktopText, ScreenDesktop},
		{"password", passwordText, ScreenPassword},
		{"plain Password prompt", []TextBox{passwordText[0], nameBox, {Text: "Password", X: 0.47, Y: 0.9, W: 0.06, H: 0.015}}, ScreenPassword},
		{"user list", userListText, ScreenUserList},
		{"name and password form", namePassText, ScreenDesktop},
		{"a full-screen sign-in page with the user's name", []TextBox{nameBox, promptBox}, ScreenDesktop},
		{"a small clock is not the lock clock", []TextBox{{Text: "7:26", X: 0.48, Y: 0.1, W: 0.04, H: 0.02}, nameBox, promptBox}, ScreenDesktop},
		{"a clock in the lower half is not the lock clock", []TextBox{{Text: "7:26", X: 0.41, Y: 0.6, W: 0.18, H: 0.11}, nameBox, promptBox}, ScreenDesktop},
		{"menu text beats a login screen", append(append([]TextBox{}, passwordText...), desktopText[0]), ScreenDesktop},
		{"status items on the right are fine", append(append([]TextBox{}, passwordText...), TextBox{Text: "U.S.", X: 0.9, Y: 0.004, W: 0.02, H: 0.01}), ScreenPassword},
		{"name in the top half", []TextBox{{Text: "Paul Puey", X: 0.48, Y: 0.2, W: 0.04, H: 0.01}}, ScreenDesktop},
		{"prompt above the name", []TextBox{passwordText[0], nameBox, {Text: "Enter Password", X: 0.47, Y: 0.6, W: 0.06, H: 0.015}}, ScreenUserList},
		{"nothing", nil, ScreenDesktop},
	}
	for _, c := range cases {
		if got := Classify(c.boxes, "Paul Puey").Kind; got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if got := Classify(passwordText, "").Kind; got != ScreenLoginNoName {
		t.Errorf("a lock screen without the full name: %s", got)
	}
	if got := Classify(desktopText, "").Kind; got != ScreenDesktop {
		t.Errorf("the desktop without the full name: %s", got)
	}
}

// Without the user's full name a lock screen cannot be logged in: the attach
// fails, types nothing, and says why.
func TestLogin_LockScreenWithoutFullName(t *testing.T) {
	s := server(t, rfbtest.Config{})
	t.Cleanup(fastTimings())
	d, err := New(context.Background(), Config{
		Addr:       s.Addr(),
		Login:      rfb.Login{Method: rfb.MethodAccount, User: "paul", Password: secret},
		Recognizer: &script{screens: [][]TextBox{passwordText}},
	})
	if d != nil {
		d.Close()
	}
	if err == nil || !errors.Is(err, errNothingTyped) || !strings.Contains(err.Error(), "full name of the macOS user paul is unknown") {
		t.Fatalf("got %v", err)
	}
	shiftOnly(t, s.Events())
}

// events waits until the server has every event sent so far (it answers a
// capture only after reading what came before it) and returns them.
func events(t *testing.T, d *Driver, s *rfbtest.Server) []rfbtest.Event {
	t.Helper()
	if _, err := d.Frame(context.Background(), image.Rect(0, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	return s.Events()
}

func pointerEvents(ev []rfbtest.Event) []rfbtest.Event {
	var out []rfbtest.Event
	for _, e := range ev {
		if !e.Key {
			out = append(out, e)
		}
	}
	return out
}

func run(t *testing.T, d *Driver, yaml string) string {
	t.Helper()
	step, err := flow.ParseStep([]byte(yaml), "t.yaml")
	if err != nil {
		t.Fatalf("%s: %v", yaml, err)
	}
	r := d.Execute(step)
	if !r.Success {
		return "FAIL: " + r.Message
	}
	return r.Message
}

func TestExecute_Taps(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	run(t, d, `tapOn: {point: "50%, 50%"}`)
	run(t, d, "tapOnPoint: {x: 300, y: -5}")
	run(t, d, `tapOn: {point: "10, 10", repeat: 2}`) // the executor repeats; the driver taps once
	run(t, d, `tapOn: {point: "20, 20", longPress: true}`)
	p := pointerEvents(events(t, d, s))
	want := []rfbtest.Event{
		{X: 100, Y: 50}, {Mask: 1, X: 100, Y: 50}, {X: 100, Y: 50},
		{X: 199, Y: 0}, {Mask: 1, X: 199, Y: 0}, {X: 199, Y: 0}, // clamped
		{X: 10, Y: 10}, {Mask: 1, X: 10, Y: 10}, {X: 10, Y: 10},
		{X: 20, Y: 20}, {Mask: 1, X: 20, Y: 20}, {X: 20, Y: 20},
	}
	if len(p) != len(want) {
		t.Fatalf("got %+v", p)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Errorf("%d: %+v want %+v", i, p[i], want[i])
		}
	}
}

func TestExecute_Clicks(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	run(t, d, `clickOn: {point: "40, 30", button: right}`)
	run(t, d, "clickOn: {count: 2, button: middle}")
	run(t, d, `moveMouse: "0%, 100%"`)
	p := pointerEvents(events(t, d, s))
	want := []rfbtest.Event{
		{X: 40, Y: 30}, {Mask: 4, X: 40, Y: 30}, {X: 40, Y: 30},
		{X: 40, Y: 30}, {Mask: 2, X: 40, Y: 30}, {X: 40, Y: 30}, {Mask: 2, X: 40, Y: 30}, {X: 40, Y: 30},
		{X: 0, Y: 99},
	}
	for i := range want {
		if i >= len(p) || p[i] != want[i] {
			t.Fatalf("got %+v", p)
		}
	}
}

func TestExecute_SwipeDrags(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	run(t, d, `swipe: {start: "10%, 10%", end: "90%, 90%"}`)
	p := pointerEvents(events(t, d, s))
	first, last := p[0], p[len(p)-1]
	if first != (rfbtest.Event{X: 20, Y: 10}) || p[1].Mask != 1 || last != (rfbtest.Event{X: 180, Y: 90}) {
		t.Errorf("drag %+v", p)
	}
	for _, e := range p[1 : len(p)-1] {
		if e.Mask != 1 {
			t.Errorf("button released mid-drag: %+v", e)
		}
	}
	s.ResetEvents()
	run(t, d, "swipe: {startX: 5, startY: 6, endX: 7, endY: 8}")
	if p := pointerEvents(events(t, d, s)); p[0] != (rfbtest.Event{X: 5, Y: 6}) || p[len(p)-1] != (rfbtest.Event{X: 7, Y: 8}) {
		t.Errorf("absolute %+v", p)
	}
	s.ResetEvents()
	run(t, d, "swipe: UP")
	if p := pointerEvents(events(t, d, s)); p[0].Y <= p[len(p)-1].Y || p[0].X != 100 {
		t.Errorf("up %+v", p)
	}
	if got := run(t, d, "swipe: {direction: SIDEWAYS}"); !strings.HasPrefix(got, "FAIL") {
		t.Errorf("bad direction %q", got)
	}
	if got := run(t, d, "swipe: {from: {id: list}, direction: UP}"); !strings.Contains(got, noHierarchy) {
		t.Errorf("anchored swipe %q", got)
	}
}

func TestExecute_Scroll(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	wheelBits := func(yaml string) []uint8 {
		s.ResetEvents()
		run(t, d, yaml)
		var bits []uint8
		for _, e := range pointerEvents(events(t, d, s)) {
			if e.Mask != 0 {
				bits = append(bits, e.Mask)
			}
		}
		return bits
	}
	for yaml, want := range map[string]uint8{"scroll": 16, "scroll: {direction: DOWN}": 16, "scroll: {direction: UP}": 8, "scroll: {direction: LEFT}": 32, "scroll: {direction: RIGHT}": 64} {
		bits := wheelBits(yaml)
		if len(bits) != 5 || bits[0] != want {
			t.Errorf("%s: %v", yaml, bits)
		}
	}
	if bits := wheelBits(`scrollAt: {point: "10, 10", x: -2, y: 3}`); len(bits) != 5 || bits[0] != 16 || bits[3] != 32 {
		t.Errorf("scrollAt %v", bits)
	}
	if got := run(t, d, "scroll: {direction: AROUND}"); !strings.HasPrefix(got, "FAIL") {
		t.Errorf("bad direction %q", got)
	}
}

func TestTypeText_LineBreaks(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	run(t, d, `inputText: "a\r\nb\rc\nd"`)
	got := keys(events(t, d, s))
	want := []uint32{'a', 0xff0d, 'b', 0xff0d, 'c', 0xff0d, 'd'}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("keys %x, want %x", got, want)
	}
}

func TestExecute_Keys(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	run(t, d, `inputText: "Hé€\n"`)
	run(t, d, "pressKey: Meta+Shift+K")
	run(t, d, "eraseText: {characters: 2}")
	run(t, d, "eraseText")
	ev := events(t, d, s)
	var seq []rfbtest.Event
	for _, e := range ev {
		if e.Key {
			seq = append(seq, e)
		}
	}
	wantStart := []rfbtest.Event{
		{Key: true, Down: true, Keysym: 'H'}, {Key: true, Keysym: 'H'},
		{Key: true, Down: true, Keysym: 0xe9}, {Key: true, Keysym: 0xe9},
		{Key: true, Down: true, Keysym: 0x010020ac}, {Key: true, Keysym: 0x010020ac},
		{Key: true, Down: true, Keysym: 0xff0d}, {Key: true, Keysym: 0xff0d},
		{Key: true, Down: true, Keysym: rfb.KeyMeta}, {Key: true, Down: true, Keysym: rfb.KeyShift},
		{Key: true, Down: true, Keysym: 'k'}, {Key: true, Keysym: 'k'},
		{Key: true, Keysym: rfb.KeyShift}, {Key: true, Keysym: rfb.KeyMeta},
	}
	for i := range wantStart {
		if seq[i] != wantStart[i] {
			t.Fatalf("%d: %+v want %+v", i, seq[i], wantStart[i])
		}
	}
	if n := len(seq) - len(wantStart); n != 2*52 {
		t.Errorf("erase sent %d events", n)
	}
	if got := run(t, d, "pressKey: Hyper+K"); !strings.HasPrefix(got, "FAIL") {
		t.Errorf("bad combo %q", got)
	}
	s.ResetEvents()
	if got := run(t, d, `inputText: "ab🙂c"`); !strings.Contains(got, "only characters up to U+FFFF") {
		t.Errorf("non-BMP text: %q", got)
	}
	if got := run(t, d, "pressKey: Meta+🙂"); !strings.Contains(got, "beyond U+FFFF") {
		t.Errorf("non-BMP key: %q", got)
	}
	if ev := events(t, d, s); len(ev) != 0 {
		t.Errorf("non-BMP text sent %d events", len(ev))
	}
	for _, k := range []string{"Alt+E", "Option+Left", "Meta+Alt+K"} {
		if got := run(t, d, "pressKey: "+k); !strings.Contains(got, "no Option key") {
			t.Errorf("%s: %q", k, got)
		}
	}
}

func TestExecute_Unsupported(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	for _, yaml := range []string{"tapOn: Login", "tapOn: {id: login}", `inputText: {text: x, id: field}`, "doubleTapOn: Login", "longPressOn: Login", "takeScreenshot: {path: a, cropOn: {id: x}}"} {
		if got := run(t, d, yaml); !strings.Contains(got, noHierarchy) {
			t.Errorf("%s: %q", yaml, got)
		}
	}
	if got := run(t, d, "launchApp: com.apple.TextEdit"); !strings.Contains(got, "launchApp is not supported on the computer device") {
		t.Errorf("launchApp: %q", got)
	}
	if got := run(t, d, "hideKeyboard"); strings.HasPrefix(got, "FAIL") {
		t.Errorf("hideKeyboard %q", got)
	}
	if _, err := d.Hierarchy(); err == nil {
		t.Error("Hierarchy succeeded")
	}
}

func TestScreenshotsAndInfo(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	data, err := d.Screenshot()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != 200 || img.Bounds().Dy() != 100 {
		t.Fatalf("png %v %v", img.Bounds(), err)
	}
	if c := color.RGBAModel.Convert(img.At(150, 80)).(color.RGBA); c != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("pixel %v", c)
	}
	for _, yaml := range []string{"takeScreenshot: shot", "assertScreenshot: shot.png"} {
		step, _ := flow.ParseStep([]byte(yaml), "t.yaml")
		r := d.Execute(step)
		if !r.Success || !bytes.HasPrefix(r.Data.([]byte), []byte("\x89PNG")) {
			t.Errorf("%s: %+v", yaml, r)
		}
	}
	f, err := d.Frame(context.Background(), image.Rect(0, 0, 10, 10))
	if err != nil || f.RGBAAt(5, 5) != (color.RGBA{255, 0, 0, 255}) {
		t.Errorf("frame %v", err)
	}
	if w, h, _ := d.ScreenshotSize(); w != 200 || h != 100 {
		t.Errorf("size %dx%d", w, h)
	}
	pi := d.GetPlatformInfo()
	if pi.Platform != "macos" || pi.DeviceID != "mac" || pi.DeviceName != "Demo Mac" || pi.OSVersion != "26.6.1" || pi.ScreenWidth != 200 {
		t.Errorf("%+v", pi)
	}
	if d.DefaultImage() != "jpeg" || d.IdleTimeout() != time.Minute || d.GetState() == nil {
		t.Error("capabilities")
	}
	d.SetFindTimeout(1)
	_ = d.SetWaitForIdleTimeout(1)
	d.SetContext(nil)
}

func TestCapturesRetryAcrossAResize(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)

	s.Resize(150, 80)
	data, err := d.Screenshot()
	if err != nil {
		t.Fatalf("screenshot across a resize: %v", err)
	}
	if img, err := png.Decode(bytes.NewReader(data)); err != nil || img.Bounds().Dx() != 150 || img.Bounds().Dy() != 80 {
		t.Fatalf("screenshot after the resize: %v %v", img.Bounds(), err)
	}

	// Frame leaves the retry to its caller, who chose the rectangle.
	s.Resize(160, 90)
	if _, err := d.Frame(context.Background(), image.Rect(0, 0, 10, 10)); !errors.Is(err, core.ErrScreenResized) {
		t.Errorf("frame across a resize: %v", err)
	}
	if f, err := d.Frame(context.Background(), image.Rect(0, 0, 160, 90)); err != nil || f.Bounds().Dx() != 160 {
		t.Errorf("frame at the new size: %v", err)
	}

	s.Resize(170, 95)
	if got := run(t, d, "waitForAnimationToEnd"); got != "screen is still" {
		t.Errorf("waitForAnimationToEnd across a resize: %q", got)
	}

	s.Resize(180, 96)
	c, err := d.check(context.Background())
	if err != nil || c.Kind != ScreenDesktop {
		t.Errorf("login check across a resize: %+v %v", c, err)
	}
	if w, h := d.client.Size(); w != 180 || h != 96 {
		t.Errorf("size %dx%d", w, h)
	}
}

func TestWaitForAnimationToEnd(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	if got := run(t, d, "waitForAnimationToEnd"); got != "screen is still" {
		t.Errorf("still: %q", got)
	}
	n := 0
	var mu sync.Mutex
	s.SetFrame(func(x, y int) color.RGBA {
		mu.Lock()
		defer mu.Unlock()
		if x == 0 && y == 0 {
			n++
		}
		return color.RGBA{uint8(n), 0, 0, 255}
	})
	if got := run(t, d, "waitForAnimationToEnd: {timeout: 20}"); !strings.Contains(got, "still changing") {
		t.Errorf("changing: %q", got)
	}
}

func TestCloseReleasesHeld(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	d.mu.Lock()
	d.buttons, d.pointer, d.modifiers = 1, image.Pt(3, 4), []uint32{rfb.KeyMeta, rfb.KeyShift}
	d.mu.Unlock()
	d.Close()
	ev := s.WaitEvents(3)
	want := []rfbtest.Event{{X: 3, Y: 4}, {Key: true, Keysym: rfb.KeyShift}, {Key: true, Keysym: rfb.KeyMeta}}
	for i := range want {
		if i >= len(ev) || ev[i] != want[i] {
			t.Fatalf("got %+v", ev)
		}
	}
}

func TestConnectionLost(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	s.DropClients()
	<-d.Done()
	if got := run(t, d, "hideKeyboard"); !strings.Contains(got, "Screen Sharing connection closed") {
		t.Errorf("got %q", got)
	}
}

func TestInputErrors(t *testing.T) {
	s := server(t, rfbtest.Config{})
	d := mustOpen(t, s)
	for _, yaml := range []string{`tapOn: {point: "abc"}`, `clickOn: {point: "x"}`, `moveMouse: "1"`, `scrollAt: {point: "q", y: 1}`, `swipe: {start: "a", end: "1, 1"}`, `swipe: {start: "1, 1", end: "b"}`} {
		if got := run(t, d, yaml); !strings.HasPrefix(got, "FAIL") {
			t.Errorf("%s: %q", yaml, got)
		}
	}
	// Every write fails once the connection is gone.
	_ = d.client.Close()
	p := image.Pt(1, 1)
	for name, err := range map[string]error{
		"press":    d.press(p, maskLeft, 1, 0),
		"drag":     d.drag(p, p, time.Millisecond),
		"wheel":    d.wheel(p, 1, 1),
		"tapKey":   d.tapKey('a'),
		"typeText": d.typeText("ab"),
		"secret":   d.typeSecret("ab"),
		"move":     d.move(p),
	} {
		if err == nil {
			t.Errorf("%s succeeded on a closed connection", name)
		}
	}
	if r := d.pressKey("Meta+K"); r.Success {
		t.Error("pressKey succeeded on a closed connection")
	}
	if _, err := d.Screenshot(); err == nil {
		t.Error("Screenshot succeeded on a closed connection")
	}
	if r := d.screenshotResult(); r.Success {
		t.Error("screenshotResult succeeded")
	}
	if r := d.waitForStill(10); r.Success {
		t.Error("waitForStill succeeded")
	}
	if r := d.tapAt("1, 1", false, 5); r.Success {
		t.Error("tapAt succeeded")
	}
	if r := d.clickOn(&flow.ClickOnStep{Button: "nope"}); r.Success {
		t.Error("clickOn succeeded")
	}
	if r := d.scroll(""); r.Success {
		t.Error("scroll succeeded")
	}
	if r := d.swipe(&flow.SwipeStep{StartX: 1, EndX: 2}); r.Success {
		t.Error("swipe succeeded")
	}
	if _, err := d.check(context.Background()); err == nil {
		t.Error("check succeeded")
	}
}

func TestLogin_ClosedDuringLogin(t *testing.T) {
	s := server(t, rfbtest.Config{})
	rec := &script{screens: [][]TextBox{passwordText}}
	// Drop the connection once the first screen has been read.
	wrapped := RecognizerFunc(func(ctx context.Context, img []byte) ([]TextBox, error) {
		b, err := rec.Recognize(ctx, img)
		s.DropClients()
		return b, err
	})
	if _, err := open(t, s, wrapped); err == nil {
		t.Error("login survived a dropped connection")
	}
}
