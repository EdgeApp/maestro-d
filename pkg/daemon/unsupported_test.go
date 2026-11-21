package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

func TestResultFromOutcome_Code(t *testing.T) {
	failed := func(code string) *executor.StepOutcome {
		return &executor.StepOutcome{Type: "x", Error: &report.Error{Message: "no"}, Code: code}
	}
	for _, c := range []struct {
		o    *executor.StepOutcome
		want Code
	}{
		{failed(""), CodeCommandFailed},
		{failed("UNSUPPORTED"), CodeUnsupported},
		{failed("USAGE"), CodeUsage},
		{func() *executor.StepOutcome { o := failed("UNSUPPORTED"); o.Panicked = true; return o }(), CodeDeviceError},
		{func() *executor.StepOutcome { o := failed("UNSUPPORTED"); o.Cancelled = true; return o }(), CodeInterrupted},
	} {
		r := ResultFromOutcome("d", "", c.o)
		if r.Error == nil || r.Error.Code != c.want || r.ExitCode != c.want.ExitCode() {
			t.Errorf("code %q: %+v", c.o.Code, r)
		}
	}
}

// An iOS device's airplane-mode steps answer UNSUPPORTED (exit 2, HTTP 501)
// on every surface, without reaching the driver.
func TestServer_IOSAirplaneModeIsUnsupported(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var cleanups int32
	deps := testDeps(&cleanups, 0)
	base := deps.NewDriver
	deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		_, info, cleanup, err := base(ctx, id, cfg)
		return mock.New(mock.Config{Platform: "ios", DeviceID: id}), info, cleanup, err
	}
	s := New(Config{Name: "test-ios-airplane", ReportsDir: filepath.Join(t.TempDir(), "reports")}, deps)
	s.info = Info{Name: s.cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	c := NewHTTPClient(hs.URL, "", nil)
	ctx := context.Background()
	if _, err := c.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []struct {
		name  string
		value any
	}{{"setAirplaneMode", "enabled"}, {"toggleAirplaneMode", nil}} {
		res, err := c.Command(ctx, "mock-1", cmd.name, cmd.value, true)
		if codeOf(t, err) != CodeUnsupported || res == nil || res.ExitCode != 2 ||
			!strings.Contains(res.Error.Message, "iOS has no airplane mode in maestro-d") {
			t.Fatalf("%s: %v %+v", cmd.name, err, res)
		}
	}
	resp, err := http.Post(hs.URL+"/v1/devices/mock-1/commands/toggleAirplaneMode", "application/json", strings.NewReader("null"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
