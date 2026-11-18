package hub

// router.go: the hub's router, in front of the daemon's own handler on
// both listeners.
//
//   - A route with {id} or {lid} is split at its @hub suffix: this hub's
//     own (or an unqualified id of a local device) is served by the
//     daemon's handler with the suffix stripped; any other goes to the
//     owner as one tunnelled request.
//   - GET /v1/devices and GET /v1/leases fan out to every linked peer
//     (scope=local) and merge; a peer that does not answer within 3 s is
//     listed under unreachable. Peers' answers are cached for 2 s.
//   - POST /v1/leases routes by its body's device; a selector is tried on
//     this hub first, then on the peers by RTT.
//   - GET /v1/events?device=<id> routes by the id; without device the
//     peers' streams are merged into one.
//   - POST /v1/devices and GET /v1/catalog take hub in the body or query.
//
// A request a peer forwarded is never forwarded again: the owner serves
// only its own devices.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type fwdKey struct{}

// forwardedFrom names the peer that forwarded the request ("" for a
// client's own request).
func forwardedFrom(ctx context.Context) string {
	s, _ := ctx.Value(fwdKey{}).(string)
	return s
}

// Handler puts the router in front of a listener's handler. The daemon
// calls it once for the unix socket and once for the TCP listener.
func (m *Mesh) Handler(local http.Handler, tcp bool) http.Handler {
	rt := &router{m: m, local: local, tcp: tcp, localSeen: map[string]time.Time{}}
	if tcp {
		m.tcpRouter.Store(rt)
	} else {
		m.unixRouter.Store(rt)
	}
	return rt
}

// serveForwarded serves a request a peer sent over a link, as a remote
// caller on the TCP listener.
func (m *Mesh) serveForwarded(w http.ResponseWriter, req *http.Request, peer string) {
	rt := m.tcpRouter.Load()
	if rt == nil {
		writeError(w, 0, daemon.Errorf(daemon.CodeDaemonUnavailable, "hub %s has no TCP listener", m.cfg.Name))
		return
	}
	if !(req.Method == http.MethodGet && req.URL.Query().Get("scope") == "local") {
		m.logf("forwarded by %s: %s %s for %s", peer, req.Method, req.URL.Path, req.RemoteAddr)
	}
	rt.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), fwdKey{}, peer)))
}

type router struct {
	m     *Mesh
	local http.Handler
	tcp   bool

	// localSeen remembers, for LocalCacheFor, the ids the daemon's own
	// handler listed, so a client naming a device without its hub does not
	// pay live discovery on every step.
	seenMu    sync.Mutex
	localSeen map[string]time.Time
}

func writeError(w http.ResponseWriter, status int, e *daemon.Error) {
	if status == 0 {
		status = e.HTTPStatus()
	}
	w.Header().Set(daemon.HeaderErrorCode, string(e.Code))
	writeJSON(w, status, daemon.FailureEnvelope(e))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// clientAuthorized checks a client's bearer token as the daemon's own
// listener does.
func (m *Mesh) clientAuthorized(r *http.Request) bool {
	return daemon.BearerAuthorized(r.Header.Get("Authorization"), m.cfg.Token)
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := rt.m
	fwd := forwardedFrom(r.Context())
	path := r.URL.Path
	if path == "/v1/hub/link" {
		if !rt.tcp || fwd != "" {
			writeError(w, 0, daemon.Errorf(daemon.CodeUsage, "peer links are made to a hub's TCP listener"))
			return
		}
		m.acceptLink(w, r)
		return
	}
	if rt.tcp && !m.clientAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, daemon.Errorf(daemon.CodeUsage, "missing or invalid bearer token"))
		return
	}
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	wide := fwd == "" && r.URL.Query().Get("scope") != "local"
	switch {
	case path == "/v1/hub" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, daemon.HubStatusResult{Envelope: daemon.Envelope{OK: true}, Hub: m.Status(r.Context())})
	case len(seg) >= 3 && seg[0] == "v1" && seg[1] == "devices" && seg[2] != "":
		rt.device(w, r, seg[2], fwd)
	case len(seg) >= 3 && seg[0] == "v1" && seg[1] == "leases" && seg[2] != "":
		rt.leaseByID(w, r, seg[2], fwd)
	case path == "/v1/leases" && r.Method == http.MethodPost:
		rt.acquire(w, r, fwd)
	case path == "/v1/devices" && r.Method == http.MethodGet && wide:
		rt.listDevices(w, r)
	case path == "/v1/leases" && r.Method == http.MethodGet && wide:
		rt.listLeases(w, r)
	case path == "/v1/events" && r.Method == http.MethodGet:
		rt.events(w, r, fwd, wide)
	case path == "/v1/devices" && r.Method == http.MethodPost:
		m.forgetPeerIDs()
		rt.boot(w, r, fwd)
	case path == "/v1/catalog" && r.Method == http.MethodGet && fwd == "":
		if h := hubName(r.URL.Query().Get("hub")); h != "" && h != m.cfg.Name {
			rt.forward(w, r, h)
			return
		}
		rt.serveLocal(w, r)
	default:
		rt.serveLocal(w, r)
	}
}

// serveLocal runs the daemon's own handler, qualifying its answer's ids.
func (rt *router) serveLocal(w http.ResponseWriter, r *http.Request) {
	qw := &qualifyWriter{w: w, hub: rt.m.cfg.Name, userData: userDataRoute(r)}
	rt.local.ServeHTTP(qw, r)
	qw.finish()
}

// withSeg returns r with path segment i (after /) replaced.
func withSeg(r *http.Request, i int, v string) *http.Request {
	seg := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	seg[i] = v
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + strings.Join(seg, "/")
	r2.URL.RawPath = ""
	return r2
}

// stripLease drops this hub's suffix from the request's lease header.
func (rt *router) stripLease(r *http.Request) {
	if v := r.Header.Get(daemon.HeaderLease); v != "" {
		if local, hub := splitID(v); hub == rt.m.cfg.Name {
			r.Header.Set(daemon.HeaderLease, local)
		}
	}
}

// device routes /v1/devices/{id}/….
func (rt *router) device(w http.ResponseWriter, r *http.Request, id, fwd string) {
	self := rt.m.cfg.Name
	local, hub := splitID(id)
	if hub == "" && fwd == "" {
		target, e := rt.resolve(r, local)
		if e != nil {
			writeError(w, 0, e)
			return
		}
		hub = target
		if hub != self {
			r = withSeg(r, 2, qualify(local, hub))
		}
	}
	switch {
	case hub == "" || hub == self:
		r = withSeg(r, 2, local)
		rt.stripLease(r)
		rt.serveLocal(w, r)
	case fwd != "":
		writeError(w, 0, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s belongs to hub %s, not %s", id, hub, self))
	default:
		rt.forward(w, r, hub)
	}
}

// resolve finds an unqualified id's hub: this one when it lists the
// device, else the one peer that does; several is USAGE. An id nobody
// lists stays local, where the daemon answers it (a lease on it is
// DEVICE_NOT_FOUND: a hub leases only devices it lists) — but only when
// every known peer answered: if a linked peer did not answer in time,
// or a peer's link is down without its having stopped, the id may be its
// device, so the request fails PEER_UNAVAILABLE (retryable) naming those
// hubs rather than DEVICE_NOT_FOUND.
//
// An id that resolves to one peer is remembered for PeerIDCacheFor, so a
// run of steps on a peer's device by bare id does not run this hub's live
// discovery and the fan-out for every step. The memory is dropped when
// that peer's link is down and when a device is booted or created here.
func (rt *router) resolve(r *http.Request, id string) (string, *daemon.Error) {
	self := rt.m.cfg.Name
	if hub, ok := rt.m.peerIDFor(id); ok {
		return hub, nil
	}
	if rt.localHas(r, id) {
		return self, nil
	}
	var hits []string
	silent := rt.m.downPeers()
	for _, res := range rt.m.fanout(r.Context(), "/v1/devices", "scope=local") {
		var devs daemon.DevicesResult
		if res.err != nil || json.Unmarshal(res.body, &devs) != nil {
			silent = append(silent, res.hub)
			continue
		}
		for _, d := range devs.Devices {
			l, _ := splitID(d.ID)
			if d.LocalID == id || l == id {
				hits = append(hits, d.ID)
			}
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 0:
		if len(silent) > 0 {
			sort.Strings(silent)
			return "", daemon.Errorf(daemon.CodePeerUnavailable, "device %s is not on this hub, and hub %s did not answer; retry, or name the hub (%s@<hub>)",
				id, strings.Join(silent, ", "), id).WithDetails("hubs", silent)
		}
		return self, nil
	case 1:
		_, hub := splitID(hits[0])
		rt.m.rememberPeerID(id, hub)
		return hub, nil
	}
	return "", daemon.Errorf(daemon.CodeUsage, "device %s is on more than one hub; name one: %s", id, strings.Join(hits, ", ")).
		WithDetails("candidates", hits)
}

type peerID struct {
	hub string
	at  time.Time
}

// peerIDFor returns the peer an unqualified id resolved to within
// PeerIDCacheFor, while that peer is linked.
func (m *Mesh) peerIDFor(id string) (string, bool) {
	m.peerIDsMu.Lock()
	p, ok := m.peerIDs[id]
	if ok && time.Since(p.at) >= m.cfg.PeerIDCacheFor {
		delete(m.peerIDs, id)
		ok = false
	}
	m.peerIDsMu.Unlock()
	if !ok {
		return "", false
	}
	if _, e := m.link(p.hub); e != nil {
		m.peerIDsMu.Lock()
		delete(m.peerIDs, id)
		m.peerIDsMu.Unlock()
		return "", false
	}
	return p.hub, true
}

func (m *Mesh) rememberPeerID(id, hub string) {
	m.peerIDsMu.Lock()
	m.peerIDs[id] = peerID{hub: hub, at: time.Now()}
	m.peerIDsMu.Unlock()
}

// forgetPeerIDs drops every remembered resolution: a device booted or
// created here may now answer to one of those ids, and a local device
// wins.
func (m *Mesh) forgetPeerIDs() {
	m.peerIDsMu.Lock()
	clear(m.peerIDs)
	m.peerIDsMu.Unlock()
}

// localHas reports whether this hub has device id, cheapest answer first:
// one it found within LocalCacheFor (a map lookup), then a device the
// daemon holds (attached or booted, from its records: GET /v1/status), and
// only then the daemon's own lookup, which runs live discovery (adb,
// simctl, emulator). Either of the last two remembers the id.
func (rt *router) localHas(r *http.Request, id string) bool {
	rt.seenMu.Lock()
	seen, ok := rt.localSeen[id]
	rt.seenMu.Unlock()
	if ok && time.Since(seen) < rt.m.cfg.LocalCacheFor {
		return true
	}
	ask := func(path string) *recorder {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://maestro-d"+path, nil)
		if err != nil {
			return nil
		}
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		req.RemoteAddr = r.RemoteAddr
		rec := newRecorder()
		rt.local.ServeHTTP(rec, req)
		return rec
	}
	found := false
	if rec := ask("/v1/status"); rec != nil && rec.status == http.StatusOK {
		var st daemon.StatusResult
		if json.Unmarshal(rec.buf.Bytes(), &st) == nil {
			for _, d := range st.Devices {
				found = found || d.ID == id
			}
		}
	}
	if !found {
		rec := ask("/v1/devices/" + url.PathEscape(id))
		found = rec != nil && rec.status == http.StatusOK
	}
	if found {
		rt.seenMu.Lock()
		rt.localSeen[id] = time.Now()
		rt.seenMu.Unlock()
	}
	return found
}

// leaseByID routes /v1/leases/{lid}/….
func (rt *router) leaseByID(w http.ResponseWriter, r *http.Request, lid, fwd string) {
	self := rt.m.cfg.Name
	local, hub := splitID(lid)
	switch {
	case hub == "" || hub == self:
		rt.serveLocal(w, withSeg(r, 2, local))
	case fwd != "":
		writeError(w, 0, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s belongs to hub %s, not %s", lid, hub, self).WithDetails("lease", lid))
	default:
		rt.forward(w, r, hub)
	}
}

// withBody returns r with a new JSON body.
func withBody(r *http.Request, body []byte) *http.Request {
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header.Del("Content-Length")
	return r2
}

// maxBody is the largest JSON body the router reads to route a request.
const maxBody = 32 << 20

// readBody reads a JSON body the router routes by. One over maxBody is
// USAGE, never cut short: a truncated body would be routed, or decoded, as
// something else.
func readBody(r *http.Request) ([]byte, *daemon.Error) {
	return readBodyMax(r, maxBody)
}

func readBodyMax(r *http.Request, limit int64) ([]byte, *daemon.Error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, daemon.WrapErr(daemon.CodeUsage, err)
	}
	if int64(len(data)) > limit {
		return nil, daemon.Errorf(daemon.CodeUsage, "request body too large (over %d bytes)", limit)
	}
	return data, nil
}

// acquire routes POST /v1/leases: by the body's device, or a selector
// across the mesh.
func (rt *router) acquire(w http.ResponseWriter, r *http.Request, fwd string) {
	self := rt.m.cfg.Name
	body, e := readBody(r)
	if e != nil {
		writeError(w, 0, e)
		return
	}
	var req daemon.LeaseRequest
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &req) != nil {
		rt.serveLocal(w, withBody(r, body))
		return
	}
	switch {
	case req.Device != "":
		local, hub := splitID(req.Device)
		if hub == "" && fwd == "" {
			var re *daemon.Error
			if hub, re = rt.resolve(r, local); re != nil {
				writeError(w, 0, re)
				return
			}
		}
		if hub == "" || hub == self || fwd != "" {
			req.Device = local
			if hub != "" && hub != self {
				req.Device = qualify(local, hub)
			}
			rt.serveLocal(w, withBody(r, mustJSON(req)))
			return
		}
		req.Device = qualify(local, hub)
		rt.forward(w, withBody(r, mustJSON(req)), hub)
	case req.Select != nil && fwd == "":
		rt.selectLease(w, r, req)
	default:
		rt.serveLocal(w, withBody(r, body))
	}
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// selectLease runs a selector on the mesh: this hub first, then the
// peers by RTT, the first owner that grants it wins. Ready devices are
// tried on every hub before any shut-down one is booted.
func (rt *router) selectLease(w http.ResponseWriter, r *http.Request, req daemon.LeaseRequest) {
	m := rt.m
	sel := *req.Select
	sel.Hub = hubName(sel.Hub)
	hubs := []string{m.cfg.Name}
	for _, l := range m.linked() {
		hubs = append(hubs, l.peer)
	}
	if sel.Hub != "" {
		var only []string
		for _, h := range hubs {
			if h == sel.Hub {
				only = append(only, h)
			}
		}
		if len(only) == 0 {
			if _, e := m.link(sel.Hub); e != nil && e.Code == daemon.CodePeerUnavailable {
				writeError(w, 0, e)
				return
			}
		}
		hubs = only
	}
	passes := []bool{false}
	if sel.Boot {
		passes = append(passes, true)
	}
	// Each try is its own request to its owner: a client's request id is
	// suffixed per hub and pass, so the owner's outcome cache neither
	// answers the boot pass with the ready pass's DEVICE_IN_USE nor runs a
	// retried selector's tries twice.
	rid := r.Header.Get(daemon.HeaderRequestID)
	// tooLong is an owner's USAGE for a window over its candidates'
	// maximum: another hub's devices may take it, so the search goes on,
	// and it is the answer when none does.
	var inUse, tooLong *recorder
	// unavailable names the hubs passed over because they were stopping or
	// unreachable: when no hub granted or refused, that is the answer, a
	// retryable PEER_UNAVAILABLE rather than "matches nothing".
	var unavailable []string
	for _, boot := range passes {
		for _, h := range hubs {
			s := sel
			s.Hub, s.Boot = h, boot
			try := req
			try.Select = &s
			tr := withBody(r, mustJSON(try))
			if rid != "" {
				pass := "ready"
				if boot {
					pass = "boot"
				}
				tr.Header.Set(daemon.HeaderRequestID, rid+"/"+h+"/"+pass)
			}
			rec := rt.capture(tr, h)
			if rec.status == http.StatusOK {
				rec.copyTo(w)
				return
			}
			switch envelopeCode(rec.buf.Bytes()) {
			case daemon.CodeDeviceInUse:
				if inUse == nil {
					inUse = rec
				}
			case daemon.CodeDeviceNotFound:
			case daemon.CodePeerUnavailable, daemon.CodeDaemonUnavailable:
				// A hub that is stopping (still linked while it ends its
				// leases) answers DAEMON_UNAVAILABLE: try the next hub.
				if !slices.Contains(unavailable, h) {
					unavailable = append(unavailable, h)
				}
			case daemon.CodeUsage:
				if !overMaxBody(rec.buf.Bytes()) {
					rec.copyTo(w)
					return
				}
				if tooLong == nil {
					tooLong = rec
				}
			default:
				rec.copyTo(w)
				return
			}
		}
	}
	if tooLong != nil {
		tooLong.copyTo(w)
		return
	}
	if inUse != nil {
		inUse.copyTo(w)
		return
	}
	if len(unavailable) > 0 {
		writeError(w, 0, daemon.Errorf(daemon.CodePeerUnavailable, "no hub granted the selector %s; hub %s did not answer or is stopping; retry",
			sel.String(), strings.Join(unavailable, ", ")).WithDetails("hubs", unavailable))
		return
	}
	writeError(w, 0, daemon.Errorf(daemon.CodeDeviceNotFound, "no device in the mesh matches the selector %s", sel.String()).WithDetails("select", sel))
}

// overMaxBody reports whether an owner's USAGE answer refused a lease
// window over the device's maximum (its details name maxSec).
func overMaxBody(body []byte) bool {
	var env daemon.Envelope
	if json.Unmarshal(body, &env) != nil || env.Error == nil {
		return false
	}
	_, ok := env.Error.Details["maxSec"]
	return ok
}

func envelopeCode(body []byte) daemon.Code {
	var env daemon.Envelope
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		return env.Error.Code
	}
	return ""
}

func (rec *recorder) copyTo(w http.ResponseWriter) {
	for k, v := range rec.header {
		if k == "Content-Length" {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.buf.Bytes())
}

// capture serves r on hub h (this one, or a peer) and returns the answer.
func (rt *router) capture(r *http.Request, h string) *recorder {
	rec := newRecorder()
	if h == rt.m.cfg.Name {
		rt.serveLocal(rec, r)
		return rec
	}
	rt.forward(rec, r, h)
	return rec
}

// boot routes POST /v1/devices by its body's hub, else its name's suffix.
func (rt *router) boot(w http.ResponseWriter, r *http.Request, fwd string) {
	self := rt.m.cfg.Name
	body, e := readBody(r)
	if e != nil {
		writeError(w, 0, e)
		return
	}
	var req daemon.BootRequest
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &req) != nil {
		rt.serveLocal(w, withBody(r, body))
		return
	}
	local, sfx := splitID(req.Name)
	hub := hubName(req.Hub)
	if hub == "" {
		hub = sfx
	}
	if sfx != "" && sfx == hub {
		req.Name = local
	}
	if hub == "" || hub == self || fwd != "" {
		req.Hub = ""
		rt.serveLocal(w, withBody(r, mustJSON(req)))
		return
	}
	rt.forward(w, withBody(r, mustJSON(req)), hub)
}

// ---------------------------------------------------------------------------
// Fan-out

type fanoutResult struct {
	hub  string
	body []byte
	err  error
}

type fanoutEntry struct {
	at  time.Time
	res fanoutResult
}

type fanoutCache struct {
	mu      sync.Mutex
	entries map[string]fanoutEntry
}

func (c *fanoutCache) get(key string, ttl time.Duration) (fanoutResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > ttl {
		return fanoutResult{}, false
	}
	return e.res, true
}

func (c *fanoutCache) put(key string, res fanoutResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = fanoutEntry{at: time.Now(), res: res}
}

// drop forgets a hub's cached answers (after a request that changed it).
func (c *fanoutCache) drop(hub string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if strings.HasPrefix(k, hub+" ") {
			delete(c.entries, k)
		}
	}
}

// fanout asks every linked peer for path (scope=local) in parallel, each
// within FanoutTimeout; answers are cached for CacheFor.
func (m *Mesh) fanout(ctx context.Context, path, query string) []fanoutResult {
	links := m.linked()
	out := make([]fanoutResult, len(links))
	var wg sync.WaitGroup
	for i, l := range links {
		key := l.peer + " " + path + "?" + query
		if res, ok := m.cache.get(key, m.cfg.CacheFor); ok {
			out[i] = res
			continue
		}
		wg.Add(1)
		go func(i int, l *Link, key string) {
			defer wg.Done()
			res := fanoutResult{hub: l.peer}
			cctx, cancel := context.WithTimeout(ctx, m.cfg.FanoutTimeout)
			defer cancel()
			resp, err := l.RoundTrip(cctx, Request{Method: http.MethodGet, Path: path, Query: query, Header: m.peerHeaders(nil),
				Principal: "hub " + m.cfg.Name})
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("no answer within %v", m.cfg.FanoutTimeout)
				}
				res.err = err
				out[i] = res
				return
			}
			res.body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && resp.Status != http.StatusOK {
				err = fmt.Errorf("HTTP %d", resp.Status)
			}
			res.err = err
			out[i] = res
			if err == nil {
				m.cache.put(key, res)
			}
		}(i, l, key)
	}
	wg.Wait()
	return out
}

// peerHeaders are the headers of a request this hub sends a peer: the
// client's, less hop-by-hop ones, with the mesh's client token.
func (m *Mesh) peerHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, v := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Te", "Trailer", "Content-Length":
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	out.Set("Authorization", "Bearer "+m.cfg.Token)
	out.Set("X-Maestro-Api-Version", daemon.APIVersion)
	return out
}

// deviceCounts counts each linked peer's shared devices.
func (m *Mesh) deviceCounts(ctx context.Context) map[string]int {
	out := map[string]int{}
	for _, res := range m.fanout(ctx, "/v1/devices", "scope=local") {
		var devs daemon.DevicesResult
		if res.err == nil && json.Unmarshal(res.body, &devs) == nil {
			out[res.hub] = len(devs.Devices)
		}
	}
	return out
}

func (rt *router) localCapture(r *http.Request) *recorder {
	rec := newRecorder()
	rt.serveLocal(rec, r)
	return rec
}

// listDevices merges this hub's devices with every peer's.
func (rt *router) listDevices(w http.ResponseWriter, r *http.Request) {
	rec := rt.localCapture(r)
	var out daemon.DevicesResult
	if rec.status != http.StatusOK || json.Unmarshal(rec.buf.Bytes(), &out) != nil {
		rec.copyTo(w)
		return
	}
	q := r.URL.Query()
	q.Set("scope", "local")
	for _, res := range rt.m.fanout(r.Context(), "/v1/devices", q.Encode()) {
		var devs daemon.DevicesResult
		if res.err == nil {
			if err := json.Unmarshal(res.body, &devs); err != nil {
				res.err = err
			}
		}
		if res.err != nil {
			out.Unreachable = append(out.Unreachable, daemon.UnreachablePeer{Hub: res.hub, Error: res.err.Error()})
			continue
		}
		out.Devices = append(out.Devices, devs.Devices...)
	}
	out.Unreachable = append(out.Unreachable, rt.m.down()...)
	writeJSON(w, http.StatusOK, out)
}

// listLeases merges this hub's leases with every peer's.
func (rt *router) listLeases(w http.ResponseWriter, r *http.Request) {
	rec := rt.localCapture(r)
	var out daemon.LeasesResult
	if rec.status != http.StatusOK || json.Unmarshal(rec.buf.Bytes(), &out) != nil {
		rec.copyTo(w)
		return
	}
	for _, res := range rt.m.fanout(r.Context(), "/v1/leases", "scope=local") {
		var ls daemon.LeasesResult
		if res.err == nil {
			if err := json.Unmarshal(res.body, &ls); err != nil {
				res.err = err
			}
		}
		if res.err != nil {
			out.Unreachable = append(out.Unreachable, daemon.UnreachablePeer{Hub: res.hub, Error: res.err.Error()})
			continue
		}
		out.Leases = append(out.Leases, ls.Leases...)
	}
	out.Unreachable = append(out.Unreachable, rt.m.down()...)
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Events

// events routes GET /v1/events: by its device's hub, or merged.
func (rt *router) events(w http.ResponseWriter, r *http.Request, fwd string, wide bool) {
	self := rt.m.cfg.Name
	q := r.URL.Query()
	if dev := q.Get("device"); dev != "" {
		local, hub := splitID(dev)
		if hub == "" && fwd == "" {
			// A bare id is the one mesh device that has it, as on the
			// device routes.
			target, e := rt.resolve(r, local)
			if e != nil {
				writeError(w, 0, e)
				return
			}
			hub = target
			if hub != self {
				q.Set("device", qualify(local, hub))
				r = r.Clone(r.Context())
				r.URL.RawQuery = q.Encode()
			}
		}
		if hub == "" || hub == self || fwd != "" {
			q.Set("device", local)
			r2 := r.Clone(r.Context())
			r2.URL.RawQuery = q.Encode()
			rt.serveLocal(w, r2)
			return
		}
		rt.forward(w, r, hub)
		return
	}
	if !wide {
		rt.serveLocal(w, r)
		return
	}
	rt.mergedEvents(w, r)
}

// pipeWriter is a ResponseWriter that streams into a pipe.
type pipeWriter struct {
	h http.Header
	w *io.PipeWriter
}

func (p *pipeWriter) Header() http.Header         { return p.h }
func (p *pipeWriter) WriteHeader(int)             {}
func (p *pipeWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeWriter) Flush()                      {}

// mergedEvents streams this hub's events and every linked peer's as one
// stream; each event names its device by qualified id. A peer is
// subscribed as soon as it is linked, and again when its stream ends while
// it is still linked (it restarted, or its link dropped and came back), so
// the stream follows the mesh for as long as the client holds it.
//
// Each hub numbers its own events, so the merged stream cannot be resumed:
// it carries no id: lines, and ?after= or Last-Event-ID is USAGE. A client
// that must not miss an event follows one device (?device=), or one hub
// (?scope=local), which resume as usual.
func (rt *router) mergedEvents(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("after") != "" || r.Header.Get("Last-Event-ID") != "" {
		writeError(w, 0, daemon.Errorf(daemon.CodeUsage, "the merged event stream of a hub cannot be resumed (each hub numbers its own events); "+
			"resume one device's stream (?device=) or one hub's (?scope=local) instead"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	blocks := make(chan string, 64)
	pr, pw := io.Pipe()
	go func() {
		rt.serveLocal(&pipeWriter{h: http.Header{}, w: pw}, r.WithContext(ctx))
		_ = pw.Close()
	}()
	go func() {
		<-ctx.Done()
		_ = pr.Close()
	}()
	go sseBlocks(bufio.NewReader(pr), blocks, ctx.Done())
	go rt.followPeers(ctx, r, blocks)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-blocks:
			if _, err := io.WriteString(w, withoutID(b)); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// followPeers keeps one event subscription open to every linked peer until
// ctx ends: a peer that links later is added, and one whose stream ends
// while it is linked is subscribed again (after BackoffMin, doubling up to
// BackoffMax while its streams end without an event).
func (rt *router) followPeers(ctx context.Context, r *http.Request, blocks chan<- string) {
	m := rt.m
	type ended struct {
		l   *Link
		got bool
	}
	subs := map[string]*Link{}
	backoff := map[string]time.Duration{}
	retryAt := map[string]time.Time{}
	done := make(chan ended)
	subscribe := func(l *Link) {
		subs[l.peer] = l
		go func() {
			got := rt.peerEvents(ctx, r, l, blocks)
			select {
			case done <- ended{l, got}:
			case <-ctx.Done():
			}
		}()
	}
	retry := time.NewTimer(time.Hour)
	defer retry.Stop()
	for {
		// Take the change channel before reading the links: a peer that
		// links after linked() closes this channel, so the loop wakes for it
		// (taken afterwards, that close would go to a channel never read).
		changed := m.changed()
		now := time.Now()
		next := time.Time{}
		for _, l := range m.linked() {
			if subs[l.peer] == l {
				continue
			}
			if at := retryAt[l.peer]; now.Before(at) {
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			subscribe(l)
		}
		if f := m.afterLinked.Load(); f != nil {
			(*f)()
		}
		if !next.IsZero() {
			retry.Reset(next.Sub(now))
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-retry.C:
		case e := <-done:
			if subs[e.l.peer] == e.l {
				delete(subs, e.l.peer)
			}
			d := backoff[e.l.peer]
			switch {
			case e.got || d == 0:
				d = m.cfg.BackoffMin
			default:
				d = min(d*2, m.cfg.BackoffMax)
			}
			backoff[e.l.peer] = d
			retryAt[e.l.peer] = time.Now().Add(d)
		}
	}
}

// peerEvents copies one peer's event stream into blocks until it ends;
// it reports whether any event came through.
func (rt *router) peerEvents(ctx context.Context, r *http.Request, l *Link, blocks chan<- string) bool {
	q := r.URL.Query()
	q.Set("scope", "local")
	res, err := rt.m.exchange(ctx, l, Request{Method: http.MethodGet, Path: "/v1/events", Query: q.Encode(),
		Header: rt.m.peerHeaders(r.Header), Principal: principal(r, rt.m.cfg.Name)}, true)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.Status != http.StatusOK {
		return false
	}
	got := make(chan string)
	fin := make(chan struct{})
	go func() {
		sseBlocks(bufio.NewReader(res.Body), got, ctx.Done())
		close(fin)
	}()
	seen := false
	for {
		select {
		case b := <-got:
			if !strings.HasPrefix(strings.TrimSpace(b), ":") {
				seen = true
			}
			select {
			case blocks <- b:
			case <-ctx.Done():
				return seen
			}
		case <-fin:
			return seen
		case <-ctx.Done():
			return seen
		}
	}
}

// withoutID drops an SSE block's id: line: the merged stream interleaves
// hubs whose sequence numbers are unrelated, so it offers none to resume.
func withoutID(block string) string {
	if !strings.Contains(block, "id:") {
		return block
	}
	lines := strings.SplitAfter(block, "\n")
	out := lines[:0]
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "id:") {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "")
}

// ---------------------------------------------------------------------------
// Forwarding

// principal names the client this hub authenticated.
func principal(r *http.Request, self string) string {
	if r.RemoteAddr == "" || r.RemoteAddr == "@" {
		return "unix socket on hub " + self
	}
	return r.RemoteAddr
}

// forward sends r to hub h and copies the answer back.
func (rt *router) forward(w http.ResponseWriter, r *http.Request, h string) {
	m := rt.m
	l, e := m.link(h)
	if e != nil {
		writeError(w, 0, e)
		return
	}
	req := Request{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Header: m.peerHeaders(r.Header),
		Principal: principal(r, m.cfg.Name)}
	bulk := needsBulk(r)
	if r.Body != nil && r.Body != http.NoBody && r.Method != http.MethodGet && r.Method != http.MethodHead {
		head, err := io.ReadAll(io.LimitReader(r.Body, maxInlineBody+1))
		switch {
		case err != nil:
			writeError(w, 0, daemon.WrapErr(daemon.CodeUsage, err))
			return
		case len(head) <= maxInlineBody:
			req.Body = head
		default:
			// The owner may answer while the body still streams (a lease
			// that ended under a stalled upload): in full duplex that
			// answer does not wait for the client's body, and the read
			// still pending is ended before this handler returns.
			rc := http.NewResponseController(w)
			_ = rc.EnableFullDuplex()
			up := &upload{r: r.Body}
			defer up.stop(rc)
			req.Stream = io.MultiReader(bytes.NewReader(head), up)
			bulk = true
		}
	}
	res, err := m.exchange(r.Context(), l, req, bulk)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeError(w, 0, daemon.Errorf(daemon.CodePeerUnavailable, "hub %s: %v", h, err).WithDetails("hub", h))
		return
	}
	defer res.Body.Close()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		m.cache.drop(h)
	}
	for k, v := range res.Header {
		if k == "Content-Length" && r.Method != http.MethodHead {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(res.Status)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// upload is a client's request body that a relaying hub streams to the
// owner beside the answer. stop ends it as forward returns: net/http
// forbids reading the body after that, and a read the client stalled
// would hold the connection until it sends or goes.
type upload struct {
	r       io.Reader
	mu      sync.Mutex
	stopped bool
	reading chan struct{}
}

func (u *upload) Read(p []byte) (int, error) {
	u.mu.Lock()
	if u.stopped {
		u.mu.Unlock()
		return 0, errUploadStopped
	}
	done := make(chan struct{})
	u.reading = done
	u.mu.Unlock()
	defer close(done)
	return u.r.Read(p)
}

// errUploadStopped is a read of an upload after its exchange answered.
var errUploadStopped = errors.New("the upload's exchange has answered")

// uploadStopWait bounds how long stop waits for the pending read.
var uploadStopWait = time.Second

// stop refuses further reads and ends one still pending, with a read
// deadline on the client's connection.
func (u *upload) stop(rc *http.ResponseController) {
	u.mu.Lock()
	u.stopped = true
	done := u.reading
	u.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	if err := rc.SetReadDeadline(time.Now()); err != nil {
		return
	}
	t := time.NewTimer(uploadStopWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
