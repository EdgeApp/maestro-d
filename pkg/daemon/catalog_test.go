package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// fakeHost stands in for simctl, adb, avdmanager and sdkmanager: a live
// device list the hooks update, and a log of every tool call.
type fakeHost struct {
	mu    sync.Mutex
	live  []DeviceInfo
	calls []string
	ios   *simulator.Catalog
	droid *emulator.Catalog
	fail  map[string]error
}

func (h *fakeHost) log(s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, s)
	for prefix, err := range h.fail {
		if strings.HasPrefix(s, prefix) {
			return err
		}
	}
	return nil
}

func (h *fakeHost) list(platform string) []DeviceInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []DeviceInfo
	for _, d := range h.live {
		if platform == "" || d.Platform == platform {
			out = append(out, d)
		}
	}
	return out
}

func (h *fakeHost) set(id string, f func(d *DeviceInfo)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.live {
		if h.live[i].ID == id {
			f(&h.live[i])
		}
	}
}

func (h *fakeHost) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.live[:0]
	for _, d := range h.live {
		if d.ID != id {
			out = append(out, d)
		}
	}
	h.live = out
}

func (h *fakeHost) joined() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.calls, "; ")
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		ios: &simulator.Catalog{
			Runtimes: []simulator.CatalogRuntime{
				{ID: "com.apple.CoreSimulator.SimRuntime.iOS-18-6", Name: "iOS 18.6", Version: "18.6", Available: true,
					DeviceTypes: []string{"com.apple.CoreSimulator.SimDeviceType.iPhone-16"}},
				{ID: "com.apple.CoreSimulator.SimRuntime.iOS-26-5", Name: "iOS 26.5", Version: "26.5", Available: true,
					DeviceTypes: []string{"com.apple.CoreSimulator.SimDeviceType.iPhone-17", "com.apple.CoreSimulator.SimDeviceType.iPhone-16"}},
			},
			DeviceTypes: []simulator.CatalogDeviceType{
				{ID: "com.apple.CoreSimulator.SimDeviceType.iPhone-17", Name: "iPhone 17", ProductFamily: "iPhone", MinRuntimeVersion: "26.0.0"},
				{ID: "com.apple.CoreSimulator.SimDeviceType.iPhone-16", Name: "iPhone 16", ProductFamily: "iPhone", MinRuntimeVersion: "18.0.0"},
			},
		},
		droid: &emulator.Catalog{
			Images: []emulator.SystemImage{
				{ID: "system-images;android-34;google_apis;arm64-v8a", API: "android-34", Tag: "google_apis", ABI: "arm64-v8a", Installed: true},
				{ID: "system-images;android-35;google_apis;arm64-v8a", API: "android-35", Tag: "google_apis", ABI: "arm64-v8a", Installed: true},
				{ID: "system-images;android-35;google_apis;x86_64", API: "android-35", Tag: "google_apis", ABI: "x86_64", Installed: true},
				{ID: "system-images;android-36;google_apis;arm64-v8a", API: "android-36", Tag: "google_apis", ABI: "arm64-v8a"},
			},
			Profiles: []string{"pixel_8", "pixel_9"},
		},
		fail: map[string]error{},
	}
}

func (h *fakeHost) deps() Deps {
	return Deps{
		ListDevices: h.list,
		IOSCatalog: func() (*simulator.Catalog, error) {
			if err := h.log("ios catalog"); err != nil {
				return nil, err
			}
			return h.ios, nil
		},
		AndroidCatalog: func(available bool) (*emulator.Catalog, error) {
			if err := h.log("android catalog"); err != nil {
				return nil, err
			}
			return h.droid, nil
		},
		CreateSimulator: func(name, dt, rt string) (string, error) {
			if err := h.log("simctl create " + name + " " + dt + " " + rt); err != nil {
				return "", err
			}
			h.mu.Lock()
			h.live = append(h.live, DeviceInfo{ID: "NEW-UDID", Platform: "ios", Kind: "simulator", Name: name, State: "Shutdown"})
			h.mu.Unlock()
			return "NEW-UDID", nil
		},
		CreateAVD: func(spec emulator.AVDSpec) error {
			if err := h.log("avdmanager create " + spec.Name + " " + spec.Image + " " + spec.Profile + " " + spec.DataSize); err != nil {
				return err
			}
			h.mu.Lock()
			h.live = append(h.live, DeviceInfo{ID: AVDPrefix + spec.Name, Platform: "android", Kind: "emulator", Name: spec.Name, State: "Shutdown"})
			h.mu.Unlock()
			return nil
		},
		InstallSystemImage: func(image string) error { return h.log("sdkmanager --install " + image) },
		DeleteSimulator: func(udid string) error {
			if err := h.log("simctl delete " + udid); err != nil {
				return err
			}
			h.remove(udid)
			return nil
		},
		DeleteAVD: func(name string) error {
			if err := h.log("avdmanager delete " + name); err != nil {
				return err
			}
			h.remove(AVDPrefix + name)
			return nil
		},
	}
}

type mgmtEnv struct {
	t    *testing.T
	host *fakeHost
	s    *Server
	c    *Client
}

// newMgmtEnv starts a daemon over httptest whose boots and shutdowns go to
// the fake host: a boot turns a shut-down simulator Booted, or an AVD into
// emulator-5554 named after it.
func newMgmtEnv(t *testing.T, host *fakeHost) *mgmtEnv {
	t.Helper()
	if os.Getenv(EnvHome) == "" || !strings.HasPrefix(os.Getenv(EnvHome), os.TempDir()) {
		t.Setenv(EnvHome, t.TempDir())
	}
	prevArch, prevShut := hostArch, shutdownForDelete
	t.Cleanup(func() { hostArch, shutdownForDelete = prevArch, prevShut })
	hostArch = "arm64"
	shutdownForDelete = func(info DeviceInfo) error {
		if err := host.log("shutdown " + info.ID); err != nil {
			return err
		}
		if info.Platform == "ios" {
			host.set(info.ID, func(d *DeviceInfo) { d.State, d.Ready = "Shutdown", false })
		} else {
			host.set(info.ID, func(d *DeviceInfo) { d.ID, d.State, d.Ready = AVDPrefix+d.Name, "Shutdown", false })
		}
		return nil
	}
	return startMgmtServer(t, host)
}

func startMgmtServer(t *testing.T, host *fakeHost) *mgmtEnv {
	t.Helper()
	s := New(Config{Name: "mgmt", ReportsDir: filepath.Join(t.TempDir(), "reports")}, host.deps())
	var cleanups int32
	s.deps.NewDriver = testDeps(&cleanups, 0).NewDriver
	s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		if err := host.log("boot " + platform + " " + name); err != nil {
			return "", err
		}
		if platform == "android" {
			host.set(AVDPrefix+name, func(d *DeviceInfo) { d.ID, d.State, d.Ready = "emulator-5554", "device", true })
			return "emulator-5554", nil
		}
		host.set(name, func(d *DeviceInfo) { d.State, d.Ready = "Booted", true })
		return name, nil
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &mgmtEnv{t: t, host: host, s: s, c: NewHTTPClient(hs.URL, "", nil)}
}

func wantCode(t *testing.T, err error, code Code, contains ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", code)
	}
	if got := codeOf(t, err); got != code {
		t.Fatalf("code = %s, want %s: %v", got, code, err)
	}
	for _, s := range contains {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err.Error(), s)
		}
	}
}

func boolp(b bool) *bool { return &b }

var bg = context.Background()

// ---------------------------------------------------------------------------
// Catalog

func TestCatalog_BothPlatformsAndQuery(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	res, err := e.c.Catalog(bg, "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.IOS == nil || res.Android == nil || len(res.Errors) != 0 || !res.OK {
		t.Errorf("catalog = %+v", res)
	}
	res, err = e.c.Catalog(bg, "Android", true, "")
	if err != nil || res.IOS != nil || res.Android == nil {
		t.Errorf("android only: %+v %v", res, err)
	}
	_, err = e.c.Catalog(bg, "windows", false, "")
	wantCode(t, err, CodeUsage, "ios or android")
}

func TestCatalog_MissingTool(t *testing.T) {
	h := newFakeHost()
	h.fail["android catalog"] = errors.New("sdkmanager not found; install the Android SDK command-line tools")
	e := newMgmtEnv(t, h)
	res, err := e.c.Catalog(bg, "", false, "")
	if err != nil || res.IOS == nil || !strings.Contains(res.Errors["android"], "sdkmanager not found") {
		t.Errorf("one platform missing is reported in errors: %+v %v", res, err)
	}
	_, err = e.c.Catalog(bg, "android", false, "")
	wantCode(t, err, CodeDeviceError, "sdkmanager not found")
}

func TestCatalog_NotConfigured(t *testing.T) {
	r := NewRegistry("", nil)
	if _, e := r.Catalog("ios", false); e == nil || e.Code != CodeInternal {
		t.Errorf("ios: %v", e)
	}
	if _, e := r.Catalog("android", false); e == nil || e.Code != CodeInternal {
		t.Errorf("android: %v", e)
	}
}

// ---------------------------------------------------------------------------
// Create

func TestCreate_IOSDefaultsAndRecord(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	dev, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Create: &CreateSpec{Name: "sim"}, Boot: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	// The newest runtime, and the newest iPhone it supports.
	if !strings.Contains(e.host.joined(), "simctl create sim com.apple.CoreSimulator.SimDeviceType.iPhone-17 com.apple.CoreSimulator.SimRuntime.iOS-26-5") {
		t.Errorf("calls = %s", e.host.joined())
	}
	if dev.ID != "NEW-UDID" || dev.State != "Shutdown" || dev.Ready || dev.CreatedBy != CreatedByMaestroD || dev.OSVersion != "26.5" {
		t.Errorf("device = %+v", dev)
	}
	recs := loadCreated()
	if len(recs) != 1 || recs[0].ID != "NEW-UDID" || recs[0].Platform != "ios" || recs[0].CreatedBy != "maestro-d" || recs[0].Daemon != "mgmt" {
		t.Errorf("created.json = %+v", recs)
	}
	list, _ := e.c.Devices(bg, "")
	if len(list) != 1 || list[0].CreatedBy != CreatedByMaestroD {
		t.Errorf("listing = %+v", list)
	}
}

func TestCreate_IOSExplicitAndBoot(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	dev, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "sim", Create: &CreateSpec{Type: "iPhone 16", Runtime: "iOS-18-6"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := e.host.joined()
	if !strings.Contains(calls, "simctl create sim com.apple.CoreSimulator.SimDeviceType.iPhone-16 com.apple.CoreSimulator.SimRuntime.iOS-18-6; boot ios NEW-UDID") {
		t.Errorf("calls = %s", calls)
	}
	if dev.ID != "NEW-UDID" || !dev.Ready || dev.BootedBy != BootedByDaemon || dev.CreatedBy != CreatedByMaestroD {
		t.Errorf("booted device = %+v", dev)
	}
	// Recorded as booted by this daemon, so a plain `device stop` works.
	if recs := e.s.registry.Records(); len(recs) != 1 || recs[0].BootedBy != BootedByDaemon {
		t.Errorf("records = %+v", recs)
	}
}

func TestCreate_IOSRefusals(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "OLD", Platform: "ios", Kind: "simulator", Name: "Taken", State: "Shutdown"}}
	e := newMgmtEnv(t, h)
	for _, tc := range []struct {
		spec CreateSpec
		want string
	}{
		{CreateSpec{Name: "taken"}, "already exists"},
		{CreateSpec{Name: "x", Runtime: "iOS-99"}, "installed: iOS-18-6, iOS-26-5"},
		{CreateSpec{Name: "x", Type: "Nokia"}, "no simulator device type"},
		{CreateSpec{Name: "x", Type: "iPhone 17", Runtime: "18.6"}, "not supported"},
		{CreateSpec{Name: "x", Image: "system-images;a;b;c"}, "--image does not apply to ios"},
		{CreateSpec{Name: "x", DataSize: "8G"}, "--data-size does not apply"},
		{CreateSpec{Name: " "}, "--name is required"},
	} {
		_, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Create: &tc.spec})
		wantCode(t, err, CodeUsage, tc.want)
	}
	_, err := e.c.CreateDevice(bg, BootRequest{Name: "x"})
	wantCode(t, err, CodeUsage, "--platform ios|android is required")
	if strings.Contains(h.joined(), "simctl create") {
		t.Errorf("a refusal must create nothing: %s", h.joined())
	}
}

func TestCreate_IOSToolFailures(t *testing.T) {
	h := newFakeHost()
	h.fail["ios catalog"] = errors.New("xcrun not found")
	e := newMgmtEnv(t, h)
	_, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "x"})
	wantCode(t, err, CodeDeviceError, "xcrun not found")

	h = newFakeHost()
	h.fail["simctl create"] = errors.New("simctl create failed")
	e = newMgmtEnv(t, h)
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "x"})
	wantCode(t, err, CodeDeviceError, "simctl create failed")

	h = newFakeHost()
	h.fail["boot"] = errors.New("boot timed out")
	e = newMgmtEnv(t, h)
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "x"})
	wantCode(t, err, CodeDeviceError, "x was created as NEW-UDID but did not boot: boot timed out")

	h = newFakeHost()
	h.ios = &simulator.Catalog{}
	e = newMgmtEnv(t, h)
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "x"})
	wantCode(t, err, CodeUsage, "no iOS simulator runtime")

	r := NewRegistry("", nil)
	if _, e := r.Create(bg, BootRequest{Platform: "ios", Create: &CreateSpec{Name: "x"}}); e == nil || e.Code != CodeInternal {
		t.Errorf("unconfigured: %v", e)
	}
}

func TestCreate_AndroidDefaultsForHostABI(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	dev, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix"}, Boot: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	// arm64 → the newest installed arm64-v8a google_apis image (36 is not
	// installed), pixel_8, and a 6G data partition.
	if !strings.Contains(e.host.joined(), "avdmanager create pix system-images;android-35;google_apis;arm64-v8a pixel_8 6G") {
		t.Errorf("calls = %s", e.host.joined())
	}
	if dev.ID != "avd:pix" || dev.State != "Shutdown" || dev.Kind != "emulator" || dev.CreatedBy != CreatedByMaestroD {
		t.Errorf("device = %+v", dev)
	}
	if recs := loadCreated(); len(recs) != 1 || recs[0].ID != "pix" || recs[0].Platform != "android" {
		t.Errorf("created.json = %+v", recs)
	}

	hostArch = "amd64"
	if _, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix86", DataSize: "8G"}, Boot: boolp(false)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.host.joined(), "avdmanager create pix86 system-images;android-35;google_apis;x86_64 pixel_8 8G") {
		t.Errorf("amd64 calls = %s", e.host.joined())
	}
}

func TestCreate_AndroidBootReturnsSerial(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	dev, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Name: "pix",
		Create: &CreateSpec{Image: "system-images;android-34;google_apis;arm64-v8a", Profile: "pixel_9"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.host.joined(), "avdmanager create pix system-images;android-34;google_apis;arm64-v8a pixel_9 6G; boot android pix") {
		t.Errorf("calls = %s", e.host.joined())
	}
	if dev.ID != "emulator-5554" || dev.Name != "pix" || !dev.Ready || dev.CreatedBy != CreatedByMaestroD {
		t.Errorf("device = %+v", dev)
	}
}

func TestCreate_AndroidImageNotInstalled(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	img := "system-images;android-36;google_apis;arm64-v8a"
	_, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix", Image: img}})
	wantCode(t, err, CodeUsage, "is not installed", `sdkmanager --install "`+img+`"`)
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix", Image: "system-images;android-99;x;y", InstallImage: true}})
	wantCode(t, err, CodeUsage, "--install-image needs --accept-licenses")
	if strings.Contains(e.host.joined(), "avdmanager create") || strings.Contains(e.host.joined(), "sdkmanager") {
		t.Errorf("a refused create must install and create nothing: %s", e.host.joined())
	}

	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "android", Boot: boolp(false),
		Create: &CreateSpec{Name: "pix", Image: img, InstallImage: true, AcceptLicenses: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.host.joined(), "sdkmanager --install "+img+"; avdmanager create pix "+img) {
		t.Errorf("calls = %s", e.host.joined())
	}

	e.host.fail["sdkmanager"] = errors.New("download failed")
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix2", Image: img, InstallImage: true, AcceptLicenses: true}})
	wantCode(t, err, CodeDeviceError, "download failed")

	e.s.registry.deps.InstallSystemImage = nil
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "pix2", Image: img, InstallImage: true, AcceptLicenses: true}})
	wantCode(t, err, CodeInternal)
}

func TestCreate_AndroidRefusals(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{
		{ID: "avd:Shut", Platform: "android", Kind: "emulator", Name: "Shut", State: "Shutdown"},
		{ID: "emulator-5556", Platform: "android", Kind: "emulator", Name: "Running", State: "device", Ready: true},
	}
	e := newMgmtEnv(t, h)
	for _, tc := range []struct {
		spec CreateSpec
		code Code
		want string
	}{
		{CreateSpec{Name: "Shut"}, CodeUsage, "already exists"},
		{CreateSpec{Name: "Running"}, CodeUsage, "already exists"},
		{CreateSpec{Name: "bad name"}, CodeUsage, "invalid AVD name"},
		{CreateSpec{Name: "x", DataSize: "12G"}, CodeUsage, "above the 10G limit"},
		{CreateSpec{Name: "x", DataSize: "lots"}, CodeUsage, "invalid data size"},
		{CreateSpec{Name: "x", Image: "android-35"}, CodeUsage, "sdkmanager package path"},
		{CreateSpec{Name: "x", Profile: "nokia"}, CodeUsage, "no hardware profile"},
		{CreateSpec{Name: "x", Type: "iPhone 16"}, CodeUsage, "--type does not apply to android"},
	} {
		_, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &tc.spec})
		wantCode(t, err, tc.code, tc.want)
	}
	if strings.Contains(h.joined(), "avdmanager create") {
		t.Errorf("a refusal must create nothing: %s", h.joined())
	}

	hostArch = "386"
	_, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "x"}})
	wantCode(t, err, CodeUsage, "no google_apis system image for x86 is installed", "sdkmanager --install")
}

func TestCreate_AndroidToolFailures(t *testing.T) {
	h := newFakeHost()
	h.fail["android catalog"] = errors.New("sdkmanager not found")
	e := newMgmtEnv(t, h)
	_, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Name: "x"})
	wantCode(t, err, CodeDeviceError, "sdkmanager not found")

	h = newFakeHost()
	h.fail["avdmanager create"] = errors.New("avdmanager failed")
	e = newMgmtEnv(t, h)
	_, err = e.c.CreateDevice(bg, BootRequest{Platform: "android", Name: "x"})
	wantCode(t, err, CodeDeviceError, "avdmanager failed")

	r := NewRegistry("", nil)
	if _, e := r.Create(bg, BootRequest{Platform: "android", Create: &CreateSpec{Name: "x"}}); e == nil || e.Code != CodeInternal {
		t.Errorf("unconfigured: %v", e)
	}
}

func TestCreatedJSON_SurvivesRestart(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	h := newFakeHost()
	first := newMgmtEnv(t, h)
	if _, err := first.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "sim", Boot: boolp(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv(EnvHome), "created.json")); err != nil {
		t.Fatalf("created.json under the home: %v", err)
	}
	// A new daemon process on the same home.
	second := startMgmtServer(t, h)
	list, _ := second.c.Devices(bg, "ios")
	if len(list) != 1 || list[0].CreatedBy != CreatedByMaestroD {
		t.Errorf("the record survives a restart: %+v", list)
	}
	if _, err := second.c.DeleteDevice(bg, "NEW-UDID", false); err != nil {
		t.Fatalf("delete without force after a restart: %v", err)
	}
	if recs := loadCreated(); len(recs) != 0 {
		t.Errorf("delete drops the record: %+v", recs)
	}
}

func TestCreatedJSON_CorruptAndReplace(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	if err := os.WriteFile(CreatedPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if recs := loadCreated(); recs != nil {
		t.Errorf("corrupt file reads as empty: %+v", recs)
	}
	for i := 0; i < 2; i++ {
		if err := recordCreated(CreatedRecord{Platform: "ios", ID: "A", Name: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordCreated(CreatedRecord{Platform: "android", ID: "A"}); err != nil {
		t.Fatal(err)
	}
	if recs := loadCreated(); len(recs) != 2 || !recs.has("ios", "A") || !recs.has("android", "A") || recs.has("ios", "") {
		t.Errorf("records = %+v", recs)
	}
	if err := forgetCreated("ios", "missing"); err != nil {
		t.Fatal(err)
	}
	if err := forgetCreated("ios", "A"); err != nil || loadCreated().has("ios", "A") || !loadCreated().has("android", "A") {
		t.Errorf("forget: %v %+v", err, loadCreated())
	}
}

// ---------------------------------------------------------------------------
// Delete

func TestDelete_RefusesExternalWithoutForce(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "EXT", Platform: "ios", Kind: "simulator", Name: "Mine", State: "Booted", Ready: true}}
	e := newMgmtEnv(t, h)
	if _, err := e.c.Attach(bg, "EXT", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.c.DeleteDevice(bg, "EXT", false)
	wantCode(t, err, CodeDeviceExternal, "not created by maestro-d", "--force")
	if h.joined() != "" {
		t.Errorf("a refused delete touches nothing: %s", h.joined())
	}
	if d, _ := e.c.Device(bg, "EXT"); !d.Attached {
		t.Error("a refused delete must not detach")
	}
	// Forced, by name: detached, shut down, then deleted.
	dev, err := e.c.DeleteDevice(bg, "mine", true)
	if err != nil {
		t.Fatal(err)
	}
	if h.joined() != "shutdown EXT; simctl delete EXT" || dev.ID != "EXT" {
		t.Errorf("calls = %s, device = %+v", h.joined(), dev)
	}
	if _, err := e.c.Device(bg, "EXT"); err == nil {
		t.Error("the deleted simulator is still listed")
	}
}

func TestDelete_StopsBootedDeviceFirst(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	if _, err := e.c.CreateDevice(bg, BootRequest{Platform: "ios", Name: "sim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.DeleteDevice(bg, "NEW-UDID", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.host.joined(), "boot ios NEW-UDID; shutdown NEW-UDID; simctl delete NEW-UDID") {
		t.Errorf("calls = %s", e.host.joined())
	}
	if len(e.s.registry.Records()) != 0 {
		t.Errorf("records = %+v", e.s.registry.Records())
	}
}

func TestDelete_RunningEmulatorDeletesItsAVD(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	dev, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Name: "pix"})
	if err != nil {
		t.Fatal(err)
	}
	if dev.ID != "emulator-5554" {
		t.Fatalf("booted id = %s", dev.ID)
	}
	if _, err := e.c.DeleteDevice(bg, "emulator-5554", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.host.joined(), "shutdown emulator-5554; avdmanager delete pix") {
		t.Errorf("calls = %s", e.host.joined())
	}
	if loadCreated().has("android", "pix") {
		t.Error("record kept")
	}
}

func TestDelete_ShutdownAVDByName(t *testing.T) {
	e := newMgmtEnv(t, newFakeHost())
	if _, err := e.c.CreateDevice(bg, BootRequest{Platform: "android", Name: "pix", Boot: boolp(false)}); err != nil {
		t.Fatal(err)
	}
	// "pix" matches avd:pix; a shut-down AVD is not stopped.
	if _, err := e.c.DeleteDevice(bg, "pix", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.host.joined(), "shutdown") || !strings.HasSuffix(e.host.joined(), "avdmanager delete pix") {
		t.Errorf("calls = %s", e.host.joined())
	}
}

func TestDelete_Errors(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{
		{ID: "PHONE", Platform: "ios", Kind: "device", Name: "phone", Ready: true},
		{ID: "emulator-5556", Platform: "android", Kind: "emulator", State: "device", Ready: true},
		{ID: "S1", Platform: "ios", Kind: "simulator", Name: "Twin", State: "Shutdown"},
		{ID: "S2", Platform: "ios", Kind: "simulator", Name: "Twin", State: "Shutdown"},
		{ID: "S3", Platform: "ios", Kind: "simulator", Name: "Solo", State: "Booted"},
	}
	e := newMgmtEnv(t, h)
	_, err := e.c.DeleteDevice(bg, "nothing", true)
	wantCode(t, err, CodeDeviceNotFound)
	_, err = e.c.DeleteDevice(bg, "PHONE", true)
	wantCode(t, err, CodeUsage, "physical device")
	_, err = e.c.DeleteDevice(bg, "emulator-5556", true)
	wantCode(t, err, CodeDeviceError, "AVD name")
	_, err = e.c.DeleteDevice(bg, "twin", true)
	wantCode(t, err, CodeUsage, "names 2 devices", "S1, S2")

	h.fail["shutdown"] = errors.New("would not stop")
	_, err = e.c.DeleteDevice(bg, "Solo", true)
	wantCode(t, err, CodeDeviceError, "would not stop")
	delete(h.fail, "shutdown")
	h.fail["simctl delete"] = errors.New("simctl delete failed")
	_, err = e.c.DeleteDevice(bg, "S1", true)
	wantCode(t, err, CodeDeviceError, "simctl delete failed")

	e.s.registry.deps.DeleteSimulator = nil
	_, err = e.c.DeleteDevice(bg, "S1", true)
	wantCode(t, err, CodeInternal)
}

func TestDelete_BodyFlag(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "S1", Platform: "ios", Kind: "simulator", Name: "a", State: "Shutdown"}}
	e := newMgmtEnv(t, h)
	var out DeviceResult
	if er := e.c.call(bg, http.MethodDelete, devPath("S1", ""), StopDeviceRequest{Delete: true, Force: true}, &out); er != nil {
		t.Fatal(er)
	}
	if out.Device.ID != "S1" || h.joined() != "simctl delete S1" {
		t.Errorf("delete from the body: %+v %s", out, h.joined())
	}
}

// ---------------------------------------------------------------------------
// Boot without --platform, and avd: ids

func TestBoot_InfersPlatform(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{
		{ID: "UDID-1", Platform: "ios", Kind: "simulator", Name: "My Phone", State: "Shutdown"},
		{ID: "avd:pix", Platform: "android", Kind: "emulator", Name: "pix", State: "Shutdown"},
		{ID: "UDID-2", Platform: "ios", Kind: "simulator", Name: "both", State: "Shutdown"},
		{ID: "avd:both", Platform: "android", Kind: "emulator", Name: "both", State: "Shutdown"},
	}
	e := newMgmtEnv(t, h)

	dev, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || dev.ID != "emulator-5554" || dev.Platform != "android" || dev.Name != "pix" || dev.BootedBy != BootedByDaemon {
		t.Errorf("avd:pix = %+v, %v", dev, err)
	}
	// Already running under its AVD name: returned as is, not booted again.
	again, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || again.ID != "emulator-5554" || strings.Count(h.joined(), "boot android") != 1 {
		t.Errorf("second start = %+v, %v (%s)", again, err, h.joined())
	}

	for _, name := range []string{"UDID-1", "my phone"} {
		h.set("UDID-1", func(d *DeviceInfo) { d.State, d.Ready = "Shutdown", false })
		dev, err = e.c.StartDevice(bg, BootRequest{Name: name})
		if err != nil || dev.Platform != "ios" {
			t.Errorf("%q = %+v, %v", name, dev, err)
		}
	}
	_, err = e.c.StartDevice(bg, BootRequest{Name: "nothing"})
	wantCode(t, err, CodeUsage, "pass --platform")
	_, err = e.c.StartDevice(bg, BootRequest{Name: "both"})
	wantCode(t, err, CodeUsage, "both an iOS simulator and an Android device")
	_, err = e.c.StartDevice(bg, BootRequest{Name: "x", Platform: "web"})
	wantCode(t, err, CodeUsage, "ios or android")
}

func TestStop_ShutdownAVDIsANoOp(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "avd:pix", Platform: "android", Kind: "emulator", Name: "pix", State: "Shutdown"}}
	e := newMgmtEnv(t, h)
	if err := e.c.StopDevice(bg, "avd:pix", false); err != nil || h.joined() != "" {
		t.Errorf("stop avd:pix: %v %s", err, h.joined())
	}
}

func TestCreatedKey(t *testing.T) {
	for _, tc := range []struct {
		d    DeviceInfo
		want string
	}{
		{DeviceInfo{ID: "U", Platform: "ios", Kind: "simulator"}, "U"},
		{DeviceInfo{ID: "U", Platform: "ios", Kind: "device"}, ""},
		{DeviceInfo{ID: "avd:p", Platform: "android", Kind: "emulator"}, "p"},
		{DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "p"}, "p"},
		{DeviceInfo{ID: "R58M", Platform: "android", Kind: "device"}, ""},
	} {
		if got := createdKey(tc.d); got != tc.want {
			t.Errorf("createdKey(%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestStatus_ListsHeldDevicesWithoutDiscovery(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{
		{ID: "S1", Platform: "ios", Kind: "simulator", Name: "a", State: "Shutdown"},
		{ID: "S2", Platform: "ios", Kind: "simulator", Name: "b", State: "Shutdown"},
		{ID: "S3", Platform: "ios", Kind: "simulator", Name: "c", State: "Booted", Ready: true},
	}
	e := newMgmtEnv(t, h)
	if _, err := e.c.StartDevice(bg, BootRequest{Platform: "ios", Name: "S1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Attach(bg, "S3", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e.s.registry.list = func(p string) []DeviceInfo { calls++; return h.list(p) }
	st, err := e.c.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("status ran device discovery %d times", calls)
	}
	ids := []string{}
	for _, d := range st.Devices {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "S1,S3" {
		t.Errorf("status devices = %v, want the booted S1 and the attached S3", ids)
	}
}
