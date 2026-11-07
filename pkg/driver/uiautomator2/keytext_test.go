package uiautomator2

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/uiautomator2"
)

// withFastPaste shortens the paste wait for the duration of a test.
func withFastPaste(t *testing.T) {
	t.Helper()
	wait := pasteSettleWait
	pasteSettleWait = 0
	t.Cleanup(func() { pasteSettleWait = wait })
}

// fieldElement returns an element whose text is the field's current text.
func fieldElement(text string) *uiautomator2.Element {
	return uiautomator2.NewCachedElement("field", text, uiautomator2.ElementRect{})
}

func TestKeyTypeable(t *testing.T) {
	for _, r := range []rune{' ', 'a', 'Z', '0', '~', '@', '\n', '\t', 0xe000, 0xe003, 0xe007, 0xe05d} {
		if !keyTypeable(r) {
			t.Errorf("keyTypeable(%q) = false, want true", r)
		}
	}
	for _, r := range []rune{0x7f, 0x1b, '\r', 0, 'é', '中', '🎉', '‍', 0xdfff, 0xe05e, 0xf000} {
		if keyTypeable(r) {
			t.Errorf("keyTypeable(%q) = true, want false", r)
		}
	}
}

func TestSplitKeyTypeable(t *testing.T) {
	tests := []struct {
		text string
		want []textRun
	}{
		{"", nil},
		{"abc", []textRun{{"abc", false}}},
		{"🎉", []textRun{{"🎉", true}}},
		{"hello 🎉", []textRun{{"hello ", false}, {"🎉", true}}},
		{"a🎉b", []textRun{{"a", false}, {"🎉", true}, {"b", false}}},
		{"é", []textRun{{"é", true}}},
		{"中文", []textRun{{"中文", true}}},
		{"👩‍👩‍👧", []textRun{{"👩‍👩‍👧", true}}},
		{"a\nb\tc", []textRun{{"a\nb\tc", false}}},
		{"a\x7fb", []textRun{{"a", false}, {"\x7f", true}, {"b", false}}},
		{"hi 🎉 there😀", []textRun{{"hi ", false}, {"🎉", true}, {" there", false}, {"😀", true}}},
		{"go🎉\ue007", []textRun{{"go", false}, {"🎉", true}, {"\ue007", false}}},
	}
	for _, tt := range tests {
		got := splitKeyTypeable(tt.text)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitKeyTypeable(%q) = %+v, want %+v", tt.text, got, tt.want)
		}
		var joined strings.Builder
		for _, run := range got {
			joined.WriteString(run.text)
		}
		if joined.String() != tt.text {
			t.Errorf("runs of %q join to %q", tt.text, joined.String())
		}
	}
}

func TestTypeKeysASCIIUsesKeyActionsOnly(t *testing.T) {
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	d := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	if err := d.typeKeys("plain ascii 123"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	if want := []string{"keys plain ascii 123"}; !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q (ASCII never touches the clipboard)", order, want)
	}
}

func TestTypeKeysPastesEmojiAndRestoresClipboard(t *testing.T) {
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	d := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	if err := d.typeKeys("hello 🎉"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	want := []string{"keys hello ", "clipboard 🎉", "keycode 279", "clipboard saved"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q", order, want)
	}
	if client.clipboardData != "saved" {
		t.Errorf("clipboard after typing = %q, want the saved %q", client.clipboardData, "saved")
	}
}

func TestTypeKeysTwoPasteRunsInOrder(t *testing.T) {
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	d := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	if err := d.typeKeys("a🎉b😀"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	want := []string{
		"keys a", "clipboard 🎉", "keycode 279",
		"keys b", "clipboard 😀", "keycode 279",
		"clipboard saved",
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q", order, want)
	}
}

func TestTypeKeysSendKeyActionsErrorStopsAndRestores(t *testing.T) {
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	client.sendKeyActionsFunc = func(text string) error {
		if text == "b" {
			return errors.New("actions failed")
		}
		return nil
	}
	d := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	err := d.typeKeys("a🎉b😀")
	if err == nil || !strings.Contains(err.Error(), "actions failed") {
		t.Fatalf("typeKeys error = %v, want the key actions error", err)
	}
	want := []string{"keys a", "clipboard 🎉", "keycode 279", "keys b", "clipboard saved"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q (no later run, clipboard restored)", order, want)
	}
}

func TestTypeKeysSetClipboardErrorStops(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{clipboardData: "saved", setClipboardErr: errors.New("clipboard busy")}
	d := New(client, nil, nil)

	err := d.typeKeys("🎉 then text")
	if err == nil || !strings.Contains(err.Error(), "clipboard busy") {
		t.Fatalf("typeKeys error = %v, want the clipboard error", err)
	}
	if len(client.pressKeyCalls) != 0 {
		t.Errorf("pasted after the clipboard write failed: %v", client.pressKeyCalls)
	}
}

func TestTypeKeysPasteKeyErrorStops(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true, pressKeyErr: errors.New("no paste")}
	d := New(client, nil, nil)

	err := d.typeKeys("🎉")
	if err == nil || !strings.Contains(err.Error(), "no paste") {
		t.Fatalf("typeKeys error = %v, want the paste key error", err)
	}
	if client.clipboardData != "saved" {
		t.Errorf("clipboard = %q, want it restored to %q", client.clipboardData, "saved")
	}
}

func TestTypeKeysUnreadableClipboardIsCleared(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{clipboardErr: errors.New("not focused"), statefulClipboard: true}
	d := New(client, nil, nil)

	if err := d.typeKeys("pässwörd"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	// Nothing known to put back: the pasted fragments are not left behind.
	if want := []string{"ä", "ö", ""}; !reflect.DeepEqual(client.setClipboardCalls, want) {
		t.Errorf("clipboard writes = %q, want %q", client.setClipboardCalls, want)
	}
}

func TestTypeKeysRestoreFailureKeepsSuccess(t *testing.T) {
	withFastPaste(t)
	client := &failingRestoreClient{MockUIA2Client: &MockUIA2Client{clipboardData: "saved"}}
	d := New(client, nil, nil)

	if err := d.typeKeys("hi 🎉"); err != nil {
		t.Fatalf("typeKeys returned %v; a failed restore must not fail the typing", err)
	}
	if client.restores != 1 {
		t.Errorf("restore attempts = %d, want 1", client.restores)
	}
}

func TestInputTextFallbackPastesEmoji(t *testing.T) {
	// The issue's case: no focused element, so inputText falls back to key
	// events, which cannot carry the emoji.
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	driver := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	result := driver.inputText(&flow.InputTextStep{Text: "hello 🎉"})
	if !result.Success {
		t.Fatalf("inputText failed: %v", result.Error)
	}
	if want := []string{"keys hello ", "clipboard 🎉", "keycode 279", "clipboard saved"}; !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q", order, want)
	}
	if strings.Contains(result.Message, "non-ASCII") {
		t.Errorf("pasted text should carry no non-ASCII warning: %s", result.Message)
	}
}

func TestInputTextSendKeysFailureFallsBackToTypeKeys(t *testing.T) {
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{clipboardData: "saved", statefulClipboard: true}
	client.activeElementFunc = func() (*uiautomator2.Element, error) {
		el := fieldElement("")
		el.SetSendKeysFunc(func(string) error { return errors.New("cannot set text") })
		return el, nil
	}
	driver := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)

	result := driver.inputText(&flow.InputTextStep{Text: "🎉"})
	if !result.Success {
		t.Fatalf("inputText failed: %v", result.Error)
	}
	if want := []string{"clipboard 🎉", "keycode 279", "clipboard saved"}; !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q", order, want)
	}
}

func TestInputTextActiveElementNeverPastes(t *testing.T) {
	withFastPaste(t)
	var sent string
	client := &MockUIA2Client{}
	client.activeElementFunc = func() (*uiautomator2.Element, error) {
		el := fieldElement("")
		el.SetSendKeysFunc(func(text string) error { sent = text; return nil })
		return el, nil
	}
	client.sendKeyActionsFunc = func(string) error {
		t.Error("key actions sent although the focused element took the text")
		return nil
	}
	driver := New(client, nil, nil)

	result := driver.inputText(&flow.InputTextStep{Text: "hello 🎉"})
	if !result.Success {
		t.Fatalf("inputText failed: %v", result.Error)
	}
	if sent != "hello 🎉" {
		t.Errorf("element got %q", sent)
	}
	if len(client.setClipboardCalls) != 0 || len(client.pressKeyCalls) != 0 {
		t.Errorf("element send-keys must not paste: clipboard %q, codes %v", client.setClipboardCalls, client.pressKeyCalls)
	}
}

func TestInputTextKeyPressTypeKeysErrorFails(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{pressKeyErr: errors.New("no paste")}
	driver := New(client, nil, nil)

	result := driver.inputText(&flow.InputTextStep{Text: "🎉", KeyPress: true})
	if result.Success {
		t.Fatal("expected failure when the paste key fails")
	}
}

func TestSetClipboardEmojiInputTextPasteText(t *testing.T) {
	// A flow sets the clipboard, types an emoji through the key-event
	// fallback, then pastes: the paste must get the flow's value, not the
	// emoji typeKeys pasted on the way.
	withFastPaste(t)
	client := &MockUIA2Client{statefulClipboard: true}
	var pasted string
	inputting := true
	client.activeElementFunc = func() (*uiautomator2.Element, error) {
		el := fieldElement("")
		el.SetSendKeysFunc(func(text string) error {
			if inputting {
				return errors.New("cannot set text") // inputText falls back to typeKeys
			}
			pasted = text
			return nil
		})
		return el, nil
	}
	driver := New(client, nil, nil)

	if r := driver.setClipboard(&flow.SetClipboardStep{Text: "saved"}); !r.Success {
		t.Fatalf("setClipboard: %v", r.Error)
	}
	if r := driver.inputText(&flow.InputTextStep{Text: "hi 🎉"}); !r.Success {
		t.Fatalf("inputText: %v", r.Error)
	}
	inputting = false
	if r := driver.pasteText(&flow.PasteTextStep{}); !r.Success {
		t.Fatalf("pasteText: %v", r.Error)
	}
	if pasted != "saved" {
		t.Errorf("pasteText pasted %q, want the flow's %q", pasted, "saved")
	}
}

// orderedClient logs key actions, clipboard writes and key codes into one
// ordered list, so a test can check the sequence typeKeys produces.
type orderedClient struct {
	*MockUIA2Client
	order *[]string
}

func (c *orderedClient) SendKeyActions(text string) error {
	*c.order = append(*c.order, "keys "+text)
	return c.MockUIA2Client.SendKeyActions(text)
}

func (c *orderedClient) SetClipboard(text string) error {
	*c.order = append(*c.order, "clipboard "+text)
	return c.MockUIA2Client.SetClipboard(text)
}

func (c *orderedClient) PressKeyCode(code int) error {
	*c.order = append(*c.order, "keycode "+strconv.Itoa(code))
	return c.MockUIA2Client.PressKeyCode(code)
}

// failingRestoreClient fails every clipboard write after the first, which
// is the paste; the second is the restore.
type failingRestoreClient struct {
	*MockUIA2Client
	writes, restores int
}

func (c *failingRestoreClient) SetClipboard(text string) error {
	c.writes++
	if c.writes > 1 {
		c.restores++
		return errors.New("restore failed")
	}
	return c.MockUIA2Client.SetClipboard(text)
}

func TestClipboardForFlow(t *testing.T) {
	// A non-empty read is the truth.
	client := &MockUIA2Client{clipboardData: "from device"}
	d := New(client, nil, nil)
	d.flowClipboard, d.flowClipboardSet = "from flow", true
	if text, ok := d.clipboardForFlow(); !ok || text != "from device" {
		t.Errorf("readable clipboard: got %q, %v", text, ok)
	}

	// Android 10+: the read is empty, so the flow's own value stands in.
	client.clipboardData = ""
	if text, ok := d.clipboardForFlow(); !ok || text != "from flow" {
		t.Errorf("empty read: got %q, %v; want the flow's value", text, ok)
	}

	// A failed read falls back the same way.
	client.clipboardErr = errors.New("read failed")
	if text, ok := d.clipboardForFlow(); !ok || text != "from flow" {
		t.Errorf("failed read: got %q, %v; want the flow's value", text, ok)
	}

	// Nothing known.
	d.flowClipboardSet = false
	if _, ok := d.clipboardForFlow(); ok {
		t.Error("with no read and no flow value, clipboardForFlow should not be ok")
	}
}

func TestTypeKeysRestoresFlowClipboardWhenUnreadable(t *testing.T) {
	// Android 10+: the clipboard reads empty, but the flow set it, so the
	// paste puts the flow's value back.
	withFastPaste(t)
	client := &MockUIA2Client{} // reads ""; writes are not readable back
	d := New(client, nil, nil)
	if r := d.setClipboard(&flow.SetClipboardStep{Text: "saved"}); !r.Success {
		t.Fatalf("setClipboard: %v", r.Error)
	}

	if err := d.typeKeys("🎉"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	if want := []string{"saved", "🎉", "saved"}; !reflect.DeepEqual(client.setClipboardCalls, want) {
		t.Errorf("clipboard writes = %q, want %q", client.setClipboardCalls, want)
	}
}

func TestTypeKeysNothingKnownClearsTheClipboard(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{} // reads "", and the flow never set it
	d := New(client, nil, nil)

	if err := d.typeKeys("🎉"); err != nil {
		t.Fatalf("typeKeys: %v", err)
	}
	if want := []string{"🎉", ""}; !reflect.DeepEqual(client.setClipboardCalls, want) {
		t.Errorf("clipboard writes = %q, want the paste then a clear", client.setClipboardCalls)
	}
}

func TestCopyTextFromRemembersTheClipboard(t *testing.T) {
	client := &MockUIA2Client{}
	client.findElementFunc = func(strategy, selector string) (*uiautomator2.Element, error) {
		return fieldElement("copied text"), nil
	}
	d := New(client, nil, nil)
	r := d.copyTextFrom(&flow.CopyTextFromStep{Selector: flow.Selector{ID: "label"}})
	if !r.Success {
		t.Fatalf("copyTextFrom: %v", r.Error)
	}
	if text, ok := d.clipboardForFlow(); !ok || text != "copied text" {
		t.Errorf("clipboardForFlow after copyTextFrom = %q, %v", text, ok)
	}
}

func TestPasteRunNeverReadsTheField(t *testing.T) {
	// Reading a Chrome password field while it handles a paste corrupts it,
	// so a paste only writes the clipboard, presses paste and waits.
	withFastPaste(t)
	pasteSettleWait = 30 * time.Millisecond
	client := &MockUIA2Client{}
	client.activeElementFunc = func() (*uiautomator2.Element, error) {
		t.Error("pasteRun read the focused field")
		return fieldElement(""), nil
	}
	d := New(client, nil, nil)

	start := time.Now()
	if err := d.pasteRun("🎉"); err != nil {
		t.Fatalf("pasteRun: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("pasteRun took %v, want the settle wait", elapsed)
	}
	if !reflect.DeepEqual(client.setClipboardCalls, []string{"🎉"}) || !reflect.DeepEqual(client.pressKeyCalls, []int{keyCodePaste}) {
		t.Errorf("clipboard %q, key codes %v", client.setClipboardCalls, client.pressKeyCalls)
	}
}

// focusedRefusing returns an ActiveElement func whose element refuses
// send-keys, and records nothing.
func focusedRefusing() func() (*uiautomator2.Element, error) {
	return func() (*uiautomator2.Element, error) {
		el := fieldElement("")
		el.SetSendKeysFunc(func(string) error { return errors.New("cannot set text") })
		return el, nil
	}
}

func TestPasteTextUnreadableClipboardPressesPasteKey(t *testing.T) {
	// Android 10+: the read is empty, so the paste key pastes whatever the
	// device clipboard holds, including what the app copied itself.
	var sent []string
	client := &MockUIA2Client{} // reads ""
	client.activeElementFunc = func() (*uiautomator2.Element, error) {
		el := fieldElement("")
		el.SetSendKeysFunc(func(text string) error { sent = append(sent, text); return nil })
		return el, nil
	}
	d := New(client, nil, nil)

	r := d.pasteText(&flow.PasteTextStep{})
	if !r.Success || !strings.Contains(r.Message, "paste key") {
		t.Fatalf("pasteText = %+v", r)
	}
	if !reflect.DeepEqual(client.pressKeyCalls, []int{keyCodePaste}) || len(sent) != 0 {
		t.Errorf("key codes %v, send-keys %q; want only KEYCODE_PASTE", client.pressKeyCalls, sent)
	}

	client.pressKeyErr = errors.New("no paste")
	if r := d.pasteText(&flow.PasteTextStep{}); r.Success {
		t.Error("a failing paste key should fail pasteText")
	}
}

func TestPasteTextRefusedSendKeysTypesTheText(t *testing.T) {
	// A focused element refuses send-keys: the text goes out as key events,
	// pasting what keys cannot type, and the clipboard is put back.
	withFastPaste(t)
	var order []string
	client := &MockUIA2Client{statefulClipboard: true}
	client.activeElementFunc = focusedRefusing()
	d := New(&orderedClient{MockUIA2Client: client, order: &order}, nil, nil)
	d.setClipboard(&flow.SetClipboardStep{Text: "saved 🎉"})
	order = nil

	r := d.pasteText(&flow.PasteTextStep{})
	if !r.Success || r.Message != "Pasted text: saved 🎉" {
		t.Fatalf("pasteText = %+v", r)
	}
	if want := []string{"keys saved ", "clipboard 🎉", "keycode 279", "clipboard saved 🎉"}; !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want %q", order, want)
	}
}

func TestPasteTextRefusedSendKeysKeyEventsFail(t *testing.T) {
	withFastPaste(t)
	client := &MockUIA2Client{clipboardData: "saved"}
	client.activeElementFunc = focusedRefusing()
	client.sendKeyActionsFunc = func(string) error { return errors.New("actions failed") }
	d := New(client, nil, nil)

	r := d.pasteText(&flow.PasteTextStep{})
	if r.Success || !strings.Contains(r.Message, "actions failed") {
		t.Errorf("pasteText = %+v, want the key events failure", r)
	}
}

func TestPasteTextNilActiveElement(t *testing.T) {
	client := &MockUIA2Client{clipboardData: "saved"}
	client.activeElementFunc = func() (*uiautomator2.Element, error) { return nil, nil }
	if r := New(client, nil, nil).pasteText(&flow.PasteTextStep{}); r.Success || r.Message != "No focused element to paste into" {
		t.Errorf("pasteText = %+v", r)
	}
}

func TestPastedNote(t *testing.T) {
	if got := pastedNote("plain ascii\n"); got != "" {
		t.Errorf("ASCII note = %q, want none", got)
	}
	if got := pastedNote("hi 🎉"); !strings.Contains(got, "clipboard overlay") {
		t.Errorf("emoji note = %q", got)
	}
}
