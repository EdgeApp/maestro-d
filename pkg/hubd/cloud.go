package hubd

import (
	"net/http"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// leaseCloud launches a cloud device for a selector lease with location
// cloud, on this hub, and leases it: the hub's --cloud-max-devices caps how
// many it holds (BUSY when full, launches in progress included), the window
// is at most the provider's session, and the device is reserved from its
// launch until the lease is granted, so no other request takes it.
func (s *Server) leaseCloud(r *http.Request, sel daemon.LeaseSelector, cloud *cloudprov.CloudSpec, holder string, ttlSec int) (*leaseEntry, *daemon.Error) {
	spec, e := s.PrepareCloudLease(sel, cloud)
	if e != nil {
		return nil, e
	}
	maxTTL := min(s.hubLeaseMax(), cloudLeaseMax)
	if sel.Name != "" {
		// A per-device maximum named for the model is checked before the
		// launch, so a window over it costs no session. (A model the name
		// matches only as a pattern is checked once it is launched.)
		maxTTL = min(maxTTL, s.leaseMaxInfo(daemon.DeviceInfo{ID: sel.Name, Name: sel.Name, Provider: spec.Provider}))
	}
	if _, e := leaseWindow("", ttlSec, maxTTL); e != nil {
		return nil, e
	}
	// The cap counts the devices held and the launches under way.
	s.leaseMu.Lock()
	held := s.CloudHeld(spec.Provider) + s.cloudLaunching
	if held >= s.cfg.CloudMaxDevices {
		s.leaseMu.Unlock()
		return nil, daemon.Errorf(daemon.CodeBusy, "hub %s holds %d cloud device(s), its limit (--cloud-max-devices %d); release one or try another hub",
			s.HubName(), held, s.cfg.CloudMaxDevices).WithDetails("limit", s.cfg.CloudMaxDevices)
	}
	s.cloudLaunching++
	s.leaseMu.Unlock()
	defer func() {
		s.leaseMu.Lock()
		s.cloudLaunching--
		s.leaseMu.Unlock()
	}()
	allocation, e := s.LaunchCloudAllocation(r.Context(), spec, func(id string) {
		// Reserved before it is listed: nothing else leases it first.
		s.leases.mu.Lock()
		s.leases.booting[id] = true
		s.leases.mu.Unlock()
	})
	if e != nil {
		return nil, e
	}
	defer func() {
		s.leases.mu.Lock()
		delete(s.leases.booting, allocation.ID)
		s.leases.mu.Unlock()
	}()
	d, ok := s.Registry().Lookup(allocation.ID)
	if !ok {
		allocation.Release(false)
		return nil, daemon.Errorf(daemon.CodeDeviceError, "cloud device %s vanished before its lease was granted", allocation.ID)
	}
	if s.ShuttingDown() {
		// The hub's leases have ended: no new one, and the device goes.
		allocation.Release(false)
		return nil, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %s is shutting down", s.HubName())
	}
	// The device is bound to the lease as it is granted, under leaseMu: a
	// lease ended at once (the hub stopping) still releases it.
	entry, le := s.leaseDevice(d, holder, ttlSec, claimReserved, func(e *leaseEntry) {
		e.cloudDone = make(chan struct{})
		e.releaseCloud = func() *cloudprov.DeviceMinutes {
			return allocation.Release(true)
		}
		allocation.OnEnd(func() { s.endLease(e, endCloudReleased) })
	})
	if le != nil {
		allocation.Release(false)
		return nil, le
	}
	s.Events().Publish(daemon.Event{Type: "device.created", Device: allocation.ID, Message: d.Name})
	s.Events().Publish(daemon.Event{Type: "device.booted", Device: allocation.ID, Message: d.Name})
	return entry, nil
}
