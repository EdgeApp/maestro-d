// Package hubd implements the lease and sharing policy of a maestro-d hub.
package hubd

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

type Server struct {
	*daemon.Server
	cfg  daemon.Config
	deps daemon.Deps

	leases         *leaseTable
	leaseMu        sync.Mutex
	reservationsMu sync.Mutex
	teardowns      pending
	cloudLaunching int

	configEdits   sync.Mutex              // serializes socket mutations, including publication
	removing      map[string]bool         // protected by leaseMu; includes verified aliases
	removedLeases map[string]removedLease // protected by leaseMu
	config        *configCache
	shareMu       sync.Mutex
	sharedIDs     map[string]bool
	shareSeen     map[string]shareAnswer
	shareGen      int
}

func (s *Server) Bind(host *daemon.Server, cfg daemon.Config, deps daemon.Deps) {
	s.Server, s.cfg, s.deps = host, cfg, deps
	s.config = newConfigCache(daemon.DeviceConfigPath(cfg.DeviceConfig), s.forgetShares, s.configChanged)
	s.leases = newLeaseTable()
	s.removing = make(map[string]bool)
	s.removedLeases = make(map[string]removedLease)
	s.sharedIDs = make(map[string]bool)
	s.shareSeen = make(map[string]shareAnswer)
}

func (s *Server) Start(ctx context.Context) error {
	if _, err := s.config.snapshot(time.Now(), true); err != nil {
		return err
	}
	s.loadReservations()
	share := s.shareList()
	if share == "" {
		share = "nothing (list devices in config.json's hubDevices)"
	}
	logger.Info("hub %q: leases on (max window %v%s); sharing %s; remote runShell %t", s.HubName(), s.hubLeaseMax(), leaseMaxNote(s.cfg.LeaseMaxDevices), share, s.cfg.AllowRemoteShell)
	go s.runLeaseSweeper(ctx)
	return nil
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/config/devices", s.localConfig(s.getConfigDevices))
	mux.HandleFunc("PUT /v1/config/devices/{id}", s.localConfig(s.putConfigDevice))
	mux.HandleFunc("DELETE /v1/config/devices/{id}", s.localConfig(s.deleteConfigDevice))
	mux.HandleFunc("POST /v1/leases", s.Idempotent(s.handleAcquireLease))
	mux.HandleFunc("POST /v1/reservations", s.Idempotent(s.handleReserve))
	mux.HandleFunc("GET /v1/leases", s.handleListLeases)
	mux.HandleFunc("GET /v1/leases/{lid}", s.handleGetLease)
	mux.HandleFunc("POST /v1/leases/{lid}/renew", s.handleRenewLease)
	mux.HandleFunc("DELETE /v1/leases/{lid}", s.handleReleaseLease)
}

func (s *Server) Shutdown(ctx context.Context, endLeases bool) {
	s.endAllLeases()
}

func (s *Server) DeviceStopped(ctx context.Context, deleted bool, ids ...string) {
	reason := endStopped
	if deleted {
		reason = endDeleted
	}
	s.endStoppedLease(ctx, reason, ids...)
}
