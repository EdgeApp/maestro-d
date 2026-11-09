package daemon

// caller.go: who sent a request, and what a remote caller may not do.
//
// Only the unix socket is local on a hub: every TCP request, loopback
// included, is remote there. On a plain `serve --http` daemon a request
// from a non-loopback address is remote and a loopback one stays local, so
// existing `--http 127.0.0.1:…` users keep host paths, runShell and the
// system environment. The caller policy for each step-running call is
// decided from the request, never from who attached the session.

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
	// Addr is the TCP peer address ("" on the unix socket).
	Addr string
}

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

// isLoopbackListen reports whether a --http address binds only loopback.
// ":7433" means 127.0.0.1:7433 for `serve`, so an empty host is loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return host == "" || isLoopbackHost(host)
}

// remoteTCP decides whether a TCP request is remote.
func (s *Server) remoteTCP(remoteAddr string) bool {
	if s.cfg.Hub {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return !isLoopbackHost(host)
}

// importsEnv reports whether sessions import the daemon's environment: not
// on a hub, nor on a daemon that listens beyond loopback.
func (s *Server) importsEnv() bool {
	return !s.cfg.Hub && (s.cfg.HTTPAddr == "" || isLoopbackListen(s.cfg.HTTPAddr))
}

// callerPolicy is the executor policy for a step-running call on a.
func (s *Server) callerPolicy(ctx context.Context, a *attached) *executor.CallerPolicy {
	c := CallerFrom(ctx)
	p := &executor.CallerPolicy{Remote: c.Remote}
	if c.Remote {
		p.AllowShell = s.cfg.AllowRemoteShell
		p.OutputsDir = filepath.Join(a.reportDir, "outputs")
	}
	return p
}

// forbidden builds a FORBIDDEN error for a remote request.
func forbidden(format string, args ...any) *Error {
	return Errorf(CodeForbidden, format, args...)
}

// checkRemoteAttach refuses the attach settings that would give a remote
// caller the hub's host: a web session, a report or flow directory, a
// browser profile, or an app or capabilities file that is not a blob ref.
func checkRemoteAttach(cfg *AttachConfig) *Error {
	if strings.EqualFold(cfg.Platform, "web") || isWebDriver(cfg.Driver) {
		return forbidden("a remote caller cannot attach a web session: hubs share simulators, emulators and phones, not the host's browser").
			WithDetails("field", "platform")
	}
	if cfg.OutputDir != "" {
		return forbidden("a remote caller cannot set outputDir (a directory on the daemon's host)").WithDetails("field", "outputDir")
	}
	if cfg.FlowDir != "" {
		return forbidden("a remote caller cannot set flowDir (a directory on the daemon's host)").WithDetails("field", "flowDir")
	}
	if cfg.AppFile != "" && !strings.HasPrefix(cfg.AppFile, "blob:") {
		return forbidden("appFile %s is a path on the daemon's host; a remote caller must upload it (a blob: ref)", cfg.AppFile).
			WithDetails("field", "appFile")
	}
	if v, ok := cfg.Extra["browser"]; ok && v != "" && v != nil {
		return forbidden("a remote caller cannot set extra.browser (a program on the daemon's host)").
			WithDetails("field", "extra.browser")
	}
	if v, ok := cfg.Extra["userDataDir"]; ok && v != "" && v != nil {
		return forbidden("a remote caller cannot set extra.userDataDir (a browser profile on the daemon's host)").
			WithDetails("field", "extra.userDataDir")
	}
	if v, ok := cfg.Extra["capsFile"].(string); ok && v != "" && !strings.HasPrefix(v, "blob:") {
		return forbidden("extra.capsFile %s is a path on the daemon's host; a remote caller must upload it (a blob: ref)", v).
			WithDetails("field", "extra.capsFile")
	}
	return nil
}

func isWebDriver(driver string) bool {
	switch strings.ToLower(driver) {
	case "cdp", "playwright", "browser", "chrome", "chromium":
		return true
	}
	return false
}
