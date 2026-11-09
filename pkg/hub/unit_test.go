package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub/hubtest"
)

func TestFrames_EncodeDecode(t *testing.T) {
	f := Frame{T: frameReq, ID: "d1", Method: "POST", Path: "/v1/x", Query: "a=1", Headers: http.Header{"A": {"b"}},
		Principal: "p", Body: []byte{0, 1, 2}, Peers: []PeerRef{{Name: "n", Addr: "h:1"}}}
	data, err := encodeFrame(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFrame(data)
	if err != nil || got.ID != "d1" || string(got.Body) != string([]byte{0, 1, 2}) || got.Headers.Get("A") != "b" || got.Peers[0].Addr != "h:1" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"{", `{"id":"x"}`} {
		if _, err := decodeFrame([]byte(bad)); err == nil {
			t.Fatalf("%s decoded", bad)
		}
	}
	id, payload, err := decodeData(encodeData("a12", []byte("body")))
	if err != nil || id != "a12" || string(payload) != "body" {
		t.Fatalf("data: %q %q %v", id, payload, err)
	}
	if id, payload, err := decodeData(encodeData("x", nil)); err != nil || id != "x" || len(payload) != 0 {
		t.Fatal("end frame")
	}
	for _, bad := range [][]byte{nil, {5, 'a'}} {
		if _, _, err := decodeData(bad); err == nil {
			t.Fatalf("%v decoded", bad)
		}
	}
}

func TestRewrite_IDs(t *testing.T) {
	for _, c := range []struct{ in, local, hub string }{
		{"a@h", "a", "h"}, {"a", "a", ""}, {"@h", "@h", ""}, {"a@", "a@", ""}, {"x@y@h", "x@y", "h"}, {"10.0.0.2:5555", "10.0.0.2:5555", ""},
	} {
		if l, h := splitID(c.in); l != c.local || h != c.hub {
			t.Errorf("splitID(%q) = %q %q", c.in, l, h)
		}
	}
	if qualify("a", "h") != "a@h" || qualify("a@x", "h") != "a@x" || qualify("", "h") != "" {
		t.Fatal("qualify")
	}
	in := `{"ok":true,"n":12345678901234567890,"devices":[{"id":"d1","platform":"ios","ready":true,"lease":{"holder":"x","expiresAt":"t"}}],
"lease":{"id":"L1","device":"d1","expiresAt":"t","ttlSec":600},"device":"d1","error":{"details":{"lease":"L2","leaseDevice":"d2","candidates":["c1","c2@o"],"attached":["d3"]}},
"catalog":{"id":"rt","platform":"ios"}}`
	out := string(qualifyJSON([]byte(in), "h", true))
	for _, want := range []string{`"id": "d1@h"`, `"localId": "d1"`, `"hub": "h"`, `"id": "L1@h"`, `"device": "d1@h"`, `"lease": "L2@h"`,
		`"leaseDevice": "d2@h"`, `"c1@h"`, `"c2@o"`, `"d3@h"`, `12345678901234567890`, `"id": "rt"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if string(qualifyJSON([]byte("not json"), "h", false)) != "not json" {
		t.Fatal("non-JSON")
	}
	if got := string(qualifyJSON([]byte(`{"device":"x"}`), "h", false)); got != `{"device":"x@h"}` {
		t.Fatalf("compact: %s", got)
	}
}

func TestRewrite_QualifyWriter(t *testing.T) {
	// JSON is rewritten whole, with its length.
	rec := httptest.NewRecorder()
	q := &qualifyWriter{w: rec, hub: "h"}
	q.Header().Set("Content-Type", "application/json")
	q.WriteHeader(201)
	_, _ = q.Write([]byte(`{"device":`))
	_, _ = q.Write([]byte(`"d"}`))
	q.Flush()
	q.finish()
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"d@h"`) || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	// An event stream is rewritten line by line, across writes.
	rec = httptest.NewRecorder()
	q = &qualifyWriter{w: rec, hub: "h"}
	q.Header().Set("Content-Type", "text/event-stream")
	_, _ = q.Write([]byte("id: 1\nevent: step\ndata: {\"device\":"))
	_, _ = q.Write([]byte("\"d\"}\n\n: keepalive\n"))
	q.Flush()
	q.finish()
	if !strings.Contains(rec.Body.String(), `data: {"device":"d@h"}`) || !strings.Contains(rec.Body.String(), ": keepalive") || !rec.Flushed {
		t.Fatalf("sse: %q", rec.Body)
	}
	// Anything else passes through.
	rec = httptest.NewRecorder()
	q = &qualifyWriter{w: rec, hub: "h"}
	q.Header().Set("Content-Type", "image/png")
	_, _ = q.Write([]byte(`{"device":"d"}`))
	q.finish()
	if rec.Body.String() != `{"device":"d"}` {
		t.Fatalf("raw: %s", rec.Body)
	}
	// The caller's own JSON (an artifact) passes through, streamed; an
	// error from the same route is qualified.
	rec = httptest.NewRecorder()
	q = &qualifyWriter{w: rec, hub: "h", userData: true}
	q.Header().Set("Content-Type", "application/json")
	_, _ = q.Write([]byte(`{"device":"d"}`))
	q.Flush()
	q.finish()
	if rec.Body.String() != `{"device":"d"}` || !rec.Flushed {
		t.Fatalf("user data: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	q = &qualifyWriter{w: rec, hub: "h", userData: true}
	q.Header().Set("Content-Type", "application/json")
	q.WriteHeader(http.StatusConflict)
	_, _ = q.Write([]byte(`{"error":{"details":{"device":"d"}}}`))
	q.finish()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"d@h"`) {
		t.Fatalf("user-data error: %d %s", rec.Code, rec.Body)
	}
	for target, want := range map[string]bool{
		"GET /v1/devices/x/artifacts/a/flow.json": true, "GET /v1/devices/x/artifacts": false,
		"GET /v1/devices/x/vars": true, "PUT /v1/devices/x/vars": true, "POST /v1/devices/x/eval": true,
		"GET /v1/devices/x/hierarchy": true, "POST /v1/devices/x/steps": false, "GET /v1/devices/x": false,
		"GET /v1/leases/x/vars": false,
	} {
		method, path, _ := strings.Cut(target, " ")
		if got := userDataRoute(httptest.NewRequest(method, path, nil)); got != want {
			t.Errorf("userDataRoute(%s) = %v", target, got)
		}
	}
}

func TestRewrite_SSEBlocks(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(": connected\n\nid: 1\ndata: {}\n\n: keepalive\n\nid: 2\ndata: {}\n\nid: 3\n"))
	out := make(chan string, 8)
	sseBlocks(r, out, make(chan struct{}))
	close(out)
	var got []string
	for b := range out {
		got = append(got, b)
	}
	if len(got) != 2 || got[0] != "id: 1\ndata: {}\n\n" {
		t.Fatalf("blocks: %q", got)
	}
	// A reader nobody drains stops on done.
	done := make(chan struct{})
	close(done)
	sseBlocks(bufio.NewReader(strings.NewReader("data: {}\n\n")), make(chan string), done)
}

func TestDiscover_Helpers(t *testing.T) {
	if got := txtRecord("hub-a", "1", true); strings.Join(got, ",") != "name=hub-a,api=1,tls=1" {
		t.Fatalf("txt: %v", got)
	}
	if got := txtRecord("hub-a", "1", false); got[2] != "tls=0" {
		t.Fatal("tls=0")
	}
	f, ok := foundFromEntry("inst", []net.IP{net.ParseIP("10.0.0.2")}, 7433, []string{"name=hub-b", "api=1", "tls=1", "junk"})
	if !ok || f.Name != "hub-b" || f.Addr != "https://10.0.0.2:7433" || f.APIVersion != "1" {
		t.Fatalf("found: %+v", f)
	}
	if f, ok := foundFromEntry("inst", []net.IP{net.ParseIP("10.0.0.2")}, 7433, nil); !ok || f.Name != "inst" || f.Addr != "10.0.0.2:7433" {
		t.Fatalf("instance name: %+v", f)
	}
	// Of several addresses, the first that accepts a connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	probeTimeout = 200 * time.Millisecond
	if f, _ := foundFromEntry("i", []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("127.0.0.1")}, port, nil); !strings.HasPrefix(f.Addr, "127.0.0.1:") {
		t.Fatalf("reachable: %+v", f)
	}
	if f, _ := foundFromEntry("i", []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")}, port, nil); !strings.HasPrefix(f.Addr, "192.0.2.1:") {
		t.Fatalf("none reachable: %+v", f)
	}
	if _, ok := foundFromEntry("inst", nil, 7433, nil); ok {
		t.Fatal("no address")
	}
	if describeFound(nil) != "no other hub" || describeFound([]Found{{Name: "a", Addr: "h:1"}}) != "a at h:1" {
		t.Fatal("describe")
	}
}

func TestDiscover_Zeroconf(t *testing.T) {
	if testing.Short() {
		t.Skip("mDNS")
	}
	name := "hubtest-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	stop, err := Zeroconf{}.Advertise(name, 17999, txtRecord(name, daemon.APIVersion, false))
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	found, err := Zeroconf{}.Browse(ctx, 2*time.Second)
	if err != nil {
		t.Skipf("browse: %v", err)
	}
	for _, f := range found {
		if f.Name == name {
			if !strings.HasSuffix(f.Addr, ":17999") {
				t.Fatalf("addr: %+v", f)
			}
			return
		}
	}
	t.Skipf("mDNS did not loop back here (found %v)", found)
}

func TestMesh_Addresses(t *testing.T) {
	for in, want := range map[string]string{
		"h": "h:7433", "h:9": "h:9", "https://h": "https://h:7433", "http://h:1/": "h:1", "[::1]": "[::1]:7433",
		"::1": "[::1]:7433", "HTTPS://h:9": "https://h:9", " h ": "h:7433",
	} {
		if got, err := ParseAddr(in, DefaultPort); err != nil || got != want {
			t.Errorf("ParseAddr(%q) = %q %v", in, got, err)
		}
	}
	// --peer takes what --target-hub takes: one parser for both.
	for _, bad := range []string{"", "h/x", "https://", "ftp://h", "user@h", "h h", "h:0", "h:99999", "h:x", ":9"} {
		if _, err := ParseAddr(bad, DefaultPort); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if wsURL("h:1", false) != "ws://h:1/v1/hub/link" || wsURL("https://h:1", true) != "wss://h:1/v1/hub/link?bulk=1" {
		t.Fatal("wsURL")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:5555"
	r.Header.Set(headerHubPort, "7433")
	r.Header.Set(headerHubTLS, "1")
	if got := peerAddr(r); got != "https://10.0.0.5:7433" {
		t.Fatalf("peerAddr: %s", got)
	}
	r.Header.Set(headerHubAddr, "hub-b.example:9")
	if got := peerAddr(r); got != "https://hub-b.example:9" {
		t.Fatalf("advertised: %s", got)
	}
	// An advertised address gets the listener port when it names none, and
	// https:// from a TLS hub.
	r.Header.Set(headerHubAddr, "10.0.0.1")
	if got := peerAddr(r); got != "https://10.0.0.1:7433" {
		t.Fatalf("advertised host on a TLS hub: %s", got)
	}
	r.Header.Set(headerHubPort, "7500")
	r.Header.Set(headerHubAddr, "10.0.0.1:7433")
	if got := peerAddr(r); got != "https://10.0.0.1:7433" {
		t.Fatalf("advertised host:port on a TLS hub: %s", got)
	}
	r.Header.Del(headerHubTLS)
	r.Header.Set(headerHubAddr, "hub-b.example")
	if got := peerAddr(r); got != "hub-b.example:7500" {
		t.Fatalf("advertised host: %s", got)
	}
	for _, c := range []struct {
		in   string
		port int
		tls  bool
		want string
	}{
		{"10.0.0.1:7433", 7433, true, "https://10.0.0.1:7433"},
		{"10.0.0.1", 7444, true, "https://10.0.0.1:7444"},
		{"https://h", 0, false, "https://h:7433"},
		{"http://h:9", 9, false, "h:9"},
		{"h", 9, false, "h:9"},
	} {
		if got, err := advertisedAddr(c.in, c.port, c.tls); err != nil || got != c.want {
			t.Errorf("advertisedAddr(%q, %d, %v) = %q %v", c.in, c.port, c.tls, got, err)
		}
	}
	if _, err := advertisedAddr("h/x", 1, false); err == nil {
		t.Error("bad advertised address accepted")
	}
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	if peerAddr(r) != "" {
		t.Fatal("no port, no address")
	}
	if !ValidName("hub-a") || ValidName("Hub") || ValidName("") || ValidName(strings.Repeat("a", 33)) {
		t.Fatal("ValidName")
	}
	if !isRefusal(daemon.Errorf(daemon.CodeHubNameTaken, "x")) || isRefusal(daemon.Errorf(daemon.CodePeerUnavailable, "x")) {
		t.Fatal("isRefusal")
	}
	if orUnknown("") != "not linked yet" || orUnknown("x") != "x" {
		t.Fatal("orUnknown")
	}
	resp := &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(""))}
	if e := refusalFrom(resp); isRefusal(asDaemonErr(e)) {
		t.Fatal("a 5xx is not a refusal")
	}
	resp = &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("<html>"))}
	if e := refusalFrom(resp); e == nil || isRefusal(asDaemonErr(e)) || asDaemonErr(e).Code != daemon.CodeInternal {
		t.Fatalf("a 404 without an envelope is retried, not refused: %v", e)
	}
	if principal(&http.Request{}, "h") != "unix socket on hub h" || principal(&http.Request{RemoteAddr: "1.2.3.4:5"}, "h") != "1.2.3.4:5" {
		t.Fatal("principal")
	}
	if needsBulk(httptest.NewRequest(http.MethodGet, "/v1/devices/x/info", nil)) ||
		!needsBulk(httptest.NewRequest(http.MethodGet, "/v1/devices/x/artifacts/a.png", nil)) ||
		!needsBulk(httptest.NewRequest(http.MethodPut, "/v1/devices/x/blobs/abc", nil)) {
		t.Fatal("needsBulk")
	}
	big := httptest.NewRequest(http.MethodPost, "/v1/devices/x/steps", nil)
	big.ContentLength = maxInlineBody + 1
	if !needsBulk(big) {
		t.Fatal("big body")
	}
}

func asDaemonErr(err error) *daemon.Error {
	if e, ok := err.(*daemon.Error); ok {
		return e
	}
	return daemon.Errorf(daemon.CodeInternal, "%v", err)
}

func TestRouter_Recorder(t *testing.T) {
	rec := newRecorder()
	rec.Header().Set("Content-Length", "3")
	rec.Header().Set("X-A", "1")
	_, _ = rec.Write([]byte("abc"))
	rec.WriteHeader(500)
	rec.Flush()
	out := httptest.NewRecorder()
	rec.copyTo(out)
	if out.Code != 200 || out.Body.String() != "abc" || out.Header().Get("X-A") != "1" {
		t.Fatalf("copy: %d %s", out.Code, out.Body)
	}
	pw := &pipeWriter{h: http.Header{}}
	pw.WriteHeader(200)
	pw.Flush()
	if pw.Header() == nil {
		t.Fatal("header")
	}
	if envelopeCode([]byte("x")) != "" {
		t.Fatal("envelopeCode")
	}
	var v map[string]any
	if json.Unmarshal(mustJSON(map[string]int{"a": 1}), &v) != nil {
		t.Fatal("mustJSON")
	}
}

// With 150 ms between the client and the entry hub, 20 commands take 20
// round trips, and a bundled flow takes one run request.
func TestMesh_WANRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	a, b := twoHubs(t)
	p, err := hubtest.Start(hubtest.ProxyConfig{Listen: "127.0.0.1:0", Target: b.addr, RTT: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c := daemon.NewHTTPClient("http://"+p.Addr(), testToken, nil)
	ctx := context.Background()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLease("a1@hub-a", lr.Lease.ID)
	before := a.count("POST /v1/devices/a1/commands/tapOn")
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := c.Command(ctx, "a1@hub-a", "tapOn", map[string]any{"text": "x"}, true); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	rtts := float64(elapsed) / float64(150*time.Millisecond)
	// At least one 150 ms round trip per command (they are sequential); the
	// upper bound is loose for a loaded machine running the suite with -race.
	if rtts < 19 || rtts > 60 || a.count("POST /v1/devices/a1/commands/tapOn")-before != 20 {
		t.Fatalf("20 commands: %v (%.1f round trips)", elapsed, rtts)
	}
	flows := a.count("POST /v1/devices/a1/steps")
	start = time.Now()
	res, err := c.Steps(ctx, "a1@hub-a", daemon.StepsRequest{YAML: "- tapOn: a\n- tapOn: b\n- tapOn: c\n- tapOn: d\n- tapOn: e\n"}, true)
	if err != nil || !res.OK {
		t.Fatalf("steps: %+v %v", res, err)
	}
	// One request carries all five steps: the owner sees a single /steps
	// call. Wall-clock time is not asserted; a loaded machine stretches it.
	if took := time.Since(start); took < 150*time.Millisecond || a.count("POST /v1/devices/a1/steps") != flows+1 {
		t.Fatalf("five steps: %v, %d requests", took, a.count("POST /v1/devices/a1/steps")-flows)
	}
}

func TestDiscover_DNSSD(t *testing.T) {
	old := dnssd
	defer func() { dnssd = old }()
	answers := map[string][]string{
		"-B": {"Browsing for _maestro-d._tcp.local.", "Timestamp     A/R    Flags  if Domain               Service Type         Instance Name",
			"21:43:37.891  Add        3  14 local.               _maestro-d._tcp.     hub-a",
			"21:43:37.891  Add        3  23 local.               _maestro-d._tcp.     hub-a",
			"21:43:38.003  Add        2  14 local.               _maestro-d._tcp.     hub-b",
			"21:43:38.003  Add        2  14 local.               _maestro-d._tcp.     hub-gone"},
		"-L hub-a":    {"21:43:41.027  hub-a._maestro-d._tcp.local. can be reached at hub-a.local.:7433 (interface 14)", " name=hub-a api=1 tls=1"},
		"-L hub-b":    {"21:43:41.027  hub-b._maestro-d._tcp.local. can be reached at hub-b.local.:7444 (interface 14)", " name=hub-b api=1 tls=0"},
		"-L hub-gone": {"21:43:41.027  ...STARTING..."},
		"-G hub-a.local.": {"Timestamp     A/R  Flags         IF  Hostname          Address        TTL",
			"21:43:44.250  Add  40000002      14  hub-a.local.      10.0.0.2       120"},
		"-G hub-b.local.": {"21:43:44.250  Add  40000002      14  hub-b.local.      10.0.0.3       120"},
	}
	dnssd = func(ctx context.Context, stop func(string) bool, args ...string) error {
		key := args[0]
		switch args[0] {
		case "-L":
			key += " " + args[1]
		case "-G":
			key += " " + args[2]
		}
		for _, line := range answers[key] {
			if stop(line) {
				return nil
			}
		}
		<-ctx.Done()
		return nil
	}
	found, err := browseDNSSD(context.Background(), 50*time.Millisecond)
	if err != nil || len(found) != 2 || found[0].Addr != "https://10.0.0.2:7433" || found[1].Addr != "10.0.0.3:7444" || found[1].Name != "hub-b" {
		t.Fatalf("found: %+v %v", found, err)
	}
	dnssd = func(context.Context, func(string) bool, ...string) error { return io.EOF }
	if _, err := browseDNSSD(context.Background(), time.Millisecond); err == nil {
		t.Fatal("dns-sd failure")
	}
	// Through the real dns-sd: the stop function or the deadline ends the
	// run. dns-sd prints nothing into a pipe until it has an answer, so with
	// no hub on the LAN only the deadline ends it.
	dnssd = old
	if browseGOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		n := 0
		start := time.Now()
		if err := dnssd(ctx, func(string) bool { n++; return true }, "-B", ServiceType, "local."); err != nil {
			t.Fatalf("dns-sd: %v", err)
		}
		if n > 1 || time.Since(start) > 10*time.Second {
			t.Fatalf("lines %d after %v", n, time.Since(start))
		}
	}
}

// The router checks a client's bearer token with the daemon's own check:
// the whole "Bearer <token>" header, and every header on a mesh without a
// client token.
func TestMesh_ClientAuthorizedMatchesTheDaemon(t *testing.T) {
	m := New(fastConfig("hub-auth"))
	for header, want := range map[string]bool{"Bearer " + testToken: true, testToken: false, "Bearer " + testToken + "x": false, "": false} {
		r := httptest.NewRequest(http.MethodGet, "/v1/hub", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := m.clientAuthorized(r); got != want || got != daemon.BearerAuthorized(header, testToken) {
			t.Errorf("header %q: authorized %v, want %v", header, got, want)
		}
	}
	m.cfg.Token = ""
	if !m.clientAuthorized(httptest.NewRequest(http.MethodGet, "/v1/hub", nil)) {
		t.Fatal("a mesh without a client token refused a client")
	}
}

// A qualifyWriter unwraps to the request's writer, so a handler behind it
// can turn on full duplex (as serveLeased does for an answer it gives
// while the handler still reads the body).
func TestQualifyWriter_UnwrapsForTheResponseController(t *testing.T) {
	got := make(chan error, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := &qualifyWriter{w: w, hub: "h"}
		got <- http.NewResponseController(q).EnableFullDuplex()
		q.finish()
	}))
	defer hs.Close()
	resp, err := http.Get(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-got; err != nil {
		t.Fatalf("full duplex through a qualifyWriter: %v", err)
	}
}
