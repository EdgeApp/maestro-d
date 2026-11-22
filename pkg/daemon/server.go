package daemon

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/executor"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Config configures a daemon process.
type Config struct {
	Name string
	// KeepRunDir names run directories the housekeeping sweep never removes
	// whole (a daemon with an installed launchd service); nil keeps none.
	KeepRunDir func(name string) bool
	// HTTPAddr optionally opens a TCP listener (127.0.0.1:port).
	HTTPAddr string
	// Token, when set, is required as `Authorization: Bearer` on TCP. A
	// listener beyond loopback, and any hub, refuses to start without one.
	Token string
	// TLSCert and TLSKey make the TCP listener serve HTTPS.
	TLSCert, TLSKey string
	// Hub makes every TCP caller remote, loopback included: only the unix
	// socket is local. (`hub start` sets it.)
	Hub bool
	// HubName names a hub (default: the short host name).
	HubName string
	// ShareDevices, on a hub, limits the devices remote callers see and
	// may lease to these ids or names; empty shares every device but TVs.
	ShareDevices []string
	// ShareTVs, on a hub, names the Apple TVs it shares, by id or name. A
	// TV pairs with several Macs, so it is shared only when named, and
	// naming it on one hub keeps one owner for it. TVs are left out of
	// ShareDevices either way.
	ShareTVs []string
	// LeaseMax, on a hub, is the longest lease window (default 8 hours);
	// LeaseMaxDevices sets it per device, by id or name.
	LeaseMax        time.Duration
	LeaseMaxDevices []LeaseMax
	// CloudMaxDevices, on a hub, is how many cloud devices it holds at
	// most (--cloud-max-devices).
	CloudMaxDevices int
	// Advertise, on a hub, is the address peers are told to dial; device
	// listings carry it in addresses.
	Advertise string
	// AllowRemoteShell lets remote callers run runShell.
	AllowRemoteShell bool
	// PassEnv names the variables of the daemon's own environment that
	// sessions import when the daemon does not import its environment
	// (a hub, or a listener beyond loopback).
	PassEnv []string
	// IdleTimeout shuts the daemon down after this much inactivity; 0
	// disables it.
	IdleTimeout time.Duration
	// ReportsDir is the root for per-device report directories.
	ReportsDir string
	// NetworkConfig is the traffic-shaping config (default
	// <home>/network.json).
	NetworkConfig string
	// BlobsDir is the blob store's root (default <home>/blobs).
	BlobsDir string
	// StartArgs are recorded in daemon.json for `ps`.
	StartArgs []string
	// Ready, when set, is written a line to once the listeners are up (the
	// spawner watches startup.log for it); a hub whose mesh refused it
	// writes a FAILED line instead.
	Ready io.Writer
	// Mesh, on a hub, links it with its peers (pkg/hub): its router fronts
	// both listeners, and READY waits for its start.
	Mesh Mesh
}

// Server is the daemon: registry + attached devices + HTTP handlers.
type Server struct {
	cfg  Config
	deps Deps
	// cloud holds the cloud providers constructed so far (cloud.go), and
	// remotes the cloud devices this daemon holds (remote.go).
	cloud    providerSet
	remoteMu sync.Mutex
	remotes  map[string]*remoteDevice
	// cloudLaunching counts the cloud launches under way for leases, under
	// leaseMu, so the hub's cap counts them.
	cloudLaunching int
	// launches is cancelled when the daemon shuts down: every cloud launch
	// joins it, so none registers a device after the teardown ran.
	launches       context.Context
	cancelLaunches context.CancelFunc
	registry       *Registry
	events         *EventBus
	idle           *idleTimer
	idem           *idemCache
	blobs          *BlobStore
	// leases are a hub's device leases; leaseMu serialises granting and
	// ending them.
	leases  *leaseTable
	leaseMu sync.Mutex
	// teardowns counts the lease teardowns not finished yet, the
	// sweeper's and those waiting for work under their lease included:
	// Shutdown waits for them before it detaches and shuts down devices.
	teardowns pending
	// sharedIDs remembers the stable device ids found in the
	// --share-device pool; shareSeen, other answers for a short while
	// (shared). shareGen counts forgetShared calls.
	shareMu   sync.Mutex
	sharedIDs map[string]bool
	shareSeen map[string]shareAnswer
	shareGen  int
	// idemGrace: how long a dropped TCP request waits for its retry.
	idemGrace time.Duration

	mu       sync.Mutex
	attached map[string]*attached
	// reportUsers counts attached sessions per report directory, so detach
	// keeps a shared outputDir's blobs/ and bundles/ while another session
	// in it is attached. Guarded by reportMu.
	reportMu    sync.Mutex
	reportUsers map[string]*reportUse
	// attaching serialises concurrent attach calls for the same device.
	attaching map[string]chan struct{}

	startedAt    time.Time
	shuttingDown atomic.Bool
	done         chan struct{}
	shutdownOnce sync.Once
	listeners    []net.Listener
	servers      []*http.Server
	info         Info
}

// New creates a server. Call Run to serve.
func New(cfg Config, deps Deps) *Server {
	if cfg.Name == "" {
		cfg.Name = DefaultName
	}
	if cfg.ReportsDir == "" {
		cfg.ReportsDir = ReportsDir(cfg.Name)
	}
	if cfg.BlobsDir == "" {
		cfg.BlobsDir = DefaultBlobRoot()
	}
	launches, cancelLaunches := context.WithCancel(context.Background())
	s := &Server{
		launches:       launches,
		cancelLaunches: cancelLaunches,
		cfg:            cfg,
		deps:           deps,
		registry:       NewRegistry(cfg.Name, deps.ListDevices),
		events:         NewEventBus(0),
		idem:           newIdemCache(),
		blobs:          NewBlobStore(cfg.BlobsDir),
		leases:         newLeaseTable(),
		sharedIDs:      map[string]bool{},
		shareSeen:      map[string]shareAnswer{},
		idemGrace:      idemGrace,
		attached:       map[string]*attached{},
		attaching:      map[string]chan struct{}{},
		done:           make(chan struct{}),
	}
	s.registry.deps = deps
	s.events.observe = s.forgetShared
	if cfg.Hub {
		s.info.Hub = s.hubName()
	}
	s.idle = newIdleTimer(cfg.IdleTimeout, func() {
		logger.Info("Idle for %v, shutting down", cfg.IdleTimeout)
		s.Shutdown("idle")
	})
	return s
}

// Registry exposes the device registry (tests, wiring).
func (s *Server) Registry() *Registry { return s.registry }

// Handler returns the HTTP handler (for tests and the unix socket).
func (s *Server) Handler() http.Handler { return s.listenerHandler(false) }

// TCPHandler returns the handler the TCP listener serves: bearer-token
// checked, and with remote callers told apart from local ones.
func (s *Server) TCPHandler() http.Handler { return s.listenerHandler(true) }

// listenerHandler is what a listener serves: the daemon's routes, behind
// the mesh's router on a hub that has one.
func (s *Server) listenerHandler(tcp bool) http.Handler {
	if s.cfg.Mesh != nil {
		return s.cfg.Mesh.Handler(s.routes(tcp), tcp)
	}
	return s.routes(tcp)
}

// checkListen refuses a TCP listener that would let anyone in: a hub, or a
// --http address beyond loopback, needs a token; TLS needs both files.
func (s *Server) checkListen() *Error {
	if s.cfg.HTTPAddr == "" {
		if s.cfg.TLSCert != "" || s.cfg.TLSKey != "" {
			return Errorf(CodeUsage, "--tls-cert/--tls-key need a TCP listener (--http)")
		}
		return nil
	}
	if s.cfg.Token == "" && (s.cfg.Hub || !isLoopbackListen(s.cfg.HTTPAddr)) {
		return Errorf(CodeUsage, "listening on %s, beyond loopback, needs --token (MAESTRO_D_TOKEN)", s.cfg.HTTPAddr)
	}
	if (s.cfg.TLSCert == "") != (s.cfg.TLSKey == "") {
		return Errorf(CodeUsage, "--tls-cert and --tls-key go together")
	}
	return nil
}

// Run listens on the unix socket (and TCP when configured), writes
// daemon.json, and blocks until the daemon shuts down. Signals SIGINT and
// SIGTERM trigger a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	if err := ValidateName(s.cfg.Name); err != nil {
		return err
	}
	if e := s.checkListen(); e != nil {
		return e
	}
	var tlsConfig *tls.Config
	if s.cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
		if err != nil {
			return Errorf(CodeUsage, "load --tls-cert/--tls-key: %v", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	if err := os.MkdirAll(RunDir(s.cfg.Name), 0o700); err != nil {
		return err
	}
	// The run-dir lock, held until Run returns, is what makes this the one
	// daemon of its name: daemon.json alone is not, since a hub writes it
	// only once its mesh has started, seconds after it binds the socket.
	// A second start in that window must not remove the socket.
	lock, err := lockRunDir(LockPath(s.cfg.Name))
	if errors.Is(err, errRunLocked) {
		return Errorf(CodeDaemonUnavailable, "daemon %q is already running or starting (another process holds %s)", s.cfg.Name, LockPath(s.cfg.Name))
	}
	if err != nil {
		return err
	}
	defer unlockRunDir(lock)
	if live, _ := LiveInfo(s.cfg.Name); live != nil {
		return Errorf(CodeDaemonUnavailable, "daemon %q is already running (pid %d)", s.cfg.Name, live.PID)
	}
	if err := os.MkdirAll(s.cfg.ReportsDir, 0o755); err != nil {
		return err
	}
	// The housekeeping sweep (old blobs, idle reports, dead daemons' run
	// dirs, orphaned Chrome profiles) runs now and hourly while this
	// daemon runs.
	hkCtx, stopHousekeeping := context.WithCancel(context.Background())
	defer stopHousekeeping()
	go s.housekeep(hkCtx)
	sockPath := SocketPath(s.cfg.Name)
	_ = os.Remove(sockPath)
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600)
	s.listeners = append(s.listeners, unixLn)
	s.servers = append(s.servers, &http.Server{Handler: s.listenerHandler(false)})

	httpAddr := ""
	if s.cfg.HTTPAddr != "" {
		addr := s.cfg.HTTPAddr
		// `serve --http :port` means loopback; a hub's --listen :port binds
		// every interface.
		if strings.HasPrefix(addr, ":") && !s.cfg.Hub {
			addr = "127.0.0.1" + addr
		}
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			_ = unixLn.Close()
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		httpAddr = tcpLn.Addr().String()
		if tlsConfig != nil {
			tcpLn = tls.NewListener(tcpLn, tlsConfig)
		}
		s.listeners = append(s.listeners, tcpLn)
		s.servers = append(s.servers, &http.Server{Handler: s.listenerHandler(true)})
	}

	s.startedAt = time.Now()
	s.info = Info{
		Name:        s.cfg.Name,
		PID:         os.Getpid(),
		APIVersion:  APIVersion,
		Version:     s.deps.Version,
		SocketPath:  sockPath,
		HTTPAddr:    httpAddr,
		StartedAt:   s.startedAt.Format(time.RFC3339),
		StartArgs:   s.cfg.StartArgs,
		RunDir:      RunDir(s.cfg.Name),
		ReportsDir:  s.cfg.ReportsDir,
		IdleTimeout: int(s.cfg.IdleTimeout / time.Second),
		TLS:         tlsConfig != nil,
	}
	if s.cfg.Hub {
		s.info.Hub = s.hubName()
	}
	// A hub with a mesh writes daemon.json once its mesh has started:
	// spawners (hub start, the launchd service's wait) treat daemon.json
	// plus a ping as ready, so they return only after the launch browse
	// and the first --peer answers, as READY does.
	if s.cfg.Mesh == nil {
		if err := WriteInfo(&s.info); err != nil {
			return err
		}
	}
	// Fresh registry file for this run.
	s.registry.mu.Lock()
	s.registry.persistLocked()
	s.registry.mu.Unlock()

	errCh := make(chan error, len(s.servers))
	for i, srv := range s.servers {
		go func(srv *http.Server, ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv, s.listeners[i])
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	logger.Info("maestro-d %q ready: socket=%s http=%s tls=%t idle=%v", s.cfg.Name, sockPath, httpAddr, tlsConfig != nil, s.cfg.IdleTimeout)
	if httpAddr != "" && !s.importsEnv() {
		logger.Info("sessions import no environment from this daemon (pass-env: %s)", strings.Join(s.cfg.PassEnv, ","))
	}
	if s.cfg.Hub {
		share := "every device but TVs"
		if len(s.cfg.ShareDevices) > 0 {
			share = strings.Join(s.cfg.ShareDevices, ", ")
		}
		if len(s.cfg.ShareTVs) > 0 {
			share += "; TVs " + strings.Join(s.cfg.ShareTVs, ", ")
		}
		logger.Info("hub %q: leases on (max window %v%s); sharing %s; remote runShell %t", s.info.Hub, s.hubLeaseMax(),
			leaseMaxNote(s.cfg.LeaseMaxDevices), share, s.cfg.AllowRemoteShell)
		sweepCtx, stopSweep := context.WithCancel(ctx)
		defer stopSweep()
		go s.runLeaseSweeper(sweepCtx)
	}
	if s.cfg.Mesh != nil {
		if err := s.cfg.Mesh.Start(ctx, s.info); err != nil {
			e := asError(err)
			logger.Error("hub %q: %s", s.info.Hub, e.Message)
			if s.cfg.Ready != nil {
				fmt.Fprintf(s.cfg.Ready, "FAILED code=%s %s\n", e.Code, e.Message)
			}
			s.Shutdown("mesh refused: " + e.Message)
			<-s.done
			return e
		}
		if err := WriteInfo(&s.info); err != nil {
			s.Shutdown("write daemon.json: " + err.Error())
			<-s.done
			return err
		}
	}
	if s.cfg.Ready != nil {
		fmt.Fprintf(s.cfg.Ready, "READY socket=%s http=%s pid=%d\n", sockPath, httpAddr, os.Getpid())
	}
	// Cloud sessions an earlier run of this daemon left (a crash, kill -9,
	// a launchd restart) are released once it serves, so a spawning client
	// never waits on it.
	go s.selfRelease()

	select {
	case <-s.done:
		return nil
	case err := <-errCh:
		s.Shutdown("listener error: " + err.Error())
		return err
	case sig := <-sigCh:
		logger.Info("Received %v, shutting down", sig)
		s.Shutdown("signal " + sig.String())
		return nil
	case <-ctx.Done():
		s.Shutdown("context cancelled")
		return nil
	}
}

// Done is closed when the daemon has finished shutting down.
func (s *Server) Done() <-chan struct{} { return s.done }

// Shutdown detaches every device, shuts down daemon-booted devices,
// removes the run files and closes the listeners. Idempotent.
func (s *Server) Shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		s.shuttingDown.Store(true)
		if s.cancelLaunches != nil {
			// Cloud launches under way stop, and release what they made.
			s.cancelLaunches()
		}
		s.idle.Stop()
		go func() {
			defer close(s.done)
			// A hub's leases end first, while the mesh still carries their
			// events to peers' clients and before daemon.shutdown: requests
			// under them are cancelled, and every teardown (one that waits
			// for work that ignores its cancellation too, for a while) is
			// done before the devices are detached and shut down below.
			s.endAllLeases()
			if s.cfg.Mesh != nil {
				s.cfg.Mesh.Close()
			}
			s.events.Publish(Event{Type: "daemon.shutdown", Message: reason})
			s.detachAll()
			s.releaseAllRemote("daemon-exit")
			s.registry.ShutdownOwned()
			RemoveRunFiles(s.cfg.Name)
			// Close listeners after cleanup so a `stop` caller can still
			// read its response; give in-flight responses a moment.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for _, srv := range s.servers {
				_ = srv.Shutdown(ctx)
			}
			logger.Info("maestro-d %q stopped (%s)", s.cfg.Name, reason)
		}()
	})
}

func (s *Server) detachAll() {
	s.mu.Lock()
	all := make([]*attached, 0, len(s.attached))
	for _, a := range s.attached {
		all = append(all, a)
	}
	s.attached = map[string]*attached{}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range all {
		wg.Add(1)
		go func(a *attached) {
			defer wg.Done()
			a.close()
			s.registry.MarkDetached(a.id)
			s.events.Publish(Event{Type: "device.detached", Device: a.id})
		}(a)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Routing

func (s *Server) routes(tcp bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /v1/catalog", s.handleCatalog)
	mux.HandleFunc("GET /v1/providers/{provider}/sessions", s.handleProviderSessions)
	mux.HandleFunc("GET /v1/providers/{provider}/account", s.handleProviderAccount)
	mux.HandleFunc("GET /v1/providers/{provider}/apps", s.handleProviderApps)
	mux.HandleFunc("POST /v1/providers/{provider}/apps", s.handleProviderUpload)
	mux.HandleFunc("GET /v1/viewer", s.handleViewer)
	mux.HandleFunc("GET /v1/devices", s.handleListDevices)
	mux.HandleFunc("POST /v1/devices", s.handleBootDevice)
	mux.HandleFunc("POST /v1/leases", s.idempotent(s.handleAcquireLease))
	mux.HandleFunc("GET /v1/leases", s.handleListLeases)
	mux.HandleFunc("GET /v1/leases/{lid}", s.handleGetLease)
	mux.HandleFunc("POST /v1/leases/{lid}/renew", s.handleRenewLease)
	mux.HandleFunc("DELETE /v1/leases/{lid}", s.handleReleaseLease)
	// Every device route goes through dev: a remote caller sees only the
	// shared devices, and on a hub the request carries the device's lease.
	open, leased := func(h http.HandlerFunc) http.HandlerFunc { return s.dev(leaseOpen, h) },
		func(h http.HandlerFunc) http.HandlerFunc { return s.dev(leaseNeeded, h) }
	// busy counts a request on a cloud device's idle clock, for the routes
	// that do not take the step lock (acquire does that for the rest).
	busy := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			done, e := s.cloudBusy(r.PathValue("id"))
			if e != nil {
				writeError(w, e)
				return
			}
			defer done()
			h(w, r)
		}
	}
	mux.HandleFunc("DELETE /v1/devices/{id}", s.dev(leaseIfHeld, s.handleStopDevice))
	mux.HandleFunc("POST /v1/devices/{id}/attach", leased(busy(s.handleAttach)))
	mux.HandleFunc("POST /v1/devices/{id}/detach", leased(s.handleDetach))
	mux.HandleFunc("GET /v1/devices/{id}", open(s.handleGetDevice))
	mux.HandleFunc("POST /v1/devices/{id}/commands/{name}", leased(s.idempotent(s.handleCommand)))
	mux.HandleFunc("POST /v1/devices/{id}/steps", leased(s.idempotent(s.handleSteps)))
	mux.HandleFunc("POST /v1/devices/{id}/flows", leased(s.idempotent(s.handleFlow)))
	mux.HandleFunc("GET /v1/devices/{id}/screenshot", leased(s.handleScreenshot))
	mux.HandleFunc("GET /v1/devices/{id}/stream", leased(s.handleStream))
	mux.HandleFunc("GET /v1/devices/{id}/hierarchy", leased(s.handleHierarchy))
	mux.HandleFunc("GET /v1/devices/{id}/state", leased(s.handleState))
	// info reads cached data, and any caller may ask: it does not hold a
	// cloud device's idle clock.
	mux.HandleFunc("GET /v1/devices/{id}/info", open(s.handleInfo))
	mux.HandleFunc("GET /v1/devices/{id}/vars", leased(busy(s.handleGetVars)))
	mux.HandleFunc("PUT /v1/devices/{id}/vars", leased(busy(s.handleSetVars)))
	mux.HandleFunc("POST /v1/devices/{id}/vars", leased(busy(s.handleSetVars)))
	mux.HandleFunc("POST /v1/devices/{id}/eval", leased(s.handleEval))
	mux.HandleFunc("HEAD /v1/devices/{id}/blobs/{sha256}", leased(busy(s.handleBlobHead)))
	mux.HandleFunc("PUT /v1/devices/{id}/blobs/{sha256}", leased(busy(s.handleBlobPut)))
	mux.HandleFunc("POST /v1/devices/{id}/apps", leased(busy(s.idempotent(s.handleInstallApp))))
	mux.HandleFunc("DELETE /v1/devices/{id}/apps/{appId}", leased(s.handleUninstallApp))
	mux.HandleFunc("GET /v1/devices/{id}/artifacts", leased(s.handleListArtifacts))
	mux.HandleFunc("GET /v1/devices/{id}/artifacts/{path...}", leased(s.handleGetArtifact))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, Errorf(CodeUsage, "no route for %s %s", r.Method, r.URL.Path))
	})
	return s.middleware(mux, tcp)
}

// BearerAuthorized reports whether an Authorization header is let through
// a TCP listener whose bearer token is token: every header when token is
// empty (a listener without a token, which checkListen allows only on a
// loopback address of a plain daemon), else exactly "Bearer <token>",
// compared in constant time. The daemon and the hub mesh's router both
// check with it.
func BearerAuthorized(header, token string) bool {
	return token == "" || subtle.ConstantTimeCompare([]byte(header), []byte("Bearer "+token)) == 1
}

func (s *Server) middleware(next http.Handler, tcp bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The viewer page carries no device data; it reads its token from
		// its URL fragment and sends it with the stream request.
		if tcp && !IsViewerRequest(r) && !BearerAuthorized(r.Header.Get("Authorization"), s.cfg.Token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErrorStatus(w, http.StatusUnauthorized, Errorf(CodeUsage, "missing or invalid bearer token"))
			return
		}
		if s.shuttingDown.Load() && r.URL.Path != "/v1/status" {
			writeError(w, Errorf(CodeDaemonUnavailable, "daemon %q is shutting down", s.cfg.Name))
			return
		}
		if v := r.Header.Get("X-Maestro-Api-Version"); v != "" && v != APIVersion {
			writeError(w, Errorf(CodeDaemonUnavailable, "API version mismatch: daemon %s, client %s", APIVersion, v))
			return
		}
		s.idle.Touch()
		w.Header().Set("X-Maestro-Api-Version", APIVersion)
		clientOutputs := r.Header.Get(HeaderClientOutputs) != ""
		if tcp {
			r = r.WithContext(withCaller(r.Context(), Caller{Remote: s.remoteTCP(r.RemoteAddr), Addr: r.RemoteAddr, ClientOutputs: clientOutputs}))
		} else if clientOutputs {
			r = r.WithContext(withCaller(r.Context(), Caller{ClientOutputs: true}))
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// JSON helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, e *Error) {
	writeErrorStatus(w, e.HTTPStatus(), e)
}

func writeErrorStatus(w http.ResponseWriter, status int, e *Error) {
	w.Header().Set(HeaderErrorCode, string(e.Code))
	writeJSON(w, status, FailureEnvelope(e))
}

func decodeBody(r *http.Request, v any) *Error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return WrapErr(CodeUsage, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return Errorf(CodeUsage, "invalid JSON body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Daemon handlers

func (s *Server) statusResult() StatusResult {
	// The devices this daemon holds, from its records: no live discovery,
	// which shells out to adb, simctl and emulator and can take seconds,
	// while clients use status as their liveness ping.
	res := StatusResult{Envelope: Envelope{OK: true}, Daemon: s.info, Devices: s.registry.Held(), Cloud: s.cloudStatus()}
	if dl := s.idle.Deadline(); !dl.IsZero() {
		res.IdleShutdownAt = dl.Format(time.RFC3339)
	}
	if res.Devices == nil {
		res.Devices = []DeviceInfo{}
	}
	return res
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	res := s.statusResult()
	res.Devices = s.visible(r, res.Devices)
	writeJSON(w, http.StatusOK, res)
}

// handleShutdown serves POST /v1/shutdown. On a hub only the unix socket
// may stop the daemon (`maestro-d hub stop` uses it): a TCP caller, which
// is remote there, and a request a peer forwarded would end every other
// client's leases and take the hub off the mesh.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Hub && CallerFrom(r.Context()).Remote {
		e := forbidden("hub %q is stopped from its own machine (maestro-d hub stop), not over TCP", s.hubName())
		s.logForbidden(r, e)
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, Envelope{OK: true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.Shutdown("shutdown request")
}

// ---------------------------------------------------------------------------
// Device handlers

// handleListDevices serves GET /v1/devices: every device the caller may
// see, each with its location relative to the caller's machine, filtered by
// the query (filter.go).
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	q, e := ParseDeviceQuery(r.URL.Query())
	if e != nil {
		writeError(w, e)
		return
	}
	devs := s.visible(r, s.registry.List(q.Platform))
	if !s.cfg.Hub {
		annotateAvailable(devs, otherOwners(s.cfg.Name))
	}
	SetLocations(devs, s.callerMachine(r))
	s.decorateCloud(devs)
	devs = q.Filter(devs)
	s.markTrafficShaping(devs)
	writeJSON(w, http.StatusOK, DevicesResult{Envelope: Envelope{OK: true}, Devices: devs})
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.registry.Lookup(id)
	if !ok {
		writeError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
		return
	}
	devs := []DeviceInfo{info}
	if s.cfg.Hub {
		s.annotate(devs)
	} else {
		devs[0].Machine = MachineName()
	}
	SetLocations(devs, s.callerMachine(r))
	s.decorateCloud(devs)
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: s.withTrafficShaping(devs[0])})
}

func (s *Server) handleBootDevice(w http.ResponseWriter, r *http.Request) {
	var req BootRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	// A cloud create goes to remote.go before the share and simulator
	// checks, so a hub always answers it with the lease hint.
	if req.Create != nil && req.Create.Cloud != nil {
		s.handleCreateCloud(w, r, req)
		return
	}
	if e := s.checkShareBoot(r, req); e != nil {
		writeError(w, e)
		return
	}
	if req.Create != nil {
		s.handleCreateDevice(w, r, req)
		return
	}
	s.idle.Hold()
	defer s.idle.Release()
	info, err := s.registry.Boot(r.Context(), req)
	if err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	s.events.Publish(Event{Type: "device.booted", Device: info.ID, Message: req.Name})
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: info})
}

func (s *Server) handleStopDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req StopDeviceRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		req.Force = true
	}
	if r.URL.Query().Get("delete") == "true" {
		req.Delete = true
	}
	if rd := s.remote(id); rd != nil {
		s.handleStopCloud(w, r, rd, req.Delete)
		return
	}
	if req.Delete {
		s.handleDeleteDevice(w, r, id, req.Force)
		return
	}
	// Detach first if we hold it.
	s.detachForStop(id)
	if err := s.registry.Stop(id, req.Force); err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	// On a hub, stopping the device ends its lease.
	s.endStoppedLease(r.Context(), endStopped, id)
	s.events.Publish(Event{Type: "device.stopped", Device: id})
	writeJSON(w, http.StatusOK, Envelope{OK: true})
}

// detachForStop closes the driver on a device about to be stopped or
// deleted, if this daemon holds one.
func (s *Server) detachForStop(id string) {
	if a := s.takeAttached(id); a != nil {
		defer s.finishDetach(a) // even if the teardown panics
		a.close()
		s.registry.MarkDetached(id)
		s.events.Publish(Event{Type: "device.detached", Device: id})
	}
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var cfg AttachConfig
	if e := decodeBody(r, &cfg); e != nil {
		writeError(w, e)
		return
	}
	if CallerFrom(r.Context()).Remote {
		if e := checkRemoteAttach(&cfg); e != nil {
			s.logForbidden(r, e)
			writeError(w, e)
			return
		}
	}
	a, e := s.attachLeased(r.Context(), id, &cfg)
	if e != nil {
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: s.withTrafficShaping(s.deviceInfo(a))})
}

func (s *Server) handleDetach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a := s.takeAttached(id)
	if a == nil {
		writeError(w, Errorf(CodeDeviceNotAttached, "device %s is not attached", id))
		return
	}
	defer s.finishDetach(a) // even if the teardown panics
	a.close()
	s.registry.MarkDetached(id)
	s.events.Publish(Event{Type: "device.detached", Device: id})
	writeJSON(w, http.StatusOK, Envelope{OK: true})
}

func (s *Server) deviceInfo(a *attached) DeviceInfo {
	if info, ok := s.registry.Lookup(a.id); ok {
		return info
	}
	return a.info
}

func (s *Server) getAttached(id string) *attached {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attached[id]
}

// takeAttached removes a device's attachment for its caller to close, and
// keeps the id reserved until finishDetach: an attach of the same device
// waits for the teardown, whose cleanup (a reset, airplane mode off) would
// otherwise reach the device after the new session had set it up.
func (s *Server) takeAttached(id string) *attached {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeAttachedLocked(id)
}

// takeAttachedLocked is takeAttached with s.mu held.
func (s *Server) takeAttachedLocked(id string) *attached {
	a := s.attached[id]
	delete(s.attached, id)
	if a != nil && s.attaching[id] == nil {
		a.closing = make(chan struct{})
		s.attaching[id] = a.closing
	}
	return a
}

// finishDetach releases the id takeAttached reserved.
func (s *Server) finishDetach(a *attached) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.closing != nil && s.attaching[a.id] == a.closing {
		delete(s.attaching, a.id)
		close(a.closing)
	}
}

// ensureAttached returns the attached device, opening it when cfg is
// given and it isn't attached yet. Concurrent attaches for one device
// wait for the first.
func (s *Server) ensureAttached(ctx context.Context, id string, cfg *AttachConfig) (*attached, *Error) {
	for {
		s.mu.Lock()
		if a := s.attached[id]; a != nil {
			s.mu.Unlock()
			return a, nil
		}
		if ch := s.attaching[id]; ch != nil {
			s.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, Errorf(CodeInterrupted, "attach %s cancelled", id)
			}
		}
		if cfg == nil {
			s.mu.Unlock()
			return nil, Errorf(CodeDeviceNotAttached, "device %s is not attached", id).
				WithDetails("attached", s.attachedIDs())
		}
		ch := make(chan struct{})
		s.attaching[id] = ch
		s.mu.Unlock()

		// A cloud device's install restarts the driver an attach adopts:
		// the attach waits for an install under way, and is registered
		// before the next install looks for it.
		unlockInstall := func() {}
		if rd := s.remote(id); rd != nil {
			rd.installMu.Lock()
			unlockInstall = rd.installMu.Unlock
		}
		s.idle.Hold()
		a, e := s.attachDevice(ctx, id, *cfg)
		s.idle.Release()

		s.mu.Lock()
		delete(s.attaching, id)
		close(ch)
		if e == nil {
			if s.shuttingDown.Load() {
				s.mu.Unlock()
				unlockInstall()
				a.close()
				return nil, Errorf(CodeDaemonUnavailable, "daemon is shutting down")
			}
			s.attached[id] = a
		}
		s.mu.Unlock()
		unlockInstall()
		if e == nil {
			// maestro-d (idledetach.go): watched only once stored, so a
			// detach the watchers start finds the attachment.
			s.watchIdle(a, a.session.Driver())
			s.watchConnection(a, a.session.Driver())
		}
		return a, e
	}
}

func (s *Server) attachedIDs() []string {
	ids := make([]string, 0, len(s.attached))
	for id := range s.attached {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// requireAttached resolves {id} to an attached device or writes the error.
func (s *Server) requireAttached(w http.ResponseWriter, r *http.Request) *attached {
	id := r.PathValue("id")
	a := s.getAttached(id)
	if a == nil {
		if e := s.cloudGone(id); e != nil {
			writeError(w, e)
			return nil
		}
		s.mu.Lock()
		ids := s.attachedIDs()
		s.mu.Unlock()
		writeError(w, Errorf(CodeDeviceNotAttached, "device %s is not attached", id).WithDetails("attached", ids))
		return nil
	}
	return a
}

// acquire takes the device's busy lock, honouring ?wait=false.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request, a *attached) bool {
	if r.URL.Query().Get("wait") == "false" {
		// A cloud device's keepalive holds the lock for one ping: that is
		// not a step in flight, so wait it out.
		if !a.busy.TryLock() && !(s.isPinging(a.id) && lockWithin(&a.busy, 5*time.Second)) {
			writeError(w, Errorf(CodeBusy, "device %s has a step in flight", a.id))
			return false
		}
	} else {
		a.busy.Lock()
	}
	// A cloud device counts the request on its idle clock (remote.go).
	if rd := s.remote(a.id); rd != nil {
		if e := rd.begin(); e != nil {
			a.busy.Unlock()
			writeError(w, e)
			return false
		}
	}
	atomic.AddInt32(&a.inFlite, 1)
	s.registry.SetBusy(a.id, true)
	s.idle.Hold()
	a.touch()
	return true
}

func (s *Server) release(a *attached) {
	a.touch()
	s.idle.Release()
	if atomic.AddInt32(&a.inFlite, -1) == 0 {
		s.registry.SetBusy(a.id, false)
	}
	if rd := s.remote(a.id); rd != nil {
		rd.end()
	}
	a.busy.Unlock()
}

// lockWithin takes mu, giving up after d.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !mu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// sessionAnswers reports whether rd's WebDriver session still answers (the
// keepalive's cheap command). A failed step asks the provider whether the
// session ended only when it does not, so ordinary failures (an assertion,
// an element not found) cost no provider call.
func (s *Server) sessionAnswers(rd *remoteDevice) bool {
	rd.driverMu.Lock()
	defer rd.driverMu.Unlock()
	return rd.driver.Keepalive() == nil
}

// cloudResult adds a cloud device's warnings to a step result, answers a
// recording step with CAPTURE_UNAVAILABLE, and turns a failure on a device
// whose provider session has ended into DEVICE_ERROR with the reason.
func (s *Server) cloudResult(res *StepResult) {
	rd := s.remote(res.Device)
	if rd == nil {
		return
	}
	res.Warnings = append(res.Warnings, s.cloudWarnings(res.Device)...)
	if res.OK || res.Error == nil || res.Error.Code == CodeInterrupted || res.Error.Code == CodeForbidden {
		return
	}
	// The held driver has no screen capture and refuses recording steps
	// itself (remoteAppium); answer them as the stream does.
	if res.Type == string(flow.StepStartRecording) || res.Type == string(flow.StepStopRecording) {
		e := unavailableErr(&capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform,
			Message: fmt.Sprintf("cloud device %s has no screen capture, so it cannot record (the provider keeps its own session video)", res.Device)}, "")
		res.Error = &ErrorBody{Code: e.Code, Message: e.Message, Step: res.Error.Step, Details: e.Details}
		res.Message = e.Message
		res.ExitCode = CodeCaptureUnavailable.ExitCode()
		return
	}
	if s.sessionAnswers(rd) || !s.checkRemote(rd) {
		return
	}
	rd.mu.Lock()
	reason := rd.ended
	rd.mu.Unlock()
	res.Error = &ErrorBody{Code: CodeDeviceError, Message: fmt.Sprintf("cloud device %s is gone: %s", rd.id, reason), Step: res.Error.Step,
		Details: map[string]any{"reason": reason}}
	res.ExitCode = CodeDeviceError.ExitCode()
}

// ---------------------------------------------------------------------------
// Step handlers

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	name := r.PathValue("name")
	if !flow.IsStepType(name) {
		writeError(w, Errorf(CodeUsage, "unknown command %q", name))
		return
	}
	var value any
	if e := decodeBody(r, &value); e != nil {
		writeError(w, e)
		return
	}
	cwd := r.URL.Query().Get("cwd")
	if cwd != "" && CallerFrom(r.Context()).Remote {
		e := forbidden("?cwd= names a directory on the daemon's host; a remote caller may not send it").WithDetails("field", "cwd")
		s.logForbidden(r, e)
		writeError(w, e)
		return
	}
	src := "request"
	if cwd != "" {
		src = filepath.Join(cwd, "request.yaml")
	}
	step, err := buildStep(name, value, src)
	if err != nil {
		writeError(w, WrapErr(CodeUsage, err))
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	ctx, cancel := s.runCtx(r, a)
	defer cancel()
	out := a.session.Execute(ctx, step)
	res := ResultFromOutcome(a.id, a.reportDir, out)
	s.cloudResult(&res)
	if c := CallerFrom(r.Context()); c.Remote || c.ClientOutputs {
		res.ForRemote(a.reportDir)
	}
	status := http.StatusOK
	if !res.OK {
		status = s.stepStatus(r, &Error{ErrorBody: *res.Error})
	}
	writeJSON(w, status, res)
}

func (s *Server) handleSteps(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	var req StepsRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if req.Cwd != "" && CallerFrom(r.Context()).Remote {
		e := forbidden("cwd names a directory on the daemon's host; a remote caller may not send it").WithDetails("field", "cwd")
		s.logForbidden(r, e)
		writeError(w, e)
		return
	}
	var steps []flow.Step
	var e *Error
	if req.Bundle != nil {
		if len(req.Steps) > 0 || req.YAML != "" {
			writeError(w, Errorf(CodeUsage, "steps: send bundle, steps or yaml, not more than one"))
			return
		}
		if e = req.Bundle.Validate(); e != nil {
			s.writeBundleError(w, r, e)
			return
		}
	} else if steps, e = resolveSteps(req); e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	bundleDir, flowDir := "", ""
	if req.Bundle != nil {
		dir, root, e := s.bundleCall(a, req.Bundle)
		if e != nil {
			s.writeBundleError(w, r, e)
			return
		}
		f, err := flow.ParseFile(root)
		if err != nil {
			writeError(w, WrapErr(CodeUsage, err))
			return
		}
		// The root file's own directory, not the bundle's: its relative
		// runFlow, runScript and addMedia resolve as beside the file.
		steps, bundleDir, flowDir = f.Steps, dir, filepath.Dir(root)
	}
	ctx, cancel := s.runCtxBundle(r, a, bundleDir, flowDir)
	defer cancel()
	outs := a.session.ExecuteAll(ctx, steps, req.ContinueOnError)
	results := make([]StepResult, 0, len(outs))
	for _, o := range outs {
		res := ResultFromOutcome(a.id, a.reportDir, o)
		s.cloudResult(&res)
		if c := CallerFrom(r.Context()); c.Remote || c.ClientOutputs {
			res.ForRemote(a.reportDir)
		}
		results = append(results, res)
	}
	res := StepsResultFrom(a.id, results)
	status := http.StatusOK
	if !res.OK {
		status = s.stepStatus(r, &Error{ErrorBody: *res.Error})
	}
	writeJSON(w, status, res)
}

func (s *Server) handleFlow(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	var req FlowRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if CallerFrom(r.Context()).Remote {
		var e *Error
		switch {
		case req.Cwd != "":
			e = forbidden("cwd names a directory on the daemon's host; a remote caller may not send it").WithDetails("field", "cwd")
		case req.File != "":
			e = forbidden("file %s is a path on the daemon's host; a remote caller sends the flow itself (bundle or yaml)", req.File).WithDetails("field", "file")
		}
		if e != nil {
			s.logForbidden(r, e)
			writeError(w, e)
			return
		}
	}
	var f *flow.Flow
	var err error
	if req.Bundle != nil && (req.File != "" || req.YAML != "") {
		writeError(w, Errorf(CodeUsage, "flows: send bundle, file or yaml, not more than one"))
		return
	}
	bundleDir, flowDir := "", ""
	switch {
	case req.Bundle != nil:
		if e := req.Bundle.Validate(); e != nil {
			s.writeBundleError(w, r, e)
			return
		}
		if !s.acquire(w, r, a) {
			return
		}
		defer s.release(a)
		dir, root, e := s.bundleCall(a, req.Bundle)
		if e != nil {
			s.writeBundleError(w, r, e)
			return
		}
		// For the whole call, hooks included: upstream sets flowDir only
		// inside executeSubFlow, so onFlowStart and onFlowComplete would
		// otherwise resolve against the daemon's working directory.
		bundleDir, flowDir = dir, filepath.Dir(root)
		f, err = flow.ParseFile(root)
	case req.File != "":
		path := req.File
		if !filepath.IsAbs(path) && req.Cwd != "" {
			path = filepath.Join(req.Cwd, path)
		}
		f, err = flow.ParseFile(path)
	case req.YAML != "":
		src := "inline.yaml"
		if req.Cwd != "" {
			src = filepath.Join(req.Cwd, src)
		}
		f, err = flow.Parse([]byte(req.YAML), src)
	default:
		writeError(w, Errorf(CodeUsage, "flows: file or yaml is required"))
		return
	}
	if err != nil {
		writeError(w, WrapErr(CodeUsage, err))
		return
	}
	if len(req.Env) > 0 {
		if f.Config.Env == nil {
			f.Config.Env = map[string]string{}
		}
		for k, v := range req.Env {
			f.Config.Env[k] = v
		}
	}
	if bundleDir == "" {
		if !s.acquire(w, r, a) {
			return
		}
		defer s.release(a)
	}
	ctx, cancel := s.runCtxBundle(r, a, bundleDir, flowDir)
	defer cancel()
	out := a.session.RunFlow(ctx, *f)
	res := ResultFromOutcome(a.id, a.reportDir, out)
	s.cloudResult(&res)
	if c := CallerFrom(r.Context()); c.Remote || c.ClientOutputs {
		res.ForRemote(a.reportDir)
	}
	status := http.StatusOK
	if !res.OK {
		status = s.stepStatus(r, &Error{ErrorBody: *res.Error})
	}
	writeJSON(w, status, res)
}

// runCtx is the context a step runs under: cancelled when the client goes
// away or the daemon shuts down.
func (s *Server) runCtx(r *http.Request, a *attached) (context.Context, context.CancelFunc) {
	return s.runCtxBundle(r, a, "", "")
}

// runCtxBundle is runCtx for a call that runs from a materialized bundle:
// the bundle is one of the policy's roots, and flowDir is the script
// engine's flow directory for the call.
func (s *Server) runCtxBundle(r *http.Request, a *attached, bundleDir, flowDir string) (context.Context, context.CancelFunc) {
	p := s.callerPolicy(r.Context(), a)
	p.BundleDir, p.FlowDir = bundleDir, flowDir
	ctx, cancel := context.WithCancel(executor.WithCallerPolicy(r.Context(), p))
	stop := context.AfterFunc(a.baseCtx(), cancel)
	return ctx, func() { stop(); cancel() }
}

// stepStatus is the HTTP status of a step result, logging a policy denial.
func (s *Server) stepStatus(r *http.Request, res *Error) int {
	if res == nil {
		return http.StatusOK
	}
	if res.Code == CodeForbidden {
		s.logForbidden(r, res)
	}
	return res.Code.HTTPStatus()
}

// writeBundleError answers a bad bundle, logging a FORBIDDEN one.
func (s *Server) writeBundleError(w http.ResponseWriter, r *http.Request, e *Error) {
	if e.Code == CodeForbidden {
		s.logForbidden(r, e)
	}
	writeError(w, e)
}

func (s *Server) logForbidden(r *http.Request, e *Error) {
	logger.Info("FORBIDDEN %s %s from %s: %s", r.Method, r.URL.Path, describeCaller(CallerFrom(r.Context())), e.Message)
}

// ---------------------------------------------------------------------------
// Inspection handlers

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	opts, e := parseShotOptions(r.URL.Query()) // maestro-d: screenshot.go
	if e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	sh, e := takeShot(r.Context(), a.session, opts)
	if e != nil {
		writeError(w, e)
		return
	}
	if r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, DataResult{Envelope: Envelope{OK: true}, Data: base64.StdEncoding.EncodeToString(sh.data)})
		return
	}
	w.Header().Set("Content-Type", sh.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(sh.data)))
	if sh.described {
		w.Header().Set("X-Maestro-Screen", fmt.Sprintf("%d,%d", sh.screen.X, sh.screen.Y))
		w.Header().Set("X-Maestro-Rect", fmt.Sprintf("%d,%d,%d,%d", sh.rect.Min.X, sh.rect.Min.Y, sh.rect.Dx(), sh.rect.Dy()))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sh.data)
}

func (s *Server) handleHierarchy(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	data, err := a.session.Hierarchy(r.Context())
	if err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	if r.URL.Query().Get("format") == "raw" {
		ct := "application/xml"
		if len(data) > 0 && (data[0] == '{' || data[0] == '[') {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	var parsed any
	if s.deps.NormalizeHierarchy != nil {
		if tree, err := s.deps.NormalizeHierarchy(data); err == nil {
			parsed = tree
		}
	}
	if parsed == nil {
		if err := json.Unmarshal(data, &parsed); err != nil {
			// XML (Android) or other text: return it as a string.
			parsed = string(data)
		}
	}
	writeJSON(w, http.StatusOK, DataResult{Envelope: Envelope{OK: true}, Data: parsed})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	writeJSON(w, http.StatusOK, DataResult{Envelope: Envelope{OK: true}, Data: a.session.State(r.Context())})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, DataResult{Envelope: Envelope{OK: true}, Data: a.session.PlatformInfo()})
}

func (s *Server) handleGetVars(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	writeJSON(w, http.StatusOK, VarsResult{Envelope: Envelope{OK: true}, Vars: a.session.Vars()})
}

func (s *Server) handleSetVars(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	// Accept {"vars": {...}} (the shape GET returns) as well as a bare map.
	var body map[string]json.RawMessage
	if e := decodeBody(r, &body); e != nil {
		writeError(w, e)
		return
	}
	vars := map[string]string{}
	if raw, ok := body["vars"]; ok && len(body) == 1 && len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &vars); err != nil {
			writeError(w, Errorf(CodeUsage, "invalid vars: %v", err))
			return
		}
	} else {
		for k, raw := range body {
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				writeError(w, Errorf(CodeUsage, "variable %q must be a string", k))
				return
			}
			vars[k] = v
		}
	}
	for k, v := range vars {
		a.session.SetVar(k, v)
	}
	writeJSON(w, http.StatusOK, VarsResult{Envelope: Envelope{OK: true}, Vars: a.session.Vars()})
}

func (s *Server) handleEval(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	var req EvalRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if strings.TrimSpace(req.Script) == "" {
		writeError(w, Errorf(CodeUsage, "eval: script is required"))
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	v, err := a.session.Eval(req.Script)
	if err != nil {
		e := Errorf(CodeCommandFailed, "%v", err)
		e.Step = "eval"
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, EvalResult{Envelope: Envelope{OK: true}, Value: jsonSafe(v)})
}

// jsonSafe converts goja exports that encoding/json can't handle (e.g.
// functions) into strings.
func jsonSafe(v any) any {
	if _, err := json.Marshal(v); err != nil {
		return fmt.Sprint(v)
	}
	return v
}
