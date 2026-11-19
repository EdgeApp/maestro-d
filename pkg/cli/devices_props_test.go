package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/device"
)

func withAndroidDevices(t *testing.T, devs []device.DeviceWithProps, err error) *int {
	t.Helper()
	prev := listAndroidDevices
	t.Cleanup(func() { listAndroidDevices = prev })
	calls := 0
	listAndroidDevices = func() ([]device.DeviceWithProps, error) { calls++; return devs, err }
	return &calls
}

func androidFixture() []device.DeviceWithProps {
	return []device.DeviceWithProps{
		{ConnectedDevice: device.ConnectedDevice{Serial: "FAKEPIXEL0001", State: "device", Type: "device"}, TransportID: "113",
			Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16"},
		{ConnectedDevice: device.ConnectedDevice{Serial: "emulator-5554", State: "device", Type: "emulator"}, TransportID: "7",
			Name: "sdk_gphone64_arm64", Model: "sdk_gphone64_arm64", OSVersion: "15"},
		{ConnectedDevice: device.ConnectedDevice{Serial: "OFF", State: "offline", Type: "device"}},
	}
}

func entriesByID(entries []DeviceEntry) map[string]DeviceEntry {
	m := map[string]DeviceEntry{}
	for _, e := range entries {
		m[e.ID] = e
	}
	return m
}

func TestAndroidEntriesCarryProps(t *testing.T) {
	withAndroidDevices(t, androidFixture(), nil)
	m := entriesByID(androidEntries())
	if p := m["FAKEPIXEL0001"]; p.Name != "Lab Pixel" || p.Model != "Pixel 8" || p.OSVersion != "16" || !p.Ready || p.Kind != "device" || p.Platform != "android" {
		t.Errorf("phone %+v", p)
	}
	if e := m["emulator-5554"]; e.Name != "" || e.Model != "sdk_gphone64_arm64" || e.OSVersion != "15" || e.Kind != "emulator" {
		t.Errorf("emulator %+v: model and version only, its name is its AVD's", e)
	}
	if o := m["OFF"]; o.Name != "" || o.Model != "" || o.OSVersion != "" || o.Ready {
		t.Errorf("offline %+v", o)
	}
}

func TestAndroidEntriesListingFails(t *testing.T) {
	withAndroidDevices(t, nil, errors.New("adb not found"))
	if got := androidEntries(); got != nil {
		t.Errorf("entries %+v", got)
	}
}

// avdEntries names the emulator by its AVD and keeps the props.
func TestCollectDevicesNamesEmulatorsByAVDWithProps(t *testing.T) {
	withAndroidDevices(t, androidFixture(), nil)
	withAVDs(t, map[string]string{"emulator-5554": "Pixel_8_API_35"}, nil)
	m := entriesByID(collectDevices("android", false))
	if e := m["emulator-5554"]; e.Name != "Pixel_8_API_35" || e.Model != "sdk_gphone64_arm64" || e.OSVersion != "15" {
		t.Errorf("emulator %+v", e)
	}
	if p := m["FAKEPIXEL0001"]; p.Name != "Lab Pixel" {
		t.Errorf("phone %+v", p)
	}
}

func TestDaemonListDevicesCopiesModel(t *testing.T) {
	withAndroidDevices(t, androidFixture(), nil)
	withAVDs(t, map[string]string{"emulator-5554": "Pixel_8_API_35"}, nil)
	byID := map[string]daemon.DeviceInfo{}
	for _, d := range daemonListDevices("android") {
		byID[d.ID] = d
	}
	if p := byID["FAKEPIXEL0001"]; p.Name != "Lab Pixel" || p.Model != "Pixel 8" || p.OSVersion != "16" || !p.Ready {
		t.Errorf("phone %+v", p)
	}
	if e := byID["emulator-5554"]; e.Name != "Pixel_8_API_35" || e.Model != "sdk_gphone64_arm64" {
		t.Errorf("emulator %+v", e)
	}
}

func TestGuessPlatformForAPhoneWithProps(t *testing.T) {
	withAndroidDevices(t, androidFixture(), nil)
	withAVDs(t, nil, nil)
	if got := guessPlatform("FAKEPIXEL0001"); got != "android" {
		t.Errorf("guessPlatform = %q", got)
	}
}

func TestDeviceEntryModelJSON(t *testing.T) {
	b, _ := json.Marshal(DeviceEntry{Platform: "android", ID: "x"})
	if strings.Contains(string(b), "model") {
		t.Errorf("empty model emitted: %s", b)
	}
	b, _ = json.Marshal(DeviceEntry{Platform: "android", ID: "x", Name: "Lab Pixel", Model: "Pixel 8"})
	if !strings.Contains(string(b), `"name":"Lab Pixel","model":"Pixel 8"`) {
		t.Errorf("model after name: %s", b)
	}
}

func TestPrintDevicesShowsModel(t *testing.T) {
	devs := []daemon.DeviceInfo{
		{ID: "A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device"},
		{ID: "B", Platform: "android", Kind: "device", Name: "Pixel 8", Model: "Pixel 8", OSVersion: "16", State: "device"},
		{ID: "C", Platform: "ios", Kind: "simulator", Name: "iPhone 16", OSVersion: "26.5", State: "Booted"},
		{ID: "emulator-5556", Platform: "android", Kind: "emulator", Model: "sdk_gphone64_arm64", OSVersion: "15", State: "device"},
	}
	check := func(out string) {
		t.Helper()
		for _, want := range []string{"Lab Pixel (Pixel 8) 16", "Pixel 8 16", "iPhone 16 26.5", "  sdk_gphone64_arm64 15"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Pixel 8 (Pixel 8)") || strings.Contains(out, "()") {
			t.Errorf("model repeated or empty:\n%s", out)
		}
	}
	var b bytes.Buffer
	printDevices(&b, devs)
	check(b.String())

	b.Reset()
	devs[0].Available = true
	devs[1].Lease = &daemon.DeviceLease{Holder: "bob", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}
	printDevices(&b, devs)
	if !strings.Contains(b.String(), "LEASE") {
		t.Fatalf("no LEASE column:\n%s", b.String())
	}
	check(b.String())
}

// doctor counts the same ready devices, props or not.
func TestDoctorCountsReadyAndroidDevicesWithProps(t *testing.T) {
	withAndroidDevices(t, androidFixture(), nil)
	withAVDs(t, nil, nil)
	ready := 0
	for _, e := range collectDevices("", false) {
		if e.Ready {
			ready++
		}
	}
	if ready < 2 {
		t.Fatalf("fixture lists %d ready devices", ready)
	}
	got := checkVisibleDevices()
	if want := fmt.Sprintf("%d ready to run", ready); got.Status != StatusOK || got.Detail != want {
		t.Errorf("doctor = %+v, want %q", got, want)
	}
}

// Android is listed beside iOS and still comes first; another platform's
// listing does not run adb.
func TestCollectDevicesListsAndroidBesideIOS(t *testing.T) {
	calls := withAndroidDevices(t, androidFixture(), nil)
	withAVDs(t, nil, nil)
	if got := collectDevices("ios", false); len(got) > 0 && got[0].Platform == "android" {
		t.Errorf("ios listing has android entries: %+v", got[0])
	}
	if *calls != 0 {
		t.Errorf("an ios listing ran adb %d times", *calls)
	}
	all := collectDevices("", false)
	if len(all) < 3 || all[0].Platform != "android" || all[1].Platform != "android" || all[2].Platform != "android" {
		t.Fatalf("android entries not first: %+v", all)
	}
	for _, e := range all[3:] {
		if e.Platform == "android" {
			t.Errorf("android entry after the iOS ones: %+v", e)
		}
	}
}

// A panic in the Android listing loses only the Android entries.
func TestCollectDevicesRecoversAnAndroidPanic(t *testing.T) {
	prev := listAndroidDevices
	t.Cleanup(func() { listAndroidDevices = prev })
	listAndroidDevices = func() ([]device.DeviceWithProps, error) { panic("boom") }
	for _, e := range collectDevices("android", false) {
		t.Errorf("entry after a panic: %+v", e)
	}
}

// Registry.Lookup runs a full listing; after a List, it reads no device
// again. adb is a script on PATH that logs each props read.
func TestRegistryLookupUsesCachedProps(t *testing.T) {
	dir := t.TempDir()
	reads := filepath.Join(dir, "reads")
	script := `#!/bin/sh
case "$1" in
devices) printf 'List of devices attached\nLOOKUP-TEST-1  device usb:1 transport_id:41\n' ;;
-s) echo "$2" >> ` + reads + `; printf 'Pixel 8\n16\n1\nLab Pixel\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	withAVDs(t, nil, nil)
	r := daemon.NewRegistry("lookup-test", daemonListDevices)
	if got := r.List("android"); len(got) != 1 || got[0].Model != "Pixel 8" {
		t.Fatalf("List = %+v", got)
	}
	info, ok := r.Lookup("LOOKUP-TEST-1")
	if !ok || info.Name != "Lab Pixel" || info.Model != "Pixel 8" || info.OSVersion != "16" {
		t.Fatalf("Lookup = %+v, %v", info, ok)
	}
	b, _ := os.ReadFile(reads)
	if n := strings.Count(string(b), "LOOKUP-TEST-1"); n != 1 {
		t.Errorf("props read %d times across List and Lookup, want 1", n)
	}
}
