package daemon

import "net/http"

// Hub requests use the hub's lease and sharing policy. The local daemon's
// listeners serve only callers on this computer.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if hub := s.cfg.Hub; hub != nil {
		hub.EventsHandler(w, r)
		return
	}
	s.events.ServeSSE(w, r)
}

func (s *Server) visible(r *http.Request, devs []DeviceInfo) []DeviceInfo {
	if hub := s.cfg.Hub; hub != nil {
		return hub.Visible(r, devs)
	}
	out := make([]DeviceInfo, len(devs))
	copy(out, devs)
	machine := MachineName()
	for i := range out {
		out[i].Machine = machine
	}
	return out
}

func (s *Server) checkShareBoot(r *http.Request, req BootRequest) *Error {
	if hub := s.cfg.Hub; hub != nil {
		return hub.CheckBoot(r, req)
	}
	return nil
}

func (s *Server) dev(mode LeaseMode, next http.HandlerFunc) http.HandlerFunc {
	if hub := s.cfg.Hub; hub != nil {
		return hub.Gate(mode, next)
	}
	return next
}
