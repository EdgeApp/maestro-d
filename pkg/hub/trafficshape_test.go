package hub

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

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func withNetworkConfig(path string) hubOpt {
	return func(_ *Config, d *daemon.Config) { d.NetworkConfig = path }
}

// A device's shaping call is made by the hub it is plugged into, with that
// hub's own config, under the caller's lease.
func TestMesh_TrafficShapingRunsOnTheOwner(t *testing.T) {
	setupHome(t)
	var mu sync.Mutex
	var calls []string
	shaper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
	}))
	t.Cleanup(shaper.Close)

	dir := t.TempDir()
	aConfig := filepath.Join(dir, "a.json")
	data, _ := json.Marshal(map[string]any{"devices": map[string]any{"a1": map[string]any{
		"setTrafficShape":   map[string]any{"method": "PUT", "url": shaper.URL + "/a1", "body": map[string]any{"loss": "${lossPct}"}},
		"resetTrafficShape": map[string]any{"method": "DELETE", "url": shaper.URL + "/a1"},
	}}})
	if err := os.WriteFile(aConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a := startHub(t, "hub-a", testDevices("a"), withNetworkConfig(aConfig))
	b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr), withNetworkConfig(filepath.Join(dir, "none.json")))
	waitLinked(t, a, "hub-b")
	waitLinked(t, b, "hub-a")
	c := b.client()
	ctx := context.Background()

	// The merged listing carries each hub's own answer.
	devs, err := c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	shaping := map[string]bool{}
	for _, d := range devs {
		shaping[d.ID] = d.TrafficShaping
	}
	if !shaping["a1@hub-a"] || shaping["a2@hub-a"] || shaping["b1"] {
		t.Fatalf("listing: %v", shaping)
	}

	// Without the lease the owner refuses and nothing is sent.
	code, body := b.do(t, http.MethodPost, "/v1/devices/a1@hub-a/commands/setTrafficShape", `{"lossPct":100}`)
	if code != http.StatusConflict || codeIn(body) != daemon.CodeLeaseRequired {
		t.Fatalf("no lease: %d %s", code, body)
	}
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Holder: "tester", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	if res, err := c.Command(ctx, "a1@hub-a", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	if res, err := c.Command(ctx, "a1@hub-a", "resetTrafficShape", nil, true); err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	mu.Lock()
	got := strings.Join(calls, "\n")
	mu.Unlock()
	if got != "PUT /a1 {\"loss\":100}\nDELETE /a1 " {
		t.Fatalf("calls: %q", got)
	}

	// hub-b has no shaper for its own device.
	lb, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "b1", Holder: "tester", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("b1", lb.Lease.ID)
	if _, err := c.Command(ctx, "b1", "setTrafficShape", nil, true); !isCode(err, daemon.CodeUnsupported) {
		t.Fatalf("b1: %v", err)
	}
}
