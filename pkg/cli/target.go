package cli

// target.go: --target-hub. A targeted command talks to a TCP daemon or hub
// (`serve --http`, later `hub start`) instead of the local named daemon,
// and never spawns one: an unreachable target exits 3 (DAEMON_UNAVAILABLE).

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
)

// parseTarget turns HOST, HOST:PORT, http://HOST[:PORT] or
// https://HOST[:PORT] into a base URL, adding the default port. It reads
// the address as --peer does (hub.ParseAddr).
func parseTarget(v string) (base string, useTLS bool, err error) {
	addr, err := hub.ParseAddr(v, hub.DefaultPort)
	if err != nil {
		return "", false, daemon.Errorf(daemon.CodeUsage, "--target-hub: %v", err)
	}
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		return "https://" + rest, true, nil
	}
	return "http://" + addr, false, nil
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
