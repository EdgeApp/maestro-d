package hubd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// fixture dispatches driver operations to the hooks chosen by each test.
// The host and hub use the same registry, event bus and HTTP handlers as the binary.
type fixture struct {
	*Server
	boot func(string, string, time.Duration) (string, error)
	stop func(daemon.DeviceInfo) error
}

func (f *fixture) bind(t *testing.T, cfg daemon.Config, deps daemon.Deps) {
	t.Helper()
	host := deps
	host.NewDriver = func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
		return f.deps.NewDriver(ctx, id, cfg)
	}
	host.InstallApp = func(ctx context.Context, id, platform, path string) error {
		return f.deps.InstallApp(ctx, id, platform, path)
	}
	host.DeleteAVD = func(name string) error { return f.deps.DeleteAVD(name) }
	host.DeleteSimulator = func(id string) error { return f.deps.DeleteSimulator(id) }
	host.BootDevice = func(platform, name string, timeout time.Duration) (string, error) {
		return f.boot(platform, name, timeout)
	}
	host.ShutdownDevice = func(d daemon.DeviceInfo) error { return f.stop(d) }
	host.NewRemoteDriver = func(ctx context.Context, endpoint string, caps map[string]any) (daemon.RemoteDriver, daemon.DriverInfo, error) {
		return f.deps.NewRemoteDriver(ctx, endpoint, caps)
	}
	// An empty provider map is shared with tests which enable a cloud provider.
	deps.Providers = make(map[string]func() (cloudprov.RemoteProvider, error))
	host.Providers = deps.Providers
	server := daemon.New(cfg, host)
	f.Server.Server = server
	f.deps = deps
	t.Cleanup(f.close)
}

func (f *fixture) close() {
	f.Server.Server.Shutdown("test")
	<-f.Done()
}

func (f *fixture) held(id string) bool {
	_, ok := f.Registry().Record(id)
	return ok
}

type plainEnv struct {
	s *fixture
	c *daemon.Client
	t *testing.T
}

func newTestEnv(t *testing.T, delay time.Duration) *plainEnv {
	t.Helper()
	t.Setenv(daemon.EnvHome, t.TempDir())
	var cleanups int32
	f := &fixture{Server: &Server{}}
	f.bind(t, daemon.Config{Name: "plain"}, testDeps(&cleanups, delay))
	server := httptest.NewServer(f.Handler())
	t.Cleanup(server.Close)
	return &plainEnv{s: f, c: daemon.NewHTTPClient(server.URL, "", nil), t: t}
}

func (e *plainEnv) attach(id string) {
	e.t.Helper()
	if _, err := e.c.Attach(context.Background(), id, daemon.AttachConfig{Platform: "mock"}); err != nil {
		e.t.Fatal(err)
	}
}

func atomicWriteJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func recordCreated(record daemon.CreatedRecord) error {
	record.CreatedBy = daemon.CreatedByMaestroD
	var records []daemon.CreatedRecord
	path := daemon.CreatedPath()
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &records); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicWriteJSON(path, append(records, record))
}

func errorFromResponse(status int, header http.Header, body []byte) *daemon.Error {
	var response daemon.Envelope
	if err := json.Unmarshal(body, &response); err != nil || response.Error == nil {
		return daemon.Errorf(daemon.CodeInternal, "HTTP %d: %s", status, body)
	}
	return daemon.Errorf(response.Error.Code, "%s", response.Error.Message)
}
