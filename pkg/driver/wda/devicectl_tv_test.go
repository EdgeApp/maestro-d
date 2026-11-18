package wda

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

const devicectlTVList = `{"result":{"devices":[
 {"connectionProperties":{"pairingState":"paired","tunnelState":"connected","tunnelIPAddress":"fd00::1"},
  "deviceProperties":{"name":"Lab TV","osVersionNumber":"26.3","developerModeStatus":"enabled"},
  "hardwareProperties":{"platform":"tvOS","reality":"physical","udid":"tv-1"}},
 {"connectionProperties":{"pairingState":"paired","tunnelState":"disconnected"},
  "deviceProperties":{"name":"Hall TV","osVersionNumber":"27.2.1","developerModeStatus":"enabled"},
  "hardwareProperties":{"platform":"tvOS","reality":"physical","udid":"tv-2"}},
 {"connectionProperties":{"pairingState":"paired","tunnelState":"unavailable"},
  "deviceProperties":{"name":"Away","osVersionNumber":"26.6.1","developerModeStatus":"enabled"},
  "hardwareProperties":{"platform":"tvOS","reality":"physical","udid":"tv-4"}},
 {"connectionProperties":{"pairingState":"unpaired","tunnelState":"disconnected"},
  "deviceProperties":{"name":"Den","osVersionNumber":"26.1","developerModeStatus":"disabled"},
  "hardwareProperties":{"platform":"tvOS","reality":"physical","udid":"tv-3"}},
 {"connectionProperties":{"pairingState":"paired","tunnelState":"connected"},
  "deviceProperties":{"name":"Phone","osVersionNumber":"26.6","developerModeStatus":"enabled"},
  "hardwareProperties":{"platform":"iOS","reality":"physical","udid":"phone-1"}},
 {"connectionProperties":{},
  "deviceProperties":{"name":"TV sim","osVersionNumber":"27.2"},
  "hardwareProperties":{"platform":"tvOS","reality":"simulated","udid":"sim-1"}},
 {"connectionProperties":{},"deviceProperties":{"name":"No udid"},
  "hardwareProperties":{"platform":"tvOS","reality":"physical"}}
]}}`

func TestParseDevicectlTVs(t *testing.T) {
	tvs, err := parseDevicectlTVs([]byte(devicectlTVList))
	if err != nil {
		t.Fatal(err)
	}
	want := []DevicectlTV{
		{UDID: "tv-1", Name: "Lab TV", OSVersion: "26.3", Paired: true, Reachable: true, Tunnel: true},
		{UDID: "tv-2", Name: "Hall TV", OSVersion: "27.2.1", Paired: true, Reachable: true},
		{UDID: "tv-4", Name: "Away", OSVersion: "26.6.1", Paired: true},
		{UDID: "tv-3", Name: "Den", OSVersion: "26.1", Reachable: true},
	}
	if len(tvs) != len(want) {
		t.Fatalf("got %+v", tvs)
	}
	for i := range want {
		if tvs[i] != want[i] {
			t.Fatalf("tv %d: got %+v; want %+v", i, tvs[i], want[i])
		}
	}
	for i, ready := range []bool{true, true, false, false} {
		if tvs[i].Ready() != ready {
			t.Fatalf("tv %d ready %v", i, tvs[i].Ready())
		}
	}
	if _, err := parseDevicectlTVs([]byte("nope")); err == nil {
		t.Fatal("bad JSON parsed")
	}
}

// fakeDevicectl puts an xcrun on PATH that writes body to --json-output, or
// fails with exit status 1 when body is "".
func fakeDevicectl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	if body == "" {
		script += "echo 'devicectl broke' >&2\nexit 1\n"
	} else {
		data := filepath.Join(dir, "out.json")
		if err := os.WriteFile(data, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		script += "while [ $# -gt 0 ]; do if [ \"$1\" = --json-output ]; then cp " + data + " \"$2\"; fi; shift; done\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestListDevicectlTVs(t *testing.T) {
	fakeDevicectl(t, devicectlTVList)
	tvs, err := ListDevicectlTVs(5 * time.Second)
	if err != nil || len(tvs) != 4 || tvs[0].UDID != "tv-1" {
		t.Fatalf("got %+v, %v", tvs, err)
	}
}

func TestListDevicectlTVs_Fails(t *testing.T) {
	fakeDevicectl(t, "")
	if _, err := ListDevicectlTVs(5 * time.Second); err == nil {
		t.Fatal("a failing devicectl listed TVs")
	}
}

func TestListDevicectlTVs_TimesOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	if _, err := ListDevicectlTVs(200 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestConfigureRuntime_PhysicalTV(t *testing.T) {
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", "")
	noSims := func() ([]simulator.SimulatorDevice, error) { return nil, nil }
	for _, tc := range []struct {
		name    string
		tv      DevicectlTV
		isTV    bool
		err     error
		wantTV  bool
		version string
		wantErr bool
	}{
		{name: "a paired Apple TV", tv: DevicectlTV{OSVersion: "26.3"}, isTV: true, wantTV: true, version: "26.3"},
		{name: "a phone or a device devicectl does not know"},
		{name: "devicectl fails", err: errors.New("devicectl wedged"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRunner("dev-1", "", "")
			r.listSimulators = noSims
			var asked string
			r.lookupTV = func(udid string) (DevicectlTV, bool, error) { asked = udid; return tc.tv, tc.isTV, tc.err }
			err := r.configureRuntime()
			if (err != nil) != tc.wantErr || (err != nil && !strings.Contains(err.Error(), "devicectl wedged")) {
				t.Fatalf("err %v", err)
			}
			if asked != "dev-1" || r.IsTV() != tc.wantTV || r.runtimeVersion != tc.version {
				t.Fatalf("asked %q, TV %v, version %q", asked, r.IsTV(), r.runtimeVersion)
			}
			if tc.wantErr && r.configureRuntime() == nil {
				t.Fatal("the error was not kept")
			}
		})
	}
}

func TestConfigureRuntime_SimulatorSkipsDevicectl(t *testing.T) {
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", "")
	r := NewRunner("sim-1", "", "")
	r.listSimulators = func() ([]simulator.SimulatorDevice, error) {
		return []simulator.SimulatorDevice{{UDID: "sim-1", Platform: "iOS", OSVersion: "27.2", IsAvailable: true}}, nil
	}
	r.lookupTV = func(string) (DevicectlTV, bool, error) {
		t.Fatal("asked devicectl about a simulator")
		return DevicectlTV{}, false, nil
	}
	if err := r.configureRuntime(); err != nil || r.IsTV() {
		t.Fatalf("TV %v, %v", r.IsTV(), err)
	}
}

func TestConfigureRuntime_SimctlFailsStillFindsTV(t *testing.T) {
	t.Setenv("MAESTRO_TV_REMOTE_CONFIG", "")
	r := NewRunner("tv-1", "", "")
	r.listSimulators = func() ([]simulator.SimulatorDevice, error) { return nil, errors.New("no simctl") }
	r.lookupTV = func(string) (DevicectlTV, bool, error) { return DevicectlTV{OSVersion: "26.3"}, true, nil }
	if err := r.configureRuntime(); err != nil || !r.IsTV() {
		t.Fatalf("TV %v, %v", r.IsTV(), err)
	}
	if r.runnerScheme() != "WebDriverAgentRunner_tvOS" || r.destination() != "platform=tvOS,id=tv-1" {
		t.Fatalf("scheme %s, destination %s", r.runnerScheme(), r.destination())
	}
}

func TestPhysicalTVLookup(t *testing.T) {
	fakeDevicectl(t, "")
	if _, _, err := physicalTVLookup("tv-1"); err == nil {
		t.Fatal("a failing devicectl answered")
	}
	fakeDevicectl(t, devicectlTVList)
	if tv, ok, err := physicalTVLookup("tv-1"); err != nil || !ok || tv.OSVersion != "26.3" {
		t.Fatalf("got %+v %v %v", tv, ok, err)
	}
	if _, ok, err := physicalTVLookup("phone-1"); err != nil || ok {
		t.Fatalf("a phone is a TV: %v %v", ok, err)
	}
}

func TestDriverIsTV(t *testing.T) {
	d := &Driver{}
	if d.IsTV() {
		t.Fatal("TV by default")
	}
	d.SetTVMode(true)
	if !d.IsTV() {
		t.Fatal("SetTVMode did not stick")
	}
}

// An attach's own devicectl lookup records the TV, so a hub knows it before
// the attach builds WebDriverAgent; it stays known after devicectl fails.
func TestKnownTV_FromLookup(t *testing.T) {
	forgetTVs()
	t.Cleanup(forgetTVs)
	if _, ok := KnownTV("tv-1"); ok {
		t.Fatal("known before any reading")
	}
	fakeDevicectl(t, devicectlTVList)
	if _, ok, err := physicalTVLookup("tv-1"); err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}
	fakeDevicectl(t, "")
	if name, ok := KnownTV("tv-1"); !ok || name != "Lab TV" {
		t.Fatalf("KnownTV: %q %v", name, ok)
	}
	if _, ok := KnownTV("phone-1"); ok {
		t.Fatal("a phone is a known TV")
	}
}
