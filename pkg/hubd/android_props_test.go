package hubd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func labPixels() []daemon.DeviceInfo {
	return []daemon.DeviceInfo{
		{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Pixel 8", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true},
		{ID: "PHONE-B", Platform: "android", Kind: "device", Name: "Pixel 8", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true},
	}
}

func TestAVDKeyOnlyForEmulators(t *testing.T) {
	for _, c := range []struct {
		d    daemon.DeviceInfo
		want string
	}{
		{daemon.DeviceInfo{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Pixel 8"}, ""},
		{daemon.DeviceInfo{ID: "PHONE-A", Platform: "android", Name: "Pixel 8"}, ""},
		{daemon.DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: "Pixel_9"}, "android:pixel_9"},
		{daemon.DeviceInfo{ID: "emulator-5554", Platform: "android", Name: "Pixel_9"}, "android:pixel_9"},
		{daemon.DeviceInfo{ID: daemon.AVDPrefix + "Pixel_9", Platform: "android", Name: "Pixel_9"}, "android:pixel_9"},
		{daemon.DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator"}, ""},
		{daemon.DeviceInfo{ID: "UDID", Platform: "ios", Kind: "simulator", Name: "iPhone"}, ""},
	} {
		if got := avdKey(c.d); got != c.want {
			t.Errorf("avdKey(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// Two phones of one model share a name. Ending one's lease must not hold
// the other while the first one's teardown runs.
func TestLeases_SameNamedPhonesDoNotBlockEachOther(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.phones = labPixels()
	env.mu.Unlock()
	a := env.lease(env.remote, daemon.LeaseRequest{Device: "PHONE-A"}).Lease

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
	_, err := env.remote.AcquireLease(ctx, daemon.LeaseRequest{Device: "PHONE-A"})
	wantErrCode(t, err, daemon.CodeDeviceInUse)
	b, err := env.remote.AcquireLease(ctx, daemon.LeaseRequest{Device: "PHONE-B"})
	if err != nil || b.Lease.Device != "PHONE-B" {
		t.Fatalf("lease phone B while A's lease ends: %+v %v", b, err)
	}
	if _, err := env.remote.ReleaseLease(ctx, b.Lease.ID); err != nil {
		t.Fatal(err)
	}
	// A selector by name skips only the ending phone.
	sel, err := env.remote.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "android", Name: "pixel 8"}})
	if err != nil || sel.Lease.Device != "PHONE-B" {
		t.Fatalf("selector by name while A's lease ends: %+v %v", sel, err)
	}
}

func TestLeases_SelectorMatchesAndroidPhoneProps(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.mu.Lock()
	env.phones = []daemon.DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}}
	env.mu.Unlock()
	for _, sel := range []daemon.LeaseSelector{
		{Platform: "android", OSVersion: "16"},
		{Platform: "android", Name: "lab pixel"},
		{Platform: "android", Kind: "device"},
	} {
		res := env.lease(env.remote, daemon.LeaseRequest{Select: &sel})
		if res.Lease.Device != "PHONE-A" {
			t.Fatalf("%s picked %s", sel.String(), res.Lease.Device)
		}
		if _, err := env.remote.ReleaseLease(context.Background(), res.Lease.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, err := env.remote.AcquireLease(context.Background(), daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "android", OSVersion: "15"}})
	wantErrCode(t, err, daemon.CodeDeviceNotFound)
}

// A selector boot of an AVD named like a shared phone boots the AVD and
// leases it, not the phone.
func TestLeases_SelectorBootOfAnAVDNamedLikeAPhone(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.mu.Lock()
	env.downAVD = "Lab Pixel"
	env.phones = []daemon.DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel", Model: "Pixel 8", OSVersion: "16", State: "device", Ready: true}}
	env.mu.Unlock()
	var booted []string
	env.s.boot = func(platform, name string, timeout time.Duration) (string, error) {
		env.mu.Lock()
		booted = append(booted, name)
		env.downAVD, env.emuAVD = "", name
		env.mu.Unlock()
		return "emulator-5554", nil
	}
	res := env.lease(env.remote, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "android", Kind: "emulator", Name: "Lab Pixel", Boot: true}})
	if res.Lease.Device != "emulator-5554" || len(booted) != 1 {
		t.Fatalf("lease %+v, booted %v", res.Lease, booted)
	}
}

// A hub shares only its configured device IDs.
func TestSharedInfo_ExplicitOnly(t *testing.T) {
	phone := daemon.DeviceInfo{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Pixel"}
	tv := daemon.DeviceInfo{ID: "TV-1", Platform: "ios", Kind: "tv", Name: "Lab TV"}
	for _, c := range []struct {
		name      string
		cfg       daemon.Config
		phone, tv bool
	}{
		{"hub naming nothing", daemon.Config{Hub: &Server{}, DeviceConfig: daemontest.DeviceConfig(t)}, false, false},

		{"config.json phone", daemon.Config{Hub: &Server{}, DeviceConfig: daemontest.DeviceConfig(t, "PHONE-A")}, true, false},
		{"config.json TV", daemon.Config{Hub: &Server{}, DeviceConfig: daemontest.DeviceConfig(t, "TV-1")}, false, true},
		{"names alone do not share", daemon.Config{Hub: &Server{}, DeviceConfig: daemontest.DeviceConfig(t, "Lab Pixel", "Lab TV")}, false, false},
	} {
		s := &Server{config: newConfigCache(daemon.DeviceConfigPath(c.cfg.DeviceConfig), nil, nil)}
		if got := s.sharedInfo(phone); got != c.phone {
			t.Errorf("%s: phone shared %v", c.name, got)
		}
		if got := s.sharedInfo(tv); got != c.tv {
			t.Errorf("%s: TV shared %v", c.name, got)
		}
	}
	s := &Server{config: newConfigCache(daemontest.DeviceConfig(t, "a", "t", "c"), nil, nil)}
	if got := s.shareList(); got != "config.json a, t, c" {
		t.Errorf("shareList = %q", got)
	}
}

// A hub that names nothing lists nothing to a remote caller, refuses its
// lease and its create, and still lists everything to a local one.
func TestLeases_HubNamingNothingSharesNothing(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t)})
	ctx := context.Background()
	if devs, err := env.remote.Devices(ctx, ""); err != nil || len(devs) != 0 {
		t.Fatalf("remote listing: %+v %v", devs, err)
	}
	if devs, err := env.local.Devices(ctx, ""); err != nil || len(devs) == 0 {
		t.Fatalf("local listing: %+v %v", devs, err)
	}
	_, err := env.remote.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1"})
	wantErrCode(t, err, daemon.CodeDeviceNotFound)
	_, err = env.remote.CreateDevice(ctx, daemon.BootRequest{Platform: "ios", Name: "sim", Create: &daemon.CreateSpec{Type: "iPhone 16"}})
	if de := wantErrCode(t, err, daemon.CodeForbidden); !strings.Contains(de.Message, "this hub shares only nothing") {
		t.Fatalf("create: %v", de.Message)
	}
}

// config.json's hubDevices put a device in the pool.
func TestLeases_ShareConfigShares(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t, "mock-2")})
	ctx := context.Background()
	devs, err := env.remote.Devices(ctx, "")
	if err != nil || len(devs) != 1 || devs[0].ID != "mock-2" {
		t.Fatalf("remote listing: %+v %v", devs, err)
	}
	if res := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-2"}); res.Lease.Device != "mock-2" {
		t.Fatalf("lease: %+v", res.Lease)
	}
}

// A hubDevices avd:<name> entry shares the AVD, its running emulator, and
// an android boot or create by its bare name. An entry is an id: a device's name
// shares nothing.
func TestSharedInfo_ConfigAVDEntry(t *testing.T) {
	s := &Server{config: newConfigCache(daemontest.DeviceConfig(t, "avd:ci-pixel", "Lab Pixel"), nil, nil)}
	for _, c := range []struct {
		d    daemon.DeviceInfo
		want bool
	}{
		{daemon.DeviceInfo{ID: "avd:ci-pixel", Kind: "emulator", Name: "ci-pixel"}, true},
		{daemon.DeviceInfo{ID: "emulator-5554", Kind: "emulator", Name: "ci-pixel"}, true},
		{daemon.DeviceInfo{ID: "ci-pixel", Name: "ci-pixel", Platform: "android"}, true},
		// Without android, a bare name may boot or create a simulator.
		{daemon.DeviceInfo{ID: "ci-pixel", Name: "ci-pixel"}, false},
		{daemon.DeviceInfo{ID: "ci-pixel", Name: "ci-pixel", Platform: "ios"}, false},
		{daemon.DeviceInfo{ID: "emulator-5556", Kind: "emulator", Name: "other"}, false},
		{daemon.DeviceInfo{ID: "PHONE-A", Kind: "device", Name: "ci-pixel"}, false},
		{daemon.DeviceInfo{ID: "PHONE-B", Kind: "device", Name: "Lab Pixel"}, false},
	} {
		if got := s.sharedInfo(c.d); got != c.want {
			t.Errorf("%+v: shared %v", c.d, got)
		}
	}
}

// An AVD shared through hubDevices stays leasable by a remote caller once
// it runs under its serial.
func TestLeases_ShareConfigAVDRunning(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t, "avd:Pixel_Lab")})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel_Lab"
	env.mu.Unlock()
	if res := env.lease(env.remote, daemon.LeaseRequest{Device: "emulator-5554"}); res.Lease.Device != "emulator-5554" {
		t.Fatalf("lease: %+v", res.Lease)
	}
	if _, err := env.remote.Device(ctx, "emulator-5554"); err != nil {
		t.Fatal(err)
	}

}

// A remote iOS boot or create under a bare name is refused even when an
// avd:<name> entry names that AVD.
func TestLeases_ShareConfigAVDEntryIsNoSimulator(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t, "avd:Lab")})
	ctx := context.Background()
	_, err := env.remote.CreateDevice(ctx, daemon.BootRequest{Platform: "ios", Name: "Lab", Create: &daemon.CreateSpec{Type: "iPhone 16"}})
	wantErrCode(t, err, daemon.CodeForbidden)
	_, err = env.remote.StartDevice(ctx, daemon.BootRequest{Platform: "ios", Name: "Lab"})
	wantErrCode(t, err, daemon.CodeDeviceNotFound)
	_, err = env.remote.StartDevice(ctx, daemon.BootRequest{Name: "Lab"})
	wantErrCode(t, err, daemon.CodeDeviceNotFound)
}

// A remote boot of a listed AVD by its bare name, with no platform, is
// android as a local boot infers it, so avd:<name> lets it through.
func TestLeases_ShareConfigAVDBareNameBoot(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t, "avd:Lab")})
	env.mu.Lock()
	env.downAVD = "Lab"
	env.mu.Unlock()
	req := daemon.BootRequest{Name: "Lab"}
	if _, err := env.remote.StartDevice(context.Background(), req); err != nil {
		t.Fatalf("bare AVD name: %v", err)
	}
	daemontest.WriteDevices(t, env.s.cfg.DeviceConfig, "avd:Other")
	env.s.config.snapshot(time.Now(), true)
	if _, err := env.remote.StartDevice(context.Background(), req); codeOf(t, err) != daemon.CodeDeviceNotFound {
		t.Fatalf("unshared AVD: %v", err)
	}

}

// A remote caller's status, which reads records, lists an emulator the hub
// shares as avd:<name> while it is attached and, booted by the hub, after
// it detaches: the attach keeps the AVD's name on the record, not the
// driver's.
func TestLeases_ShareConfigAVDInStatus(t *testing.T) {
	env := newHubEnv(t, daemon.Config{DeviceConfig: daemontest.DeviceConfig(t, "avd:Pixel_Lab")})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel_Lab"
	env.mu.Unlock()
	env.mu.Lock()
	env.emuAVD = ""
	env.downAVD = "Pixel_Lab"
	env.mu.Unlock()
	env.s.boot = func(string, string, time.Duration) (string, error) {
		env.mu.Lock()
		env.emuAVD = "Pixel_Lab"
		env.mu.Unlock()
		return "emulator-5554", nil
	}
	if _, err := env.s.Registry().Boot(ctx, daemon.BootRequest{Platform: "android", Name: "avd:Pixel_Lab"}); err != nil {
		t.Fatal(err)
	}
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
	res := env.lease(env.remote, daemon.LeaseRequest{Device: "emulator-5554", Attach: &daemon.AttachConfig{Platform: "mock"}})
	inStatus("attached")
	if rec, _ := env.s.Registry().Record("emulator-5554"); rec.Name != "Pixel_Lab" || rec.Kind != "emulator" {
		t.Fatalf("attached record: %+v", rec)
	}
	if _, err := env.remote.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	_ = env.local.Detach(ctx, "emulator-5554")
	if rec, ok := env.s.Registry().Record("emulator-5554"); !ok || rec.Attached {
		t.Fatalf("detached record: %+v %v", rec, ok)
	}
	inStatus("detached")
}
