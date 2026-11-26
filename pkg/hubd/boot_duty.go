package hubd

import (
	"context"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// An AVD's boot duty belongs to its name even when adb reuses its old port.
type bootDuty struct {
	Device string `json:"device"`
	AVD    string `json:"avd,omitempty"`
}

func (d bootDuty) key() string {
	if d.AVD != "" {
		return d.AVD
	}
	return d.Device
}

func (e *leaseEntry) duty() bootDuty { return bootDuty{Device: e.device, AVD: e.avd} }
func (e *leaseEntry) restoredAVD() bool {
	return e.restored && (e.avd != "" || daemon.IsEmulatorSerial(e.device))
}
func (e *leaseEntry) tableKey() string {
	if e.restoredAVD() {
		if e.avd == "" {
			return "unresolved:" + e.id
		}
		return e.avd
	}
	return e.device
}

// resolve uses discovery alone: a registry record may refer to a process
// that has since lost its emulator serial. Ambiguous live names fail closed.
func (d bootDuty) resolve(devices []daemon.DeviceInfo) (daemon.DeviceInfo, bool) {
	if d.AVD == "" && daemon.IsEmulatorSerial(d.Device) {
		return daemon.DeviceInfo{}, false
	}
	var match daemon.DeviceInfo
	for _, info := range devices {
		if d.AVD != "" {
			if avdKey(info) != d.AVD {
				continue
			}
		} else if info.ID != d.Device {
			continue
		}
		if match.ID != "" {
			return daemon.DeviceInfo{}, false
		}
		match = info
	}
	// An installed AVD is present even if adb cannot identify an emulator.
	// Only confirmed shutdown discharges a duty without stopping a serial.
	if strings.HasPrefix(match.ID, daemon.AVDPrefix) && !strings.EqualFold(match.State, "Shutdown") {
		return daemon.DeviceInfo{}, false
	}
	return match, match.ID != ""
}

func (s *Server) resolvedDevice(e *leaseEntry) string {
	if !e.restoredAVD() {
		return e.device
	}
	return e.duty().runningSerial(s.Registry().Live("android"))
}

func (d bootDuty) runningSerial(devices []daemon.DeviceInfo) string {
	info, ok := d.resolve(devices)
	if !ok || !info.Ready || !daemon.IsEmulatorSerial(info.ID) {
		return ""
	}
	return info.ID
}

func (s *Server) VerifyBinding(ctx context.Context, id string) *daemon.Error {
	if e := leaseFrom(ctx); e != nil && e.restoredAVD() && s.resolvedDevice(e) != id {
		return daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is no longer bound to lease %s", id, e.id)
	}
	return nil
}

func (t *leaseTable) deviceKey(info daemon.DeviceInfo) string {
	if key := avdKey(info); key != "" {
		t.mu.Lock()
		held := t.byDevice[key] != nil
		t.mu.Unlock()
		if held {
			return key
		}
	}
	return info.ID
}

// transferDuty applies the live handoff rule to both live and restored
// grants. The recipient and removal of the duty share one disk snapshot.
func (t *leaseTable) transferDuty(d bootDuty) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d.AVD == "" && daemon.IsEmulatorSerial(d.Device) {
		return false
	}
	next := t.successorLocked(d.key())
	if next == nil && d.AVD != "" {
		for _, e := range t.byID {
			if e.avd == d.AVD && (next == nil || e.starts.Before(next.starts)) {
				next = e
			}
		}
	}
	if next == nil || !t.now().Before(next.expires) || next.starts.After(t.now().Add(handBootGrace)) {
		return false
	}
	next.booted = true
	delete(t.bootDuties, d.key())
	return true
}

// A scheduled successor can enter the handoff window during platform discovery.
func (s *Server) handOffBootDuty(d bootDuty, reason string) bool {
	if reason == endHubStopped {
		return false
	}
	s.leaseMu.Lock()
	transferred := s.leases.transferDuty(d)
	s.leaseMu.Unlock()
	if transferred {
		s.queueLeases(true)
	}
	return transferred
}

// The caller reserves this device throughout discovery and shutdown. Other
// devices can continue to receive grants and renewals while discovery runs.
func (s *Server) dischargeBoot(d bootDuty, reason string) {
	if s.handOffBootDuty(d, reason) {
		return
	}
	info, found := d.resolve(s.Registry().Live(""))
	if s.handOffBootDuty(d, reason) {
		return
	}
	if !found || s.cfg.FindDeviceOwner(info.ID) != nil {
		return
	}
	if !strings.EqualFold(info.State, "Shutdown") && !strings.HasPrefix(info.ID, daemon.AVDPrefix) {
		if err := s.Registry().StopLeaseBoot(info); err != nil {
			logger.Warn("lease: shut down %s: %v", info.ID, err)
			return
		}
		s.Events().Publish(daemon.Event{Type: "device.stopped", Device: info.ID, Message: "lease " + reason})
	}
	s.leases.mu.Lock()
	delete(s.leases.bootDuties, d.key())
	s.leases.mu.Unlock()
	s.queueLeases(true)
}

func (s *Server) retryBootDuties() {
	t := s.leases
	s.leaseMu.Lock()
	if s.ShuttingDown() {
		s.leaseMu.Unlock()
		return
	}
	t.mu.Lock()
	var duties []bootDuty
	for _, d := range t.bootDuties {
		if !t.ending[d.key()] {
			t.beginEndingLocked(d.key())
			s.teardowns.add()
			duties = append(duties, d)
		}
	}
	t.mu.Unlock()
	s.leaseMu.Unlock()
	for _, d := range duties {
		s.dischargeBoot(d, endExpired)
		t.mu.Lock()
		t.doneEndingLocked(d.key())
		t.mu.Unlock()
		s.teardowns.done()
	}
}
