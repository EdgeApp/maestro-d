package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// A device's capabilities come from the hub it is plugged into, and every
// hub in the mesh passes them on unchanged: in the listing, on their own
// and in a lease's answer.
func TestMesh_DeviceCapabilitiesFromTheOwner(t *testing.T) {
	setupHome(t)
	dir := t.TempDir()
	aConfig := filepath.Join(dir, "config.json")
	// Key order, a big integer and an object shaped like a device entry
	// all come back as written.
	const caps = `{"teamstation":{"class":"ipad","features":["camera"]},"anything":[1,2],"big":123456789012345678901234567890,"like":{"id":"x","platform":"ios","ready":true,"device":"y"}}`
	data := []byte(`{"hubDevices": [{"id": "a1", "description": "Lab iPad", "capabilities": ` + caps + `}, {"id": "a2"}]}`)
	if err := os.WriteFile(aConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a := startHub(t, "hub-a", testDevices("a"), withDeviceConfig(aConfig))
	b := startHub(t, "hub-b", testDevices("b"), withPeers(a.addr), withDeviceConfig(filepath.Join(dir, "none.json")))
	waitLinked(t, a, "hub-b")
	waitLinked(t, b, "hub-a")
	ctx := context.Background()

	var got map[string]string
	for deadline := time.Now().Add(10 * time.Second); ; {
		res, err := b.client().DeviceList(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]string{}
		for _, d := range res.Devices {
			got[d.ID] = compactJSON(d.Capabilities)
		}
		if _, ok := got["a1@hub-a"]; ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got["a1@hub-a"] != caps || got["a2@hub-a"] != "" || got["b1"] != "" {
		t.Fatalf("listing: %v", got)
	}
	if d, err := b.client().Device(ctx, "a1@hub-a"); err != nil || compactJSON(d.Capabilities) != caps || d.Description != "Lab iPad" {
		t.Fatalf("device: %v %+v", err, d)
	}
	lr, err := b.client().AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Holder: "tester"})
	if err != nil || lr.Device == nil || compactJSON(lr.Device.Capabilities) != caps {
		t.Fatalf("lease: %v %+v", err, lr)
	}
}

// The commands M1 adds, and an install from a URL, reach the device's own
// hub through a peer unchanged.
func TestMesh_NewCommandsRouteToTheOwner(t *testing.T) {
	a, b := twoHubs(t)
	type seen struct{ path, body string }
	var mu sync.Mutex
	var got []seen
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/devices/a1/apps" && !strings.HasPrefix(r.URL.Path, "/v1/devices/a1/commands/") {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.URL.Path, string(body)})
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exitCode": 0, "device": "a1", "success": true})
		return true
	})
	cases := []struct{ path, body string }{
		{"/v1/devices/a1@hub-a/commands/setStatusBar", `{"mode":"demo","time":"9:41","battery":100}`},
		{"/v1/devices/a1@hub-a/commands/keyboardVisible", `null`},
		{"/v1/devices/a1@hub-a/commands/startSampler", `{"kind":"cpu","appId":"app.x","intervalMs":500}`},
		{"/v1/devices/a1@hub-a/commands/stopSampler", `"cpu"`},
		{"/v1/devices/a1@hub-a/commands/assertWithAI", `{"assertion":"A sign-in form","budgetUsd":0.2,"thresholds":{"jevPass":1}}`},
		{"/v1/devices/a1@hub-a/commands/assertNoDefectsWithAI", `{"budgetUsd":0.1}`},
		{"/v1/devices/a1@hub-a/apps", `{"url":"https://builds.example/download","headers":{"Authorization":"Bearer x"},"appId":"app.x"}`},
	}
	for _, c := range cases {
		if code, body := b.do(t, http.MethodPost, c.path, []byte(c.body)); code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.path, code, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(cases) {
		t.Fatalf("owner saw %d requests: %+v", len(got), got)
	}
	for i, c := range cases {
		want := "/v1/devices/a1/" + c.path[len("/v1/devices/a1@hub-a/"):]
		if got[i].path != want || got[i].body != c.body {
			t.Errorf("owner got %s %s, want %s %s", got[i].path, got[i].body, want, c.body)
		}
	}
}

// An Android create through a peer reaches the hub it names with its
// platform and spec, and answers with the new AVD's qualified id.
func TestMesh_AndroidCreateRoutesByHub(t *testing.T) {
	a, b := twoHubs(t)
	var bootBody guarded[daemon.BootRequest]
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/devices" {
			return false
		}
		var req daemon.BootRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		bootBody.store(req)
		writeJSON(w, 200, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "ci-pixel", Ready: true}})
		return true
	})
	noBoot := false
	dev, err := b.client().CreateDevice(context.Background(), daemon.BootRequest{Platform: "android", Name: "ci-pixel", Hub: "hub-a", Boot: &noBoot,
		Create: &daemon.CreateSpec{Name: "ci-pixel", Image: "system-images;android-35;google_apis;arm64-v8a", Profile: "pixel_8", DataSize: "8G"}})
	if err != nil || dev.ID != "emulator-5554@hub-a" || dev.Hub != "hub-a" {
		t.Fatalf("create on hub-a: %+v %v", dev, err)
	}
	req := bootBody.load()
	if req.Platform != "android" || req.Name != "ci-pixel" || req.Hub != "" || req.Create == nil ||
		req.Create.Image != "system-images;android-35;google_apis;arm64-v8a" || req.Create.Profile != "pixel_8" || req.Create.DataSize != "8G" ||
		req.Boot == nil || *req.Boot {
		t.Fatalf("owner got %+v %+v", req, req.Create)
	}
}

// compactJSON is raw without its spacing: the tokens a hub passes on.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// guarded holds one value across goroutines.
type guarded[T any] struct {
	mu sync.Mutex
	v  T
}

func (a *guarded[T]) store(v T) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *guarded[T]) load() T   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
