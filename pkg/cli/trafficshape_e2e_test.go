package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// shapeRecorder stands in for a lab's traffic shaper.
type shapeRecorder struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
}

func newShapeRecorder(t *testing.T) *shapeRecorder {
	t.Helper()
	sr := &shapeRecorder{}
	sr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sr.mu.Lock()
		sr.calls = append(sr.calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		sr.mu.Unlock()
	}))
	t.Cleanup(sr.Close)
	return sr
}

// take returns the calls received since the last take, and logs them so
// a -v run shows what the shaper was sent.
func (sr *shapeRecorder) take(t *testing.T) []string {
	t.Helper()
	sr.mu.Lock()
	out := sr.calls
	sr.calls = nil
	sr.mu.Unlock()
	if len(out) == 0 {
		t.Logf("the shaper received nothing")
	} else {
		t.Logf("the shaper received:\n%s", strings.Join(out, "\n"))
	}
	return out
}

// writeShapingConfig writes home/config.json configuring one device.
func writeShapingConfig(t *testing.T, home, device, base string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"hubDevices": []any{map[string]any{"id": device, "trafficShaping": map[string]any{
		"setTrafficShape": map[string]any{"method": "POST", "url": base + "/v1/shape",
			"body":            map[string]any{"device": "${deviceId}", "mbit": "${mbit}", "lossPct": "${lossPct}", "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}"},
			"directionValues": map[string]string{"download": "down", "upload": "up"}},
		"resetTrafficShape": map[string]any{"method": "POST", "url": base + "/v1/shape", "body": map[string]any{"device": "${deviceId}"}},
	}}}})
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// B1: the real binary, mock devices, a config for mock-1 only.
func TestE2E_TrafficShape(t *testing.T) {
	e := newE2E(t)
	sr := newShapeRecorder(t)
	cfg := writeShapingConfig(t, e.home, "mock-1", sr.URL)
	mocks := []string{"MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	run := func(args ...string) result { return e.runEnv(mocks, append(args, "--daemon", "shape")...) }
	expectCalls := func(want ...string) {
		t.Helper()
		if got := sr.take(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("shaper calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}

	// 1. Every value is sent, the ones not given at their defaults.
	if r := run("setTrafficShape", "--device", "mock-1", "--mbit", "1.5", "--latencyMs", "100", "--direction", "download"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	expectCalls(`POST /v1/shape {"device":"mock-1","direction":"down","jitterMs":0,"latencyMs":100,"lossPct":0,"mbit":1.5}`)

	// 2. An unconfigured device answers UNSUPPORTED; no flags sends the
	// all-defaults shape on a configured one.
	if r := run("setTrafficShape", "--device", "mock-2"); r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUnsupported ||
		!strings.Contains(r.stderr, "no traffic shaper is configured for mock-2 on this host") {
		t.Fatalf("mock-2: exit %d\n%s", r.code, r.stderr)
	}
	if r := run("setTrafficShape", "--device", "mock-1"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	expectCalls(`POST /v1/shape {"device":"mock-1","direction":"both","jitterMs":0,"latencyMs":0,"lossPct":0,"mbit":0}`)

	// 3. The listing says which device can be shaped.
	r := run("device", "list", "--json")
	var listed daemon.DevicesResult
	if err := json.Unmarshal([]byte(r.stdout), &listed); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	shaping := map[string]bool{}
	for _, d := range listed.Devices {
		shaping[d.ID] = d.TrafficShaping
	}
	if !shaping["mock-1"] || shaping["mock-2"] {
		t.Fatalf("listing: %v", shaping)
	}

	// 4. A broken config is USAGE naming the file; nothing is sent.
	good, _ := os.ReadFile(cfg)
	if err := os.WriteFile(cfg, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run("setTrafficShape", "--device", "mock-1", "--lossPct", "100"); r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage ||
		!strings.Contains(r.stderr, "config "+cfg) {
		t.Fatalf("broken config: exit %d\n%s", r.code, r.stderr)
	}
	if err := os.WriteFile(cfg, good, 0o600); err != nil {
		t.Fatal(err)
	}
	expectCalls()

	if r := run("resetTrafficShape", "--device", "mock-1"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	expectCalls(`POST /v1/shape {"device":"mock-1"}`)

	// 5. The end of the session that shaped a device resets it.
	if r := run("setTrafficShape", "--device", "mock-1", "--latencyMs", "250"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	sr.take(t)
	if r := run("detach", "--device", "mock-1"); r.code != 0 {
		t.Fatalf("detach: exit %d\n%s", r.code, r.stderr)
	}
	expectCalls(`POST /v1/shape {"device":"mock-1"}`)
}

// B2: through two hubs, the hub the device is plugged into makes the call
// with its own config, under the caller's lease.
func TestE2E_TrafficShapeTwoHubs(t *testing.T) {
	e := newE2E(t)
	sr := newShapeRecorder(t)
	homeA, homeB, homeC := filepath.Join(e.cwd, "home-a"), filepath.Join(e.cwd, "home-b"), filepath.Join(e.cwd, "home-c")
	writeShapingConfig(t, homeA, "mock-1", sr.URL)
	envA := []string{daemon.EnvHome + "=" + homeA, "MAESTRO_D_MOCK_DEVICES=mock-1"}
	writeSharedMocks(t, homeB, "mock-3")
	envB := []string{daemon.EnvHome + "=" + homeB, "MAESTRO_D_MOCK_DEVICES=mock-3"}
	t.Cleanup(func() {
		e.runEnv(envB, "maestro-d-hub", "stop")
		e.runEnv(envA, "maestro-d-hub", "stop")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "shape-tok", "--peer-token", "shape-peer", "--json"}
	started := func(r result) daemon.Info {
		t.Helper()
		var out struct {
			Daemon daemon.Info `json:"daemon"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		return out.Daemon
	}
	a := started(e.runEnv(envA, append([]string{"maestro-d-hub", "start", "--name", "hub-a"}, mesh...)...))
	b := started(e.runEnv(envB, append([]string{"maestro-d-hub", "start", "--name", "hub-b", "--peer", a.HTTPAddr}, mesh...)...))
	client := []string{daemon.EnvHome + "=" + homeC, "MAESTRO_D_HUB=" + b.HTTPAddr, "MAESTRO_D_TOKEN=shape-tok"}

	// Without a lease the owner refuses, and nothing is sent.
	if r := e.runEnv(client, "setTrafficShape", "--device", "mock-1@hub-a", "--lossPct", "100"); r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("no lease: exit %d\n%s", r.code, r.stderr)
	}
	if got := sr.take(t); len(got) != 0 {
		t.Fatalf("calls without a lease: %v", got)
	}
	if r := e.runEnv(client, "lease", "--device", "mock-1@hub-a"); r.code != 0 {
		t.Fatalf("lease: exit %d\n%s", r.code, r.stderr)
	}
	if r := e.runEnv(client, "setTrafficShape", "--device", "mock-1@hub-a", "--lossPct", "100"); r.code != 0 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	if got := sr.take(t); len(got) != 1 || got[0] != `POST /v1/shape {"device":"mock-1","direction":"both","jitterMs":0,"latencyMs":0,"lossPct":100,"mbit":0}` {
		t.Fatalf("calls: %v", got)
	}

	// The merged listing through hub-b carries hub-a's answer.
	r := e.runEnv(client, "device", "list", "--platform", "mock", "--json")
	var listed daemon.DevicesResult
	if err := json.Unmarshal([]byte(r.stdout), &listed); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	shaping := map[string]bool{}
	for _, d := range listed.Devices {
		shaping[d.ID] = d.TrafficShaping
	}
	if !shaping["mock-1@hub-a"] || shaping["mock-3@hub-b"] {
		t.Fatalf("listing: %v", shaping)
	}
}

// config.json through real hubs: hub-a shares only the device its
// hubDevices lists, which carries its description and shaping across the
// mesh; a broken config.json stops
// hub start.
func TestE2E_ConfigJSONTwoHubs(t *testing.T) {
	e := newE2E(t)
	sr := newShapeRecorder(t)
	homeA, homeB, homeC := filepath.Join(e.cwd, "cfg-a"), filepath.Join(e.cwd, "cfg-b"), filepath.Join(e.cwd, "cfg-c")
	for _, h := range []string{homeA, homeB, homeC} {
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := json.Marshal(map[string]any{"hubDevices": []any{map[string]any{
		"id": "mock-1", "description": "Lab phone on hub A",
		"trafficShaping": map[string]any{
			"setTrafficShape":   map[string]any{"method": "POST", "url": sr.URL + "/v1/shape", "body": map[string]any{"device": "${deviceId}", "lossPct": "${lossPct}"}},
			"resetTrafficShape": map[string]any{"method": "POST", "url": sr.URL + "/v1/shape", "body": map[string]any{"device": "${deviceId}"}},
		},
	}}})
	if err := os.WriteFile(filepath.Join(homeA, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	envA := []string{daemon.EnvHome + "=" + homeA, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	writeSharedMocks(t, homeB, "mock-3")
	envB := []string{daemon.EnvHome + "=" + homeB, "MAESTRO_D_MOCK_DEVICES=mock-3"}
	t.Cleanup(func() {
		e.runEnv(envB, "maestro-d-hub", "stop")
		e.runEnv(envA, "maestro-d-hub", "stop")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "cfg-tok", "--peer-token", "cfg-peer", "--json"}

	// A broken config is refused before hub start.
	broken := filepath.Join(e.cwd, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"hubDevices":[{"description":"no id"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.runEnv(envA, append([]string{"maestro-d-hub", "start", "--name", "hub-a", "--config", broken}, mesh...)...); r.code != 2 || !strings.Contains(r.stderr, "hubDevices[0] has no id") {
		t.Fatalf("broken config: exit %d\n%s", r.code, r.stderr)
	}

	started := func(r result) daemon.Info {
		t.Helper()
		var out struct {
			Daemon daemon.Info `json:"daemon"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		return out.Daemon
	}
	a := started(e.runEnv(envA, append([]string{"maestro-d-hub", "start", "--name", "hub-a"}, mesh...)...))
	b := started(e.runEnv(envB, append([]string{"maestro-d-hub", "start", "--name", "hub-b", "--peer", a.HTTPAddr}, mesh...)...))
	client := []string{daemon.EnvHome + "=" + homeC, "MAESTRO_D_HUB=" + b.HTTPAddr, "MAESTRO_D_TOKEN=cfg-tok"}

	var listed daemon.DevicesResult
	for deadline := time.Now().Add(15 * time.Second); ; {
		r := e.runEnv(client, "device", "list", "--platform", "mock", "--json")
		listed = daemon.DevicesResult{}
		if err := json.Unmarshal([]byte(r.stdout), &listed); err != nil {
			t.Fatalf("%v\n%s", err, r.stdout)
		}
		found := false
		for _, d := range listed.Devices {
			found = found || d.ID == "mock-1@hub-a"
		}
		if found || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	byID := map[string]daemon.DeviceInfo{}
	for _, d := range listed.Devices {
		byID[d.ID] = d
	}
	if d, ok := byID["mock-1@hub-a"]; !ok || d.Description != "Lab phone on hub A" || !d.TrafficShaping {
		t.Fatalf("mock-1@hub-a: %+v (listing %v)", d, listed.Devices)
	}
	if _, ok := byID["mock-2@hub-a"]; ok {
		t.Fatalf("hub-a shared mock-2, which its config.json does not list: %v", listed.Devices)
	}
	if _, ok := byID["mock-3@hub-b"]; !ok {
		t.Fatalf("hub-b's configured mock-3 missing: %v", listed.Devices)
	}
	if r := e.runEnv(client, "lease", "--device", "mock-1@hub-a"); r.code != 0 {
		t.Fatalf("lease: exit %d\n%s", r.code, r.stderr)
	}
	if r := e.runEnv(client, "setTrafficShape", "--device", "mock-1@hub-a", "--lossPct", "100"); r.code != 0 {
		t.Fatalf("setTrafficShape: exit %d\n%s", r.code, r.stderr)
	}
	if got := sr.take(t); len(got) != 1 || got[0] != `POST /v1/shape {"device":"mock-1","lossPct":100}` {
		t.Fatalf("calls: %v", got)
	}
	if r := e.runEnv(client, "release", "--device", "mock-1@hub-a"); r.code != 0 {
		t.Fatalf("release: exit %d\n%s", r.code, r.stderr)
	}
}
