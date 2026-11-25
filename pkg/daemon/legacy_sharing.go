package daemon

// The single 3.x binary also supports TCP on a plain daemon. These checks
// preserve that listener's sharing policy during the binary split.

import (
	"net/http"
	"strings"
	"time"
)

// shareMatches reports whether a --share-device entry names the device, by
// id or by name (a running emulator's name is its AVD name).
func shareMatches(entry string, d DeviceInfo) bool {
	return entry == d.ID || (d.Name != "" && strings.EqualFold(entry, d.Name)) ||
		(strings.HasPrefix(d.ID, AVDPrefix) && strings.EqualFold(entry, strings.TrimPrefix(d.ID, AVDPrefix)))
}

// sharedInfo applies the plain daemon's optional share list to a listed device.
func (s *Server) sharedInfo(d DeviceInfo) bool {
	if IsComputer(d) {
		return false // local only, whatever --share-device says
	}
	if s.sharesAll() {
		return true
	}
	return namedIn(s.cfg.ShareDevices, d) || s.configShares(d)
}

// sharesAll reports whether remote callers see every device: on a daemon
// that is not a hub and names none. A hub shares only what it names.
func (s *Server) sharesAll() bool {
	return len(s.cfg.ShareDevices) == 0
}

// configShares reports whether config.json's hubDevices names device d
// (see configNames).
func (s *Server) configShares(d DeviceInfo) bool {
	for _, id := range s.cfg.ShareConfig {
		if ConfigNames(id, d) {
			return true
		}
	}
	return false
}

// namedIn reports whether an entry of list names device d.
func namedIn(list []string, d DeviceInfo) bool {
	for _, entry := range list {
		if shareMatches(entry, d) {
			return true
		}
	}
	return false
}

// shareList describes what a hub shares, for its log and refusals: "" when
// it names nothing.
func (s *Server) shareList() string {
	var parts []string
	if len(s.cfg.ShareDevices) > 0 {
		parts = append(parts, strings.Join(s.cfg.ShareDevices, ", "))
	}
	if len(s.cfg.ShareTVs) > 0 {
		parts = append(parts, "TVs "+strings.Join(s.cfg.ShareTVs, ", "))
	}
	if len(s.cfg.ShareConfig) > 0 {
		parts = append(parts, "config.json "+strings.Join(s.cfg.ShareConfig, ", "))
	}
	return strings.Join(parts, "; ")
}

// shareCacheFor is how long shared remembers an answer it may not keep:
// an emulator serial's (emulator-5554 is whichever AVD booted on that port
// last) and a negative one. A device booted, stopped, created or deleted
// here, or a lease ending, forgets them at once.
const shareCacheFor = 30 * time.Second

type shareAnswer struct {
	ok bool
	at time.Time
}

// shared reports whether device id is in the hub's pool. An id that is not
// listed (a device discovery cannot see) is shared only when it is named
// itself. Positive answers are remembered for stable ids, whose device's
// name never changes; an emulator serial's answer, and any negative one,
// for shareCacheFor. So a run of steps, and the event stream that reports
// them to a remote caller, look the device up once, not per request and
// per event: a lookup runs live discovery (adb, emulator, simctl).
func (s *Server) shared(id string) bool {
	// The computer device is never in the pool. Checked first: the empty
	// share-all answer below and the positive cache would share it.
	if id == ComputerID {
		return false
	}
	if rec, ok := s.registry.Record(id); ok && IsComputer(rec) {
		return false
	}

	if s.sharesAll() {
		return true
	}
	stable := !IsEmulatorSerial(id)
	now := time.Now()
	s.shareMu.Lock()
	if stable && s.sharedIDs[id] {
		s.shareMu.Unlock()
		return true
	}
	if a, ok := s.shareSeen[id]; ok && now.Sub(a.at) < shareCacheFor && !now.Before(a.at) {
		s.shareMu.Unlock()
		return a.ok
	}
	gen := s.shareGen
	s.shareMu.Unlock()
	info, found := s.registry.Lookup(id)
	if !found {
		info = DeviceInfo{ID: id}
	}
	ok := s.sharedInfo(info)
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	switch {
	case gen != s.shareGen:
		// Forgotten while looking: this answer may predate the change.
	case ok && stable:
		s.sharedIDs[id] = true
	default:
		s.shareSeen[id] = shareAnswer{ok: ok, at: now}
	}
	return ok
}

// forgetShared drops the answers shared may not keep, when a device's
// state changes here: a booted, stopped, created or deleted device, or an
// ended lease (its device may be stopped, and another AVD boot on the
// serial).
func (s *Server) forgetShared(ev Event) {
	switch {
	case ev.Type == "device.booted", ev.Type == "device.stopped", ev.Type == "device.created", ev.Type == "device.deleted",
		strings.HasPrefix(ev.Type, "lease.") && ev.Type != "lease.granted":
		s.shareMu.Lock()
		s.shareGen++
		clear(s.shareSeen)
		s.shareMu.Unlock()
	}
}

// sharedWith reports whether the request's caller may see device id: a
// remote caller sees only the shared pool.
func (s *Server) sharedWith(r *http.Request, id string) bool {
	return !CallerFrom(r.Context()).Remote || s.shared(id)
}

// handleEvents serves GET /v1/events. A remote caller sees only the shared
// pool's events: a ?device= naming another device is not found, other
// devices' events are left out, and so is any event that names no device
// but the daemon's shutdown. A device's attach event carries the host's
// report directory, which a remote caller does not get either.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if hub := s.cfg.Hub; hub != nil {
		hub.EventsHandler(w, r)
		return
	}
	if !CallerFrom(r.Context()).Remote {
		s.events.ServeSSE(w, r)
		return
	}
	if id := r.URL.Query().Get("device"); id != "" && !s.shared(id) {
		WriteError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
		return
	}
	s.events.ServeSSEView(w, r, s.remoteEvent)
}

// remoteEvent is a remote caller's view of an event.
func (s *Server) remoteEvent(ev Event) (Event, bool) {
	switch {
	case ev.Device == "":
		return ev, ev.Type == "daemon.shutdown"
	case !s.shared(ev.Device):
		return ev, false
	case ev.Type == "device.attached":
		ev.Message = ""
	}
	return ev, true
}

// visible filters and annotates a device list for the request's caller:
// a remote caller sees only the shared pool, and on a hub every entry
// carries its lease state.
func (s *Server) visible(r *http.Request, devs []DeviceInfo) []DeviceInfo {
	if hub := s.cfg.Hub; hub != nil {
		return hub.Visible(r, devs)
	}
	out := make([]DeviceInfo, 0, len(devs))
	remote := CallerFrom(r.Context()).Remote
	for _, d := range devs {
		if remote && !s.sharedInfo(d) {
			continue
		}
		out = append(out, d)
	}
	machine := MachineName()
	for i := range out {
		out[i].Machine = machine
	}
	return out
}

// stopTarget is the device DELETE /v1/devices/{id} acts on, for its share
// and lease checks: `device delete` takes a simulator or AVD by name
// (Registry.Delete resolves it), so a name is checked as the device it
// names, never as an id nobody leases. An id that names no device, or
// several, is checked as given; the handler reports it. The device comes
// back as listed, so its reservations are checked by its AVD too.
func (s *Server) stopTarget(id string) DeviceInfo {
	if info, e := s.registry.ResolveManaged(id); e == nil {
		return info
	}
	return DeviceInfo{ID: id}
}

// checkShareBoot refuses a remote caller's boot or create of a device the
// hub does not share: booting an unshared one is DEVICE_NOT_FOUND, and a
// device may be created only under a name the hub shares.
func (s *Server) checkShareBoot(r *http.Request, req BootRequest) *Error {
	if hub := s.cfg.Hub; hub != nil {
		return hub.CheckBoot(r, req)
	}
	if s.sharesAll() || !CallerFrom(r.Context()).Remote {
		return nil
	}
	if req.Create != nil {
		name := req.Create.Name
		if name == "" {
			name = req.Name
		}
		if !s.sharedInfo(DeviceInfo{ID: name, Name: name, Platform: strings.ToLower(req.Platform)}) {
			shares := s.shareList()
			if shares == "" {
				shares = "nothing"
			}
			return Forbidden("this hub shares only %s; a remote caller cannot create %q", shares, name).
				WithDetails("field", "name")
		}
		return nil
	}
	// A boot without a platform takes the one Boot infers, so a bare AVD
	// name is android; a simulator's, or one naming both, is not.
	platform := strings.ToLower(req.Platform)
	if platform == "" && len(s.cfg.ShareConfig) > 0 {
		platform, _ = s.registry.InferBootPlatform(req.Name)
	}
	if s.sharedInfo(DeviceInfo{ID: req.Name, Name: strings.TrimPrefix(req.Name, AVDPrefix), Platform: platform}) || s.shared(req.Name) {
		return nil
	}
	return Errorf(CodeDeviceNotFound, "device %s not found", req.Name)
}

func (s *Server) dev(mode LeaseMode, next http.HandlerFunc) http.HandlerFunc {
	if hub := s.cfg.Hub; hub != nil {
		return hub.Gate(mode, next)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if mode == LeaseIfHeld && CallerFrom(r.Context()).Remote {
			id = s.stopTarget(id).ID
		}
		if !s.sharedWith(r, id) {
			WriteError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
			return
		}
		next(w, r)
	}
}
