package cli

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// Fetch the public projection as a browser would, through a separate listener
// and without either of the credentials used on the API and mesh.
func TestE2E_HubWeb(t *testing.T) {
	e := newE2E(t)
	a := newRestartHub(t, e, "hub-a", "mock-1")
	a.env = append(a.env, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2")
	b := newRestartHub(t, e, "hub-b", "mock-3")
	a.start("--web-port", "0")
	b.start("--web-port", "0", "--peer", a.info.HTTPAddr)
	_, port, err := net.SplitHostPort(a.info.WebAddr)
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", port)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}, Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS.Version != tls.VersionTLS13 {
		t.Fatal(resp.Status, resp.TLS.Version)
	}
	digest := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	fingerprint := hex.EncodeToString(digest[:])
	home := strings.TrimPrefix(a.env[0], daemon.EnvHome+"=")
	log, err := os.ReadFile(filepath.Join(home, "hub", "run", "daemon.log"))
	if err != nil || !strings.Contains(strings.ReplaceAll(strings.ToLower(string(log)), ":", ""), fingerprint) {
		t.Fatalf("fingerprint %s missing from hub log: %v\n%s", fingerprint, err, log)
	}
	t.Logf("GET https://%s/ → %s TLS 1.3; certificate SHA-256 %s matches hub log", addr, resp.Status, fingerprint)
	resp, err = client.Get("https://" + addr + "/v1/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal(resp.Status)
	}
	t.Logf("GET /v1/devices on web port → %s", resp.Status)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, "wss://"+addr+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	type lease struct{ Holder, Description, StartsAt, ExpiresAt string }
	type row struct {
		Name, Hub string
		Lease     *lease
		Upcoming  []lease
	}
	forbidden := []string{"mock-2", "restart-token", "restart-peer", `"mac"`, a.info.HTTPAddr, b.info.HTTPAddr}
	read := func(ctx context.Context) map[string]row {
		t.Helper()
		_, data, err := socket.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range forbidden {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatalf("public snapshot leaked %q: %s", secret, data)
			}
		}
		t.Logf("WS snapshot %s", data)
		var snap struct{ Devices []row }
		if err := json.Unmarshal(data, &snap); err != nil {
			t.Fatal(err)
		}
		rows := map[string]row{}
		for _, d := range snap.Devices {
			rows[d.Name] = d
		}
		return rows
	}
	rows := read(ctx)
	if len(rows) != 2 || rows["mock-1"].Hub != "hub-a" || rows["mock-3"].Hub != "hub-b" {
		t.Fatal(rows)
	}
	grant, err := a.client().AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1@hub-a", TTLSec: 30, Description: "local job"})
	if err != nil {
		t.Fatal(err)
	}
	forbidden = append(forbidden, strings.Split(grant.Lease.ID, "@")[0])
	started, err := time.Parse(time.RFC3339, grant.Lease.StartsAt)
	if err != nil {
		t.Fatal(err)
	}
	// Move beyond the API's one-second precision to distinguish the original
	// start from the renewal time in the public view.
	time.Sleep(time.Until(started.Add(time.Second)))
	renewed, err := a.client().RenewLease(ctx, grant.Lease.ID, 60)
	if err != nil || renewed.Lease.ExpiresAt == grant.Lease.ExpiresAt {
		t.Fatalf("renew: %+v, %v", renewed, err)
	}
	now := time.Now()
	for _, reservation := range []struct {
		hub                 *restartHub
		device, description string
		offset              time.Duration
	}{
		{a, "mock-1@hub-a", "later", 4 * time.Minute},
		{a, "mock-1@hub-a", "earlier", 2 * time.Minute},
		{b, "mock-3@hub-b", "peer reservation", 3 * time.Minute},
	} {
		res, err := reservation.hub.client().Reserve(ctx, daemon.ReservationRequest{Device: reservation.device, StartAt: now.Add(reservation.offset).UTC().Format(time.RFC3339), TTLSec: 30, Description: reservation.description})
		if err != nil {
			t.Fatal(err)
		}
		forbidden = append(forbidden, strings.Split(res.Lease.ID, "@")[0])
	}
	res, err := b.client().AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-3@hub-b", TTLSec: 60, Description: "peer job"})
	if err != nil {
		t.Fatal(err)
	}
	forbidden = append(forbidden, strings.Split(res.Lease.ID, "@")[0])
	t.Logf("Peer grant acknowledged at %s", time.Now().UTC().Format(time.RFC3339Nano))
	update, cancelUpdate := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancelUpdate()
	for {
		rows = read(update)
		peer, local := rows["mock-3"], rows["mock-1"]
		if peer.Lease == nil || local.Lease == nil || len(local.Upcoming) != 2 || len(peer.Upcoming) != 1 {
			continue
		}
		if peer.Lease.Holder != "another client" || peer.Lease.Description != "peer job" ||
			local.Lease.Holder != "another client" || local.Lease.Description != "local job" ||
			local.Lease.StartsAt != grant.Lease.StartsAt || local.Lease.ExpiresAt != renewed.Lease.ExpiresAt ||
			peer.Upcoming[0].Holder != "another client" || peer.Upcoming[0].Description != "peer reservation" ||
			local.Upcoming[0].Description != "earlier" || local.Upcoming[1].Description != "later" ||
			local.Upcoming[0].Holder != "another client" || local.Upcoming[1].Holder != "another client" ||
			local.Upcoming[0].StartsAt >= local.Upcoming[1].StartsAt {
			t.Fatal(rows)
		}
		return
	}
}
