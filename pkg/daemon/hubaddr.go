package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultHubPort is the client and peer port when an address omits one.
const DefaultHubPort = 7433

// ParseHubAddr parses a hub address, as --peer, --advertise and
// --target-hub take it: HOST, HOST:PORT, http://HOST[:PORT] or
// https://HOST[:PORT] (a bare IPv6 address too). It returns host:port, or
// https://host:port for https, with port when the address names none.
func ParseHubAddr(v string, port int) (string, error) {
	v = strings.TrimSpace(v)
	scheme := "http"
	if i := strings.Index(v, "://"); i >= 0 {
		scheme, v = strings.ToLower(v[:i]), v[i+3:]
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (use http:// or https://)", scheme)
	}
	v = strings.TrimRight(v, "/")
	if v == "" || strings.ContainsAny(v, "/?#@ \t") {
		return "", fmt.Errorf("want HOST[:PORT] or https://HOST[:PORT], got %q", v)
	}
	host, p := v, strconv.Itoa(port)
	if h, hp, err := net.SplitHostPort(v); err == nil {
		if n, err := strconv.Atoi(hp); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("bad port %q", hp)
		}
		if h == "" {
			return "", fmt.Errorf("want HOST[:PORT], got %q", v)
		}
		host, p = h, hp
	}
	addr := net.JoinHostPort(strings.Trim(host, "[]"), p)
	if scheme == "https" {
		return "https://" + addr, nil
	}
	return addr, nil
}
