package rfb

import (
	"fmt"
	"strings"
)

// Modifier keysyms. Command is Super_L: Apple's server maps it to the
// Command key.
const (
	KeyShift   uint32 = 0xffe1
	KeyControl uint32 = 0xffe3
	KeyAlt     uint32 = 0xffe9
	KeyMeta    uint32 = 0xffeb
)

var modifierKeys = map[string]uint32{
	"shift":   KeyShift,
	"control": KeyControl,
	"ctrl":    KeyControl,
	"alt":     KeyAlt,
	"option":  KeyAlt,
	"meta":    KeyMeta,
	"cmd":     KeyMeta,
	"command": KeyMeta,
}

// namedKeys holds Maestro's pressKey names (matched without case or
// separators, so "Enter", "enter" and "ENTER" are one key) and the usual
// spellings of the same keys.
var namedKeys = map[string]uint32{
	"enter":     0xff0d,
	"return":    0xff0d,
	"backspace": 0xff08,
	"delete":    0xffff,
	"tab":       0xff09,
	"escape":    0xff1b,
	"esc":       0xff1b,
	"space":     0x20,
	"home":      0xff50,
	"end":       0xff57,
	"pageup":    0xff55,
	"pagedown":  0xff56,
	"left":      0xff51,
	"up":        0xff52,
	"right":     0xff53,
	"down":      0xff54,
	"leftarrow": 0xff51, "uparrow": 0xff52, "rightarrow": 0xff53, "downarrow": 0xff54,
	"remotedpadleft": 0xff51, "remotedpadup": 0xff52, "remotedpadright": 0xff53, "remotedpaddown": 0xff54,
}

func init() {
	for i := 1; i <= 12; i++ {
		namedKeys[fmt.Sprintf("f%d", i)] = 0xffbe + uint32(i-1)
	}
}

func normalizeKey(name string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(name)))
}

// KeysymForRune is the keysym that types r: Latin-1 characters are their own
// keysym, newline and tab are Return and Tab, anything else is Unicode's
// 0x01000000 + code point.
func KeysymForRune(r rune) uint32 {
	switch {
	case r == '\n' || r == '\r':
		return 0xff0d
	case r == '\t':
		return 0xff09
	case r >= 0x20 && r <= 0xff:
		return uint32(r)
	default:
		return 0x01000000 + uint32(r)
	}
}

// KeysymForName resolves one key: a modifier, a named key, or a single
// character.
func KeysymForName(name string) (uint32, error) {
	n := normalizeKey(name)
	if k, ok := modifierKeys[n]; ok {
		return k, nil
	}
	if k, ok := namedKeys[n]; ok {
		return k, nil
	}
	if r := []rune(strings.TrimSpace(name)); len(r) == 1 {
		return KeysymForRune(r[0]), nil
	}
	return 0, fmt.Errorf("unknown key %q", name)
}

// Combo is a key press with the modifiers held around it.
type Combo struct {
	Modifiers []uint32 // pressed in order, released in reverse
	Key       uint32
}

// ParseCombo reads "Meta+Shift+K", "Enter" or "a". Every part but the last
// must be a modifier; "+" on its own names the plus key.
func ParseCombo(s string) (Combo, error) {
	if strings.TrimSpace(s) == "" {
		return Combo{}, fmt.Errorf("empty key")
	}
	if strings.TrimSpace(s) == "+" {
		return Combo{Key: '+'}, nil
	}
	parts := strings.Split(s, "+")
	if strings.HasSuffix(s, "++") { // "Shift++": the last key is "+"
		parts = append(parts[:len(parts)-2], "+")
	}
	var c Combo
	for i, p := range parts {
		if strings.TrimSpace(p) == "" {
			return Combo{}, fmt.Errorf("empty key in %q", s)
		}
		last := i == len(parts)-1
		if !last {
			m, ok := modifierKeys[normalizeKey(p)]
			if !ok {
				return Combo{}, fmt.Errorf("%q in %q is not a modifier (Shift, Control, Alt/Option, Meta/Cmd)", p, s)
			}
			c.Modifiers = append(c.Modifiers, m)
			continue
		}
		k, err := KeysymForName(p)
		if err != nil {
			return Combo{}, err
		}
		c.Key = k
	}
	return c, nil
}
