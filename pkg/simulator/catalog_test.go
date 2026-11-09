package simulator

import (
	"os/exec"
	"strings"
	"testing"
)

const catalogRuntimesJSON = `{"runtimes": [
  {"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-26-5", "name": "iOS 26.5", "version": "26.5",
   "buildversion": "23F77", "platform": "iOS", "isAvailable": true,
   "supportedDeviceTypes": [
     {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", "name": "iPhone 17 Pro", "productFamily": "iPhone"},
     {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16", "name": "iPhone 16", "productFamily": "iPhone"},
     {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPad-Air", "name": "iPad Air", "productFamily": "iPad"},
     {"identifier": "com.apple.CoreSimulator.SimDeviceType.Watch-10", "name": "Apple Watch", "productFamily": "Apple Watch"}
   ]},
  {"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-26-10", "name": "iOS 26.10", "version": "26.10",
   "platform": "iOS", "isAvailable": false, "supportedDeviceTypes": []},
  {"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-18-6", "name": "iOS 18.6", "version": "18.6",
   "platform": "iOS", "isAvailable": true,
   "supportedDeviceTypes": [
     {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16", "name": "iPhone 16", "productFamily": "iPhone"}
   ]},
  {"identifier": "com.apple.CoreSimulator.SimRuntime.tvOS-26-0", "name": "tvOS 26.0", "version": "26.0",
   "platform": "tvOS", "isAvailable": true, "supportedDeviceTypes": []}
]}`

const catalogTypesJSON = `{"devicetypes": [
  {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", "name": "iPhone 17 Pro", "productFamily": "iPhone", "minRuntimeVersionString": "26.0.0"},
  {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16", "name": "iPhone 16", "productFamily": "iPhone", "minRuntimeVersionString": "18.0.0"},
  {"identifier": "com.apple.CoreSimulator.SimDeviceType.iPad-Air", "name": "iPad Air", "productFamily": "iPad", "minRuntimeVersionString": "26.0.0"},
  {"identifier": "com.apple.CoreSimulator.SimDeviceType.Apple-TV", "name": "Apple TV", "productFamily": "Apple TV"}
]}`

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := parseCatalog([]byte(catalogRuntimesJSON), []byte(catalogTypesJSON))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	return cat
}

func TestParseCatalog(t *testing.T) {
	cat := testCatalog(t)
	if len(cat.Runtimes) != 3 {
		t.Fatalf("runtimes = %+v, want the three iOS ones", cat.Runtimes)
	}
	rt := cat.Runtimes[0]
	if rt.ID != "com.apple.CoreSimulator.SimRuntime.iOS-26-5" || rt.Name != "iOS 26.5" || rt.Version != "26.5" || rt.Build != "23F77" || !rt.Available {
		t.Errorf("runtime = %+v", rt)
	}
	if len(rt.DeviceTypes) != 3 {
		t.Errorf("supported types = %v, want the iPhones and iPad only", rt.DeviceTypes)
	}
	if len(cat.DeviceTypes) != 3 || cat.DeviceTypes[2].ProductFamily != "iPad" || cat.DeviceTypes[0].MinRuntimeVersion != "26.0.0" {
		t.Errorf("device types = %+v, want iPhone and iPad types only", cat.DeviceTypes)
	}
}

func TestParseCatalog_EmptyOutput(t *testing.T) {
	cat, err := parseCatalog(nil, []byte("  \n"))
	if err != nil {
		t.Fatalf("empty output: %v", err)
	}
	if cat.Runtimes == nil || cat.DeviceTypes == nil || len(cat.Runtimes) != 0 || len(cat.DeviceTypes) != 0 {
		t.Errorf("want empty, non-nil lists: %+v", cat)
	}
	if _, err := cat.DefaultRuntime(); err == nil || !strings.Contains(err.Error(), "Xcode") {
		t.Errorf("DefaultRuntime on an empty catalog: %v", err)
	}
}

func TestParseCatalog_InvalidJSON(t *testing.T) {
	if _, err := parseCatalog([]byte("nope"), nil); err == nil || !strings.Contains(err.Error(), "runtimes") {
		t.Errorf("bad runtimes JSON: %v", err)
	}
	if _, err := parseCatalog(nil, []byte("nope")); err == nil || !strings.Contains(err.Error(), "devicetypes") {
		t.Errorf("bad devicetypes JSON: %v", err)
	}
}

func TestListCatalog(t *testing.T) {
	withFakeLookPath(t, func(string) (string, error) { return "/usr/bin/xcrun", nil })
	var calls []string
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		calls = append(calls, strings.Join(args, " "))
		if args[2] == "runtimes" {
			return exec.Command("printf", "%s", catalogRuntimesJSON)
		}
		return exec.Command("printf", "%s", catalogTypesJSON)
	})
	cat, err := ListCatalog()
	if err != nil {
		t.Fatalf("ListCatalog: %v", err)
	}
	if len(cat.Runtimes) != 3 || len(cat.DeviceTypes) != 3 {
		t.Errorf("catalog = %+v", cat)
	}
	if strings.Join(calls, ";") != "simctl list runtimes -j;simctl list devicetypes -j" {
		t.Errorf("calls = %v", calls)
	}
}

func TestListCatalog_Failures(t *testing.T) {
	withFakeLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	if _, err := ListCatalog(); err == nil || !strings.Contains(err.Error(), "xcrun not found") || !strings.Contains(err.Error(), "xcode-select --install") {
		t.Errorf("missing xcrun should name the tool and how to install it: %v", err)
	}

	withFakeLookPath(t, func(string) (string, error) { return "/usr/bin/xcrun", nil })
	withFakeExec(t, fakeExecError())
	if _, err := ListCatalog(); err == nil || !strings.Contains(err.Error(), "list runtimes") {
		t.Errorf("runtimes failure: %v", err)
	}
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		if args[2] == "runtimes" {
			return exec.Command("printf", "%s", "{}")
		}
		return exec.Command("false")
	})
	if _, err := ListCatalog(); err == nil || !strings.Contains(err.Error(), "list devicetypes") {
		t.Errorf("devicetypes failure: %v", err)
	}
}

func TestCatalog_DefaultRuntimeIsNewestAvailable(t *testing.T) {
	rt, err := testCatalog(t).DefaultRuntime()
	if err != nil {
		t.Fatal(err)
	}
	// 26.10 is newer but unavailable; 26.5 beats 18.6 numerically.
	if rt.Version != "26.5" {
		t.Errorf("default runtime = %s, want 26.5", rt.Version)
	}
}

func TestCatalog_ResolveRuntime(t *testing.T) {
	cat := testCatalog(t)
	for _, spec := range []string{"com.apple.CoreSimulator.SimRuntime.iOS-26-5", "iOS-26-5", "ios-26-5", "iOS 26.5", "26.5", " 26.5 "} {
		rt, err := cat.ResolveRuntime(spec)
		if err != nil || rt.Version != "26.5" {
			t.Errorf("ResolveRuntime(%q) = %v, %v", spec, rt, err)
		}
	}
	if _, err := cat.ResolveRuntime("iOS-26-10"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("unavailable runtime: %v", err)
	}
	if _, err := cat.ResolveRuntime("iOS-99-0"); err == nil || !strings.Contains(err.Error(), "installed: iOS-26-5, iOS-18-6") {
		t.Errorf("unknown runtime should list the installed ones: %v", err)
	}
	empty := &Catalog{}
	if _, err := empty.ResolveRuntime("26.5"); err == nil || !strings.Contains(err.Error(), "no iOS simulator runtime is installed") {
		t.Errorf("no runtimes: %v", err)
	}
}

func TestCatalog_ResolveDeviceType(t *testing.T) {
	cat := testCatalog(t)
	rt := &cat.Runtimes[0]
	for _, spec := range []string{"iPhone 16", "iphone 16", "iPhone-16", "com.apple.CoreSimulator.SimDeviceType.iPhone-16"} {
		dt, err := cat.ResolveDeviceType(spec, rt)
		if err != nil || dt.ID != "com.apple.CoreSimulator.SimDeviceType.iPhone-16" {
			t.Errorf("ResolveDeviceType(%q) = %v, %v", spec, dt, err)
		}
	}
	if _, err := cat.ResolveDeviceType("iPhone 17 Pro", &cat.Runtimes[2]); err == nil || !strings.Contains(err.Error(), "not supported by iOS 18.6") {
		t.Errorf("unsupported type: %v", err)
	}
	if _, err := cat.ResolveDeviceType("iPhone 17 Pro", nil); err != nil {
		t.Errorf("no runtime means no support check: %v", err)
	}
	if _, err := cat.ResolveDeviceType("Nokia 3310", rt); err == nil || !strings.Contains(err.Error(), "device catalog") {
		t.Errorf("unknown type: %v", err)
	}
}

func TestCatalog_DefaultDeviceTypeIsNewestIPhone(t *testing.T) {
	cat := testCatalog(t)
	dt, err := cat.DefaultDeviceType(&cat.Runtimes[0])
	if err != nil || dt.Name != "iPhone 17 Pro" {
		t.Errorf("26.5 default = %v, %v; want iPhone 17 Pro (never the iPad)", dt, err)
	}
	dt, err = cat.DefaultDeviceType(&cat.Runtimes[2])
	if err != nil || dt.Name != "iPhone 16" {
		t.Errorf("18.6 default = %v, %v; want the newest type it supports", dt, err)
	}
	dt, err = cat.DefaultDeviceType(nil)
	if err != nil || dt.Name != "iPhone 17 Pro" {
		t.Errorf("no runtime = %v, %v", dt, err)
	}
	if _, err := cat.DefaultDeviceType(&cat.Runtimes[1]); err != nil {
		t.Errorf("a runtime without a type list falls back to every iPhone: %v", err)
	}
	only := &Catalog{DeviceTypes: []CatalogDeviceType{{ID: "x.iPad", Name: "iPad", ProductFamily: "iPad"}}}
	if _, err := only.DefaultDeviceType(&CatalogRuntime{Name: "iOS 26.5"}); err == nil || !strings.Contains(err.Error(), "iOS 26.5") {
		t.Errorf("no iPhone: %v", err)
	}
	if _, err := only.DefaultDeviceType(nil); err == nil || !strings.Contains(err.Error(), "the runtime") {
		t.Errorf("no iPhone, no runtime: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"26.5", "26.10", -1}, {"26.10", "26.5", 1}, {"18.0.0", "18", 0}, {"", "1", -1}, {"27.0", "26.9.9", 1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
