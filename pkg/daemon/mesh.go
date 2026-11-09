package daemon

// mesh.go: the seam between the daemon and a hub's peer mesh (pkg/hub,
// which imports this package, never the other way round), and the hub
// status types of GET /v1/hub.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
)

// Mesh is a hub's peer mesh. The daemon puts its router in front of both
// listeners' handlers, starts it once the listeners are up, and closes it
// first when it shuts down.
type Mesh interface {
	// Handler returns the handler a listener serves: the mesh's router in
	// front of local, the daemon's own handler for that listener.
	Handler(local http.Handler, tcp bool) http.Handler
	// Start runs discovery and dials the peers. It returns when the hub
	// may report READY; an error stops the hub (a peer refused it).
	Start(ctx context.Context, self Info) error
	// Close ends every peer link.
	Close()
}

// HubStatusResult answers GET /v1/hub.
type HubStatusResult struct {
	Envelope
	Hub HubStatus `json:"hub"`
}

// HubStatus describes a hub and its peers.
type HubStatus struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	APIVersion string `json:"apiVersion"`
	// Addr is the --advertise address ("" when peers use the link's).
	Addr string `json:"addr,omitempty"`
	// Listen is the bound client and peer listener.
	Listen string `json:"listen,omitempty"`
	TLS    bool   `json:"tls,omitempty"`
	// MDNS is false with --no-mdns.
	MDNS  bool      `json:"mdns"`
	Peers []HubPeer `json:"peers"`
}

// HubPeer is one peer in GET /v1/hub.
type HubPeer struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr,omitempty"`
	// State: linked, down, stopped (it shut down cleanly), connecting (a
	// dial that has not linked yet) or refused (it refused this hub).
	State string `json:"state"`
	// Direction: outbound (this hub dialed) or inbound.
	Direction string `json:"direction,omitempty"`
	// RTTMs is the last ping round trip.
	RTTMs float64 `json:"rttMs,omitempty"`
	// Devices is how many devices the peer shares (linked peers only).
	Devices *int   `json:"devices,omitempty"`
	Since   string `json:"since,omitempty"`
	Error   string `json:"error,omitempty"`
}

// UnreachablePeer is a hub a mesh-wide listing could not ask.
type UnreachablePeer struct {
	Hub   string `json:"hub"`
	Error string `json:"error,omitempty"`
}

// Hub returns GET /v1/hub.
func (c *Client) Hub(ctx context.Context) (*HubStatusResult, error) {
	var out HubStatusResult
	if e := c.call(ctx, http.MethodGet, "/v1/hub", nil, &out); e != nil {
		return nil, e
	}
	return &out, nil
}

// DeviceList is Devices with the whole answer: on a hub it names the
// peers that did not answer.
func (c *Client) DeviceList(ctx context.Context, platform string) (*DevicesResult, error) {
	path := "/v1/devices"
	if platform != "" {
		path += "?platform=" + url.QueryEscape(platform)
	}
	var out DevicesResult
	if e := c.call(ctx, http.MethodGet, path, nil, &out); e != nil {
		return nil, e
	}
	return &out, nil
}

// asError is err as an *Error (DAEMON_UNAVAILABLE when it is not one).
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return WrapErr(CodeDaemonUnavailable, err)
}

// hostIdentity is what a hub's device entries carry to name the machine
// they are attached to: the host name (os.Hostname), and the hub's
// --advertise address followed by the host's non-loopback IPs.
func (s *Server) hostIdentity() (machine string, addrs []string) {
	machine, _ = os.Hostname()
	if s.cfg.Advertise != "" {
		addrs = append(addrs, s.cfg.Advertise)
	}
	return machine, append(addrs, hostIPs(net.InterfaceAddrs)...)
}

// hostIPs lists the host's non-loopback, non-link-local unicast IPs.
func hostIPs(list func() ([]net.Addr, error)) []string {
	all, err := list()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range all {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if s := ip.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
