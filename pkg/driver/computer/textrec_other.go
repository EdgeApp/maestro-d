//go:build !darwin

package computer

import (
	"context"
	"errors"
)

// VisionRecognizer needs Apple's Vision framework; the computer device only
// exists on macOS.
type VisionRecognizer struct{}

func (VisionRecognizer) Recognize(context.Context, []byte) ([]TextBox, error) {
	return nil, errors.New("text recognition needs macOS")
}
