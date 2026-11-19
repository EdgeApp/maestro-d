package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

func TestAVDKeyOnlyForEmulators(t *testing.T) {
	for _, c := range []struct {
		d    DeviceInfo
		want string
	}{
		{DeviceInfo{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Pixel 8"}, ""},
		{DeviceInfo{ID: "PHONE-A", Platform: "android", Name: "Pixel 8"}, ""},
		{DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "Pixel_9"}, "android:pixel_9"},
		{DeviceInfo{ID: "emulator-5554", Platform: "android", Name: "Pixel_9"}, "android:pixel_9"},
		{DeviceInfo{ID: AVDPrefix + "Pixel_9", Platform: "android", Name: "Pixel_9"}, "android:pixel_9"},
		{DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator"}, ""},
		{DeviceInfo{ID: "UDID", Platform: "ios", Kind: "simulator", Name: "iPhone"}, ""},
	} {
		if got := avdKey(c.d); got != c.want {
			t.Errorf("avdKey(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Two phones of one model share a name. Ending one's lease must not hold
// the other while the first one's teardown runs.
func TestLeases_SameNamedPhonesDoNotBlockEachOther(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.phones = labPixels()
	env.mu.Unlock()
	a := env.lease(env.remote, LeaseRequest{Device: "PHONE-A"}).Lease

	env.s.leaseMu.Lock()
	e := env.s.leases.byDevice["PHONE-A"]
	if e == nil {
		env.s.leaseMu.Unlock()
		t.Fatalf("no lease entry for %s", a.Device)
	}
	teardown := env.s.endLeaseLocked(e, endReleased)
	env.s.leaseMu.Unlock()
	defer teardown()

	// Phone A's teardown has not run: it is reserved, phone B is not.
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "PHONE-A"})
	wantErrCode(t, err, CodeDeviceInUse)
	b, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "PHONE-B"})
	if err != nil || b.Lease.Device != "PHONE-B" {
		t.Fatalf("lease phone B while A's lease ends: %+v %v", b, err)
	}
	if _, err := env.remote.ReleaseLease(ctx, b.Lease.ID); err != nil {
		t.Fatal(err)
	}
	// A selector by name skips only the ending phone.
	sel, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "android", Name: "pixel 8"}})
	if err != nil || sel.Lease.Device != "PHONE-B" {
		t.Fatalf("selector by name while A's lease ends: %+v %v", sel, err)
	}
}

func TestLeases_SelectorMatchesAndroidPhoneProps(t *testing.T) {
	env := newHubEnv(t, Config{})
	env.mu.Lock()
	env.phones = []DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}}
	env.mu.Unlock()
	for _, sel := range []LeaseSelector{
		{Platform: "android", OSVersion: "16"},
		{Platform: "android", Name: "lab pixel"},
		{Platform: "android", Kind: "device"},
	} {
		res := env.lease(env.remote, LeaseRequest{Select: &sel})
		if res.Lease.Device != "PHONE-A" {
			t.Fatalf("%s picked %s", sel.String(), res.Lease.Device)
		}
		if _, err := env.remote.ReleaseLease(context.Background(), res.Lease.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, err := env.remote.AcquireLease(context.Background(), LeaseRequest{Select: &LeaseSelector{Platform: "android", OSVersion: "15"}})
	wantErrCode(t, err, CodeDeviceNotFound)
}

func TestSharedInfoMatchesAPhoneBySerialOrName(t *testing.T) {
	phone := DeviceInfo{ID: "FAKEPIXEL0001", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8"}
	for _, entry := range []string{"FAKEPIXEL0001", "Lab Pixel"} {
		s := &Server{cfg: Config{ShareDevices: []string{entry}}}
		if !s.sharedInfo(phone) {
			t.Errorf("--share-device %q does not share %+v", entry, phone)
		}
	}
	s := &Server{cfg: Config{ShareDevices: []string{"Other"}}}
	if s.sharedInfo(phone) {
		t.Error("--share-device Other shares the phone")
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

// A selector boot of an AVD named like a shared phone boots the AVD and
// leases it, not the phone.
func TestLeases_SelectorBootOfAnAVDNamedLikeAPhone(t *testing.T) {
	env := newHubEnv(t, Config{})
	env.mu.Lock()
	env.downAVD = "Lab Pixel"
	env.phones = []DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}}
	env.mu.Unlock()
	var booted []string
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		env.mu.Lock()
		booted = append(booted, name)
		env.downAVD, env.emuAVD = "", name
		env.mu.Unlock()
		return "emulator-5554", nil
	}
	res := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Platform: "android", Kind: "emulator", Name: "Lab Pixel", Boot: true}})
	if res.Lease.Device != "emulator-5554" || len(booted) != 1 {
		t.Fatalf("lease %+v, booted %v", res.Lease, booted)
	}
}
