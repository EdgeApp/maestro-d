package hubd

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

func (s *Server) localConfig(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if daemon.CallerFrom(r.Context()).Remote {
			daemon.WriteError(w, daemon.Forbidden(daemon.ConfigLocalOnlyMessage))
			return
		}
		if raw := r.PathValue("id"); raw != "" {
			if err := daemon.CheckHubConfigPath(r.URL.Query().Get("expectedPath"), s.config.path); err != nil {
				daemon.WriteError(w, err)
				return
			}
			id, err := daemon.LocalConfigID(raw, s.HubName())
			if err != nil {
				daemon.WriteError(w, err)
				return
			}
			r.SetPathValue("id", id)
		}
		next(w, r)
	}
}

func listedConfigDevice(id string, devices []daemon.DeviceInfo) (daemon.DeviceInfo, bool) {
	for _, d := range devices {
		if daemon.ConfigNames(id, d) {
			return d, true
		}
	}
	return daemon.DeviceInfo{ID: id}, false
}

func configDeviceRow(id string, entry netshape.Device, listed bool) daemon.HubConfigDevice {
	return daemon.HubConfigDevice{ID: id, Listed: listed, Description: entry.Description, TrafficShaping: entry.Shapeable()}
}

func (s *Server) configRow(cfg *netshape.Config, id string, devices []daemon.DeviceInfo) daemon.HubConfigDevice {
	d, listed := listedConfigDevice(id, devices)
	row := configDeviceRow(id, cfg.Devices[id], listed)
	row.Reservations = s.leases.reservations(d.ID)
	if l, ok := s.leases.liveFor(d.ID); ok {
		row.Lease = &daemon.DeviceLease{Holder: l.Holder, StartsAt: l.StartsAt, ExpiresAt: l.ExpiresAt}
	}
	return row
}

func (s *Server) getConfigDevices(w http.ResponseWriter, r *http.Request) {
	s.startDue()
	cfg, err := s.config.snapshot(time.Now(), false)
	res := daemon.HubConfigListing{Envelope: daemon.Envelope{OK: true}, Path: s.config.path, Devices: make([]daemon.HubConfigDevice, 0, len(cfg.Shared))}
	if err != nil {
		res.ConfigError = err.Error()
	}
	devices := s.Registry().List("")
	for _, id := range cfg.Shared {
		res.Devices = append(res.Devices, s.configRow(cfg, id, devices))
	}
	daemon.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) putConfigDevice(w http.ResponseWriter, r *http.Request) {
	var fields netshape.DeviceFields
	if e := daemon.DecodeBody(r, &fields); e != nil {
		daemon.WriteError(w, e)
		return
	}
	id := r.PathValue("id")
	s.configEdits.Lock()
	defer s.configEdits.Unlock()
	cfg, err := s.config.snapshot(time.Now(), true)
	if err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeUsage, err))
		return
	}
	_, existed := cfg.Lookup(id)
	cfg, err = s.config.edit(func() error {
		_, err := netshape.UpsertDevice(s.config.path, id, fields)
		return err
	}, s.configChanged)
	if err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	daemon.WriteJSON(w, status, map[string]any{"ok": true, "device": s.configRow(cfg, id, s.Registry().List(""))})
}

// admitConfigDevice is the final eligibility check, under leaseMu. Socket
// callers can lease private devices, but even they wait for a removal in flight.
func (s *Server) admitConfigDevice(remote bool, d daemon.DeviceInfo) *daemon.Error {
	if s.removing[d.ID] || s.removing[daemon.AVDEntryID(d)] || (remote && !s.sharedInfo(d)) {
		return daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", d.ID)
	}
	return nil
}

type removedLease struct {
	device string
	until  time.Time
}

// Keep every ended credential through its window. If the bound is reached,
// refuse another forced release until a window ends.
const maxRemovedLeases = 4096

func (s *Server) pruneRemovedLeases() {
	now := s.leases.now()
	maps.DeleteFunc(s.removedLeases, func(_ string, entry removedLease) bool { return !now.Before(entry.until) })
}

func (s *Server) removedLeaseMatches(device, credential string) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	entry, ok := s.removedLeases[credential]
	if ok && !s.leases.now().Before(entry.until) {
		delete(s.removedLeases, credential)
		return false
	}
	return ok && entry.device == device
}

func (s *Server) publishLeaseEnd(e *leaseEntry, reason string) {
	event := daemon.Event{Type: leaseEndEvent(reason), Device: e.device, Message: daemon.HolderName(e.holder)}
	if reason == endRemoved {
		event.Reason = reason
	}
	s.Events().Publish(event)
}

type deviceRemoval struct {
	aliases   []string
	devices   []daemon.DeviceInfo
	teardowns []func()
}

// prepareRemoval checks all grants and installs the admission mark in one
// critical section. After it returns, unrelated devices can be leased even
// while this device's driver, state file and config file are being closed.
func (s *Server) prepareRemoval(cfg *netshape.Config, id string, force bool) (*deviceRemoval, *daemon.Error) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if _, ok := cfg.Lookup(id); !ok {
		return nil, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is not in %s", id, s.config.path)
	}
	devices := s.Registry().List("")
	d, _ := listedConfigDevice(id, devices)
	aliases := []string{d.ID}
	if alias := daemon.AVDEntryID(d); alias != "" {
		aliases = append(aliases, alias)
	}
	targetAVD := avdKey(d)
	if strings.HasPrefix(id, daemon.AVDPrefix) {
		targetAVD = bootKey(daemon.DeviceInfo{ID: id})
	}
	var entries []*leaseEntry
	var windows []string
	// When both identities are known, the recorded AVD takes precedence
	// over a serial which another emulator may since have reused.
	for _, l := range s.leases.list() {
		e := s.leases.lookup(l.ID)
		matches := slices.Contains(aliases, e.device)
		if e.avd != "" && targetAVD != "" {
			matches = e.avd == targetAVD
		}
		if !matches {
			continue
		}
		entries = append(entries, e)
		windows = append(windows, fmt.Sprintf("%s (%s to %s)", daemon.HolderName(l.Holder), l.StartsAt, l.ExpiresAt))
	}
	if !force && len(windows) > 0 {
		return nil, daemon.Errorf(daemon.CodeDeviceInUse, "device %s is in use by %s", id, strings.Join(windows, ", "))
	}
	s.pruneRemovedLeases()
	if len(entries)+len(s.removedLeases) > maxRemovedLeases {
		return nil, daemon.Errorf(daemon.CodeBusy, "too many recently ended lease windows; retry when a window expires")
	}
	rm := &deviceRemoval{aliases: aliases, devices: devices}
	for _, key := range aliases {
		s.removing[key] = true
	}
	for _, e := range entries {
		s.removedLeases[e.id] = removedLease{device: e.device, until: e.expires}
		rm.teardowns = append(rm.teardowns, s.endLeaseLocked(e, endRemoved))
	}
	return rm, nil
}

func (s *Server) deleteConfigDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.configEdits.Lock()
	defer s.configEdits.Unlock()
	cfg, err := s.config.snapshot(time.Now(), true)
	if err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeUsage, err))
		return
	}
	rm, e := s.prepareRemoval(cfg, id, r.URL.Query().Get("endLeases") == "true")
	if e != nil {
		daemon.WriteError(w, e)
		return
	}
	defer func() {
		s.leaseMu.Lock()
		for _, key := range rm.aliases {
			delete(s.removing, key)
		}
		s.leaseMu.Unlock()
	}()
	for _, teardown := range rm.teardowns {
		teardown()
	}
	// A previously interrupted cancellation may still need its disk flush.
	if err := s.flushReservations(); err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
		return
	}
	_, err = s.config.edit(func() error {
		return netshape.RemoveDevices(s.config.path, id, rm.aliases)
	}, func(old, current *netshape.Config) {
		s.publishConfigChange(old, current, rm.devices)
	})
	if err != nil {
		code := daemon.CodeInternal
		if errors.Is(err, os.ErrNotExist) {
			code = daemon.CodeDeviceNotFound
		}
		daemon.WriteError(w, daemon.WrapErr(code, err))
		return
	}
	daemon.WriteJSON(w, http.StatusOK, daemon.Envelope{OK: true})
}
