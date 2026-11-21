package daemon

// location.go: where each listed device is, relative to the caller's
// computer: local (attached to or running on it), mesh (on another computer
// in the hub mesh) or cloud (held by a cloud provider).

import (
	"net"
	"net/http"
	"os"
	"strings"
)

// HeaderClientMachine names the caller's computer. The Go, CLI and JS
// clients always send it, so a daemon or hub can tell which listed devices
// are on that computer.
const HeaderClientMachine = "X-Maestro-Client-Machine"

// Device locations.
const (
	LocationLocal = "local"
	LocationMesh  = "mesh"
	LocationCloud = "cloud"
)

// envTestMachine overrides the machine name, so tests can run several
// "machines" on one host.
const envTestMachine = "MAESTRO_D_TEST_MACHINE"

// MachineName is this computer's name as maestro-d reports it: the host name,
// or MAESTRO_D_TEST_MACHINE when that is set.
func MachineName() string {
	if v := os.Getenv(envTestMachine); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}

// SameMachine compares machine names case-insensitively, ignoring a
// trailing ".local".
func SameMachine(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		return strings.TrimSuffix(strings.TrimSuffix(s, "."), ".local")
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// CallerMachine is the machine a request came from: its
// X-Maestro-Client-Machine header; else this machine (self) when the request
// arrived over the unix socket (local) or from a loopback address; else ""
// (unknown).
func CallerMachine(r *http.Request, local bool, self string) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderClientMachine)); v != "" {
		return v
	}
	if local {
		return self
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopbackHost(host) {
		return self
	}
	return ""
}

// SetLocations sets each device's location for a caller on machine caller
// ("" when unknown): cloud for a provider's device, local when the device's
// machine is the caller's, mesh otherwise.
func SetLocations(devs []DeviceInfo, caller string) {
	for i := range devs {
		devs[i].Location = deviceLocation(devs[i], caller)
	}
}

func deviceLocation(d DeviceInfo, caller string) string {
	switch {
	case d.Provider != "":
		return LocationCloud
	case SameMachine(d.Machine, caller):
		return LocationLocal
	default:
		return LocationMesh
	}
}

// callerMachine is CallerMachine for a request this daemon serves directly.
func (s *Server) callerMachine(r *http.Request) string {
	return CallerMachine(r, CallerFrom(r.Context()).Addr == "", MachineName())
}
