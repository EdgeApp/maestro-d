//go:build !darwin

package quicktime

import (
	"context"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
)

// U45: off macOS a device never captures, and says why.
func TestStub_NotMacOS(t *testing.T) {
	d := Open("00008110-EXAMPLE000000001", "iPhone")
	if d.Session() != nil {
		t.Fatal("stub has no session")
	}
	res := d.Wait(context.Background())
	if capture.ReasonOf(res.Err, "") != capture.ReasonNotMacOS || res.Found {
		t.Fatalf("stub result: %+v", res)
	}
	d.Close()
}
