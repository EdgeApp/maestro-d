package daemon

// maestro-d: the per-device traffic-shaping calls a host is configured
// with (pkg/netshape), wired into sessions and device listings.

import (
	"path/filepath"
	"sync"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// networkConfigPath is the network.json this daemon reads: --network-config,
// else <home>/network.json.
func (s *Server) networkConfigPath() string {
	if s.cfg.NetworkConfig != "" {
		return s.cfg.NetworkConfig
	}
	return filepath.Join(Home(), netshape.FileName)
}

// trafficShaper is a session's shaper for one device. The file is read on
// every call, so an edit needs no restart.
func (s *Server) trafficShaper(id string) *netshape.Shaper {
	return &netshape.Shaper{Path: s.networkConfigPath(), DeviceID: id, Log: logger.Info}
}

// markTrafficShaping sets trafficShaping on each device this host can
// shape. A config that cannot be read shapes nothing, and is logged once
// per problem: listings are polled, the shaping commands answer USAGE.
func (s *Server) markTrafficShaping(devs []DeviceInfo) {
	if len(devs) == 0 {
		return
	}
	path := s.networkConfigPath()
	cfg, err := netshape.Load(path)
	if err != nil {
		warnShapingOnce(path, err.Error())
		return
	}
	shapingWarned.Delete(path)
	for i := range devs {
		// Load refuses a device with only one of its calls, so an entry
		// means both are there.
		if _, ok := cfg.Lookup(devs[i].ID); ok {
			devs[i].TrafficShaping = true
		}
	}
}

// withTrafficShaping is markTrafficShaping for one device.
func (s *Server) withTrafficShaping(info DeviceInfo) DeviceInfo {
	devs := []DeviceInfo{info}
	s.markTrafficShaping(devs)
	return devs[0]
}

// shapingWarned holds, per config path, the last problem a listing logged.
var shapingWarned sync.Map

// warnShapingOnce logs msg unless it is the problem last logged for path,
// and reports whether it logged.
func warnShapingOnce(path, msg string) bool {
	if prev, loaded := shapingWarned.Swap(path, msg); loaded && prev == msg {
		return false
	}
	logger.Warn("traffic shaping: %s", msg)
	return true
}
