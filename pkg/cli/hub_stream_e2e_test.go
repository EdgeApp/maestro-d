package cli

// hub_stream_e2e_test.go: a screen stream across two hubs (I7) and the
// viewer page and token on a hub's TCP listener (the hub half of I5).

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestE2E_HubStream(t *testing.T) {
	e := newE2E(t)
	homeA, homeB, homeC := filepath.Join(e.cwd, "home-a"), filepath.Join(e.cwd, "home-b"), filepath.Join(e.cwd, "home-c")
	envA := []string{daemon.EnvHome + "=" + homeA, "MAESTRO_D_MOCK_DEVICES=mock-9"}
	envB := []string{daemon.EnvHome + "=" + homeB, "MAESTRO_D_MOCK_DEVICES=mock-1"}
	t.Cleanup(func() {
		e.runEnv(envB, "hub", "stop", "--daemon", "ss-hub-b")
		e.runEnv(envA, "hub", "stop", "--daemon", "ss-hub-a")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "hs-tok", "--peer-token", "hs-peer", "--json"}
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
	b := started(e.runEnv(envB, append([]string{"hub", "start", "--daemon", "ss-hub-b", "--name", "hub-b", "--share-device", "mock-1"}, mesh...)...))
	a := started(e.runEnv(envA, append([]string{"hub", "start", "--daemon", "ss-hub-a", "--name", "hub-a", "--peer", b.HTTPAddr}, mesh...)...))
	client := []string{daemon.EnvHome + "=" + homeC, "MAESTRO_D_HUB=" + a.HTTPAddr, "MAESTRO_D_TOKEN=hs-tok"}
	out := filepath.Join(e.cwd, "hub-stream.mp4")

	// Without a lease the owner refuses the stream: LEASE_REQUIRED.
	r := e.runEnv(client, "stream", "--device", "mock-1@hub-b", "--duration", "1", "-o", out)
	if r.code != 5 || envelopeCode(t, r.stderr) != daemon.CodeLeaseRequired {
		t.Fatalf("stream without a lease: exit %d\n%s", r.code, r.stderr)
	}

	// A lease through hub a, then the stream through a from b's device.
	r = e.runEnv(client, "lease", "--device", "mock-1@hub-b", "--json")
	var lr daemon.LeaseResult
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &lr) != nil || lr.Lease.Device != "mock-1@hub-b" {
		t.Fatalf("lease: %s%s", r.stdout, r.stderr)
	}
	r = e.runEnv(client, "stream", "--target-hub="+a.HTTPAddr, "--device", "mock-1@hub-b", "--duration", "3", "-o", out)
	if r.code != 0 || !strings.Contains(r.stderr, "stream: avc3.") {
		t.Fatalf("stream through the mesh: exit %d\n%s", r.code, r.stderr)
	}
	videoOf(t, out, 30)
	data := readFile(t, filepath.Join(homeA, "run", "ss-hub-a", "daemon.log"))
	if !strings.Contains(data, "bulk link to hub-b for GET /v1/devices/mock-1@hub-b/stream") {
		t.Fatalf("hub a's log has no bulk link for the stream:\n%s", data)
	}

	// The first bytes arrive within a second: the owner flushes through
	// the lease gate and the bulk link, and hub a passes them on.
	c := daemon.NewHTTPClient(a.HTTPAddr, "hs-tok", nil)
	c.SetLease("mock-1@hub-b", lr.Lease.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var firstAt atomic.Int64
	start := time.Now()
	_, err := c.Stream(ctx, "mock-1@hub-b", daemon.StreamOptions{Duration: 2 * time.Second}, writerFunc(func(p []byte) (int, error) {
		firstAt.CompareAndSwap(0, int64(time.Since(start)))
		return len(p), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(firstAt.Load()); d == 0 || d > time.Second {
		t.Fatalf("first bytes after %v", d)
	}

	// I5 on a hub: the viewer without a token, a stream without one 401.
	resp, body := httpGet(t, "http://"+a.HTTPAddr+"/v1/viewer", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "<video") {
		t.Fatalf("viewer on a hub: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ = httpGet(t, "http://"+a.HTTPAddr+"/v1/devices/mock-1@hub-b/stream", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream without a token on a hub: %d", resp.StatusCode)
	}
	e.runEnv(client, "release", "--device", "mock-1@hub-b")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
