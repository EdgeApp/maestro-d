package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// fakeDaemon answers the device management routes with canned bodies and
// records each request, to check the CLI → REST mapping.
type fakeDaemon struct {
	method, path, query string
	body                map[string]any
	reply               any
	status              int
}

func newFakeDaemon(t *testing.T, f *fakeDaemon) *daemon.Client {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method, f.path, f.query, f.body = r.Method, r.URL.Path, r.URL.RawQuery, nil
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &f.body)
		}
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(f.reply)
	}))
	t.Cleanup(hs.Close)
	return daemon.NewHTTPClient(hs.URL, "", nil)
}

func parseFor(t *testing.T, flags []dflag, args ...string) (*daemonOpts, []string) {
	t.Helper()
	o, p, err := parseDaemonArgs(args, flags, nil)
	if err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	if len(p.step) > 0 {
		t.Fatalf("unknown flags %+v", p.step)
	}
	return o, p.positional
}

func TestDeviceCreateFlags(t *testing.T) {
	o, pos := parseFor(t, deviceCreateFlags, "--platform", "android", "--name", "pix", "--image", "system-images;android-35;google_apis;arm64-v8a",
		"--profile", "pixel_8", "--data-size", "8G", "--no-boot", "--install-image", "--accept-licenses", "--boot-timeout", "300", "--daemon", "e2")
	s := o.manage.spec
	if o.Attach.Platform != "android" || s.Name != "pix" || s.Image != "system-images;android-35;google_apis;arm64-v8a" || s.Profile != "pixel_8" ||
		s.DataSize != "8G" || !o.manage.noBoot || !s.InstallImage || !s.AcceptLicenses || o.StartTimeout != 300 || o.Name != "e2" || len(pos) != 0 {
		t.Errorf("opts = %+v manage = %+v", o, o.manage)
	}
	o, _ = parseFor(t, deviceCreateFlags, "--platform", "ios", "--name", "sim", "--type", "iPhone 16", "--runtime", "iOS-26-5")
	if o.manage.spec.Type != "iPhone 16" || o.manage.spec.Runtime != "iOS-26-5" || o.manage.noBoot {
		t.Errorf("ios opts = %+v", o.manage)
	}
	o, _ = parseFor(t, deviceCreateFlags, "--boot=false")
	if !o.manage.noBoot {
		t.Error("--boot=false")
	}
	o, _ = parseFor(t, deviceCatalogFlags, "--available", "-p", "ios")
	if !o.manage.available || o.Attach.Platform != "ios" {
		t.Errorf("catalog opts = %+v", o)
	}
	o, pos = parseFor(t, deviceDeleteFlags, "UDID", "--force")
	if !o.force || len(pos) != 1 || pos[0] != "UDID" {
		t.Errorf("delete opts = %+v %v", o, pos)
	}
}

func TestRunDeviceCatalog(t *testing.T) {
	f := &fakeDaemon{reply: daemon.CatalogResult{
		Envelope: daemon.Envelope{OK: true},
		IOS: &simulator.Catalog{
			Runtimes: []simulator.CatalogRuntime{
				{ID: "com.apple.CoreSimulator.SimRuntime.iOS-26-5", Name: "iOS 26.5", Version: "26.5", Available: true},
				{ID: "com.apple.CoreSimulator.SimRuntime.iOS-26-4", Name: "iOS 26.4", Version: "26.4"},
			},
			DeviceTypes: []simulator.CatalogDeviceType{{ID: "com.apple.CoreSimulator.SimDeviceType.iPhone-16", Name: "iPhone 16", ProductFamily: "iPhone"}},
		},
		Android: &emulator.Catalog{
			Images: []emulator.SystemImage{
				{ID: "system-images;android-35;google_apis;arm64-v8a", Installed: true},
				{ID: "system-images;android-36;google_apis;arm64-v8a"},
			},
			Profiles: []string{"pixel_8", "pixel_9"},
		},
	}}
	c := newFakeDaemon(t, f)
	var out bytes.Buffer
	o := &daemonOpts{Attach: daemon.AttachConfig{Platform: "android"}, manage: deviceManageOpts{available: true}}
	if err := runDeviceCatalog(context.Background(), c, o, nil, &out); err != nil {
		t.Fatal(err)
	}
	if f.method != "GET" || f.path != "/v1/catalog" || f.query != "available=true&platform=android" {
		t.Errorf("request = %s %s?%s", f.method, f.path, f.query)
	}
	for _, want := range []string{"iOS runtimes", "iOS 26.5", "(unavailable)", "iPhone 16", "installed  system-images;android-35",
		"available  system-images;android-36", "pixel_8, pixel_9"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	o.JSON = true
	if err := runDeviceCatalog(context.Background(), c, o, nil, &out); err != nil || !strings.Contains(out.String(), `"runtimes"`) {
		t.Errorf("json: %v\n%s", err, out.String())
	}
	if err := runDeviceCatalog(context.Background(), c, o, []string{"extra"}, &out); err == nil {
		t.Error("a positional argument is USAGE")
	}
	f.status, f.reply = http.StatusBadRequest, daemon.FailureEnvelope(daemon.Errorf(daemon.CodeUsage, "bad platform"))
	if err := runDeviceCatalog(context.Background(), c, o, nil, &out); err == nil || !strings.Contains(err.Error(), "bad platform") {
		t.Errorf("daemon error: %v", err)
	}
}

func TestPrintCatalog_EmptyAndErrors(t *testing.T) {
	var out bytes.Buffer
	printCatalog(&out, &daemon.CatalogResult{
		IOS:     &simulator.Catalog{},
		Android: &emulator.Catalog{},
		Errors:  map[string]string{"android": "sdkmanager not found"},
	})
	for _, want := range []string{"(none installed)", "Android system images\n  (none)", "android: unavailable: sdkmanager not found"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRunDeviceCreate(t *testing.T) {
	f := &fakeDaemon{reply: daemon.DeviceResult{Envelope: daemon.Envelope{OK: true},
		Device: daemon.DeviceInfo{ID: "UDID", Platform: "ios", Name: "sim", Ready: true}}}
	c := newFakeDaemon(t, f)
	var out bytes.Buffer
	o, _ := parseFor(t, deviceCreateFlags, "--platform", "ios", "--name", "sim", "--type", "iPhone 16", "--runtime", "iOS-26-5", "--boot-timeout", "90")
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err != nil {
		t.Fatal(err)
	}
	if f.method != "POST" || f.path != "/v1/devices" {
		t.Errorf("request = %s %s", f.method, f.path)
	}
	create, _ := f.body["create"].(map[string]any)
	if f.body["platform"] != "ios" || f.body["name"] != "sim" || f.body["boot"] != true || f.body["bootTimeout"] != float64(90) ||
		create["name"] != "sim" || create["type"] != "iPhone 16" || create["runtime"] != "iOS-26-5" {
		t.Errorf("body = %v", f.body)
	}
	if out.String() != "UDID created and booted: sim (ios)\n" {
		t.Errorf("output = %q", out.String())
	}

	f.reply = daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{ID: "avd:pix", Platform: "android", Name: "pix"}}
	o, _ = parseFor(t, deviceCreateFlags, "--platform", "android", "--name", "pix", "--data-size", "8G", "--no-boot")
	out.Reset()
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err != nil {
		t.Fatal(err)
	}
	create, _ = f.body["create"].(map[string]any)
	if f.body["boot"] != false || create["dataSize"] != "8G" {
		t.Errorf("body = %v", f.body)
	}
	if out.String() != "avd:pix created: pix (android, shut down)\n" {
		t.Errorf("output = %q", out.String())
	}

	o.JSON = true
	out.Reset()
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err != nil || !strings.Contains(out.String(), `"id": "avd:pix"`) {
		t.Errorf("json: %v %s", err, out.String())
	}
	if err := runDeviceCreate(context.Background(), c, o, []string{"stray"}, &out); err == nil || !strings.Contains(err.Error(), "stray") {
		t.Errorf("positional: %v", err)
	}
	f.status, f.reply = http.StatusBadRequest, daemon.FailureEnvelope(daemon.Errorf(daemon.CodeUsage, "data size 12G is above the 10G limit"))
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err == nil || !strings.Contains(err.Error(), "10G") {
		t.Errorf("daemon error: %v", err)
	}
}

func TestRunDeviceDelete(t *testing.T) {
	f := &fakeDaemon{reply: daemon.DeviceResult{Envelope: daemon.Envelope{OK: true},
		Device: daemon.DeviceInfo{ID: "avd:pix", Platform: "android", Name: "pix"}}}
	c := newFakeDaemon(t, f)
	var out bytes.Buffer
	o, args := parseFor(t, deviceDeleteFlags, "avd:pix", "--force")
	if err := runDeviceDelete(context.Background(), c, o, args, &out); err != nil {
		t.Fatal(err)
	}
	if f.method != "DELETE" || f.path != "/v1/devices/avd:pix" || f.query != "delete=true&force=true" {
		t.Errorf("request = %s %s?%s", f.method, f.path, f.query)
	}
	if out.String() != "avd:pix deleted: pix (android)\n" {
		t.Errorf("output = %q", out.String())
	}
	o, args = parseFor(t, deviceDeleteFlags, "UDID", "--json")
	out.Reset()
	if err := runDeviceDelete(context.Background(), c, o, args, &out); err != nil || f.query != "delete=true" || !strings.Contains(out.String(), `"ok": true`) {
		t.Errorf("json: %v %s %s", err, f.query, out.String())
	}
	if err := runDeviceDelete(context.Background(), c, o, nil, &out); err == nil {
		t.Error("no id is USAGE")
	}
	f.status, f.reply = http.StatusConflict, daemon.FailureEnvelope(daemon.Errorf(daemon.CodeDeviceExternal, "not created by maestro-d"))
	err := runDeviceDelete(context.Background(), c, o, args, &out)
	if de, ok := err.(*daemon.Error); !ok || de.Code != daemon.CodeDeviceExternal || de.ExitCode() != 5 {
		t.Errorf("DEVICE_EXTERNAL: %v", err)
	}
}

func TestDaemonDepsWiresDeviceManagement(t *testing.T) {
	d := daemonDeps()
	if d.IOSCatalog == nil || d.AndroidCatalog == nil || d.CreateSimulator == nil || d.CreateAVD == nil ||
		d.InstallSystemImage == nil || d.DeleteSimulator == nil || d.DeleteAVD == nil {
		t.Errorf("a device management hook is not wired: %+v", d)
	}
	names := map[string]bool{}
	for _, c := range daemonDeviceCommand.Subcommands {
		names[c.Name] = true
	}
	for _, n := range []string{"list", "start", "stop", "catalog", "create", "delete"} {
		if !names[n] {
			t.Errorf("device %s is not registered", n)
		}
	}
}
