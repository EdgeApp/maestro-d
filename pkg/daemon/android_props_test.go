package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
		s := &Server{cfg: Config{Hub: true, ShareDevices: []string{entry}}}
		if !s.sharedInfo(phone) {
			t.Errorf("--share-device %q does not share %+v", entry, phone)
		}
	}
	s := &Server{cfg: Config{Hub: true, ShareDevices: []string{"Other"}}}
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

// A hub shares only what it names, through --share-device, --share-tv or
// config.json's hubDevices; a daemon that is not a hub shares everything.
func TestSharedInfo_ExplicitOnly(t *testing.T) {
	phone := DeviceInfo{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel"}
	tv := DeviceInfo{ID: "TV-1", Platform: "ios", Kind: "tv", Name: "Lab TV"}
	for _, c := range []struct {
		name      string
		cfg       Config
		phone, tv bool
	}{
		{"hub naming nothing", Config{Hub: true}, false, false},
		{"plain daemon", Config{}, true, true},
		{"--share-device", Config{Hub: true, ShareDevices: []string{"PHONE-A"}}, true, false},
		{"--share-device never shares a TV", Config{Hub: true, ShareDevices: []string{"TV-1"}}, false, false},
		{"--share-tv", Config{Hub: true, ShareTVs: []string{"Lab TV"}}, false, true},
		{"config.json phone", Config{Hub: true, ShareConfig: []string{"PHONE-A"}}, true, false},
		{"config.json TV", Config{Hub: true, ShareConfig: []string{"TV-1"}}, false, true},
		{"flags and config.json add up", Config{Hub: true, ShareDevices: []string{"Lab Pixel"}, ShareConfig: []string{"TV-1"}}, true, true},
	} {
		s := &Server{cfg: c.cfg}
		if got := s.sharedInfo(phone); got != c.phone {
			t.Errorf("%s: phone shared %v", c.name, got)
		}
		if got := s.sharedInfo(tv); got != c.tv {
			t.Errorf("%s: TV shared %v", c.name, got)
		}
	}
	s := &Server{cfg: Config{Hub: true, ShareDevices: []string{"a"}, ShareTVs: []string{"t"}, ShareConfig: []string{"c"}}}
	if got := s.shareList(); got != "a; TVs t; config.json c" {
		t.Errorf("shareList = %q", got)
	}
}

// A hub that names nothing lists nothing to a remote caller, refuses its
// lease and its create, and still lists everything to a local one.
func TestLeases_HubNamingNothingSharesNothing(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{}})
	ctx := context.Background()
	if devs, err := env.remote.Devices(ctx, ""); err != nil || len(devs) != 0 {
		t.Fatalf("remote listing: %+v %v", devs, err)
	}
	if devs, err := env.local.Devices(ctx, ""); err != nil || len(devs) == 0 {
		t.Fatalf("local listing: %+v %v", devs, err)
	}
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.CreateDevice(ctx, BootRequest{Platform: "ios", Name: "sim", Create: &CreateSpec{Type: "iPhone 16"}})
	if de := wantErrCode(t, err, CodeForbidden); !strings.Contains(de.Message, "this hub shares only nothing") {
		t.Fatalf("create: %v", de.Message)
	}
}

// config.json's hubDevices put a device in the pool.
func TestLeases_ShareConfigShares(t *testing.T) {
	env := newHubEnv(t, Config{ShareConfig: []string{"mock-2"}})
	ctx := context.Background()
	devs, err := env.remote.Devices(ctx, "")
	if err != nil || len(devs) != 1 || devs[0].ID != "mock-2" {
		t.Fatalf("remote listing: %+v %v", devs, err)
	}
	if res := env.lease(env.remote, LeaseRequest{Device: "mock-2"}); res.Lease.Device != "mock-2" {
		t.Fatalf("lease: %+v", res.Lease)
	}
}

// A hubDevices avd:<name> entry shares the AVD, its running emulator, and
// an android boot or create by its bare name. An entry is an id: a device's name
// shares nothing, as it would through --share-device.
func TestSharedInfo_ConfigAVDEntry(t *testing.T) {
	s := &Server{cfg: Config{Hub: true, ShareConfig: []string{"avd:ci-pixel", "Lab Pixel"}}}
	for _, c := range []struct {
		d    DeviceInfo
		want bool
	}{
		{DeviceInfo{ID: "avd:ci-pixel", Kind: "emulator", Name: "ci-pixel"}, true},
		{DeviceInfo{ID: "emulator-5554", Kind: "emulator", Name: "ci-pixel"}, true},
		{DeviceInfo{ID: "ci-pixel", Name: "ci-pixel", Platform: "android"}, true},
		// Without android, a bare name may boot or create a simulator.
		{DeviceInfo{ID: "ci-pixel", Name: "ci-pixel"}, false},
		{DeviceInfo{ID: "ci-pixel", Name: "ci-pixel", Platform: "ios"}, false},
		{DeviceInfo{ID: "emulator-5556", Kind: "emulator", Name: "other"}, false},
		{DeviceInfo{ID: "PHONE-A", Kind: "device", Name: "ci-pixel"}, false},
		{DeviceInfo{ID: "PHONE-B", Kind: "device", Name: "Lab Pixel"}, false},
	} {
		if got := s.sharedInfo(c.d); got != c.want {
			t.Errorf("%+v: shared %v", c.d, got)
		}
	}
}

// An AVD shared through hubDevices stays leasable by a remote caller once
// it runs under its serial.
func TestLeases_ShareConfigAVDRunning(t *testing.T) {
	env := newHubEnv(t, Config{ShareConfig: []string{"avd:Pixel_Lab"}})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel_Lab"
	env.mu.Unlock()
	if res := env.lease(env.remote, LeaseRequest{Device: "emulator-5554"}); res.Lease.Device != "emulator-5554" {
		t.Fatalf("lease: %+v", res.Lease)
	}
	if _, err := env.remote.Device(ctx, "emulator-5554"); err != nil {
		t.Fatal(err)
	}
	// The session's shaper knows the AVD from attach: no shaping call
	// runs discovery.
	if got := env.s.avdAlias("emulator-5554"); got != "avd:Pixel_Lab" {
		t.Fatalf("avdAlias = %q", got)
	}
	before := env.lists.Load()
	sh := env.s.trafficShaper("emulator-5554")
	after := env.lists.Load()
	_, _ = sh.Device()
	_, _ = sh.Reset(ctx)
	if sh.Alias != "avd:Pixel_Lab" || env.lists.Load() != after || after != before+1 {
		t.Fatalf("alias %q; listings %d, %d, %d", sh.Alias, before, after, env.lists.Load())
	}
	if env.s.avdAlias("mock-1") != "" || env.lists.Load() != after {
		t.Fatal("a device that is no emulator ran discovery for its alias")
	}
}

// A remote iOS boot or create under a bare name is refused even when an
// avd:<name> entry names that AVD.
func TestLeases_ShareConfigAVDEntryIsNoSimulator(t *testing.T) {
	env := newHubEnv(t, Config{ShareConfig: []string{"avd:Lab"}})
	ctx := context.Background()
	_, err := env.remote.CreateDevice(ctx, BootRequest{Platform: "ios", Name: "Lab", Create: &CreateSpec{Type: "iPhone 16"}})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.StartDevice(ctx, BootRequest{Platform: "ios", Name: "Lab"})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.StartDevice(ctx, BootRequest{Name: "Lab"})
	wantErrCode(t, err, CodeDeviceNotFound)
}

// A remote boot of a listed AVD by its bare name, with no platform, is
// android as a local boot infers it, so avd:<name> lets it through.
func TestLeases_ShareConfigAVDBareNameBoot(t *testing.T) {
	env := newHubEnv(t, Config{ShareConfig: []string{"avd:Lab"}})
	env.mu.Lock()
	env.downAVD = "Lab"
	env.mu.Unlock()
	req := BootRequest{Name: "Lab"}
	r := httptest.NewRequest("POST", "/v1/devices", nil)
	r = r.WithContext(withCaller(r.Context(), Caller{Remote: true}))
	if e := env.s.checkShareBoot(r, req); e != nil {
		t.Fatalf("bare AVD name: %v", e)
	}
	env.s.cfg.ShareConfig = []string{"avd:Other"}
	if e := env.s.checkShareBoot(r, req); e == nil || e.Code != CodeDeviceNotFound {
		t.Fatalf("an AVD the hub does not name: %v", e)
	}
}

// A remote caller's status, which reads records, lists an emulator the hub
// shares as avd:<name> while it is attached and, booted by the hub, after
// it detaches: the attach keeps the AVD's name on the record, not the
// driver's.
func TestLeases_ShareConfigAVDInStatus(t *testing.T) {
	env := newHubEnv(t, Config{ShareConfig: []string{"avd:Pixel_Lab"}})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel_Lab"
	env.mu.Unlock()
	env.s.registry.mu.Lock()
	env.s.registry.records["emulator-5554"] = &DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator",
		Name: "Pixel_Lab", BootedBy: BootedByDaemon, BootedAt: nowRFC3339()}
	env.s.registry.mu.Unlock()
	inStatus := func(when string) {
		t.Helper()
		st, err := env.remote.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range st.Devices {
			if d.ID == "emulator-5554" {
				return
			}
		}
		t.Fatalf("%s: status: %+v", when, st.Devices)
	}
	res := env.lease(env.remote, LeaseRequest{Device: "emulator-5554", Attach: &AttachConfig{Platform: "mock"}})
	inStatus("attached")
	if rec, _ := env.s.registry.Record("emulator-5554"); rec.Name != "Pixel_Lab" || rec.Kind != "emulator" {
		t.Fatalf("attached record: %+v", rec)
	}
	if _, err := env.remote.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	_ = env.local.Detach(ctx, "emulator-5554")
	if rec, ok := env.s.registry.Record("emulator-5554"); !ok || rec.Attached {
		t.Fatalf("detached record: %+v %v", rec, ok)
	}
	inStatus("detached")
}
