package flutter

// maestro-d addition: core.Unwrap reaches a capture-capable driver through
// the Flutter wrapper, as the stream route and the executor's recording
// steps do.

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

func TestFlutterWrap_UnwrapReachesCapturer(t *testing.T) {
	inner := mock.New(mock.Config{})
	defer inner.Close()
	wrapped := Wrap(inner, nil, nil, "com.example", "")
	if _, ok := wrapped.(core.Capturer); ok {
		t.Fatal("FlutterDriver forwards Capturer; the unwrap is untested")
	}
	c, ok := core.Unwrap(wrapped).(core.Capturer)
	if !ok {
		t.Fatal("core.Unwrap does not reach the mock's Capturer")
	}
	if s, err := c.Capture(); err != nil || s == nil {
		t.Fatalf("Capture: %v %v", s, err)
	}
}
