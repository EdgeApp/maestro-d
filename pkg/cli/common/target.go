package common

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// ParseTarget turns HOST, HOST:PORT, http://HOST[:PORT] or
// https://HOST[:PORT] into a base URL, adding the default port. It reads
// the address as --peer does.
func ParseTarget(v string) (base string, useTLS bool, err error) {
	addr, err := daemon.ParseHubAddr(v, daemon.DefaultHubPort)
	if err != nil {
		return "", false, daemon.Errorf(daemon.CodeUsage, "--target-hub: %v", err)
	}
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		return "https://" + rest, true, nil
	}
	return "http://" + addr, false, nil
}

// TargetTLSConfig returns the TLS settings for a targeted https:// hub:
// the system roots, plus caFile's certificates when given.
func TargetTLSConfig(caFile string) (*tls.Config, error) {
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

// connectTarget returns a client for target. It does not contact the
// target; the first request does, and fails DAEMON_UNAVAILABLE if it is
// unreachable.
func ConnectTarget(target, caFile, token string, verbose bool) (*daemon.Client, error) {
	base, useTLS, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if useTLS {
		if tlsConfig, err = TargetTLSConfig(caFile); err != nil {
			return nil, err
		}
	} else if caFile != "" && verbose {
		Progress("note: --ca is ignored for a plain http:// target")
	}
	c := daemon.NewHTTPClient(base, token, tlsConfig)
	// The target is another machine: this client names its output files
	// itself and fetches them back (remote_files.go).
	c.ClientOutputs = true
	if verbose {
		Progress("Targeting %s", base)
		daemon.RetryLog = func(format string, args ...any) { Progress(format, args...) }
	}
	return c, nil
}
