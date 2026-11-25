package daemon

import (
	"context"
	"net/http"
)

// Hub supplies the device-lease policy and routes of a hub. New binds it to
// one device server. A plain daemon leaves Config.Hub nil.
type Hub interface {
	Bind(*Server, Config, Deps)
	Start(context.Context)
	Routes(*http.ServeMux)
	Gate(LeaseMode, http.HandlerFunc) http.HandlerFunc
	Visible(*http.Request, []DeviceInfo) []DeviceInfo
	EventsHandler(http.ResponseWriter, *http.Request)
	CheckBoot(*http.Request, BootRequest) *Error
	Attach(context.Context, string, *AttachConfig) (*DeviceInfo, *Error)
	DeviceStopped(context.Context, bool, ...string)
	OnEvent(Event)
	Shutdown(context.Context, bool)
}

// Events returns the server's stream of device and lifecycle events.
func (s *Server) Events() *EventBus { return s.events }

// HoldIdle keeps the daemon alive until its caller releases the hold.
func (s *Server) HoldIdle() func() {
	s.idle.Hold()
	return s.idle.Release
}

// ShuttingDown reports whether the server has begun draining devices.
func (s *Server) ShuttingDown() bool { return s.shuttingDown.Load() }

func (s *Server) deviceStopped(ctx context.Context, deleted bool, ids ...string) {
	if hub := s.cfg.Hub; hub != nil {
		hub.DeviceStopped(ctx, deleted, ids...)
	}
}
