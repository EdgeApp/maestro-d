package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func TestNewRemoteAppiumDriver(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", fakeKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	fake := dftest.New(t)
	p, err := devicefarm.NewProvider(context.Background(), devicefarm.Options{Endpoint: fake.URL(), CredentialsFile: writeTempCreds(t)})
	if err != nil {
		t.Fatal(err)
	}
	alloc, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "ios", Name: "Apple iPhone 15"},
		cloudprov.AllocateHooks{DeviceID: "df-00000001", Created: func(string) error { return nil }, Abandoned: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = p.Release(context.Background(), alloc) }()
	// It opens the session on the endpoint it is given, with the
	// provider's capabilities, and never pins a udid.
	d, info, err := newRemoteAppiumDriver(context.Background(), alloc.Endpoint, alloc.Caps)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if info.Platform != "ios" || info.Kind != "cloud" || info.DriverName != "appium" {
		t.Fatalf("info: %+v", info)
	}
	s, _ := fake.Session(alloc.Ref)
	if len(s.Caps) != 1 || s.Caps[0]["appium:udid"] != nil {
		t.Fatalf("caps: %+v", s.Caps)
	}
	if err := d.Keepalive(); err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	// Recording steps say the device has no screen capture.
	for _, step := range []flow.Step{&flow.StartRecordingStep{Path: "rec"}, &flow.StopRecordingStep{}} {
		res := d.Execute(step)
		if res.Success || !strings.Contains(res.Message, "no screen capture") || strings.Contains(res.Message, "*flow.") {
			t.Fatalf("%T: %+v", step, res)
		}
	}
	if err := d.RestartSession(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	// An endpoint that does not answer is an error.
	if _, _, err := newRemoteAppiumDriver(context.Background(), "http://127.0.0.1:1/wd/hub", map[string]any{"platformName": "Android"}); err == nil {
		t.Fatal("a dead endpoint opened")
	}
}

// writeTempCreds writes a shared credentials file with the fake key.
func writeTempCreds(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds")
	writeCreds(t, p)
	return p
}

func TestCloudCreateFlags(t *testing.T) {
	caps := filepath.Join(t.TempDir(), "caps.json")
	_ = os.WriteFile(caps, []byte(`{"appium:language":"fr"}`), 0o600)
	o, _ := parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--platform", "ios", "--name", "iPhone 1[45]", "--os-min", "17",
		"--os-max", "18", "--os-version", "17.4", "--app", "app.ipa", "--auxiliary-app", "arn:aws:devicefarm:x", "--auxiliary-app", "b.ipa",
		"--app-id", "org.x", "--appium-version", "3", "--caps", caps, "--idle-release", "30m", "--boot-timeout", "600")
	spec, err := cloudSpec(o)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Provider != "devicefarm" || spec.Platform != "ios" || spec.Name != "iPhone 1[45]" || spec.OSVersionMin != "17" || spec.OSVersionMax != "18" ||
		spec.OSVersion != "17.4" || !filepath.IsAbs(spec.App) || spec.AuxiliaryApps[0] != "arn:aws:devicefarm:x" || !filepath.IsAbs(spec.AuxiliaryApps[1]) ||
		spec.AppID != "org.x" || spec.AppiumVersion != "3" || spec.Caps["appium:language"] != "fr" || spec.IdleReleaseSec != 1800 || spec.BootTimeoutSec != 600 {
		t.Fatalf("spec %+v", spec)
	}
	for _, bad := range [][]string{
		{"--provider", "devicefarm", "--idle-release", "soon"},
		{"--provider", "devicefarm", "--caps", "/nonexistent/caps.json"},
	} {
		o, _ := parseFor(t, deviceCreateFlags, bad...)
		if _, err := cloudSpec(o); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	o, _ = parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--caps", bad)
	if _, err := cloudSpec(o); err == nil {
		t.Error("a bad caps file")
	}
	if n, err := durationSeconds("90"); err != nil || n != 90 {
		t.Errorf("seconds: %d %v", n, err)
	}
	if n, err := durationSeconds("1m30s"); err != nil || n != 90 {
		t.Errorf("duration: %d %v", n, err)
	}
	// An explicit value under a second would read as "the default": refused.
	for _, v := range []string{"0", "0s", "500ms", "-5", "soon"} {
		if _, err := durationSeconds(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	o, _ = parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--idle-release", "500ms")
	if _, err := cloudSpec(o); err == nil {
		t.Error("--idle-release 500ms accepted")
	}
	if firstNonEmpty("", "b") != "b" || firstNonEmpty() != "" {
		t.Error("firstNonEmpty")
	}
}

func TestRunCloudCreate(t *testing.T) {
	f := &fakeDaemon{reply: daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{
		ID: "df-1", Name: "Google Pixel 8", OSVersion: "14", Platform: "android", Provider: "devicefarm", IdleReleaseSec: 600, ExpiresAt: "2026-11-21T02:00:00Z"}}}
	c := newFakeDaemon(t, f)
	o, _ := parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--platform", "android", "--name", "Google Pixel 8")
	var out bytes.Buffer
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err != nil {
		t.Fatal(err)
	}
	cloud, _ := f.body["create"].(map[string]any)["cloud"].(map[string]any)
	if f.method != "POST" || f.path != "/v1/devices" || cloud["name"] != "Google Pixel 8" || f.body["platform"] != "android" || f.body["name"] != "" {
		t.Fatalf("request %s %s %+v", f.method, f.path, f.body)
	}
	if !strings.Contains(out.String(), "df-1 launched: Google Pixel 8 14 (android, devicefarm); released after 600s idle") {
		t.Fatalf("output: %s", out.String())
	}
	out.Reset()
	o.JSON = true
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err != nil || !strings.Contains(out.String(), `"df-1"`) {
		t.Fatalf("json: %v %s", err, out.String())
	}
	o, _ = parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--idle-release", "x")
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err == nil {
		t.Fatal("a bad --idle-release")
	}
	f.status, f.reply = 400, daemon.FailureEnvelope(daemon.Errorf(daemon.CodeUsage, "no credentials"))
	o, _ = parseFor(t, deviceCreateFlags, "--provider", "devicefarm", "--platform", "android")
	if err := runDeviceCreate(context.Background(), c, o, nil, &out); err == nil {
		t.Fatal("a daemon error")
	}
	// Delete prints the minutes of a cloud device.
	f.status, f.reply = 0, daemon.DeviceResult{Envelope: daemon.Envelope{OK: true}, Device: daemon.DeviceInfo{ID: "df-1", Name: "Google Pixel 8",
		Platform: "android", DeviceMinutes: &cloudprov.DeviceMinutes{Total: 3, Metered: 3}}}
	out.Reset()
	if err := runDeviceDelete(context.Background(), c, &daemonOpts{}, []string{"df-1"}, &out); err != nil || !strings.Contains(out.String(), "3 device minutes") {
		t.Fatalf("delete: %v %s", err, out.String())
	}
	if !strings.Contains(minutesNote(&cloudprov.DeviceMinutes{Total: 2, Estimated: true}), "about 2 device minutes so far") {
		t.Fatal("estimated minutes note")
	}
}

func TestPrintOrphansAndSessions(t *testing.T) {
	var b bytes.Buffer
	printOrphans(&b, &daemon.OrphanResult{})
	if !strings.Contains(b.String(), "No orphaned devices.") {
		t.Fatal(b.String())
	}
	b.Reset()
	printOrphans(&b, &daemon.OrphanResult{Stopped: []string{"sim-1"}, Released: []daemon.ReleasedCloud{{ID: "df-1", DeviceMinutes: &cloudprov.DeviceMinutes{Metered: 3, Total: 3, Estimated: true}}, {ID: "df-3"}},
		Unreleased: []daemon.UnreleasedCloud{{ID: "df-2", Daemon: "d", Provider: "devicefarm", ProviderRef: "arn:2", Error: "no credentials"}}})
	for _, want := range []string{"stopped: sim-1", "released: df-1 (about 3 device minutes so far", "released: df-3\n", "unreleased: df-2 (daemon d, devicefarm session arn:2): no credentials"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	printSessions(&b, &daemon.ProviderSessionsResult{})
	if !strings.Contains(b.String(), "No sessions.") {
		t.Fatal(b.String())
	}
	b.Reset()
	printSessions(&b, &daemon.ProviderSessionsResult{Sessions: []cloudprov.SessionSummary{
		{Name: "df-1", Status: "COMPLETED", Result: "STOPPED", Model: "Google Pixel 8", Created: "2026-11-20T00:00:00Z", DeviceMinutes: &cloudprov.DeviceMinutes{Total: 4}},
		{Name: "df-2", Status: "RUNNING"},
	}})
	if !strings.Contains(b.String(), "df-1") || !strings.Contains(b.String(), "COMPLETED") || !strings.Contains(b.String(), " 4 ") {
		t.Fatal(b.String())
	}
}

func TestLeaseCloudFlags(t *testing.T) {
	o, _ := parseFor(t, leaseFlags, "--location", "cloud", "--platform", "ios", "--provider", "devicefarm", "--name", "Apple iPhone 15",
		"--os-min", "17", "--os-max", "18", "--hub", "b", "--ttl", "1h", "--idle-release", "30m", "--appium-version", "3", "--app-id", "org.x")
	sel := o.lease.sel
	if sel.Location != "cloud" || sel.Provider != "devicefarm" || sel.Name != "Apple iPhone 15" || sel.OSVersionMin != "17" || sel.OSVersionMax != "18" ||
		sel.Hub != "b" || o.lease.ttl != 3600 {
		t.Fatalf("selector %+v ttl %d", sel, o.lease.ttl)
	}
	spec, err := leaseCloudSpec(o)
	if err != nil || spec.IdleReleaseSec != 1800 || spec.AppiumVersion != "3" || spec.AppID != "org.x" {
		t.Fatalf("spec %+v %v", spec, err)
	}
	o, _ = parseFor(t, leaseFlags, "--location", "cloud", "--idle-release", "soon")
	if _, err := leaseCloudSpec(o); err == nil {
		t.Fatal("a bad --idle-release")
	}
}

func TestDevicefarmCommands(t *testing.T) {
	ctx := context.Background()
	f := &fakeDaemon{}
	c := newFakeDaemon(t, f)
	var b bytes.Buffer

	// account: --hub routes, the table and --json.
	o, _ := parseFor(t, devicefarmSessionsFlags, "--hub", "hub-b")
	f.reply = daemon.ProviderAccountResult{Provider: "devicefarm", Hub: "hub-b", Account: map[string]any{
		"trialMinutes": map[string]any{"total": 1000, "remaining": 941}, "unmeteredDevices": map[string]any{}}}
	if err := runDevicefarmAccount(ctx, c, o, nil, &b); err != nil {
		t.Fatal(err)
	}
	if f.method != "GET" || f.path != "/v1/providers/devicefarm/account" || f.query != "hub=hub-b" {
		t.Fatalf("account request: %s %s?%s", f.method, f.path, f.query)
	}
	for _, want := range []string{"hub: hub-b", "trial minutes: 941 of 1000 left", "unmeteredDevices: map[]"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("account lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	o, _ = parseFor(t, devicefarmSessionsFlags, "--json")
	if err := runDevicefarmAccount(ctx, c, o, nil, &b); err != nil || !strings.Contains(b.String(), `"remaining": 941`) {
		t.Fatalf("account --json: %v\n%s", err, b.String())
	}

	// apps: empty, the table, --json.
	b.Reset()
	o, _ = parseFor(t, devicefarmSessionsFlags)
	f.reply = daemon.ProviderAppsResult{Provider: "devicefarm"}
	if err := runDevicefarmApps(ctx, c, o, nil, &b); err != nil || !strings.Contains(b.String(), "No uploads.") {
		t.Fatalf("no apps: %v\n%s", err, b.String())
	}
	b.Reset()
	app := cloudprov.AppRef{Ref: "arn:upload:1", Name: "maestro-d-0123abcd-a.apk", Type: "ANDROID_APP", Status: "SUCCEEDED", Created: "2026-11-20T00:00:00Z"}
	f.reply = daemon.ProviderAppsResult{Provider: "devicefarm", Apps: []cloudprov.AppRef{app}}
	if err := runDevicefarmApps(ctx, c, o, nil, &b); err != nil || !strings.Contains(b.String(), "maestro-d-0123abcd-a.apk") || !strings.Contains(b.String(), "ANDROID_APP") {
		t.Fatalf("apps: %v\n%s", err, b.String())
	}
	b.Reset()
	o, _ = parseFor(t, devicefarmSessionsFlags, "--json")
	if err := runDevicefarmApps(ctx, c, o, nil, &b); err != nil || !strings.Contains(b.String(), `"arn:upload:1"`) {
		t.Fatalf("apps --json: %v\n%s", err, b.String())
	}

	// upload: the absolute path, the two wordings, --json, and no file.
	b.Reset()
	o, _ = parseFor(t, devicefarmSessionsFlags)
	f.reply = daemon.ProviderUploadResult{Provider: "devicefarm", AppRef: app}
	if err := runDevicefarmUpload(ctx, c, o, []string{"a.apk"}, &b); err != nil || !strings.Contains(b.String(), "maestro-d-0123abcd-a.apk uploaded: arn:upload:1") {
		t.Fatalf("upload: %v\n%s", err, b.String())
	}
	if f.method != "POST" || f.path != "/v1/providers/devicefarm/apps" || !filepath.IsAbs(f.body["app"].(string)) {
		t.Fatalf("upload request: %s %s %v", f.method, f.path, f.body)
	}
	b.Reset()
	app.Reused = true
	f.reply = daemon.ProviderUploadResult{Provider: "devicefarm", AppRef: app}
	if err := runDevicefarmUpload(ctx, c, o, []string{"a.apk"}, &b); err != nil || !strings.Contains(b.String(), "already uploaded (reused)") {
		t.Fatalf("reused: %v\n%s", err, b.String())
	}
	b.Reset()
	o, _ = parseFor(t, devicefarmSessionsFlags, "--json")
	if err := runDevicefarmUpload(ctx, c, o, []string{"a.apk"}, &b); err != nil || !strings.Contains(b.String(), `"reused": true`) {
		t.Fatalf("upload --json: %v\n%s", err, b.String())
	}
	if err := runDevicefarmUpload(ctx, c, o, nil, &b); !hasCode(err, daemon.CodeUsage) {
		t.Fatalf("upload without a file: %v", err)
	}
	// An error from the daemon comes back unchanged.
	f.status, f.reply = http.StatusForbidden, map[string]any{"error": map[string]any{"code": "FORBIDDEN", "message": "install it on a leased cloud device"}}
	if err := runDevicefarmUpload(ctx, c, o, []string{"a.apk"}, &b); !hasCode(err, daemon.CodeForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	for _, run := range []func(context.Context, *daemon.Client, *daemonOpts, []string, io.Writer) error{runDevicefarmAccount, runDevicefarmApps} {
		if err := run(ctx, c, o, nil, &b); !hasCode(err, daemon.CodeForbidden) {
			t.Fatalf("error passthrough: %v", err)
		}
	}
}

func hasCode(err error, code daemon.Code) bool {
	var de *daemon.Error
	return errors.As(err, &de) && de.Code == code
}
