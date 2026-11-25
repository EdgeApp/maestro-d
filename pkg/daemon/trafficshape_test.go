package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
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
	if codeOf(t, err) != CodeUsage || !strings.Contains(res.Error.Message, "config "+cfgPath) {
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
	if got, legacy := env.s.deviceConfig().Resolve(); got != path || !legacy {
		t.Fatalf("path = %s, legacy %v", got, legacy)
	}
	env.attach("mock-1")
	if _, err := env.c.Command(context.Background(), "mock-1", "resetTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	if len(ss.calls()) != 1 {
		t.Fatalf("calls: %v", ss.calls())
	}
	env.s.ApplyDeviceConfig(nil) // nothing to mark is not an error
}

const resetCall = "DELETE /shape/mock-1"

func countCalls(ss *shaperServer, call string) int {
	n := 0
	for _, c := range ss.calls() {
		if c == call {
			n++
		}
	}
	return n
}

// A reset that never answers holds the teardown for its bound only.
func TestServer_SessionEndResetIsBounded(t *testing.T) {
	env := newTestEnv(t, 0)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(srv.Close)
	writeNetworkConfig(t, filepath.Join(Home(), "network.json"), srv.URL)
	env.attach("mock-1")
	if _, err := env.c.Command(context.Background(), "mock-1", "setTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := env.c.Detach(context.Background(), "mock-1"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < 3*time.Second || took > 8*time.Second {
		t.Fatalf("detach took %s; the reset's bound is 4 s", took)
	}
	if atomic.LoadInt32(&env.cleanups) != 1 {
		t.Fatalf("the driver was not closed: %d", env.cleanups)
	}
}

// airplaneDriver records the airplane-mode values a device was set to.
type airplaneDriver struct {
	*mock.Driver
	mu   sync.Mutex
	seen []bool
}

func (d *airplaneDriver) Execute(step flow.Step) *core.CommandResult {
	if s, ok := step.(*flow.SetAirplaneModeStep); ok {
		d.mu.Lock()
		d.seen = append(d.seen, s.Enabled)
		d.mu.Unlock()
	}
	return d.Driver.Execute(step)
}

func (d *airplaneDriver) got() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]bool(nil), d.seen...)
}

// A listing logs an unusable config once per problem, not once per poll.
func TestWarnShapingOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	if !warnShapingOnce(path, "mode 0644") || warnShapingOnce(path, "mode 0644") {
		t.Fatal("the same problem logged twice")
	}
	if !warnShapingOnce(path, "invalid JSON") {
		t.Fatal("a new problem was not logged")
	}
	shapingWarned.Delete(path) // the file loaded again
	if !warnShapingOnce(path, "invalid JSON") {
		t.Fatal("a problem after a good load was not logged")
	}
	shapingWarned.Delete(path)
}

// A new attach of a device waits for the previous session's teardown, so
// that session's reset never lands after the new session's shape.
func TestServer_AttachWaitsForTheDetachCleanup(t *testing.T) {
	env := newTestEnv(t, 0)
	var mu sync.Mutex
	var events []string
	resetStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			resetStarted <- struct{}{}
			time.Sleep(500 * time.Millisecond)
		}
		mu.Lock()
		events = append(events, r.Method+" done")
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	writeNetworkConfig(t, filepath.Join(Home(), "network.json"), srv.URL)
	ctx := context.Background()
	env.attach("mock-1")
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
		t.Fatal(err)
	}
	detached := make(chan error, 1)
	go func() { detached <- env.c.Detach(ctx, "mock-1") }()
	<-resetStarted
	// The old session's reset is in flight: attach again and shape.
	env.attach("mock-1")
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"latencyMs": 50}, true); err != nil {
		t.Fatal(err)
	}
	if err := <-detached; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(events, ", ")
	mu.Unlock()
	if got != "POST done, DELETE done, POST done" {
		t.Fatalf("shaper saw %s; the new shape must follow the old reset", got)
	}
}

// A teardown that panics still releases the device's reservation, so it
// can be attached again.
func TestServer_DetachPanicReleasesTheDevice(t *testing.T) {
	env := newTestEnv(t, 0)
	env.attach("mock-1")
	env.s.mu.Lock()
	a := env.s.attached["mock-1"]
	env.s.mu.Unlock()
	a.release = func() { panic("release") }
	func() {
		defer func() { _ = recover() }()
		env.s.DetachForStop("mock-1")
	}()
	env.s.mu.Lock()
	_, reserved := env.s.attaching["mock-1"]
	env.s.mu.Unlock()
	if reserved {
		t.Fatal("the device stayed reserved after a panicking teardown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := env.c.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
}

// hubDevicesJSON is config.json for devices given by id: each entry's
// setTrafficShape and resetTrafficShape go under trafficShaping.
func hubDevicesJSON(t *testing.T, devices map[string]any) []byte {
	t.Helper()
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := []map[string]any{}
	for _, id := range ids {
		e := map[string]any{"id": id}
		shaping := map[string]any{}
		for k, v := range devices[id].(map[string]any) {
			if k == "setTrafficShape" || k == "resetTrafficShape" {
				shaping[k] = v
			} else {
				e[k] = v
			}
		}
		if len(shaping) > 0 {
			e["trafficShaping"] = shaping
		}
		list = append(list, e)
	}
	data, err := json.Marshal(map[string]any{"hubDevices": list})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A device's description in config.json is returned in listings, on its
// own and in the attach answer; an entry with only a description is listed
// but not shapeable. The file is read on every listing.
func TestServer_DeviceDescription(t *testing.T) {
	env := newTestEnv(t, 0)
	ss := newShaperServer(t)
	cfgPath := filepath.Join(Home(), "config.json")
	write := func(devices map[string]any) {
		t.Helper()
		if err := os.WriteFile(cfgPath, hubDevicesJSON(t, devices), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{
		"mock-1": map[string]any{
			"description":       "Lab Pixel 8, landscape stand",
			"setTrafficShape":   map[string]any{"method": "POST", "url": ss.URL + "/shape"},
			"resetTrafficShape": map[string]any{"method": "DELETE", "url": ss.URL + "/shape"},
		},
		"mock-2": map[string]any{"description": "Spare phone, no shaper"},
	})
	ctx := context.Background()

	if info := env.attach("mock-2"); info.Description != "Spare phone, no shaper" || info.TrafficShaping {
		t.Fatalf("attach answer: %+v", info)
	}
	devs, err := env.c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DeviceInfo{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	if d := byID["mock-1"]; d.Description != "Lab Pixel 8, landscape stand" || !d.TrafficShaping {
		t.Fatalf("mock-1 listed as %+v", d)
	}
	if d := byID["mock-2"]; d.Description != "Spare phone, no shaper" || d.TrafficShaping {
		t.Fatalf("mock-2 listed as %+v", d)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || d.Description != "Lab Pixel 8, landscape stand" {
		t.Fatalf("device: %v %+v", err, d)
	}

	// A described device without calls is not shapeable, and sends nothing.
	res, err := env.c.Command(ctx, "mock-2", "setTrafficShape", nil, true)
	if codeOf(t, err) != CodeUnsupported || res.Error.Message != "no traffic shaper is configured for mock-2 on this host" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(ss.calls()) != 0 {
		t.Fatalf("calls: %v", ss.calls())
	}

	// An edit shows at once; a removed description is omitted.
	write(map[string]any{"mock-2": map[string]any{"description": "Spare phone, cracked screen"}})
	if d, err := env.c.Device(ctx, "mock-2"); err != nil || d.Description != "Spare phone, cracked screen" {
		t.Fatalf("after an edit: %v %+v", err, d)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || d.Description != "" || d.TrafficShaping {
		t.Fatalf("mock-1 after its entry went: %v %+v", err, d)
	}

	// A broken file lists no descriptions.
	if err := os.WriteFile(cfgPath, []byte(`{"devices":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := env.c.Device(ctx, "mock-2"); err != nil || d.Description != "" {
		t.Fatalf("a broken file still lists a description: %v %+v", err, d)
	}
}

// writeDescriptions writes a config.json in this daemon's home that only
// describes devices.
func writeDescriptions(t *testing.T, descriptions map[string]string) {
	t.Helper()
	devices := map[string]any{}
	for id, d := range descriptions {
		devices[id] = map[string]any{"description": d}
	}
	if err := os.WriteFile(filepath.Join(Home(), "config.json"), hubDevicesJSON(t, devices), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An AVD keyed avd:<name> keeps its description once it boots and is
// listed under its serial; the boot and create answers carry descriptions,
// and an AVD entry without calls shapes nothing.
func TestBootAndCreate_AnswersCarryTheDescription(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "avd:pix", Platform: "android", Kind: "emulator", Name: "pix", State: "Shutdown"}}
	e := newMgmtEnv(t, h)
	writeDescriptions(t, map[string]string{"avd:pix": "Pixel 9 AVD, API 35", "NEW-UDID": "CI simulator"})

	dev, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || dev.ID != "emulator-5554" || dev.Description != "Pixel 9 AVD, API 35" || dev.TrafficShaping {
		t.Fatalf("boot answer: %+v %v", dev, err)
	}
	listed, err := e.c.Device(bg, "emulator-5554")
	if err != nil || listed.Description != "Pixel 9 AVD, API 35" || listed.TrafficShaping {
		t.Fatalf("running emulator: %+v %v", listed, err)
	}

	created, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "sim", Create: &CreateSpec{Type: "iPhone 16", Runtime: "iOS-18-6"}})
	if err != nil || created.ID != "NEW-UDID" || created.Description != "CI simulator" {
		t.Fatalf("create answer: %+v %v", created, err)
	}
}

// A running emulator whose serial has its own entry (calls, no
// description) still takes its AVD's description; a blank description on
// an entry with calls is omitted.
func TestServer_DescriptionPrecedenceAndBlanks(t *testing.T) {
	ss := newShaperServer(t)
	devs := []DeviceInfo{
		{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "pix"},
		{ID: "emulator-5556", Platform: "android", Kind: "emulator", Name: "other"},
		{ID: "mock-1", Platform: "mock", Kind: "device"},
	}
	calls := map[string]any{
		"setTrafficShape":   map[string]any{"method": "POST", "url": ss.URL + "/shape"},
		"resetTrafficShape": map[string]any{"method": "DELETE", "url": ss.URL + "/shape"},
	}
	withDescription := func(desc string) map[string]any {
		m := map[string]any{"description": desc}
		for k, v := range calls {
			m[k] = v
		}
		return m
	}
	env := newTestEnv(t, 0)
	data := hubDevicesJSON(t, map[string]any{
		"emulator-5554": calls,
		"avd:pix":       map[string]any{"description": "Pixel 9 AVD"},
		"emulator-5556": withDescription("Its own"),
		"avd:other":     map[string]any{"description": "Not used"},
		"mock-1":        withDescription(" \t "),
	})
	if err := os.WriteFile(filepath.Join(Home(), "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	env.s.ApplyDeviceConfig(devs)
	if d := devs[0]; d.Description != "Pixel 9 AVD" || !d.TrafficShaping {
		t.Errorf("emulator-5554: %+v", d)
	}
	if d := devs[1]; d.Description != "Its own" || !d.TrafficShaping {
		t.Errorf("emulator-5556: %+v", d)
	}
	if d := devs[2]; d.Description != "" || !d.TrafficShaping {
		t.Errorf("mock-1 with a blank description: %+v", d)
	}
	if b, _ := json.Marshal(devs[2]); strings.Contains(string(b), "description") {
		t.Errorf("a blank description is serialized: %s", b)
	}
}

// A running emulator with no entry of its own takes its AVD entry's calls,
// in the listing and in the session's shaper; an entry keyed by a device's
// name gives it nothing.
func TestServer_AVDEntryShapesTheRunningEmulator(t *testing.T) {
	ss := newShaperServer(t)
	env := newTestEnv(t, 0)
	data := hubDevicesJSON(t, map[string]any{
		"avd:pix": map[string]any{
			"description":       "Pixel 9 AVD",
			"setTrafficShape":   map[string]any{"method": "POST", "url": ss.URL + "/shape"},
			"resetTrafficShape": map[string]any{"method": "DELETE", "url": ss.URL + "/shape"},
		},
		"Lab Pixel": map[string]any{"description": "By name"},
	})
	if err := os.WriteFile(filepath.Join(Home(), "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	devs := []DeviceInfo{
		{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "pix"},
		{ID: "PHONE-B", Platform: "android", Kind: "device", Name: "Lab Pixel"},
	}
	env.s.ApplyDeviceConfig(devs)
	if d := devs[0]; d.Description != "Pixel 9 AVD" || !d.TrafficShaping {
		t.Errorf("emulator-5554: %+v", d)
	}
	if d := devs[1]; d.Description != "" || d.TrafficShaping {
		t.Errorf("an entry by name: %+v", d)
	}
	sh := env.s.trafficShaper("emulator-5554")
	sh.Alias = avdEntryID(devs[0])
	if _, err := sh.Device(); err != nil {
		t.Errorf("shaper: %v", err)
	}
}
