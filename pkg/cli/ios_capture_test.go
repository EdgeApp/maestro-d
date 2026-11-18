package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/quicktime"
	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// captureLog records step 0's and the runner's calls in order.
type captureLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *captureLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *captureLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.calls, " ")
}

func (l *captureLog) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c == s {
			n++
		}
	}
	return n
}

type fakeCaptureDevice struct {
	log  *captureLog
	res  quicktime.Result
	sess *capture.Session
}

func (d *fakeCaptureDevice) Session() *capture.Session { return d.sess }
func (d *fakeCaptureDevice) Wait(context.Context) quicktime.Result {
	d.log.add("wait")
	return d.res
}
func (d *fakeCaptureDevice) Close() { d.log.add("capture-close") }

// idleSource is an always-on source that runs until cancelled.
type idleSource struct{}

func (idleSource) Mode() capture.Mode { return capture.AlwaysOn }
func (idleSource) FPSControl() bool   { return false }
func (idleSource) Run(ctx context.Context, _ capture.Options, _ func(capture.Packet)) error {
	<-ctx.Done()
	return nil
}

type loggedRunner struct {
	log      *captureLog
	url      string
	buildErr error
	startErr error
}

func (r *loggedRunner) Build(context.Context) error {
	r.log.add("build")
	return r.buildErr
}
func (r *loggedRunner) Start(context.Context) error {
	r.log.add("start")
	return r.startErr
}
func (r *loggedRunner) Cleanup()                        { r.log.add("runner-cleanup") }
func (r *loggedRunner) Port() uint16                    { return 0 }
func (r *loggedRunner) BaseURL() string                 { return r.url }
func (r *loggedRunner) Devicectl() *wda.DevicectlLookup { return nil }
func (r *loggedRunner) IsTV() bool                      { return false }

// captureFixture wires CreateIOSDriver's seams for a physical "phone"
// (usbmux lists it over USB, lockdown says iPhone) and a fake capture.
type captureFixture struct {
	log        *captureLog
	runner     *loggedRunner
	dev        *fakeCaptureDevice
	entries    []goios.DeviceEntry
	relisted   int // ListDevices calls (after the first) that omit the device
	class      string
	listErr    error
	valuesErr  error
	devInfoErr error
	sleeps     []time.Duration
	opened     []string
}

const capturePhone = "00008110-FIXTURE0000PHONE"

func newCaptureFixture(t *testing.T, simulators ...string) *captureFixture {
	t.Helper()
	f := &captureFixture{log: &captureLog{}, class: "iPhone"}
	f.entries = []goios.DeviceEntry{
		{Properties: goios.DeviceProperties{SerialNumber: capturePhone, ConnectionType: "Network"}},
		{Properties: goios.DeviceProperties{SerialNumber: capturePhone, ConnectionType: "USB"}},
	}
	sess := capture.NewSession(idleSource{})
	if err := sess.Start(capture.Options{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	f.dev = &fakeCaptureDevice{log: f.log, sess: sess, res: quicktime.Result{Found: true, At: time.Now(), Match: "name"}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":{"sessionId":"s","capabilities":{},"width":390,"height":844},"sessionId":"s","status":0}`))
	}))
	t.Cleanup(server.Close)
	f.runner = &loggedRunner{log: f.log, url: server.URL}

	// xcrun: simctl lists only the given simulators; devicectl is absent.
	bin := t.TempDir()
	var devs []string
	for _, s := range simulators {
		devs = append(devs, fmt.Sprintf(`{"udid":%q,"name":"Sim","state":"Booted"}`, s))
	}
	json := `{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-1":[` + strings.Join(devs, ",") + `]}}`
	script := "#!/bin/sh\nif [ \"$1\" = simctl ]; then cat <<'JSON'\n" + json + "\nJSON\nexit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldGOOS, oldList, oldValues, oldOpen := iosCaptureGOOS, iosListDevices, iosGetValues, iosCaptureOpen
	oldNow, oldWait, oldRunner, oldInfo := iosCaptureNow, iosCaptureWait, newIOSWDARunner, devicectlDeviceInfo
	t.Cleanup(func() {
		iosCaptureGOOS, iosListDevices, iosGetValues, iosCaptureOpen = oldGOOS, oldList, oldValues, oldOpen
		iosCaptureNow, iosCaptureWait, newIOSWDARunner, devicectlDeviceInfo = oldNow, oldWait, oldRunner, oldInfo
	})
	iosCaptureGOOS = "darwin"
	clock := time.Now()
	iosCaptureNow = func() time.Time { return clock }
	iosCaptureWait = func(d time.Duration) {
		f.sleeps = append(f.sleeps, d)
		clock = clock.Add(d)
	}
	f.dev.res.At = clock
	lists := 0
	iosListDevices = func() (goios.DeviceList, error) {
		lists++
		if lists == 1 {
			f.log.add("list")
		} else {
			f.log.add("relist")
		}
		if f.listErr != nil {
			return goios.DeviceList{}, f.listErr
		}
		if lists > 1 && lists-1 <= f.relisted {
			return goios.DeviceList{}, nil
		}
		return goios.DeviceList{DeviceList: f.entries}, nil
	}
	iosGetValues = func(e goios.DeviceEntry) (goios.GetAllValuesResponse, error) {
		f.log.add("values")
		if e.Properties.ConnectionType != "USB" {
			t.Errorf("lockdown asked over %s", e.Properties.ConnectionType)
		}
		var v goios.GetAllValuesResponse
		v.Value.DeviceClass, v.Value.DeviceName = f.class, "iPhone13Pro"
		return v, f.valuesErr
	}
	iosCaptureOpen = func(udid, name string) iosCaptureDevice {
		f.log.add("open")
		f.opened = append(f.opened, udid+"/"+name)
		return f.dev
	}
	newIOSWDARunner = func(udid, team, bundle string) iosWDARunner { return f.runner }
	devicectlDeviceInfo = func(udid string) (wda.DevicectlDevice, error) {
		f.log.add("device-info")
		return wda.DevicectlDevice{Name: "iPhone13Pro", OSVersion: "26.6.1"}, f.devInfoErr
	}
	return f
}

func captureCfg(udid string) *RunConfig {
	return &RunConfig{Devices: []string{udid}, ScreenCapture: true, NoAppInstall: true, NoDriverInstall: true, NoFlutterFallback: true}
}

// U41: step 0 runs before install and WebDriverAgent, waits for usbmux to
// list the re-enumerated device, and cleanup ends WDA before the capture.
func TestIOSCapture_StepZeroOrder(t *testing.T) {
	f := newCaptureFixture(t)
	f.relisted = 2
	f.dev.res.Match = "serial"
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	if err := logger.Init(logPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)
	driver, cleanup, err := CreateIOSDriver(captureCfg(capturePhone))
	if err != nil {
		t.Fatal(err)
	}
	// The quicktime Host logs "capturing (match: …)"; step 0 must not log it again.
	if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "capturing (match:") {
		t.Fatalf("step 0 logged the capturing status:\n%s", b)
	}
	if got, want := f.log.String(), "list values open wait relist relist relist build start device-info"; got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	if len(f.opened) != 1 || f.opened[0] != capturePhone+"/iPhone13Pro" {
		t.Fatalf("opened %v", f.opened)
	}
	// Two 250 ms polls, then the rest of the 1.5 s since the status.
	if len(f.sleeps) != 3 || f.sleeps[0] != iosRelistPoll || f.sleeps[2] != iosRelistSettle-2*iosRelistPoll {
		t.Fatalf("sleeps %v", f.sleeps)
	}
	s, err := core.Unwrap(driver).(core.Capturer).Capture()
	if err != nil || s != f.dev.sess {
		t.Fatalf("Capture: %v %v", s, err)
	}
	cleanup()
	if got := f.log.String(); !strings.HasSuffix(got, "runner-cleanup capture-close") {
		t.Fatalf("cleanup order: %s", got)
	}
}

// U41: an attach that fails after step 0 closes the capture session (after
// what it started) before returning.
func TestIOSCapture_FailedAttachClosesCapture(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*captureFixture, *RunConfig)
		tail  string
	}{
		{"install", func(_ *captureFixture, c *RunConfig) {
			c.NoAppInstall, c.AppFile = false, "/nonexistent/App.app"
		}, "wait capture-close"},
		{"setup", func(_ *captureFixture, c *RunConfig) {
			c.NoDriverInstall = false
		}, "wait capture-close"},
		{"build", func(f *captureFixture, _ *RunConfig) { f.runner.buildErr = errors.New("xcodebuild") }, "build capture-close"},
		{"start", func(f *captureFixture, _ *RunConfig) { f.runner.startErr = errors.New("no WDA") }, "start runner-cleanup capture-close"},
		{"device info", func(f *captureFixture, _ *RunConfig) { f.devInfoErr = errors.New("locked") }, "device-info runner-cleanup capture-close"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "setup" {
				t.Setenv("MAESTRO_RUNNER_HOME", t.TempDir())
				config.ResetHome()
				t.Cleanup(config.ResetHome)
			}
			f := newCaptureFixture(t)
			f.dev.res.Found = false
			cfg := captureCfg(capturePhone)
			tc.setup(f, cfg)
			if _, _, err := CreateIOSDriver(cfg); err == nil {
				t.Fatal("attach succeeded")
			}
			if got := f.log.String(); !strings.HasSuffix(got, tc.tail) || f.log.count("capture-close") != 1 {
				t.Fatalf("calls %s, want ending %q", got, tc.tail)
			}
		})
	}
}

// U42: a failed capture still starts WebDriverAgent, and the driver says
// why it has no capture; a failure before device-found skips the
// re-listing wait.
func TestIOSCapture_FailureStillStartsWDA(t *testing.T) {
	f := newCaptureFixture(t)
	f.dev.res = quicktime.Result{Err: &capture.Unavailable{Reason: capture.ReasonCameraPermission, Message: "denied"}}
	driver, cleanup, err := CreateIOSDriver(captureCfg(capturePhone))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got := f.log.String(); got != "list values open wait capture-close build start device-info" {
		t.Fatalf("calls %s", got)
	}
	if _, err := driver.(core.Capturer).Capture(); capture.ReasonOf(err, "") != capture.ReasonCameraPermission {
		t.Fatalf("Capture: %v", err)
	}

	// After device-found the device re-enumerated: the wait applies.
	g := newCaptureFixture(t)
	g.dev.res = quicktime.Result{Found: true, At: iosCaptureNow(), Err: &capture.Unavailable{Reason: capture.ReasonHelperFailed}}
	_, cleanup2, err := CreateIOSDriver(captureCfg(capturePhone))
	if err != nil {
		t.Fatal(err)
	}
	cleanup2()
	if got := g.log.String(); !strings.HasPrefix(got, "list values open wait capture-close relist build") {
		t.Fatalf("calls %s", got)
	}
	if g.log.count("capture-close") != 1 {
		t.Fatalf("closed %d times", g.log.count("capture-close"))
	}
}

// U42: the cases that never start capture, and what Capture answers.
func TestIOSCapture_Skipped(t *testing.T) {
	cases := []struct {
		name   string
		udid   string
		sim    bool
		setup  func(*captureFixture, *RunConfig)
		calls  string
		reason string
	}{
		{name: "capture off", udid: capturePhone, setup: func(_ *captureFixture, c *RunConfig) { c.ScreenCapture = false },
			calls: "build start device-info", reason: capture.ReasonUnsupportedPlatform},
		{name: "simulator", udid: "SIM-1", sim: true, calls: "build start", reason: capture.ReasonSimulator},
		{name: "not macOS", udid: capturePhone, setup: func(*captureFixture, *RunConfig) { iosCaptureGOOS = "linux" },
			calls: "build start device-info", reason: capture.ReasonNotMacOS},
		{name: "not listed", udid: capturePhone, setup: func(f *captureFixture, _ *RunConfig) { f.entries = nil },
			calls: "list build start device-info", reason: capture.ReasonNoCaptureDevice},
		{name: "network only", udid: capturePhone, setup: func(f *captureFixture, _ *RunConfig) { f.entries = f.entries[:1] },
			calls: "list build start device-info", reason: capture.ReasonNoCaptureDevice},
		{name: "usbmuxd down", udid: capturePhone, setup: func(f *captureFixture, _ *RunConfig) { f.listErr = errors.New("no usbmuxd") },
			calls: "list build start device-info", reason: capture.ReasonNoCaptureDevice},
		{name: "lockdown", udid: capturePhone, setup: func(f *captureFixture, _ *RunConfig) { f.valuesErr = errors.New("pairing") },
			calls: "list values build start device-info", reason: capture.ReasonNoCaptureDevice},
		{name: "Apple TV", udid: capturePhone, setup: func(f *captureFixture, _ *RunConfig) { f.class = "AppleTV" },
			calls: "list values build start device-info", reason: capture.ReasonUnsupportedPlatform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f *captureFixture
			if tc.sim {
				f = newCaptureFixture(t, tc.udid)
			} else {
				f = newCaptureFixture(t)
			}
			cfg := captureCfg(tc.udid)
			if tc.setup != nil {
				tc.setup(f, cfg)
			}
			driver, cleanup, err := CreateIOSDriver(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if got := f.log.String(); got != tc.calls {
				t.Fatalf("calls %q, want %q", got, tc.calls)
			}
			if _, err := core.Unwrap(driver).(core.Capturer).Capture(); capture.ReasonOf(err, "") != tc.reason {
				t.Fatalf("Capture: %v, want %s", err, tc.reason)
			}
		})
	}
}

// The relisting wait gives up after its timeout.
func TestIOSCapture_RelistTimeout(t *testing.T) {
	f := newCaptureFixture(t)
	f.relisted = 1000
	if _, cleanup, err := CreateIOSDriver(captureCfg(capturePhone)); err != nil {
		t.Fatal(err)
	} else {
		cleanup()
	}
	if n := f.log.count("relist"); n != int(iosRelistTimeout/iosRelistPoll)+1 {
		t.Fatalf("relisted %d times", n)
	}
}

func parseTestFlow(t *testing.T, yaml string) flow.Flow {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flow.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := flow.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return *f
}

// U42: `maestro-d test` captures only when a flow can record.
func TestFlowsRecord(t *testing.T) {
	head := "appId: x\n---\n"
	cases := map[string]bool{
		"- launchApp\n- tapOn: OK\n":                                                 false,
		"- runFlow:\n    commands:\n      - tapOn: OK\n":                             false,
		"- repeat:\n    times: 2\n    commands:\n      - tapOn: OK\n":                false,
		"- startRecording: clip\n":                                                   true,
		"- repeat:\n    times: 2\n    commands:\n      - startRecording: clip\n":     true,
		"- runFlow:\n    commands:\n      - startRecording: clip\n":                  true,
		"- runFlow: other.yaml\n":                                                    true,
		"- retry:\n    maxRetries: 1\n    commands:\n      - startRecording: clip\n": true,
		"- retry:\n    maxRetries: 1\n    file: other.yaml\n":                        true,
		"- runFlow:\n    when:\n      visible: x\n    commands:\n      - tapOn: OK\n    else:\n      - startRecording: clip\n": true,
	}
	for body, want := range cases {
		if got := flowsRecord([]flow.Flow{parseTestFlow(t, head+body)}); got != want {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
	hook := parseTestFlow(t, "appId: x\nonFlowStart:\n  - startRecording: clip\n---\n- tapOn: OK\n")
	done := parseTestFlow(t, "appId: x\nonFlowComplete:\n  - startRecording: clip\n---\n- tapOn: OK\n")
	plain := parseTestFlow(t, head+"- tapOn: OK\n")
	if !flowsRecord([]flow.Flow{plain, hook}) || !flowsRecord([]flow.Flow{done}) || flowsRecord([]flow.Flow{plain}) || flowsRecord(nil) {
		t.Fatal("hooks")
	}
}
