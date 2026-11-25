package daemon

import (
	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"strings"
)

// IsEmulatorSerial reports whether id is an Android emulator's adb serial
// (emulator-<port>), which the next AVD to boot on that port reuses.
func IsEmulatorSerial(id string) bool {
	return strings.HasPrefix(id, "emulator-")
}

// annotateAvailable sets available on a plain daemon's entries: ready, not
// attached here or by another daemon, and not an AVD that is not running.
// A hub's annotate also weighs its leases.
func annotateAvailable(devs []DeviceInfo, owners map[string]string) {
	for i := range devs {
		d := &devs[i]
		d.Available = d.Ready && !d.Attached && owners[d.ID] == "" && !strings.HasPrefix(d.ID, AVDPrefix)
	}
}

// OtherOwners maps each device another live daemon has attached to that
// daemon's name.
func OtherOwners(self string) map[string]string {
	out := map[string]string{}
	daemons, _ := ListDaemons()
	for _, d := range daemons {
		if d.Name == self || !d.Alive {
			continue
		}
		for _, dev := range d.Devices {
			if dev.Attached {
				out[dev.ID] = d.Name
			}
		}
	}
	return out
}

type LeaseMode int

const (
	// LeaseOpen: GET /v1/devices/{id} and …/info stay open to every
	// authenticated client.
	LeaseOpen LeaseMode = iota
	// LeaseNeeded: every route that drives or changes the device.
	LeaseNeeded
	// LeaseIfHeld: stopping or deleting a device needs its lease only when
	// someone holds one.
	LeaseIfHeld
)

func (sel LeaseSelector) Matches(d DeviceInfo) bool {
	if sel.Platform != "" && !strings.EqualFold(sel.Platform, d.Platform) {
		return false
	}
	if sel.Kind != "" && !strings.EqualFold(sel.Kind, d.Kind) {
		return false
	}
	// TVs are taken only by a selector that names their kind or name, and
	// the computer only by one that names its kind, name or platform, so a
	// selector for phones never lands on either.
	if sel.Kind == "" && sel.Name == "" && strings.EqualFold(d.Kind, "tv") {
		return false
	}
	if sel.Kind == "" && sel.Name == "" && sel.Platform == "" && IsComputer(d) {
		return false
	}
	if sel.Name != "" && sel.Name != d.ID && !strings.EqualFold(sel.Name, d.Name) {
		return false
	}
	if !cloudprov.OSVersionMatches(d.OSVersion, sel.OSVersion) {
		return false
	}
	if (sel.OSVersionMin != "" || sel.OSVersionMax != "") && !VersionInRange(d.OSVersion, sel.OSVersionMin, sel.OSVersionMax) {
		return false
	}
	return true
}

// HubName is the hub's name: Config.HubName, else the short host name.
func (s *Server) HubName() string {
	if s.cfg.HubName != "" {
		return s.cfg.HubName
	}
	return DefaultHubName()
}
