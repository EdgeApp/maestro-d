package daemon

import (
	"context"
	"net/http"

	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// Hub supplies the device-lease policy and routes of a hub. New binds it to
// one device server. A plain daemon leaves Config.Hub nil.
type Hub interface {
	Bind(*Server, Config, Deps)
	DeviceConfig() *netshape.Config
	Start(context.Context) error
	Routes(*http.ServeMux)
	TCPCaller(addr string, clientOutputs bool) Caller
	Gate(LeaseMode, http.HandlerFunc) http.HandlerFunc
	Visible(*http.Request, []DeviceInfo) []DeviceInfo
	EventsHandler(http.ResponseWriter, *http.Request)
	CheckBoot(*http.Request, BootRequest) *Error
	VerifyBinding(context.Context, string) *Error
	Attach(context.Context, string, *AttachConfig) (*DeviceInfo, *Error)
	DeviceStopped(context.Context, bool, ...string) *Error
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

// verifyBinding repeats the owning hub's identity check immediately before
// using a device, after queued work, request bodies, and uploads have settled.
func (s *Server) verifyBinding(ctx context.Context, device string) *Error {
	if s.cfg.Hub != nil {
		return s.cfg.Hub.VerifyBinding(ctx, device)
	}
	return nil
}

func (s *Server) deviceStopped(ctx context.Context, deleted bool, ids ...string) *Error {
	if hub := s.cfg.Hub; hub != nil {
		return hub.DeviceStopped(ctx, deleted, ids...)
	}
	return nil
}
