package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

// fromAddr makes every request look as if it came from addr, as a TCP
// listener sees a LAN client.
func fromAddr(addr string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addr
		h.ServeHTTP(w, r)
	})
}

type callerEnv struct {
	s      *Server
	local  *Client // the unix-socket handler
	remote *Client // the TCP handler, from a LAN address
	lo     *Client // the TCP handler, from 127.0.0.1
}

func newCallerEnv(t *testing.T, cfg Config) *callerEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	if cfg.Name == "" {
		cfg.Name = "callers"
	}
	cfg.ReportsDir = filepath.Join(t.TempDir(), "reports")
	s := New(cfg, testDeps(&n, 0))
	s.info = Info{Name: cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	t.Cleanup(s.detachAll)
	mk := func(h http.Handler) *Client {
		hs := httptest.NewServer(h)
		t.Cleanup(hs.Close)
		return NewHTTPClient(hs.URL, cfg.Token, nil)
	}
	return &callerEnv{
		s:      s,
		local:  mk(s.Handler()),
		remote: mk(fromAddr("192.0.2.10:40000", s.TCPHandler())),
		lo:     mk(s.TCPHandler()),
	}
}

func wantErrCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got success", code)
	}
	if got := codeOf(t, err); got != code {
		t.Fatalf("code %s, want %s: %v", got, code, err)
	}
	de, _ := err.(*Error)
	return de
}

func TestCaller_RemoteRunShellForbiddenLocalRuns(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	if _, err := env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	res, err := env.remote.Command(ctx, "mock-1", "runShell", "echo hi", true)
	de := wantErrCode(t, err, CodeForbidden)
	if de.ExitCode() != 2 || res == nil || res.Error.Details["command"] != "runShell" {
		t.Fatalf("FORBIDDEN details: %+v %+v", de, res)
	}
	// The same over the unix socket runs: the policy follows the request,
	// not who attached.
	if _, err := env.local.Command(ctx, "mock-1", "runShell", map[string]any{"command": "echo hi", "output": "x"}, true); err != nil {
		t.Fatalf("unix-socket runShell: %v", err)
	}
	// On a plain serve --http daemon, loopback stays local.
	if _, err := env.lo.Command(ctx, "mock-1", "runShell", "echo hi", true); err != nil {
		t.Fatalf("loopback runShell on serve --http: %v", err)
	}
}

func TestCaller_HubTreatsLoopbackAsRemote(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: ":7433", Token: "t", Hub: true})
	ctx := context.Background()
	if _, err := env.lo.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	_, err := env.lo.Command(ctx, "mock-1", "runShell", "echo hi", true)
	wantErrCode(t, err, CodeForbidden)
	// A remote-attached session driven over the unix socket is not checked.
	if _, err := env.local.Command(ctx, "mock-1", "runShell", "echo hi", true); err != nil {
		t.Fatalf("unix socket on a hub: %v", err)
	}
	// AllowRemoteShell lets remote callers through.
	env.s.cfg.AllowRemoteShell = true
	if _, err := env.lo.Command(ctx, "mock-1", "runShell", "echo hi", true); err != nil {
		t.Fatalf("with AllowRemoteShell: %v", err)
	}
}

func TestCaller_RemoteHostDirectoriesRefusedAtTheRoute(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	if _, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	_, err := env.remote.CommandCwd(ctx, "mock-1", "tapOn", "X", "/home/someone", true)
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.Steps(ctx, "mock-1", StepsRequest{YAML: "- tapOn: X\n", Cwd: "/tmp"}, true)
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.Flow(ctx, "mock-1", FlowRequest{File: "/etc/flow.yaml"}, true)
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.Flow(ctx, "mock-1", FlowRequest{YAML: "appId: x\n---\n- tapOn: X\n", Cwd: "/tmp"}, true)
	wantErrCode(t, err, CodeForbidden)
	// Without them the same calls run, policy-checked.
	if _, err := env.remote.CommandCwd(ctx, "mock-1", "tapOn", "X", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.Steps(ctx, "mock-1", StepsRequest{YAML: "- tapOn: X\n"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.Flow(ctx, "mock-1", FlowRequest{YAML: "appId: x\n---\n- tapOn: X\n"}, true); err != nil {
		t.Fatal(err)
	}
	res, err := env.remote.Steps(ctx, "mock-1", StepsRequest{YAML: "- tapOn: X\n- addMedia: [/etc/hosts]\n- tapOn: Y\n", ContinueOnError: true}, true)
	wantErrCode(t, err, CodeForbidden)
	if res == nil || len(res.Results) != 2 || res.Results[1].Error.Details["field"] != "files" {
		t.Fatalf("steps: %+v", res)
	}
	// A local caller keeps ?cwd=.
	if _, err := env.local.CommandCwd(ctx, "mock-1", "tapOn", "X", t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
}

func TestCaller_RemoteAttachRefusals(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	cases := map[string]AttachConfig{
		"web":           {Platform: "web"},
		"web driver":    {Driver: "cdp"},
		"outputDir":     {Platform: "mock", OutputDir: "/tmp/x"},
		"flowDir":       {Platform: "mock", FlowDir: "/tmp/x"},
		"appFile":       {Platform: "mock", AppFile: "/tmp/app.apk"},
		"userDataDir":   {Platform: "mock", Extra: map[string]any{"userDataDir": "/tmp/profile"}},
		"browser":       {Platform: "mock", Extra: map[string]any{"browser": "/bin/sh"}},
		"capsFile":      {Platform: "mock", Extra: map[string]any{"capsFile": "/tmp/caps.json"}},
		"capsFile blob": {Platform: "mock", Extra: map[string]any{"capsFile": "blob:sha256:" + sha([]byte("{}")) + "/c.json"}},
		"appium":        {Platform: "android", Driver: "Appium"},
		"appiumUrl":     {Platform: "mock", AppiumURL: "http://10.0.0.9:4723"},
	}
	for name, cfg := range cases {
		if _, err := env.remote.Attach(ctx, "mock-1", cfg); err == nil || codeOf(t, err) != CodeForbidden {
			t.Errorf("%s: want FORBIDDEN, got %v", name, err)
		}
	}
	// An uploaded app passes the route check; the unix socket attaches web
	// as today.
	app := []byte("PK\x03\x04")
	if _, e := env.s.blobs.Put(sha(app), "", bytes.NewReader(app)); e != nil {
		t.Fatal(e)
	}
	if _, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock", AppFile: "blob:sha256:" + sha(app) + "/a.apk"}); err != nil {
		t.Fatalf("blob ref: %v", err)
	}
	if _, err := env.local.Attach(ctx, "mock-2", AttachConfig{Platform: "web"}); err != nil {
		t.Fatalf("unix-socket web attach: %v", err)
	}
}

func TestCaller_EnvironmentStaysOnTheHost(t *testing.T) {
	t.Setenv("MAESTRO_D_DAEMON_ONLY", "hunter2")
	t.Setenv("MAESTRO_D_PASSED", "fine")
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		cfg     Config
		imports bool
	}{
		{"hub", Config{Hub: true, HTTPAddr: ":7433", Token: "t"}, false},
		{"non-loopback serve", Config{HTTPAddr: "0.0.0.0:7433", Token: "t", PassEnv: []string{"MAESTRO_D_PASSED"}}, false},
		{"loopback serve", Config{HTTPAddr: "127.0.0.1:7433"}, true},
		{"socket only", Config{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newCallerEnv(t, tc.cfg)
			if _, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
				t.Fatal(err)
			}
			vars, err := env.remote.Vars(ctx, "mock-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := vars["MAESTRO_D_DAEMON_ONLY"]; ok {
				t.Fatal("GET vars must not show the daemon's environment")
			}
			v, err := env.local.Eval(ctx, "mock-1", "typeof MAESTRO_D_DAEMON_ONLY === 'undefined' ? 'unset' : MAESTRO_D_DAEMON_ONLY")
			if err != nil {
				t.Fatal(err)
			}
			if got := v == "hunter2"; got != tc.imports {
				t.Fatalf("imports env = %v (value %v), want %v", got, v, tc.imports)
			}
			if len(tc.cfg.PassEnv) > 0 {
				if v, _ := env.local.Eval(ctx, "mock-1", "MAESTRO_D_PASSED"); v != "fine" {
					t.Fatalf("--pass-env: %v", v)
				}
			}
		})
	}
}

func TestCaller_ListenChecks(t *testing.T) {
	cases := []struct {
		cfg Config
		ok  bool
	}{
		{Config{}, true},
		{Config{HTTPAddr: ":7433"}, true},
		{Config{HTTPAddr: "127.0.0.1:7433"}, true},
		{Config{HTTPAddr: "localhost:7433"}, true},
		{Config{HTTPAddr: "[::1]:7433"}, true},
		{Config{HTTPAddr: "0.0.0.0:7433"}, false},
		{Config{HTTPAddr: "0.0.0.0:7433", Token: "t"}, true},
		{Config{HTTPAddr: ":7433", Hub: true}, false},
		{Config{HTTPAddr: ":7433", Hub: true, Token: "t"}, true},
		{Config{TLSCert: "c"}, false},
		{Config{HTTPAddr: ":7433", TLSCert: "c"}, false},
		{Config{HTTPAddr: ":7433", TLSCert: "c", TLSKey: "k"}, true},
	}
	for _, tc := range cases {
		s := New(tc.cfg, Deps{})
		if e := s.checkListen(); (e == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.cfg, e)
		} else if e != nil && e.Code != CodeUsage {
			t.Errorf("%+v: code %s", tc.cfg, e.Code)
		}
	}
	// Run refuses before listening.
	t.Setenv(EnvHome, t.TempDir())
	s := New(Config{Name: "notoken", HTTPAddr: "0.0.0.0:0"}, Deps{})
	if err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "--token") {
		t.Fatalf("Run without a token: %v", err)
	}
	s = New(Config{Name: "badtls", HTTPAddr: "127.0.0.1:0", TLSCert: "/nope.pem", TLSKey: "/nope.key"}, Deps{})
	if err := s.Run(context.Background()); err == nil || codeOf(t, err) != CodeUsage {
		t.Fatalf("Run with missing TLS files: %v", err)
	}
	if isLoopbackListen("example.com:1") || !isLoopbackListen("localhost") {
		t.Fatal("isLoopbackListen")
	}
}

// selfSigned writes a certificate for 127.0.0.1 and its key.
func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	kder, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return certFile, keyFile
}

func TestServer_RunServesTLS(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	certFile, keyFile := selfSigned(t)
	pr, pw, _ := os.Pipe()
	var n int32
	s := New(Config{Name: "tls", HTTPAddr: "127.0.0.1:0", Token: "tok", TLSCert: certFile, TLSKey: keyFile, Ready: pw,
		ReportsDir: t.TempDir()}, testDeps(&n, 0))
	go func() { _ = s.Run(context.Background()) }()
	defer func() { s.Shutdown("test"); <-s.Done() }()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	addr := ""
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, "http=") {
			addr = strings.TrimPrefix(f, "http=")
		}
	}
	pemData, _ := os.ReadFile(certFile)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemData)
	c := NewHTTPClient("https://"+addr, "tok", &tls.Config{RootCAs: pool})
	st, err := c.Status(context.Background())
	if err != nil || !st.Daemon.TLS {
		t.Fatalf("status over TLS: %v %+v", err, st)
	}
	// Without the trust root the certificate is refused: DAEMON_UNAVAILABLE.
	_, err = NewHTTPClient("https://"+addr, "tok", nil).Status(context.Background())
	if err == nil || codeOf(t, err) != CodeDaemonUnavailable {
		t.Fatalf("untrusted certificate: %v", err)
	}
}

func TestCallerHelpers(t *testing.T) {
	if CallerFrom(context.Background()).Remote {
		t.Fatal("no caller recorded means local")
	}
	for _, c := range []struct {
		c    Caller
		want string
	}{{Caller{}, "unix socket"}, {Caller{Remote: true, Addr: "a"}, "remote a"}, {Caller{Addr: "b"}, "local b"}} {
		if got := describeCaller(c.c); got != c.want {
			t.Errorf("%+v: %q", c.c, got)
		}
	}
	s := New(Config{}, Deps{})
	if !s.remoteTCP("garbage") || s.remoteTCP("127.0.0.1:1") || !s.remoteTCP("[2001:db8::1]:1") {
		t.Fatal("remoteTCP")
	}
}

// 5.harness-review.1: Appium capabilities name host files and programs, and
// appiumUrl aims the daemon at any address, so a remote attach gets no
// Appium; the route applies this only to remote callers.
func TestCheckRemoteAttach_Appium(t *testing.T) {
	blob := "blob:sha256:" + strings.Repeat("a", 64) + "/caps.json"
	for _, tc := range []struct {
		name  string
		cfg   AttachConfig
		field string
	}{
		{"driver appium", AttachConfig{Platform: "android", Driver: "appium"}, "driver"},
		{"driver Appium", AttachConfig{Driver: "Appium"}, "driver"},
		{"driver APPIUM padded", AttachConfig{Platform: "ios", Driver: " APPIUM "}, "driver"},
		{"appiumUrl", AttachConfig{Platform: "android", AppiumURL: "http://127.0.0.1:4723"}, "appiumUrl"},
		{"capsFile blob", AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": blob}}, "extra.capsFile"},
		{"capsFile path", AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": "/tmp/caps.json"}}, "extra.capsFile"},
		{"capsFile non-string", AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": map[string]any{"appium:app": "/bin/sh"}}}, "extra.capsFile"},
		{"no driver", AttachConfig{Platform: "android"}, ""},
		{"uiautomator2", AttachConfig{Platform: "android", Driver: "uiautomator2"}, ""},
		{"devicelab", AttachConfig{Platform: "ios", Driver: "devicelab"}, ""},
		{"empty capsFile", AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": ""}}, ""},
	} {
		e := checkRemoteAttach(&tc.cfg)
		switch {
		case tc.field == "" && e != nil:
			t.Errorf("%s: refused: %v", tc.name, e)
		case tc.field != "" && (e == nil || e.Code != CodeForbidden || e.Details["field"] != tc.field):
			t.Errorf("%s: want FORBIDDEN on %s, got %+v", tc.name, tc.field, e)
		}
	}
}

// A unix-socket caller attaches with Appium as today: the route never
// applies the remote check to it.
func TestCaller_LocalAppiumAttach(t *testing.T) {
	env := newCallerEnv(t, Config{HTTPAddr: "0.0.0.0:7433", Token: "t"})
	ctx := context.Background()
	var got AttachConfig
	next := env.s.deps.NewDriver
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		got = cfg
		return next(ctx, id, cfg)
	}
	cfg := AttachConfig{Platform: "mock", Driver: "appium", AppiumURL: "http://127.0.0.1:4723",
		Extra: map[string]any{"capsFile": "/tmp/caps.json"}}
	if _, err := env.local.Attach(ctx, "mock-1", cfg); err != nil {
		t.Fatalf("unix-socket Appium attach: %v", err)
	}
	if got.Driver != "appium" || got.AppiumURL != cfg.AppiumURL || got.Extra["capsFile"] != "/tmp/caps.json" {
		t.Fatalf("driver got %+v", got)
	}
	if _, err := env.remote.Attach(ctx, "mock-2", cfg); codeOf(t, err) != CodeForbidden {
		t.Fatalf("remote Appium attach: %v", err)
	}
}
