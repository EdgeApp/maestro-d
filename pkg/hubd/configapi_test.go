package hubd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

const configURL = "/v1/config/devices"

func configRequest(t *testing.T, c *daemon.Client, method, path, body string, want int) []byte {
	t.Helper()
	r, err := http.NewRequest(method, c.BaseURL()+configURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer t")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s %s: %d %s (%v), want %d", method, path, res.StatusCode, data, err, want)
	}
	return data
}

func configFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConfigRemoteForbidden(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	path := env.s.config.path
	before := configFileBytes(t, path)
	for _, test := range []struct{ method, path string }{{"GET", ""}, {"PUT", "/mock-1@peer"}, {"DELETE", "/mock-1?endLeases=true"}} {
		b := configRequest(t, env.remote, test.method, test.path, "{", 403)
		if !bytes.Contains(b, []byte(daemon.ConfigLocalOnlyMessage)) || !bytes.Equal(before, configFileBytes(t, path)) {
			t.Fatalf("%s %s: %s", test.method, test.path, b)
		}
	}
}

func TestConfigDeviceFieldsAndWindows(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	path := env.s.config.path
	writeShapingConfig(t, path, "https://lab.invalid")
	env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Holder: "Ada", TTLSec: 60})
	env.mustReserve(env.remote, daemon.ReservationRequest{Device: "mock-1", Holder: "Grace", StartAt: env.at(2 * time.Minute), TTLSec: 60})
	configRequest(t, env.local, "PUT", "/mock-1@other", "{}", 400)
	configRequest(t, env.local, "PUT", "/mock-1", "{", 400)
	configRequest(t, env.local, "PUT", "/mock-1@hubtest", `{"description":"rack A"}`, 200)
	configRequest(t, env.local, "PUT", "/offline", `{"description":"later"}`, 201)
	list := func() configListing {
		t.Helper()
		b := configRequest(t, env.local, "GET", "", "", 200)
		if bytes.Contains(b, []byte("lab.invalid")) || bytes.Contains(b, []byte("setTrafficShape")) {
			t.Fatalf("shaping request in listing: %s", b)
		}
		var res configListing
		if err := json.Unmarshal(b, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := list()
	r, off := res.Devices[0], res.Devices[1]
	if !res.OK || res.Path != path || len(res.Devices) != 2 || r.ID != "mock-1" || r.Description != "rack A" || !r.Listed || !r.TrafficShaping ||
		r.Lease == nil || r.Lease.Holder != "Ada" || len(r.Reservations) != 1 || r.Reservations[0].Holder != "Grace" || off.ID != "offline" || off.Listed || off.TrafficShaping {
		t.Fatalf("%+v", res)
	}
	configRequest(t, env.local, "PUT", "/mock-1", `{"description":""}`, 200)
	if list().Devices[0].Description != "" {
		t.Fatal("description did not clear")
	}
	configRequest(t, env.local, "DELETE", "/missing", "", 404)
	before := configFileBytes(t, path)
	b := configRequest(t, env.local, "DELETE", "/mock-1", "", 409)
	for _, want := range []string{"DEVICE_IN_USE", "Ada", "Grace", env.at(0), env.at(time.Minute), env.at(2 * time.Minute), env.at(3 * time.Minute)} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s: %s", want, b)
		}
	}
	if !bytes.Equal(before, configFileBytes(t, path)) {
		t.Fatal("refused removal rewrote config")
	}
	configRequest(t, env.local, "DELETE", "/mock-1?endLeases=true", "", 200)
	if len(env.s.leases.list()) != 0 || len(list().Devices) != 1 {
		t.Fatal("forced removal kept a grant or config row")
	}
	if bytes.Contains(configFileBytes(t, ReservationsPath(env.s.cfg.Name)), []byte("Grace")) {
		t.Fatal("forced removal left a persisted reservation")
	}
	n := 0
	for _, ev := range env.s.Events().Recent("mock-1", 0) {
		if ev.Type == "lease.released" && ev.Reason == endRemoved {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d removed grants", n)
	}

	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	configRequest(t, env.local, "PUT", "/mock-1", "{}", 400)
	configRequest(t, env.local, "DELETE", "/offline", "", 400)
	if res := list(); res.ConfigError == "" || len(res.Devices) != 1 {
		t.Fatalf("invalid file: %+v", res)
	}
	daemontest.WriteDevices(t, path, "mock-2")
	configRequest(t, env.local, "PUT", "/mock-2", "{}", 200)
	if res := list(); res.ConfigError != "" || res.Devices[0].ID != "mock-2" {
		t.Fatalf("repair: %+v", res)
	}
}

func TestConfigTVAndComputer(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.mu.Lock()
	env.tv, env.tvKnown = true, true
	env.mu.Unlock()
	configRequest(t, env.local, "PUT", "/TV-1", "{}", 201)
	configRequest(t, env.local, "PUT", "/mac", `{"description":"host"}`, 201)
	if !env.s.shared("TV-1") || env.s.shared("mac") || env.s.DeviceConfig().Devices["mac"].Description != "host" {
		t.Fatal("TV or computer policy")
	}
}

func TestConfigRemoveAVDAliases(t *testing.T) {
	for _, id := range []string{"avd:Pixel", "emulator-5554"} {
		t.Run(id, func(t *testing.T) {
			path := daemontest.DeviceConfig(t, "emulator-5554", "mock-1", "avd:Pixel", "avd:Other")
			env := newHubEnv(t, daemon.Config{DeviceConfig: path})
			env.emuAVD = "Pixel"
			other := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Holder: "other"}).Lease
			lease := env.lease(env.remote, daemon.LeaseRequest{Device: "emulator-5554", Holder: "AVD holder", TTLSec: 30}).Lease
			before := configFileBytes(t, path)
			configRequest(t, env.local, "DELETE", "/"+id, "", 409)
			if !bytes.Equal(before, configFileBytes(t, path)) || len(env.s.leases.list()) != 2 {
				t.Fatal("refusal changed grants or either spelling")
			}
			configRequest(t, env.local, "DELETE", "/"+id+"?endLeases=true", "", 200)
			if !slices.Equal(env.s.DeviceConfig().Shared, []string{"mock-1", "avd:Other"}) || len(env.s.leases.list()) != 1 || !env.s.leases.isLive(other.ID) {
				t.Fatalf("forced removal: %+v, %+v", env.s.DeviceConfig().Shared, env.s.leases.list())
			}
			if env.s.shared("emulator-5554") || env.s.shared("avd:Pixel") {
				t.Fatal("removed AVD still shared")
			}
			devs, err := env.remote.Devices(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range devs {
				if d.ID == "emulator-5554" || d.ID == "avd:Pixel" {
					t.Fatal("removed AVD in listing")
				}
			}
			if _, err := env.remote.AcquireLease(context.Background(), daemon.LeaseRequest{Device: "emulator-5554"}); err == nil {
				t.Fatal("leased removed AVD")
			}
			unshares := 0
			for _, ev := range env.s.Events().Recent("", 0) {
				if ev.Type == "device.unshared" {
					unshares++
					if ev.Device != "emulator-5554" {
						t.Fatalf("wrong unshare: %+v", ev)
					}
				}
			}
			if unshares != 1 {
				t.Fatalf("%d unshares", unshares)
			}
			if !env.s.removedLeaseMatches("emulator-5554", lease.ID) || env.s.removedLeaseMatches("mock-1", lease.ID) {
				t.Fatal("tombstone identity")
			}
			env.clock.advance(30 * time.Second)
			if env.s.removedLeaseMatches("emulator-5554", lease.ID) {
				t.Fatal("expired tombstone")
			}
			configRequest(t, env.local, "PUT", "/avd:Pixel", "{}", 201)
			configRequest(t, env.local, "PUT", "/emulator-5554", "{}", 201)
			env.mu.Lock()
			env.emuAVD = "Other"
			env.mu.Unlock()
			configRequest(t, env.local, "DELETE", "/"+id, "", 200)
			want := []string{"mock-1", "avd:Pixel"}
			if id == "avd:Pixel" {
				want = []string{"mock-1", "avd:Other", "emulator-5554"}
			}
			if !slices.Equal(env.s.DeviceConfig().Shared, want) {
				t.Fatalf("recycled serial: %v", env.s.DeviceConfig().Shared)
			}
		})
	}
}

func TestConfigRemoveStaleAdmissions(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, reservation := range []bool{false, true} {
			t.Run(fmt.Sprintf("force=%t/reserve=%t", force, reservation), func(t *testing.T) {
				env := newHubEnv(t, daemon.Config{})
				d, _ := env.s.Registry().Lookup("mock-1")
				checked, resume := make(chan struct{}), make(chan struct{})
				result := make(chan *daemon.Error, 1)
				go func() {
					if !env.s.sharedInfo(d) {
						result <- daemon.Errorf(daemon.CodeInternal, "bad initial share")
						close(checked)
						return
					}
					close(checked)
					<-resume
					var e *daemon.Error
					if reservation {
						env.s.leaseMu.Lock()
						_, e = env.s.reserveDeviceLocked(true, d, "Grace", env.clock.now().Add(time.Minute), 10)
						env.s.leaseMu.Unlock()
					} else {
						_, e = env.s.leaseDevice(true, d, "Ada", 10, claimNone)
					}
					result <- e
				}()
				<-checked
				configRequest(t, env.local, "DELETE", fmt.Sprintf("/mock-1?endLeases=%t", force), "", 200)
				close(resume)
				if e := <-result; e == nil || e.Code != daemon.CodeDeviceNotFound || len(env.s.leases.list()) != 0 {
					t.Fatalf("stale admission: %v", e)
				}
			})
		}
	}
}

func TestConfigRemoveDuringActivation(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprint(force), func(t *testing.T) {
			env := newHubEnv(t, daemon.Config{})
			grant := env.mustReserve(env.remote, daemon.ReservationRequest{Device: "mock-2", StartAt: env.at(time.Minute), TTLSec: 120})
			env.clock.advance(time.Minute)
			var race sync.WaitGroup
			start := make(chan struct{})
			race.Add(1)
			go func() { defer race.Done(); <-start; env.s.sweepLeases() }()
			want := 409
			if force {
				want = 200
			}
			close(start)
			configRequest(t, env.local, "DELETE", fmt.Sprintf("/mock-2?endLeases=%t", force), "", want)
			race.Wait()
			if env.s.shared("mock-2") == force || env.s.leases.isLive(grant.ID) == force {
				t.Fatalf("force=%t grants=%v", force, env.s.leases.list())
			}
		})
	}
}

func TestConfigRemoveRetryAfterStateWriteFailure(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Holder: "Ada"})
	path := ReservationsPath(env.s.cfg.Name)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	configRequest(t, env.local, "DELETE", "/mock-1?endLeases=true", "", 500)
	if !env.s.shared("mock-1") || len(env.s.leases.list()) != 0 || len(env.s.removing) != 0 {
		t.Fatal("inconsistent partial removal")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	configRequest(t, env.local, "DELETE", "/mock-1", "", 200)
	if env.s.shared("mock-1") {
		t.Fatal("retry did not finish")
	}
}

func TestConfigRemoveRecycledSerialPreservesRecordedGrant(t *testing.T) {
	path := daemontest.DeviceConfig(t, "emulator-5554", "avd:Old", "avd:New")
	env := newHubEnv(t, daemon.Config{DeviceConfig: path})
	env.emuAVD = "Old"
	original := env.lease(env.remote, daemon.LeaseRequest{Device: "emulator-5554", Holder: "Old holder"}).Lease
	env.mu.Lock()
	env.emuAVD = "New"
	env.mu.Unlock()
	configRequest(t, env.local, "DELETE", "/emulator-5554?endLeases=true", "", 200)
	if !env.s.leases.isLive(original.ID) || !slices.Equal(env.s.DeviceConfig().Shared, []string{"avd:Old"}) {
		t.Fatal("recycled serial ended the previous AVD's grant")
	}
	b := configRequest(t, env.local, "DELETE", "/avd:Old", "", 409)
	if !bytes.Contains(b, []byte("Old holder")) {
		t.Fatal(string(b))
	}
	configRequest(t, env.local, "DELETE", "/avd:Old?endLeases=true", "", 200)
	if env.s.leases.isLive(original.ID) {
		t.Fatal("removal by stable identity left the old grant")
	}
}

func TestConfigRemoveHoldsAdmissionDuringTeardown(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	d, _ := env.s.Registry().Lookup("SIM-UP")
	lease := env.lease(env.remote, daemon.LeaseRequest{Device: d.ID, Holder: "Ada"})
	if !env.s.leases.markBooted(d.ID) {
		t.Fatal("mark boot duty")
	}
	stopping, resume := make(chan struct{}), make(chan struct{})
	env.s.stop = func(daemon.DeviceInfo) error { close(stopping); <-resume; return nil }
	done := make(chan struct{})
	go func() { defer close(done); configRequest(t, env.local, "DELETE", "/SIM-UP?endLeases=true", "", 200) }()
	select {
	case <-stopping:
	case <-time.After(10 * time.Second):
		close(resume)
		t.Fatal("teardown did not start")
	}
	env.s.leaseMu.Lock()
	_, reserveErr := env.s.reserveDeviceLocked(true, d, "next", env.clock.now().Add(time.Minute), 10)
	env.s.leaseMu.Unlock()
	_, grantErr := env.s.leaseDevice(true, d, "next", 10, claimNone)
	other, otherErr := env.remote.AcquireLease(context.Background(), daemon.LeaseRequest{Device: "mock-1"})
	close(resume)
	<-done
	if reserveErr == nil || grantErr == nil || otherErr != nil || !env.s.leases.isLive(other.Lease.ID) || env.s.leases.isLive(lease.Lease.ID) || env.s.shared(d.ID) {
		t.Fatalf("grant=%v reserve=%v other=%v", grantErr, reserveErr, otherErr)
	}
}

func TestConfigRemovedWindowBound(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	until := env.clock.now().Add(time.Minute)
	for i := 0; i < maxRemovedLeases; i++ {
		env.s.removedLeases[fmt.Sprint(i)] = removedLease{device: "mock-1", until: until}
	}
	lease := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-2", Holder: "Ada", TTLSec: 300})
	configRequest(t, env.local, "DELETE", "/mock-2?endLeases=true", "", 409)
	if !env.s.leases.isLive(lease.Lease.ID) || !env.s.removedLeaseMatches("mock-1", "0") {
		t.Fatal("full history discarded a credential or grant")
	}
	env.clock.advance(time.Minute)
	env.s.sweepLeases()
	configRequest(t, env.local, "DELETE", "/mock-2?endLeases=true", "", 200)
	if len(env.s.removedLeases) != 1 {
		t.Fatalf("%d expired entries retained", len(env.s.removedLeases))
	}
}
