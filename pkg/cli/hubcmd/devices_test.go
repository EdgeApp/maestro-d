package hubcmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

func devicesCLI(t *testing.T, deps daemon.Deps, args ...string) (string, error) {
	t.Helper()
	o, parsed, err := parseDaemonArgs(args, deviceFlags, nil)
	if err != nil {
		t.Fatal(err)
	}
	o.deps = func() daemon.Deps { return deps }
	out := captureStdout(t, func() { err = runHubDevices(t.Context(), o, parsed.Positional) })
	return out, err
}

func mustDevices(t *testing.T, deps daemon.Deps, args ...string) string {
	t.Helper()
	out, err := devicesCLI(t, deps, args...)
	if err != nil {
		t.Fatal(args, err)
	}
	return out
}

func storedBytes(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func requireCode(t *testing.T, err error, code daemon.Code, exit int) {
	t.Helper()
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != code || de.ExitCode() != exit {
		t.Fatalf("got %v, want %s (exit %d)", err, code, exit)
	}
}

func writeDeviceGrants(t *testing.T, records ...map[string]string) string {
	t.Helper()
	path := hubd.LeasesPath("hub")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		r["state"] = "scheduled"
		r["startsAt"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		r["expiresAt"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	}
	b, err := json.Marshal(map[string]any{"version": 2, "leases": records})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHubDevicesOffline(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	t.Setenv("MAESTRO_D_HUB_NAME", "lab")
	deps := daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { return []daemon.DeviceInfo{{ID: "one"}} }}
	path := filepath.Join(t.TempDir(), "alternative.json")
	if got := mustDevices(t, deps, "add", "one@lab", "--config", path, "--description", "rack"); strings.TrimSpace(got) != "hub not running; edited "+path {
		t.Fatal(got)
	}
	mustDevices(t, deps, "--config", path, "add", "two", "--json")
	state := writeDeviceGrants(t, map[string]string{"id": "A", "device": "one", "holder": "Ada"}, map[string]string{"id": "B", "device": "two", "holder": "Grace"})
	out := mustDevices(t, deps, "--config", path, "--json")
	var listing daemon.HubConfigListing
	if err := json.Unmarshal([]byte(out), &listing); err != nil {
		t.Fatal(err)
	}
	if !listing.OK || listing.Path != path || len(listing.Devices) != 2 {
		t.Fatal(out)
	}
	d := listing.Devices[0]
	if !d.Listed || d.TrafficShaping || d.ID != "one" || d.Description != "rack" || len(d.Reservations) != 1 || d.Reservations[0].Holder != "Ada" || listing.Devices[1].Listed {
		t.Fatal(out)
	}
	before, grants := storedBytes(t, path), storedBytes(t, state)
	_, err := devicesCLI(t, deps, "--config", path, "remove", "one")
	requireCode(t, err, daemon.CodeDeviceInUse, 5)
	for _, want := range []string{"Ada", d.Reservations[0].StartsAt, d.Reservations[0].ExpiresAt} {
		if !strings.Contains(err.Error(), want) {
			t.Fatal(err)
		}
	}
	if before != storedBytes(t, path) || grants != storedBytes(t, state) {
		t.Fatal("refusal changed files")
	}
	mustDevices(t, deps, "--config", path, "remove", "one", "--end-leases", "--json")
	if got := storedBytes(t, state); strings.Contains(got, "Ada") || !strings.Contains(got, "Grace") {
		t.Fatal(got)
	}
	if got := mustDevices(t, deps, "list", "--config", path); !strings.Contains(got, "two") || strings.Contains(got, "one") {
		t.Fatal(got)
	}
	for _, args := range [][]string{{"list", "one"}, {"set", "two"}, {"remove", "two", "--description", "x"}, {"add", "x", "--end-leases"}, {"foo"}, {"add"}, {"add", "one@wrong"}} {
		_, err := devicesCLI(t, deps, args...)
		requireCode(t, err, daemon.CodeUsage, 2)
	}
	_, err = devicesCLI(t, deps, "remove", "unknown")
	requireCode(t, err, daemon.CodeDeviceNotFound, 4)
}

func TestHubDevicesOfflineAliases(t *testing.T) {
	const serial = "emulator-5554"
	for _, tc := range []struct {
		name, id, live, grantAVD string
		unresolved               bool
		want                     []string
		keepGrant                bool
	}{
		{"remove-serial", serial, "Pixel", "", false, []string{"other", "avd:Old"}, false},
		{"remove-stable", "avd:Pixel", "Pixel", "", false, []string{"other", "avd:Old"}, false},
		{"unverified", serial, "", "", true, nil, true},
		{"unverified-stable", "avd:Pixel", "", "", true, nil, true},
		{"recorded", "avd:Pixel", "", "android:pixel", false, []string{"other", "avd:Old"}, false},
		{"recycled-serial", serial, "Pixel", "android:old", false, []string{"other", "avd:Old"}, true},
		{"recycled-stable", "avd:Old", "Pixel", "android:old", false, []string{serial, "other", "avd:Pixel"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(daemon.EnvHome, t.TempDir())
			path := daemon.DeviceConfigPath("")
			daemontest.WriteDevices(t, path, serial, "other", "avd:Pixel", "avd:Old")
			deps := daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo {
				if tc.live == "" {
					return nil
				}
				return []daemon.DeviceInfo{{ID: serial, Name: tc.live, Kind: "emulator", Platform: "android", Ready: true}, {ID: "other", Platform: "mock", Ready: true}}
			}}
			state := writeDeviceGrants(t, map[string]string{"id": "A", "holder": "Ada", "device": serial, "avd": tc.grantAVD}, map[string]string{"id": "B", "holder": "Grace", "device": "other"})
			before, grants := storedBytes(t, path), storedBytes(t, state)
			if tc.unresolved {
				for _, suffix := range [][]string{nil, {"--end-leases"}} {
					_, err := devicesCLI(t, deps, append([]string{"remove", tc.id}, suffix...)...)
					requireCode(t, err, daemon.CodeUsage, 2)
					if !strings.Contains(err.Error(), serial) || !strings.Contains(err.Error(), "avd:Pixel") {
						t.Fatal(err)
					}
					if before != storedBytes(t, path) || grants != storedBytes(t, state) {
						t.Fatal("unresolved entries changed")
					}
				}
				return
			}
			if !tc.keepGrant {
				_, err := devicesCLI(t, deps, "remove", tc.id)
				requireCode(t, err, daemon.CodeDeviceInUse, 5)
			}
			mustDevices(t, deps, "remove", tc.id, "--end-leases")
			cfg, err := netshape.LoadConfig(path)
			if err != nil || !slices.Equal(cfg.Shared, tc.want) {
				t.Fatal(cfg, err)
			}
			if got := storedBytes(t, state); !strings.Contains(got, "Grace") || strings.Contains(got, "Ada") != tc.keepGrant {
				t.Fatal(got)
			}
			c := startDevicesHub(t, path, deps)
			list, err := c.HubDevices(t.Context())
			if err != nil || len(list.Devices) != len(tc.want) {
				t.Fatal(list, err)
			}
			remote := daemon.NewHTTPClient(c.Info.HTTPAddr, "token", nil)
			devs, err := remote.Devices(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			shared := slices.Contains(tc.want, serial)
			if slices.ContainsFunc(devs, func(d daemon.DeviceInfo) bool { return d.ID == serial }) != shared {
				t.Fatal(devs)
			}
			if !shared {
				_, err = remote.AcquireLease(t.Context(), daemon.LeaseRequest{Device: serial, Holder: "new"})
				requireCode(t, err, daemon.CodeDeviceNotFound, 4)
			}
			restored, err := c.Leases(t.Context())
			if err != nil || len(restored) == 0 || !slices.ContainsFunc(restored, func(l daemon.Lease) bool { return l.ID == "B" }) || slices.ContainsFunc(restored, func(l daemon.Lease) bool { return l.ID == "A" }) != tc.keepGrant {
				t.Fatal(restored, err)
			}
		})
	}
}

func newDevicesHub(path string, deps daemon.Deps) *daemon.Server {
	return daemon.New(daemon.Config{HubProcess: true, Name: "hub", HubName: "lab", HTTPAddr: "127.0.0.1:0", Token: "token", Hub: &hubd.Server{}, DeviceConfig: path}, deps)
}

func startDevicesHub(t *testing.T, path string, deps daemon.Deps) *daemon.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	s := newDevicesHub(path, deps)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client, _, err := waitConfigSocket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHubDevicesOfflineRemovalContendsWithStartup(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	path := daemon.DeviceConfigPath("")
	daemontest.WriteDevices(t, path, "one", "two")
	writeDeviceGrants(t, map[string]string{"id": "A", "device": "one"}, map[string]string{"id": "B", "device": "two"})
	locked, finish := make(chan struct{}), make(chan struct{})
	devices := []daemon.DeviceInfo{{ID: "one", Ready: true}, {ID: "two", Ready: true}}
	o := &daemonOpts{endLeases: true, deps: func() daemon.Deps {
		return daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { close(locked); <-finish; return devices }}
	}}
	done := make(chan error, 1)
	go func() { done <- runHubDevices(t.Context(), o, []string{"remove", "one"}) }()
	<-locked
	deps := daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { return devices }}
	err := newDevicesHub(path, deps).Run(t.Context())
	close(finish)
	if editErr := <-done; editErr != nil {
		t.Fatal(editErr)
	}
	requireCode(t, err, daemon.CodeHubRunning, 2)
	local := startDevicesHub(t, path, deps)
	remote := daemon.NewHTTPClient(local.Info.HTTPAddr, "token", nil)
	list, err := remote.Devices(t.Context(), "")
	if err != nil || len(list) != 1 || list[0].ID != "two" {
		t.Fatal(list, err)
	}
	leases, err := local.Leases(t.Context())
	if err != nil || len(leases) != 1 || leases[0].ID != "B" {
		t.Fatal(leases, err)
	}
}

func TestHubDevicesConfigPathAndEndLeases(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	path, other := daemontest.DeviceConfig(t, "one"), daemontest.DeviceConfig(t, "private")
	deps := daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { return []daemon.DeviceInfo{{ID: "one", Ready: true}} }}
	c := startDevicesHub(t, path, deps)
	before, untouched := storedBytes(t, path), storedBytes(t, other)
	_, err := devicesCLI(t, deps, "remove", "one", "--config", other)
	requireCode(t, err, daemon.CodeUsage, 2)
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), other) {
		t.Fatal(err)
	}
	// A restarted process serving a different config independently checks the bound path.
	err = c.RemoveHubDevice(t.Context(), "one", other, true)
	requireCode(t, err, daemon.CodeUsage, 2)
	_, err = c.PutHubDevice(t.Context(), "x", netshape.DeviceFields{}, other)
	requireCode(t, err, daemon.CodeUsage, 2)
	if before != storedBytes(t, path) || untouched != storedBytes(t, other) {
		t.Fatal("mismatched paths changed")
	}
	mustDevices(t, deps, "set", "one", "--config", path, "--description", "abc")
	mustDevices(t, deps, "add", "other", "--json")
	_, err = c.Reserve(t.Context(), daemon.ReservationRequest{Device: "one", Holder: "Ada", StartAt: time.Now().Add(time.Hour).Format(time.RFC3339), TTLSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	_, err = devicesCLI(t, deps, "remove", "one", "--config", path)
	requireCode(t, err, daemon.CodeDeviceInUse, 5)
	mustDevices(t, deps, "remove", "one", "--end-leases", "--config", path)
	if got := mustDevices(t, deps, "--json"); strings.Contains(got, `"id": "one"`) {
		t.Fatal(got)
	}
}

func TestHubDevicesHeldLockWithoutReadySocket(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	path := daemon.DeviceConfigPath("")
	daemontest.WriteDevices(t, path, "one")
	before := storedBytes(t, path)
	lock, err := fsage.TryHubLock(daemon.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer fsage.UnlockFile(lock)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	err = runHubDevices(ctx, &daemonOpts{}, []string{"remove", "one"})
	requireCode(t, err, daemon.CodeDaemonUnavailable, 3)
	if before != storedBytes(t, path) {
		t.Fatal("lock holder's config changed")
	}
	// A definitive socket error is returned directly, without an offline fallback.
	dir := t.TempDir()
	sock := filepath.Join(dir, "u.sock")
	if len(sock) >= 100 {
		dir, err = os.MkdirTemp("", "hs-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		sock = filepath.Join(dir, "u")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemon.WriteError(w, daemon.Forbidden(daemon.ConfigLocalOnlyMessage))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if err := os.MkdirAll(daemon.HubRunDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := daemon.WriteInfo(&daemon.Info{Name: "hub", RunDir: daemon.HubRunDir(), SocketPath: sock, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	_, err = devicesCLI(t, daemon.Deps{}, "remove", "one")
	requireCode(t, err, daemon.CodeForbidden, 2)
	if before != storedBytes(t, path) {
		t.Fatal("forbidden socket request fell back")
	}
}
