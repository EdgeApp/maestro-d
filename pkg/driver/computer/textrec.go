package computer

import (
	"context"
	"encoding/json"
	"fmt"
)

// TextBox is one line of text recognised on the screen. X, Y, W and H are
// fractions of the screen, origin top-left.
type TextBox struct {
	Text string  `json:"text"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w"`
	H    float64 `json:"h"`
}

// CenterX and CenterY are the box's centre as fractions of the screen.
func (b TextBox) CenterX() float64 { return b.X + b.W/2 }
func (b TextBox) CenterY() float64 { return b.Y + b.H/2 }

// Recognizer reads the text on a screen image (PNG or JPEG).
type Recognizer interface {
	Recognize(ctx context.Context, image []byte) ([]TextBox, error)
}

// RecognizerFunc adapts a function to Recognizer.
type RecognizerFunc func(ctx context.Context, image []byte) ([]TextBox, error)

func (f RecognizerFunc) Recognize(ctx context.Context, image []byte) ([]TextBox, error) {
	return f(ctx, image)
}

func parseBoxes(out []byte) ([]TextBox, error) {
	var boxes []TextBox
	if err := json.Unmarshal(out, &boxes); err != nil {
		return nil, fmt.Errorf("text recognition output: %w", err)
	}
	return boxes, nil
}
