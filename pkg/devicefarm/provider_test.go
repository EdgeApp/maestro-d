package devicefarm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

// hookLog records what Allocate reports through its hooks.
type hookLog struct {
	mu        sync.Mutex
	f         *dftest.Fake
	progress  []string
	created   []string
	abandoned []string
	createErr error
	onCreated func(arn string)
	onState   func(state string)
}

func (l *hookLog) hooks(id string) cloudprov.AllocateHooks {
	return cloudprov.AllocateHooks{
		DeviceID: id,
		Progress: func(s string) {
			l.mu.Lock()
			l.progress = append(l.progress, s)
			l.mu.Unlock()
			if l.onState != nil {
				l.onState(s)
			}
		},
		Created: func(arn string) error {
			l.f.Mark("created")
			l.mu.Lock()
			l.created = append(l.created, arn)
			l.mu.Unlock()
			if l.onCreated != nil {
				l.onCreated(arn)
			}
			return l.createErr
		},
		Abandoned: func(arn string) {
			l.f.Mark("abandoned")
			l.mu.Lock()
			l.abandoned = append(l.abandoned, arn)
			l.mu.Unlock()
		},
	}
}

func newTestProvider(t *testing.T) (*Provider, *dftest.Fake, *hookLog) {
	t.Helper()
	c, f := newTestClient(t)
	return NewProviderFromClient(c), f, &hookLog{f: f}
}

// wantError asserts err is a *cloudprov.Error of kind k whose message holds
// no secret.
func wantError(t *testing.T, err error, k cloudprov.Kind, f *dftest.Fake) *cloudprov.Error {
	t.Helper()
	var ce *cloudprov.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T %v, want *cloudprov.Error", err, err)
	}
	if ce.Kind != k {
		t.Fatalf("kind = %s (%s), want %s", ce.Kind, ce.Msg, k)
	}
	assertClean(t, ce.Msg, f)
	return ce
}

// assertClean fails when s holds a session endpoint token or the key.
func assertClean(t *testing.T, s string, f *dftest.Fake) {
	t.Helper()
	if strings.Contains(s, testKeyID) || strings.Contains(s, testSecret) {
		t.Fatalf("credentials leaked: %q", s)
	}
	for _, sess := range f.Sessions() {
		if strings.Contains(s, sess.Token) {
			t.Fatalf("endpoint token leaked: %q", s)
		}
	}
}

func indexOf(ops []string, op string, from int) int {
	for i := from; i < len(ops); i++ {
		if ops[i] == op {
			return i
		}
	}
	return -1
}

func TestProviderIdentity(t *testing.T) {
	p, f, _ := newTestProvider(t)
	if p.Name() != "devicefarm" || p.IDPrefix() != "df" || p.Client() == nil {
		t.Fatal("identity")
	}
	assertClean(t, p.Redact("key "+testKeyID+" secret "+testSecret+" at "+f.URL()+"/remote-endpoint/abc/wd/hub"), f)
	if strings.Contains(p.Redact("/remote-endpoint/abc"), "abc") {
		t.Fatal("endpoint kept")
	}
}

func TestNewProvider(t *testing.T) {
	pinAWS(t)
	f := dftest.New(t)
	_, err := NewProvider(context.Background(), Options{Endpoint: f.URL()})
	if ce := wantError(t, err, cloudprov.Auth, f); !errors.Is(ce, ErrNoCredentials) {
		t.Fatalf("err = %v", err)
	}
	withKeys(t)
	p, err := NewProvider(context.Background(), Options{Endpoint: f.URL()})
	if err != nil || p == nil {
		t.Fatal(err)
	}
}

func TestProviderAllocateHappyPath(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	a, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android", Name: "Google Pixel 8", AppID: "com.example",
		Caps: map[string]any{"appium:udid": "x", "appium:noReset": true}}, log.hooks("df-0000abcd"))
	if err != nil {
		t.Fatal(err)
	}
	arn := f.CreatedSessions()[0]
	ops := f.Ops()
	ci := indexOf(ops, "CreateRemoteAccessSession", 0)
	if ci < 0 || ops[ci+1] != "mark:created" || ops[ci+2] != "GetRemoteAccessSession" {
		t.Fatalf("order = %v", ops)
	}
	s, _ := f.Session(arn)
	if s.Name != "df-0000abcd" || s.DeviceARN != dftest.ARNPixel8 {
		t.Fatalf("session = %+v", s)
	}
	if a.Ref != arn || a.Endpoint != f.Endpoint(arn) {
		t.Fatalf("allocation ref/endpoint = %q %q", a.Ref, a.Endpoint)
	}
	want := cloudprov.DeviceDesc{Model: "Google Pixel 8", Platform: "android", OSVersion: "14", FormFactor: "phone",
		Manufacturer: "Google", DashboardURL: DashboardURL(arn), AppID: "com.example", AppiumVersion: "2"}
	if a.Device != want || !strings.Contains(a.Device.DashboardURL, "/mobile/projects/") {
		t.Fatalf("device = %+v", a.Device)
	}
	if a.Caps["platformName"] != "Android" || a.Caps["appium:noReset"] != true || a.Caps["appium:udid"] != nil {
		t.Fatalf("caps = %v", a.Caps)
	}
	if a.Started.IsZero() || a.Expires.Sub(a.Started) != 150*time.Minute {
		t.Fatalf("started %v expires %v", a.Started, a.Expires)
	}
	if !reflect.DeepEqual(log.progress, []string{"PENDING", "PREPARING", "RUNNING"}) || len(log.abandoned) != 0 {
		t.Fatalf("progress %v abandoned %v", log.progress, log.abandoned)
	}
	if len(f.StoppedSessions()) != 0 {
		t.Fatalf("stopped = %v", f.StoppedSessions())
	}
	assertClean(t, a.Device.DashboardURL, f)
}

func TestProviderAllocateWithApps(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	project, _ := p.c.Project(ctx)
	seeded := f.SeedUpload(project, "aux.apk", "ANDROID_APP", "SUCCEEDED", time.Now())
	app := writeFile(t, "main.apk", "main app")
	a, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android", Name: "pixel 7", App: app,
		AuxiliaryApps: []string{seeded, writeFile(t, "aux2.apk", "aux two")}}, log.hooks("df-1"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := f.Session(a.Ref)
	if s.AppARN == "" || len(s.AuxiliaryApps) != 2 || s.AuxiliaryApps[0] != seeded || s.DeviceARN != dftest.ARNPixel7 {
		t.Fatalf("session = %+v", s)
	}
	// Apps are resolved before any session exists.
	ops := f.Ops()
	if indexOf(ops, "CreateUpload", 0) > indexOf(ops, "CreateRemoteAccessSession", 0) {
		t.Fatalf("order = %v", ops)
	}
	if _, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android", App: "a.aab"}, log.hooks("df-2")); err == nil {
		t.Fatal("a bad app allocated")
	}
	if _, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android", AuxiliaryApps: []string{"a.txt"}}, log.hooks("df-2")); err == nil {
		t.Fatal("a bad auxiliary app allocated")
	}
	if len(f.CreatedSessions()) != 1 {
		t.Fatalf("sessions = %v", f.CreatedSessions())
	}
}

func TestProviderAllocateCreatedHookError(t *testing.T) {
	p, f, log := newTestProvider(t)
	log.createErr = errors.New("ledger locked")
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "ios"}, log.hooks("df-1"))
	ce := wantError(t, err, cloudprov.Other, f)
	if !strings.Contains(ce.Msg, "cannot record the cloud session; it was stopped") || !strings.Contains(ce.Msg, "ledger locked") {
		t.Fatalf("msg = %q", ce.Msg)
	}
	arn := f.CreatedSessions()[0]
	if !reflect.DeepEqual(f.StoppedSessions(), []string{arn}) || !reflect.DeepEqual(log.abandoned, []string{arn}) {
		t.Fatalf("stopped %v abandoned %v", f.StoppedSessions(), log.abandoned)
	}
	ops := f.Ops()
	ci := indexOf(ops, "mark:created", 0)
	if ops[ci+1] != "StopRemoteAccessSession" || indexOf(ops, "mark:abandoned", ci) < 0 {
		t.Fatalf("order = %v", ops)
	}
	if g := indexOf(ops, "GetRemoteAccessSession", 0); g >= 0 && g < indexOf(ops, "StopRemoteAccessSession", 0) {
		t.Fatalf("polled before stopping: %v", ops)
	}
	// When the stop fails too, the session is not reported abandoned.
	log2 := &hookLog{f: f, createErr: errors.New("ledger locked")}
	f.FailOp("StopRemoteAccessSession", "InternalServiceException", "try later")
	_, err = p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "ios"}, log2.hooks("df-2"))
	ce = wantError(t, err, cloudprov.Other, f)
	if !strings.Contains(ce.Msg, "stopping it failed") || len(log2.abandoned) != 0 {
		t.Fatalf("msg %q abandoned %v", ce.Msg, log2.abandoned)
	}
}

func TestProviderAllocatePendingDeviceMovesOn(t *testing.T) {
	p, f, log := newTestProvider(t)
	f.HoldDevice(dftest.ARNPixel8, "PENDING_DEVICE")
	a, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android", OSVersionMin: "13"}, log.hooks("df-1"))
	if err != nil {
		t.Fatal(err)
	}
	created := f.CreatedSessions()
	if a.Device.Model != "Google Pixel 7" || len(created) != 2 {
		t.Fatalf("model %s created %v", a.Device.Model, created)
	}
	if !reflect.DeepEqual(f.StoppedSessions(), created[:1]) || !reflect.DeepEqual(log.abandoned, created[:1]) {
		t.Fatalf("stopped %v abandoned %v", f.StoppedSessions(), log.abandoned)
	}
}

func TestProviderAllocateThreeBusy(t *testing.T) {
	p, f, log := newTestProvider(t)
	f.Hold("PENDING_DEVICE")
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	ce := wantError(t, err, cloudprov.DeviceBusy, f)
	created := f.CreatedSessions()
	if len(created) != 3 || !strings.Contains(ce.Msg, "Google Pixel 8") {
		t.Fatalf("created %v msg %q", created, ce.Msg)
	}
	if !reflect.DeepEqual(f.StoppedSessions(), created) || !reflect.DeepEqual(log.abandoned, created) ||
		!reflect.DeepEqual(log.created, created) {
		t.Fatalf("stopped %v abandoned %v", f.StoppedSessions(), log.abandoned)
	}
}

func TestProviderAllocatePendingConcurrency(t *testing.T) {
	p, f, log := newTestProvider(t)
	f.Hold("PENDING_CONCURRENCY")
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "ios", BootTimeoutSec: 1}, log.hooks("df-1"))
	ce := wantError(t, err, cloudprov.NoCapacity, f)
	if !strings.Contains(ce.Msg, "PENDING_CONCURRENCY") || !strings.Contains(ce.Msg, "2 metered") {
		t.Fatalf("msg = %q", ce.Msg)
	}
	created := f.CreatedSessions()
	if len(created) != 1 || !reflect.DeepEqual(f.StoppedSessions(), created) || !reflect.DeepEqual(log.abandoned, created) {
		t.Fatalf("created %v stopped %v abandoned %v", created, f.StoppedSessions(), log.abandoned)
	}
	if indexOf(log.progress, "PENDING_CONCURRENCY", 0) < 0 {
		t.Fatalf("progress = %v", log.progress)
	}
}

func TestProviderAllocateEndedBeforeRunning(t *testing.T) {
	p, f, log := newTestProvider(t)
	log.onCreated = func(arn string) { f.ExpireSession(arn) }
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	wantError(t, err, cloudprov.Ended, f)
	// Stopping an already-completed session counts as stopped.
	if len(log.abandoned) != 1 || len(f.StoppedSessions()) != 1 {
		t.Fatalf("abandoned %v stopped %v", log.abandoned, f.StoppedSessions())
	}
}

func TestProviderAllocateWaitFailureAndStopFailure(t *testing.T) {
	p, f, log := newTestProvider(t)
	log.onCreated = func(arn string) {
		f.FailOp("GetRemoteAccessSession", "ArgumentException", "broken "+f.URL()+"/remote-endpoint/x/wd/hub")
		f.FailOp("StopRemoteAccessSession", "InternalServiceException", "try later")
	}
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	ce := wantError(t, err, cloudprov.Other, f)
	if !strings.Contains(ce.Msg, "stays in the cloud ledger") || len(log.abandoned) != 0 || strings.Contains(ce.Msg, "/remote-endpoint/x") {
		t.Fatalf("msg %q abandoned %v", ce.Msg, log.abandoned)
	}
}

func TestProviderAllocateCancelledStopsSession(t *testing.T) {
	p, f, log := newTestProvider(t)
	f.Hold("PENDING_CONCURRENCY")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log.onState = func(s string) {
		if s == "PENDING_CONCURRENCY" {
			cancel()
		}
	}
	_, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	wantError(t, err, cloudprov.Other, f)
	created := f.CreatedSessions()
	if len(created) != 1 || !reflect.DeepEqual(f.StoppedSessions(), created) || !reflect.DeepEqual(log.abandoned, created) {
		t.Fatalf("created %v stopped %v abandoned %v", created, f.StoppedSessions(), log.abandoned)
	}
}

func TestProviderAllocateCreateFails(t *testing.T) {
	p, f, log := newTestProvider(t)
	f.FailCreate("LimitExceededException", "remote access sessions at the limit")
	_, err := p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	wantError(t, err, cloudprov.NoCapacity, f)
	f.FailCreate("AccessDeniedException", "User: u is not authorized to perform: devicefarm:CreateRemoteAccessSession on resource: r")
	_, err = p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	if ce := wantError(t, err, cloudprov.Auth, f); ce.Action != "devicefarm:CreateRemoteAccessSession" {
		t.Fatalf("action = %q", ce.Action)
	}
	if len(log.created) != 0 {
		t.Fatalf("created hook ran: %v", log.created)
	}
	f.FailCreate("", "")
	f.FailOp("ListDevices", "UnrecognizedClientException", "bad token")
	p.c.catalog = map[string]catalogEntry{}
	_, err = p.Allocate(context.Background(), cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	wantError(t, err, cloudprov.Auth, f)
}

func TestProviderAllocateSpecErrors(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	_, err := p.Allocate(ctx, cloudprov.CloudSpec{}, log.hooks("df-1"))
	wantError(t, err, cloudprov.Other, f)
	_, err = p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios", AuxiliaryApps: []string{"a", "b", "c", "d"}}, log.hooks("df-1"))
	wantError(t, err, cloudprov.Other, f)
	_, err = p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android", Name: "Nokia 3310"}, log.hooks("df-1"))
	ce := wantError(t, err, cloudprov.NotFound, f)
	if !strings.Contains(ce.Msg, "closest: Google Pixel") {
		t.Fatalf("msg = %q", ce.Msg)
	}
	_, err = p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios", OSVersionMax: "17.2"}, log.hooks("df-1"))
	wantError(t, err, cloudprov.NotFound, f)
	if len(f.CreatedSessions()) != 0 {
		t.Fatal("a session was created")
	}
}

func TestCandidates(t *testing.T) {
	all := []cloudprov.CloudModel{
		{ID: "df:android:a:14", Platform: "android", Name: "A", OSVersion: "14", Availability: "AVAILABLE"},
		{ID: "df:android:b:13", Platform: "android", Name: "B", OSVersion: "13", Availability: "HIGHLY_AVAILABLE"},
		{ID: "df:android:c:15", Platform: "android", Name: "C", OSVersion: "15", Availability: "BUSY"},
		{ID: "df:android:d:14", Platform: "android", Name: "D", OSVersion: "14", Availability: "AVAILABLE"},
		{ID: "df:android:e:16", Platform: "android", Name: "E", OSVersion: "16", Availability: "TEMPORARY_NOT_AVAILABLE"},
		{ID: "df:android:f:12", Platform: "android", Name: "F", OSVersion: "", Availability: "HIGHLY_AVAILABLE"},
		{ID: "df:android:g:9", Platform: "android", Name: "G", OSVersion: "9", Availability: "OTHER"},
		{ID: "df:ios:i:17.4", Platform: "ios", Name: "I (Pro)", OSVersion: "17.4", Availability: "HIGHLY_AVAILABLE"},
		{ID: "df:ios:j:17.2", Platform: "ios", Name: "J", OSVersion: "17.2", Availability: "HIGHLY_AVAILABLE"},
		{ID: "df:ios:j-plus:18", Platform: "ios", Name: "J Plus", OSVersion: "18", Availability: "HIGHLY_AVAILABLE"},
	}
	names := func(spec cloudprov.CloudSpec) []string {
		got, err := Candidates(all, spec)
		if err != nil {
			return []string{"error: " + string(cloudprov.KindOf(err))}
		}
		var out []string
		for _, m := range got {
			out = append(out, m.Name)
		}
		return out
	}
	for _, tc := range []struct {
		spec cloudprov.CloudSpec
		want []string
	}{
		{cloudprov.CloudSpec{Platform: "android"}, []string{"B", "F", "A", "D", "C", "G"}},
		{cloudprov.CloudSpec{Platform: "android", OSVersion: "14"}, []string{"A", "D"}},
		{cloudprov.CloudSpec{Platform: "android", OSVersionMin: "14"}, []string{"A", "D", "C"}},
		{cloudprov.CloudSpec{Platform: "android", OSVersionMax: "13"}, []string{"B", "G"}},
		{cloudprov.CloudSpec{Platform: "android", Name: "e"}, []string{"error: not-found"}},
		{cloudprov.CloudSpec{Platform: "android", Name: "df:android:d:14"}, []string{"D"}},
		{cloudprov.CloudSpec{Platform: "android", Name: "^[ab]$"}, []string{"B", "A"}},
		{cloudprov.CloudSpec{Platform: "ios", Name: "i (pro)"}, []string{"I (Pro)"}},
		{cloudprov.CloudSpec{Platform: "ios", Name: "(["}, []string{"error: not-found"}},
		{cloudprov.CloudSpec{Platform: "ios", OSVersionMax: "17"}, []string{"I (Pro)", "J"}},
		// An exact name picks only its models, not others its pattern matches.
		{cloudprov.CloudSpec{Platform: "ios", Name: "j"}, []string{"J"}},
		{cloudprov.CloudSpec{Platform: "ios", Name: "^J"}, []string{"J Plus", "J"}},
		{cloudprov.CloudSpec{Platform: "ios", OSVersionMax: "17.2"}, []string{"J"}},
		// osVersion is the version or a prefix of it, as on every lease.
		{cloudprov.CloudSpec{Platform: "ios", OSVersion: "17"}, []string{"I (Pro)", "J"}},
		{cloudprov.CloudSpec{Platform: "ios", OSVersion: "17.2"}, []string{"J"}},
		{cloudprov.CloudSpec{Platform: "ios", OSVersionMin: "17.3", OSVersionMax: "17.4.0"}, []string{"I (Pro)"}},
		{cloudprov.CloudSpec{Platform: "windows"}, []string{"error: not-found"}},
	} {
		if got := names(tc.spec); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.spec, got, tc.want)
		}
	}
	_, err := Candidates(all, cloudprov.CloudSpec{Platform: "android", Name: "Bee", OSVersion: "1", OSVersionMin: "1", OSVersionMax: "2"})
	msg := err.Error()
	if !strings.Contains(msg, `name "Bee"`) || !strings.Contains(msg, "OS >= 1") || !strings.Contains(msg, "closest: B 13") {
		t.Fatalf("msg = %q", msg)
	}
	if n := strings.Count(strings.SplitN(msg, "closest: ", 2)[1], ", "); n != 4 {
		t.Fatalf("closest count = %d: %q", n+1, msg)
	}
	_, err = Candidates(all, cloudprov.CloudSpec{Platform: "windows"})
	if !strings.Contains(err.Error(), "no remote-access windows devices") {
		t.Fatalf("msg = %q", err)
	}
}

func TestDashboardURL(t *testing.T) {
	got := DashboardURL("arn:aws:devicefarm:us-west-2:123456789012:session:proj-id/sess-id/00000")
	if got != "https://us-west-2.console.aws.amazon.com/devicefarm/home?region=us-west-2#/mobile/projects/proj-id/sessions/sess-id" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", "arn:aws:devicefarm:us-west-2:1:project:x", "arn:aws:devicefarm:us-west-2:1:session:only"} {
		if DashboardURL(bad) != "" {
			t.Errorf("%q gave a URL", bad)
		}
	}
}

func TestProviderInstallMakesNoUploadLookup(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	a, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := p.Upload(ctx, writeFile(t, "a.apk", "app"))
	if err != nil {
		t.Fatal(err)
	}
	lists, creates := f.CountOp("ListUploads"), f.CountOp("CreateUpload")
	res, err := p.Install(ctx, a, *app)
	if err != nil || res.AppRef.Ref != app.Ref || res.AppRef.Name != app.Name {
		t.Fatalf("Install = %+v, %v", res, err)
	}
	if f.CountOp("ListUploads") != lists || f.CountOp("CreateUpload") != creates || f.CountOp("GetUpload") != 1 {
		t.Fatalf("install looked uploads up: %v", f.Ops())
	}
	if s, _ := f.Session(a.Ref); !reflect.DeepEqual(s.Installed, []string{app.Ref}) {
		t.Fatalf("installed = %v", s.Installed)
	}
	if _, err := p.Install(ctx, nil, *app); err == nil {
		t.Fatal("nil allocation")
	}
	_, err = p.Install(ctx, a, cloudprov.AppRef{Ref: "arn:aws:devicefarm:us-west-2:1:upload:p/none"})
	wantError(t, err, cloudprov.NotFound, f)
}

func TestProviderRefreshReleaseAndReleaseRef(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	a, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "android"}, log.hooks("df-1"))
	if err != nil {
		t.Fatal(err)
	}
	if st, reason, err := p.Refresh(ctx, a); err != nil || st != "RUNNING" || reason != "" {
		t.Fatalf("Refresh = %s %q %v", st, reason, err)
	}
	res, err := p.Release(ctx, a)
	if err != nil || res.Minutes == nil || res.Minutes.Total < 1 {
		t.Fatalf("Release = %+v, %v", res, err)
	}
	// A second release finds it completed and still reports the minutes.
	res, err = p.Release(ctx, a)
	if err != nil || res.Minutes == nil {
		t.Fatalf("second Release = %+v, %v", res, err)
	}
	if st, reason, err := p.Refresh(ctx, a); err != nil || st != "COMPLETED" || !strings.Contains(reason, "STOPPED") {
		t.Fatalf("Refresh = %s %q %v", st, reason, err)
	}
	if m, err := p.ReleaseRef(ctx, a.Ref); err != nil || m == nil || m.Total == 0 {
		t.Fatalf("ReleaseRef completed: %+v %v", m, err)
	}
	if m, err := p.ReleaseRef(ctx, "arn:aws:devicefarm:us-west-2:1:session:p/gone/00000"); err != nil || m != nil {
		t.Fatalf("ReleaseRef gone: %+v %v", m, err)
	}
	_, err = p.ReleaseRef(ctx, "")
	wantError(t, err, cloudprov.Other, f)
	if _, err := p.Release(ctx, nil); err == nil {
		t.Fatal("nil allocation released")
	}
	if _, _, err := p.Refresh(ctx, nil); err == nil {
		t.Fatal("nil allocation refreshed")
	}
	_, _, err = p.Refresh(ctx, &cloudprov.Allocation{Ref: "arn:aws:devicefarm:us-west-2:1:session:p/gone/00000"})
	wantError(t, err, cloudprov.NotFound, f)

	// Minutes come from GetRemoteAccessSession when Stop does not carry them.
	b, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios"}, log.hooks("df-2"))
	if err != nil {
		t.Fatal(err)
	}
	f.ExpireSession(b.Ref)
	if st, reason, _ := p.Refresh(ctx, b); st != "COMPLETED" || !strings.Contains(reason, "time limit") {
		t.Fatalf("expired: %s %q", st, reason)
	}
	c, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios"}, log.hooks("df-3"))
	if err != nil {
		t.Fatal(err)
	}
	f.OmitStopMinutes(true)
	if res, err := p.Release(ctx, c); err != nil || res.Minutes == nil || res.Minutes.Total < 1 {
		t.Fatalf("Release without stop minutes = %+v, %v", res, err)
	}
	// Minutes Device Farm has not settled are estimated, rounded down.
	now := time.Now()
	if m := estimateMinutes(now.Add(-100*time.Second), now); m.Total != 1.6 || m.Metered != 1.6 || !m.Estimated {
		t.Fatalf("estimate: %+v", m)
	}
	if m := estimateMinutes(now, now.Add(-time.Second)); m.Total != 0 {
		t.Fatalf("a clock step back: %+v", m)
	}
	// A session that never ran has no minutes: the wait for them ends,
	// and the release still stands.
	f.Hold("PENDING_CONCURRENCY")
	arn, err := p.c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNIPhone15, Platform: "ios", OSVersion: "17.4"}, "df-5")
	if err != nil {
		t.Fatal(err)
	}
	// A session that never ran is estimated from its creation.
	if m, err := p.ReleaseRef(ctx, arn); err != nil || m == nil || !m.Estimated {
		t.Fatalf("release of a session that never ran: %+v %v", m, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if m := p.finalMinutes(cctx, arn, nil); m != nil {
		t.Fatalf("finalMinutes on a cancelled context: %+v", m)
	}
	f.Hold("")
	// A running session whose stop fails is not released.
	d, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios"}, log.hooks("df-4"))
	if err != nil {
		t.Fatal(err)
	}
	f.FailOp("StopRemoteAccessSession", "InternalServiceException", "try later")
	_, err = p.ReleaseRef(ctx, d.Ref)
	wantError(t, err, cloudprov.Other, f)
	f.FailOp("GetRemoteAccessSession", "NotFoundException", "gone")
	if m, err := p.ReleaseRef(ctx, d.Ref); err != nil || m != nil {
		t.Fatalf("stop failed, session gone: %+v %v", m, err)
	}
}

func TestProviderUtilities(t *testing.T) {
	p, f, log := newTestProvider(t)
	ctx := context.Background()
	models, err := p.Catalog(ctx, "ios")
	if err != nil || len(models) != 2 {
		t.Fatalf("Catalog = %v, %v", models, err)
	}
	ref, err := p.Upload(ctx, writeFile(t, "a.ipa", "ios"))
	if err != nil || ref.Platform != "ios" {
		t.Fatalf("Upload = %+v, %v", ref, err)
	}
	apps, err := p.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Ref != ref.Ref {
		t.Fatalf("Apps = %+v, %v", apps, err)
	}
	acct, err := p.Account(ctx)
	if err != nil || acct["trialMinutes"] == nil {
		t.Fatalf("Account = %v, %v", acct, err)
	}
	a, err := p.Allocate(ctx, cloudprov.CloudSpec{Platform: "ios", Name: "iphone 14"}, log.hooks("df-77"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := f.Session(a.Ref); s.Parameters["appium:version"] != "3" {
		t.Fatalf("iOS 27 parameters = %v", s.Parameters)
	}
	p.Release(ctx, a)
	sessions, err := p.Sessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("Sessions = %+v, %v", sessions, err)
	}
	s := sessions[0]
	if s.Name != "df-77" || s.Ref != a.Ref || s.Status != "COMPLETED" || s.Result != "STOPPED" ||
		s.Model != "Apple iPhone 14" || s.Created == "" || s.DeviceMinutes == nil {
		t.Fatalf("session = %+v", s)
	}

	for op, call := range map[string]func() error{
		"ListDevices":              func() error { p.c.catalog = map[string]catalogEntry{}; _, err := p.Catalog(ctx, ""); return err },
		"ListUploads":              func() error { _, err := p.Apps(ctx); return err },
		"GetAccountSettings":       func() error { _, err := p.Account(ctx); return err },
		"ListRemoteAccessSessions": func() error { _, err := p.Sessions(ctx); return err },
		"CreateUpload":             func() error { _, err := p.Upload(ctx, writeFile(t, "n.apk", "new")); return err },
	} {
		f.FailOp(op, "AccessDeniedException", "User: u is not authorized to perform: devicefarm:"+op+" on resource: "+f.URL()+"/remote-endpoint/tok/wd/hub with "+testKeyID)
		ce := wantError(t, call(), cloudprov.Auth, f)
		if ce.Action != "devicefarm:"+op || strings.Contains(ce.Msg, "/remote-endpoint/tok") {
			t.Errorf("%s: %+v", op, ce)
		}
		f.FailOp(op, "", "")
	}
}

func TestProviderAppiumVersion(t *testing.T) {
	p, _, log := newTestProvider(t)
	ctx := context.Background()
	for _, c := range []struct {
		spec cloudprov.CloudSpec
		want string
	}{
		{cloudprov.CloudSpec{Platform: "android", Name: "Google Pixel 8"}, "2"},
		{cloudprov.CloudSpec{Platform: "ios", OSVersionMin: "27"}, "3"},
		{cloudprov.CloudSpec{Platform: "ios", Name: "Apple iPhone 15", AppiumVersion: "3"}, "3"},
	} {
		a, err := p.Allocate(ctx, c.spec, log.hooks("df-av"))
		if err != nil {
			t.Fatal(err)
		}
		if a.Device.AppiumVersion != c.want {
			t.Errorf("%+v: Appium %q, want %q", c.spec, a.Device.AppiumVersion, c.want)
		}
		if _, err := p.Release(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
}
