package devicefarm

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/devicefarm/dftest"
)

func TestAppiumVersionDefault(t *testing.T) {
	for _, tc := range []struct{ platform, os, want string }{
		{"ios", "27.0", "3"}, {"iOS", "28", "3"}, {"ios", "17.4", "2"}, {"ios", "", "2"},
		{"android", "27", "2"}, {"android", "14", "2"},
	} {
		if got := AppiumVersion(tc.platform, tc.os); got != tc.want {
			t.Errorf("AppiumVersion(%s, %s) = %s", tc.platform, tc.os, got)
		}
	}
}

func TestCreateFields(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	project, _ := c.Project(ctx)
	app := f.SeedUpload(project, "a.apk", "ANDROID_APP", "SUCCEEDED", time.Now())
	arn, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8, Platform: "android", OSVersion: "14",
		AppARN: app, AuxiliaryApps: []string{"aux1", "aux2"}}, "df-1234abcd")
	if err != nil {
		t.Fatal(err)
	}
	body := lastBody(t, f, "CreateRemoteAccessSession")
	want := map[string]any{
		"projectArn": project, "deviceArn": dftest.ARNPixel8, "appArn": app, "name": "df-1234abcd",
		"configuration": map[string]any{
			"billingMethod": "METERED",
			"auxiliaryApps": []any{"aux1", "aux2"},
			"parameters":    map[string]any{"appium:version": "2"},
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %#v", body)
	}
	if s, _ := f.Session(arn); s.Name != "df-1234abcd" || s.BillingMethod != "METERED" {
		t.Fatalf("session = %+v", s)
	}
	// iOS 27 defaults to Appium 3; an explicit version wins; no name, no app.
	if _, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNIPhone14, Platform: "ios", OSVersion: "27.0"}, ""); err != nil {
		t.Fatal(err)
	}
	body = lastBody(t, f, "CreateRemoteAccessSession")
	if v := body["configuration"].(map[string]any)["parameters"].(map[string]any)["appium:version"]; v != "3" {
		t.Fatalf("ios 27 version = %v", v)
	}
	if _, ok := body["name"]; ok {
		t.Fatalf("empty name sent: %v", body)
	}
	if _, ok := body["appArn"]; ok {
		t.Fatalf("empty app sent: %v", body)
	}
	if _, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8, Platform: "android", OSVersion: "14", AppiumVersion: "3"}, "x"); err != nil {
		t.Fatal(err)
	}
	body = lastBody(t, f, "CreateRemoteAccessSession")
	if v := body["configuration"].(map[string]any)["parameters"].(map[string]any)["appium:version"]; v != "3" {
		t.Fatalf("override = %v", v)
	}
}

func TestWaitRunsAndReportsProgress(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	arn, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8, Platform: "android"}, "df-1")
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	s, err := c.Wait(ctx, arn, time.Second, func(st string) { states = append(states, st) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(states, []string{"PENDING", "PREPARING", "RUNNING"}) {
		t.Fatalf("states = %v", states)
	}
	if s.Endpoint != f.Endpoint(arn) || !strings.Contains(s.Endpoint, "/remote-endpoint/") || s.Started.IsZero() {
		t.Fatalf("session = %+v", s)
	}
	if s.DeviceName != "Google Pixel 8" || s.DeviceARN != dftest.ARNPixel8 || s.Name != "df-1" {
		t.Fatalf("session = %+v", s)
	}
	m, err := c.Stop(ctx, arn)
	if err != nil || m == nil || m.Total < 1 || m.Metered < 1 {
		t.Fatalf("Stop = %+v, %v", m, err)
	}
}

func TestWaitFailures(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	create := func() string {
		arn, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8, Platform: "android"}, "")
		if err != nil {
			t.Fatal(err)
		}
		return arn
	}
	for _, state := range []string{"STOPPING", "COMPLETED"} {
		arn := create()
		f.SetSessionState(arn, state)
		_, err := c.Wait(ctx, arn, time.Second, nil)
		if cloudprov.KindOf(err) != cloudprov.Ended || !strings.Contains(err.Error(), state) {
			t.Errorf("%s: err = %v", state, err)
		}
	}

	arn := create()
	f.SetSessionState(arn, "PENDING_DEVICE")
	if _, err := c.Wait(ctx, arn, time.Second, nil); cloudprov.KindOf(err) != cloudprov.DeviceBusy {
		t.Errorf("pending device: err = %v", err)
	}

	arn = create()
	f.SetSessionState(arn, "PENDING_CONCURRENCY")
	_, err := c.Wait(ctx, arn, 20*time.Millisecond, nil)
	if cloudprov.KindOf(err) != cloudprov.NoCapacity || !strings.Contains(err.Error(), "2 metered") ||
		!strings.Contains(err.Error(), "account") {
		t.Errorf("pending concurrency: err = %v", err)
	}

	arn = create()
	f.SetSessionState(arn, "PREPARING")
	if _, err := c.Wait(ctx, arn, 20*time.Millisecond, nil); cloudprov.KindOf(err) != cloudprov.Other || !strings.Contains(err.Error(), "PREPARING") {
		t.Errorf("slow start: err = %v", err)
	}

	arn = create()
	f.SetSessionState(arn, "PREPARING")
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := c.Wait(cctx, arn, time.Hour, nil); err == nil {
		t.Error("a cancelled wait succeeded")
	}

	if _, err := c.Wait(ctx, "arn:aws:devicefarm:us-west-2:1:session:p/none/00000", time.Second, nil); Classify(err).Kind != cloudprov.NotFound {
		t.Errorf("unknown: err = %v", err)
	}
}

func TestStopInstallListAndErrors(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	arn, _ := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8, Platform: "android"}, "df-a")
	if _, err := c.Wait(ctx, arn, time.Second, nil); err != nil {
		t.Fatal(err)
	}
	project, _ := c.Project(ctx)
	app := f.SeedUpload(project, "a.apk", "ANDROID_APP", "SUCCEEDED", time.Now())
	ref, err := c.Install(ctx, arn, app)
	if err != nil || ref.Ref != app || ref.Name != "a.apk" {
		t.Fatalf("Install = %+v, %v", ref, err)
	}
	body := lastBody(t, f, "InstallToRemoteAccessSession")
	if body["remoteAccessSessionArn"] != arn || body["appArn"] != app {
		t.Fatalf("install body = %v", body)
	}
	if s, _ := f.Session(arn); !reflect.DeepEqual(s.Installed, []string{app}) {
		t.Fatalf("installed = %v", s.Installed)
	}
	if _, err := c.Install(ctx, arn, "arn:aws:devicefarm:us-west-2:1:upload:p/none"); Classify(err).Kind != cloudprov.NotFound {
		t.Fatalf("install unknown: %v", err)
	}

	f.SetPageSize(1)
	c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel7, Platform: "android"}, "df-b")
	list, err := c.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "df-a" || list[1].Name != "df-b" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if list[0].Endpoint != "" {
		t.Fatal("List carries an endpoint")
	}
	if f.CountOp("ListRemoteAccessSessions") != 2 {
		t.Fatalf("pages: %v", f.Ops())
	}

	f.OmitStopMinutes(true)
	if m, err := c.Stop(ctx, arn); err != nil || m != nil {
		t.Fatalf("Stop without minutes = %+v, %v", m, err)
	}

	f.FailOp("ListRemoteAccessSessions", "ArgumentException", "x")
	if _, err := c.List(ctx); err == nil {
		t.Fatal("List error ignored")
	}
	for _, op := range []string{"CreateRemoteAccessSession", "StopRemoteAccessSession", "ListProjects"} {
		f.FailOp(op, "ArgumentException", "x")
	}
	if _, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8}, ""); err == nil {
		t.Fatal("Create error ignored")
	}
	if _, err := c.Stop(ctx, arn); err == nil {
		t.Fatal("Stop error ignored")
	}
	c.projectARN = ""
	if _, err := c.Create(ctx, SessionSpec{DeviceARN: dftest.ARNPixel8}, ""); err == nil {
		t.Fatal("project error ignored")
	}
	if _, err := c.List(ctx); err == nil {
		t.Fatal("project error ignored")
	}
}

func TestCompareOS(t *testing.T) {
	if compareOS("17.4", "17.4.0") != 0 || compareOS("18", "17.4") != 1 || compareOS("", "15") != -1 {
		t.Fatal("compareOS")
	}
}
