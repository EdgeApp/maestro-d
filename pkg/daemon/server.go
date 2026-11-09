package daemon

import (
	"context"
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

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Config configures a daemon process.
type Config struct {
	Name string
	// HTTPAddr optionally opens a TCP listener (127.0.0.1:port).
	HTTPAddr string
	// Token, when set, is required as `Authorization: Bearer` on TCP.
	Token string
	// IdleTimeout shuts the daemon down after this much inactivity; 0
	// disables it.
	IdleTimeout time.Duration
	// ReportsDir is the root for per-device report directories.
	ReportsDir string
	// StartArgs are recorded in daemon.json for `ps`.
	StartArgs []string
	// Ready, when set, is written a line to once the listeners are up (the
	// spawner watches startup.log for it).
	Ready io.Writer
}

// Server is the daemon: registry + attached devices + HTTP handlers.
type Server struct {
	cfg      Config
	deps     Deps
	registry *Registry
	events   *EventBus
	idle     *idleTimer

	mu       sync.Mutex
	attached map[string]*attached
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
	s := &Server{
		cfg:       cfg,
		deps:      deps,
		registry:  NewRegistry(cfg.Name, deps.ListDevices),
		events:    NewEventBus(0),
		attached:  map[string]*attached{},
		attaching: map[string]chan struct{}{},
		done:      make(chan struct{}),
	}
	s.registry.deps = deps
	s.idle = newIdleTimer(cfg.IdleTimeout, func() {
		logger.Info("Idle for %v, shutting down", cfg.IdleTimeout)
		s.Shutdown("idle")
	})
	return s
}

// Registry exposes the device registry (tests, wiring).
func (s *Server) Registry() *Registry { return s.registry }

// Handler returns the HTTP handler (for tests and the unix socket).
func (s *Server) Handler() http.Handler { return s.routes(false) }

// Run listens on the unix socket (and TCP when configured), writes
// daemon.json, and blocks until the daemon shuts down. Signals SIGINT and
// SIGTERM trigger a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	if err := ValidateName(s.cfg.Name); err != nil {
		return err
	}
	if live, _ := LiveInfo(s.cfg.Name); live != nil {
		return Errorf(CodeDaemonUnavailable, "daemon %q is already running (pid %d)", s.cfg.Name, live.PID)
	}
	if err := os.MkdirAll(RunDir(s.cfg.Name), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.ReportsDir, 0o755); err != nil {
		return err
	}
	sockPath := SocketPath(s.cfg.Name)
	_ = os.Remove(sockPath)
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600)
	s.listeners = append(s.listeners, unixLn)
	s.servers = append(s.servers, &http.Server{Handler: s.routes(false)})

	httpAddr := ""
	if s.cfg.HTTPAddr != "" {
		addr := s.cfg.HTTPAddr
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		tcpLn, err := net.Listen("tcp", addr)
		if err != nil {
			_ = unixLn.Close()
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		httpAddr = tcpLn.Addr().String()
		s.listeners = append(s.listeners, tcpLn)
		s.servers = append(s.servers, &http.Server{Handler: s.routes(true)})
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
	}
	if err := WriteInfo(&s.info); err != nil {
		return err
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

	logger.Info("maestro-d %q ready: socket=%s http=%s idle=%v", s.cfg.Name, sockPath, httpAddr, s.cfg.IdleTimeout)
	if s.cfg.Ready != nil {
		fmt.Fprintf(s.cfg.Ready, "READY socket=%s http=%s pid=%d\n", sockPath, httpAddr, os.Getpid())
	}

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
		s.idle.Stop()
		s.events.Publish(Event{Type: "daemon.shutdown", Message: reason})
		go func() {
			defer close(s.done)
			s.detachAll()
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
	mux.HandleFunc("GET /v1/events", s.events.ServeSSE)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /v1/catalog", s.handleCatalog)
	mux.HandleFunc("GET /v1/devices", s.handleListDevices)
	mux.HandleFunc("POST /v1/devices", s.handleBootDevice)
	mux.HandleFunc("DELETE /v1/devices/{id}", s.handleStopDevice)
	mux.HandleFunc("POST /v1/devices/{id}/attach", s.handleAttach)
	mux.HandleFunc("POST /v1/devices/{id}/detach", s.handleDetach)
	mux.HandleFunc("GET /v1/devices/{id}", s.handleGetDevice)
	mux.HandleFunc("POST /v1/devices/{id}/commands/{name}", s.handleCommand)
	mux.HandleFunc("POST /v1/devices/{id}/steps", s.handleSteps)
	mux.HandleFunc("POST /v1/devices/{id}/flows", s.handleFlow)
	mux.HandleFunc("GET /v1/devices/{id}/screenshot", s.handleScreenshot)
	mux.HandleFunc("GET /v1/devices/{id}/hierarchy", s.handleHierarchy)
	mux.HandleFunc("GET /v1/devices/{id}/state", s.handleState)
	mux.HandleFunc("GET /v1/devices/{id}/info", s.handleInfo)
	mux.HandleFunc("GET /v1/devices/{id}/vars", s.handleGetVars)
	mux.HandleFunc("PUT /v1/devices/{id}/vars", s.handleSetVars)
	mux.HandleFunc("POST /v1/devices/{id}/vars", s.handleSetVars)
	mux.HandleFunc("POST /v1/devices/{id}/eval", s.handleEval)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, Errorf(CodeUsage, "no route for %s %s", r.Method, r.URL.Path))
	})
	return s.middleware(mux, tcp)
}

func (s *Server) middleware(next http.Handler, tcp bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tcp && s.cfg.Token != "" {
			if r.Header.Get("Authorization") != "Bearer "+s.cfg.Token {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeErrorStatus(w, http.StatusUnauthorized, Errorf(CodeUsage, "missing or invalid bearer token"))
				return
			}
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
	res := StatusResult{Envelope: Envelope{OK: true}, Daemon: s.info, Devices: s.registry.Held()}
	if dl := s.idle.Deadline(); !dl.IsZero() {
		res.IdleShutdownAt = dl.Format(time.RFC3339)
	}
	if res.Devices == nil {
		res.Devices = []DeviceInfo{}
	}
	return res
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusResult())
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Envelope{OK: true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.Shutdown("shutdown request")
}

// ---------------------------------------------------------------------------
// Device handlers

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs := s.registry.List(strings.ToLower(r.URL.Query().Get("platform")))
	if devs == nil {
		devs = []DeviceInfo{}
	}
	writeJSON(w, http.StatusOK, DevicesResult{Envelope: Envelope{OK: true}, Devices: devs})
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	info, ok := s.registry.Lookup(id)
	if !ok {
		writeError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
		return
	}
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: info})
}

func (s *Server) handleBootDevice(w http.ResponseWriter, r *http.Request) {
	var req BootRequest
	if e := decodeBody(r, &req); e != nil {
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
	if req.Delete {
		s.handleDeleteDevice(w, id, req.Force)
		return
	}
	// Detach first if we hold it.
	s.detachForStop(id)
	if err := s.registry.Stop(id, req.Force); err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	s.events.Publish(Event{Type: "device.stopped", Device: id})
	writeJSON(w, http.StatusOK, Envelope{OK: true})
}

// detachForStop closes the driver on a device about to be stopped or
// deleted, if this daemon holds one.
func (s *Server) detachForStop(id string) {
	if a := s.takeAttached(id); a != nil {
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
	a, e := s.ensureAttached(r.Context(), id, &cfg)
	if e != nil {
		writeError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: s.deviceInfo(a)})
}

func (s *Server) handleDetach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a := s.takeAttached(id)
	if a == nil {
		writeError(w, Errorf(CodeDeviceNotAttached, "device %s is not attached", id))
		return
	}
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

func (s *Server) takeAttached(id string) *attached {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.attached[id]
	delete(s.attached, id)
	return a
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

		s.idle.Hold()
		a, e := s.attachDevice(ctx, id, *cfg)
		s.idle.Release()

		s.mu.Lock()
		delete(s.attaching, id)
		close(ch)
		if e == nil {
			if s.shuttingDown.Load() {
				s.mu.Unlock()
				a.close()
				return nil, Errorf(CodeDaemonUnavailable, "daemon is shutting down")
			}
			s.attached[id] = a
		}
		s.mu.Unlock()
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
		if !a.busy.TryLock() {
			writeError(w, Errorf(CodeBusy, "device %s has a step in flight", a.id))
			return false
		}
	} else {
		a.busy.Lock()
	}
	atomic.AddInt32(&a.inFlite, 1)
	s.registry.SetBusy(a.id, true)
	s.idle.Hold()
	return true
}

func (s *Server) release(a *attached) {
	s.idle.Release()
	if atomic.AddInt32(&a.inFlite, -1) == 0 {
		s.registry.SetBusy(a.id, false)
	}
	a.busy.Unlock()
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
	status := http.StatusOK
	if !res.OK {
		status = res.Error.Code.HTTPStatus()
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
	steps, e := resolveSteps(req)
	if e != nil {
		writeError(w, e)
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	ctx, cancel := s.runCtx(r, a)
	defer cancel()
	outs := a.session.ExecuteAll(ctx, steps, req.ContinueOnError)
	results := make([]StepResult, 0, len(outs))
	for _, o := range outs {
		results = append(results, ResultFromOutcome(a.id, a.reportDir, o))
	}
	res := StepsResultFrom(a.id, results)
	status := http.StatusOK
	if !res.OK {
		status = res.Error.Code.HTTPStatus()
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
	var f *flow.Flow
	var err error
	switch {
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
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	ctx, cancel := s.runCtx(r, a)
	defer cancel()
	out := a.session.RunFlow(ctx, *f)
	res := ResultFromOutcome(a.id, a.reportDir, out)
	status := http.StatusOK
	if !res.OK {
		status = res.Error.Code.HTTPStatus()
	}
	writeJSON(w, status, res)
}

// runCtx is the context a step runs under: cancelled when the client goes
// away or the daemon shuts down.
func (s *Server) runCtx(r *http.Request, a *attached) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(a.baseCtx(), cancel)
	return ctx, func() { stop(); cancel() }
}

// ---------------------------------------------------------------------------
// Inspection handlers

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	if !s.acquire(w, r, a) {
		return
	}
	defer s.release(a)
	png, err := a.session.Screenshot(r.Context())
	if err != nil {
		writeError(w, WrapErr(CodeDeviceError, err))
		return
	}
	if r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusOK, DataResult{Envelope: Envelope{OK: true}, Data: base64.StdEncoding.EncodeToString(png)})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
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
