package cli

// hub_devicefarm_e2e_test.go: two hub binaries against a fake AWS Device
// Farm, each standing in for its own machine (MAESTRO_D_TEST_MACHINE):
// locations through the mesh, the cloud catalog routed to the hub with
// credentials, and (from M4) cloud leases.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

// dfHubs are two linked hubs: a on "machine-a" without credentials, b on
// "machine-b" with --devicefarm (and extraB), each sharing one mock device.
type dfHubs struct {
	e          *e2e
	fake       *dftest.Fake
	a, b       daemon.Info
	envA, envB []string
	// client is the environment of a CLI on machine-a that targets hub a.
	client []string
}

func startDFHubs(t *testing.T, extraB ...string) *dfHubs {
	t.Helper()
	e := newE2E(t)
	h := &dfHubs{e: e, fake: dftest.New(t)}
	aws := dfEnv(t, e, h.fake, false)
	creds := filepath.Join(t.TempDir(), "aws-credentials")
	writeCreds(t, creds)
	h.envA = append(append([]string{}, aws...), daemon.EnvHome+"="+filepath.Join(e.cwd, "home-a"), "MAESTRO_D_MOCK_DEVICES=mock-a", "MAESTRO_D_TEST_MACHINE=machine-a")
	h.envB = append(append([]string{}, aws...), daemon.EnvHome+"="+filepath.Join(e.cwd, "home-b"), "MAESTRO_D_MOCK_DEVICES=mock-b", "MAESTRO_D_TEST_MACHINE=machine-b",
		"AWS_SHARED_CREDENTIALS_FILE="+creds)
	t.Cleanup(func() {
		e.runEnv(h.envB, "hub", "stop", "--daemon", "dfhub-b")
		e.runEnv(h.envA, "hub", "stop", "--daemon", "dfhub-a")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "df-tok", "--peer-token", "df-peer", "--json"}
	started := func(r result) daemon.Info {
		t.Helper()
		var out struct {
			Daemon daemon.Info `json:"daemon"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		return out.Daemon
	}
	h.a = started(e.runEnv(h.envA, append([]string{"hub", "start", "--daemon", "dfhub-a", "--name", "hub-a", "--share-device", "mock-a"}, mesh...)...))
	h.b = started(e.runEnv(h.envB, append(append([]string{"hub", "start", "--daemon", "dfhub-b", "--name", "hub-b", "--peer", h.a.HTTPAddr, "--devicefarm", "--share-device", "mock-b"}, extraB...), mesh...)...))
	h.client = append(append([]string{}, aws...), daemon.EnvHome+"="+filepath.Join(e.cwd, "home-c"), "MAESTRO_D_TEST_MACHINE=machine-a",
		"MAESTRO_D_HUB="+h.a.HTTPAddr, "MAESTRO_D_TOKEN=df-tok")
	// Wait for the link: hub status through a names b, linked.
	for i := 0; i < 50; i++ {
		r := e.runEnv(h.client, "hub", "status", "--json")
		var st daemon.HubStatusResult
		if json.Unmarshal([]byte(r.stdout), &st) == nil && len(st.Hub.Peers) == 1 && st.Hub.Peers[0].State == "linked" {
			if strings.Join(st.Hub.Peers[0].Cloud, ",") != "devicefarm" {
				t.Fatalf("b's hello did not announce devicefarm: %+v", st.Hub.Peers[0])
			}
			return h
		}
		sleepBriefly()
	}
	t.Fatal("hubs did not link")
	return nil
}

// writeCreds writes a shared credentials file with the fake's keys, mode
// 0600.
func writeCreds(t *testing.T, path string) {
	t.Helper()
	data := "[default]\naws_access_key_id = " + fakeKeyID + "\naws_secret_access_key = fake-secret-for-tests\n"
	if err := writeFile0600(path, data); err != nil {
		t.Fatal(err)
	}
}

func TestE2E_HubDeviceFarmLocations(t *testing.T) {
	h := startDFHubs(t)
	e := h.e
	list := func(args ...string) []daemon.DeviceInfo {
		t.Helper()
		r := e.runEnv(h.client, append([]string{"device", "list", "--platform", "mock", "--json"}, args...)...)
		var res daemon.DevicesResult
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil {
			t.Fatalf("device list %v: exit %d\n%s%s", args, r.code, r.stdout, r.stderr)
		}
		return res.Devices
	}
	byID := map[string]daemon.DeviceInfo{}
	for _, d := range list() {
		byID[d.ID] = d
	}
	if d := byID["mock-a@hub-a"]; d.Location != daemon.LocationLocal || d.Machine != "machine-a" {
		t.Fatalf("a's mock: %+v", d)
	}
	if d := byID["mock-b@hub-b"]; d.Location != daemon.LocationMesh || d.Machine != "machine-b" {
		t.Fatalf("b's mock: %+v", d)
	}
	if got := list("--location", "local"); len(got) != 1 || got[0].ID != "mock-a@hub-a" {
		t.Fatalf("--location local: %+v", got)
	}
	if got := list("--location", "mesh"); len(got) != 1 || got[0].ID != "mock-b@hub-b" {
		t.Fatalf("--location mesh: %+v", got)
	}
	r := e.runEnv(h.client, "device", "list", "--platform", "mock")
	if r.code != 0 || !strings.Contains(r.stdout, "mesh (machine-b)") {
		t.Fatalf("table: %s%s", r.stdout, r.stderr)
	}
	// The cloud catalog, asked at a, is answered by b.
	r = e.runEnv(h.client, "device", "catalog", "--location", "cloud", "--platform", "android", "--json")
	var cat daemon.CatalogResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &cat) != nil || len(cat.Cloud) != 1 || cat.Cloud[0].Hub != "hub-b" || len(cat.Cloud[0].Models) == 0 {
		t.Fatalf("routed catalog: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// Named, a answers it has no credentials.
	if r = e.runEnv(h.client, "device", "catalog", "--location", "cloud", "--hub", "hub-a"); r.code != 2 || !strings.Contains(r.stderr, "--devicefarm") {
		t.Fatalf("catalog at a: exit %d\n%s", r.code, r.stderr)
	}
	// b's own status lists its provider and cap.
	r = e.runEnv(h.envB, "hub", "status", "--daemon", "dfhub-b", "--json")
	var st daemon.HubStatusResult
	if json.Unmarshal([]byte(r.stdout), &st) != nil || len(st.Hub.Cloud) != 1 || st.Hub.Cloud[0].MaxDevices != 2 {
		t.Fatalf("b's status: %s%s", r.stdout, r.stderr)
	}
}

func sleepBriefly() { time.Sleep(100 * time.Millisecond) }

func writeFile0600(path, data string) error { return os.WriteFile(path, []byte(data), 0o600) }

// dfLease is `lease --location cloud` through hub a with args, decoded.
func dfLease(t *testing.T, h *dfHubs, args ...string) daemon.LeaseResult {
	t.Helper()
	r := h.e.runEnv(h.client, append([]string{"lease", "--location", "cloud", "--json"}, args...)...)
	var res daemon.LeaseResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil || res.Lease.ID == "" {
		t.Fatalf("lease: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	return res
}

func TestE2E_HubDeviceFarmLease(t *testing.T) {
	creds := filepath.Join(t.TempDir(), "creds")
	writeCreds(t, creds)
	h := startDFHubs(t, "--devicefarm-credentials-file", creds, "--cloud-max-devices", "1")
	e := h.e
	res := dfLease(t, h, "--platform", "ios", "--name", "Apple iPhone 15", "--ttl", "30m")
	dev := res.Lease.Device
	if !strings.HasPrefix(dev, "df-") || !strings.HasSuffix(dev, "@hub-b") || res.Device == nil || !res.Device.Attached {
		t.Fatalf("lease: %+v %+v", res.Lease, res.Device)
	}
	e.mustOK(h.client, "tapOn", "Button", "--device", dev)
	r := e.runEnv(h.client, "device", "list", "--location", "cloud", "--json")
	var listed daemon.DevicesResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &listed) != nil || len(listed.Devices) != 1 {
		t.Fatalf("listing: %s%s", r.stdout, r.stderr)
	}
	if d := listed.Devices[0]; d.ID != dev || d.Location != daemon.LocationCloud || d.Hub != "hub-b" || d.Provider != "devicefarm" || d.Available {
		t.Fatalf("listed: %+v", d)
	}
	r = e.runEnv(h.client, "hub", "status", "--json")
	if !strings.Contains(r.stdout, `"cloud"`) {
		t.Fatalf("hub status: %s", r.stdout)
	}
	// I8: the hub's one slot is taken.
	r = e.runEnv(h.client, "lease", "--location", "cloud", "--platform", "android")
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeBusy || !strings.Contains(r.stderr, "--cloud-max-devices 1") {
		t.Fatalf("over the cap: exit %d\n%s", r.code, r.stderr)
	}
	r = e.runEnv(h.client, "release", "--device", dev)
	if r.code != 0 || !strings.Contains(r.stdout, "device minutes") {
		t.Fatalf("release: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	sessions := h.fake.Sessions()
	if len(sessions) != 1 || sessions[0].Status != "COMPLETED" {
		t.Fatalf("sessions: %+v", sessions)
	}
	assertNoSecrets(t, e.cwd, h.fake)
}

func TestE2E_HubDeviceFarmLeaseEnd(t *testing.T) {
	// b shares a list, as hubs usually do: a released cloud device is then
	// not even in its pool.
	h := startDFHubs(t, "--share-device", "mock-b")
	e := h.e
	// The lease's window runs out: the device goes with it.
	res := dfLease(t, h, "--platform", "android", "--name", "Google Pixel 8", "--ttl", "3s", "--no-attach")
	ref := ""
	for _, s := range h.fake.Sessions() {
		ref = s.ARN
	}
	start := time.Now()
	for time.Since(start) < 15*time.Second {
		if s, _ := h.fake.Session(ref); s.Status == "COMPLETED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s, _ := h.fake.Session(ref); s.Status != "COMPLETED" {
		t.Fatalf("lease expiry left %s running", res.Lease.Device)
	}
	t.Logf("lease %s expired: its device's session %s after %s", res.Lease.ID, "COMPLETED", time.Since(start).Round(100*time.Millisecond))
	// The device's idle release ends its lease: lease.expired, and a
	// command under it then answers LEASE_EXPIRED.
	res = dfLease(t, h, "--platform", "android", "--name", "Google Pixel 8", "--idle-release", "2s", "--no-attach")
	// Wait until the hub has dropped the device entirely.
	var r result
	for start := time.Now(); time.Since(start) < 15*time.Second; time.Sleep(200 * time.Millisecond) {
		r = e.runEnv(h.client, "device", "list", "--location", "cloud", "--json")
		if !strings.Contains(r.stdout, "df-") {
			break
		}
	}
	if strings.Contains(r.stdout, "df-") {
		t.Fatalf("a released device is listed: %s", r.stdout)
	}
	r = e.runEnv(h.client, "tapOn", "Button", "--device", res.Lease.Device)
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeLeaseExpired || !strings.Contains(r.stderr, "`maestro-d lease --location cloud --platform ios|android`") {
		t.Fatalf("a command after the idle release: exit %d\n%s", r.code, r.stderr)
	}
	// The lease forgotten, the next command finds no device.
	if r := e.runEnv(h.client, "tapOn", "Button", "--device", res.Lease.Device); envelopeCode(t, r.stderr) != daemon.CodeDeviceNotFound {
		t.Fatalf("a command after the lease was forgotten: exit %d\n%s", r.code, r.stderr)
	}
	// The client forgets the lease it was told has ended.
	if data, _ := os.ReadFile(filepath.Join(envValue(h.client, "MAESTRO_D_HOME"), "leases.json")); strings.Contains(string(data), res.Lease.ID) {
		t.Fatalf("leases.json still holds the ended lease: %s", data)
	}
	t.Logf("idle release of %s ended lease %s: once the device was gone the next command answered LEASE_EXPIRED and the client forgot the lease", res.Lease.Device, res.Lease.ID)
}

// envValue returns key's value in an environment list.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func TestE2E_HubDeviceFarmRefusals(t *testing.T) {
	h := startDFHubs(t)
	e := h.e
	// Cloud-only lease flags without --location cloud are refused, not
	// ignored.
	for _, args := range [][]string{{"--idle-release", "5m"}, {"--appium-version", "3"}} {
		if r := e.runEnv(h.client, append([]string{"lease", "--platform", "mock"}, args...)...); r.code != 2 {
			t.Fatalf("lease %v without --location cloud: exit %d\n%s", args, r.code, r.stderr)
		}
	}
	// device create with a cloud spec on a hub: FORBIDDEN with the lease
	// hint, for a remote client and on the hub's own machine.
	for _, env := range [][]string{h.client, h.envB} {
		args := []string{"device", "create", "--provider", "devicefarm", "--platform", "android"}
		if strings.Contains(strings.Join(env, " "), "MAESTRO_D_HUB=") {
			args = append(args, "--target-hub="+h.b.HTTPAddr)
		} else {
			args = append(args, "--daemon", "dfhub-b")
		}
		r := e.runEnv(env, args...)
		if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeForbidden || !strings.Contains(r.stderr, "lease --location cloud") {
			t.Fatalf("create on a hub: exit %d\n%s", r.code, r.stderr)
		}
	}
	// A hub refuses to start with a credentials file others can read.
	loose := filepath.Join(t.TempDir(), "creds")
	writeCreds(t, loose)
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.runEnv(h.envB, "hub", "start", "--daemon", "dfhub-c", "--name", "hub-c", "--listen", "127.0.0.1:0", "--no-mdns", "--token", "x",
		"--peer-token", "y", "--devicefarm", "--devicefarm-credentials-file", loose)
	if r.code == 0 || !strings.Contains(r.stderr, "mode 0644") {
		t.Fatalf("a loose credentials file: exit %d\n%s", r.code, r.stderr)
	}
	// And the Device Farm flags need --devicefarm.
	r = e.runEnv(h.envB, "hub", "start", "--daemon", "dfhub-c", "--name", "hub-c", "--listen", "127.0.0.1:0", "--no-mdns", "--token", "x",
		"--peer-token", "y", "--cloud-max-devices", "3")
	if r.code == 0 || !strings.Contains(r.stderr, "need --devicefarm") {
		t.Fatalf("flags without --devicefarm: exit %d\n%s", r.code, r.stderr)
	}
	// A start check that fails stops the hub, redacted.
	h.fake.FailOp("GetAccountSettings", "AccessDeniedException", "User is not authorized to perform: devicefarm:GetAccountSettings")
	r = e.runEnv(h.envB, "hub", "start", "--daemon", "dfhub-c", "--name", "hub-c", "--listen", "127.0.0.1:0", "--no-mdns", "--token", "x",
		"--peer-token", "y", "--devicefarm")
	if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "GetAccountSettings") {
		t.Fatalf("a failing start check: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
}

func TestE2E_HubDeviceFarmInstall(t *testing.T) {
	h := startDFHubs(t)
	e := h.e
	res := dfLease(t, h, "--platform", "android", "--name", "Google Pixel 8")
	dev := res.Lease.Device
	// The app goes up under the lease through the mesh, and b installs it.
	e.mustOK(h.client, "install", writeFakeApp(t, e.cwd, "b.apk"), "--device", dev)
	var installed []string
	for _, s := range h.fake.Sessions() {
		installed = append(installed, s.Installed...)
	}
	if len(installed) != 1 {
		t.Fatalf("installed %v", installed)
	}
	e.mustOK(h.client, "tapOn", "Button", "--device", dev)
	if r := e.runEnv(h.client, "uninstall", "org.b", "--device", dev); r.code != 2 {
		t.Fatalf("uninstall: exit %d\n%s", r.code, r.stderr)
	}
	e.mustOK(h.client, "release", "--device", dev)
}

func TestE2E_HubDeviceFarmUtilities(t *testing.T) {
	h := startDFHubs(t)
	e := h.e
	for _, sub := range []string{"account", "apps", "sessions"} {
		r := e.mustOK(h.client, "devicefarm", sub, "--json")
		if !strings.Contains(r.stdout, `"hub": "hub-b"`) {
			t.Fatalf("%s through a: %s", sub, r.stdout)
		}
	}
	r := e.runEnv(h.client, "devicefarm", "upload", writeFakeApp(t, e.cwd, "a.apk"))
	if r.code == 0 || envelopeCode(t, r.stderr) != daemon.CodeForbidden || !strings.Contains(r.stderr, "leased cloud device") {
		t.Fatalf("a remote upload: exit %d\n%s", r.code, r.stderr)
	}
}
