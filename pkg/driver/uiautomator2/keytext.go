package uiautomator2

import (
	"fmt"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// keyCodePaste is Android's KEYCODE_PASTE. A focused TextView and Chrome or
// WebView content both paste the clipboard when they receive it.
const keyCodePaste = 279

// pasteSettleWait is how long typeKeys lets a paste land before it writes
// the clipboard again or sends more keys. The app reads the clipboard when it
// handles the paste key, asynchronously in Chrome and WebViews. The focused
// field is not read to see the paste land: Chrome's <input> fields read ""
// through UiAutomator2, and reading a password field while Chrome handles a
// paste moves its caret and splits the pasted characters. A variable so tests
// can shorten it.
var pasteSettleWait = 300 * time.Millisecond

// textRun is a stretch of text that is typed one way: as key events, or by
// pasting it through the clipboard.
type textRun struct {
	text  string
	paste bool
}

// keyTypeable reports whether the UiAutomator2 server can type r as key
// events. It builds them from Android's virtual-keyboard KeyCharacterMap,
// which maps printable ASCII, newline and tab, and maps the W3C special keys
// (U+E000–U+E05D: Enter, Backspace, arrows, …) to key codes; for anything
// else (emoji, accented letters, CJK) it fails with "KeyCharacterMap.getEvents
// is unable to synthesize KeyEvent sequence", after typing what came before.
func keyTypeable(r rune) bool {
	return r == '\n' || r == '\t' || (r >= 0x20 && r <= 0x7e) || (r >= 0xe000 && r <= 0xe05d)
}

// splitKeyTypeable splits text into consecutive runs that are each wholly
// key-typeable or wholly not. Joined in order, the runs are the text.
func splitKeyTypeable(text string) []textRun {
	var runs []textRun
	for _, r := range text {
		paste := !keyTypeable(r)
		if n := len(runs); n > 0 && runs[n-1].paste == paste {
			runs[n-1].text += string(r)
			continue
		}
		runs = append(runs, textRun{text: string(r), paste: paste})
	}
	return runs
}

// typeKeys types text into whatever holds focus, without element send-keys.
// Key-typeable runs go out as W3C key actions; every other run is put on the
// clipboard and pasted with KEYCODE_PASTE. The text is split before anything
// is sent, so no character the key map lacks ever reaches the key actions.
//
// A paste overwrites the clipboard, so the value it held for the flow before
// the first paste (clipboardForFlow) is written back at the end, also after a
// failure, and a flow's setClipboard or copyTextFrom value is still what a
// later pasteText gets. When that value is unknown, the clipboard is cleared.
func (d *Driver) typeKeys(text string) error {
	var saved string
	readClipboard := false
	// With nothing to put back, the clipboard is cleared rather than left
	// holding a pasted fragment, which may be part of a password.
	defer func() {
		if !readClipboard {
			return
		}
		if err := d.client.SetClipboard(saved); err != nil {
			logger.Warn("inputText: could not restore the clipboard after pasting: %v", err)
		}
	}()

	for _, run := range splitKeyTypeable(text) {
		if !run.paste {
			if err := d.client.SendKeyActions(run.text); err != nil {
				return err
			}
			continue
		}
		if !readClipboard {
			readClipboard = true
			saved, _ = d.clipboardForFlow()
		}
		if err := d.pasteRun(run.text); err != nil {
			return err
		}
	}
	return nil
}

// pasteRun pastes run into the focused field and lets the paste land, so the
// next clipboard write cannot be the one the app reads.
func (d *Driver) pasteRun(run string) error {
	if err := d.client.SetClipboard(run); err != nil {
		return fmt.Errorf("put %q on the clipboard to paste it: %w", run, err)
	}
	if err := d.client.PressKeyCode(keyCodePaste); err != nil {
		return fmt.Errorf("paste %q: %w", run, err)
	}
	time.Sleep(pasteSettleWait)
	return nil
}

// clipboardForFlow returns the value typeKeys puts back on the clipboard: a
// non-empty read, else the text the flow last put there. ok is false when
// neither is known. On Android 10 and later only the focused app and the
// keyboard may read the clipboard, so UiAutomator2's read comes back empty
// and a value the app copied itself since the flow's setClipboard or
// copyTextFrom is not known: the flow's value replaces it.
func (d *Driver) clipboardForFlow() (string, bool) {
	if text, err := d.client.GetClipboard(); err == nil && text != "" {
		return text, true
	}
	if d.flowClipboardSet {
		return d.flowClipboard, true
	}
	return "", false
}

// pastedNote is the remark an inputText result carries when typeKeys pasted
// part of text: the clipboard overlay Android 13 and later raise takes the
// taps aimed under it for a few seconds.
func pastedNote(text string) string {
	for _, r := range text {
		if !keyTypeable(r) {
			return " (pasted what the key map cannot type; Android 13+ shows the clipboard overlay at the top left for a few seconds)"
		}
	}
	return ""
}
