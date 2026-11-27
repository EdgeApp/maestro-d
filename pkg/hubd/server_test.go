package hubd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// testDeps builds Deps backed by the mock driver. cleanups counts driver
// teardowns so tests can assert detach/shutdown closed them.
func testDeps(cleanups *int32, stepDelay time.Duration) daemon.Deps {
	return daemon.Deps{
		Version: "test",
		NewDriver: func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
			if strings.HasPrefix(id, "bad-") {
				return nil, daemon.DriverInfo{}, nil, errors.New("no such device")
			}
			d := mock.New(mock.Config{Platform: "mock", DeviceID: id, StepDelay: stepDelay})
			info := daemon.DriverInfo{
				DriverName: "mock",
				Device:     report.Device{ID: id, Platform: "mock"},
				App:        report.App{ID: cfg.AppID},
				Platform:   "mock", Kind: "device", Name: "Mock " + id,
			}
			return d, info, func() { atomic.AddInt32(cleanups, 1) }, nil
		},
		ListDevices: func(platform string) []daemon.DeviceInfo {
			if platform != "" && platform != "mock" {
				return nil
			}
			return []daemon.DeviceInfo{{ID: "mock-1", Platform: "mock", Kind: "device", Ready: true, State: "Booted"}}
		},
	}
}

func codeOf(t *testing.T, err error) daemon.Code {
	t.Helper()
	var de *daemon.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	return de.Code
}
