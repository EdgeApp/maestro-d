package flow

// maestro-d: mouse steps for the computer device (pkg/driver/computer), a
// Mac driven over its own Screen Sharing. Other drivers reach their default
// case and answer that the step is not supported.

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// MoveMouseStep moves the computer's pointer to a point, keeping any held button down.
type MoveMouseStep struct {
	BaseStep `yaml:",inline"`
	Point    string `yaml:"point"` // "x, y" in screen pixels or "x%, y%"
}

// ClickOnStep clicks a mouse button on the computer, optionally after moving to a point.
type ClickOnStep struct {
	BaseStep `yaml:",inline"`
	Point    string `yaml:"point"`  // "x, y" in screen pixels or "x%, y%"; omitted clicks where the pointer is
	Button   string `yaml:"button"` // left (default), right or middle
	Count    int    `yaml:"count"`  // 1 (default) to 3: a double or triple click
}

// ScrollAtStep turns the computer's scroll wheel at a point.
type ScrollAtStep struct {
	BaseStep `yaml:",inline"`
	Point    string `yaml:"point"` // "x, y" in screen pixels or "x%, y%"
	X        int    `yaml:"x"`     // Horizontal wheel ticks, -50 to 50; positive scrolls right
	Y        int    `yaml:"y"`     // Vertical wheel ticks, -50 to 50; positive scrolls down
}

// ClickButtons are the buttons clickOn accepts.
var ClickButtons = []string{"left", "right", "middle"}

// validateDesktopStep checks the mouse steps' values after decoding and
// fills their defaults.
func validateDesktopStep(step Step, node *yaml.Node, sourcePath string) (Step, error) {
	var err error
	switch s := step.(type) {
	case *MoveMouseStep:
		if strings.TrimSpace(s.Point) == "" {
			err = fmt.Errorf("moveMouse needs a point")
		}
	case *ClickOnStep:
		s.Button = strings.ToLower(strings.TrimSpace(s.Button))
		if s.Button == "" {
			s.Button = "left"
		}
		if s.Count == 0 && !hasKey(node, "count") {
			s.Count = 1
		}
		switch {
		case !slices.Contains(ClickButtons, s.Button):
			err = fmt.Errorf("clickOn button %q: use left, right or middle", s.Button)
		case s.Count < 1 || s.Count > 3:
			err = fmt.Errorf("clickOn count %d: use 1, 2 or 3", s.Count)
		}
	case *ScrollAtStep:
		switch {
		case strings.TrimSpace(s.Point) == "":
			err = fmt.Errorf("scrollAt needs a point")
		case s.X == 0 && s.Y == 0:
			err = fmt.Errorf("scrollAt needs x or y ticks")
		case s.X < -50 || s.X > 50 || s.Y < -50 || s.Y > 50:
			err = fmt.Errorf("scrollAt ticks must be between -50 and 50")
		}
	}
	if err != nil {
		return nil, wrapParseError(sourcePath, node.Line, err)
	}
	return step, nil
}

// hasKey reports whether a mapping node sets key, so an explicit 0 is told
// from an omitted field.
func hasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}
