package daemon

// caller.go: who sent a request, and what a remote caller may not do.
//
// Only the unix socket is local on a hub. Both transports are local on a
// loopback-only daemon. Each request carries its own caller policy.

import (
	"context"
	"net"
	"path/filepath"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/executor"
)

// Caller describes the sender of one request.
type Caller struct {
	// Remote is true for a caller that must not reach the daemon's host.
	Remote bool
	// AllowShell permits a remote caller to execute runShell on this hub.
	AllowShell bool
	// Addr is the TCP peer address ("" on the unix socket).
	Addr string
	// ClientOutputs is set when the client names its own output files
	// (HeaderClientOutputs): they are written under the session's
	// outputs/ and listed in the result, for a local caller too.
	ClientOutputs bool
}

// HeaderClientOutputs is sent by a client that targets a daemon on another
// machine (--target-hub, the JavaScript library on a hub): it names its
// output files itself and fetches them back from outputs/.
const HeaderClientOutputs = "X-Maestro-Client-Outputs"

type callerKey struct{}

func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the request's caller (local when none was recorded).
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// isLoopbackHost reports whether host (an IP or name, no port) is loopback.
func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckLocalListen requires an explicit loopback host. Wildcard addresses bind all interfaces.
func CheckLocalListen(addr string) *Error {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !isLoopbackHost(host) {
		return Errorf(CodeUsage, "remote access is maestro-d-hub's job: run maestro-d-hub start (--http on maestro-d serve takes a loopback address only)")
	}
	return nil
}

func (s *Server) tcpCaller(addr string, clientOutputs bool) Caller {
	if hub := s.cfg.Hub; hub != nil {
		return hub.TCPCaller(addr, clientOutputs)
	}
	return Caller{Addr: addr, ClientOutputs: clientOutputs}
}
func (s *Server) importsEnv() bool { return s.cfg.Hub == nil }

// callerPolicy is the executor policy for a step-running call on a.
func (s *Server) callerPolicy(ctx context.Context, a *attached) *executor.CallerPolicy {
	c := CallerFrom(ctx)
	// Every caller's blob refs are mapped; only a remote one is confined.
	p := &executor.CallerPolicy{Remote: c.Remote, ClientOutputs: c.ClientOutputs,
		Blobs: sessionBlobs{store: s.blobs, dir: filepath.Join(a.reportDir, "blobs"), bundles: filepath.Join(a.reportDir, "bundles")}}
	if c.Remote {
		p.AllowShell = c.AllowShell
	}
	if c.Remote || c.ClientOutputs {
		p.OutputsDir = filepath.Join(a.reportDir, "outputs")
	}
	return p
}

// Forbidden builds a FORBIDDEN error for a remote request.
func Forbidden(format string, args ...any) *Error {
	return Errorf(CodeForbidden, format, args...)
}
