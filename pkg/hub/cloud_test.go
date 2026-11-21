package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// stubProvider answers the catalog with one model; the rest is unused here.
type stubProvider struct{}

func (stubProvider) Name() string     { return "devicefarm" }
func (stubProvider) IDPrefix() string { return "df" }
func (stubProvider) Catalog(context.Context, string) ([]cloudprov.CloudModel, error) {
	return []cloudprov.CloudModel{{ID: "df:android:google-pixel-8:14", Platform: "android", Name: "Google Pixel 8", OSVersion: "14", Available: true}}, nil
}
func (stubProvider) Allocate(context.Context, cloudprov.CloudSpec, cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	return nil, cloudprov.Errorf(cloudprov.Other, "stub")
}
func (stubProvider) Install(context.Context, *cloudprov.Allocation, cloudprov.AppRef) (*cloudprov.InstallResult, error) {
	return nil, cloudprov.Errorf(cloudprov.Other, "stub")
}
func (stubProvider) Refresh(context.Context, *cloudprov.Allocation) (string, string, error) {
	return "", "", nil
}
func (stubProvider) Release(context.Context, *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	return &cloudprov.ReleaseResult{}, nil
}
func (stubProvider) ReleaseRef(context.Context, string) (*cloudprov.DeviceMinutes, error) {
	return nil, nil
}
func (stubProvider) Upload(context.Context, string) (*cloudprov.AppRef, error) { return nil, nil }
func (stubProvider) Apps(context.Context) ([]cloudprov.AppRef, error)          { return nil, nil }
func (stubProvider) Account(context.Context) (map[string]any, error)           { return nil, nil }
func (stubProvider) Sessions(context.Context) ([]cloudprov.SessionSummary, error) {
	return nil, nil
}
func (stubProvider) Redact(s string) string { return s }

func TestCloud_HelloCarriesProvidersAndHubStatus(t *testing.T) {
	a, b := twoHubs(t, withCloud(1))
	ctx := context.Background()
	st, err := a.client().Hub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Hub.Peers) != 1 || !slices.Equal(st.Hub.Peers[0].Cloud, []string{"devicefarm"}) || st.Hub.Cloud != nil {
		t.Fatalf("a's view: %+v cloud %+v", st.Hub.Peers, st.Hub.Cloud)
	}
	st, err = b.client().Hub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hub.Peers[0].Cloud != nil || len(st.Hub.Cloud) != 1 || st.Hub.Cloud[0].Provider != "devicefarm" || st.Hub.Cloud[0].MaxDevices != 1 {
		t.Fatalf("b's view: %+v cloud %+v", st.Hub.Peers, st.Hub.Cloud)
	}
	if got := a.mesh.cloudPeers(); len(got) != 1 || got[0].Hub != "hub-b" {
		t.Fatalf("cloudPeers: %+v", got)
	}
}

func TestCloud_CatalogRoutedToTheHubWithCredentials(t *testing.T) {
	a, _ := twoHubs(t, withCloud(2))
	ctx := context.Background()
	c := a.client()
	// Asked at a, answered by b: the launch routing rule, not a fan-out.
	res, err := c.CatalogQuery(ctx, daemon.CatalogQuery{Location: []string{"cloud"}, Platform: "android"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cloud) != 1 || res.Cloud[0].Hub != "hub-b" || len(res.Cloud[0].Models) != 1 {
		t.Fatalf("routed catalog: %+v", res.Cloud)
	}
	// Naming a hub sends it there: a, which has no credentials, says so.
	_, err = c.CatalogQuery(ctx, daemon.CatalogQuery{Location: []string{"cloud"}, Hub: "hub-a"})
	var de *daemon.Error
	if !asErr(err, &de) || de.Code != daemon.CodeUsage || !strings.Contains(de.Message, "--devicefarm") {
		t.Fatalf("named hub without credentials: %v", err)
	}
}

// A request for the local and cloud catalogs at a hub without credentials:
// the local sections are that hub's own, the cloud section is the routed
// hub's.
func TestCloud_CatalogMixedLocations(t *testing.T) {
	a, b := twoHubs(t, withCloud(2))
	// The harness hubs have no Android SDK, so a's own section answers
	// that; what matters is who served which part.
	_, err := a.client().CatalogQuery(context.Background(), daemon.CatalogQuery{Location: []string{"local", "cloud"}, Platform: "android"})
	if err == nil || !strings.Contains(err.Error(), "no Android catalog configured") {
		t.Fatalf("a's local section: %v", err)
	}
	if a.count("GET /v1/catalog") != 1 || b.count("GET /v1/catalog") != 1 {
		t.Fatalf("a served %d, b served %d", a.count("GET /v1/catalog"), b.count("GET /v1/catalog"))
	}
}

func TestCloud_CatalogWithNoHubWithCredentials(t *testing.T) {
	a, _ := twoHubs(t)
	_, err := a.client().CatalogQuery(context.Background(), daemon.CatalogQuery{Location: []string{"cloud"}})
	var de *daemon.Error
	if !asErr(err, &de) || de.Code != daemon.CodeUsage || de.Message != "no hub in this mesh has Device Farm credentials" {
		t.Fatalf("no hub with credentials: %v", err)
	}
}

func TestCloud_LocationComputedAfterTheMerge(t *testing.T) {
	a, b := twoHubs(t)
	var mu sync.Mutex
	var peerQueries []string
	b.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/devices" {
			mu.Lock()
			peerQueries = append(peerQueries, r.URL.RawQuery)
			mu.Unlock()
		}
		return false
	})
	list := func(query, machine string) []daemon.DeviceInfo {
		t.Helper()
		code, body := a.do(t, "GET", "/v1/devices?"+query, nil, daemon.HeaderClientMachine, machine)
		if code != 200 {
			t.Fatalf("list %s: %d %s", query, code, body)
		}
		var res daemon.DevicesResult
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
		return res.Devices
	}
	// Both hubs run on this host, so a caller on it sees every device as
	// local, and a caller elsewhere sees them all as mesh.
	here := daemon.MachineName()
	devs := list("", here)
	if len(devs) != 4 {
		t.Fatalf("merged list: %+v", devs)
	}
	for _, d := range devs {
		if d.Location != daemon.LocationLocal || d.Machine == "" {
			t.Fatalf("%s: %q on %q", d.ID, d.Location, d.Machine)
		}
	}
	for _, d := range list("", "laptop") {
		if d.Location != daemon.LocationMesh {
			t.Fatalf("%s from elsewhere: %q", d.ID, d.Location)
		}
	}
	// location filters after the merge; the peer never sees it, and gets
	// the other filters.
	if got := list("location=mesh&name=b1", "laptop"); len(got) != 1 || got[0].ID != "b1@hub-b" {
		t.Fatalf("location=mesh&name=b1: %+v", got)
	}
	if got := list("location=local", "laptop"); len(got) != 0 {
		t.Fatalf("location=local from elsewhere: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peerQueries) == 0 {
		t.Fatal("the peer was never asked")
	}
	for _, q := range peerQueries {
		if strings.Contains(q, "location=") {
			t.Fatalf("the peer got location: %q", q)
		}
	}
	if !slices.ContainsFunc(peerQueries, func(q string) bool { return strings.Contains(q, "name=b1") }) {
		t.Fatalf("the peer did not get name: %v", peerQueries)
	}
	code, body := a.do(t, "GET", "/v1/devices?location=moon", nil)
	if code == 200 || codeIn(body) != daemon.CodeUsage {
		t.Fatalf("bad location: %d %s", code, body)
	}
}

func asErr(err error, target **daemon.Error) bool {
	e, ok := err.(*daemon.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestCloud_LeaseRoutedToTheHubWithCredentials(t *testing.T) {
	a, b := twoHubs(t, withCloud(2))
	ctx := context.Background()
	c := a.client()
	sel := &daemon.LeaseSelector{Location: "cloud", Platform: "android"}
	// Asked at a, tried once at b (the harness gives b no driver factory,
	// so its launch fails): b's answer is the client's, and a never tries
	// its own devices.
	before := a.count("POST /v1/leases")
	_, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: sel})
	var de *daemon.Error
	if !asErr(err, &de) || de.Code != daemon.CodeInternal || !strings.Contains(de.Message, "cloud devices are not configured") {
		t.Fatalf("routed lease: %v", err)
	}
	if b.count("POST /v1/leases") != 1 || a.count("POST /v1/leases") != before {
		t.Fatalf("tries: a %d b %d", a.count("POST /v1/leases")-before, b.count("POST /v1/leases"))
	}
	// Naming a sends it to a, which has no credentials.
	named := *sel
	named.Hub = "hub-a"
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &named}); !asErr(err, &de) || de.Code != daemon.CodeUsage {
		t.Fatalf("named hub: %v", err)
	}
}

func TestCloud_LeaseWithNoHubWithCredentials(t *testing.T) {
	a, _ := twoHubs(t)
	_, err := a.client().AcquireLease(context.Background(), daemon.LeaseRequest{Select: &daemon.LeaseSelector{Location: "cloud", Platform: "ios"}})
	var de *daemon.Error
	if !asErr(err, &de) || de.Code != daemon.CodeUsage || !strings.Contains(de.Message, "no hub in this mesh") {
		t.Fatalf("no hub: %v", err)
	}
}
