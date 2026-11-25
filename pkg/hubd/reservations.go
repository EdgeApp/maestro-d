package hubd

// reservations.go: POST /v1/reservations, a lease whose window starts
// later.
//
// A reservation is a lease in state "scheduled": the lease table keeps it
// beside the device's started lease, no other window on the device may
// overlap it, and requests carrying its id answer LEASE_NOT_STARTED until
// its start. At the start the sweeper makes it an ordinary lease
// (lease.granted); no connection waits for that. The holder, or anyone it
// hands the id to, then sends the id in X-Maestro-Lease. A reservation
// starts at most the hub's lead (--reservation-lead, 24 hours by default)
// ahead and lasts at most the device's maximum window, and the caller's
// clock (clientTime) must agree with the hub's within a minute.
//
// The route is new rather than a field of POST /v1/leases: a hub that
// predates it answers "no route" and grants nothing, where it would grant
// an unknown startAt at once.

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// reservationSkew is how far the caller's clock (clientTime) may be from
// the hub's, and how far in the past a startAt may be and still mean now.
const reservationSkew = 60 * time.Second

// reservationLead is the hub's lead.
func (s *Server) reservationLead() time.Duration {
	if s.cfg.ReservationLead > 0 {
		return s.cfg.ReservationLead
	}
	return daemon.DefaultReservationLead
}

// decodeStrict decodes a JSON body, refusing a field v does not have.
func decodeStrict(r *http.Request, v any) *daemon.Error {
	if r.Body == nil {
		return daemon.Errorf(daemon.CodeUsage, "a JSON body is required")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if field, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
			field = strings.Trim(field, `"`)
			return daemon.Errorf(daemon.CodeUsage, "reservations: unknown field %q", field).WithDetails("field", field)
		}
		return daemon.Errorf(daemon.CodeUsage, "invalid JSON body: %v", err)
	}
	return nil
}

// parseReservationTimes checks clientTime and startAt against the hub's
// clock and lead: the start, or zero for one that means now.
func (s *Server) parseReservationTimes(req daemon.ReservationRequest, now time.Time) (time.Time, *daemon.Error) {
	if req.ClientTime == "" {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: clientTime (the caller's clock, RFC 3339) is required").WithDetails("field", "clientTime")
	}
	client, err := time.Parse(time.RFC3339, req.ClientTime)
	if err != nil {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: clientTime %q is not RFC 3339", req.ClientTime).WithDetails("field", "clientTime")
	}
	if skew := client.Sub(now); skew > reservationSkew || skew < -reservationSkew {
		sec := int(math.Round(skew.Seconds()))
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: the client's clock is %d s off hub %s's (at most %d s is allowed); set the client's clock",
			sec, s.HubName(), int(reservationSkew/time.Second)).
			WithDetails("field", "clientTime", "reason", "clock-skew", "clientTime", client.UTC().Format(time.RFC3339),
				"hubTime", now.UTC().Format(time.RFC3339), "skewSec", sec)
	}
	if req.StartAt == "" {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: startAt (RFC 3339) is required").WithDetails("field", "startAt")
	}
	start, err := time.Parse(time.RFC3339, req.StartAt)
	if err != nil {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: startAt %q is not RFC 3339", req.StartAt).WithDetails("field", "startAt")
	}
	// Lease times are whole seconds, as they are reported: a fraction is
	// dropped, so the window starts where startsAt says.
	start = start.Truncate(time.Second)
	if start.Before(now.Add(-reservationSkew)) {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: startAt %s is in the past", start.UTC().Format(time.RFC3339)).
			WithDetails("field", "startAt")
	}
	lead := s.reservationLead()
	if latest := now.Add(lead); start.After(latest) {
		return time.Time{}, daemon.Errorf(daemon.CodeUsage, "reservations: startAt %s is more than %s ahead; the latest start hub %s takes now is %s",
			start.UTC().Format(time.RFC3339), lead, s.HubName(), latest.UTC().Format(time.RFC3339)).
			WithDetails("field", "startAt", "maxStartAt", latest.UTC().Format(time.RFC3339), "maxLeadSec", int(lead/time.Second))
	}
	if !start.After(now) {
		return time.Time{}, nil
	}
	return start, nil
}

func (s *Server) handleReserve(w http.ResponseWriter, r *http.Request) {
	var req daemon.ReservationRequest
	if e := decodeStrict(r, &req); e != nil {
		daemon.WriteError(w, e)
		return
	}
	if (req.Device == "") == (req.Select == nil) {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "reservations: give device or select, not both or neither"))
		return
	}
	if req.TTLSec < 0 {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "reservations: ttlSec must be positive").WithDetails("field", "ttlSec"))
		return
	}
	s.startDue()
	start, e := s.parseReservationTimes(req, s.leases.now())
	if e != nil {
		daemon.WriteError(w, e)
		return
	}
	if req.Select != nil {
		switch {
		case req.Select.Boot:
			e = daemon.Errorf(daemon.CodeUsage, "reservations: select.boot does not apply; the hub boots nothing for a reservation").WithDetails("field", "boot")
		case req.Select.Location != "":
			e = daemon.Errorf(daemon.CodeUsage, "reservations: select.location does not apply; a reservation takes one of the hub's devices").WithDetails("field", "location")
		case req.Select.Provider != "":
			e = daemon.Errorf(daemon.CodeUsage, "reservations: select.provider does not apply; a reservation takes one of the hub's devices").WithDetails("field", "provider")
		}
		if e != nil {
			daemon.WriteError(w, e)
			return
		}
	}
	holder := strings.TrimSpace(req.Holder)
	if holder == "" {
		holder = daemon.DescribeCaller(daemon.CallerFrom(r.Context()))
	}
	var entry *leaseEntry
	if req.Device != "" {
		d, de := s.reservable(r, req.Device)
		if de != nil {
			daemon.WriteError(w, de)
			return
		}
		if start.IsZero() {
			entry, e = s.leaseDevice(d, holder, req.TTLSec, claimNone)
		} else {
			s.leaseMu.Lock()
			entry, e = s.reserveDeviceLocked(d, holder, start, req.TTLSec)
			s.leaseMu.Unlock()
		}
	} else {
		entry, e = s.reserveSelect(r, *req.Select, holder, start, req.TTLSec)
	}
	if e != nil {
		daemon.WriteError(w, e)
		return
	}
	l := entry.view()
	if entry.pending() {
		s.saveReservations()
		logger.Info("lease %s: %s reserved by %s from %s until %s", entry.id, entry.device, holder, l.StartsAt, l.ExpiresAt)
		s.Events().Publish(daemon.Event{Type: "lease.scheduled", Device: entry.device, Message: holder})
	} else {
		logger.Info("lease %s: %s leased by %s for %v (a reservation starting now)", entry.id, entry.device, holder, entry.ttl)
		s.Events().Publish(daemon.Event{Type: "lease.granted", Device: entry.device, Message: holder})
	}
	var info *daemon.DeviceInfo
	if d, ok := s.Registry().Lookup(entry.device); ok {
		devs := []daemon.DeviceInfo{d}
		s.annotate(devs)
		s.ApplyDeviceConfig(devs)
		info = &devs[0]
	}
	daemon.WriteJSON(w, http.StatusOK, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l, Device: info})
}

// reservable returns the listed, shared device id names, refusing one no
// reservation can hold: a shut-down AVD and a running emulator (an
// emulator's serial names whichever AVD runs on that port when the
// reservation starts) and a cloud device (it goes with the lease that
// launched it).
func (s *Server) reservable(r *http.Request, id string) (daemon.DeviceInfo, *daemon.Error) {
	if !s.sharedWith(r, id) {
		return daemon.DeviceInfo{}, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", id)
	}
	if isEmulator(daemon.DeviceInfo{ID: id}) {
		return daemon.DeviceInfo{}, notReservableEmulator(id)
	}
	d, listed := s.Registry().Lookup(id)
	if !listed {
		return daemon.DeviceInfo{}, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is not listed on hub %s", id, s.HubName())
	}
	if daemon.CallerFrom(r.Context()).Remote && !s.sharedInfo(d) {
		return daemon.DeviceInfo{}, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", id)
	}
	if d.Provider != "" {
		return daemon.DeviceInfo{}, daemon.Errorf(daemon.CodeUsage, "reservations: %s is a cloud device, which belongs to the lease that launched it; reserve a hub device", id).
			WithDetails("field", "device")
	}
	if isEmulator(d) {
		return daemon.DeviceInfo{}, notReservableEmulator(id)
	}
	return d, nil
}

// isEmulator reports whether d is an Android emulator, running or shut
// down: an emulator's serial names whichever AVD runs on that port when a
// reservation starts, so no reservation takes one, by id or by selector.
func isEmulator(d daemon.DeviceInfo) bool {
	return strings.HasPrefix(d.ID, daemon.AVDPrefix) || daemon.IsEmulatorSerial(d.ID) || strings.EqualFold(d.Kind, "emulator")
}

func notReservableEmulator(id string) *daemon.Error {
	return daemon.Errorf(daemon.CodeUsage, "reservations: %s is an Android emulator, whose serial names whichever AVD runs on that port; "+
		"reserve a phone, tablet, TV or simulator", id).WithDetails("field", "device")
}

// reserveDeviceLocked reserves listed device d from start, with
// Server.leaseMu held: a window of ttlSec seconds (0: the default, capped
// at the device's maximum), refused USAGE over the maximum and
// DEVICE_IN_USE when another lease's window overlaps it. Whether the
// device is attached or booting now does not matter: that is before the
// window.
func (s *Server) reserveDeviceLocked(d daemon.DeviceInfo, holder string, start time.Time, ttlSec int) (*leaseEntry, *daemon.Error) {
	maxTTL := s.leaseMaxInfo(d)
	window, we := leaseWindow(d.ID, ttlSec, maxTTL)
	if we != nil {
		return nil, we
	}
	e, ge := s.leases.grant(d.ID, "", holder, start, window, maxTTL, false)
	if ge == nil {
		e.sharedAtGrant.Store(s.sharedInfo(d))
	}
	return e, ge
}

// reserveSelect reserves the first device that matches sel and is free
// for the window, by the order leaseSelect uses (selectorMatches): ready
// now (the hub boots nothing for a reservation), not an emulator or a
// cloud device, and not reserved for a boot or a teardown. For a later
// start, whether it is leased or attached now does not matter: a lease
// that ends before the start leaves it free. A start of zero means now:
// the device must be available now too, and is leased at once, as
// POST /v1/leases leases it.
func (s *Server) reserveSelect(r *http.Request, sel daemon.LeaseSelector, holder string, start time.Time, ttlSec int) (*leaseEntry, *daemon.Error) {
	notFound := daemon.Errorf(daemon.CodeDeviceNotFound, "no device matches the selector %s", sel.String()).WithDetails("select", sel)
	if sel.Hub != "" && !strings.EqualFold(sel.Hub, s.HubName()) {
		return nil, notFound
	}
	matches, e := s.selectorMatches(r, sel)
	if e != nil {
		return nil, e
	}
	if len(matches) == 0 {
		return nil, notFound
	}
	var tooLong *daemon.Error
	ids := make([]string, 0, len(matches))
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	for _, d := range matches {
		ids = append(ids, d.ID)
		if !d.Ready || isEmulator(d) {
			continue
		}
		s.leases.mu.Lock()
		reserved := s.leases.reservedLocked(d)
		s.leases.mu.Unlock()
		if reserved || (start.IsZero() && !d.Available) {
			continue
		}
		var e *leaseEntry
		var le *daemon.Error
		if start.IsZero() {
			e, le = s.leaseDeviceLocked(d, holder, ttlSec, claimNone)
		} else {
			e, le = s.reserveDeviceLocked(d, holder, start, ttlSec)
		}
		if le == nil {
			return e, nil
		}
		if le.Code == daemon.CodeUsage {
			tooLong = le
		}
	}
	if tooLong != nil {
		return nil, tooLong
	}
	return nil, daemon.Errorf(daemon.CodeDeviceInUse, "every device matching %s is reserved, leased past the start, or not ready", sel.String()).
		WithDetails("candidates", ids)
}
