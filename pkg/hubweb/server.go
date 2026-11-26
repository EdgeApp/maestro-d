package hubweb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
	"github.com/devicelab-dev/maestro-runner/pkg/hubd"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

//go:embed dist
var bundle embed.FS

// Server owns just the public page, its assets, and its snapshot socket.
type Server struct {
	net.Listener
	http   *http.Server
	ctx    context.Context
	cancel context.CancelFunc
	feed   *feed
	events *daemon.EventBus
}

func Listen(homeDir, advertise, cert, key string, port int, policy *hubd.Server, mesh *hub.Mesh) (*Server, error) {
	cfg, fingerprint, err := webTLSConfig(homeDir, advertise, cert, key)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "dashboard TLS: %v", err)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--web-port %d: %v", port, err)
	}
	logger.Info("dashboard certificate SHA-256: %s", fingerprint)
	files, _ := fs.Sub(bundle, "dist")
	return newServer(ln, cfg, meshSource(policy, mesh), policy.Events(), files), nil
}

type connKey struct{}

func newServer(ln net.Listener, cfg *tls.Config, source source, events *daemon.EventBus, files fs.FS) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{Listener: &protocolListener{ln, cfg}, ctx: ctx, cancel: cancel, feed: newFeed(source), events: events}
	s.http = &http.Server{
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		BaseContext: func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := r.Context().Value(connKey{}).(*protocolConn)
			if !c.secure {
				http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
				return
			}
			if r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/ws" {
				s.feed.socket(w, r)
				return
			}
			serveFile(files, w, r)
		}),
	}
	return s
}

func (s *Server) Serve() error {
	ev, _, unsub := s.events.Subscribe("", 0)
	defer unsub()
	ticks := time.NewTicker(2 * time.Second)
	defer ticks.Stop()
	go s.feed.run(s.ctx, ev, ticks.C, time.Now)
	return s.http.Serve(s.Listener)
}

func (s *Server) Close() error {
	s.cancel()
	_ = s.http.Close()
	// Serve normally closes its listener. Close also works if startup
	// stopped before Serve was called.
	err := s.Listener.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

var hashedAsset = regexp.MustCompile(`[-.][a-zA-Z0-9_]{8,}\.[a-zA-Z0-9]+$`)

func serveFile(files fs.FS, w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	} else if !strings.HasPrefix(name, "assets/") || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	cache := "no-cache"
	if hashedAsset.MatchString(path.Base(name)) {
		cache = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// Sniff on the HTTP server's per-connection goroutine, under its header
// deadline. A client that connects without sending a byte cannot stall Accept.
type protocolListener struct {
	net.Listener
	cfg *tls.Config
}

func (l *protocolListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &protocolConn{Conn: c, cfg: l.cfg}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type protocolConn struct {
	net.Conn
	cfg    *tls.Config
	stream net.Conn
	secure bool
}

func (c *protocolConn) Read(p []byte) (int, error) {
	if c.stream == nil {
		buf := bufio.NewReader(c.Conn)
		first, err := buf.Peek(1)
		if err != nil {
			return 0, err
		}
		c.stream = &bufferedConn{c.Conn, buf}
		if first[0] == 0x16 { // TLS handshake record
			c.secure = true
			c.stream = tls.Server(c.stream, c.cfg)
		}
	}
	return c.stream.Read(p)
}
func (c *protocolConn) Write(p []byte) (int, error) {
	if c.stream == nil {
		return c.Conn.Write(p)
	}
	return c.stream.Write(p)
}
