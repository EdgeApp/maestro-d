package cli

// daemon_devicefarm_e2e_test.go: the maestro-d binary against a fake AWS
// Device Farm (pkg/devicefarm/dftest), with no credentials or network:
// locations and filters, the cloud catalog, and (from M3) launching,
// driving and releasing cloud devices.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

// awsEnvDrop are the inherited variables that could hand a test daemon real
// AWS credentials.
var awsEnvDrop = []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_*", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
	"MAESTRO_D_DEVICEFARM_PROJECT"}

// dfEnv pins the AWS credential chain of every command e runs: the
// inherited AWS variables are dropped, the shared files are empty files in
// the test's temp dir, instance metadata is off, and fake points the
// provider at the fake. It returns the variables to add, with the fake's
// keys when withKey.
func dfEnv(t *testing.T, e *e2e, fake *dftest.Fake, withKey bool) []string {
	t.Helper()
	e.drop = append(e.drop, awsEnvDrop...)
	dir := t.TempDir()
	creds, conf := filepath.Join(dir, "credentials"), filepath.Join(dir, "config")
	for _, f := range []string{creds, conf} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"AWS_SHARED_CREDENTIALS_FILE=" + creds, "AWS_CONFIG_FILE=" + conf, "AWS_EC2_METADATA_DISABLED=true",
		"MAESTRO_D_DEVICEFARM_ENDPOINT=" + fake.URL(), "MAESTRO_D_TEST_FAST_CLOUD=1"}
	if withKey {
		env = append(env, "AWS_ACCESS_KEY_ID="+fakeKeyID, "AWS_SECRET_ACCESS_KEY=fake-secret-for-tests")
	}
	return env
}

// fakeKeyID is the access key id the fake-backed tests use; it must never
// appear in a run file, a log or an answer.
const fakeKeyID = "AKIAFAKEFAKEFAKE0001"

func TestE2E_DeviceFarmCatalog(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := append(dfEnv(t, e, fake, true), "MAESTRO_D_MOCK_DEVICES=mock-1")
	dflag := []string{"--daemon", "dfcat"}

	// Every entry has a location; the mock device is on this machine.
	r := e.runEnv(env, append([]string{"device", "list", "--json"}, dflag...)...)
	var listed daemon.DevicesResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &listed) != nil {
		t.Fatalf("device list: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	var mock *daemon.DeviceInfo
	for i := range listed.Devices {
		if listed.Devices[i].Location == "" {
			t.Fatalf("%s has no location", listed.Devices[i].ID)
		}
		if listed.Devices[i].ID == "mock-1" {
			mock = &listed.Devices[i]
		}
	}
	if mock == nil || mock.Location != daemon.LocationLocal || mock.Machine != daemon.MachineName() {
		t.Fatalf("mock-1: %+v", mock)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--location", "mesh", "--json"}, dflag...)...)
	if r.code != 0 || strings.Contains(r.stdout, `"mock-1"`) {
		t.Fatalf("--location mesh: exit %d\n%s", r.code, r.stdout)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--name", "^mock", "--location", "local"}, dflag...)...)
	if r.code != 0 || !strings.Contains(r.stdout, "LOCATION") || !strings.Contains(r.stdout, "mock-1") {
		t.Fatalf("table: %s%s", r.stdout, r.stderr)
	}
	if r = e.runEnv(env, append([]string{"device", "list", "--name", "(["}, dflag...)...); r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage {
		t.Fatalf("bad regexp: exit %d\n%s", r.code, r.stderr)
	}

	// The cloud catalog, filtered on the daemon.
	r = e.runEnv(env, append([]string{"device", "catalog", "--location", "cloud", "--platform", "ios", "--os-min", "16", "--os-max", "18", "--json"}, dflag...)...)
	var cat daemon.CatalogResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &cat) != nil || len(cat.Cloud) != 1 {
		t.Fatalf("catalog: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if ms := cat.Cloud[0].Models; len(ms) != 1 || ms[0].Name != "Apple iPhone 15" || ms[0].ID != "df:ios:apple-iphone-15:17.4" || cat.Cloud[0].Provider != "devicefarm" {
		t.Fatalf("catalog models: %+v", cat.Cloud)
	}
	r = e.runEnv(env, append([]string{"device", "catalog", "--location", "cloud", "--platform", "android", "--name", "pixel", "--available"}, dflag...)...)
	if r.code != 0 || !strings.Contains(r.stdout, "Google Pixel 8") || !strings.Contains(r.stdout, "AVAILABILITY") || strings.Contains(r.stdout, "Galaxy") {
		t.Fatalf("catalog table: %s%s", r.stdout, r.stderr)
	}
	for _, op := range []string{"ListDevices"} {
		if fake.CountOp(op) == 0 {
			t.Fatalf("the fake never saw %s: %v", op, fake.Ops())
		}
	}

	// Another daemon without AWS credentials answers USAGE naming them.
	noKey := dfEnv(t, e, fake, false)
	r = e.runEnv(noKey, "device", "catalog", "--location", "cloud", "--daemon", "dfnokey")
	if r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage || !strings.Contains(r.stderr, "AWS_ACCESS_KEY_ID") {
		t.Fatalf("without credentials: exit %d\n%s", r.code, r.stderr)
	}
	// Without --location cloud the catalog asks no provider.
	before := fake.CountOp("ListDevices")
	e.runEnv(noKey, "device", "catalog", "--platform", "android", "--daemon", "dfnokey")
	if fake.CountOp("ListDevices") != before {
		t.Fatal("a local catalog asked Device Farm")
	}
}

// dfLaunch is `device create --provider devicefarm` with args, decoded.
func dfLaunch(t *testing.T, e *e2e, env []string, args ...string) daemon.DeviceInfo {
	t.Helper()
	r := e.runEnv(env, append([]string{"device", "create", "--provider", "devicefarm", "--json"}, args...)...)
	var res daemon.DeviceResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil || res.Device.ID == "" {
		t.Fatalf("device create: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	return res.Device
}

func TestE2E_DeviceFarmCreateDrive(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)
	d := []string{"--daemon", "dfdrive"}
	apk := filepath.Join(e.cwd, "fixture.apk")
	if err := os.WriteFile(apk, []byte("PK fake apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	flowFile := filepath.Join(e.cwd, "flow.yaml")
	if err := os.WriteFile(flowFile, []byte("appId: org.example\n---\n- tapOn: Button\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dev := dfLaunch(t, e, env, append([]string{"--platform", "android", "--name", "Google Pixel 8", "--app", apk, "--app-id", "org.example"}, d...)...)
	if !strings.HasPrefix(dev.ID, "df-") || dev.Location != daemon.LocationCloud || dev.Provider != "devicefarm" || dev.Kind != "cloud" ||
		dev.State != "allocated" || dev.ProviderRef == "" || dev.ExpiresAt == "" || dev.IdleReleaseSec != 600 || dev.Name != "Google Pixel 8" {
		t.Fatalf("launched: %+v", dev)
	}
	// One upload, a session with the app and Appium 2, a WebDriver session
	// without a udid.
	if n := fake.CountOp("CreateUpload"); n != 1 {
		t.Fatalf("uploads: %d", n)
	}
	sessions := fake.Sessions()
	if len(sessions) != 1 || sessions[0].AppARN == "" || sessions[0].Parameters["appium:version"] != "2" || sessions[0].Name != dev.ID {
		t.Fatalf("session: %+v", sessions)
	}
	if caps := sessions[0].Caps; len(caps) != 1 || caps[0]["appium:udid"] != nil || caps[0]["platformName"] != "Android" {
		t.Fatalf("WebDriver caps: %+v", caps)
	}

	id := append([]string{"--device", dev.ID}, d...)
	e.mustOK(env, append([]string{"tapOn", "Button"}, id...)...)
	e.mustOK(env, append([]string{"run", flowFile}, id...)...)
	shot := filepath.Join(e.cwd, "shot.png")
	e.mustOK(env, append([]string{"get", "screenshot", "--out", shot}, id...)...)
	if fi, err := os.Stat(shot); err != nil || fi.Size() == 0 {
		t.Fatalf("screenshot: %v", err)
	}
	r := e.runEnv(env, append([]string{"device", "list", "--location", "cloud", "--json"}, d...)...)
	if r.code != 0 || !strings.Contains(r.stdout, `"`+dev.ID+`"`) {
		t.Fatalf("listed while held: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(env, append([]string{"device", "delete", dev.ID, "--json"}, d...)...)
	var del daemon.DeviceResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &del) != nil || del.Device.DeviceMinutes == nil {
		t.Fatalf("delete: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if stops := fake.StoppedSessions(); len(stops) != 1 || stops[0] != dev.ProviderRef {
		t.Fatalf("stops: %v", stops)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--location", "cloud", "--json"}, d...)...)
	if strings.Contains(r.stdout, `"`+dev.ID+`"`) {
		t.Fatalf("still listed after delete: %s", r.stdout)
	}
	// The ledger is empty again, and no secret reached a run file.
	if data, err := os.ReadFile(filepath.Join(e.home, "run", "cloud.json")); err == nil {
		t.Fatalf("ledger not empty: %s", data)
	}
	assertNoSecrets(t, e.home, fake)
}

// mustOK runs a command and fails the test on a non-zero exit.
func (e *e2e) mustOK(env []string, args ...string) result {
	e.t.Helper()
	r := e.runEnv(env, args...)
	if r.code != 0 {
		e.t.Fatalf("%v: exit %d\n%s%s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

// assertNoSecrets fails when a file under dir holds the fake's key id or
// any of its sessions' endpoint tokens.
func assertNoSecrets(t *testing.T, dir string, fake *dftest.Fake) {
	t.Helper()
	needles := []string{fakeKeyID, "remote-endpoint/"}
	for _, s := range fake.Sessions() {
		needles = append(needles, s.Token)
	}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() > 8<<20 {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, n := range needles {
			if n != "" && strings.Contains(string(data), n) {
				t.Errorf("%s holds a secret (%q)", p, n[:min(len(n), 12)])
			}
		}
		return nil
	})
}

// logLedger logs the sessions the fake created and stopped.
func logLedger(t *testing.T, label string, fake *dftest.Fake) {
	t.Helper()
	t.Logf("%s: CreateRemoteAccessSession %d %v; StopRemoteAccessSession %d %v", label,
		len(fake.CreatedSessions()), tails(fake.CreatedSessions()), len(fake.StoppedSessions()), tails(fake.StoppedSessions()))
}

// tails shortens session ARNs (…:session:<project>/<session>/<n>) to the
// session id's first 8 characters.
func tails(arns []string) []string {
	out := make([]string, len(arns))
	for i, a := range arns {
		parts := strings.Split(a, "/")
		out[i] = a
		if len(parts) == 3 && len(parts[1]) >= 8 {
			out[i] = parts[1][:8]
		}
	}
	return out
}

// allStopped reports whether every session the fake created was stopped.
func allStopped(fake *dftest.Fake) bool {
	stopped := map[string]bool{}
	for _, a := range fake.StoppedSessions() {
		stopped[a] = true
	}
	for _, a := range fake.CreatedSessions() {
		if !stopped[a] {
			return false
		}
	}
	return true
}

func TestE2E_DeviceFarmPending(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)
	d := []string{"--daemon", "dfpend"}

	// (a) The first candidate stays in PENDING_DEVICE: it is stopped, and the
	// next one runs.
	fake.HoldDevice(dftest.ARNPixel8, "PENDING_DEVICE")
	dev := dfLaunch(t, e, env, append([]string{"--platform", "android", "--name", "^Google Pixel [78]$"}, d...)...)
	logLedger(t, "(a) pending device", fake)
	if dev.Name != "Google Pixel 7" || len(fake.CreatedSessions()) != 2 || len(fake.StoppedSessions()) != 1 ||
		fake.StoppedSessions()[0] != fake.CreatedSessions()[0] {
		t.Fatalf("(a): %+v", dev)
	}
	e.mustOK(env, append([]string{"device", "delete", dev.ID}, d...)...)
	fake.HoldDevice(dftest.ARNPixel8, "")

	// (b) Every session waits on the account's concurrency past the boot
	// timeout: BUSY, and each one stopped.
	fake.Hold("PENDING_CONCURRENCY")
	r := e.runEnv(env, append([]string{"device", "create", "--provider", "devicefarm", "--platform", "android", "--name", "Google Pixel 8", "--boot-timeout", "2"}, d...)...)
	logLedger(t, "(b) concurrency timeout", fake)
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeBusy || !strings.Contains(r.stderr, "PENDING_CONCURRENCY") || !allStopped(fake) {
		t.Fatalf("(b): exit %d\n%s", r.code, r.stderr)
	}

	// (c) The WebDriver session does not open: DEVICE_ERROR, redacted, and
	// the session stopped.
	fake.Hold("")
	fake.FailWebDriver(true)
	r = e.runEnv(env, append([]string{"device", "create", "--provider", "devicefarm", "--platform", "android", "--name", "Google Pixel 8"}, d...)...)
	logLedger(t, "(c) WebDriver failure", fake)
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeDeviceError || strings.Contains(r.stderr, "remote-endpoint") || !allStopped(fake) {
		t.Fatalf("(c): exit %d\n%s", r.code, r.stderr)
	}
	fake.FailWebDriver(false)

	// (d) The client that asked is killed while the session waits: the
	// launch is cancelled and its session stopped, nothing left queued.
	fake.Hold("PENDING_CONCURRENCY")
	cmd := exec.Command(e.bin, append([]string{"device", "create", "--provider", "devicefarm", "--platform", "android", "--name", "Google Pixel 8"}, d...)...)
	cmd.Dir, cmd.Env = e.cwd, e.env(env...)
	created := len(fake.CreatedSessions())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(fake.CreatedSessions()) == created && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(time.Second)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	// The provider acknowledges Stop before it settles final device minutes
	// and removes the launch from the daemon's ledger.
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(e.home, "run", "cloud.json"))
		if allStopped(fake) && os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("(d): cleanup after 5 s: all stopped=%t, ledger=%v", allStopped(fake), ledgerRefs(t, e))
		}
		time.Sleep(50 * time.Millisecond)
	}
	logLedger(t, "(d) client killed", fake)
	fake.Hold("")

	// (e) The provider ends a held session: the next step answers
	// DEVICE_ERROR with the reason, and the device is listed as ended.
	dev = dfLaunch(t, e, env, append([]string{"--platform", "android", "--name", "Google Pixel 8"}, d...)...)
	fake.ExpireSession(dev.ProviderRef)
	r = e.runEnv(env, append([]string{"tapOn", "Button", "--device", dev.ID}, d...)...)
	logLedger(t, "(e) session expired", fake)
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeDeviceError || !strings.Contains(r.stderr, "gone") {
		t.Fatalf("(e) step: exit %d\n%s", r.code, r.stderr)
	}
	r = e.runEnv(env, append([]string{"device", "list", "--location", "cloud", "--json"}, d...)...)
	var listed daemon.DevicesResult
	if json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed.Devices) != 1 || listed.Devices[0].State != "ended" || listed.Devices[0].EndedReason == "" {
		t.Fatalf("(e) listing: %s", r.stdout)
	}
	e.mustOK(env, append([]string{"device", "delete", dev.ID}, d...)...)
	if data, err := os.ReadFile(filepath.Join(e.home, "run", "cloud.json")); err == nil {
		t.Fatalf("ledger not empty: %s", data)
	}
}

// watchEvents collects daemon name's events until the test ends.
func watchEvents(t *testing.T, e *e2e, name string) func() []daemon.Event {
	t.Helper()
	t.Setenv(daemon.EnvHome, e.home)
	c, err := daemon.Dial(name)
	if err != nil || c == nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	var mu sync.Mutex
	var evs []daemon.Event
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = c.Events(ctx, "", 0, func(ev daemon.Event) bool {
			mu.Lock()
			evs = append(evs, ev)
			mu.Unlock()
			return true
		})
	}()
	return func() []daemon.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]daemon.Event(nil), evs...)
	}
}

// released finds the device.released event of id.
func released(evs []daemon.Event, id string) *daemon.Event {
	for i := range evs {
		if evs[i].Type == "device.released" && evs[i].Device == id {
			return &evs[i]
		}
	}
	return nil
}

func TestE2E_DeviceFarmIdle(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)

	// (a) Nothing happens for longer than --idle-release: released.
	e.mustOK(env, "device", "list", "--daemon", "dfidle")
	events := watchEvents(t, e, "dfidle")
	dev := dfLaunch(t, e, env, "--platform", "ios", "--name", "Apple iPhone 15", "--idle-release", "3s", "--daemon", "dfidle")
	// The idle interval is already running when the create command returns.
	started := func(id string) time.Time {
		t.Helper()
		session, ok := fake.Session(id)
		if !ok || session.Started.IsZero() {
			t.Fatalf("no provider launch time for %s", id)
		}
		return session.Started
	}
	start := started(dev.ProviderRef)
	for released(events(), dev.ID) == nil && time.Since(start) < 15*time.Second {
		time.Sleep(100 * time.Millisecond)
	}
	ev := released(events(), dev.ID)
	if ev == nil || ev.Message != "idle" || ev.DeviceMinutes == nil {
		t.Fatalf("(a) no idle release: %+v", events())
	}
	t.Logf("(a) %s: device.released reason=%s deviceMinutes=%+v, %s after the session started",
		dev.ID, ev.Message, *ev.DeviceMinutes, time.Since(start).Round(100*time.Millisecond))
	if took := time.Since(start); took < 2*time.Second {
		t.Fatalf("(a) released after %s, before its 3 s", took)
	}
	r := e.runEnv(env, "device", "list", "--location", "cloud", "--json", "--daemon", "dfidle")
	if strings.Contains(r.stdout, dev.ID) || !allStopped(fake) {
		t.Fatalf("(a) still held: %s", r.stdout)
	}

	// (b) A held device outlives the daemon's own idle timeout: the daemon
	// exits only once the device is released.
	idleEnv := append(append([]string{}, env...), "MAESTRO_D_IDLE_TIMEOUT=2")
	dev = dfLaunch(t, e, idleEnv, "--platform", "android", "--name", "Google Pixel 8", "--idle-release", "6s", "--daemon", "dfidle2")
	start = started(dev.ProviderRef)
	time.Sleep(time.Until(start.Add(4 * time.Second)))
	t.Setenv(daemon.EnvHome, e.home)
	if info, _ := daemon.LiveInfo("dfidle2"); info == nil {
		t.Fatalf("(b) the daemon exited at 4 s with a device held")
	}
	if s, ok := fake.Session(dev.ProviderRef); !ok || s.Status != "RUNNING" {
		t.Fatalf("(b) the session is not running at 4 s: %+v", s)
	}
	t.Logf("(b) at %s: daemon running, %s allocated", time.Since(start).Round(100*time.Millisecond), dev.ID)
	for time.Since(start) < 15*time.Second {
		if s, _ := fake.Session(dev.ProviderRef); s.Status == "COMPLETED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	releasedAt := time.Since(start)
	for time.Since(start) < 20*time.Second {
		if info, _ := daemon.LiveInfo("dfidle2"); info == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	exitedAt := time.Since(start)
	t.Logf("(b) released at %s, daemon exited at %s", releasedAt.Round(100*time.Millisecond), exitedAt.Round(100*time.Millisecond))
	if s, _ := fake.Session(dev.ProviderRef); s.Status != "COMPLETED" || releasedAt < 5*time.Second || exitedAt < releasedAt+time.Second || exitedAt > 18*time.Second {
		t.Fatalf("(b) released at %s, exited at %s, session %s", releasedAt, exitedAt, s.Status)
	}
}

// killDaemon kills daemon name with SIGKILL, as a crash would.
func killDaemon(t *testing.T, e *e2e, name string) {
	t.Helper()
	t.Setenv(daemon.EnvHome, e.home)
	info, err := daemon.ReadInfo(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for daemon.PIDAlive(info.PID) {
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("$ kill -9 %d   # daemon %s", info.PID, name)
}

// ledgerRefs are the session refs the ledger holds.
func ledgerRefs(t *testing.T, e *e2e) []string {
	t.Helper()
	t.Setenv(daemon.EnvHome, e.home)
	entries, err := daemon.ReadLedger()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		out = append(out, en.ProviderRef)
	}
	return out
}

func TestE2E_DeviceFarmRelease(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)
	noKey := dfEnv(t, e, fake, false)
	creds := filepath.Join(t.TempDir(), "creds")
	writeCreds(t, creds)
	stopped := func(ref string) bool {
		s, ok := fake.Session(ref)
		return ok && s.Status == "COMPLETED"
	}

	// The daemon stops: its device is released.
	dev := dfLaunch(t, e, env, "--platform", "android", "--name", "Google Pixel 8", "--daemon", "dfrel")
	e.mustOK(env, "stop", "--daemon", "dfrel")
	if !stopped(dev.ProviderRef) {
		t.Fatalf("stop did not release %s", dev.ID)
	}

	// A crash: the next command auto-spawns the same name without
	// credentials, which cannot release; the ledger keeps the session.
	dev = dfLaunch(t, e, env, "--platform", "android", "--name", "Google Pixel 8", "--daemon", "dfrel")
	killDaemon(t, e, "dfrel")
	e.mustOK(noKey, "device", "list", "--daemon", "dfrel")
	time.Sleep(time.Second)
	if refs := ledgerRefs(t, e); len(refs) != 1 || refs[0] != dev.ProviderRef || stopped(dev.ProviderRef) {
		t.Fatalf("after the auto-spawn without credentials: ledger %v", refs)
	}
	// --orphans without credentials reports it unreleased, with its ARN.
	r := e.runEnv(noKey, "device", "stop", "--orphans", "--json")
	if r.code == 0 || !strings.Contains(r.stdout, `"unreleased"`) || !strings.Contains(r.stdout, dev.ProviderRef) || len(ledgerRefs(t, e)) != 1 {
		t.Fatalf("--orphans without credentials: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// stdout is one JSON document, and its exitCode is the process's.
	var out struct {
		OK       bool `json:"ok"`
		ExitCode int  `json:"exitCode"`
	}
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	if err := dec.Decode(&out); err != nil || out.OK || out.ExitCode != r.code || dec.More() {
		t.Fatalf("--orphans --json stdout: %+v %v (exit %d)\n%s", out, err, r.code, r.stdout)
	}
	// With the credentials file it releases it, and the entry is gone.
	r = e.runEnv(noKey, "device", "stop", "--orphans", "--devicefarm-credentials-file", creds, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, dev.ID) || !stopped(dev.ProviderRef) || len(ledgerRefs(t, e)) != 0 {
		t.Fatalf("--orphans with credentials: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	e.mustOK(noKey, "stop", "--daemon", "dfrel")

	// A crash, then an auto-spawn with credentials: it answers at once and
	// releases its leftover by itself.
	dev = dfLaunch(t, e, env, "--platform", "ios", "--name", "Apple iPhone 15", "--daemon", "dfrel")
	killDaemon(t, e, "dfrel")
	e.mustOK(env, "device", "list", "--daemon", "dfrel")
	start := time.Now()
	for !stopped(dev.ProviderRef) && time.Since(start) < 10*time.Second {
		time.Sleep(100 * time.Millisecond)
	}
	if !stopped(dev.ProviderRef) || len(ledgerRefs(t, e)) != 0 {
		t.Fatalf("self-release: session %s stopped=%v, ledger %v", dev.ID, stopped(dev.ProviderRef), ledgerRefs(t, e))
	}
	t.Logf("self-release took %s", time.Since(start).Round(100*time.Millisecond))

	// Every session is COMPLETED, by the provider's own listing.
	r = e.mustOK(env, "devicefarm", "sessions", "--json", "--daemon", "dfrel")
	var ses daemon.ProviderSessionsResult
	if json.Unmarshal([]byte(r.stdout), &ses) != nil || len(ses.Sessions) != 3 {
		t.Fatalf("sessions: %s", r.stdout)
	}
	for _, s := range ses.Sessions {
		if s.Status != "COMPLETED" || !strings.HasPrefix(s.Name, "df-") {
			t.Fatalf("session %+v", s)
		}
	}
	e.mustOK(env, "stop", "--daemon", "dfrel")
	assertNoSecrets(t, e.home, fake)
}

// writeFakeApp writes a stand-in app file under dir.
func writeFakeApp(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("PK fake "+name), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFakeIPA writes an .ipa whose Info.plist names bundleID, so the
// daemon can read its app id.
func writeFakeIPA(t *testing.T, dir, name, bundleID string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Payload/B.app/Info.plist")
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>`+bundleID+`</string></dict></plist>`)
	_ = zw.Close()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestE2E_DeviceFarmInstall(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)
	d := []string{"--daemon", "dfinst"}
	dev := dfLaunch(t, e, env, append([]string{"--platform", "ios", "--name", "Apple iPhone 15"}, d...)...)
	id := append([]string{"--device", dev.ID}, d...)
	e.mustOK(env, append([]string{"tapOn", "Button"}, id...)...)
	// Device Farm answers the install before the device has the app: the
	// install waits until the device lists it.
	fake.SetInstallLatency(1500 * time.Millisecond)
	start := time.Now()
	r := e.mustOK(env, append([]string{"install", writeFakeIPA(t, e.cwd, "b.ipa", "org.b")}, id...)...)
	if !strings.Contains(r.stdout, "org.b") {
		t.Fatalf("install: %s", r.stdout)
	}
	if took := time.Since(start); took < 1500*time.Millisecond {
		t.Fatalf("the install answered after %s, before the device had the app", took)
	}
	s, _ := fake.Session(dev.ProviderRef)
	if len(s.Installed) != 1 || len(s.Caps) != 2 || s.Caps[1]["appium:udid"] != nil {
		t.Fatalf("install into the session and reopen: installed %v, WebDriver sessions %d", s.Installed, len(s.Caps))
	}
	e.mustOK(env, append([]string{"tapOn", "Button"}, id...)...)
	if r := e.runEnv(env, append([]string{"uninstall", "org.b"}, id...)...); r.code != 2 || !strings.Contains(r.stderr, "no uninstall during a session") {
		t.Fatalf("uninstall: exit %d\n%s", r.code, r.stderr)
	}
	// A flow that records: the step and the run say the device has no
	// screen capture, and no Go type reaches the output.
	flowPath := filepath.Join(e.cwd, "rec-flow.yaml")
	if err := os.WriteFile(flowPath, []byte("appId: org.b\n---\n- startRecording: rec\n- tapOn: Button\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = e.runEnv(env, append([]string{"run", flowPath}, id...)...)
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "no screen capture") || strings.Contains(r.stdout+r.stderr, "*flow.") {
		t.Fatalf("a recording flow: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	e.mustOK(env, append([]string{"device", "delete", dev.ID}, d...)...)
}

func TestE2E_DeviceFarmUtilities(t *testing.T) {
	e := newE2E(t)
	fake := dftest.New(t)
	env := dfEnv(t, e, fake, true)
	d := []string{"--daemon", "dfutil", "--json"}
	r := e.mustOK(env, append([]string{"devicefarm", "account"}, d...)...)
	if !strings.Contains(r.stdout, `"trialMinutes"`) || strings.Contains(r.stdout, "maxSlots") {
		t.Fatalf("account: %s", r.stdout)
	}
	apk := writeFakeApp(t, e.cwd, "a.apk")
	r = e.mustOK(env, append([]string{"devicefarm", "upload", apk}, d...)...)
	var up daemon.ProviderUploadResult
	if json.Unmarshal([]byte(r.stdout), &up) != nil || up.Ref == "" || up.Reused {
		t.Fatalf("upload: %s", r.stdout)
	}
	r = e.mustOK(env, append([]string{"devicefarm", "upload", apk}, d...)...)
	if json.Unmarshal([]byte(r.stdout), &up) != nil || !up.Reused || fake.CountOp("CreateUpload") != 1 {
		t.Fatalf("second upload: %s (CreateUpload %d)", r.stdout, fake.CountOp("CreateUpload"))
	}
	r = e.mustOK(env, append([]string{"devicefarm", "apps"}, d...)...)
	if !strings.Contains(r.stdout, up.Name) {
		t.Fatalf("apps: %s", r.stdout)
	}
	e.mustOK(env, append([]string{"devicefarm", "sessions"}, d...)...)
	// The tables.
	for _, sub := range []string{"account", "apps", "sessions"} {
		e.mustOK(env, "devicefarm", sub, "--daemon", "dfutil")
	}
	r = e.mustOK(env, "devicefarm", "upload", apk, "--daemon", "dfutil")
	if !strings.Contains(r.stdout, "reused") {
		t.Fatalf("upload table: %s", r.stdout)
	}
	if r := e.runEnv(env, "devicefarm", "upload", "--daemon", "dfutil"); r.code != 2 {
		t.Fatalf("upload without a file: exit %d", r.code)
	}
}
