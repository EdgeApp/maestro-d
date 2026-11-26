package hubweb

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func testListener(t *testing.T, ping time.Duration) (*Server, *http.Client, []byte) {
	t.Helper()
	home := t.TempDir()
	original, _, err := webTLSConfig(home, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "hub", "web")
	configured, _, err := webTLSConfig(t.TempDir(), "", filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	source := source{scan: func(context.Context) inventory { return inventory{} }, project: func(inventory) Snapshot { return project("self", "machine", nil, inventory{}) }}
	server := newServer(ln, configured, source, daemon.NewEventBus(0), fstest.MapFS{
		"index.html":             {Data: []byte("<h1>Hub</h1>")},
		"assets/app-ABCD1234.js": {Data: []byte("app")},
		"assets/theme-boot.js":   {Data: []byte("theme")},
	})
	server.feed.ping = ping
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		server.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return server, client, original.Certificates[0].Certificate[0]
}

func TestWebTLSAndRoutes(t *testing.T) {
	s, c, der := testListener(t, time.Minute)
	addr := s.Addr().String()
	// Keep an undetermined connection open while HTTPS and HTTP both work.
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	for _, tc := range []struct {
		method, scheme, path string
		status               int
		body, cache          string
	}{
		{"GET", "https", "/", 200, "<h1>Hub</h1>", "no-cache"},
		{"GET", "https", "/assets/app-ABCD1234.js", 200, "app", "public, max-age=31536000, immutable"},
		{"GET", "https", "/assets/theme-boot.js", 200, "theme", "no-cache"},
		{"GET", "https", "/assets/missing.js", 404, "", ""},
		{"GET", "https", "/assets/%2e%2e/index.html", 404, "", ""},
		{"GET", "https", "/v1/devices", 404, "", ""},
		{"POST", "https", "/ws", 404, "", ""},
		{"GET", "http", "/ws?q=1", 308, "", ""},
	} {
		req, err := http.NewRequest(tc.method, tc.scheme+"://"+addr+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != tc.status || tc.body != "" && string(body) != tc.body || resp.Header.Get("Cache-Control") != tc.cache {
			t.Fatalf("%s: %s %q %v %v", tc.path, resp.Status, body, resp.Header, err)
		}
		if tc.scheme == "https" && (!bytes.Equal(resp.TLS.PeerCertificates[0].Raw, der) || resp.TLS.Version != tls.VersionTLS13) {
			t.Fatal("wrong certificate or TLS version")
		}
		if tc.scheme == "http" && resp.Header.Get("Location") != "https://"+addr+tc.path {
			t.Fatal(resp.Header)
		}
	}
	if old, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}); err == nil {
		old.Close()
		t.Fatal("accepted TLS 1.2")
	}
}

func TestWebSocketLimitAndReadOnly(t *testing.T) {
	s, c, _ := testListener(t, time.Minute)
	url := "wss://" + s.Addr().String() + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var conns []*websocket.Conn
	defer func() {
		for _, conn := range conns {
			conn.CloseNow()
		}
	}()
	dial := func() (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: c})
	}
	for i := 0; i < 64; i++ {
		conn, _, err := dial()
		if err != nil {
			t.Fatal(i, err)
		}
		conns = append(conns, conn)
		_, body, err := conn.Read(ctx)
		if err != nil || !json.Valid(body) {
			t.Fatal(err, string(body))
		}
	}
	if extra, resp, err := dial(); err == nil || resp == nil || resp.StatusCode != 503 {
		if extra != nil {
			extra.CloseNow()
		}
		t.Fatal(resp, err)
	}
	first := conns[0]
	if err := first.Write(ctx, websocket.MessageText, []byte(`{"command":"lease"}`)); err != nil {
		t.Fatal(err)
	}
	if err := first.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("a"), 4097)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("oversize close: %v", err)
	}
}

func TestWebSocketPings(t *testing.T) {
	s, c, _ := testListener(t, 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ping := make(chan struct{}, 1)
	conn, _, err := websocket.Dial(ctx, "wss://"+s.Addr().String()+"/ws", &websocket.DialOptions{
		HTTPClient: c, OnPingReceived: func(context.Context, []byte) bool {
			select {
			case ping <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, body, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(body), `"hub":"self"`) {
		t.Fatal(string(body), err)
	}
	conn.CloseRead(ctx)
	select {
	case <-ping:
	case <-ctx.Done():
		t.Fatal("no ping")
	}
}
