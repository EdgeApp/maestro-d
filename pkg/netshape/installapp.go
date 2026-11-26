package netshape

// installapp.go: config.json's installApp block, the hosts this daemon
// downloads a build from for installApp with a url.

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// SpaceHostSuffix is the one place a download may be redirected outside
// allowHosts: a DigitalOcean Space, where Mason signs its build URLs.
const SpaceHostSuffix = ".digitaloceanspaces.com"

// InstallApp is config.json's {"installApp": {"allowHosts": [...]}}.
type InstallApp struct {
	// AllowHosts are the host names (or IP addresses) a url install may
	// fetch from, without scheme or port, matched case-insensitively.
	AllowHosts []string `json:"allowHosts"`
}

func (a *InstallApp) validate() error {
	if len(a.AllowHosts) == 0 {
		return errors.New("allowHosts lists no host")
	}
	for i, h := range a.AllowHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || strings.ContainsAny(h, "/@*?# \t") ||
			(strings.Contains(h, ":") && net.ParseIP(strings.Trim(h, "[]")) == nil) {
			return fmt.Errorf("allowHosts[%d] %q is not a host name: give it without scheme, port, path or wildcard", i, a.AllowHosts[i])
		}
		a.AllowHosts[i] = strings.Trim(h, "[]")
	}
	return nil
}

// Allows reports whether a download may be fetched from host (a URL's
// Hostname()), as the first request or a redirect.
func (a *InstallApp) Allows(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, h := range a.AllowHosts {
		if h == host {
			return true
		}
	}
	return false
}

// AllowsRedirect reports whether a download may be redirected to host
// over scheme: a host Allows, or a DigitalOcean Space over https.
func (a *InstallApp) AllowsRedirect(scheme, host string) bool {
	if a.Allows(host) {
		return true
	}
	host = strings.ToLower(host)
	return scheme == "https" && strings.HasSuffix(host, SpaceHostSuffix) && len(host) > len(SpaceHostSuffix)
}
