package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// Exercise the normal CLI command with a hub that can move the held AVD,
// lose its staged files and return DEVICE_NOT_ATTACHED on the retry.
func TestTargetedOneShotRestoresOriginalGrant(t *testing.T) {
	e := newE2E(t)
	t.Setenv(daemon.EnvHome, e.home)
	old, next := "emulator-5554@bench", "emulator-5556@bench"
	lease := daemon.Lease{ID: "held@bench", Device: old, State: daemon.LeaseActive, StartsAt: time.Now().UTC().Format(time.RFC3339), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), TTLSec: 3600}
	var mu sync.Mutex
	var attached, alwaysDetached, expired bool
	var attaches, steps, uploaded, replacementCalls, leases int
	var installed string
	var blob []byte
	app := filepath.Join(e.cwd, "checkout.apk")
	if err := os.WriteFile(app, []byte("checkout app bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		info := func() daemon.DeviceInfo {
			return daemon.DeviceInfo{ID: lease.Device, Platform: "android", Ready: true, Attached: attached}
		}
		ok := func(v any) { daemon.WriteJSON(w, 200, v) }
		switch r.URL.Path {
		case "/v1/status":
			ok(map[string]any{"ok": true, "daemon": map[string]string{"name": "hub", "hub": "bench"}})
			return
		case "/v1/leases":
			leases++
			var req daemon.LeaseRequest
			json.NewDecoder(r.Body).Decode(&req)
			if expired && req.Device == next {
				ok(daemon.LeaseResult{Lease: daemon.Lease{ID: "replacement", Device: next, State: daemon.LeaseActive}})
				return
			}
			if req.Attach != nil {
				attached = true
				attaches++
			}
			ok(map[string]any{"ok": true, "lease": lease, "device": info()})
			return
		case "/v1/leases/" + lease.ID:
			if expired {
				daemon.WriteError(w, daemon.Errorf(daemon.CodeLeaseExpired, "grant ended"))
				return
			}
			ok(map[string]any{"ok": true, "lease": lease})
			return
		}
		prefix := "/v1/devices/" + lease.Device
		if !strings.HasPrefix(r.URL.Path, prefix) {
			replacementCalls++
			daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "wrong device"))
			return
		}
		suffix := strings.TrimPrefix(r.URL.Path, prefix)
		if suffix == "" {
			if expired && r.Header.Get(daemon.HeaderLease) == lease.ID {
				t.Error("device lookup reused the expired credential")
			}
			ok(map[string]any{"ok": true, "device": info()})
			return
		}
		if r.Header.Get(daemon.HeaderLease) != lease.ID {
			daemon.WriteError(w, daemon.Errorf(daemon.CodeLeaseRequired, "wrong credential"))
			return
		}
		switch {
		case strings.HasPrefix(suffix, "/blobs/") && r.Method == "HEAD":
			if len(blob) == 0 {
				w.WriteHeader(404)
			} else {
				w.WriteHeader(200)
			}
		case strings.HasPrefix(suffix, "/blobs/") && r.Method == "PUT":
			blob, _ = io.ReadAll(r.Body)
			uploaded++
			ok(map[string]any{"ok": true})
		case suffix == "/attach":
			var cfg daemon.AttachConfig
			json.NewDecoder(r.Body).Decode(&cfg)
			if cfg.AppFile != "" {
				if !strings.HasPrefix(cfg.AppFile, "blob:sha256:") || !bytes.Equal(blob, []byte("checkout app bytes")) || cfg.NoAppInstall {
					t.Errorf("attach install inputs: %+v blob %q", cfg, blob)
				}
				installed = string(blob)
			}
			attaches++
			attached = true
			lease.AwaitingAttach = false
			ok(map[string]any{"ok": true, "device": info()})
		case strings.HasPrefix(suffix, "/commands/"):
			steps++
			if !attached || alwaysDetached {
				daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotAttached, "detached"))
				return
			}
			ok(map[string]any{"ok": true, "step": "tapOn"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	call := func(args ...string) result { return e.run(append(args, "--target-hub="+srv.URL)...) }
	if r := call("lease", "--device", old, "--no-attach"); r.code != 0 {
		t.Fatal(r)
	}
	if r := call("tapOn", "x", "--device", old, "--app-file", app); r.code != 0 || strings.Contains(r.stderr, "hub restarted") {
		t.Fatal(r)
	}
	mu.Lock()
	if leases != 1 || attaches != 1 || uploaded != 1 || steps != 1 || installed != "checkout app bytes" {
		t.Fatalf("first attach: leases=%d attaches=%d uploaded=%d steps=%d installed=%q", leases, attaches, uploaded, steps, installed)
	}
	lease.Device = ""
	lease.AwaitingAttach = true
	attached = false
	blob = nil
	mu.Unlock()
	if r := call("tapOn", "x", "--device", old); r.code == 0 || !strings.Contains(r.stderr, "unavailable") {
		t.Fatal(r)
	}
	mu.Lock()
	lease.Device = next
	mu.Unlock()
	if r := call("tapOn", "x", "--device", old); r.code != 0 || strings.Count(r.stderr, "(the hub restarted)") != 1 {
		t.Fatal(r)
	}
	if r := call("lease", "--device", old, "--json"); r.code != 0 || !strings.Contains(r.stdout, next) {
		t.Fatal(r)
	}
	mu.Lock()
	if attaches != 2 || steps != 3 || uploaded != 2 || replacementCalls != 0 {
		t.Fatalf("restored attaches=%d steps=%d uploads=%d replacement calls=%d", attaches, steps, uploaded, replacementCalls)
	}
	mu.Unlock()
	records := loadLeases()
	if len(records) != 1 || records[0].Device != next || records[0].OriginalDevice != old || records[0].Attach == nil || records[0].Attach.AppFile != app || records[0].ExpiresAt != lease.ExpiresAt {
		t.Fatal(records)
	}
	// A client's record from an older release has no saved config. The
	// invocation supplies the install inputs after the restored command fails.
	updateLeases(func(records []leaseRecord) []leaseRecord { records[0].Attach = nil; return records })
	mu.Lock()
	attached = false
	lease.AwaitingAttach = true
	blob = nil
	alwaysDetached = true
	mu.Unlock()
	if r := call("tapOn", "x", "--device", old, "--app-file", app); r.code == 0 || strings.Count(r.stderr, "(the hub restarted)") != 1 {
		t.Fatal(r)
	}
	mu.Lock()
	if steps != 5 || attaches != 3 || uploaded != 3 {
		t.Fatalf("retry limit: attaches=%d steps=%d uploaded=%d", attaches, steps, uploaded)
	}
	attached = false
	lease.AwaitingAttach = true
	mu.Unlock()
	if r := call("attach", "--device", old, "--app-id", "explicit.app", "--json"); r.code != 0 || !strings.Contains(r.stdout, "android") {
		t.Fatal(r)
	}
	if rec := loadLeases()[0]; rec.Attach == nil || rec.Attach.AppID != "explicit.app" || rec.Attach.AppFile != "" {
		t.Fatalf("explicit attach configuration: %+v", rec.Attach)
	}
	mu.Lock()
	expired = true
	mu.Unlock()
	if r := call("tapOn", "x", "--device", old); r.code == 0 {
		t.Fatal(r)
	}
	// Explicit acquisition handles definitive expiry in the same invocation.
	recordLease(srv.URL, daemon.Lease{ID: lease.ID, Device: next})
	if r := call("lease", "--device", next, "--no-attach", "--json"); r.code != 0 || !strings.Contains(r.stdout, "replacement") {
		t.Fatal(r)
	}
	mu.Lock()
	defer mu.Unlock()
	if leases != 2 || attaches != 4 || replacementCalls != 0 {
		t.Fatal(leases, attaches, replacementCalls)
	}
}

func TestOneShotAfterLeasedAVDsExchangeSerials(t *testing.T) {
	e := newE2E(t)
	t.Setenv(daemon.EnvHome, e.home)
	first, second := "emulator-5554@lab", "emulator-5556@lab"
	original := map[string]string{"alpha": first, "beta": second}
	live := map[string]string{"alpha": second, "beta": first}
	attached := map[string]bool{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/status" {
			daemon.WriteJSON(w, 200, map[string]any{"ok": true, "daemon": map[string]string{"hub": "lab"}})
			return
		}
		if id, ok := strings.CutPrefix(r.URL.Path, "/v1/leases/"); ok {
			if device, ok := live[id]; ok {
				daemon.WriteJSON(w, 200, daemon.LeaseResult{Lease: daemon.Lease{ID: id, Device: device, State: daemon.LeaseActive, AwaitingAttach: !attached[id]}})
				return
			}
		}
		id := r.Header.Get(daemon.HeaderLease)
		suffix, ok := strings.CutPrefix(r.URL.Path, "/v1/devices/"+live[id])
		if _, known := live[id]; !known || !ok {
			t.Errorf("misdirected credential %q: %s", id, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch suffix {
		case "/attach":
			var cfg daemon.AttachConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil || cfg.AppID != id+".app" {
				t.Errorf("%s attach: %+v, %v", id, cfg, err)
			}
			attached[id] = true
			fallthrough
		case "":
			daemon.WriteJSON(w, 200, daemon.DeviceResult{Device: daemon.DeviceInfo{ID: live[id], Attached: attached[id]}})
		case "/commands/tapOn":
			if !attached[id] {
				daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotAttached, "detached"))
				return
			}
			daemon.WriteJSON(w, 200, map[string]any{"ok": true, "step": "tapOn"})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	for id, serial := range original {
		recordLeaseTarget(server.URL, daemon.Lease{ID: id, Device: serial}, "avd:"+id)
		rememberAttach(server.URL, id, daemon.AttachConfig{AppID: id + ".app"})
	}
	for _, id := range []string{"alpha", "beta", "alpha"} {
		if r := e.run("tapOn", "x", "--device", original[id], "--target-hub="+server.URL); r.code != 0 {
			t.Fatal(r)
		}
		for other, serial := range original {
			for _, target := range []string{serial, "avd:" + other} {
				recs, err := targetLeaseRecords(leasesFor(server.URL), target)
				if err != nil || len(recs) != 1 || recs[0].ID != other || recs[0].Attach == nil || recs[0].Attach.AppID != other+".app" {
					t.Fatalf("%s: %+v, %v", target, recs, err)
				}
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !attached["alpha"] || !attached["beta"] {
		t.Fatal(attached)
	}
}

func TestAcquiringRecycledSerialPreservesMovedGrant(t *testing.T) {
	e := newE2E(t)
	t.Setenv(daemon.EnvHome, e.home)
	const serial = "emulator-5554@lab"
	const moved = "emulator-5556@lab"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			daemon.WriteJSON(w, 200, map[string]any{"ok": true, "daemon": map[string]string{"hub": "lab"}})
		case "/v1/leases":
			daemon.WriteJSON(w, 200, daemon.LeaseResult{Lease: daemon.Lease{ID: "replacement", Device: serial, State: daemon.LeaseActive}})
		case "/v1/leases/original":
			daemon.WriteJSON(w, 200, daemon.LeaseResult{Lease: daemon.Lease{ID: "original", Device: moved, State: daemon.LeaseActive}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	recordLeaseTarget(server.URL, daemon.Lease{ID: "original", Device: serial}, "avd:Original")
	rememberAttach(server.URL, "original", daemon.AttachConfig{AppID: "original.app"})
	if r := e.run("lease", "--platform", "android", "--no-attach", "--target-hub="+server.URL); r.code != 0 {
		t.Fatal(r)
	}
	if r := e.run("lease", "--device", "avd:Original", "--json", "--target-hub="+server.URL); r.code != 0 || !strings.Contains(r.stdout, moved) {
		t.Fatal(r)
	}
	original, replacement := findLease(server.URL, moved), findLease(server.URL, serial)
	if original == nil || original.ID != "original" || original.Attach == nil || original.Attach.AppID != "original.app" || replacement == nil || replacement.ID != "replacement" {
		t.Fatal(original, replacement)
	}
}
