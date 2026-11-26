package daemon

// maestro-d: a host's device config (pkg/netshape: config.json, else
// network.json) — each device's traffic-shaping calls and description —
// wired into sessions and device listings and answers.

import (
	"strings"
	"sync"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// DeviceConfigSource is where a daemon reads its device config: config
// (--config), else networkConfig (--network-config), else
// <home>/config.json, falling back to <home>/network.json when config.json
// does not exist.
func DeviceConfigSource(config, networkConfig string) netshape.Source {
	return netshape.Source{Config: config, Legacy: networkConfig, Home: Home()}
}

// deviceConfig is this daemon's DeviceConfigSource.
func (s *Server) deviceConfig() netshape.Source {
	return DeviceConfigSource(s.cfg.DeviceConfig, s.cfg.NetworkConfig)
}

// logDeviceConfig logs, at start, the device config file this daemon
// reads, and warns about each other one that the resolution order leaves
// unread.
func (s *Server) logDeviceConfig() {
	src := s.deviceConfig()
	path, legacy := src.Resolve()
	format := netshape.ConfigFileName
	if legacy {
		format = netshape.FileName + ", the older format"
	}
	logger.Info("device config: %s (%s)", path, format)
	for _, p := range src.Unread() {
		logger.Warn("device config: %s is not read; only %s is", p, path)
	}
}

// trafficShaper is a session's shaper for one device. The file is read on
// every call, so an edit needs no restart; the AVD a running emulator
// runs is looked up once, here at attach, so a set and its reset use the
// same entry and no shaping call runs discovery.
func (s *Server) trafficShaper(id string) *netshape.Shaper {
	return &netshape.Shaper{Source: s.deviceConfig(), DeviceID: id, Alias: s.avdAlias(id), Log: logger.Info}
}

// avdAlias is the avd:<name> entry of the AVD that emulator serial id
// runs, whose calls the session uses when the serial has none; "" for any
// other device.
func (s *Server) avdAlias(id string) string {
	if !isEmulatorSerial(id) {
		return ""
	}
	if d, ok := s.registry.Lookup(id); ok {
		return avdEntryID(d)
	}
	return ""
}

// avdEntryID is the config entry that names a running emulator by its AVD,
// avd:<name>, since its serial changes from boot to boot; "" for any other
// device.
func avdEntryID(d DeviceInfo) string {
	if d.Kind != "emulator" || d.Name == "" || strings.HasPrefix(d.ID, AVDPrefix) {
		return ""
	}
	return AVDPrefix + d.Name
}

// configNames reports whether a hubDevices id names device d: its own id,
// or, for a running emulator, its avd:<name>. A boot or create by a bare
// name (no kind yet) is taken to be the AVD of that name only when it asks
// for android: without a platform a bare name may boot a simulator. Names
// do not match, as they do for --share-device: an entry is an id.
func configNames(entry string, d DeviceInfo) bool {
	if entry == d.ID {
		return true
	}
	if d.Kind == "" {
		return d.Platform == "android" && d.Name != "" && !strings.HasPrefix(d.ID, AVDPrefix) &&
			entry == AVDPrefix+d.Name
	}
	return entry != "" && entry == avdEntryID(d)
}

// configEntries returns device d's config entries, its own id's first,
// then a running emulator's AVD's.
func configEntries(cfg *netshape.Config, d DeviceInfo) []netshape.Device {
	var out []netshape.Device
	for _, id := range []string{d.ID, avdEntryID(d)} {
		if id == "" {
			continue
		}
		if e, ok := cfg.Lookup(id); ok {
			out = append(out, e)
		}
	}
	return out
}

// applyDeviceConfig sets, from the device config, each device's
// description, capabilities and trafficShaping (whether this host can shape it). Each
// comes from the first of the device's entries that has one (see
// configEntries), as the session's shaper picks its calls. A config that
// cannot be read sets neither, and is logged once per problem: listings
// are polled, the shaping commands answer USAGE.
func (s *Server) applyDeviceConfig(devs []DeviceInfo) {
	if len(devs) == 0 {
		return
	}
	cfg, path, err := s.deviceConfig().Load()
	if err != nil {
		warnShapingOnce(path, err.Error())
		return
	}
	shapingWarned.Delete(path)
	for i := range devs {
		dev := &devs[i]
		dev.TrafficShaping, dev.Description, dev.Capabilities = false, "", nil
		for _, e := range configEntries(cfg, *dev) {
			dev.TrafficShaping = dev.TrafficShaping || e.Shapeable()
			if dev.Description == "" {
				dev.Description = e.Description
			}
			if dev.Capabilities == nil {
				dev.Capabilities = e.Capabilities
			}
		}
	}
}

// withDeviceConfig is applyDeviceConfig for one device.
func (s *Server) withDeviceConfig(info DeviceInfo) DeviceInfo {
	devs := []DeviceInfo{info}
	s.applyDeviceConfig(devs)
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
	logger.Warn("device config: %s", msg)
	return true
}
