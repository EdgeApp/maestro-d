package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestE2E_HubLiveShare(t *testing.T) {
	e := newE2E(t)
	writeSharedMocks(t, e.home, "mock-1")
	path := filepath.Join(e.home, "config.json")
	mock := []string{"MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	args := []string{"maestro-d-hub", "start", "--name", "live", "--config", path, "--listen", "127.0.0.1:0", "--no-mdns", "--token", "t", "--peer-token", "p", "--json"}
	r := e.runEnv(mock, args...)
	var start struct{ Daemon daemon.Info }
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &start) != nil || start.Daemon.HTTPAddr == "" {
		t.Fatalf("hub start: %s%s", r.stdout, r.stderr)
	}
	t.Cleanup(func() { e.runEnv(mock, "maestro-d-hub", "stop") })
	t.Logf("$ %s\nhub pid %d at %s", strings.Join(args, " "), start.Daemon.PID, start.Daemon.HTTPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr := "http://" + start.Daemon.HTTPAddr
	holder := daemon.NewHTTPClient(addr, "t", nil)
	public := daemon.NewHTTPClient(addr, "t", nil)
	const id = "mock-1@live"
	grant, err := holder.AcquireLease(ctx, daemon.LeaseRequest{Device: id, Holder: "live-edit", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	holder.SetLease(id, grant.Lease.ID)
	t.Logf("POST /v1/leases -> %s for %s until %s", grant.Lease.ID, id, grant.Lease.ExpiresAt)
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("write config.json %s", data)
	}
	list := func(want int) {
		t.Helper()
		for {
			devs, err := public.Devices(ctx, "mock")
			if err != nil {
				t.Fatal(err)
			}
			good := len(devs) == want
			seen := false
			for _, d := range devs {
				if d.ID == "mock-2@live" && d.Description == "New lab phone" {
					seen = true
				}
			}
			if good && seen {
				t.Logf("GET /v1/devices -> %d public rows, mock-2@live description %q", len(devs), "New lab phone")
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	checkLease := func() {
		t.Helper()
		current, err := holder.Lease(ctx, grant.Lease.ID)
		if err != nil || current.Lease != grant.Lease {
			t.Fatalf("changed lease: %+v %v", current, err)
		}
		if _, err := holder.Command(ctx, id, "tapOn", "x", true); err != nil {
			t.Fatal(err)
		}
		t.Logf("GET /v1/leases/%s -> unchanged; POST /v1/devices/%s/commands/tapOn -> success", grant.Lease.ID, id)
	}
	write(`{"hubDevices":[{"id":"mock-1"},{"id":"mock-2","description":"New lab phone"}]}`)
	list(2)
	checkLease()
	write(`{"hubDevices":[{"id":"mock-2","description":"New lab phone"}]}`)
	list(1)
	checkLease()
	write("{")
	logpath := filepath.Join(start.Daemon.RunDir, "daemon.log")
	for {
		data, err := os.ReadFile(logpath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "still sharing [mock-2]") {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("missing invalid-config warning: %s", data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	list(1)
	time.Sleep(1100 * time.Millisecond)
	list(1)
	data, err := os.ReadFile(logpath)
	if err != nil || strings.Count(string(data), "still sharing [mock-2]") != 1 {
		t.Fatalf("invalid config warnings: %s %v", data, err)
	}
	st, err := public.Status(ctx)
	if err != nil || st.Daemon.PID != start.Daemon.PID {
		t.Fatalf("pid changed: %+v %v", st, err)
	}
	t.Logf("invalid edit logged once; GET /v1/status -> original pid %d", st.Daemon.PID)
}
