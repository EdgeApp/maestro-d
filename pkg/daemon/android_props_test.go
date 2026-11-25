package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

// Android phones now list with a name, model and Android version. Code
// that matches on name and osVersion sees them.

func labPixels() []DeviceInfo {
	return []DeviceInfo{
		{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Pixel 8", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true},
		{ID: "PHONE-B", Platform: "android", Kind: "device", Name: "Pixel 8", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true},
	}
}

func TestDeviceInfoModelJSON(t *testing.T) {
	b, _ := json.Marshal(DeviceInfo{ID: "x", Platform: "android"})
	if strings.Contains(string(b), "model") {
		t.Errorf("empty model emitted: %s", b)
	}
	b, _ = json.Marshal(DeviceInfo{ID: "x", Platform: "android", Model: "Pixel 8"})
	if !strings.Contains(string(b), `"model":"Pixel 8"`) {
		t.Errorf("model missing: %s", b)
	}
}

// device start <phone name> finds the phone running and boots nothing, and
// the name alone resolves to android.
func TestBoot_PhoneNameIsAlreadyRunning(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}}
	e := newMgmtEnv(t, h)
	dev, err := e.c.StartDevice(bg, BootRequest{Name: "lab pixel"})
	if err != nil || dev.ID != "PHONE-A" || dev.Model != "Pixel 8" || dev.OSVersion != "16" {
		t.Fatalf("start lab pixel = %+v, %v", dev, err)
	}
	if strings.Contains(h.joined(), "boot") {
		t.Errorf("booted: %s", h.joined())
	}
}

// A booted emulator's answer carries the live entry's model and version.
func TestBoot_CopiesModelFromTheLiveEntry(t *testing.T) {
	h := newFakeHost()
	// The fake boot turns avd:pix into emulator-5554 and keeps the rest of
	// the entry, so the live entry after the boot carries these.
	h.live = []DeviceInfo{{ID: "avd:pix", Platform: "android", Kind: "emulator", Name: "pix", Model: "sdk_gphone64_arm64", OSVersion: "15", State: "Shutdown"}}
	e := newMgmtEnv(t, h)
	dev, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || dev.ID != "emulator-5554" || dev.Model != "sdk_gphone64_arm64" || dev.OSVersion != "15" || dev.Name != "pix" {
		t.Fatalf("boot = %+v, %v", dev, err)
	}
}

// avd:<name> names an AVD: a phone with that name is not it, and the AVD
// is booted.
func TestBoot_AVDRequestSkipsAPhoneOfTheSameName(t *testing.T) {
	h := newFakeHost()
	h.live = []DeviceInfo{
		{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "pix", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true},
		{ID: "avd:pix", Platform: "android", Kind: "emulator", Name: "pix", State: "Shutdown"},
	}
	e := newMgmtEnv(t, h)
	dev, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || dev.ID != "emulator-5554" || strings.Count(h.joined(), "boot android pix") != 1 {
		t.Fatalf("start avd:pix = %+v, %v (%s)", dev, err, h.joined())
	}
	// Running now: the same request finds the emulator, not the phone.
	again, err := e.c.StartDevice(bg, BootRequest{Name: "avd:pix"})
	if err != nil || again.ID != "emulator-5554" || strings.Count(h.joined(), "boot android") != 1 {
		t.Errorf("second start = %+v, %v (%s)", again, err, h.joined())
	}
}

func TestShaperResolvesAVDOnce(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var lists atomic.Int32
	host := New(Config{}, Deps{ListDevices: func(platform string) []DeviceInfo {
		lists.Add(1)
		return []DeviceInfo{{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "Pixel_Lab"}}
	}})
	ctx := context.Background()
	// The session's shaper knows the AVD from attach: no shaping call
	// runs discovery.
	if got := host.avdAlias("emulator-5554"); got != "avd:Pixel_Lab" {
		t.Fatalf("avdAlias = %q", got)
	}
	before := lists.Load()
	sh := host.trafficShaper("emulator-5554")
	after := lists.Load()
	_, _ = sh.Device()
	_, _ = sh.Reset(ctx)
	if sh.Alias != "avd:Pixel_Lab" || lists.Load() != after || after != before+1 {
		t.Fatalf("alias %q; listings %d, %d, %d", sh.Alias, before, after, lists.Load())
	}
	if host.avdAlias("mock-1") != "" || lists.Load() != after {
		t.Fatal("a device that is no emulator ran discovery for its alias")
	}
}
