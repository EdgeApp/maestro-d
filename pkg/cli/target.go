package cli

// target.go: --target-hub. A targeted command talks to a TCP daemon or hub
// (`serve --http`, later `hub start`) instead of the local named daemon,
// and never spawns one: an unreachable target exits 3 (DAEMON_UNAVAILABLE).

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// DefaultHubPort is the port a --target-hub without one uses.
const DefaultHubPort = 7433

// parseTarget turns HOST, HOST:PORT, http://HOST[:PORT] or
// https://HOST[:PORT] into a base URL, adding the default port.
func parseTarget(v string) (base string, useTLS bool, err error) {
	v = strings.TrimSpace(v)
	scheme := "http"
	if i := strings.Index(v, "://"); i >= 0 {
		scheme = strings.ToLower(v[:i])
		v = v[i+3:]
	}
	if scheme != "http" && scheme != "https" {
		return "", false, daemon.Errorf(daemon.CodeUsage, "--target-hub: unsupported scheme %q (use http:// or https://)", scheme)
	}
	v = strings.TrimRight(v, "/")
	if v == "" || strings.ContainsAny(v, "/?#@ ") {
		return "", false, daemon.Errorf(daemon.CodeUsage, "--target-hub: expected HOST[:PORT], got %q", v)
	}
	host, port := v, strconv.Itoa(DefaultHubPort)
	if h, p, err := net.SplitHostPort(v); err == nil {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", false, daemon.Errorf(daemon.CodeUsage, "--target-hub: bad port %q", p)
		}
		host, port = h, p
	} else if strings.Count(v, ":") > 1 && !strings.HasPrefix(v, "[") {
		host = v // a bare IPv6 address
	}
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(strings.Trim(host, "[]"), port)}
	return u.String(), scheme == "https", nil
}

// targetTLSConfig returns the TLS settings for a targeted https:// hub:
// the system roots, plus caFile's certificates when given.
func targetTLSConfig(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ca: %v", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, daemon.Errorf(daemon.CodeUsage, "--ca: %s holds no PEM certificate", caFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// connectTarget returns a client for o.Target. It does not contact the
// target; the first request does, and fails DAEMON_UNAVAILABLE if it is
// unreachable.
func connectTarget(o *daemonOpts) (*daemon.Client, error) {
	base, useTLS, err := parseTarget(o.Target)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if useTLS {
		if tlsConfig, err = targetTLSConfig(o.CAFile); err != nil {
			return nil, err
		}
	} else if o.CAFile != "" && o.Verbose {
		progress("note: --ca is ignored for a plain http:// target")
	}
	c := daemon.NewHTTPClient(base, o.Token, tlsConfig)
	// The target is another machine: this client names its output files
	// itself and fetches them back (remote_files.go).
	c.ClientOutputs = true
	if o.Verbose {
		progress("Targeting %s", base)
		daemon.RetryLog = func(format string, args ...any) { progress(format, args...) }
	}
	return c, nil
}

// targetLabel names the daemon a command talks to, for messages.
func targetLabel(o *daemonOpts) string {
	if o.Target != "" {
		if base, _, err := parseTarget(o.Target); err == nil {
			return base
		}
		return o.Target
	}
	return fmt.Sprintf("daemon %q", o.Name)
}
