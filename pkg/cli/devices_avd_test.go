package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func withAVDs(t *testing.T, bySerial map[string]string, names []string) *int {
	t.Helper()
	prevRun, prevList := runningAVDsBySerial, listAVDNames
	t.Cleanup(func() { runningAVDsBySerial, listAVDNames = prevRun, prevList })
	adbCalls := 0
	runningAVDsBySerial = func() map[string]string { adbCalls++; return bySerial }
	listAVDNames = func() []string { return names }
	return &adbCalls
}

func TestAVDEntries_NamesRunningAndListsShutDown(t *testing.T) {
	withAVDs(t, map[string]string{"emulator-5554": "pix"}, []string{"pix", "other"})
	entries := []DeviceEntry{
		{Platform: "android", Kind: "emulator", ID: "emulator-5554", State: "device", Ready: true},
		{Platform: "android", Kind: "emulator", ID: "emulator-5556", State: "offline"},
		{Platform: "android", Kind: "device", ID: "R58M", State: "device", Ready: true},
	}
	got := avdEntries(entries, true)
	if len(got) != 4 {
		t.Fatalf("entries = %+v", got)
	}
	if got[0].Name != "pix" || got[1].Name != "" || got[2].Name != "" {
		t.Errorf("a running emulator carries its AVD name, others are untouched: %+v", got[:3])
	}
	// "pix" is running, so it is listed once, under its serial.
	shut := got[3]
	if shut.ID != "avd:other" || shut.Name != "other" || shut.State != "Shutdown" || shut.Ready || shut.Kind != "emulator" || shut.Platform != "android" {
		t.Errorf("shut-down AVD = %+v", shut)
	}
}

func TestAVDEntries_WithoutShutdown(t *testing.T) {
	calls := withAVDs(t, map[string]string{"emulator-5554": "pix"}, []string{"pix", "other"})
	got := avdEntries([]DeviceEntry{{Platform: "android", ID: "emulator-5554"}, {Platform: "android", ID: "emulator-5556"}}, false)
	if len(got) != 2 || got[0].Name != "pix" {
		t.Errorf("entries = %+v", got)
	}
	if *calls != 1 {
		t.Errorf("adb is asked once per listing, not per emulator: %d", *calls)
	}
}

func TestAVDEntries_NoEmulatorsNoAdb(t *testing.T) {
	calls := withAVDs(t, nil, nil)
	got := avdEntries([]DeviceEntry{{Platform: "android", Kind: "device", ID: "R58M"}}, true)
	if len(got) != 1 || *calls != 0 {
		t.Errorf("entries = %+v, adb calls %d", got, *calls)
	}
}

func TestCollectDevices_AVDEntriesAreAndroid(t *testing.T) {
	withAVDs(t, nil, []string{"202611080630-unit-avd"})
	found := false
	for _, e := range collectDevices("android", true) {
		if e.ID == "avd:202611080630-unit-avd" {
			found = true
		}
	}
	if !found {
		t.Error("collectDevices does not list the shut-down AVD")
	}
	for _, e := range collectDevices("android", false) {
		if e.ID == "avd:202611080630-unit-avd" {
			t.Error("without includeShutdown (doctor, devices without --all) no avd: entry is listed")
		}
	}
	if got := guessPlatform("avd:202611080630-unit-avd"); got != "android" {
		t.Errorf("guessPlatform(avd:x) = %q", got)
	}
}

func TestAVDHardwareProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	for _, name := range []string{"bench", "spare"} {
		dir := filepath.Join(home, name+".avd")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.ini"), []byte("hw.device.name = pixel_8\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	withAVDs(t, map[string]string{"emulator-5554": "bench"}, []string{"bench", "spare"})
	found := avdEntries([]DeviceEntry{{ID: "emulator-5554", Platform: "android", Kind: "emulator", Model: "sdk_gphone64_arm64"}}, true)
	if len(found) != 2 || found[0].Model != "sdk_gphone64_arm64" || found[0].Name != "bench" || found[0].DeviceType != "pixel_8" || found[1].DeviceType != "pixel_8" {
		t.Fatal(found)
	}
	if avdDeviceType("../outside") != "" || avdDeviceType("absent") != "" {
		t.Fatal("unexpected profile")
	}
}
