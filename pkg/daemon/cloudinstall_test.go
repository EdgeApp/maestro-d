package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// installFake is a cloudFake whose uploads can be slow and fail, and whose
// installs are counted.
type installFake struct {
	*cloudFake
	uploadDelay time.Duration
	uploadErr   error
	installs    atomic.Int32
	uploads     atomic.Int32
}

func (f *installFake) Upload(_ context.Context, path string) (*cloudprov.AppRef, error) {
	f.uploads.Add(1)
	time.Sleep(f.uploadDelay)
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	return &cloudprov.AppRef{Ref: "arn:upload:" + filepath.Base(path), Name: filepath.Base(path)}, nil
}

func (f *installFake) Install(context.Context, *cloudprov.Allocation, cloudprov.AppRef) (*cloudprov.InstallResult, error) {
	f.installs.Add(1)
	return &cloudprov.InstallResult{}, nil
}

func newInstallEnv(t *testing.T) (*remoteEnv, *installFake, *atomic.Int32) {
	t.Helper()
	e := newRemoteEnv(t, nil)
	f := &installFake{cloudFake: e.f}
	e.s.deps.Providers["devicefarm"] = func() (cloudprov.RemoteProvider, error) { return f, nil }
	var uninstalls atomic.Int32
	e.s.deps.UninstallApp = func(context.Context, string, string, string) error { uninstalls.Add(1); return nil }
	e.s.deps.InstallApp = func(context.Context, string, string, string) error {
		return errors.New("the local installer ran for a cloud device")
	}
	e.s.deps.AppID = func(path string) (string, error) {
		if strings.HasSuffix(path, ".apk") {
			return "org.example", nil
		}
		return "", errors.New("unreadable")
	}
	return e, f, &uninstalls
}

func writeApp(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("PK app"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCloudInstall_UploadInstallRestart(t *testing.T) {
	e, f, uninstalls := newInstallEnv(t)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	events, _, unsub := e.s.events.Subscribe(dev.ID, 0)
	defer unsub()
	apk := writeApp(t, "app.apk")
	res, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: apk, Reinstall: true})
	if err != nil || res.AppID != "org.example" || res.Device != dev.ID {
		t.Fatalf("install: %+v %v", res, err)
	}
	if f.uploads.Load() != 1 || f.installs.Load() != 1 || e.driver(0).restarts.Load() != 1 || uninstalls.Load() != 0 {
		t.Fatalf("uploads %d installs %d restarts %d uninstalls %d", f.uploads.Load(), f.installs.Load(), e.driver(0).restarts.Load(), uninstalls.Load())
	}
	var installed bool
	for len(events) > 0 {
		ev := <-events
		installed = installed || (ev.Type == "app.installed" && ev.Message == "org.example")
	}
	if !installed {
		t.Fatal("no app.installed")
	}
	// The executor session still runs steps.
	if step, err := e.c.Command(ctx, dev.ID, "tapOn", "x", true); err != nil || !step.OK {
		t.Fatalf("step after the install: %+v %v", step, err)
	}
	// From a blob: a remote client's upload.
	data := []byte("PK blob app")
	blob := filepath.Join(t.TempDir(), "b.apk")
	_ = os.WriteFile(blob, data, 0o644)
	if _, err := e.c.UploadBlob(ctx, dev.ID, sha(data), blob, int64(len(data))); err != nil {
		t.Fatalf("blob: %v", err)
	}
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: "blob:sha256:" + sha(data) + "/b.apk"}); err != nil {
		t.Fatalf("install a blob: %v", err)
	}
	// An app id that cannot be read installs anyway.
	if res, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "x.ipa")}); err != nil || res.AppID != "" {
		t.Fatalf("unreadable id: %+v %v", res, err)
	}
	// Uninstall is refused.
	if err := e.c.UninstallApp(ctx, dev.ID, "org.example"); codeOf(t, err) != CodeUsage {
		t.Fatalf("uninstall: %v", err)
	}
	// Provider failures map through their kind.
	f.uploadErr = cloudprov.Errorf(cloudprov.Other, "FAILED: bad apk")
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: apk}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("upload failure: %v", err)
	}
	f.uploadErr = nil
	// A restart refused once (the device settling after the install) is
	// tried again.
	d := e.driver(0)
	before := d.restarts.Load()
	d.restartFails.Store(1)
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: apk}); err != nil || d.restarts.Load() != before+2 {
		t.Fatalf("a restart refused once: %v (restarts %d)", err, d.restarts.Load()-before)
	}
	// A restart that keeps failing is DEVICE_ERROR after three attempts.
	gone := errors.New("session gone")
	d.restartErr.Store(&gone)
	before = d.restarts.Load()
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: apk}); codeOf(t, err) != CodeDeviceError || d.restarts.Load() != before+3 {
		t.Fatalf("restart failure: %v (restarts %d)", err, d.restarts.Load()-before)
	}
}

func TestCloudInstall_WaitsForTheStepInFlight(t *testing.T) {
	e, _, _ := newInstallEnv(t)
	e.stepDelay = 2 * time.Second
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	stepDone := make(chan time.Time, 1)
	go func() {
		_, _ = e.c.Command(ctx, dev.ID, "tapOn", "x", true)
		stepDone <- time.Now()
	}()
	time.Sleep(300 * time.Millisecond)
	// wait=false while the step runs: BUSY.
	r := httptest.NewRequest("POST", "/v1/devices/"+dev.ID+"/apps?wait=false", strings.NewReader(`{"app":"`+writeApp(t, "a.apk")+`"}`))
	r.SetPathValue("id", dev.ID)
	w := httptest.NewRecorder()
	e.s.handleInstallApp(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("wait=false install during a step: %d %s", w.Code, w.Body.String())
	}
	start := time.Now()
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err != nil {
		t.Fatal(err)
	}
	// The step had about 1.7 s left: the install waited for it.
	if took := time.Since(start); took < 1200*time.Millisecond {
		t.Fatalf("the install took %s; it did not wait for the step", took)
	}
	<-stepDone
}

func TestCloudInstall_SlowUploadHoldsTheIdleClock(t *testing.T) {
	e, f, _ := newInstallEnv(t)
	f.uploadDelay = 3 * time.Second
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 1})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err != nil {
		t.Fatalf("install with an upload longer than the idle release: %v", err)
	}
	answered := time.Now()
	waitUntil(t, 4*time.Second, "the idle release after the install", func() bool { return e.s.remote(dev.ID) == nil })
	if since := time.Since(answered); since < 500*time.Millisecond {
		t.Fatalf("released %s after the install answered", since)
	}
}

func TestCloudInstall_AttachAppFile(t *testing.T) {
	e, f, _ := newInstallEnv(t)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	info, err := e.c.Attach(ctx, dev.ID, AttachConfig{AppFile: writeApp(t, "a.apk")})
	if err != nil || info.AppID != "org.example" || f.installs.Load() != 1 {
		t.Fatalf("attach with appFile: %+v %v (installs %d)", info, err, f.installs.Load())
	}
	f.uploadErr = cloudprov.Errorf(cloudprov.Auth, "denied")
	_ = e.c.Detach(ctx, dev.ID)
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{AppFile: writeApp(t, "a.apk")}); codeOf(t, err) != CodeUsage {
		t.Fatalf("attach with a failing upload: %v", err)
	}
}

func TestProviderUtilities(t *testing.T) {
	e, f, _ := newInstallEnv(t)
	ctx := context.Background()
	if res, err := e.c.ProviderAccount(ctx, "devicefarm", ""); err != nil || res.Provider != "devicefarm" || res.Hub != "" {
		t.Fatalf("account: %+v %v", res, err)
	}
	if res, err := e.c.ProviderApps(ctx, "devicefarm", ""); err != nil || res.Apps == nil {
		t.Fatalf("apps: %+v %v", res, err)
	}
	if res, err := e.c.ProviderSessions(ctx, "devicefarm", ""); err != nil || res.Sessions == nil {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	res, err := e.c.ProviderUpload(ctx, "devicefarm", "", writeApp(t, "a.apk"))
	if err != nil || res.Ref != "arn:upload:a.apk" {
		t.Fatalf("upload: %+v %v", res, err)
	}
	if _, err := e.c.ProviderUpload(ctx, "devicefarm", "", ""); codeOf(t, err) != CodeUsage {
		t.Fatalf("upload without app: %v", err)
	}
	f.uploadErr = cloudprov.Errorf(cloudprov.Other, "FAILED")
	if _, err := e.c.ProviderUpload(ctx, "devicefarm", "", "/x.apk"); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("upload failure: %v", err)
	}
	if _, err := e.c.ProviderAccount(ctx, "browserstack", ""); codeOf(t, err) != CodeUsage {
		t.Fatalf("no such provider: %v", err)
	}
	if _, err := e.c.ProviderApps(ctx, "browserstack", ""); codeOf(t, err) != CodeUsage {
		t.Fatalf("apps of no provider: %v", err)
	}
	if _, err := e.c.ProviderUpload(ctx, "browserstack", "", "/x.apk"); codeOf(t, err) != CodeUsage {
		t.Fatalf("upload to no provider: %v", err)
	}
	if _, err := e.c.ProviderSessions(ctx, "browserstack", ""); codeOf(t, err) != CodeUsage {
		t.Fatalf("sessions of no provider: %v", err)
	}
	// A remote caller reads, but never uploads from the daemon's disk.
	for _, path := range []string{"/v1/providers/devicefarm/account", "/v1/providers/devicefarm/apps", "/v1/providers/devicefarm/sessions"} {
		r := httptest.NewRequest("GET", path, nil).WithContext(withCaller(context.Background(), Caller{Remote: true, Addr: "10.0.0.9:1"}))
		w := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("remote %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/v1/providers/devicefarm/apps", strings.NewReader(`{"app":"/etc/passwd"}`))
	r = r.WithContext(withCaller(r.Context(), Caller{Remote: true, Addr: "10.0.0.9:1"}))
	r.SetPathValue("provider", "devicefarm")
	w := httptest.NewRecorder()
	e.s.handleProviderUpload(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "leased cloud device") {
		t.Fatalf("remote upload: %d %s", w.Code, w.Body.String())
	}
}

func TestCloudInstall_WaitsForTheAppOnTheDevice(t *testing.T) {
	e, f, _ := newInstallEnv(t)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	d := e.driver(0)
	// The provider answers first; the device lists the app on the third check.
	d.notYet.Store(2)
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err != nil {
		t.Fatal(err)
	}
	if d.installChecks.Load() != 3 || d.restarts.Load() != 1 {
		t.Fatalf("checks %d restarts %d", d.installChecks.Load(), d.restarts.Load())
	}
	// Never there: DEVICE_ERROR after the wait (3 s with the fast seam),
	// and no restart.
	d.installChecks.Store(0)
	d.notYet.Store(-1)
	start := time.Now()
	_, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")})
	if codeOf(t, err) != CodeDeviceError || !strings.Contains(err.Error(), "was not on the device after 3s") {
		t.Fatalf("never installed: %v", err)
	}
	if took := time.Since(start); took < 3*time.Second || d.restarts.Load() != 1 || d.installChecks.Load() < 10 {
		t.Fatalf("took %s, restarts %d, checks %d", took, d.restarts.Load(), d.installChecks.Load())
	}
	// A device that cannot tell: no wait.
	d.installChecks.Store(0)
	unsupported := errors.New("unknown mobile command")
	d.checkErr.Store(&unsupported)
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err != nil || d.installChecks.Load() != 1 {
		t.Fatalf("unsupported check: %v (checks %d)", err, d.installChecks.Load())
	}
	// An app id that cannot be read: nothing to wait for.
	d.installChecks.Store(0)
	if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "x.ipa")}); err != nil || d.installChecks.Load() != 0 {
		t.Fatalf("no app id: %v (checks %d)", err, d.installChecks.Load())
	}
	// A client that gives up ends the wait.
	d.checkErr.Store(nil)
	d.notYet.Store(-1)
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := e.c.InstallApp(cctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err == nil {
		t.Fatal("a cancelled install succeeded")
	}
	if f.installs.Load() != 5 {
		t.Fatalf("installs %d", f.installs.Load())
	}
}

// An attach that arrives while an install restarts the WebDriver session of
// a device that is not attached waits for the install, so no step drives a
// session being reopened.
func TestCloudInstall_AttachWaitsForTheInstall(t *testing.T) {
	e, _, _ := newInstallEnv(t)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	d := e.driver(0)
	d.restartDelay.Store(int64(1500 * time.Millisecond))
	installed := make(chan struct{})
	go func() {
		defer close(installed)
		if _, err := e.c.InstallApp(ctx, dev.ID, InstallRequest{App: writeApp(t, "a.apk")}); err != nil {
			t.Error(err)
		}
	}()
	waitUntil(t, 5*time.Second, "the restart to begin", func() bool { return d.restarts.Load() == 1 })
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	attached := time.Now().UnixNano()
	if done := d.restartedAt.Load(); done == 0 || done > attached {
		t.Fatalf("the attach returned before the install's restart finished (restart %d, attach %d)", done, attached)
	}
	<-installed
	if step, err := e.c.Command(ctx, dev.ID, "tapOn", "x", true); err != nil || !step.OK {
		t.Fatalf("a step after both: %+v %v", step, err)
	}
}
