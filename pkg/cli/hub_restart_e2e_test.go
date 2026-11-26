package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type restartHub struct {
	t    *testing.T
	e    *e2e
	name string
	env  []string
	info daemon.Info
}

func newRestartHub(t *testing.T, e *e2e, name string, ids ...string) *restartHub {
	t.Helper()
	home := filepath.Join(t.TempDir(), "hub-home")
	writeSharedMocks(t, home, ids...)
	h := &restartHub{t: t, e: e, name: name, env: []string{daemon.EnvHome + "=" + home, "MAESTRO_D_MOCK_DEVICES=" + strings.Join(ids, ",")}}
	t.Cleanup(func() { e.runEnv(h.env, "maestro-d-hub", "stop", "--end-leases") })
	return h
}
func (h *restartHub) start(extra ...string) {
	h.t.Helper()
	addr := h.info.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	args := []string{"maestro-d-hub", "start", "--name", h.name, "--no-mdns", "--listen", addr, "--token", "restart-token", "--peer-token", "restart-peer", "--json"}
	r := h.e.okEnv(h.env, append(args, extra...)...)
	var out struct{ Daemon daemon.Info }
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Daemon.PID == 0 {
		h.t.Fatal(r.stdout, err)
	}
	h.info = out.Daemon
}
func (h *restartHub) stop(extra ...string) {
	h.t.Helper()
	h.e.okEnv(h.env, append([]string{"maestro-d-hub", "stop"}, extra...)...)
}
func (h *restartHub) kill() {
	h.t.Helper()
	p, err := os.FindProcess(h.info.PID)
	if err == nil {
		err = p.Signal(syscall.SIGKILL)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Logf("$ kill -KILL %d\nSIGKILL delivered", h.info.PID)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("hub still alive after SIGKILL")
}
func (h *restartHub) client() *daemon.Client {
	return daemon.NewHTTPClient("http://"+h.info.HTTPAddr, "restart-token", nil)
}
func (h *restartHub) cli(args ...string) result {
	h.t.Helper()
	return h.e.okEnv([]string{"MAESTRO_D_TOKEN=restart-token"}, append(args, "--target-hub="+h.info.HTTPAddr)...)
}
func (h *restartHub) acquire(id string, ttl int) daemon.Lease {
	h.t.Helper()
	res, err := h.client().AcquireLease(context.Background(), daemon.LeaseRequest{Device: id, TTLSec: ttl, Description: "restart test"})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Logf("$ POST /v1/leases device=%s ttlSec=%d\n%+v", id, ttl, res.Lease)
	return res.Lease
}
func (h *restartHub) leases() []daemon.Lease {
	h.t.Helper()
	ls, err := h.client().Leases(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Logf("$ GET /v1/leases\n%+v", ls)
	return ls
}
func TestE2E_HubLeasesRestart(t *testing.T) {
	e := newE2E(t)
	t.Run("reservation starts while down", testReservationsRestart)
	t.Run("lost client reply across mesh", func(t *testing.T) { testLostMeshGrant(t, e, false) })
	t.Run("lost owner reply across mesh", func(t *testing.T) { testLostMeshGrant(t, e, true) })
	t.Run("clean stop crash and explicit end", func(t *testing.T) {
		h := newRestartHub(t, e, "restart", "mock-1", "mock-2")
		h.start()
		r := h.cli("lease", "--device", "mock-1", "--attach", "--ttl", "300", "--description", "restart e2e", "--json")
		var l daemon.LeaseResult
		if err := json.Unmarshal([]byte(r.stdout), &l); err != nil {
			t.Fatal(err)
		}
		scheduled, err := h.client().Reserve(context.Background(), daemon.ReservationRequest{Device: "mock-2", StartAt: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339), TTLSec: 60})
		if err != nil {
			t.Fatal(err)
		}
		for _, stop := range []func(){func() { h.stop() }, h.kill} {
			stop()
			h.start()
			live, err := h.client().Lease(context.Background(), l.Lease.ID)
			if err != nil || live.Lease.ID != l.Lease.ID || live.Lease.ExpiresAt != l.Lease.ExpiresAt || live.Lease.Description != "restart e2e" || !live.Lease.AwaitingAttach {
				t.Fatalf("restored grant %+v %v", live, err)
			}
			t.Logf("$ GET /v1/leases/%s\n%+v", l.Lease.ID, live.Lease)
			cmd := h.cli("tapOn", "x", "--device", "mock-1")
			if strings.Count(cmd.stderr, "(the hub restarted)") != 1 {
				t.Fatal(cmd.stderr)
			}
		}
		h.stop("--end-leases")
		h.start()
		if ls := h.leases(); len(ls) != 1 || ls[0].ID != scheduled.Lease.ID {
			t.Fatal(ls)
		}
	})
	for _, mutation := range []string{"grant", "renew", "release"} {
		t.Run("kill immediately after "+mutation, func(t *testing.T) {
			h := newRestartHub(t, e, "acked", "mock-1")
			h.start()
			l := h.acquire("mock-1", 300)
			switch mutation {
			case "renew":
				r, err := h.client().RenewLease(context.Background(), l.ID, 600)
				if err != nil {
					t.Fatal(err)
				}
				l = r.Lease
			case "release":
				if _, err := h.client().ReleaseLease(context.Background(), l.ID); err != nil {
					t.Fatal(err)
				}
			}
			h.kill()
			h.start()
			ls := h.leases()
			if mutation == "release" {
				if len(ls) != 0 {
					t.Fatal(ls)
				}
				return
			}
			if len(ls) != 1 || ls[0].ID != l.ID || ls[0].ExpiresAt != l.ExpiresAt {
				t.Fatal(ls)
			}
		})
	}
	for _, target := range []string{`"device":"mock-1"`, `"select":{"platform":"mock"}`} {
		t.Run("lost reply "+target, func(t *testing.T) {
			h := newRestartHub(t, e, "lost", "mock-1", "mock-2")
			h.start()
			body := []byte("{" + target + `,"ttlSec":180}`)
			captured := withholdGrant(t, h.info.HTTPAddr, body, func() { h.kill(); h.start() })
			retry, header := sendGrant(t, h.info.HTTPAddr, "lost-key", body)
			if header != "true" || retry.Lease.ID != captured.ID || retry.Lease.Device != captured.Device || retry.Lease.ExpiresAt != captured.ExpiresAt || len(h.leases()) != 1 {
				t.Fatal(retry, header)
			}
			other := "mock-2"
			if captured.Device == "mock-2@lost" {
				other = "mock-1"
			}
			if h.acquire(other, 60).ID == captured.ID {
				t.Fatal("two devices received the same grant")
			}
		})
	}
}
func sendGrant(t *testing.T, addr, key string, body []byte) (daemon.LeaseResult, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/leases", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer restart-token")
	req.Header.Set(daemon.HeaderRequestID, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res daemon.LeaseResult
	if err = json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	t.Logf("$ POST http://%s/v1/leases %s (request id %s)\nHTTP %d replayed=%q %+v", addr, body, key, resp.StatusCode, resp.Header.Get(daemon.HeaderReplayed), res)
	return res, resp.Header.Get(daemon.HeaderReplayed)
}

// Terminate the fixture hub once its success reaches the proxy, while the
// original caller is still waiting for HTTP headers.
func withholdGrant(t *testing.T, owner string, body []byte, restart func()) daemon.Lease {
	t.Helper()
	captured := make(chan daemon.Lease, 1)
	resume := make(chan struct{})
	deferClose := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream, _ := http.NewRequest(r.Method, "http://"+owner+r.URL.Path, r.Body)
		upstream.Header = r.Header.Clone()
		res, err := http.DefaultClient.Do(upstream)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer res.Body.Close()
		var out daemon.LeaseResult
		if err = json.NewDecoder(res.Body).Decode(&out); err != nil || !out.OK {
			http.Error(w, "owner refused fixture grant", 502)
			return
		}
		captured <- out.Lease
		<-resume
		http.Error(w, "reply lost", 502)
	}))
	defer proxy.Close()
	defer func() {
		if !deferClose {
			close(resume)
		}
	}()
	complete := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("POST", proxy.URL+"/v1/leases", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer restart-token")
		req.Header.Set(daemon.HeaderRequestID, "lost-key")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		complete <- err
	}()
	select {
	case granted := <-captured:
		t.Logf("$ proxy withhold successful owner reply\nlease=%+v", granted)
		restart()
		close(resume)
		deferClose = true
		if err := <-complete; err != nil {
			t.Fatal(err)
		}
		return granted
	case err := <-complete:
		t.Fatalf("request ended before capture: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("no grant reached the proxy")
	}
	return daemon.Lease{}
}
func TestE2E_HubLeaseExpiredWhileDown(t *testing.T) {
	e := newE2E(t)
	h := newRestartHub(t, e, "expired", "mock-1")
	h.start()
	old := h.acquire("mock-1", 5)
	h.stop()
	time.Sleep(7 * time.Second)
	h.start()
	if ls := h.leases(); len(ls) != 0 {
		t.Fatal(ls)
	}
	fresh := h.acquire("mock-1", 60)
	if old.ID == fresh.ID {
		t.Fatal(fresh)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var expired, granted bool
	err := h.client().Events(ctx, "", 0, func(ev daemon.Event) bool {
		if ev.Type == "lease.expired" {
			expired = true
		}
		granted = ev.Type == "lease.granted"
		return !granted
	})
	if !granted {
		t.Fatalf("absent grant event after restart: %v", err)
	}
	if expired {
		t.Fatal("hub published lease.expired for an offline expiry")
	}
}
