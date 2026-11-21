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
	h.a = started(e.runEnv(h.envA, append([]string{"hub", "start", "--daemon", "dfhub-a", "--name", "hub-a"}, mesh...)...))
	h.b = started(e.runEnv(h.envB, append(append([]string{"hub", "start", "--daemon", "dfhub-b", "--name", "hub-b", "--peer", h.a.HTTPAddr, "--devicefarm"}, extraB...), mesh...)...))
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
