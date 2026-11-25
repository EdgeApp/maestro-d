// Package hubd implements the lease and sharing policy of a maestro-d hub.
package hubd

import (
	"context"
	"net/http"
	"sync"

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

	shareMu   sync.Mutex
	sharedIDs map[string]bool
	shareSeen map[string]shareAnswer
	shareGen  int
}

func (s *Server) Bind(host *daemon.Server, cfg daemon.Config, deps daemon.Deps) {
	s.Server, s.cfg, s.deps = host, cfg, deps
	s.leases = newLeaseTable()
	s.sharedIDs = make(map[string]bool)
	s.shareSeen = make(map[string]shareAnswer)
}

func (s *Server) Start(ctx context.Context) {
	s.loadReservations()
	share := s.shareList()
	if share == "" {
		share = "nothing (list devices in config.json's hubDevices)"
	}
	logger.Info("hub %q: leases on (max window %v%s); sharing %s; remote runShell %t", s.HubName(), s.hubLeaseMax(), leaseMaxNote(s.cfg.LeaseMaxDevices), share, s.cfg.AllowRemoteShell)
	go s.runLeaseSweeper(ctx)
}

func (s *Server) Routes(mux *http.ServeMux) {
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
