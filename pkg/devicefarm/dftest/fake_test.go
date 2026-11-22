package dftest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	appiumdriver "github.com/devicelab-dev/maestro-runner/pkg/driver/appium"
)

// call posts an awsJson1.1 operation and decodes the answer.
func call(t *testing.T, f *Fake, op string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.URL()+"/", bytes.NewReader(b))
	req.Header.Set("X-Amz-Target", Target+op)
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func wantCode(t *testing.T, status int, out map[string]any, code string) {
	t.Helper()
	if status != http.StatusBadRequest || out["__type"] != code {
		t.Fatalf("status %d out %v, want %s", status, out, code)
	}
}

// runningSession creates a session on device and polls it to RUNNING.
func runningSession(t *testing.T, f *Fake, device string) string {
	t.Helper()
	project := f.EnsureProject("p")
	_, out := call(t, f, "CreateRemoteAccessSession", map[string]any{"projectArn": project, "deviceArn": device, "name": "df-x",
		"configuration": map[string]any{"billingMethod": "METERED", "auxiliaryApps": []string{"a"}, "parameters": map[string]string{"appium:version": "2"}}})
	arn := out["remoteAccessSession"].(map[string]any)["arn"].(string)
	for i := 0; i < 3; i++ {
		call(t, f, "GetRemoteAccessSession", map[string]any{"arn": arn})
	}
	if s, _ := f.Session(arn); s.Status != "RUNNING" {
		t.Fatalf("status = %s", s.Status)
	}
	return arn
}

func TestNewServerAndProjects(t *testing.T) {
	f := NewServer()
	defer f.Close()
	if !strings.HasPrefix(f.URL(), "http://") {
		t.Fatal(f.URL())
	}
	_, out := call(t, f, "CreateProject", map[string]any{"name": "maestro-d"})
	arn := out["project"].(map[string]any)["arn"].(string)
	if f.EnsureProject("maestro-d") != arn || len(f.Projects()) != 1 {
		t.Fatal("EnsureProject made a second project")
	}
	st, out := call(t, f, "CreateProject", map[string]any{})
	wantCode(t, st, out, "ArgumentException")
	f.EnsureProject("b")
	f.SetPageSize(0) // clamps to 1
	_, out = call(t, f, "ListProjects", map[string]any{})
	if len(out["projects"].([]any)) != 1 || out["nextToken"] != "page-1" {
		t.Fatalf("page = %v", out)
	}
	_, out = call(t, f, "ListProjects", map[string]any{"nextToken": "page-1"})
	if _, more := out["nextToken"]; more || len(out["projects"].([]any)) != 1 {
		t.Fatalf("page 2 = %v", out)
	}
	st, out = call(t, f, "ListProjects", map[string]any{"nextToken": "bogus"})
	wantCode(t, st, out, "ArgumentException")
}

func TestRequestValidationAndScriptedFailures(t *testing.T) {
	f := New(t)
	req, _ := http.NewRequest(http.MethodGet, f.URL()+"/", nil)
	st, out := do(t, req)
	wantCode(t, st, out, "UnknownOperationException")
	st, out = call(t, f, "RunSomething", map[string]any{})
	wantCode(t, st, out, "UnknownOperationException")
	req, _ = http.NewRequest(http.MethodPost, f.URL()+"/", strings.NewReader("{"))
	req.Header.Set("X-Amz-Target", Target+"ListDevices")
	st, out = do(t, req)
	wantCode(t, st, out, "SerializationException")

	f.FailOp("GetAccountSettings", "AccessDeniedException", "denied")
	st, out = call(t, f, "GetAccountSettings", nil)
	wantCode(t, st, out, "AccessDeniedException")
	f.FailOp("GetAccountSettings", "", "")
	f.SetTrialMinutes(1000, 5)
	_, out = call(t, f, "GetAccountSettings", nil)
	if out["accountSettings"].(map[string]any)["trialMinutes"].(map[string]any)["remaining"] != 5.0 {
		t.Fatalf("account = %v", out)
	}
	f.FailOp("StopRemoteAccessSession", "InternalServiceException", "x")
	st, out = call(t, f, "StopRemoteAccessSession", map[string]any{"arn": "a1"})
	wantCode(t, st, out, "InternalServiceException")
	if got := f.StoppedSessions(); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("stops = %v", got)
	}
	f.Mark("here")
	ops := f.Ops()
	if ops[len(ops)-1] != "mark:here" || f.CountOp("GetAccountSettings") != 2 {
		t.Fatalf("ops = %v", ops)
	}
	if reqs := f.Requests(); reqs[0].Op != "" || reqs[0].Method != http.MethodGet || len(reqs) != len(ops)+1 {
		t.Fatalf("requests = %+v", reqs[0])
	}
}

func TestDevices(t *testing.T) {
	f := New(t)
	f.SetPageSize(100)
	_, out := call(t, f, "ListDevices", map[string]any{"filters": []any{
		map[string]any{"attribute": "REMOTE_ACCESS_ENABLED", "operator": "EQUALS", "values": []string{"TRUE"}},
		map[string]any{"attribute": "PLATFORM", "operator": "EQUALS", "values": []string{"IOS"}},
	}})
	if n := len(out["devices"].([]any)); n != 2 {
		t.Fatalf("ios devices = %d", n)
	}
	_, out = call(t, f, "ListDevices", map[string]any{"filters": []any{
		map[string]any{"attribute": "AVAILABILITY", "operator": "NOT_EQUALS", "values": []string{"HIGHLY_AVAILABLE"}},
		map[string]any{"attribute": "FORM_FACTOR", "operator": "IN", "values": []string{"PHONE"}},
		map[string]any{"attribute": "MANUFACTURER", "operator": "EQUALS", "values": []string{"samsung"}},
		map[string]any{"attribute": "MODEL", "operator": "EQUALS", "values": []string{"ignored"}},
	}})
	if n := len(out["devices"].([]any)); n != 2 {
		t.Fatalf("samsung phones not highly available = %d", n)
	}
	f.AddDevice(Device{ARN: "arn:x", Name: "Extra", Platform: "ANDROID", OS: "15", RemoteAccess: true})
	_, out = call(t, f, "GetDevice", map[string]any{"arn": "arn:x"})
	if out["device"].(map[string]any)["name"] != "Extra" {
		t.Fatalf("GetDevice = %v", out)
	}
	st, out := call(t, f, "GetDevice", map[string]any{"arn": "arn:none"})
	wantCode(t, st, out, "NotFoundException")
	f.SetDevices(SeedDevices()[:1])
	_, out = call(t, f, "ListDevices", map[string]any{})
	if n := len(out["devices"].([]any)); n != 1 {
		t.Fatalf("SetDevices: %d", n)
	}
}

func TestUploads(t *testing.T) {
	f := New(t)
	project := f.EnsureProject("p")
	st, out := call(t, f, "CreateUpload", map[string]any{"projectArn": "arn:none", "name": "a.apk", "type": "ANDROID_APP"})
	wantCode(t, st, out, "NotFoundException")
	st, out = call(t, f, "CreateUpload", map[string]any{"projectArn": project, "name": "a.zip", "type": "APPIUM_NODE_TEST_PACKAGE"})
	wantCode(t, st, out, "ArgumentException")
	_, out = call(t, f, "CreateUpload", map[string]any{"projectArn": project, "name": "a.apk", "type": "ANDROID_APP", "contentType": "application/octet-stream"})
	up := out["upload"].(map[string]any)
	url, arn := up["url"].(string), up["arn"].(string)
	if up["status"] != "INITIALIZED" || !strings.Contains(url, "X-Amz-Signature=") {
		t.Fatalf("upload = %v", up)
	}
	put := func(method, u string) int {
		req, _ := http.NewRequest(method, u, strings.NewReader("app bytes"))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if put(http.MethodPut, f.URL()+"/upload/none?X-Amz-Signature=x") != http.StatusNotFound ||
		put(http.MethodPost, url) != http.StatusMethodNotAllowed ||
		put(http.MethodPut, strings.Split(url, "?")[0]) != http.StatusForbidden {
		t.Fatal("bad PUTs accepted")
	}
	f.SetUploadDelay(time.Hour)
	if put(http.MethodPut, url) != http.StatusOK {
		t.Fatal("PUT failed")
	}
	_, out = call(t, f, "GetUpload", map[string]any{"arn": arn})
	if out["upload"].(map[string]any)["status"] != "PROCESSING" {
		t.Fatalf("delayed = %v", out)
	}
	f.SetUploadDelay(0)
	_, out = call(t, f, "GetUpload", map[string]any{"arn": arn})
	if out["upload"].(map[string]any)["status"] != "SUCCEEDED" {
		t.Fatalf("settled = %v", out)
	}
	if u := f.Uploads()[0]; u.PutContentType != "application/octet-stream" || u.Size != int64(len("app bytes")) || u.ContentType != "application/octet-stream" {
		t.Fatalf("upload = %+v", u)
	}
	st, out = call(t, f, "GetUpload", map[string]any{"arn": "arn:none"})
	wantCode(t, st, out, "NotFoundException")

	f.FailUploads("bad app", "meta")
	_, out = call(t, f, "CreateUpload", map[string]any{"projectArn": project, "name": "b.ipa", "type": "IOS_APP"})
	put(http.MethodPut, out["upload"].(map[string]any)["url"].(string))
	_, out = call(t, f, "GetUpload", map[string]any{"arn": out["upload"].(map[string]any)["arn"]})
	if u := out["upload"].(map[string]any); u["status"] != "FAILED" || u["message"] != "bad app" || u["metadata"] != "meta" {
		t.Fatalf("failed = %v", u)
	}
	f.FailUploads("", "")

	old := f.SeedUpload(project, "old.apk", "ANDROID_APP", "SUCCEEDED", time.Now().Add(-40*24*time.Hour))
	f.SetPageSize(10)
	_, out = call(t, f, "ListUploads", map[string]any{"arn": project, "type": "ANDROID_APP"})
	if n := len(out["uploads"].([]any)); n != 2 {
		t.Fatalf("android uploads = %d", n)
	}
	_, out = call(t, f, "ListUploads", map[string]any{"arn": project})
	if n := len(out["uploads"].([]any)); n != 3 {
		t.Fatalf("uploads = %d", n)
	}
	call(t, f, "DeleteUpload", map[string]any{"arn": old})
	st, out = call(t, f, "DeleteUpload", map[string]any{"arn": old})
	wantCode(t, st, out, "NotFoundException")
	if len(f.Uploads()) != 2 {
		t.Fatalf("uploads after delete = %d", len(f.Uploads()))
	}
}

func TestSessionLifecycle(t *testing.T) {
	f := New(t)
	project := f.EnsureProject("p")
	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"projectArn": "arn:none", "deviceArn": ARNPixel8}, "NotFoundException"},
		{map[string]any{"projectArn": project, "deviceArn": ARNPixel6Local}, "ArgumentException"},
		{map[string]any{"projectArn": project, "deviceArn": "arn:none"}, "ArgumentException"},
		{map[string]any{"projectArn": project, "deviceArn": ARNPixel8, "appArn": "arn:none"}, "NotFoundException"},
	} {
		st, out := call(t, f, "CreateRemoteAccessSession", tc.body)
		wantCode(t, st, out, tc.code)
	}

	arn := runningSession(t, f, ARNPixel8)
	s, ok := f.Session(arn)
	if !ok || s.Name != "df-x" || s.BillingMethod != "METERED" || s.Parameters["appium:version"] != "2" || len(s.AuxiliaryApps) != 1 {
		t.Fatalf("session = %+v", s)
	}
	_, out := call(t, f, "GetRemoteAccessSession", map[string]any{"arn": arn})
	ras := out["remoteAccessSession"].(map[string]any)
	if ras["endpoints"].(map[string]any)["remoteDriverEndpoint"] != f.Endpoint(arn) || ras["deviceMinutes"] == nil {
		t.Fatalf("running = %v", ras)
	}
	if f.Endpoint("arn:none") != "" {
		t.Fatal("endpoint for an unknown session")
	}
	_, out = call(t, f, "ListRemoteAccessSessions", map[string]any{"arn": project})
	if l := out["remoteAccessSessions"].([]any); len(l) != 1 || l[0].(map[string]any)["endpoints"] != nil {
		t.Fatalf("list = %v", l)
	}

	app := f.SeedUpload(project, "a.apk", "ANDROID_APP", "SUCCEEDED", time.Now())
	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"remoteAccessSessionArn": "arn:none", "appArn": app}, "NotFoundException"},
		{map[string]any{"remoteAccessSessionArn": arn, "appArn": "arn:none"}, "NotFoundException"},
	} {
		st, out := call(t, f, "InstallToRemoteAccessSession", tc.body)
		wantCode(t, st, out, tc.code)
	}
	_, out = call(t, f, "InstallToRemoteAccessSession", map[string]any{"remoteAccessSessionArn": arn, "appArn": app})
	if out["appUpload"].(map[string]any)["arn"] != app {
		t.Fatalf("install = %v", out)
	}

	f.OmitStopMinutes(true)
	_, out = call(t, f, "StopRemoteAccessSession", map[string]any{"arn": arn})
	if ras := out["remoteAccessSession"].(map[string]any); ras["status"] != "COMPLETED" || ras["deviceMinutes"] != nil {
		t.Fatalf("stop = %v", ras)
	}
	st, out := call(t, f, "StopRemoteAccessSession", map[string]any{"arn": arn})
	wantCode(t, st, out, "ArgumentException")
	st, out = call(t, f, "StopRemoteAccessSession", map[string]any{"arn": "arn:none"})
	wantCode(t, st, out, "NotFoundException")
	st, out = call(t, f, "GetRemoteAccessSession", map[string]any{"arn": "arn:none"})
	wantCode(t, st, out, "NotFoundException")
	st, out = call(t, f, "InstallToRemoteAccessSession", map[string]any{"remoteAccessSessionArn": arn, "appArn": app})
	wantCode(t, st, out, "ArgumentException")
	if got := f.CreatedSessions(); len(got) != 1 || got[0] != arn || len(f.Sessions()) != 1 {
		t.Fatalf("created = %v", got)
	}
}

func TestHoldsAndPinnedStates(t *testing.T) {
	f := New(t)
	project := f.EnsureProject("p")
	create := func(device string) string {
		_, out := call(t, f, "CreateRemoteAccessSession", map[string]any{"projectArn": project, "deviceArn": device})
		return out["remoteAccessSession"].(map[string]any)["arn"].(string)
	}
	status := func(arn string) string {
		_, out := call(t, f, "GetRemoteAccessSession", map[string]any{"arn": arn})
		return out["remoteAccessSession"].(map[string]any)["status"].(string)
	}
	f.HoldDevice(ARNPixel8, "PENDING_DEVICE")
	f.Hold("PENDING_CONCURRENCY")
	a, b := create(ARNPixel8), create(ARNPixel7)
	for i := 0; i < 3; i++ {
		status(a)
		status(b)
	}
	if status(a) != "PENDING_DEVICE" || status(b) != "PENDING_CONCURRENCY" {
		t.Fatal("holds not applied")
	}
	f.HoldDevice(ARNPixel8, "")
	f.Hold("")
	status(a)
	if status(a) != "PREPARING" || status(a) != "RUNNING" {
		t.Fatal("released hold did not run")
	}
	if !f.SetSessionState(b, "RUNNING") || status(b) != "RUNNING" {
		t.Fatal("pinned RUNNING")
	}
	if s, _ := f.Session(b); s.Started.IsZero() {
		t.Fatal("pinned RUNNING has no start")
	}
	if f.SetSessionState("arn:none", "RUNNING") || f.ExpireSession("arn:none") {
		t.Fatal("unknown session scripted")
	}
	if !f.ExpireSession(a) || status(a) != "COMPLETED" {
		t.Fatal("expire")
	}
	_, out := call(t, f, "GetRemoteAccessSession", map[string]any{"arn": a})
	if out["remoteAccessSession"].(map[string]any)["message"] == nil {
		t.Fatal("expired session has no message")
	}
}

func TestAppiumEndpoint(t *testing.T) {
	f := New(t)
	arn := runningSession(t, f, ARNPixel8)
	ep := f.Endpoint(arn)

	c := appiumdriver.NewClient(ep)
	if err := c.Connect(map[string]any{"platformName": "Android", "appium:automationName": "UiAutomator2"}); err != nil {
		t.Fatal(err)
	}
	if c.Platform() != "android" || c.DeviceName() != "Google Pixel 8" || c.OSVersion() != "14" || !c.IsRealDevice() {
		t.Fatalf("session: %s %s %s", c.Platform(), c.DeviceName(), c.OSVersion())
	}
	if w, h := c.ScreenSize(); w != 1080 || h != 2400 {
		t.Fatalf("screen %dx%d", w, h)
	}
	png, err := c.Screenshot()
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("screenshot: %v", err)
	}
	src, err := c.Source()
	if err != nil || !strings.Contains(src, `text="Button"`) {
		t.Fatalf("source: %v %s", err, src)
	}
	if o, err := c.GetOrientation(); err != nil || o != "portrait" {
		t.Fatalf("orientation %q %v", o, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Keepalive())
	id, err := c.FindElement("id", "button")
	must(err)
	ids, err := c.FindElements("id", "button")
	must(err)
	if id != "el-1" || len(ids) != 1 {
		t.Fatalf("find %q %v", id, ids)
	}
	if x, y, w, h, err := c.GetElementRect(id); err != nil || x != 100 || y != 200 || w != 200 || h != 60 {
		t.Fatalf("rect %d %d %d %d %v", x, y, w, h, err)
	}
	if txt, err := c.GetElementText(id); err != nil || txt != "Button" {
		t.Fatalf("text %q %v", txt, err)
	}
	if v, err := c.GetElementAttribute(id, "text"); err != nil || v != "Button" {
		t.Fatalf("attribute %q %v", v, err)
	}
	if ok, err := c.IsElementDisplayed(id); err != nil || !ok {
		t.Fatal("displayed")
	}
	if ok, err := c.IsElementEnabled(id); err != nil || !ok {
		t.Fatal("enabled")
	}
	if _, err := c.GetActiveElement(); err != nil {
		t.Fatal(err)
	}
	must(c.ClickElement(id))
	must(c.ClearElement(id))
	must(c.Tap(10, 20))
	must(c.Swipe(1, 2, 3, 4, 100))
	must(c.LaunchApp("com.example"))
	must(c.TerminateApp("com.example"))
	_, err = c.ExecuteMobile("shell", map[string]any{"command": "echo"})
	must(err)

	// A second WebDriver session on the same device, then RestartSession-style
	// delete and reopen with the driver.
	d, err := appiumdriver.NewDriver(ep, map[string]any{"platformName": "Android"})
	must(err)
	must(d.RestartSession())
	must(d.Close())
	must(c.Disconnect())
	s, _ := f.Session(arn)
	if s.WebDrivers != 0 || len(s.Caps) != 3 {
		t.Fatalf("webdrivers %d caps %d", s.WebDrivers, len(s.Caps))
	}

	// The request log has the Appium traffic with paths.
	var sawSource bool
	for _, r := range f.Requests() {
		if r.Op == "" && r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/source") {
			sawSource = true
		}
	}
	if !sawSource {
		t.Fatal("source request not recorded")
	}
}

func TestAppiumEndpointErrors(t *testing.T) {
	f := New(t)
	arn := runningSession(t, f, ARNIPhone15)
	ep := f.Endpoint(arn)
	get := func(path string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		return do(t, req)
	}
	if st, out := get(ep + "/status"); st != http.StatusOK || out["value"].(map[string]any)["ready"] != true {
		t.Fatalf("status %d %v", st, out)
	}
	if st, _ := get(f.URL() + "/remote-endpoint/nope/wd/hub/status"); st != http.StatusNotFound {
		t.Fatal("unknown token")
	}
	if st, _ := get(strings.TrimSuffix(ep, "/wd/hub") + "/other"); st != http.StatusNotFound {
		t.Fatal("not under /wd/hub")
	}
	if st, _ := get(ep + "/sessions"); st != http.StatusNotFound {
		t.Fatal("unknown command")
	}
	if st, out := get(ep + "/session/none/source"); st != http.StatusNotFound || out["value"].(map[string]any)["error"] != "invalid session id" {
		t.Fatal("unknown WebDriver session")
	}
	// strictCaps.
	c := appiumdriver.NewClient(ep)
	if err := c.Connect(map[string]any{"platformName": "iOS", "appium:udid": "x"}); err == nil || !strings.Contains(err.Error(), "strictCaps") {
		t.Fatalf("udid accepted: %v", err)
	}
	// iOS page source and the GET session/settings routes.
	if err := c.Connect(map[string]any{"platformName": "iOS"}); err != nil {
		t.Fatal(err)
	}
	src, _ := c.Source()
	if !strings.Contains(src, "XCUIElementTypeButton") {
		t.Fatalf("ios source %s", src)
	}
	sid := c.SessionID()
	if st, out := get(ep + "/session/" + sid); st != http.StatusOK || out["value"].(map[string]any)["platformName"] != "iOS" {
		t.Fatalf("GET session %d %v", st, out)
	}
	if st, _ := get(ep + "/session/" + sid + "/appium/settings"); st != http.StatusOK {
		t.Fatal("GET settings")
	}
	if st, out := get(ep + "/session/" + sid + "/element/el-1/selected"); st != http.StatusOK || out["value"] != false {
		t.Fatal("selected")
	}
	// Refused WebDriver sessions.
	f.FailWebDriver(true)
	if err := appiumdriver.NewClient(ep).Connect(map[string]any{"platformName": "iOS"}); err == nil {
		t.Fatal("WebDriver refusal ignored")
	}
	f.FailWebDriver(false)
	// An ended session's endpoint answers 404.
	f.ExpireSession(arn)
	if st, out := get(ep + "/status"); st != http.StatusNotFound || !strings.Contains(out["value"].(map[string]any)["message"].(string), "COMPLETED") {
		t.Fatalf("expired: %d %v", st, out)
	}
	if err := c.Keepalive(); err == nil {
		t.Fatal("keepalive on an ended session")
	}
}

func TestScreenshotIsPNG(t *testing.T) {
	b, err := base64.StdEncoding.DecodeString(screenshot)
	if err != nil || !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatal("screenshot is not a PNG")
	}
}
