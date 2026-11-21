package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// shaperServer records every call a daemon's traffic shaper makes.
type shaperServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string // "METHOD path body"
}

func newShaperServer(t *testing.T) *shaperServer {
	t.Helper()
	ss := &shaperServer{}
	ss.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ss.mu.Lock()
		ss.reqs = append(ss.reqs, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		ss.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ss.Close)
	return ss
}

func (ss *shaperServer) calls() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return append([]string(nil), ss.reqs...)
}

// writeNetworkConfig configures mock-1 (only) against the shaper server.
func writeNetworkConfig(t *testing.T, path, base string) {
	t.Helper()
	cfg := map[string]any{"devices": map[string]any{"mock-1": map[string]any{
		"setTrafficShape": map[string]any{"method": "POST", "url": base + "/shape/${deviceId}",
			"body":            map[string]any{"mbit": "${mbit}", "lossPct": "${lossPct}", "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}"},
			"directionValues": map[string]string{"download": "down", "upload": "up"}},
		"resetTrafficShape": map[string]any{"method": "DELETE", "url": base + "/shape/${deviceId}"},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServer_TrafficShaping(t *testing.T) {
	env := newTestEnv(t, 0)
	ss := newShaperServer(t)
	cfgPath := filepath.Join(Home(), "network.json")
	writeNetworkConfig(t, cfgPath, ss.URL)
	ctx := context.Background()

	info := env.attach("mock-1")
	if !info.TrafficShaping {
		t.Fatalf("attach answer: %+v", info)
	}
	env.attach("mock-2")

	res, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"mbit": 1.5, "latencyMs": 100, "direction": "download"}, true)
	if err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := env.c.Command(ctx, "mock-1", "resetTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /shape/mock-1 {"direction":"down","jitterMs":0,"latencyMs":100,"lossPct":0,"mbit":1.5}`,
		"DELETE /shape/mock-1",
	}
	if got := ss.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// An unconfigured device: UNSUPPORTED, and no call.
	res, err = env.c.Command(ctx, "mock-2", "setTrafficShape", map[string]any{"lossPct": 100}, true)
	if codeOf(t, err) != CodeUnsupported || res.ExitCode != 2 || res.Error.Message != "no traffic shaper is configured for mock-2 on this host" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(ss.calls()) != 2 {
		t.Fatalf("an unconfigured device made a call: %v", ss.calls())
	}

	// Listings: mock-1 only, in the list and on its own.
	devs, err := env.c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	shaping := map[string]bool{}
	for _, d := range devs {
		shaping[d.ID] = d.TrafficShaping
	}
	if !shaping["mock-1"] || shaping["mock-2"] || len(shaping) < 2 {
		t.Fatalf("listing: %v", shaping)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || !d.TrafficShaping {
		t.Fatalf("device: %v %+v", err, d)
	}

	// Bad values never reach the shaper.
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 101}, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("lossPct 101: %v", err)
	}

	// The file is read on every call: an edit takes effect, and a broken
	// file is USAGE naming it.
	if err := os.WriteFile(cfgPath, []byte(`{"devices":`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = env.c.Command(ctx, "mock-1", "setTrafficShape", nil, true)
	if codeOf(t, err) != CodeUsage || !strings.Contains(res.Error.Message, "network config "+cfgPath) {
		t.Fatalf("%v %+v", err, res)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || d.TrafficShaping {
		t.Fatalf("a broken file still lists shaping: %v %+v", err, d)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := env.c.Command(ctx, "mock-1", "resetTrafficShape", nil, true); codeOf(t, err) != CodeUnsupported {
		t.Fatalf("no file: %v", err)
	}
	if len(ss.calls()) != 2 {
		t.Fatalf("calls: %v", ss.calls())
	}
}

func TestServer_NetworkConfigFlag(t *testing.T) {
	env := newTestEnv(t, 0)
	ss := newShaperServer(t)
	path := filepath.Join(t.TempDir(), "shapes.json")
	writeNetworkConfig(t, path, ss.URL)
	env.s.cfg.NetworkConfig = path
	if got := env.s.networkConfigPath(); got != path {
		t.Fatalf("path = %s", got)
	}
	env.attach("mock-1")
	if _, err := env.c.Command(context.Background(), "mock-1", "resetTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	if len(ss.calls()) != 1 {
		t.Fatalf("calls: %v", ss.calls())
	}
	env.s.markTrafficShaping(nil) // nothing to mark is not an error
}
