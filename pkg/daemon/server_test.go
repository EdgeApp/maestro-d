package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// testDeps builds Deps backed by the mock driver. cleanups counts driver
// teardowns so tests can assert detach/shutdown closed them.
func testDeps(cleanups *int32, stepDelay time.Duration) Deps {
	return Deps{
		Version: "test",
		NewDriver: func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
			if strings.HasPrefix(id, "bad-") {
				return nil, DriverInfo{}, nil, errors.New("no such device")
			}
			d := mock.New(mock.Config{Platform: "mock", DeviceID: id, StepDelay: stepDelay})
			info := DriverInfo{
				DriverName: "mock",
				Device:     report.Device{ID: id, Platform: "mock"},
				App:        report.App{ID: cfg.AppID},
				Platform:   "mock", Kind: "device", Name: "Mock " + id,
			}
			return d, info, func() { atomic.AddInt32(cleanups, 1) }, nil
		},
		ListDevices: func(platform string) []DeviceInfo {
			if platform != "" && platform != "mock" {
				return nil
			}
			return []DeviceInfo{{ID: "mock-1", Platform: "mock", Kind: "device", Ready: true, State: "Booted"}}
		},
	}
}

type testEnv struct {
	t        *testing.T
	s        *Server
	c        *Client
	cleanups int32
}

func newTestEnv(t *testing.T, stepDelay time.Duration) *testEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	env := &testEnv{t: t}
	env.s = New(Config{Name: "test-" + strings.ToLower(t.Name()), ReportsDir: filepath.Join(t.TempDir(), "reports")}, testDeps(&env.cleanups, stepDelay))
	env.s.info = Info{Name: env.s.cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	hs := httptest.NewServer(env.s.Handler())
	t.Cleanup(hs.Close)
	env.c = NewHTTPClient(hs.URL, "", nil)
	return env
}

func (e *testEnv) attach(id string) *DeviceInfo {
	e.t.Helper()
	info, err := e.c.Attach(context.Background(), id, AttachConfig{Platform: "mock", AppID: "com.example"})
	if err != nil {
		e.t.Fatalf("attach %s: %v", id, err)
	}
	return info
}

func codeOf(t *testing.T, err error) Code {
	t.Helper()
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	return de.Code
}

func TestServer_StatusAndDevices(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()
	st, err := env.c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Daemon.APIVersion != APIVersion {
		t.Fatalf("bad status: %+v", st)
	}
	devs, err := env.c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].ID != "mock-1" || devs[0].Attached || devs[0].BootedBy != BootedByExternal {
		t.Fatalf("unexpected devices: %+v", devs)
	}
	if _, err := env.c.Device(ctx, "nope"); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("expected DEVICE_NOT_FOUND, got %v", err)
	}
}

func TestServer_AttachCommandDetach(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()

	info := env.attach("mock-1")
	if !info.Attached || info.AppID != "com.example" || info.Driver != "mock" || info.ReportDir == "" {
		t.Fatalf("unexpected attach info: %+v", info)
	}
	// Second attach is a no-op.
	env.attach("mock-1")

	res, err := env.c.Command(ctx, "mock-1", "tapOn", "Login", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ExitCode != 0 || res.Type != "tapOn" || res.Device != "mock-1" || res.Index != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	res, err = env.c.Command(ctx, "mock-1", "back", nil, true)
	if err != nil || !res.OK || res.Index != 1 {
		t.Fatalf("back: %v %+v", err, res)
	}

	// Failing step: result + COMMAND_FAILED with artifacts.
	res, err = env.c.Command(ctx, "mock-1", "assertTrue", "false", true)
	if err == nil || res == nil {
		t.Fatalf("expected failure with result, got err=%v res=%v", err, res)
	}
	if codeOf(t, err) != CodeCommandFailed || res.OK || res.ExitCode != 1 || res.Error == nil {
		t.Fatalf("unexpected failure result: %+v (%v)", res, err)
	}
	if res.Error.Details["screenshot"] == nil {
		t.Fatalf("expected screenshot artifact in details: %+v", res.Error.Details)
	}
	// Optional failure is ok:true with the error retained.
	res, err = env.c.Command(ctx, "mock-1", "assertTrue", map[string]any{"condition": "false", "optional": true}, true)
	if err != nil || !res.OK || !res.Optional || res.Error == nil {
		t.Fatalf("optional: %v %+v", err, res)
	}

	// Unknown command / bad value.
	if _, err := env.c.Command(ctx, "mock-1", "flyAway", nil, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE, got %v", err)
	}
	if _, err := env.c.Command(ctx, "mock-1", "tapOn", []int{1}, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE for bad value, got %v", err)
	}

	// Report on disk.
	idx := filepath.Join(info.ReportDir, "report.json")
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("report.json missing: %v", err)
	}

	if err := env.c.Detach(ctx, "mock-1"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&env.cleanups) != 1 {
		t.Fatalf("expected 1 cleanup, got %d", env.cleanups)
	}
	if _, err := env.c.Command(ctx, "mock-1", "back", nil, true); codeOf(t, err) != CodeDeviceNotAttached {
		t.Fatalf("expected DEVICE_NOT_ATTACHED, got %v", err)
	}
	if err := env.c.Detach(ctx, "mock-1"); codeOf(t, err) != CodeDeviceNotAttached {
		t.Fatalf("expected DEVICE_NOT_ATTACHED, got %v", err)
	}
	devs, _ := env.c.Devices(ctx, "")
	if len(devs) != 1 || devs[0].Attached {
		t.Fatalf("device should be listed detached: %+v", devs)
	}
}

func TestServer_AttachFailure(t *testing.T) {
	env := newTestEnv(t, 0)
	_, err := env.c.Attach(context.Background(), "bad-1", AttachConfig{Platform: "mock"})
	if codeOf(t, err) != CodeDeviceError {
		t.Fatalf("expected DEVICE_ERROR, got %v", err)
	}
	_, err = env.c.Attach(context.Background(), "", AttachConfig{Platform: "mock"})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestServer_StepsAndFlow(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()
	env.attach("mock-1")

	res, err := env.c.Steps(ctx, "mock-1", StepsRequest{Steps: []any{
		map[string]any{"tapOn": "A"},
		"back",
		map[string]any{"assertTrue": "false"},
		map[string]any{"tapOn": "B"},
	}}, true)
	if err == nil || res == nil {
		t.Fatalf("expected failure with results: %v %v", err, res)
	}
	if res.Passed != 2 || res.Failed != 1 || len(res.Results) != 3 || res.ExitCode != 1 {
		t.Fatalf("unexpected batch: %+v", res)
	}

	res, err = env.c.Steps(ctx, "mock-1", StepsRequest{YAML: "- tapOn: A\n- assertTrue: 'false'\n- tapOn: B\n", ContinueOnError: true}, true)
	if err == nil || res.Passed != 2 || res.Failed != 1 || len(res.Results) != 3 {
		t.Fatalf("continueOnError: %v %+v", err, res)
	}

	if _, err := env.c.Steps(ctx, "mock-1", StepsRequest{Steps: []any{map[string]any{"nope": 1}}}, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE, got %v", err)
	}

	dir := t.TempDir()
	flowPath := filepath.Join(dir, "login.yaml")
	os.WriteFile(flowPath, []byte("appId: com.example\nenv:\n  USER: bob\n---\n- launchApp\n- tapOn: ${USER}\n- assertVisible: Home\n"), 0o644)
	fres, err := env.c.Flow(ctx, "mock-1", FlowRequest{File: "login.yaml", Cwd: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !fres.OK || len(fres.SubSteps) != 3 || fres.Type != "runFlow" {
		t.Fatalf("flow: %+v", fres)
	}
	// Flow env is scoped to the flow (upstream runFlow semantics) but is
	// expanded inside it.
	if y := fres.SubSteps[1].YAML; !strings.Contains(y, "bob") {
		t.Fatalf("expected ${USER} expanded in sub-step, got %q", y)
	}
	if vars, _ := env.c.Vars(ctx, "mock-1"); vars["USER"] == "bob" {
		t.Fatalf("flow env should not leak into session vars: %v", vars["USER"])
	}
	fres, err = env.c.Flow(ctx, "mock-1", FlowRequest{YAML: "appId: x\n---\n- assertTrue: 'false'\n"}, true)
	if err == nil || fres == nil || fres.OK {
		t.Fatalf("expected flow failure: %v %+v", err, fres)
	}
	if _, err := env.c.Flow(ctx, "mock-1", FlowRequest{}, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE, got %v", err)
	}
}

func TestServer_Inspection(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()
	env.attach("mock-1")

	png, err := env.c.Screenshot(ctx, "mock-1")
	if err != nil || len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("screenshot: %v %d bytes", err, len(png))
	}
	h, err := env.c.Hierarchy(ctx, "mock-1")
	if err != nil || h == nil {
		t.Fatalf("hierarchy: %v %v", err, h)
	}
	raw, err := env.c.HierarchyRaw(ctx, "mock-1")
	if err != nil || len(raw) == 0 {
		t.Fatalf("hierarchy raw: %v", err)
	}
	if _, err := env.c.State(ctx, "mock-1"); err != nil {
		t.Fatal(err)
	}
	pi, err := env.c.PlatformInfo(ctx, "mock-1")
	if err != nil || pi == nil || pi.Platform != "mock" {
		t.Fatalf("info: %v %+v", err, pi)
	}

	if _, err := env.c.SetVars(ctx, "mock-1", map[string]string{"A": "1"}); err != nil {
		t.Fatal(err)
	}
	v, err := env.c.Eval(ctx, "mock-1", "output.b = A + '2'; A + '3'")
	if err != nil || v != "13" {
		t.Fatalf("eval: %v %v", err, v)
	}
	vars, _ := env.c.Vars(ctx, "mock-1")
	if vars["A"] != "1" || vars["b"] != "12" {
		t.Fatalf("vars: %v", vars)
	}
	// PUT accepts the wrapped shape GET returns, and rejects non-string values.
	var vr VarsResult
	if e := env.c.call(ctx, http.MethodPut, devPath("mock-1", "/vars"), map[string]any{"vars": map[string]string{"C": "3"}}, &vr); e != nil || vr.Vars["C"] != "3" || vr.Vars["A"] != "1" {
		t.Fatalf("wrapped vars: %v %v", e, vr.Vars)
	}
	if e := env.c.call(ctx, http.MethodPut, devPath("mock-1", "/vars"), map[string]any{"D": 4}, &vr); e == nil || e.Code != CodeUsage {
		t.Fatalf("expected USAGE for non-string var, got %v", e)
	}
	if _, err := env.c.Eval(ctx, "mock-1", "throw new Error('boom')"); codeOf(t, err) != CodeCommandFailed {
		t.Fatalf("expected COMMAND_FAILED, got %v", err)
	}
	if _, err := env.c.Eval(ctx, "mock-1", "  "); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE, got %v", err)
	}
	if _, err := env.c.Screenshot(ctx, "mock-2"); codeOf(t, err) != CodeDeviceNotAttached {
		t.Fatalf("expected DEVICE_NOT_ATTACHED, got %v", err)
	}
}

func TestServer_BusyAndCancel(t *testing.T) {
	env := newTestEnv(t, 300*time.Millisecond)
	ctx := context.Background()
	env.attach("mock-1")

	done := make(chan *StepResult, 1)
	go func() {
		r, _ := env.c.Command(ctx, "mock-1", "tapOn", "slow", true)
		done <- r
	}()
	time.Sleep(50 * time.Millisecond)
	_, err := env.c.Command(ctx, "mock-1", "back", nil, false)
	if codeOf(t, err) != CodeBusy {
		t.Fatalf("expected BUSY, got %v", err)
	}
	devs, _ := env.c.Devices(ctx, "")
	if !devs[0].Busy {
		t.Fatalf("device should be busy: %+v", devs)
	}
	// wait=true queues behind the slow step.
	r, err := env.c.Command(ctx, "mock-1", "back", nil, true)
	if err != nil || !r.OK {
		t.Fatalf("queued step: %v %+v", err, r)
	}
	if first := <-done; first == nil || !first.OK {
		t.Fatalf("slow step: %+v", first)
	}

	// Client cancellation is reported as INTERRUPTED.
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = env.c.Command(cctx, "mock-1", "tapOn", "slow", true)
	if codeOf(t, err) != CodeInterrupted {
		t.Fatalf("expected INTERRUPTED, got %v", err)
	}
}

func TestServer_EventsAndShutdown(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()
	env.attach("mock-1")
	env.attach("mock-2")

	evCtx, evCancel := context.WithCancel(ctx)
	defer evCancel()
	got := make(chan Event, 64)
	go func() {
		_ = env.c.Events(evCtx, "", 0, func(ev Event) bool { got <- ev; return true })
	}()
	// Backlog contains the two attach events.
	seen := map[string]int{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-got:
			seen[ev.Type]++
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for backlog events")
		}
	}
	if seen["device.attached"] != 2 {
		t.Fatalf("expected 2 device.attached, got %v", seen)
	}
	if _, err := env.c.Command(ctx, "mock-2", "tapOn", "X", true); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if ev.Type != "step" || ev.Device != "mock-2" || !ev.Passed {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for step event")
	}

	if err := env.c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-env.s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if atomic.LoadInt32(&env.cleanups) != 2 {
		t.Fatalf("expected both drivers cleaned up, got %d", env.cleanups)
	}
	// Post-shutdown requests are refused; status still answers.
	if _, err := env.c.Command(ctx, "mock-1", "back", nil, true); codeOf(t, err) != CodeDaemonUnavailable {
		t.Fatalf("expected DAEMON_UNAVAILABLE, got %v", err)
	}
	if _, err := env.c.Status(ctx); err != nil {
		t.Fatalf("status after shutdown: %v", err)
	}
}

func TestServer_APIVersionAndToken(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	s := New(Config{Name: "tok", Token: "secret", ReportsDir: t.TempDir()}, testDeps(&n, 0))
	hs := httptest.NewServer(s.routes(true))
	defer hs.Close()

	if _, err := NewHTTPClient(hs.URL, "", nil).Status(context.Background()); codeOf(t, err) != CodeUsage {
		t.Fatalf("expected USAGE (401), got %v", err)
	}
	// A wrong token of the same length, a prefix and an extension are all
	// refused (BearerAuthorized compares the whole header in constant time).
	for _, tok := range []string{"secreT", "secre", "secrets"} {
		if _, err := NewHTTPClient(hs.URL, tok, nil).Status(context.Background()); codeOf(t, err) != CodeUsage {
			t.Fatalf("token %q: expected USAGE (401), got %v", tok, err)
		}
	}
	if BearerAuthorized("secret", "secret") || !BearerAuthorized("Bearer secret", "secret") {
		t.Fatal("BearerAuthorized wants the whole \"Bearer <token>\" header")
	}
	// A listener without a token lets every header through, none included.
	if !BearerAuthorized("", "") || !BearerAuthorized("Bearer x", "") || BearerAuthorized("", "secret") {
		t.Fatal("BearerAuthorized with and without a token")
	}
	if _, err := NewHTTPClient(hs.URL, "secret", nil).Status(context.Background()); err != nil {
		t.Fatalf("with token: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Maestro-Api-Version", "99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on version mismatch, got %d", resp.StatusCode)
	}
}

func TestServer_DeviceInUseByOtherDaemon(t *testing.T) {
	env := newTestEnv(t, 0)
	// Simulate another live daemon that has mock-1 attached.
	other := &Info{Name: "other", PID: os.Getpid(), APIVersion: APIVersion}
	if err := WriteInfo(other); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(DevicesPath("other"), []DeviceInfo{{ID: "mock-1", Platform: "mock", Attached: true}}); err != nil {
		t.Fatal(err)
	}
	_, err := env.c.Attach(context.Background(), "mock-1", AttachConfig{Platform: "mock"})
	if codeOf(t, err) != CodeDeviceInUse {
		t.Fatalf("expected DEVICE_IN_USE, got %v", err)
	}
	var de *Error
	errors.As(err, &de)
	if de.Details["owner"] != "other" {
		t.Fatalf("expected owner detail: %+v", de.Details)
	}
	// Another device is fine.
	env.attach("mock-2")
}

func TestServer_StopDevice(t *testing.T) {
	env := newTestEnv(t, 0)
	ctx := context.Background()
	env.attach("mock-1")
	// External devices need --force; attached ones are detached first.
	if err := env.c.StopDevice(ctx, "mock-1", false); codeOf(t, err) != CodeDeviceExternal {
		t.Fatalf("expected DEVICE_EXTERNAL, got %v", err)
	}
	if atomic.LoadInt32(&env.cleanups) != 1 {
		t.Fatalf("stop should detach first, cleanups=%d", env.cleanups)
	}
	if err := env.c.StopDevice(ctx, "mock-1", true); err != nil {
		t.Fatalf("force stop of a mock device is a no-op: %v", err)
	}
	if err := env.c.StopDevice(ctx, "nope", false); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("expected DEVICE_NOT_FOUND, got %v", err)
	}
}

func TestRunfile_LiveInfoAndList(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	if info, err := LiveInfo("none"); info != nil || err != nil {
		t.Fatalf("expected no daemon: %v %v", info, err)
	}
	dead := &Info{Name: "dead", PID: 999999999}
	WriteInfo(dead)
	if info, _ := LiveInfo("dead"); info != nil {
		t.Fatal("dead pid should not be live")
	}
	if _, err := os.Stat(InfoPath("dead")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale daemon.json should be removed")
	}
	live := &Info{Name: "live", PID: os.Getpid()}
	WriteInfo(live)
	if info, _ := LiveInfo("live"); info == nil || info.PID != os.Getpid() {
		t.Fatal("expected live daemon")
	}
	// Orphan: dead pid but daemon-booted device recorded.
	WriteInfo(&Info{Name: "orphan", PID: 999999999})
	atomicWriteJSON(DevicesPath("orphan"), []DeviceInfo{{ID: "sim-1", Platform: "ios", BootedBy: BootedByDaemon}})
	list, err := ListDaemons()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range list {
		names[d.Name] = d.Alive
	}
	if alive, ok := names["live"]; !ok || !alive {
		t.Fatalf("live missing: %v", names)
	}
	if alive, ok := names["orphan"]; !ok || alive {
		t.Fatalf("orphan missing or alive: %v", names)
	}
	if err := ValidateName("../x"); err == nil {
		t.Fatal("expected invalid name")
	}
	t.Setenv(EnvName, "agent7")
	if ResolveName("") != "agent7" || ResolveName("flag") != "flag" {
		t.Fatal("ResolveName precedence")
	}
}

func TestCodes_Table(t *testing.T) {
	cases := []struct {
		code Code
		exit int
		http int
	}{
		{"", 0, 200}, {CodeCommandFailed, 1, 422}, {CodeUsage, 2, 400}, {CodeDaemonUnavailable, 3, 503},
		{CodeDeviceError, 4, 502}, {CodeBusy, 5, 409}, {CodeDeviceExternal, 5, 409}, {CodeDeviceInUse, 5, 409},
		{CodeDeviceNotFound, 4, 404}, {CodeDeviceNotAttached, 2, 404}, {CodeInterrupted, 130, 499}, {CodeInternal, 4, 500},
		{CodeForbidden, 2, 403}, {CodePeerUnavailable, 3, 503}, {CodeHubNameTaken, 5, 409}, {CodeHubVersion, 5, 409},
		{CodeLeaseRequired, 5, 409}, {CodeLeaseExpired, 5, 409},
	}
	if len(cases)-1 != len(Codes) {
		t.Fatalf("the table covers %d codes, Codes has %d", len(cases)-1, len(Codes))
	}
	for _, c := range cases {
		if c.code.ExitCode() != c.exit || c.code.HTTPStatus() != c.http {
			t.Errorf("%s: exit %d http %d", c.code, c.code.ExitCode(), c.code.HTTPStatus())
		}
	}
	if Code("NEW_THING").ExitCode() != 4 || Code("NEW_THING").HTTPStatus() != 500 {
		t.Error("unknown codes")
	}
	// CodeFromHTTPStatus inverts HTTPStatus where a status has one code;
	// 401 (the token check) is USAGE and 403 FORBIDDEN.
	for status, want := range map[int]Code{422: CodeCommandFailed, 400: CodeUsage, 401: CodeUsage, 403: CodeForbidden,
		503: CodeDaemonUnavailable, 502: CodeDeviceError, 409: CodeBusy, 404: CodeDeviceNotFound, 418: CodeInternal} {
		if got := CodeFromHTTPStatus(status); got != want {
			t.Errorf("CodeFromHTTPStatus(%d) = %s, want %s", status, got, want)
		}
	}
	if e := errorFromResponse(403, []byte("<html>forbidden</html>")); e.Code != CodeForbidden || e.ExitCode() != 2 {
		t.Errorf("a bare 403: %+v", e)
	}
}
