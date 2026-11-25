package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestCollectionLeaseCredentials(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease := r.Header.Get(HeaderLease)
		calls = append(calls, lease)
		if r.URL.Path != "/v1/devices" || r.URL.Query().Get("platform") != "mock" {
			t.Errorf("unexpected query %s", r.URL)
		}
		result := DevicesResult{Devices: []DeviceInfo{{ID: "public"}}, Unreachable: []UnreachablePeer{{Hub: "offline"}}}
		if lease != "" {
			result.Devices = append(result.Devices, DeviceInfo{ID: lease})
			result.Unreachable = append(result.Unreachable, UnreachablePeer{Hub: lease})
		}
		json.NewEncoder(w).Encode(result)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, "", nil)
	c.SetLease("device-a", "aa")
	c.StoredLeases = func() []string { return []string{"", "aa", "bb"} }
	got, err := c.DeviceList(context.Background(), "mock")
	if err != nil {
		t.Fatal(err)
	}
	var ids, hubs []string
	for _, d := range got.Devices {
		ids = append(ids, d.ID)
	}
	for _, p := range got.Unreachable {
		hubs = append(hubs, p.Hub)
	}
	if !slices.Equal(ids, []string{"public", "aa", "bb"}) || !slices.Equal(hubs, []string{"offline", "aa", "bb"}) {
		t.Fatalf("merged %v, %v", ids, hubs)
	}
	if _, err := NewHTTPClient(srv.URL, "", nil).Devices(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"aa", "bb", ""}) {
		t.Fatalf("request credentials: %v", calls)
	}
}
