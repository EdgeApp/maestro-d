// Package dftest is an in-process fake of AWS Device Farm for tests: the
// awsJson1.1 operations pkg/devicefarm uses, presigned-style upload URLs and
// one fake Appium endpoint per remote access session, all on one
// httptest.Server.
//
// Point a client at it with devicefarm.Options.Endpoint or the
// MAESTRO_D_DEVICEFARM_ENDPOINT environment variable. It records every
// request in order and can be scripted to hold sessions pending, fail
// operations, expire sessions, fail or delay uploads and refuse WebDriver
// sessions.
package dftest

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountID is the fake's AWS account number.
const AccountID = "123456789012"

// Target is the X-Amz-Target prefix of Device Farm operations.
const Target = "DeviceFarm_20150623."

// ARNs of the seeded devices.
const (
	ARNPixel8      = "arn:aws:devicefarm:us-west-2::device:8A11C0DE000000000000000000000008"
	ARNPixel7      = "arn:aws:devicefarm:us-west-2::device:7A11C0DE000000000000000000000007"
	ARNIPhone15    = "arn:aws:devicefarm:us-west-2::device:15A9E00000000000000000000000000F"
	ARNIPhone14    = "arn:aws:devicefarm:us-west-2::device:14A9E00000000000000000000000000E"
	ARNGalaxyTab   = "arn:aws:devicefarm:us-west-2::device:7AB5900000000000000000000000000A"
	ARNGalaxyA54   = "arn:aws:devicefarm:us-west-2::device:A54A0000000000000000000000000001"
	ARNGalaxyA54B  = "arn:aws:devicefarm:us-west-2::device:B54B0000000000000000000000000002"
	ARNPixel6Local = "arn:aws:devicefarm:us-west-2::device:6A11C0DE000000000000000000000006"
)

// Device is a model in the fake's pool. Platform is ANDROID or IOS,
// FormFactor PHONE or TABLET, Availability one of Device Farm's values.
type Device struct {
	ARN          string
	Name         string
	Platform     string
	OS           string
	FormFactor   string
	Manufacturer string
	Availability string
	RemoteAccess bool
}

// SeedDevices is the pool every fake starts with: two Android and two iOS
// phones, a TEMPORARY_NOT_AVAILABLE tablet, two models whose names slug the
// same ("Samsung Galaxy A54 5G" and "Samsung Galaxy A54 (5G)", both 13) and
// one device without remote access.
func SeedDevices() []Device {
	return []Device{
		{ARNPixel8, "Google Pixel 8", "ANDROID", "14", "PHONE", "Google", "HIGHLY_AVAILABLE", true},
		{ARNPixel7, "Google Pixel 7", "ANDROID", "13", "PHONE", "Google", "AVAILABLE", true},
		{ARNIPhone15, "Apple iPhone 15", "IOS", "17.4", "PHONE", "Apple", "HIGHLY_AVAILABLE", true},
		{ARNIPhone14, "Apple iPhone 14", "IOS", "27.0", "PHONE", "Apple", "HIGHLY_AVAILABLE", true},
		{ARNGalaxyTab, "Samsung Galaxy Tab S9", "ANDROID", "14", "TABLET", "Samsung", "TEMPORARY_NOT_AVAILABLE", true},
		{ARNGalaxyA54, "Samsung Galaxy A54 5G", "ANDROID", "13", "PHONE", "Samsung", "BUSY", true},
		{ARNGalaxyA54B, "Samsung Galaxy A54 (5G)", "ANDROID", "13", "PHONE", "Samsung", "AVAILABLE", true},
		{ARNPixel6Local, "Google Pixel 6", "ANDROID", "12", "PHONE", "Google", "HIGHLY_AVAILABLE", false},
	}
}

// Request is one request the fake served.
type Request struct {
	Op     string // the AWS operation ("ListDevices"), "mark:<label>" for Mark, "" for plain HTTP
	Method string
	Path   string // URL path without the query
	Body   []byte // the JSON body; nil for upload PUTs
	At     time.Time
}

// Session is a remote access session as the fake holds it.
type Session struct {
	ARN           string
	ProjectARN    string
	Name          string
	DeviceARN     string
	AppARN        string
	AuxiliaryApps []string
	Parameters    map[string]string
	BillingMethod string
	Status        string
	Result        string
	Message       string
	Created       time.Time
	Started       time.Time
	Installed     []string         // upload ARNs installed into it
	Caps          []map[string]any // alwaysMatch of every WebDriver session opened on it
	WebDrivers    int              // WebDriver sessions open now
	Token         string           // the endpoint path token
	installedAt   time.Time
	pinned        bool
	everRan       bool
	wd            map[string]bool
}

// Upload is an upload as the fake holds it.
type Upload struct {
	ARN            string
	ProjectARN     string
	Name           string
	Type           string
	ContentType    string // from CreateUpload
	PutContentType string // from the PUT
	Size           int64
	Created        time.Time
	Status         string // INITIALIZED until the PUT; then PROCESSING, SUCCEEDED or FAILED
	Message        string
	Metadata       string
	token          string
	putAt          time.Time
	failMsg        string
	failMeta       string
	fixed          bool // seeded with a final status
}

type project struct {
	arn, name string
	created   time.Time
}

type apiErr struct{ code, msg string }

// TB is the part of testing.TB that New needs.
type TB interface {
	Helper()
	Cleanup(func())
}

// Fake is a running fake Device Farm.
type Fake struct {
	srv *httptest.Server

	mu          sync.Mutex
	devices     []Device
	projects    []project
	uploads     []*Upload
	sessions    []*Session
	reqs        []Request
	stops       []string
	fail        map[string]apiErr
	hold        string
	holdDevice  map[string]string
	pageSize    int
	trialTotal  float64
	trialLeft   float64
	uploadDelay time.Duration
	// installLatency: how long after an install the device lists the app
	// (mobile: isAppInstalled), as Device Farm installs asynchronously.
	installLatency time.Duration
	failUpload     *apiErr
	failWD         bool
	noStopMins     bool
}

// New starts a fake and closes it when the test ends.
func New(t TB) *Fake {
	t.Helper()
	f := NewServer()
	t.Cleanup(f.Close)
	return f
}

// NewServer starts a fake; Close stops it.
func NewServer() *Fake {
	f := &Fake{
		devices:    SeedDevices(),
		fail:       map[string]apiErr{},
		holdDevice: map[string]string{},
		pageSize:   3,
		trialTotal: 1000,
		trialLeft:  1000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/remote-endpoint/", f.serveAppium)
	mux.HandleFunc("/upload/", f.serveUpload)
	mux.HandleFunc("/", f.serveAWS)
	f.srv = httptest.NewServer(mux)
	return f
}

// Close stops the server.
func (f *Fake) Close() { f.srv.Close() }

// URL is the API endpoint to configure clients with.
func (f *Fake) URL() string { return f.srv.URL }

// --- scripting ---

// SetPageSize sets how many items each List page holds (default 3).
func (f *Fake) SetPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 1 {
		n = 1
	}
	f.pageSize = n
}

// SetTrialMinutes sets GetAccountSettings' trial minutes.
func (f *Fake) SetTrialMinutes(total, remaining float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trialTotal, f.trialLeft = total, remaining
}

// AddDevice adds a device to the pool.
func (f *Fake) AddDevice(d Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = append(f.devices, d)
}

// SetDevices replaces the pool.
func (f *Fake) SetDevices(ds []Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = append([]Device(nil), ds...)
}

// FailOp makes every call of op (e.g. "CreateRemoteAccessSession") answer
// the AWS error code with msg; an empty code clears it.
func (f *Fake) FailOp(op, code, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == "" {
		delete(f.fail, op)
		return
	}
	f.fail[op] = apiErr{code, msg}
}

// FailCreate is FailOp for CreateRemoteAccessSession.
func (f *Fake) FailCreate(code, msg string) { f.FailOp("CreateRemoteAccessSession", code, msg) }

// Hold keeps every session that is not yet running in state (for example
// PENDING_DEVICE or PENDING_CONCURRENCY); "" lets them run.
func (f *Fake) Hold(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = state
}

// HoldDevice is Hold for the sessions on one device.
func (f *Fake) HoldDevice(deviceARN, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "" {
		delete(f.holdDevice, deviceARN)
		return
	}
	f.holdDevice[deviceARN] = state
}

// SetSessionState pins a session to state; it no longer advances by itself.
func (f *Fake) SetSessionState(arn, state string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(arn)
	if s == nil {
		return false
	}
	s.Status, s.pinned = state, true
	if state == "RUNNING" && !s.everRan {
		s.everRan, s.Started = true, time.Now()
	}
	return true
}

// ExpireSession ends a session as Device Farm does at its time limit.
func (f *Fake) ExpireSession(arn string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(arn)
	if s == nil {
		return false
	}
	s.Status, s.Result, s.Message, s.pinned = "COMPLETED", "STOPPED", "The session reached its time limit", true
	return true
}

// FailUploads makes uploads PUT from now on end FAILED with msg and
// metadata; an empty msg clears it.
func (f *Fake) FailUploads(msg, metadata string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if msg == "" {
		f.failUpload = nil
		return
	}
	f.failUpload = &apiErr{msg, metadata}
}

// SetUploadDelay keeps uploads PROCESSING for d after their PUT.
func (f *Fake) SetUploadDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadDelay = d
}

// SetInstallLatency makes an installed app appear on the device (to mobile:
// isAppInstalled) only d after InstallToRemoteAccessSession answered.
func (f *Fake) SetInstallLatency(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installLatency = d
}

// FailWebDriver makes POST /session on every Appium endpoint fail.
func (f *Fake) FailWebDriver(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWD = fail
}

// OmitStopMinutes makes StopRemoteAccessSession answer without
// deviceMinutes, as Device Farm does while it still counts them.
func (f *Fake) OmitStopMinutes(omit bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noStopMins = omit
}

// EnsureProject returns the ARN of the project named name, creating it
// without recording a request.
func (f *Fake) EnsureProject(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensureProject(name)
}

// SeedUpload adds an upload with a final status (SUCCEEDED, FAILED) and a
// creation time, and returns its ARN.
func (f *Fake) SeedUpload(projectARN, name, typ, status string, created time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.newUpload(projectARN, name, typ, "")
	u.Status, u.Created, u.fixed = status, created, true
	return u.ARN
}

// Mark appends a "mark:<label>" entry to the request log, so a test can
// order its own events among the requests.
func (f *Fake) Mark(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, Request{Op: "mark:" + label, At: time.Now()})
}

// --- inspection ---

// Requests returns every request so far, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Ops returns the AWS operations (and marks) so far, in order.
func (f *Fake) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ops []string
	for _, r := range f.reqs {
		if r.Op != "" {
			ops = append(ops, r.Op)
		}
	}
	return ops
}

// CountOp counts the calls of an AWS operation.
func (f *Fake) CountOp(op string) int {
	n := 0
	for _, o := range f.Ops() {
		if o == op {
			n++
		}
	}
	return n
}

// CreatedSessions returns the ARNs of the sessions created, in order.
func (f *Fake) CreatedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sessions))
	for i, s := range f.sessions {
		out[i] = s.ARN
	}
	return out
}

// StoppedSessions returns the ARN of every StopRemoteAccessSession call, in
// order (a session stopped twice appears twice).
func (f *Fake) StoppedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stops...)
}

// Session returns a copy of a session.
func (f *Fake) Session(arn string) (Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.session(arn)
	if s == nil {
		return Session{}, false
	}
	return copySession(s), true
}

// Sessions returns copies of every session, in creation order.
func (f *Fake) Sessions() []Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Session, len(f.sessions))
	for i, s := range f.sessions {
		out[i] = copySession(s)
	}
	return out
}

// Endpoint returns a session's Appium endpoint URL.
func (f *Fake) Endpoint(arn string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.session(arn); s != nil {
		return f.endpoint(s)
	}
	return ""
}

// Uploads returns copies of every upload, in creation order.
func (f *Fake) Uploads() []Upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Upload, len(f.uploads))
	for i, u := range f.uploads {
		f.settle(u)
		out[i] = *u
	}
	return out
}

// Projects returns the project names, in creation order.
func (f *Fake) Projects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.projects))
	for i, p := range f.projects {
		out[i] = p.name
	}
	return out
}

func copySession(s *Session) Session {
	c := *s
	c.AuxiliaryApps = append([]string(nil), s.AuxiliaryApps...)
	c.Installed = append([]string(nil), s.Installed...)
	c.Caps = append([]map[string]any(nil), s.Caps...)
	c.Parameters = map[string]string{}
	for k, v := range s.Parameters {
		c.Parameters[k] = v
	}
	c.wd = nil
	return c
}

// --- AWS operations ---

func (f *Fake) serveAWS(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), Target)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, Request{Op: op, Method: r.Method, Path: r.URL.Path, Body: body, At: time.Now()})
	if r.Method != http.MethodPost || op == "" {
		writeErr(w, "UnknownOperationException", "not a Device Farm operation")
		return
	}
	if e, ok := f.fail[op]; ok {
		if op == "StopRemoteAccessSession" {
			f.stops = append(f.stops, arnOf(body))
		}
		writeErr(w, e.code, e.msg)
		return
	}
	var in struct {
		Arn                    string `json:"arn"`
		Name                   string `json:"name"`
		NextToken              string `json:"nextToken"`
		Type                   string `json:"type"`
		ProjectArn             string `json:"projectArn"`
		DeviceArn              string `json:"deviceArn"`
		AppArn                 string `json:"appArn"`
		RemoteAccessSessionArn string `json:"remoteAccessSessionArn"`
		ContentType            string `json:"contentType"`
		Filters                []struct {
			Attribute string   `json:"attribute"`
			Operator  string   `json:"operator"`
			Values    []string `json:"values"`
		} `json:"filters"`
		Configuration *struct {
			AuxiliaryApps []string          `json:"auxiliaryApps"`
			BillingMethod string            `json:"billingMethod"`
			Parameters    map[string]string `json:"parameters"`
		} `json:"configuration"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeErr(w, "SerializationException", "bad JSON")
			return
		}
	}
	switch op {
	case "GetAccountSettings":
		writeJSON(w, map[string]any{"accountSettings": map[string]any{
			"awsAccountNumber":             AccountID,
			"trialMinutes":                 map[string]any{"total": f.trialTotal, "remaining": f.trialLeft},
			"maxSlots":                     map[string]int{"00000000-0000-0000-0000-000000000000": 1},
			"unmeteredDevices":             map[string]int{"ANDROID": 0, "IOS": 0},
			"unmeteredRemoteAccessDevices": map[string]int{"ANDROID": 0, "IOS": 0},
			"maxJobTimeoutMinutes":         150,
			"defaultJobTimeoutMinutes":     150,
		}})
	case "ListProjects":
		items := make([]any, len(f.projects))
		for i, p := range f.projects {
			items[i] = map[string]any{"arn": p.arn, "name": p.name, "created": epoch(p.created)}
		}
		f.writePage(w, "projects", items, in.NextToken)
	case "CreateProject":
		if in.Name == "" {
			writeErr(w, "ArgumentException", "name is required")
			return
		}
		arn := f.ensureProject(in.Name)
		p := f.projectByARN(arn)
		writeJSON(w, map[string]any{"project": map[string]any{"arn": p.arn, "name": p.name, "created": epoch(p.created)}})
	case "ListDevices":
		var items []any
		for _, d := range f.devices {
			if matchDevice(d, in.Filters) {
				items = append(items, deviceJSON(d))
			}
		}
		f.writePage(w, "devices", items, in.NextToken)
	case "GetDevice":
		for _, d := range f.devices {
			if d.ARN == in.Arn {
				writeJSON(w, map[string]any{"device": deviceJSON(d)})
				return
			}
		}
		writeErr(w, "NotFoundException", "no device "+in.Arn)
	case "CreateUpload":
		if f.projectByARN(in.ProjectArn) == nil {
			writeErr(w, "NotFoundException", "no project "+in.ProjectArn)
			return
		}
		if in.Name == "" || (in.Type != "ANDROID_APP" && in.Type != "IOS_APP") {
			writeErr(w, "ArgumentException", "name and an app type are required")
			return
		}
		u := f.newUpload(in.ProjectArn, in.Name, in.Type, in.ContentType)
		j := f.uploadJSON(u)
		j["url"] = fmt.Sprintf("%s/upload/%s?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s&X-Amz-Signature=%s",
			f.srv.URL, u.token, "AKIAFAKEFAKEFAKEFAKE%2F20261120%2Fus-west-2%2Fs3%2Faws4_request", randHex(16))
		writeJSON(w, map[string]any{"upload": j})
	case "GetUpload":
		u := f.upload(in.Arn)
		if u == nil {
			writeErr(w, "NotFoundException", "no upload "+in.Arn)
			return
		}
		writeJSON(w, map[string]any{"upload": f.uploadJSON(u)})
	case "ListUploads":
		var items []any
		for _, u := range f.uploads {
			if u.ProjectARN == in.Arn && (in.Type == "" || u.Type == in.Type) {
				items = append(items, f.uploadJSON(u))
			}
		}
		f.writePage(w, "uploads", items, in.NextToken)
	case "DeleteUpload":
		for i, u := range f.uploads {
			if u.ARN == in.Arn {
				f.uploads = append(f.uploads[:i], f.uploads[i+1:]...)
				writeJSON(w, map[string]any{})
				return
			}
		}
		writeErr(w, "NotFoundException", "no upload "+in.Arn)
	case "CreateRemoteAccessSession":
		f.createSession(w, in.ProjectArn, in.DeviceArn, in.AppArn, in.Name, in.Configuration)
	case "GetRemoteAccessSession":
		s := f.session(in.Arn)
		if s == nil {
			writeErr(w, "NotFoundException", "no session "+in.Arn)
			return
		}
		writeJSON(w, map[string]any{"remoteAccessSession": f.sessionJSON(s, true)})
		f.advance(s)
	case "StopRemoteAccessSession":
		f.stops = append(f.stops, in.Arn)
		s := f.session(in.Arn)
		if s == nil {
			writeErr(w, "NotFoundException", "no session "+in.Arn)
			return
		}
		if s.Status == "COMPLETED" {
			writeErr(w, "ArgumentException", "the session has already completed")
			return
		}
		s.Status, s.Result, s.pinned = "COMPLETED", "STOPPED", true
		j := f.sessionJSON(s, true)
		if f.noStopMins {
			delete(j, "deviceMinutes")
		}
		writeJSON(w, map[string]any{"remoteAccessSession": j})
	case "ListRemoteAccessSessions":
		var items []any
		for _, s := range f.sessions {
			if s.ProjectARN == in.Arn {
				items = append(items, f.sessionJSON(s, false))
			}
		}
		f.writePage(w, "remoteAccessSessions", items, in.NextToken)
	case "InstallToRemoteAccessSession":
		s := f.session(in.RemoteAccessSessionArn)
		if s == nil {
			writeErr(w, "NotFoundException", "no session "+in.RemoteAccessSessionArn)
			return
		}
		u := f.upload(in.AppArn)
		if u == nil {
			writeErr(w, "NotFoundException", "no upload "+in.AppArn)
			return
		}
		if s.Status != "RUNNING" {
			writeErr(w, "ArgumentException", "the session is not running")
			return
		}
		s.Installed = append(s.Installed, u.ARN)
		s.installedAt = time.Now()
		writeJSON(w, map[string]any{"appUpload": f.uploadJSON(u)})
	default:
		writeErr(w, "UnknownOperationException", "the fake does not serve "+op)
	}
}

func (f *Fake) createSession(w http.ResponseWriter, projectARN, deviceARN, appARN, name string, cfg *struct {
	AuxiliaryApps []string          `json:"auxiliaryApps"`
	BillingMethod string            `json:"billingMethod"`
	Parameters    map[string]string `json:"parameters"`
}) {
	p := f.projectByARN(projectARN)
	if p == nil {
		writeErr(w, "NotFoundException", "no project "+projectARN)
		return
	}
	var dev *Device
	for i := range f.devices {
		if f.devices[i].ARN == deviceARN {
			dev = &f.devices[i]
		}
	}
	if dev == nil || !dev.RemoteAccess {
		writeErr(w, "ArgumentException", "the device does not support remote access")
		return
	}
	if appARN != "" && f.upload(appARN) == nil {
		writeErr(w, "NotFoundException", "no upload "+appARN)
		return
	}
	pid := arnTail(p.arn)
	s := &Session{
		ARN:        fmt.Sprintf("arn:aws:devicefarm:us-west-2:%s:session:%s/%s/%05d", AccountID, pid, uuid(), 0),
		ProjectARN: p.arn,
		Name:       name,
		DeviceARN:  deviceARN,
		AppARN:     appARN,
		Status:     "PENDING",
		Result:     "PENDING",
		Created:    time.Now(),
		Token:      randHex(16),
		Parameters: map[string]string{},
		wd:         map[string]bool{},
	}
	if cfg != nil {
		s.AuxiliaryApps = cfg.AuxiliaryApps
		s.BillingMethod = cfg.BillingMethod
		for k, v := range cfg.Parameters {
			s.Parameters[k] = v
		}
	}
	f.sessions = append(f.sessions, s)
	writeJSON(w, map[string]any{"remoteAccessSession": f.sessionJSON(s, true)})
}

// advance moves a session one step after a GetRemoteAccessSession:
// PENDING → PREPARING → RUNNING, or into a held state.
func (f *Fake) advance(s *Session) {
	if s.pinned || s.Status == "RUNNING" || s.Status == "COMPLETED" || s.Status == "STOPPING" {
		return
	}
	if h := f.holdDevice[s.DeviceARN]; h != "" {
		s.Status = h
		return
	}
	if f.hold != "" {
		s.Status = f.hold
		return
	}
	if s.Status == "PREPARING" {
		s.Status, s.everRan, s.Started = "RUNNING", true, time.Now()
		return
	}
	s.Status = "PREPARING"
}

func (f *Fake) session(arn string) *Session {
	for _, s := range f.sessions {
		if s.ARN == arn {
			return s
		}
	}
	return nil
}

func (f *Fake) endpoint(s *Session) string {
	return f.srv.URL + "/remote-endpoint/" + s.Token + "/wd/hub"
}

func (f *Fake) sessionJSON(s *Session, full bool) map[string]any {
	j := map[string]any{
		"arn":           s.ARN,
		"name":          s.Name,
		"status":        s.Status,
		"result":        s.Result,
		"created":       epoch(s.Created),
		"billingMethod": s.BillingMethod,
	}
	if s.Message != "" {
		j["message"] = s.Message
	}
	if s.AppARN != "" {
		j["appUpload"] = s.AppARN
	}
	for _, d := range f.devices {
		if d.ARN == s.DeviceARN {
			j["device"] = deviceJSON(d)
		}
	}
	if s.everRan {
		j["started"] = epoch(s.Started)
		m := math.Max(1, math.Ceil(time.Since(s.Started).Minutes()))
		j["deviceMinutes"] = map[string]any{"total": m, "metered": m, "unmetered": 0}
	}
	if full && s.Status == "RUNNING" {
		ep := f.endpoint(s)
		j["endpoints"] = map[string]any{
			"remoteDriverEndpoint": ep,
			"interactiveEndpoint":  "wss://devicefarm-interactive.us-west-2.api.aws/" + s.Token + "?X-Amz-Signature=" + s.Token,
		}
	}
	return j
}

func (f *Fake) ensureProject(name string) string {
	for _, p := range f.projects {
		if p.name == name {
			return p.arn
		}
	}
	p := project{arn: fmt.Sprintf("arn:aws:devicefarm:us-west-2:%s:project:%s", AccountID, uuid()), name: name, created: time.Now()}
	f.projects = append(f.projects, p)
	return p.arn
}

func (f *Fake) projectByARN(arn string) *project {
	for i := range f.projects {
		if f.projects[i].arn == arn {
			return &f.projects[i]
		}
	}
	return nil
}

func (f *Fake) newUpload(projectARN, name, typ, contentType string) *Upload {
	u := &Upload{
		ARN:         fmt.Sprintf("arn:aws:devicefarm:us-west-2:%s:upload:%s/%s", AccountID, arnTail(projectARN), uuid()),
		ProjectARN:  projectARN,
		Name:        name,
		Type:        typ,
		ContentType: contentType,
		Created:     time.Now(),
		Status:      "INITIALIZED",
		token:       randHex(16),
	}
	f.uploads = append(f.uploads, u)
	return u
}

func (f *Fake) upload(arn string) *Upload {
	for _, u := range f.uploads {
		if u.ARN == arn {
			return u
		}
	}
	return nil
}

// settle moves an upload past PROCESSING once its delay is over.
func (f *Fake) settle(u *Upload) {
	if u.fixed || u.putAt.IsZero() || u.Status != "PROCESSING" {
		return
	}
	if time.Since(u.putAt) < f.uploadDelay {
		return
	}
	if u.failMsg != "" {
		u.Status, u.Message, u.Metadata = "FAILED", u.failMsg, u.failMeta
		return
	}
	u.Status = "SUCCEEDED"
}

func (f *Fake) uploadJSON(u *Upload) map[string]any {
	f.settle(u)
	j := map[string]any{
		"arn":      u.ARN,
		"name":     u.Name,
		"type":     u.Type,
		"status":   u.Status,
		"created":  epoch(u.Created),
		"category": "PRIVATE",
	}
	if u.ContentType != "" {
		j["contentType"] = u.ContentType
	}
	if u.Message != "" {
		j["message"] = u.Message
	}
	if u.Metadata != "" {
		j["metadata"] = u.Metadata
	}
	return j
}

func (f *Fake) writePage(w http.ResponseWriter, key string, items []any, token string) {
	start := 0
	if token != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(token, "page-"))
		if err != nil || n < 0 || n > len(items) {
			writeErr(w, "ArgumentException", "bad nextToken")
			return
		}
		start = n
	}
	end := start + f.pageSize
	if end > len(items) {
		end = len(items)
	}
	out := map[string]any{key: append([]any{}, items[start:end]...)}
	if end < len(items) {
		out["nextToken"] = fmt.Sprintf("page-%d", end)
	}
	writeJSON(w, out)
}

func matchDevice(d Device, filters []struct {
	Attribute string   `json:"attribute"`
	Operator  string   `json:"operator"`
	Values    []string `json:"values"`
}) bool {
	for _, flt := range filters {
		var v string
		switch flt.Attribute {
		case "REMOTE_ACCESS_ENABLED":
			v = strings.ToUpper(strconv.FormatBool(d.RemoteAccess))
		case "PLATFORM":
			v = d.Platform
		case "AVAILABILITY":
			v = d.Availability
		case "FORM_FACTOR":
			v = d.FormFactor
		case "MANUFACTURER":
			v = d.Manufacturer
		default:
			continue
		}
		ok := false
		for _, want := range flt.Values {
			if strings.EqualFold(v, want) {
				ok = true
			}
		}
		if (flt.Operator == "NOT_EQUALS" || flt.Operator == "NOT_IN") == ok {
			return false
		}
	}
	return true
}

func deviceJSON(d Device) map[string]any {
	return map[string]any{
		"arn":                 d.ARN,
		"name":                d.Name,
		"platform":            d.Platform,
		"os":                  d.OS,
		"formFactor":          d.FormFactor,
		"manufacturer":        d.Manufacturer,
		"availability":        d.Availability,
		"remoteAccessEnabled": d.RemoteAccess,
		"model":               d.Name,
		"type":                "PHYSICAL",
	}
}

// --- uploads ---

func (f *Fake) serveUpload(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/upload/")
	n, _ := io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, Request{Method: r.Method, Path: r.URL.Path, At: time.Now()})
	var u *Upload
	for _, x := range f.uploads {
		if x.token == token {
			u = x
		}
	}
	switch {
	case u == nil:
		http.Error(w, "<Error><Code>NoSuchUpload</Code></Error>", http.StatusNotFound)
	case r.Method != http.MethodPut:
		http.Error(w, "<Error><Code>MethodNotAllowed</Code></Error>", http.StatusMethodNotAllowed)
	case r.URL.Query().Get("X-Amz-Signature") == "":
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
	default:
		u.PutContentType, u.Size, u.putAt, u.Status = r.Header.Get("Content-Type"), n, time.Now(), "PROCESSING"
		if f.failUpload != nil {
			u.failMsg, u.failMeta = f.failUpload.code, f.failUpload.msg
		}
		w.WriteHeader(http.StatusOK)
	}
}

// --- Appium ---

const elementKey = "element-6066-11e4-a52e-4f735466cecf"

func (f *Fake) serveAppium(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, Request{Method: r.Method, Path: r.URL.Path, Body: body, At: time.Now()})
	rest := strings.TrimPrefix(r.URL.Path, "/remote-endpoint/")
	token, sub, _ := strings.Cut(rest, "/")
	var s *Session
	for _, x := range f.sessions {
		if x.Token == token {
			s = x
		}
	}
	sub = "/" + sub
	if s == nil || !strings.HasPrefix(sub, "/wd/hub") {
		wdErr(w, http.StatusNotFound, "unknown command", "no such endpoint")
		return
	}
	if s.Status != "RUNNING" {
		wdErr(w, http.StatusNotFound, "invalid session id", "the remote access session is "+s.Status)
		return
	}
	sub = strings.TrimPrefix(sub, "/wd/hub")
	switch {
	case sub == "/status":
		wdOK(w, map[string]any{"ready": true, "message": "fake Device Farm Appium"})
		return
	case sub == "/session" && r.Method == http.MethodPost:
		f.newWebDriver(w, s, body)
		return
	case !strings.HasPrefix(sub, "/session/"):
		wdErr(w, http.StatusNotFound, "unknown command", sub)
		return
	}
	id, cmd, _ := strings.Cut(strings.TrimPrefix(sub, "/session/"), "/")
	if !s.wd[id] {
		wdErr(w, http.StatusNotFound, "invalid session id", "no WebDriver session "+id)
		return
	}
	cmd = "/" + cmd
	ios := f.isIOS(s)
	switch {
	case cmd == "/" && r.Method == http.MethodDelete:
		delete(s.wd, id)
		s.WebDrivers = len(s.wd)
		wdOK(w, nil)
	case cmd == "/":
		wdOK(w, s.Caps[len(s.Caps)-1])
	case cmd == "/window/rect":
		wdOK(w, map[string]any{"x": 0, "y": 0, "width": 1080, "height": 2400})
	case cmd == "/appium/settings" && r.Method == http.MethodGet:
		wdOK(w, map[string]any{})
	case cmd == "/orientation" && r.Method == http.MethodGet:
		wdOK(w, "PORTRAIT")
	case cmd == "/screenshot":
		wdOK(w, screenshot)
	case cmd == "/source":
		if ios {
			wdOK(w, iosSource)
		} else {
			wdOK(w, androidSource)
		}
	case cmd == "/element" || cmd == "/element/active":
		wdOK(w, map[string]any{elementKey: "el-1"})
	case cmd == "/elements":
		wdOK(w, []any{map[string]any{elementKey: "el-1"}})
	case strings.HasPrefix(cmd, "/element/"):
		_, what, _ := strings.Cut(strings.TrimPrefix(cmd, "/element/"), "/")
		switch {
		case what == "rect":
			wdOK(w, map[string]any{"x": 100, "y": 200, "width": 200, "height": 60})
		case what == "text" || strings.HasPrefix(what, "attribute/"):
			wdOK(w, "Button")
		case what == "displayed" || what == "enabled":
			wdOK(w, true)
		case what == "selected":
			wdOK(w, false)
		default: // click, clear, value
			wdOK(w, nil)
		}
	case cmd == "/appium/device/terminate_app":
		wdOK(w, true)
	case cmd == "/execute/sync" && bytes.Contains(body, []byte("mobile: isAppInstalled")):
		wdOK(w, s.installedAt.IsZero() || time.Since(s.installedAt) >= f.installLatency)
	default: // settings, actions, activate_app, execute/sync, timeouts …
		wdOK(w, nil)
	}
}

func (f *Fake) isIOS(s *Session) bool {
	for _, d := range f.devices {
		if d.ARN == s.DeviceARN {
			return d.Platform == "IOS"
		}
	}
	return false
}

func (f *Fake) newWebDriver(w http.ResponseWriter, s *Session, body []byte) {
	if f.failWD {
		wdErr(w, http.StatusInternalServerError, "session not created", "the fake refuses WebDriver sessions")
		return
	}
	var req struct {
		Capabilities struct {
			AlwaysMatch map[string]any `json:"alwaysMatch"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(body, &req)
	caps := req.Capabilities.AlwaysMatch
	if caps == nil {
		caps = map[string]any{}
	}
	for _, k := range []string{"udid", "appium:udid", "platformVersion", "appium:platformVersion", "app", "appium:app", "deviceName", "appium:deviceName"} {
		if _, ok := caps[k]; ok {
			wdErr(w, http.StatusBadRequest, "invalid argument", "strictCaps: the capability "+k+" is not allowed")
			return
		}
	}
	id := randHex(8)
	s.wd[id] = true
	s.WebDrivers = len(s.wd)
	s.Caps = append(s.Caps, caps)
	answer := map[string]any{}
	for k, v := range caps {
		answer[k] = v
	}
	for _, d := range f.devices {
		if d.ARN == s.DeviceARN {
			answer["appium:deviceName"] = d.Name
			answer["appium:platformVersion"] = d.OS
		}
	}
	answer["isRealDevice"] = true
	wdOK(w, map[string]any{"sessionId": id, "capabilities": answer})
}

const androidSource = `<?xml version="1.0" encoding="UTF-8"?>
<hierarchy rotation="0"><android.widget.FrameLayout class="android.widget.FrameLayout" package="com.example" bounds="[0,0][1080,2400]" enabled="true" displayed="true"><android.widget.Button class="android.widget.Button" text="Button" resource-id="com.example:id/button" content-desc="" package="com.example" bounds="[100,200][300,260]" enabled="true" displayed="true" clickable="true" focusable="true" checked="false" selected="false"/></android.widget.FrameLayout></hierarchy>`

const iosSource = `<?xml version="1.0" encoding="UTF-8"?>
<XCUIElementTypeApplication type="XCUIElementTypeApplication" name="Example" label="Example" enabled="true" visible="true" x="0" y="0" width="390" height="844"><XCUIElementTypeButton type="XCUIElementTypeButton" name="Button" label="Button" enabled="true" visible="true" accessible="true" x="100" y="200" width="200" height="60"/></XCUIElementTypeApplication>`

// screenshot is a base64 PNG of a small grey image.
var screenshot = func() string {
	img := image.NewRGBA(image.Rect(0, 0, 4, 8))
	for x := 0; x < 4; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{0x80, 0x80, 0x80, 0xff})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}()

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.Header().Set("X-Amzn-ErrorType", code)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": msg})
}

func wdOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"value": v})
}

func wdErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{"error": code, "message": msg}})
}

// arnOf reads the arn field of a request body.
func arnOf(body []byte) string {
	var in struct {
		Arn string `json:"arn"`
	}
	_ = json.Unmarshal(body, &in)
	return in.Arn
}

// arnTail is the resource id after the last ":".
func arnTail(arn string) string {
	if i := strings.LastIndex(arn, ":"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

func epoch(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	h := randHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
