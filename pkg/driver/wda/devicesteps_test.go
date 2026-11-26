package wda

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func TestSetStatusBar_PhysicalDeviceIsUnsupported(t *testing.T) {
	d := NewDriver(&Client{}, &core.PlatformInfo{Platform: "ios"}, "DEV-1")
	r := d.Execute(&flow.SetStatusBarStep{Mode: "demo"})
	var u *core.UnsupportedError
	if r.Success || r.Message != "UNSUPPORTED: status bar override on a physical iOS device" || !asUnsupported(r.Error, &u) {
		t.Fatalf("%+v", r)
	}
}

func asUnsupported(err error, u **core.UnsupportedError) bool {
	v, ok := err.(*core.UnsupportedError)
	*u = v
	return ok
}

func TestSetStatusBar_SimulatorRunsSimctl(t *testing.T) {
	var got [][]string
	prev := core.RunXcrun
	core.RunXcrun = func(args ...string) ([]byte, error) { got = append(got, args); return nil, nil }
	t.Cleanup(func() { core.RunXcrun = prev })
	d := NewDriver(&Client{}, &core.PlatformInfo{Platform: "ios", IsSimulator: true}, "SIM-1")
	battery := 80
	if r := d.Execute(&flow.SetStatusBarStep{Mode: "demo", Time: "10:30", Battery: &battery}); !r.Success {
		t.Fatalf("%+v", r)
	}
	want := []string{"simctl", "status_bar", "SIM-1", "override", "--time", "10:30", "--batteryState", "charged", "--batteryLevel", "80",
		"--wifiBars", "3", "--cellularBars", "4", "--dataNetwork", "wifi"}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("%q", got)
	}
}

func TestKeyboardVisible(t *testing.T) {
	var sent bool
	server := collapsedFieldServer(t, &sent)
	defer server.Close()
	r := createTestDriver(server).Execute(&flow.KeyboardVisibleStep{})
	if !r.Success || r.Data.(map[string]any)["visible"] != true {
		t.Fatalf("%+v", r)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"value":{"error":"unknown error","message":"boom"}}`))
	}))
	defer down.Close()
	if r := createTestDriver(down).Execute(&flow.KeyboardVisibleStep{}); r.Success {
		t.Fatalf("%+v", r)
	}
}

func TestSampleProbe(t *testing.T) {
	d := NewDriver(&Client{}, &core.PlatformInfo{Platform: "ios"}, "DEV-1")
	if _, err := d.SampleProbe(core.SampleCPU, "com.x"); err == nil {
		t.Fatal("a physical device sampled")
	}
	d = NewDriver(&Client{}, &core.PlatformInfo{Platform: "ios", IsSimulator: true}, "SIM-1")
	if p, err := d.SampleProbe(core.SampleMemory, "com.x"); err != nil || p == nil {
		t.Fatal(err)
	}
}
