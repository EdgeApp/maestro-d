package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func hubConfigSocket(t *testing.T, socket string) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

// configRequest checks the transport status and envelope of an actual hub
// response. Credentials are deliberately omitted from the command log.
func configRequest(t *testing.T, client *http.Client, base, method, route, body, token string, want int, code daemon.Code) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var env daemon.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("%d: %s", resp.StatusCode, data)
	}
	t.Logf("$ %s %s%s %s\nHTTP %d %s", method, base, route, body, resp.StatusCode, bytes.TrimSpace(data))
	if resp.StatusCode != want || (code == "" && !env.OK) || (code != "" && (env.Error == nil || env.Error.Code != code)) {
		t.Fatalf("want HTTP %d %s, got %d %s", want, code, resp.StatusCode, data)
	}
	return data
}

func TestE2E_HubConfigForbidden(t *testing.T) {
	e := newE2E(t)
	a, b, envA, envB := startTwoHubs(t, e, "forbidden")
	configBytes := func(env []string) []byte {
		t.Helper()
		path := filepath.Join(envValue(env, daemon.EnvHome), "config.json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("$ shasum -a 256 %s\n%x", path, sha256.Sum256(b))
		return b
	}
	beforeA, beforeB := configBytes(envA), configBytes(envB)
	base := "http://" + a.HTTPAddr
	for _, token := range []string{"res-tok", "res-peer", ""} {
		configRequest(t, http.DefaultClient, base, "PUT", "/v1/config/devices/mock-1", `{"description":"changed"}`, token, 403, daemon.CodeForbidden)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		route := "/v1/config/devices"
		body := ""
		if method != "GET" {
			route += "/mock-3@hub-b"
		}
		if method == "PUT" {
			body = "{}"
		}
		configRequest(t, http.DefaultClient, base, method, route, body, "res-tok", 403, daemon.CodeForbidden)
	}
	if !bytes.Equal(beforeA, configBytes(envA)) || !bytes.Equal(beforeB, configBytes(envB)) {
		t.Fatal("a remote request changed the device config")
	}
	log, err := os.ReadFile(filepath.Join(b.RunDir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "forwarded by") && strings.Contains(line, "/v1/config/") {
			t.Fatal(line)
		}
	}
	t.Log("no forwarded device config request in hub-b log")
}

func TestE2E_HubConfigLocal(t *testing.T) {
	e := newE2E(t)
	configHome := filepath.Join(e.cwd, "selected")
	writeSharedMocks(t, configHome, "mock-1")
	path := filepath.Join(configHome, "config.json")
	a, _, _, _ := startMockHubPair(t, e, "local-edit", []string{"mock-1", "mock-2", "mock-3"}, nil, "--config", path)
	local := hubConfigSocket(t, a.SocketPath)
	root := "/v1/config/devices"
	call := func(method, suffix, body string, status int, code daemon.Code) []byte {
		t.Helper()
		return configRequest(t, local, "http://unix", method, root+suffix, body, "", status, code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	holder := daemon.NewHTTPClient(a.HTTPAddr, "res-tok", nil)
	peer := daemon.NewHTTPClient(a.HTTPAddr, "res-tok", nil)
	id := "mock-1@hub-a"
	grant, err := holder.AcquireLease(ctx, daemon.LeaseRequest{Device: id, Holder: "config-test", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	holder.SetLease(id, grant.Lease.ID)
	call("PUT", "/mock-2", `{"description":"rack 2"}`, 201, "")
	call("PUT", "/mock-1", `{"description":"bench"}`, 200, "")
	type entry struct{ ID, Description string }
	var cfg struct {
		Devices []entry `json:"hubDevices"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := []entry{{ID: "mock-1", Description: "bench"}, {ID: "mock-2", Description: "rack 2"}}
	if err := json.Unmarshal(raw, &cfg); err != nil || !slices.Equal(cfg.Devices, wantEntries) {
		t.Fatalf("unexpected on-disk config: %s (%v)", raw, err)
	}
	t.Logf("$ cat %s\n%s", path, raw)
	list := func() map[string]string {
		t.Helper()
		devices, err := peer.Devices(ctx, "mock")
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]string{}
		for _, d := range devices {
			result[d.ID] = d.Description
		}
		return result
	}
	listed := list()
	if len(listed) != 2 || listed[id] != "bench" || listed["mock-2@hub-a"] != "rack 2" {
		t.Fatalf("remote list after edit: %v", listed)
	}
	if now, err := holder.Lease(ctx, grant.Lease.ID); err != nil || now.Lease.ID != grant.Lease.ID || now.Lease.ExpiresAt != grant.Lease.ExpiresAt {
		t.Fatalf("lease changed after edit: %+v %v", now, err)
	}
	if _, err := holder.Command(ctx, id, "tapOn", "Button", false); err != nil {
		t.Fatalf("live command after edit: %v", err)
	}
	denied := call("DELETE", "/mock-1", "", 409, daemon.CodeDeviceInUse)
	if !bytes.Contains(denied, []byte("config-test")) {
		t.Fatalf("missing lease holder: %s", denied)
	}
	call("DELETE", "/mock-1?endLeases=true", "", 200, "")
	if listed := list(); len(listed) != 1 || listed["mock-2@hub-a"] != "rack 2" {
		t.Fatalf("remote list after removal: %v", listed)
	}
	check := func(client *daemon.Client, device, caller string, want daemon.Code) {
		t.Helper()
		_, err := client.Command(ctx, device, "tapOn", "Button", false)
		var de *daemon.Error
		if !errors.As(err, &de) || de.Code != want {
			t.Fatalf("after removal: %s got %v, want %s", device, err, want)
		}
		t.Logf("$ POST /v1/devices/%s/commands/tapOn (%s) -> %s", device, caller, de.Code)
	}
	check(holder, id, "former holder", daemon.CodeLeaseExpired)
	check(peer, id, "unrelated client", daemon.CodeDeviceNotFound)
	holder.SetLease("mock-3@hub-a", grant.Lease.ID)
	check(holder, "mock-3@hub-a", "former credential for a different device", daemon.CodeDeviceNotFound)
}
