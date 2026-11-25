package daemon

// Wire types and conventions shared by hub clients, the mesh and the hub.
import (
	"os"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// HeaderLease carries the lease id on every request that drives a leased
// device.
const HeaderLease = "X-Maestro-Lease"

const (
	// DefaultLeaseTTL is a lease's window when the request names none (or
	// the device's maximum, when that is shorter).
	DefaultLeaseTTL = 8 * time.Hour
	// DefaultLeaseMax is the longest window a hub grants unless
	// --lease-max says otherwise.
	DefaultLeaseMax = 8 * time.Hour
)

// Lease is one lease, as the lease routes return it.
type Lease struct {
	// ID is what X-Maestro-Lease sends.
	ID     string `json:"id"`
	Device string `json:"device"`
	Holder string `json:"holder,omitempty"`
	// StartsAt is RFC 3339: the start of the window (the grant, for a
	// lease taken at once). A renew leaves it as it is.
	StartsAt string `json:"startsAt"`
	// ExpiresAt is RFC 3339: the end of the window. Only an explicit
	// renew moves it.
	ExpiresAt string `json:"expiresAt"`
	// State is LeaseScheduled until the window starts, then LeaseActive.
	State string `json:"state"`
	// TTLSec is the window's length; MaxSec the device's maximum window.
	TTLSec int `json:"ttlSec"`
	MaxSec int `json:"maxSec"`
	// Booted is true when the hub booted the device for this lease; it
	// shuts it down again when the lease ends.
	Booted bool `json:"booted,omitempty"`
}

// The states of a lease.
const (
	LeaseScheduled = "scheduled"
	LeaseActive    = "active"
)

// DeviceLease is the lease state a hub's device listings carry: the
// active lease, and each reservation, which carries StartsAt.
type DeviceLease struct {
	Holder    string `json:"holder,omitempty"`
	StartsAt  string `json:"startsAt,omitempty"`
	ExpiresAt string `json:"expiresAt"`
}

// LeaseSelector picks a device by its properties. Every field set must
// match: platform and kind exactly, name as the device's name or id,
// osVersion as the version or a prefix of it ("26" matches "26.5"), hub as
// the hub's name. A TV (kind tv) is picked only by a selector that asks
// for kind tv or names the TV: a client asking for an iOS device wants a
// phone or tablet.
type LeaseSelector struct {
	Platform  string `json:"platform,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	Hub       string `json:"hub,omitempty"`
	// Location "cloud" launches a cloud device for the lease (remote.go):
	// Provider (default devicefarm), Platform, Name (the model), OSVersion
	// and the OSVersionMin/Max range pick it. In a mesh it goes to the
	// hub the launch routing rule names (cloudroute.go).
	Location     string `json:"location,omitempty"`
	Provider     string `json:"provider,omitempty"`
	OSVersionMin string `json:"osVersionMin,omitempty"`
	OSVersionMax string `json:"osVersionMax,omitempty"`
	// Boot makes shut-down simulators and AVDs candidates too: the hub
	// boots the one it picks, and shuts it down when the lease ends.
	Boot bool `json:"boot,omitempty"`
}

// LeaseRequest is the body of POST /v1/leases.
type LeaseRequest struct {
	// Device or Select names the device; exactly one is given.
	Device string         `json:"device,omitempty"`
	Select *LeaseSelector `json:"select,omitempty"`
	// TTLSec is the window in seconds: default 8 hours (or the device's
	// maximum, when that is shorter); over the maximum is USAGE.
	TTLSec int    `json:"ttlSec,omitempty"`
	Holder string `json:"holder,omitempty"`
	// Attach, when given, attaches the device in the same call.
	Attach *AttachConfig `json:"attach,omitempty"`
	// Cloud tunes a cloud launch (Select.Location "cloud"): idle release,
	// Appium version, capabilities, app id. Apps are installed after the
	// lease, through the install route.
	Cloud *cloudprov.CloudSpec `json:"cloud,omitempty"`
}

// RenewRequest is the body of POST /v1/leases/{lid}/renew.
type RenewRequest struct {
	// TTLSec is the new window from now (default the lease's own).
	TTLSec int `json:"ttlSec,omitempty"`
}

// LeaseMax is one --lease-max DEVICE=DURATION: the longest window a lease
// on that device (by id or name) may have.
type LeaseMax struct {
	Device string
	Max    time.Duration
}

// LeaseResult answers POST /v1/leases, GET /v1/leases/{lid}, renew and
// release.
type LeaseResult struct {
	Envelope
	Lease  Lease       `json:"lease"`
	Device *DeviceInfo `json:"device,omitempty"`
	// DeviceMinutes, on a release, is the time the lease's cloud device
	// used (absent when its release was still pending or failed).
	DeviceMinutes *cloudprov.DeviceMinutes `json:"deviceMinutes,omitempty"`
}

// LeasesResult answers GET /v1/leases.
type LeasesResult struct {
	Envelope
	Leases []Lease `json:"leases"`
	// Unreachable, on a hub, names the peers whose leases are missing.
	Unreachable []UnreachablePeer `json:"unreachable,omitempty"`
}

// DefaultReservationLead is how far ahead a reservation may start unless
// --reservation-lead says otherwise.
const DefaultReservationLead = 24 * time.Hour

// ReservationRequest is the body of POST /v1/reservations. Unknown fields
// are refused.
type ReservationRequest struct {
	// Device or Select names the device; exactly one is given.
	Device string         `json:"device,omitempty"`
	Select *LeaseSelector `json:"select,omitempty"`
	// StartAt is the window's start, RFC 3339.
	StartAt string `json:"startAt"`
	// ClientTime is the caller's clock when it sent the request, RFC 3339.
	ClientTime string `json:"clientTime"`
	// TTLSec is the window's length, as for POST /v1/leases.
	TTLSec int    `json:"ttlSec,omitempty"`
	Holder string `json:"holder,omitempty"`
}

// String renders the selector for messages: {platform=ios kind=simulator}.
func (sel LeaseSelector) String() string {
	var parts []string
	for _, kv := range [][2]string{{"platform", sel.Platform}, {"kind", sel.Kind}, {"name", sel.Name}, {"osVersion", sel.OSVersion},
		{"osVersionMin", sel.OSVersionMin}, {"osVersionMax", sel.OSVersionMax}, {"hub", sel.Hub}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if sel.Boot {
		parts = append(parts, "boot=true")
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// HolderName is how a lease's holder reads in messages and listings:
// the holder, or "another client" when it gave none.
func HolderName(h string) string {
	if h == "" {
		return "another client"
	}
	return h
}

// DefaultHubName is the short host name, lowercased, as a hub name:
// [a-z0-9-]{1,32}.
func DefaultHubName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	var b strings.Builder
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 32 {
		name = name[:32]
	}
	if name == "" {
		return "hub"
	}
	return name
}

// NoRouteMessage is a daemon's answer to a route it does not have.
func NoRouteMessage(method, path string) string {
	return "no route for " + method + " " + path
}

// ReservationsUnsupported is USAGE for hub, which predates reservations
// and answered POST /v1/reservations "no route".
func ReservationsUnsupported(hub string) *Error {
	return Errorf(CodeUsage, "hub %s does not take reservations; it needs a maestro-d release that has them", hub).
		WithDetails("hub", hub, "reason", "reservations-unsupported")
}
