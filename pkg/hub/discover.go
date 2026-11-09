package hub

// discover.go: LAN discovery with mDNS (grandcat/zeroconf; on macOS the
// browse goes through the system's mDNS, see discover_dnssd.go). A hub
// advertises _maestro-d._tcp for its whole life, and browses for other
// hubs exactly once, at launch. After that it never searches the network
// again: the mesh stays full through join propagation, and a hub's
// restart is its next discovery.

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// ServiceType is the mDNS service hubs advertise.
const ServiceType = "_maestro-d._tcp"

// Found is a hub a browse found.
type Found struct {
	Name string
	// Addr is host:port, or https://host:port for a hub that serves TLS.
	Addr       string
	APIVersion string
}

// Discoverer advertises this hub and browses for others. Tests replace it.
type Discoverer interface {
	// Advertise announces the hub until stop is called.
	Advertise(name string, port int, txt []string) (stop func(), err error)
	// Browse lists the hubs that answer within d.
	Browse(ctx context.Context, d time.Duration) ([]Found, error)
}

// Zeroconf is the mDNS Discoverer.
type Zeroconf struct{}

// Advertise registers the service on every multicast interface.
func (Zeroconf) Advertise(name string, port int, txt []string) (func(), error) {
	srv, err := zeroconf.Register(name, ServiceType, "local.", port, txt, nil)
	if err != nil {
		return nil, err
	}
	return srv.Shutdown, nil
}

// browseGOOS selects the browser: macOS browses through the system's mDNS
// (discover_dnssd.go), anything else with zeroconf.
var browseGOOS = runtime.GOOS

// Browse collects the hubs that answer within d.
func (Zeroconf) Browse(ctx context.Context, d time.Duration) ([]Found, error) {
	if browseGOOS == "darwin" {
		return browseDNSSD(ctx, d)
	}
	r, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry, 32)
	if err := r.Browse(ctx, ServiceType, "local.", entries); err != nil {
		return nil, err
	}
	var out []Found
	seen := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return out, nil
		case e, ok := <-entries:
			if !ok {
				return out, nil
			}
			if f, ok := foundFromEntry(e.Instance, e.AddrIPv4, e.Port, e.Text); ok && !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f)
			}
		}
	}
}

// txtRecord is what a hub advertises: its name, API version and TLS.
func txtRecord(name, apiVersion string, useTLS bool) []string {
	tls := "0"
	if useTLS {
		tls = "1"
	}
	return []string{"name=" + name, "api=" + apiVersion, "tls=" + tls}
}

// probeTimeout bounds the TCP probe that picks a found hub's address.
var probeTimeout = time.Second

// reachable picks, of a hub's advertised addresses, the first that accepts
// a TCP connection (a hub on several interfaces answers with all of them,
// and not every one is reachable from here); the first when none does.
func reachable(ips []net.IP, port int) net.IP {
	if len(ips) == 1 {
		return ips[0]
	}
	for _, ip := range ips {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)), probeTimeout)
		if err == nil {
			_ = c.Close()
			return ip
		}
	}
	return ips[0]
}

// foundFromEntry turns a browse answer into a Found: the TXT name (else
// the instance), the first reachable IPv4 address, and https:// when tls=1.
func foundFromEntry(instance string, ips []net.IP, port int, txt []string) (Found, bool) {
	f := Found{Name: instance}
	useTLS := false
	for _, kv := range txt {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "name":
			f.Name = v
		case "api":
			f.APIVersion = v
		case "tls":
			useTLS = v == "1"
		}
	}
	if len(ips) == 0 || port == 0 || f.Name == "" {
		return Found{}, false
	}
	f.Addr = net.JoinHostPort(reachable(ips, port).String(), strconv.Itoa(port))
	if useTLS {
		f.Addr = "https://" + f.Addr
	}
	return f, true
}

// describeFound renders a browse result for the log.
func describeFound(found []Found) string {
	if len(found) == 0 {
		return "no other hub"
	}
	parts := make([]string, len(found))
	for i, f := range found {
		parts[i] = fmt.Sprintf("%s at %s", f.Name, f.Addr)
	}
	return strings.Join(parts, ", ")
}
