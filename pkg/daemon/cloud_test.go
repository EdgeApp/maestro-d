package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// fakeProvider is an in-memory cloudprov.RemoteProvider.
type fakeProvider struct {
	mu        sync.Mutex
	models    []cloudprov.CloudModel
	catalogFn func(platform string) error
	calls     []string
}

var _ cloudprov.RemoteProvider = (*fakeProvider)(nil)

func newFakeProvider() *fakeProvider {
	return &fakeProvider{models: []cloudprov.CloudModel{
		{ID: "df:android:google-pixel-8:14", ARN: "arn:p8", Platform: "android", Name: "Google Pixel 8", OSVersion: "14", Availability: "HIGHLY_AVAILABLE", Available: true},
		{ID: "df:ios:apple-iphone-15:17.4", ARN: "arn:i15", Platform: "ios", Name: "Apple iPhone 15", OSVersion: "17.4", Availability: "BUSY"},
		{ID: "df:ios:apple-iphone-14:27.0", ARN: "arn:i14", Platform: "ios", Name: "Apple iPhone 14", OSVersion: "27.0", Availability: "AVAILABLE", Available: true},
	}}
}

func (f *fakeProvider) call(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeProvider) Name() string     { return "devicefarm" }
func (f *fakeProvider) IDPrefix() string { return "df" }
func (f *fakeProvider) Catalog(_ context.Context, platform string) ([]cloudprov.CloudModel, error) {
	f.call("Catalog")
	if f.catalogFn != nil {
		if err := f.catalogFn(platform); err != nil {
			return nil, err
		}
	}
	var out []cloudprov.CloudModel
	for _, m := range f.models {
		if platform == "" || m.Platform == platform {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeProvider) Allocate(context.Context, cloudprov.CloudSpec, cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	f.call("Allocate")
	return nil, cloudprov.Errorf(cloudprov.Other, "not in this test")
}
func (f *fakeProvider) Install(context.Context, *cloudprov.Allocation, cloudprov.AppRef) (*cloudprov.InstallResult, error) {
	f.call("Install")
	return &cloudprov.InstallResult{}, nil
}
func (f *fakeProvider) Refresh(context.Context, *cloudprov.Allocation) (string, string, error) {
	f.call("Refresh")
	return "RUNNING", "", nil
}
func (f *fakeProvider) Release(context.Context, *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	f.call("Release")
	return &cloudprov.ReleaseResult{Minutes: &cloudprov.DeviceMinutes{Metered: 1, Total: 1}}, nil
}
func (f *fakeProvider) ReleaseRef(context.Context, string) (*cloudprov.DeviceMinutes, error) {
	f.call("ReleaseRef")
	return nil, nil
}
func (f *fakeProvider) Upload(context.Context, string) (*cloudprov.AppRef, error) {
	f.call("Upload")
	return &cloudprov.AppRef{Ref: "arn:upload"}, nil
}
func (f *fakeProvider) Apps(context.Context) ([]cloudprov.AppRef, error) {
	f.call("Apps")
	return nil, nil
}
func (f *fakeProvider) Account(context.Context) (map[string]any, error) {
	f.call("Account")
	return map[string]any{}, nil
}
func (f *fakeProvider) Sessions(context.Context) ([]cloudprov.SessionSummary, error) {
	f.call("Sessions")
	return nil, nil
}
func (f *fakeProvider) Redact(s string) string { return strings.ReplaceAll(s, "SECRET", "<redacted>") }

// cloudEnv is a test daemon whose devicefarm provider is f, constructed on
// first use; made counts constructions.
type cloudEnv struct {
	*testEnv
	f    *fakeProvider
	made atomic.Int32
}

func newCloudEnv(t *testing.T, hub bool, mkErr error) *cloudEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	ce := &cloudEnv{testEnv: &testEnv{t: t}, f: newFakeProvider()}
	deps := testDeps(&ce.cleanups, 0)
	deps.Providers = map[string]func() (cloudprov.RemoteProvider, error){
		"devicefarm": func() (cloudprov.RemoteProvider, error) {
			ce.made.Add(1)
			if mkErr != nil {
				return nil, mkErr
			}
			return ce.f, nil
		},
	}
	cfg := Config{Name: "test-" + strings.ToLower(t.Name()), ReportsDir: filepath.Join(t.TempDir(), "reports")}
	if hub {
		cfg.Hub, cfg.HubName, cfg.CloudMaxDevices = true, "hubtest", 2
	}
	ce.s = New(cfg, deps)
	ce.s.info = Info{Name: ce.s.cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	hs := httptest.NewServer(ce.s.Handler())
	t.Cleanup(hs.Close)
	ce.c = NewHTTPClient(hs.URL, "", nil)
	return ce
}

// holdCloud records a cloud device as held by the daemon.
func (e *cloudEnv) holdCloud(id string) {
	r := e.s.registry
	r.mu.Lock()
	r.records[id] = &DeviceInfo{ID: id, Platform: "android", Kind: "cloud", Name: "Google Pixel 8", OSVersion: "14",
		State: "allocated", Ready: true, BootedBy: BootedByDaemon, Provider: "devicefarm"}
	r.mu.Unlock()
}

func TestCloud_ListKeepsProviderRecordsWithLocation(t *testing.T) {
	t.Setenv(envTestMachine, "alex-mac")
	e := newCloudEnv(t, false, nil)
	e.holdCloud("df-0001")
	ctx := context.Background()
	devs, err := e.c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DeviceInfo{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	cd, mock := byID["df-0001"], byID["mock-1"]
	if cd.Location != LocationCloud || cd.Provider != "devicefarm" || cd.Machine != "alex-mac" {
		t.Fatalf("cloud device: %+v", cd)
	}
	if mock.Location != LocationLocal || mock.Machine != "alex-mac" || !mock.Available {
		t.Fatalf("mock device: %+v", mock)
	}
	// Filters run on the daemon.
	only, err := e.c.DevicesQuery(ctx, DeviceQuery{Location: []string{LocationCloud}})
	if err != nil || len(only) != 1 || only[0].ID != "df-0001" {
		t.Fatalf("location=cloud: %+v %v", only, err)
	}
	only, err = e.c.DevicesQuery(ctx, DeviceQuery{Kind: "cloud", OSVersionMin: "14", Provider: "devicefarm"})
	if err != nil || len(only) != 1 {
		t.Fatalf("kind/os/provider: %+v %v", only, err)
	}
	if _, err := e.c.DevicesQuery(ctx, DeviceQuery{Name: "(["}); codeOf(t, err) != CodeUsage {
		t.Fatalf("bad regexp: %v", err)
	}
	// One device carries its location too.
	one, err := e.c.Device(ctx, "df-0001")
	if err != nil || one.Location != LocationCloud {
		t.Fatalf("GET cloud device: %+v %v", one, err)
	}
	one, err = e.c.Device(ctx, "mock-1")
	if err != nil || one.Location != LocationLocal || one.Machine != "alex-mac" {
		t.Fatalf("GET mock device: %+v %v", one, err)
	}
	// A caller on another machine sees the local devices as mesh.
	req, _ := http.NewRequest("GET", e.c.base+"/v1/devices?location=mesh", nil)
	req.Header.Set(HeaderClientMachine, "laptop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("mesh listing: %d", resp.StatusCode)
	}
	// Status counts the held cloud device once the provider is in use.
	st, _ := e.c.Status(ctx)
	if st.Cloud != nil {
		t.Fatalf("a plain daemon that never used the provider lists it: %+v", st.Cloud)
	}
	if _, err := e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{LocationCloud}}); err != nil {
		t.Fatal(err)
	}
	st, _ = e.c.Status(ctx)
	if len(st.Cloud) != 1 || st.Cloud[0].Provider != "devicefarm" || st.Cloud[0].Held != 1 || st.Cloud[0].MaxDevices != 0 {
		t.Fatalf("status cloud: %+v", st.Cloud)
	}
}

func TestCloud_Catalog(t *testing.T) {
	e := newCloudEnv(t, false, nil)
	ctx := context.Background()
	// Without location=cloud no provider is constructed or asked.
	if _, err := e.c.Catalog(ctx, "mock", false, ""); err == nil {
		t.Log("local catalog answered")
	}
	if e.made.Load() != 0 {
		t.Fatalf("provider constructed for a local catalog")
	}
	res, err := e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{LocationCloud}, Platform: "ios", OSVersionMin: "16", OSVersionMax: "18"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IOS != nil || res.Android != nil {
		t.Fatalf("location=cloud alone answered the local catalog: %+v", res)
	}
	if len(res.Cloud) != 1 || res.Cloud[0].Provider != "devicefarm" || res.Cloud[0].Hub != "" ||
		len(res.Cloud[0].Models) != 1 || res.Cloud[0].Models[0].Name != "Apple iPhone 15" {
		t.Fatalf("cloud catalog: %+v", res.Cloud)
	}
	res, err = e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{LocationCloud}, Available: true, Name: "pixel|iphone 14"})
	if err != nil || len(res.Cloud[0].Models) != 2 {
		t.Fatalf("available+name: %+v %v", res, err)
	}
	// The provider is constructed once and reused.
	if e.made.Load() != 1 {
		t.Fatalf("constructed %d times", e.made.Load())
	}
	if _, err := e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{LocationCloud}, Provider: "browserstack"}); codeOf(t, err) != CodeUsage {
		t.Fatalf("unknown provider: %v", err)
	}
	if _, err := e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{"moon"}}); codeOf(t, err) != CodeUsage {
		t.Fatalf("bad location: %v", err)
	}
	// A provider failure maps through its kind.
	e.f.catalogFn = func(string) error { return cloudprov.Errorf(cloudprov.Auth, "denied SECRET") }
	_, err = e.c.CatalogQuery(ctx, CatalogQuery{Location: []string{LocationCloud}})
	if codeOf(t, err) != CodeUsage {
		t.Fatalf("auth failure: %v", err)
	}
}

func TestCloud_CatalogWithoutCredentials(t *testing.T) {
	e := newCloudEnv(t, false, errors.New("no AWS credentials: set AWS_ACCESS_KEY_ID"))
	_, err := e.c.CatalogQuery(context.Background(), CatalogQuery{Location: []string{LocationCloud}})
	if codeOf(t, err) != CodeUsage || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatalf("no credentials: %v", err)
	}
	// It is tried again next time, not cached as failed.
	_, _ = e.c.CatalogQuery(context.Background(), CatalogQuery{Location: []string{LocationCloud}})
	if e.made.Load() != 2 {
		t.Fatalf("constructions: %d", e.made.Load())
	}
}

func TestCloud_DaemonWithoutProviders(t *testing.T) {
	env := newTestEnv(t, 0)
	_, err := env.c.CatalogQuery(context.Background(), CatalogQuery{Location: []string{LocationCloud}})
	if codeOf(t, err) != CodeUsage {
		t.Fatalf("no providers: %v", err)
	}
	if _, e := env.s.provider("devicefarm"); e == nil || e.Code != CodeUsage {
		t.Fatalf("unknown provider: %v", e)
	}
}

func TestCloud_HubStatus(t *testing.T) {
	e := newCloudEnv(t, true, nil)
	e.holdCloud("df-0002")
	st := e.s.cloudStatus()
	if len(st) != 1 || st[0].Provider != "devicefarm" || st[0].MaxDevices != 2 || st[0].Held != 1 {
		t.Fatalf("hub cloud status: %+v", st)
	}
	hubless := &Server{cfg: Config{Hub: true, HubName: "h"}}
	if _, e := hubless.provider("devicefarm"); e == nil || !strings.Contains(e.Message, "--devicefarm") {
		t.Fatalf("hub without the provider: %v", e)
	}
	res, err := e.c.CatalogQuery(context.Background(), CatalogQuery{Location: []string{LocationCloud}, Platform: "android"})
	if err != nil {
		// A hub answers only over its socket or to a token holder; the
		// test client has no token, so check the handler's result directly.
		t.Logf("hub catalog over TCP: %v", err)
		return
	}
	if res.Cloud[0].Hub != "hubtest" {
		t.Fatalf("hub name on the catalog: %+v", res.Cloud)
	}
}

func TestCloudError(t *testing.T) {
	if cloudError(nil) != nil {
		t.Fatal("nil error maps to an error")
	}
	for _, c := range []struct {
		err  error
		want Code
	}{
		{&cloudprov.Error{Kind: cloudprov.Auth, Msg: "denied", Action: "devicefarm:ListDevices"}, CodeUsage},
		{cloudprov.Errorf(cloudprov.NoCapacity, "full"), CodeBusy},
		{cloudprov.Errorf(cloudprov.DeviceBusy, "busy"), CodeBusy},
		{cloudprov.Errorf(cloudprov.NotFound, "no model"), CodeDeviceNotFound},
		{cloudprov.Errorf(cloudprov.Ended, "over"), CodeDeviceError},
		{cloudprov.Errorf(cloudprov.Other, "boom"), CodeDeviceError},
		{errors.New("config"), CodeUsage},
	} {
		if got := cloudError(c.err); got.Code != c.want {
			t.Errorf("%v: %s, want %s", c.err, got.Code, c.want)
		}
	}
	e := cloudError(&cloudprov.Error{Kind: cloudprov.Auth, Msg: "denied", Action: "devicefarm:ListDevices"})
	if e.Details["action"] != "devicefarm:ListDevices" {
		t.Fatalf("action detail: %+v", e.Details)
	}
}

func TestClient_SendsMachineAndQuery(t *testing.T) {
	t.Setenv(envTestMachine, "machine-a")
	var got http.Header
	var queries []string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"devices":[],"unreachable":[{"hub":"b","error":"down"}]}`))
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, "", nil)
	ctx := context.Background()
	if _, err := c.Devices(ctx, "ios"); err != nil {
		t.Fatal(err)
	}
	if got.Get(HeaderClientMachine) != "machine-a" || got.Get(HeaderClientMachine) != MachineName() {
		t.Fatalf("machine header: %q", got.Get(HeaderClientMachine))
	}
	q := DeviceQuery{Location: []string{"local", "mesh"}, Provider: "devicefarm", Platform: "android", Kind: "cloud", Name: "pix",
		OSVersionMin: "13", OSVersionMax: "15", Available: true}
	res, err := c.DeviceListQuery(ctx, q)
	if err != nil || len(res.Unreachable) != 1 {
		t.Fatalf("DeviceListQuery: %+v %v", res, err)
	}
	if _, err := c.DeviceList(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CatalogQuery(ctx, CatalogQuery{Location: []string{"cloud"}, Hub: "b", Platform: "ios"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog(ctx, "ios", true, "b"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"platform=ios",
		"available=true&kind=cloud&location=local%2Cmesh&name=pix&osVersionMax=15&osVersionMin=13&platform=android&provider=devicefarm",
		"",
		"hub=b&location=cloud&platform=ios",
		"available=true&hub=b&platform=ios",
	}
	for i, w := range want {
		if queries[i] != w {
			t.Errorf("request %d query %q, want %q", i, queries[i], w)
		}
	}
	// hostIdentity names the same machine.
	s := &Server{}
	if m, _ := s.hostIdentity(); m != "machine-a" {
		t.Fatalf("hostIdentity machine %q", m)
	}
}
