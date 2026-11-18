package cli

// hub_e2e_test.go: two hub binaries with mock devices linked by --peer:
// the fan-out device list, a lease and a command routed to the owner, a
// whole flow bundle run there, HUB_NAME_TAKEN for a hub that takes a
// linked name, and PEER_UNAVAILABLE once the owner is killed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// copyTree copies the files of src (one level of subdirectories) to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestE2E_TwoHubs(t *testing.T) {
	e := newE2E(t)
	homeA, homeB, homeC := filepath.Join(e.cwd, "home-a"), filepath.Join(e.cwd, "home-b"), filepath.Join(e.cwd, "home-c")
	envA := []string{daemon.EnvHome + "=" + homeA, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	envB := []string{daemon.EnvHome + "=" + homeB, "MAESTRO_D_MOCK_DEVICES=mock-3"}
	// Each hub has its own daemon name: a long test home puts the unix
	// socket in the temp dir, named by the daemon.
	t.Cleanup(func() {
		e.runEnv(envB, "hub", "stop", "--daemon", "two-b")
		e.runEnv(envA, "hub", "stop", "--daemon", "two-a")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "two-tok", "--peer-token", "two-peer", "--json"}
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
	a := started(e.runEnv(envA, append([]string{"hub", "start", "--daemon", "two-a", "--name", "hub-a", "--share-device", "mock-1", "--share-device", "mock-2"}, mesh...)...))
	b := started(e.runEnv(envB, append([]string{"hub", "start", "--daemon", "two-b", "--name", "hub-b", "--peer", a.HTTPAddr, "--share-device", "mock-3",
		"--advertise", "hub-b.example:7433", "--lease-max", "mock-3=90s"}, mesh...)...))
	for _, arg := range b.StartArgs {
		if strings.Contains(arg, "two-peer") || strings.Contains(arg, "two-tok") {
			t.Fatalf("a token reached the hub's command line: %v", b.StartArgs)
		}
	}
	client := []string{daemon.EnvHome + "=" + homeC, "MAESTRO_D_HUB=" + b.HTTPAddr, "MAESTRO_D_TOKEN=two-tok"}

	// hub status through b names a, linked.
	r := e.runEnv(client, "hub", "status", "--json")
	var st daemon.HubStatusResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &st) != nil || len(st.Hub.Peers) != 1 || st.Hub.Peers[0].State != "linked" || st.Hub.Peers[0].Name != "hub-a" {
		t.Fatalf("hub status: %s%s", r.stdout, r.stderr)
	}
	if r = e.runEnv(client, "hub", "status"); !strings.Contains(r.stdout, "hub-a") || !strings.Contains(r.stdout, "linked") {
		t.Fatalf("hub status text: %s", r.stdout)
	}
	if r = e.runEnv(envA, "hub", "status", "--daemon", "two-a"); r.code != 0 || !strings.Contains(r.stdout, "hub hub-a") || !strings.Contains(r.stdout, "inbound") {
		t.Fatalf("hub status over the unix socket: %s%s", r.stdout, r.stderr)
	}

	// The fan-out list shows both hubs' devices, qualified.
	r = e.runEnv(client, "device", "list", "--platform", "mock", "--json")
	for _, want := range []string{`"mock-1@hub-a"`, `"mock-2@hub-a"`, `"mock-3@hub-b"`} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("device list lacks %s:\n%s", want, r.stdout)
		}
	}
	// Every entry names its owner's machine and addresses, and its
	// maximum window, through the mesh.
	var listed daemon.DevicesResult
	if err := json.Unmarshal([]byte(r.stdout), &listed); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	for _, d := range listed.Devices {
		if d.Machine != host || len(d.Addresses) == 0 {
			t.Fatalf("%s: machine %q addresses %v", d.ID, d.Machine, d.Addresses)
		}
		if want := map[string]int{"hub-a": 28800, "hub-b": 90}[d.Hub]; d.MaxSec != want || (d.Hub == "hub-b") != (d.Addresses[0] == "hub-b.example:7433") {
			t.Fatalf("%s: maxSec %d addresses %v", d.ID, d.MaxSec, d.Addresses)
		}
	}
	// No lease, no routed command: the owner answers LEASE_REQUIRED.
	if r = e.runEnv(client, "tapOn", "Login", "--device", "mock-1@hub-a"); r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("routed tapOn without a lease: exit %d\n%s", r.code, r.stderr)
	}
	// Over the owner's maximum, through b: USAGE.
	if r = e.runEnv(client, "lease", "--device", "mock-3@hub-b", "--ttl", "2m"); r.code != 2 || !strings.Contains(r.stderr, "1m30s") {
		t.Fatalf("lease over the maximum: exit %d\n%s", r.code, r.stderr)
	}
	// A lease on a's device through b, then a routed command.
	r = e.runEnv(client, "lease", "--device", "mock-1@hub-a", "--json")
	var lr daemon.LeaseResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &lr) != nil || !strings.HasSuffix(lr.Lease.ID, "@hub-a") || lr.Lease.Device != "mock-1@hub-a" {
		t.Fatalf("lease: %s%s", r.stdout, r.stderr)
	}
	if r = e.runEnv(client, "tapOn", "Login", "--device", "mock-1@hub-a"); r.code != 0 {
		t.Fatalf("routed tapOn: exit %d\n%s", r.code, r.stderr)
	}
	// An unqualified id resolves to the one hub that has it.
	if r = e.runEnv(client, "tapOn", "Again", "--device", "mock-1"); r.code != 0 || strings.Contains(r.stderr, "Leased") {
		t.Fatalf("unqualified id reuses the lease: exit %d\n%s", r.code, r.stderr)
	}
	// A whole flow bundle runs on the owner.
	flowDir := filepath.Join(e.cwd, "flow")
	copyTree(t, filepath.Join("..", "..", "e2e", "testdata", "hub"), flowDir)
	if r = e.runEnv(client, "run", filepath.Join(flowDir, "main.yaml"), "--device", "mock-1@hub-a"); r.code != 0 || !strings.Contains(r.stdout, "runScript") {
		t.Fatalf("routed run: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	data, _ := os.ReadFile(filepath.Join(homeA, "run", "two-a", "daemon.log"))
	if !strings.Contains(string(data), "forwarded by hub-b: POST /v1/devices/mock-1@hub-a/flows") {
		t.Fatalf("the owner's log has no forwarded flow:\n%s", data)
	}

	// A hub named like a linked one is refused: exit 5, HUB_NAME_TAKEN.
	envD := []string{daemon.EnvHome + "=" + filepath.Join(e.cwd, "home-d")}
	r = e.runEnv(envD, append([]string{"hub", "start", "--daemon", "two-d", "--name", "hub-b", "--peer", b.HTTPAddr}, mesh...)...)
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeHubNameTaken {
		t.Fatalf("taken name: exit %d\n%s", r.code, r.stderr)
	}

	// Kill a: requests for its devices fail PEER_UNAVAILABLE (exit 3) and
	// the list names it unreachable.
	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		r = e.runEnv(client, "tapOn", "Gone", "--device", "mock-1@hub-a")
		if r.code == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if r.code != 3 || envelopeCode(t, r.stderr) != daemon.CodePeerUnavailable {
		t.Fatalf("after kill: exit %d\n%s", r.code, r.stderr)
	}
	r = e.runEnv(client, "device", "list", "--platform", "mock")
	if !strings.Contains(r.stderr, "unreachable: hub hub-a") || strings.Contains(r.stdout, "@hub-a") {
		t.Fatalf("list after kill:\n%s%s", r.stdout, r.stderr)
	}
}
