package daemon

import (
	"context"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingHub exercises the host's side of the interface. Unexpected hooks
// panic through the embedded interface, including any accidental attach.
type recordingHub struct {
	Hub
	gated, completed, visible int
	reject                    bool
	lastMode                  LeaseMode
	events                    []Event
	ended                     bool
}

func (h *recordingHub) TCPCaller(addr string, outputs bool) Caller {
	return Caller{Remote: true, Addr: addr, ClientOutputs: outputs}
}
func (h *recordingHub) VerifyBinding(context.Context, string) *Error { return nil }
func (h *recordingHub) DeviceConfig() *netshape.Config               { return &netshape.Config{} }
func (h *recordingHub) Bind(*Server, Config, Deps)                   {}
func (h *recordingHub) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /test-hub", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
}
func (h *recordingHub) Gate(mode LeaseMode, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.gated++
		h.lastMode = mode
		if h.reject {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		next(w, r)
		h.completed++
	}
}
func (h *recordingHub) Visible(r *http.Request, devices []DeviceInfo) []DeviceInfo {
	h.visible++
	if !CallerFrom(r.Context()).Remote {
		return devices
	}
	for _, d := range devices {
		if d.ID == "first" {
			return []DeviceInfo{d}
		}
	}
	return []DeviceInfo{}
}
func (h *recordingHub) OnEvent(ev Event)                 { h.events = append(h.events, ev) }
func (h *recordingHub) PrepareStop(endLeases bool) error { h.ended = endLeases; return nil }
func (h *recordingHub) Shutdown(context.Context) error   { return nil }

func TestHubSeam(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	h := new(recordingHub)
	devices := []DeviceInfo{{ID: "first", Ready: true}, {ID: "private", Ready: true}}
	s := New(Config{Name: "test", Hub: h, Token: "token"}, Deps{ListDevices: func(string) []DeviceInfo { return devices }})
	for _, d := range devices {
		s.Registry().MarkAttached(d)
	}
	local, tcp := s.Handler(), s.TCPHandler()
	request := func(handler http.Handler, spec string) *httptest.ResponseRecorder {
		t.Helper()
		method, path, _ := strings.Cut(spec, " ")
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request(local, "GET /test-hub").Code; got != http.StatusAccepted {
		t.Fatalf("hub route: %d", got)
	}
	for _, path := range []string{"GET /v1/status", "GET /v1/devices"} {
		for _, conn := range []struct {
			handler http.Handler
			private bool
		}{{local, true}, {tcp, false}} {
			w := request(conn.handler, path)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"first"`) || strings.Contains(w.Body.String(), `"private"`) != conn.private {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body)
			}
		}
	}
	if h.visible != 4 {
		t.Fatalf("Visible called %d times", h.visible)
	}
	h.reject = true
	for _, route := range []struct {
		method, path string
		mode         LeaseMode
	}{
		{"DELETE", "", LeaseIfHeld}, {"POST", "/attach", LeaseNeeded}, {"POST", "/detach", LeaseNeeded}, {"GET", "", LeaseOpen}, {"POST", "/commands/tapOn", LeaseNeeded}, {"POST", "/steps", LeaseNeeded}, {"POST", "/flows", LeaseNeeded}, {"GET", "/screenshot", LeaseNeeded}, {"GET", "/stream", LeaseNeeded}, {"GET", "/hierarchy", LeaseNeeded}, {"GET", "/state", LeaseNeeded}, {"GET", "/info", LeaseOpen}, {"GET", "/vars", LeaseNeeded}, {"PUT", "/vars", LeaseNeeded}, {"POST", "/vars", LeaseNeeded}, {"POST", "/eval", LeaseNeeded}, {"HEAD", "/blobs/hash", LeaseNeeded}, {"PUT", "/blobs/hash", LeaseNeeded}, {"POST", "/apps", LeaseNeeded}, {"DELETE", "/apps/id", LeaseNeeded}, {"GET", "/artifacts", LeaseNeeded}, {"GET", "/artifacts/file", LeaseNeeded},
	} {
		spec := route.method + " /v1/devices/first" + route.path
		before := h.gated
		if w := request(tcp, spec); w.Code != http.StatusTeapot || h.gated != before+1 || h.completed != 0 || h.lastMode != route.mode {
			t.Fatalf("gate on %s: %d, mode %v", spec, w.Code, h.lastMode)
		}
	}
	h.reject = false
	if w := request(local, "GET /v1/devices/first"); w.Code != 200 || h.completed != 1 {
		t.Fatalf("gate completion: %d, completed %d", w.Code, h.completed)
	}
	s.Events().Publish(Event{Type: "test"})
	s.Shutdown("test")
	<-s.Done()
	if h.ended || len(h.events) != 2 || h.events[0].Type != "test" || h.events[1].Type != "daemon.shutdown" {
		t.Fatalf("lifecycle: ended %t, events %+v", h.ended, h.events)
	}
	plain := New(Config{Name: "plain"}, Deps{})
	if got := request(plain.Handler(), "GET /v1/leases"); !strings.Contains(got.Body.String(), "not a hub") {
		t.Fatalf("plain leases: %s", got.Body)
	}
}
