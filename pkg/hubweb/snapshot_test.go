package hubweb

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
)

func TestWebDeviceTypes(t *testing.T) {
	for _, tc := range []struct {
		d    daemon.DeviceInfo
		want string
	}{
		{daemon.DeviceInfo{Platform: "android", Model: "Pixel 8", OSVersion: "16"}, "Pixel 8 · Android 16"},
		{daemon.DeviceInfo{Platform: "ios", Model: "iPhone14,2", OSVersion: "26.3"}, "iPhone 13 Pro · iOS 26.3"},
		{daemon.DeviceInfo{Platform: "ios", Model: "iPad16,3", OSVersion: "26.3"}, "iPad Pro 11-inch (M4) · iPadOS 26.3"},
		{daemon.DeviceInfo{Platform: "ios", Kind: "tv", Model: "AppleTV14,1", OSVersion: "26"}, "Apple TV 4K (3rd generation) · tvOS 26"},
		{daemon.DeviceInfo{Platform: "ios", Kind: "simulator", Model: "iPhone17,1", Name: "Shared sim", OSVersion: "26"}, "iPhone 16 Pro · iOS 26"},
		{daemon.DeviceInfo{Platform: "android", Kind: "emulator", DeviceType: "Pixel 8", Model: "sdk_arm64", Name: "Custom AVD", OSVersion: "16"}, "Pixel 8 · Android 16"},
		{daemon.DeviceInfo{Platform: "ios", Model: "iPhone99,7", OSVersion: "30"}, "iPhone99,7 · iOS 30"},
		{daemon.DeviceInfo{Platform: "web", Name: "Chromium", Model: "Chromium", OSVersion: "132"}, "Chromium 132"},
		{daemon.DeviceInfo{Platform: "ios", Model: "iPad99,7", Name: "test", OSVersion: "30"}, "iPad99,7 · iPadOS 30"},
	} {
		if got := deviceType(tc.d); got != tc.want {
			t.Errorf("%+v → %q; want %q", tc.d, got, tc.want)
		}
	}
}

func TestPublicStateOfExternallyOwnedDevice(t *testing.T) {
	d := daemon.DeviceInfo{ID: "mock", Ready: true, Owner: "private-local-daemon"}
	r := publicRow(d)
	data, err := json.Marshal(r)
	if err != nil || r.State != "busy" || r.Lease != nil || strings.Contains(string(data), d.Owner) {
		t.Fatalf("externally owned device: %s %v", data, err)
	}
	d.Owner = ""
	if got := publicRow(d); got.State != "ready" {
		t.Fatalf("released device state: %s", got.State)
	}
}

func TestSnapshotPublicSchemaAndLegacyPeer(t *testing.T) {
	const start = "2026-11-25T12:01:00Z"
	const end = "2026-11-25T13:11:00Z"
	const soon = "2026-11-25T15:01:00+01:00"
	const later = "2026-11-25T14:11:00Z"
	private := "sensitive-value"
	local := []daemon.DeviceInfo{{ID: "serial@a", Hub: "a", Machine: "Machine A", Name: "Bench", Kind: "device", Ready: true, Platform: "ios",
		ReportDir: private, Driver: private, TrafficShaping: true, ProviderRef: private, DashboardURL: private, Addresses: []string{private},
		Lease:        &daemon.DeviceLease{Holder: "local 192.0.2.5:4831", Description: "local job", StartsAt: start, ExpiresAt: end},
		Reservations: []daemon.DeviceLease{{Holder: "unix socket", StartsAt: soon, ExpiresAt: end}}},
		{ID: "mac@a", LocalID: "mac", Hub: "a", Kind: "computer"},
	}
	// Decode an older peer's wire data: an active row has no start, while
	// both of its reservations retain their own start and end.
	var peer daemon.DeviceInfo
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"id":"serial@b","hub":"b","machine":"Machine B","name":"Bench","lease":{"holder":"named holder","expiresAt":%q},"reservations":[{"holder":"remote 192.0.2.6:4832","startsAt":%q,"expiresAt":"2026-11-25T14:16:00Z"},{"holder":"","startsAt":%q,"expiresAt":"2026-11-25T14:05:00Z"}]}`, end, later, soon)), &peer); err != nil {
		t.Fatal(err)
	}
	p := inventory{machines: map[string]string{"empty": "Empty machine", "b": "", "down": "Down machine"}, peers: daemon.DevicesResult{Devices: []daemon.DeviceInfo{peer}, Unreachable: []daemon.UnreachablePeer{{Hub: "down", Error: private}}}}
	got := project("a", "Machine A", local, p)
	if !slices.Equal(got.Hubs, []computer{{"a", "Machine A", "self"}, {"b", "Machine B", "linked"}, {"down", "Down machine", "down"}, {"empty", "Empty machine", "linked"}}) {
		t.Fatal(got.Hubs)
	}
	if len(got.Devices) != 2 {
		t.Fatal(got.Devices)
	}
	a, b := got.Devices[0], got.Devices[1]
	if a.Key == b.Key || a.Key != fmt.Sprintf("%x", sha256.Sum256([]byte("serial@a"))) || a.State != "busy" || b.State != "offline" {
		t.Fatal(a, b)
	}
	if a.Lease.StartsAt != start || a.Lease.ExpiresAt != end || b.Lease.Holder != "named holder" || b.Lease.StartsAt != "" || b.Lease.ExpiresAt != end {
		t.Fatal(a.Lease, b.Lease)
	}
	if b.Upcoming[0].StartsAt != soon || b.Upcoming[0].ExpiresAt != "2026-11-25T14:05:00Z" || b.Upcoming[1].StartsAt != later || b.Upcoming[1].ExpiresAt != "2026-11-25T14:16:00Z" {
		t.Fatal(b.Upcoming)
	}
	for _, lease := range []window{*a.Lease, a.Upcoming[0], b.Upcoming[0], b.Upcoming[1]} {
		if lease.Holder != "another client" {
			t.Fatal(lease)
		}
	}
	got.GeneratedAt = start
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{private, "serial@", "192.0.2.", "4831", "4832", "unix socket"} {
		if strings.Contains(string(data), secret) {
			t.Fatal(string(data))
		}
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	keys := func(v any, want ...string) {
		t.Helper()
		actual := slices.Sorted(maps.Keys(v.(map[string]any)))
		slices.Sort(want)
		if !slices.Equal(actual, want) {
			t.Errorf("keys %v; want %v", actual, want)
		}
	}
	keys(wire, "hub", "hubs", "devices", "generatedAt")
	for _, h := range wire["hubs"].([]any) {
		keys(h, "name", "machine", "state")
	}
	for i, v := range wire["devices"].([]any) {
		keys(v, "key", "name", "type", "platform", "kind", "computer", "hub", "state", "lease", "upcoming")
		d := v.(map[string]any)
		if i == 0 {
			keys(d["lease"], "holder", "description", "startsAt", "expiresAt")
		} else {
			keys(d["lease"], "holder", "description", "expiresAt")
		}
		for _, item := range d["upcoming"].([]any) {
			keys(item, "holder", "description", "startsAt", "expiresAt")
		}
	}
}

func TestSnapshotAppliesLiveSharingAndLease(t *testing.T) {
	home := t.TempDir()
	t.Setenv(daemon.EnvHome, home)
	cfg := filepath.Join(home, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"hubDevices":[{"id":"shared"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows := []daemon.DeviceInfo{
		{ID: "private", Name: "private phone", Platform: "mock", Ready: true},
		{ID: "shared", Name: "shared phone", Platform: "mock", Ready: true},
		{ID: "mac", Name: "private computer", Kind: "computer", Platform: "macos", Ready: true},
	}
	mesh := hub.New(hub.Config{Name: "bench", Token: "private-token", PeerToken: "private-peer", NoMDNS: true})
	policy := &hubd.Server{}
	srv := daemon.New(daemon.Config{Hub: policy, HubProcess: true, HubName: "bench", DeviceConfig: cfg, Token: "private-token", Mesh: mesh}, daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { return slices.Clone(rows) }})
	if err := policy.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown("test complete"); <-srv.Done() })
	api := httptest.NewServer(srv.TCPHandler())
	defer api.Close()
	c := daemon.NewHTTPClient(api.URL, "private-token", nil)
	s := meshSource(policy, mesh)
	pool := s.scan(t.Context())
	get := func() row {
		t.Helper()
		snap := s.project(pool)
		if len(snap.Devices) != 1 || snap.Devices[0].Name != "shared phone" {
			t.Fatal(snap)
		}
		return snap.Devices[0]
	}
	if r := get(); r.Lease != nil || r.State != "ready" {
		t.Fatal(r)
	}
	grant, err := c.AcquireLease(t.Context(), daemon.LeaseRequest{Device: "shared@bench", TTLSec: 45, Description: "active"})
	if err != nil {
		t.Fatal(err)
	}
	renew, err := c.RenewLease(t.Context(), grant.Lease.ID, 90)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339)
	res, err := c.Reserve(t.Context(), daemon.ReservationRequest{Device: "shared@bench", StartAt: start, TTLSec: 30, Description: "later"})
	if err != nil {
		t.Fatal(err)
	}
	r := get()
	if r.Lease == nil || r.Lease.Holder != "another client" || r.Lease.StartsAt != grant.Lease.StartsAt || r.Lease.ExpiresAt != renew.Lease.ExpiresAt {
		t.Fatal(r)
	}
	if len(r.Upcoming) != 1 || r.Upcoming[0].Holder != "another client" || r.Upcoming[0].StartsAt != start || r.Upcoming[0].ExpiresAt != res.Lease.ExpiresAt {
		t.Fatal(r)
	}
	body, err := json.Marshal(s.project(pool))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private", grant.Lease.ID, res.Lease.ID, "127.0.0.1"} {
		if strings.Contains(string(body), secret) {
			t.Fatal(string(body))
		}
	}
}
