package executor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

// ensuringDriver is the mock driver plus core.SessionEnsurer, recording what
// the session asked for. This is the WDA driver's shape: interactive commands
// only work inside a session, and a session that names an app launches it.
type ensuringDriver struct {
	*mock.Driver
	calls []string
	err   error
}

func (d *ensuringDriver) EnsureSession(appID string) error {
	d.calls = append(d.calls, appID)
	return d.err
}

func TestNewSessionEnsuresSessionWithoutLaunchingApp(t *testing.T) {
	drv := &ensuringDriver{Driver: mock.New(mock.Config{})}
	s, err := NewSession(context.Background(), drv, RunnerConfig{OutputDir: t.TempDir()},
		SessionConfig{AppID: "com.example.app"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	if len(drv.calls) != 1 {
		t.Fatalf("EnsureSession calls = %v, want exactly one", drv.calls)
	}
	if drv.calls[0] != "" {
		t.Fatalf("EnsureSession(%q): attach must not name an app, or the driver launches it", drv.calls[0])
	}
}

func TestNewSessionFailsWhenSessionCannotBeCreated(t *testing.T) {
	drv := &ensuringDriver{Driver: mock.New(mock.Config{}), err: errors.New("WDA is down")}
	s, err := NewSession(context.Background(), drv, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
	if err == nil {
		s.Close()
		t.Fatal("NewSession succeeded without a driver session; interactive commands would all fail")
	}
	if !strings.Contains(err.Error(), "WDA is down") {
		t.Fatalf("error %q does not carry the driver's reason", err)
	}
}
