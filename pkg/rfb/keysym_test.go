package rfb

import "testing"

func TestKeysymForName(t *testing.T) {
	cases := map[string]uint32{
		"Enter": 0xff0d, "enter": 0xff0d, "RETURN": 0xff0d, "Backspace": 0xff08, "Delete": 0xffff,
		"Tab": 0xff09, "Escape": 0xff1b, "Space": 0x20, "Home": 0xff50, "End": 0xff57,
		"PageUp": 0xff55, "page_down": 0xff56, "Left": 0xff51, "Up": 0xff52, "Right": 0xff53, "Down": 0xff54,
		"Remote Dpad Up": 0xff52, "F1": 0xffbe, "F12": 0xffc9,
		"Shift": KeyShift, "Ctrl": KeyControl, "Control": KeyControl, "Alt": KeyAlt, "Option": KeyAlt,
		"Meta": KeyMeta, "Cmd": KeyMeta, "Command": KeyMeta,
		"a": 'a', "A": 'A', "~": '~', "é": 0xe9, "€": 0x010020ac,
	}
	for name, want := range cases {
		got, err := KeysymForName(name)
		if err != nil || got != want {
			t.Errorf("%q: %#x %v, want %#x", name, got, err, want)
		}
	}
	for _, bad := range []string{"", "volume up", "ab"} {
		if _, err := KeysymForName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestKeysymForRune(t *testing.T) {
	for r, want := range map[rune]uint32{'\n': 0xff0d, '\r': 0xff0d, '\t': 0xff09, ' ': 0x20, 'ÿ': 0xff, 'Ā': 0x01000100, '😀': 0x0101f600} {
		if got := KeysymForRune(r); got != want {
			t.Errorf("%q: %#x want %#x", r, got, want)
		}
	}
}

func TestParseCombo(t *testing.T) {
	c, err := ParseCombo("Meta+Shift+K")
	if err != nil || len(c.Modifiers) != 2 || c.Modifiers[0] != KeyMeta || c.Modifiers[1] != KeyShift || c.Key != 'K' {
		t.Errorf("%+v %v", c, err)
	}
	if c, err := ParseCombo("Enter"); err != nil || len(c.Modifiers) != 0 || c.Key != 0xff0d {
		t.Errorf("%+v %v", c, err)
	}
	if c, err := ParseCombo("+"); err != nil || c.Key != '+' {
		t.Errorf("plus %+v %v", c, err)
	}
	if c, err := ParseCombo("Shift++"); err != nil || c.Key != '+' || len(c.Modifiers) != 1 {
		t.Errorf("shift plus %+v %v", c, err)
	}
	for _, bad := range []string{"", "  ", "K+Shift", "Meta++Shift", "Meta+", "Hyper+K", "Meta+nope"} {
		if _, err := ParseCombo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
