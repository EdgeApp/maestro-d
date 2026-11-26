package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// G5 reads the file immediately after each acknowledged mutation.
func TestE2E_HubLeaseFile(t *testing.T) {
	e := newE2E(t)
	start := func(home string) daemon.Info {
		t.Helper()
		writeSharedMocks(t, home, "mock-1", "mock-2")
		env := []string{daemon.EnvHome + "=" + home, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
		r := e.runEnv(env, "maestro-d-hub", "start", "--name", "disk", "--no-mdns", "--listen", "127.0.0.1:0", "--token", "disk-token", "--peer-token", "disk-peer", "--json")
		var out struct{ Daemon daemon.Info }
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("start: %+v", r)
		}
		t.Cleanup(func() { e.runEnv(env, "maestro-d-hub", "stop") })
		return out.Daemon
	}
	type state struct {
		Version            int
		Leases             []daemon.Lease
		AcquisitionReplays []daemon.AcquisitionReplay
	}
	read := func(home string) state {
		t.Helper()
		file := filepath.Join(home, "hub", "leases.json")
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("$ cat %s\n%s", file, data)
		st, err := os.Stat(file)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("lease state permissions: %v %v", st, err)
		}
		t.Logf("$ stat -f %%Sp %s\n%s", file, st.Mode())
		var s state
		if err := json.Unmarshal(data, &s); err != nil || s.Version != 2 {
			t.Fatalf("decode lease state: version %d error %v", s.Version, err)
		}
		return s
	}
	home := filepath.Join(e.cwd, "disk-hub")
	info := start(home)
	run := func(args ...string) daemon.Lease {
		t.Helper()
		r := e.runEnv([]string{"MAESTRO_D_TOKEN=disk-token"}, append(args, "--target-hub="+info.HTTPAddr, "--json")...)
		var res daemon.LeaseResult
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil || !res.OK {
			t.Fatalf("command: %+v", r)
		}
		return res.Lease
	}
	active := run("lease", "--device", "mock-1", "--description", "e2e run", "--ttl", "600", "--no-attach")
	s := read(home)
	localID := strings.TrimSuffix(active.ID, "@disk")
	if len(s.Leases) != 1 || s.Leases[0].ID != localID || s.Leases[0].State != "active" || s.Leases[0].Description != "e2e run" || s.Leases[0].ExpiresAt != active.ExpiresAt {
		t.Fatalf("grant: %+v", s)
	}
	var original daemon.AcquisitionReplay
	for _, rp := range s.AcquisitionReplays {
		if rp.Device == "mock-1" && rp.LeaseID == localID {
			original = rp
		}
	}
	if original.RequestID == "" {
		t.Fatal("grant has no durable acquisition replay")
	}
	scheduled := run("reserve", "--device", "mock-2", "--start-at", "5m", "--ttl", "240", "--description", "nightly QA")
	s = read(home)
	scheduledID := strings.TrimSuffix(scheduled.ID, "@disk")
	find := func(s state, id string) daemon.Lease {
		for _, l := range s.Leases {
			if l.ID == id {
				return l
			}
		}
		return daemon.Lease{}
	}
	if l := find(s, scheduledID); l.State != "scheduled" || l.Description != "nightly QA" || l.StartsAt != scheduled.StartsAt {
		t.Fatalf("reservation: %+v", s)
	}
	renewed := run("renew", "--device", "mock-1", "--ttl", "1200")
	s = read(home)
	if l := find(s, localID); l.ExpiresAt != renewed.ExpiresAt || l.ExpiresAt == active.ExpiresAt || l.StartsAt != active.StartsAt || l.Description != active.Description {
		t.Fatalf("renew: %+v", s)
	}
	c := daemon.NewHTTPClient("http://"+info.HTTPAddr, "disk-token", nil)
	rows, err := c.Devices(context.Background(), "mock")
	listed, _ := json.MarshalIndent(rows, "", "  ")
	t.Logf("$ GET /v1/devices?platform=mock\n%s", listed)
	if err != nil || len(rows) != 2 {
		t.Fatalf("devices: %+v %v", rows, err)
	}
	for _, d := range rows {
		switch d.ID {
		case "mock-1@disk":
			if d.Lease == nil || d.Lease.Description != "e2e run" || d.Lease.StartsAt != active.StartsAt || d.Lease.ExpiresAt != renewed.ExpiresAt {
				t.Fatal(d.Lease)
			}
		case "mock-2@disk":
			if len(d.Reservations) != 1 || d.Reservations[0].Description != "nightly QA" {
				t.Fatal(d.Reservations)
			}
		default:
			t.Fatal(d.ID)
		}
	}
	run("release", "--device", "mock-1")
	s = read(home)
	if len(s.Leases) != 1 || s.Leases[0].ID != scheduledID {
		t.Fatalf("release changed wrong windows: %+v", s)
	}
	retained := false
	for _, rp := range s.AcquisitionReplays {
		if rp.RequestID == original.RequestID && rp.LeaseID == localID && rp.Device == "mock-1" && rp.CompletedAt.Equal(original.CompletedAt) {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("released replay lost: %+v", s)
	}
	// Model a late delivery of the original request after the caller released it.
	// The recorded response carries the pre-renewal expiry and allocates nothing.
	req, err := http.NewRequest(http.MethodPost, "http://"+info.HTTPAddr+"/v1/leases", strings.NewReader(`{"device":"mock-1","ttlSec":600,"description":"e2e run"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer disk-token")
	req.Header.Set(daemon.HeaderRequestID, original.RequestID)
	reply, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var replay daemon.LeaseResult
	if err := json.NewDecoder(reply.Body).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	t.Logf("$ POST /v1/leases (%s=%s)\nHTTP %d %s=%s lease=%+v", daemon.HeaderRequestID, original.RequestID, reply.StatusCode, daemon.HeaderReplayed, reply.Header.Get(daemon.HeaderReplayed), replay.Lease)
	if reply.Header.Get(daemon.HeaderReplayed) != "true" || !replay.OK || replay.Lease != active {
		t.Fatalf("released grant replay: %+v", replay)
	}
	if after := read(home); len(after.Leases) != 1 || after.Leases[0].ID != scheduledID {
		t.Fatalf("replay allocated a grant: %+v", after)
	}
	legacyHome := filepath.Join(e.cwd, "legacy")
	legacyFile := filepath.Join(legacyHome, "hub", "scheduled-leases.json")
	if err := os.MkdirAll(filepath.Dir(legacyFile), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{"reservations": []map[string]any{{"id": scheduledID, "device": "mock-2", "holder": "legacy", "startsAt": scheduled.StartsAt, "expiresAt": scheduled.ExpiresAt, "ttlSec": scheduled.TTLSec, "maxSec": scheduled.MaxSec, "shared": true}}}
	old, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, old, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("$ cat %s\n%s", legacyFile, old)
	start(legacyHome)
	if l := find(read(legacyHome), scheduledID); l.Holder != "legacy" || l.State != "scheduled" {
		t.Fatal(l)
	}
	copied, err := os.ReadFile(legacyFile + ".migrated")
	if err != nil || string(copied) != string(old) {
		t.Fatalf("legacy backup: %s %v", copied, err)
	}
	if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
		t.Fatalf("old file after migration: %v", err)
	}
	t.Logf("$ cat %s.migrated\n%s", legacyFile, copied)
}
