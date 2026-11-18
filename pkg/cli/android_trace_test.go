package cli

import (
	"path/filepath"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/tracelog"
)

func TestTraceAndroidClientOnlyWhenAsked(t *testing.T) {
	var got string
	set := func(p string) { got = p }

	t.Setenv(tracelog.EnvVar, "")
	traceAndroidClient(set, "/report")
	if got != "" {
		t.Fatalf("trace set without %s: %q", tracelog.EnvVar, got)
	}
	t.Setenv(tracelog.EnvVar, "1")
	traceAndroidClient(set, "/report")
	if got != filepath.Join("/report", "client.log") {
		t.Errorf("trace path = %q", got)
	}
}
