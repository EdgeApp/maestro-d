package daemon

// cloud.go: the cloud providers a daemon launches devices on (Deps.Providers),
// how their errors map to error codes, and the cloud section of the catalog
// (GET /v1/catalog?location=cloud).

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// providerSet holds the providers a daemon has constructed.
type providerSet struct {
	mu   sync.Mutex
	made map[string]cloudprov.RemoteProvider
}

// providerNames lists the providers this daemon is configured with, sorted.
func (s *Server) providerNames() []string {
	names := make([]string, 0, len(s.deps.Providers))
	for n := range s.deps.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// provider returns the named provider, constructing it on first use. A
// provider this daemon is not configured with, or one whose construction
// fails (no credentials), is USAGE.
func (s *Server) provider(name string) (cloudprov.RemoteProvider, *Error) {
	mk := s.deps.Providers[name]
	if mk == nil {
		if s.cfg.Hub != nil {
			return nil, Errorf(CodeUsage, "hub %q has no %s credentials (start it with --%s)", s.HubName(), name, name)
		}
		return nil, Errorf(CodeUsage, "cloud provider %q is not supported", name)
	}
	s.cloud.mu.Lock()
	defer s.cloud.mu.Unlock()
	if p := s.cloud.made[name]; p != nil {
		return p, nil
	}
	p, err := mk()
	if err != nil {
		return nil, cloudError(err)
	}
	if s.cloud.made == nil {
		s.cloud.made = map[string]cloudprov.RemoteProvider{}
	}
	s.cloud.made[name] = p
	return p, nil
}

// madeProviders lists the providers constructed so far, by name.
func (s *Server) madeProviders() []string {
	s.cloud.mu.Lock()
	defer s.cloud.mu.Unlock()
	out := make([]string, 0, len(s.cloud.made))
	for n := range s.cloud.made {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// cloudStatus is the cloud entry of GET /v1/status and GET /v1/hub: on a hub
// every provider it was started with, on a plain daemon the ones it has
// used.
func (s *Server) cloudStatus() []CloudStatus {
	names := s.madeProviders()
	if s.cfg.Hub != nil {
		names = s.providerNames()
	}
	out := make([]CloudStatus, 0, len(names))
	for _, n := range names {
		st := CloudStatus{Provider: n, Held: s.CloudHeld(n)}
		if s.cfg.Hub != nil {
			st.MaxDevices = s.cfg.CloudMaxDevices
		}
		out = append(out, st)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CloudHeld counts the devices of provider this daemon holds.
func (s *Server) CloudHeld(provider string) int {
	n := 0
	for _, d := range s.registry.Held() {
		if d.Provider == provider {
			n++
		}
	}
	return n
}

// cloudError maps a provider failure to an error code: credentials and
// denied actions are USAGE, capacity and busy models BUSY, no matching
// model DEVICE_NOT_FOUND, and the rest DEVICE_ERROR. The message is the
// provider's, already redacted.
func cloudError(err error) *Error {
	if err == nil {
		return nil
	}
	var ce *cloudprov.Error
	if !errors.As(err, &ce) {
		// A constructor that fails before any call: credentials or
		// configuration.
		return Errorf(CodeUsage, "%s", err.Error())
	}
	var e *Error
	switch ce.Kind {
	case cloudprov.Auth:
		e = Errorf(CodeUsage, "%s", ce.Error())
		if ce.Action != "" {
			e = e.WithDetails("action", ce.Action)
		}
	case cloudprov.NoCapacity, cloudprov.DeviceBusy:
		e = Errorf(CodeBusy, "%s", ce.Error())
	case cloudprov.NotFound:
		e = Errorf(CodeDeviceNotFound, "%s", ce.Error())
	default:
		e = Errorf(CodeDeviceError, "%s", ce.Error())
	}
	return e
}

// cloudCatalog answers the cloud section of the catalog: each asked-for
// provider's models that pass q.
func (s *Server) cloudCatalog(ctx context.Context, q DeviceQuery) ([]CloudCatalog, *Error) {
	names := s.providerNames()
	if q.Provider != "" {
		if !slices.Contains(names, q.Provider) {
			_, e := s.provider(q.Provider)
			return nil, e
		}
		names = []string{q.Provider}
	}
	if len(names) == 0 {
		_, e := s.provider("devicefarm")
		return nil, e
	}
	hub := ""
	if s.cfg.Hub != nil {
		hub = s.HubName()
	}
	out := make([]CloudCatalog, 0, len(names))
	for _, n := range names {
		p, e := s.provider(n)
		if e != nil {
			return nil, e
		}
		models, err := p.Catalog(ctx, q.Platform)
		if err != nil {
			return nil, cloudError(err)
		}
		cat := CloudCatalog{Provider: n, Hub: hub, Models: []cloudprov.CloudModel{}}
		for _, m := range models {
			if q.MatchModel(m.Platform, m.Name, m.ID, m.OSVersion, m.Available) {
				cat.Models = append(cat.Models, m)
			}
		}
		out = append(out, cat)
	}
	return out, nil
}
