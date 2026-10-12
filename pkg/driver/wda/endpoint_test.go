package wda

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testUDID = "00000000-0000000000000000"

// fakeCloser records whether it was closed.
type fakeCloser struct{ closed bool }

func (c *fakeCloser) Close() error { c.closed = true; return nil }

// failingDeps returns deps where every path fails, so a test can enable
// just the ones it is about.
func failingDeps(t *testing.T) reachDeps {
	t.Helper()
	return reachDeps{
		usbmuxForward: func(string, uint16) (io.Closer, error) { return nil, errors.New("not listed") },
		agentTunnel:   func(string) (goIOSTunnel, error) { return goIOSTunnel{}, errors.New("no agent") },
		tunnelForward: func(goIOSTunnel, uint16) (io.Closer, error) {
			t.Fatal("tunnelForward called without an agent tunnel")
			return nil, nil
		},
		coreDeviceIP: func(string) (string, error) { return "", errors.New("no tunnel") },
		probe:        func(string) error { return errors.New("refused") },
	}
}

func TestReachWDAPrefersUSBMux(t *testing.T) {
	c := &fakeCloser{}
	d := failingDeps(t)
	d.usbmuxForward = func(udid string, port uint16) (io.Closer, error) {
		if udid != testUDID || port != 8480 {
			t.Fatalf("usbmuxForward(%q, %d)", udid, port)
		}
		return c, nil
	}
	d.agentTunnel = func(string) (goIOSTunnel, error) {
		t.Fatal("agentTunnel called although usbmuxd listed the device")
		return goIOSTunnel{}, nil
	}
	ep, err := reachWDA(testUDID, 8480, "http://192.0.2.1:8480", d)
	if err != nil {
		t.Fatal(err)
	}
	if ep.baseURL != "http://localhost:8480" || ep.path != reachUSBMux || ep.closer != c {
		t.Fatalf("got %+v", ep)
	}
}

func TestReachWDAFallsBackToGoIOSTunnel(t *testing.T) {
	c := &fakeCloser{}
	d := failingDeps(t)
	d.agentTunnel = func(string) (goIOSTunnel, error) { return goIOSTunnel{Address: "fd00::1"}, nil }
	d.tunnelForward = func(tun goIOSTunnel, port uint16) (io.Closer, error) {
		if tun.Address != "fd00::1" || port != 8480 {
			t.Fatalf("tunnelForward(%+v, %d)", tun, port)
		}
		return c, nil
	}
	d.probe = func(u string) error {
		if u != "http://127.0.0.1:8480" {
			return errors.New("unexpected probe " + u)
		}
		return nil
	}
	ep, err := reachWDA(testUDID, 8480, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if ep.baseURL != "http://127.0.0.1:8480" || ep.path != reachGoIOSTunnel || ep.closer != c || c.closed {
		t.Fatalf("got %+v closed=%v", ep, c.closed)
	}
}

func TestReachWDAClosesUnansweredTunnelAndUsesCoreDevice(t *testing.T) {
	c := &fakeCloser{}
	d := failingDeps(t)
	d.agentTunnel = func(string) (goIOSTunnel, error) { return goIOSTunnel{Address: "fd00::1"}, nil }
	d.tunnelForward = func(goIOSTunnel, uint16) (io.Closer, error) { return c, nil }
	d.coreDeviceIP = func(string) (string, error) { return "fd7d::1", nil }
	d.probe = func(u string) error {
		if u == "http://[fd7d::1]:8480" {
			return nil
		}
		return errors.New("refused")
	}
	ep, err := reachWDA(testUDID, 8480, "http://192.0.2.1:8480", d)
	if err != nil {
		t.Fatal(err)
	}
	if !c.closed {
		t.Error("the tunnel forward WDA did not answer through was left open")
	}
	if ep.baseURL != "http://[fd7d::1]:8480" || ep.path != reachCoreDevice || ep.closer != nil {
		t.Fatalf("got %+v", ep)
	}
}

func TestReachWDAFallsBackToReportedAddress(t *testing.T) {
	d := failingDeps(t)
	d.agentTunnel = func(string) (goIOSTunnel, error) { return goIOSTunnel{Address: "fd00::1"}, nil }
	d.tunnelForward = func(goIOSTunnel, uint16) (io.Closer, error) { return nil, errors.New("port busy") }
	d.coreDeviceIP = func(string) (string, error) { return "fd7d::1", nil }
	d.probe = func(u string) error {
		if u == "http://192.0.2.1:8480" {
			return nil
		}
		return errors.New("no route")
	}
	ep, err := reachWDA(testUDID, 8480, "http://192.0.2.1:8480/", d)
	if err != nil {
		t.Fatal(err)
	}
	if ep.baseURL != "http://192.0.2.1:8480" || ep.path != reachReported || ep.closer != nil {
		t.Fatalf("got %+v", ep)
	}
}

func TestReachWDAReportsEveryPathTried(t *testing.T) {
	for _, tc := range []struct {
		name, reported, want string
	}{
		{"no reported address", "", "WDA printed no address"},
		{"reported address unanswered", "http://192.0.2.1:8480", reachReported + ": refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reachWDA(testUDID, 8480, tc.reported, failingDeps(t))
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, part := range []string{testUDID, reachUSBMux + ": not listed", reachGoIOSTunnel + ": no agent", reachCoreDevice + ": no tunnel", tc.want} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q lacks %q", err, part)
				}
			}
		})
	}
}

func TestReachWDACoreDeviceProbeFails(t *testing.T) {
	d := failingDeps(t)
	d.coreDeviceIP = func(string) (string, error) { return "fd7d::1", nil }
	_, err := reachWDA(testUDID, 8480, "", d)
	if err == nil || !strings.Contains(err.Error(), reachCoreDevice+": refused") {
		t.Fatalf("got %v", err)
	}
}

func TestReportedServerURL(t *testing.T) {
	if got := reportedServerURL("Testing started\n"); got != "" {
		t.Errorf("no marker: got %q", got)
	}
	one := "x ServerURLHere->http://192.0.2.1:8480<-ServerURLHere y"
	if got := reportedServerURL(one); got != "http://192.0.2.1:8480" {
		t.Errorf("one marker: got %q", got)
	}
	two := one + "\n=== attempt 2/4 ===\nServerURLHere->http://192.0.2.2:8480<-ServerURLHere\n"
	if got := reportedServerURL(two); got != "http://192.0.2.2:8480" {
		t.Errorf("retried: got %q, want the last attempt's address", got)
	}
}

func TestProbeWDA(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":{"ready":true}}`))
	}))
	defer ok.Close()
	if err := probeWDA(ok.URL); err != nil {
		t.Errorf("answering WDA: %v", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	if err := probeWDA(broken.URL); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("HTTP 500: got %v", err)
	}

	if err := probeWDA("http://127.0.0.1:" + strconv.Itoa(closedPort(t))); err == nil {
		t.Error("closed port: expected an error")
	}
}

// closedPort returns a local port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestAgentTunnel(t *testing.T) {
	var reply func(w http.ResponseWriter)
	var gotPath string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		reply(w)
	}))
	defer agent.Close()
	t.Setenv("GO_IOS_AGENT_PORT", strconv.Itoa(agent.Listener.Addr().(*net.TCPAddr).Port))

	reply = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"address":"fd00::1","udid":"` + testUDID + `","userspaceTun":true,"userspaceTunPort":60106}`))
	}
	tun, err := agentTunnel(testUDID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/tunnel/"+testUDID {
		t.Errorf("asked the agent for %q", gotPath)
	}
	if tun.Address != "fd00::1" || !tun.UserspaceTUN || tun.UserspaceTUNPort != 60106 {
		t.Errorf("got %+v", tun)
	}

	for name, r := range map[string]func(w http.ResponseWriter){
		"not found":     func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
		"bad JSON":      func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{`)) },
		"empty address": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) },
	} {
		reply = r
		if _, err := agentTunnel(testUDID); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	t.Setenv("GO_IOS_AGENT_PORT", strconv.Itoa(closedPort(t)))
	if _, err := agentTunnel(testUDID); err == nil || !strings.Contains(err.Error(), "no go-ios agent") {
		t.Errorf("no agent: got %v", err)
	}
}

// echoServer accepts connections and echoes what each sends, after
// discarding the first skip bytes.
func echoServer(t *testing.T, skip int) (port int, header chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	header = make(chan []byte, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if skip > 0 {
					h := make([]byte, skip)
					if _, err := io.ReadFull(c, h); err != nil {
						return
					}
					header <- h
				}
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, header
}

// roundTrip sends msg to 127.0.0.1:port and returns what comes back once
// the far side closes.
func roundTrip(t *testing.T, port uint16, msg string) string {
	t.Helper()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	return string(got)
}

func TestForwardTCPPipesBothWays(t *testing.T) {
	echoPort, _ := echoServer(t, 0)
	port := uint16(closedPort(t))
	ln, err := forwardTCP(port, func() (net.Conn, error) {
		return net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := roundTrip(t, port, "GET /status"); got != "GET /status" {
		t.Errorf("echo through the forward: got %q", got)
	}
}

func TestForwardTCPDropsConnectionWhenDialFails(t *testing.T) {
	port := uint16(closedPort(t))
	ln, err := forwardTCP(port, func() (net.Conn, error) { return nil, errors.New("tunnel down") })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := roundTrip(t, port, "hello"); got != "" {
		t.Errorf("got %q from a forward whose dial failed", got)
	}
}

func TestForwardTCPPortTaken(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := uint16(busy.Addr().(*net.TCPAddr).Port)
	if _, err := forwardTCP(port, nil); err == nil {
		t.Error("expected an error for a port in use")
	}
}

func TestTunnelForwardThroughUserspaceTUN(t *testing.T) {
	// A go-ios userspace TUN takes a 16-byte IPv6 address and a 4-byte
	// little-endian port, then carries the stream.
	tunPort, header := echoServer(t, 20)
	port := uint16(closedPort(t))
	ln, err := tunnelForward(goIOSTunnel{Address: "fd00::1", UserspaceTUN: true, UserspaceTUNPort: tunPort}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := roundTrip(t, port, "ping"); got != "ping" {
		t.Errorf("got %q", got)
	}
	h := <-header
	if !net.IP(h[:16]).Equal(net.ParseIP("fd00::1")) {
		t.Errorf("tunnel was asked for %v", net.IP(h[:16]))
	}
	if gotPort := int(h[16]) | int(h[17])<<8; gotPort != int(port) {
		t.Errorf("tunnel was asked for port %d, want %d", gotPort, port)
	}
}

const devicectlConnected = `{"result":{"connectionProperties":{"tunnelIPAddress":"fd7d::1","tunnelState":"connected","transportType":"wired"},"deviceProperties":{"name":"Test iPad","osVersionNumber":"26.6.1"}}}`

const devicectlDisconnected = `{"result":{"connectionProperties":{"tunnelIPAddress":"fd7d::1","tunnelState":"disconnected"},"deviceProperties":{"name":"Test iPad","osVersionNumber":"26.6.1"}}}`

func TestParseDevicectlDetails(t *testing.T) {
	dev, err := parseDevicectlDetails([]byte(devicectlConnected))
	if err != nil {
		t.Fatal(err)
	}
	if dev != (DevicectlDevice{Name: "Test iPad", OSVersion: "26.6.1", TunnelIP: "fd7d::1"}) {
		t.Errorf("connected: got %+v", dev)
	}
	dev, err = parseDevicectlDetails([]byte(devicectlDisconnected))
	if err != nil || dev.TunnelIP != "" {
		t.Errorf("disconnected: got %+v, %v", dev, err)
	}
	if _, err := parseDevicectlDetails([]byte("not json")); err == nil {
		t.Error("expected an error for bad JSON")
	}
}

// fakeXcrun puts an xcrun on PATH that writes json to --json-output, or
// fails when json is "".
func fakeXcrun(t *testing.T, json string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho boom >&2\nexit 1\n"
	if json != "" {
		src := filepath.Join(dir, "details.json")
		if err := os.WriteFile(src, []byte(json), 0o644); err != nil {
			t.Fatal(err)
		}
		script = "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--json-output\" ]; then out=\"$2\"; fi\n  shift\ndone\ncat '" + src + "' > \"$out\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDevicectlDeviceInfo(t *testing.T) {
	fakeXcrun(t, devicectlConnected)
	dev, err := DevicectlDeviceInfo(testUDID)
	if err != nil {
		t.Fatal(err)
	}
	if dev.Name != "Test iPad" || dev.TunnelIP != "fd7d::1" {
		t.Errorf("got %+v", dev)
	}
	ip, err := coreDeviceIP(testUDID)
	if err != nil || ip != "fd7d::1" {
		t.Errorf("coreDeviceIP: got %q, %v", ip, err)
	}
}

func TestDevicectlDeviceInfoFails(t *testing.T) {
	fakeXcrun(t, "")
	if _, err := DevicectlDeviceInfo(testUDID); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
	if _, err := coreDeviceIP(testUDID); err == nil {
		t.Error("coreDeviceIP: expected an error")
	}
}

func TestDevicectlDeviceInfoTimesOut(t *testing.T) {
	// devicectl hangs on a locked device or one still pairing; the query
	// must give up instead of freezing attach.
	dir := t.TempDir()
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := devicectlInfoTimeout
	devicectlInfoTimeout = 200 * time.Millisecond
	t.Cleanup(func() { devicectlInfoTimeout = old })

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := DevicectlDeviceInfo(testUDID)
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
			t.Errorf("got %v, want a timeout error", r.err)
		}
		if r.elapsed > 5*time.Second {
			t.Errorf("took %v", r.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DevicectlDeviceInfo still running after 10 s")
	}
}

func TestCoreDeviceIPWithoutTunnel(t *testing.T) {
	fakeXcrun(t, devicectlDisconnected)
	if _, err := coreDeviceIP(testUDID); err == nil || !strings.Contains(err.Error(), "no connected tunnel") {
		t.Errorf("got %v", err)
	}
}

func TestUSBMuxForwardUnknownDevice(t *testing.T) {
	if _, err := usbmuxForward(testUDID, uint16(closedPort(t))); err == nil {
		t.Error("expected an error for a device usbmuxd does not list")
	}
}

func TestDefaultReachDepsAreTheRealOperations(t *testing.T) {
	d := defaultReachDeps()
	if d.usbmuxForward == nil || d.agentTunnel == nil || d.tunnelForward == nil || d.coreDeviceIP == nil || d.probe == nil {
		t.Fatalf("missing operations: %+v", d)
	}
}

func TestStartPortForwardRecordsEndpoint(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "runner.log")
	if err := os.WriteFile(logPath, []byte("ServerURLHere->http://192.0.2.1:8480<-ServerURLHere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := failingDeps(t)
	d.probe = func(u string) error {
		if u == "http://192.0.2.1:8480" {
			return nil
		}
		return errors.New("refused")
	}
	r := &Runner{deviceUDID: testUDID, port: 8480, reach: &d}
	if got := r.BaseURL(); got != "http://localhost:8480" {
		t.Errorf("before Start: BaseURL %q", got)
	}
	if err := r.startPortForward(logPath); err != nil {
		t.Fatal(err)
	}
	if got := r.BaseURL(); got != "http://192.0.2.1:8480" {
		t.Errorf("BaseURL %q, want the address WDA reported", got)
	}
	if r.portForwardListener != nil {
		t.Error("a direct address needs no forward to close")
	}
	if c := NewClientURL(r.BaseURL()); c.baseURL != "http://192.0.2.1:8480" {
		t.Errorf("client base URL %q", c.baseURL)
	}
}

func TestStartPortForwardKeepsUSBMuxForward(t *testing.T) {
	c := &fakeCloser{}
	d := failingDeps(t)
	d.usbmuxForward = func(string, uint16) (io.Closer, error) { return c, nil }
	r := &Runner{deviceUDID: testUDID, port: 8480, reach: &d}
	if err := r.startPortForward(filepath.Join(t.TempDir(), "missing.log")); err != nil {
		t.Fatal(err)
	}
	if r.portForwardListener != c || r.BaseURL() != "http://localhost:8480" {
		t.Errorf("listener %v, BaseURL %q", r.portForwardListener, r.BaseURL())
	}
	r.Stop()
	if !c.closed {
		t.Error("Stop left the usbmuxd forward open")
	}
}

func TestStartPortForwardFails(t *testing.T) {
	d := failingDeps(t)
	r := &Runner{deviceUDID: testUDID, port: 8480, reach: &d}
	if err := r.startPortForward(filepath.Join(t.TempDir(), "missing.log")); err == nil {
		t.Fatal("expected an error")
	}
	if r.baseURL != "" {
		t.Errorf("baseURL %q after a failure", r.baseURL)
	}
}

func TestStartPortForwardDefaultsToRealOperations(t *testing.T) {
	// No usbmuxd entry, no go-ios agent, devicectl failing and no address
	// in the log: every real path is tried and fails.
	fakeXcrun(t, "")
	t.Setenv("GO_IOS_AGENT_PORT", strconv.Itoa(closedPort(t)))
	r := &Runner{deviceUDID: testUDID, port: uint16(closedPort(t))}
	err := r.startPortForward(filepath.Join(t.TempDir(), "missing.log"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, part := range []string{reachUSBMux, "no go-ios agent", "boom", "WDA printed no address"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q lacks %q", err, part)
		}
	}
}
