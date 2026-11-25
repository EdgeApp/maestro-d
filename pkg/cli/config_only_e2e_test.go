package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestE2E_ConfigOnly(t *testing.T) {
	e := newE2E(t)
	e.drop = append(e.drop, "MAESTRO_D_CONFIG", "MAESTRO_D_NETWORK_CONFIG")
	mock := []string{"MAESTRO_D_MOCK_DEVICES=mock-1"}
	for _, tc := range []struct {
		args []string
		hint string
	}{
		{[]string{"maestro-d-hub", "start", "--share-device", "mock-1"}, "mock-1"},
		{[]string{"serve", "--network-config", "x.json"}, "<id>"},
	} {
		r := e.runEnv(mock, tc.args...)
		if r.code != 2 || envelopeCode(t, r.stderr) != daemon.CodeUsage || !strings.Contains(r.stderr, "maestro-d-hub devices add "+tc.hint) {
			t.Fatalf("migration error: %s", r.stderr)
		}
	}
	sr := newShapeRecorder(t)
	legacy := fmt.Sprintf(`{"devices":{"mock-1":{"setTrafficShape":{"method":"POST","url":%q},"resetTrafficShape":{"method":"DELETE","url":%q}}}}`, sr.URL, sr.URL)
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.home, "network.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.runEnv(mock, "maestro-d-hub", "start", "--name", "config-only", "--listen", "127.0.0.1:0", "--token", "t", "--peer-token", "p", "--no-mdns", "--json")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	t.Cleanup(func() { e.runEnv(mock, "maestro-d-hub", "stop") })
	var started struct{ Daemon daemon.Info }
	if err := json.Unmarshal([]byte(r.stdout), &started); err != nil || started.Daemon.SocketPath == "" {
		t.Fatalf("hub endpoint: %s %v", r.stdout, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := daemon.NewUnixClient(started.Daemon.SocketPath)
	devs, err := client.Devices(ctx, "mock")
	if err != nil || len(devs) != 1 || devs[0].TrafficShaping {
		t.Fatalf("local devices: %+v %v", devs, err)
	}
	id := devs[0].ID
	granted, err := client.AcquireLease(ctx, daemon.LeaseRequest{Device: id, Holder: "config-only"})
	if err != nil || granted.Lease.ID == "" {
		t.Fatalf("lease %s: %+v %v", id, granted, err)
	}
	client.SetLease(id, granted.Lease.ID)
	if _, err := client.Attach(ctx, id, daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("attach %s: %v", id, err)
	}
	_, err = client.Command(ctx, id, "setTrafficShape", map[string]any{"lossPct": 100}, true)
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeUnsupported {
		t.Fatalf("setTrafficShape: %v", err)
	}
	t.Logf("Unix socket: listed, leased and attached %s; setTrafficShape returned %s", id, de.Code)
	if got := sr.take(t); len(got) != 0 {
		t.Fatalf("legacy fixture received %v", got)
	}
	log, err := os.ReadFile(filepath.Join(e.home, "hub", "run", "daemon.log"))
	const warning = "network.json is no longer read (4.0); move its devices into config.json (docs/daemon/config.md)"
	if err != nil || strings.Count(string(log), warning) != 1 {
		t.Fatalf("startup log: %s %v", log, err)
	}
	t.Log(warning)
}
