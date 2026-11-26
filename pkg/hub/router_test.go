package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestRouterConfigLocalOnly(t *testing.T) {
	a, b := twoHubs(t)
	path := "/v1/config/devices/b1@hub-b"
	for _, token := range []string{testToken, testPeerToken, ""} {
		status, data := a.do(t, http.MethodPut, path, `{}`, "Authorization", "Bearer "+token)
		if status != http.StatusForbidden || codeIn(data) != daemon.CodeForbidden {
			t.Fatalf("TCP config write: %d %s", status, data)
		}
	}
	// A peer submitting an inbound request reaches the same TCP guard.
	resp := httptest.NewRecorder()
	a.mesh.serveForwarded(resp, httptest.NewRequest(http.MethodGet, "/v1/config/devices", nil), "hub-b")
	if resp.Code != http.StatusForbidden || codeIn(resp.Body.Bytes()) != daemon.CodeForbidden {
		t.Fatalf("peer config read: %d %s", resp.Code, resp.Body)
	}

	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != path {
			return false
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	})
	resp = httptest.NewRecorder()
	a.mesh.unixRouter.Load().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, path, strings.NewReader("{}")))
	if resp.Code != http.StatusNoContent || a.count("PUT "+path) != 1 || b.count("PUT "+path) != 0 || b.logged("forwarded by hub-a: PUT /v1/config") != 0 {
		t.Fatalf("config write routed to wrong handler: HTTP %d", resp.Code)
	}
}

func TestPeerListsAddedConfigDevice(t *testing.T) {
	a, b := twoHubs(t, withShare(t, "b1"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := a.client()
	hasB2 := func() bool {
		t.Helper()
		devices, err := client.Devices(ctx, "mock")
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(devices, func(d daemon.DeviceInfo) bool { return d.ID == "b2@hub-b" })
	}
	if hasB2() {
		t.Fatal("b2 was shared before PUT")
	}
	resp := httptest.NewRecorder()
	b.mesh.unixRouter.Load().ServeHTTP(resp, httptest.NewRequest("PUT", "/v1/config/devices/b2", strings.NewReader(`{"description":"bench"}`)))
	if resp.Code != http.StatusCreated {
		t.Fatalf("local PUT: %d %s", resp.Code, resp.Body)
	}
	for deadline := time.Now().Add(2 * time.Second); !hasB2(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("peer did not list b2 after its cached response expired")
		}
	}
}
