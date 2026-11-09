package hub

// mesh.go: membership. A hub dials every hub its launch browse found and
// every --peer, and accepts the links other hubs dial. The first hello on
// each link carries the sender's linked peers, and the receiver dials the
// ones it is not linked to (join propagation), so one seed is enough for
// a full mesh, and a hub behind NAT joins just by dialing out. Only the
// dialing side redials a link that went down (backoff 1 s up to 30 s, for
// the life of the process); redialing a known peer is not discovery.

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// Peer-link headers.
const (
	headerHub         = "X-Maestro-Hub"
	headerHubInstance = "X-Maestro-Hub-Instance"
	headerHubAddr     = "X-Maestro-Hub-Addr"
	headerHubPort     = "X-Maestro-Hub-Port"
	headerHubTLS      = "X-Maestro-Hub-Tls"
)

// DefaultPort is a hub's port when an address names none.
const DefaultPort = 7433

var nameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidName reports whether name is a valid hub name: [a-z0-9-]{1,32}.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Config configures a hub's mesh.
type Config struct {
	// Name is this hub's name, unique in the mesh.
	Name string
	// Token is the client bearer token; forwarded requests carry it.
	Token string
	// PeerToken is the mesh secret every peer link sends.
	PeerToken string
	// Peers are the static peers (--peer): host[:port] or https://host[:port].
	Peers []string
	// Advertise is the address peers are told to dial (default: the
	// link's remote host and this hub's port).
	Advertise string
	// NoMDNS skips LAN discovery.
	NoMDNS bool
	// PeerCA, when set, is the trust root for peers' TLS.
	PeerCA *x509.CertPool
	// Version is maestro-d's version, sent in hello.
	Version string
	// Discoverer finds hubs on the LAN (default Zeroconf).
	Discoverer Discoverer
	// Logf receives the mesh's log lines (default: none).
	Logf func(format string, args ...any)

	// Timings; zero means the default.
	BrowseFor     time.Duration // 3 s
	ReadyWait     time.Duration // 10 s
	PingEvery     time.Duration // 10 s
	PingTimeout   time.Duration // PingEvery
	MissedPongs   int           // 3
	BackoffMin    time.Duration // 1 s
	BackoffMax    time.Duration // 30 s
	DialTimeout   time.Duration // 10 s
	BulkRetry     time.Duration // 60 s: retesting a peer address bulk links failed on
	FanoutTimeout time.Duration // 3 s
	CacheFor      time.Duration // 2 s
	LocalCacheFor time.Duration // 30 s: an unqualified id this hub listed
	// PeerIDCacheFor (10 s): an unqualified id this hub did not list and
	// exactly one peer did, so the next request for it skips this hub's
	// live discovery and the fan-out.
	PeerIDCacheFor time.Duration
}

func (c *Config) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&c.BrowseFor, 3*time.Second)
	set(&c.ReadyWait, 10*time.Second)
	set(&c.PingEvery, 10*time.Second)
	set(&c.PingTimeout, c.PingEvery)
	set(&c.BackoffMin, time.Second)
	set(&c.BackoffMax, 30*time.Second)
	set(&c.DialTimeout, 10*time.Second)
	set(&c.BulkRetry, time.Minute)
	set(&c.FanoutTimeout, 3*time.Second)
	set(&c.CacheFor, 2*time.Second)
	set(&c.LocalCacheFor, 30*time.Second)
	set(&c.PeerIDCacheFor, 10*time.Second)
	if c.MissedPongs <= 0 {
		c.MissedPongs = 3
	}
	if c.Discoverer == nil {
		c.Discoverer = Zeroconf{}
	}
}

type peerState struct {
	name, addr, instance string
	link                 *Link
	since                time.Time
	reason               string
	// stopped: the peer closed its link because it was shutting down.
	stopped bool
}

type dialer struct {
	addr string
	// expect is the hub the browse or a peer's list named at addr: a
	// different hub answering there is not linked.
	expect  string
	mu      sync.Mutex
	name    string
	refused *daemon.Error
	lastErr string
	first   chan struct{}
	firstE  error
	once    sync.Once
}

func (d *dialer) report(err error) {
	d.once.Do(func() {
		d.firstE = err
		close(d.first)
	})
}

// Mesh is a hub's peer mesh. It implements daemon.Mesh.
type Mesh struct {
	cfg      Config
	instance string
	self     daemon.Info
	port     int
	useTLS   bool

	ctx    context.Context
	cancel context.CancelFunc
	stopAd func()

	// routers in front of the unix socket and the TCP listener; forwarded
	// requests are served by the TCP one.
	unixRouter, tcpRouter atomic.Pointer[router]

	mu      sync.Mutex
	peers   map[string]*peerState
	dialers map[string]*dialer
	change  chan struct{}

	// browses counts the launch browses (tests assert there is one).
	browses atomic.Int32

	// afterLinked, when set (tests), runs in a merged event stream's loop
	// right after it read the linked peers.
	afterLinked atomic.Pointer[func()]

	cache fanoutCache

	// peerIDs remembers, for PeerIDCacheFor, the hub an unqualified id
	// resolved to when this hub did not list it (router.resolve).
	peerIDsMu sync.Mutex
	peerIDs   map[string]peerID

	bulkMu   sync.Mutex
	bulkSems map[string]chan struct{}
}

// New builds a mesh; Start links it.
func New(cfg Config) *Mesh {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Mesh{cfg: cfg, instance: uuid.New().String(), ctx: ctx, cancel: cancel,
		peers: map[string]*peerState{}, dialers: map[string]*dialer{}, change: make(chan struct{}),
		cache: fanoutCache{entries: map[string]fanoutEntry{}}, bulkSems: map[string]chan struct{}{}, peerIDs: map[string]peerID{}}
}

// Browses is how many launch browses ran (one, or none with --no-mdns).
func (m *Mesh) Browses() int { return int(m.browses.Load()) }

func (m *Mesh) logf(format string, args ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf("mesh: "+format, args...)
	}
}

// changed returns a channel that closes at the next change to the links.
func (m *Mesh) changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.change
}

func (m *Mesh) signalLocked() {
	close(m.change)
	m.change = make(chan struct{})
}

// Start runs discovery once and dials the peers. It returns when the hub
// may report READY: once every --peer's first dial has its hello answer,
// or after ReadyWait. A --peer that reaches this hub itself is dropped.
// When every other --peer refused this hub and the browse found no other
// hub, it returns that refusal.
func (m *Mesh) Start(ctx context.Context, self daemon.Info) error {
	m.self = self
	m.useTLS = self.TLS
	if _, p, err := net.SplitHostPort(self.HTTPAddr); err == nil {
		m.port, _ = strconv.Atoi(p)
	}
	if m.cfg.Advertise != "" {
		addr, err := advertisedAddr(m.cfg.Advertise, m.port, m.useTLS)
		if err != nil {
			return daemon.Errorf(daemon.CodeUsage, "--advertise %s: %v", m.cfg.Advertise, err)
		}
		m.cfg.Advertise = addr
	}
	var found []Found
	if !m.cfg.NoMDNS {
		stop, err := m.cfg.Discoverer.Advertise(m.cfg.Name, m.port, txtRecord(m.cfg.Name, daemon.APIVersion, m.useTLS))
		if err != nil {
			m.logf("mdns advertise: %v", err)
		} else {
			m.stopAd = stop
			m.logf("mdns advertise %s %q on port %d", ServiceType, m.cfg.Name, m.port)
		}
		m.browses.Add(1)
		found, err = m.cfg.Discoverer.Browse(ctx, m.cfg.BrowseFor)
		if err != nil {
			m.logf("mdns browse: %v", err)
		}
		var others []Found
		for _, f := range found {
			if f.Name != m.cfg.Name {
				others = append(others, f)
			}
		}
		found = others
		m.logf("mdns browse (%v, once at launch): %s", m.cfg.BrowseFor, describeFound(found))
	}
	for _, f := range found {
		m.ensureDialer(f.Addr, f.Name)
	}
	var static []*dialer
	for _, p := range m.cfg.Peers {
		addr, err := ParseAddr(p, DefaultPort)
		if err != nil {
			return daemon.Errorf(daemon.CodeUsage, "--peer %s: %v", p, err)
		}
		if d := m.ensureDialer(addr, ""); d != nil {
			static = append(static, d)
		}
	}
	if len(static) == 0 {
		return nil
	}
	deadline := time.NewTimer(m.cfg.ReadyWait)
	defer deadline.Stop()
	for _, d := range static {
		select {
		case <-d.first:
		case <-deadline.C:
			m.logf("ready after %v without an answer from every --peer", m.cfg.ReadyWait)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var refusal *daemon.Error
	for _, d := range static {
		var de *daemon.Error
		if !errors.As(d.firstE, &de) {
			return nil
		}
		if m.isSelf(de) {
			continue // this hub's own address: nothing to link
		}
		if !isRefusal(de) {
			return nil
		}
		refusal = de
	}
	if len(found) > 0 || refusal == nil {
		return nil
	}
	return refusal
}

// isSelf reports whether a dial's answer came from this process: a --peer
// that resolves to this hub (one shared config naming the seed, on the
// seed too). That dialer stops without counting as a refusal.
func (m *Mesh) isSelf(e *daemon.Error) bool {
	return e.Code == daemon.CodeHubNameTaken && e.Details["instance"] == m.instance
}

// Close ends every link, telling the peers this hub is stopping.
func (m *Mesh) Close() {
	m.cancel()
	if m.stopAd != nil {
		m.stopAd()
	}
	m.mu.Lock()
	var links []*Link
	for _, p := range m.peers {
		if p.link != nil {
			links = append(links, p.link)
		}
	}
	m.mu.Unlock()
	for _, l := range links {
		l.close(websocket.StatusGoingAway, "hub "+m.cfg.Name+" stopping")
	}
	for _, l := range links {
		select {
		case <-l.closed:
		case <-time.After(time.Second):
		}
	}
}

// advertisedAddr normalizes an address a hub advertises for itself (its
// --advertise, or a peer's): with no port it is the hub's listener port,
// and a hub that serves TLS is dialed with https://.
func advertisedAddr(v string, port int, useTLS bool) (string, error) {
	if port <= 0 {
		port = DefaultPort
	}
	addr, err := ParseAddr(v, port)
	if err != nil {
		return "", err
	}
	if useTLS && !strings.HasPrefix(addr, "https://") {
		addr = "https://" + addr
	}
	return addr, nil
}

// ParseAddr parses a hub address, as --peer, --advertise and
// --target-hub take it: HOST, HOST:PORT, http://HOST[:PORT] or
// https://HOST[:PORT] (a bare IPv6 address too). It returns host:port, or
// https://host:port for https, with port when the address names none.
func ParseAddr(v string, port int) (string, error) {
	v = strings.TrimSpace(v)
	scheme := "http"
	if i := strings.Index(v, "://"); i >= 0 {
		scheme, v = strings.ToLower(v[:i]), v[i+3:]
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (use http:// or https://)", scheme)
	}
	v = strings.TrimRight(v, "/")
	if v == "" || strings.ContainsAny(v, "/?#@ \t") {
		return "", fmt.Errorf("want HOST[:PORT] or https://HOST[:PORT], got %q", v)
	}
	host, p := v, strconv.Itoa(port)
	if h, hp, err := net.SplitHostPort(v); err == nil {
		if n, err := strconv.Atoi(hp); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("bad port %q", hp)
		}
		if h == "" {
			return "", fmt.Errorf("want HOST[:PORT], got %q", v)
		}
		host, p = h, hp
	}
	addr := net.JoinHostPort(strings.Trim(host, "[]"), p)
	if scheme == "https" {
		return "https://" + addr, nil
	}
	return addr, nil
}

// secure reports whether this hub dials only https:// peers: it serves
// TLS, or it has --peer-ca. Its links then carry the peer token, and the
// client token in forwarded requests, only inside TLS.
func (m *Mesh) secure() bool { return m.useTLS || m.cfg.PeerCA != nil }

// secureAddr is the address dial uses for addr: on a secure hub a plain
// address (from mDNS tls=0, a peer's list, a dialed-in peer or an explicit
// http:// --peer) becomes https://, so no peer token goes out over ws://.
// upgraded says it changed.
func (m *Mesh) secureAddr(addr string) (out string, upgraded bool) {
	if !m.secure() || addr == "" || strings.HasPrefix(addr, "https://") {
		return addr, false
	}
	return "https://" + addr, true
}

// ensureDialer starts dialing addr unless something already does; name is
// the hub expected there, when known.
func (m *Mesh) ensureDialer(addr, name string) *dialer {
	addr, upgraded := m.secureAddr(addr)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil
	}
	if d := m.dialers[addr]; d != nil {
		return d
	}
	if upgraded {
		m.logf("%s is a plain address; dialing %s, since this hub serves TLS or has --peer-ca and never dials ws://", strings.TrimPrefix(addr, "https://"), addr)
	}
	d := &dialer{addr: addr, expect: name, name: name, first: make(chan struct{})}
	m.dialers[addr] = d
	go m.runDialer(d)
	return d
}

// learn dials the peers another hub listed that this hub is not linked to.
func (m *Mesh) learn(refs []PeerRef) {
	for _, r := range refs {
		if r.Name == "" || r.Addr == "" || r.Name == m.cfg.Name {
			continue
		}
		m.mu.Lock()
		p := m.peers[r.Name]
		linked := p != nil && p.link != nil && p.link.alive()
		dialing := false
		for _, d := range m.dialers {
			d.mu.Lock()
			if a, _ := m.secureAddr(r.Addr); d.name == r.Name || d.addr == a {
				dialing = true
			}
			d.mu.Unlock()
		}
		m.mu.Unlock()
		if !linked && !dialing {
			m.logf("join: %s at %s, from a peer's list", r.Name, r.Addr)
			m.ensureDialer(r.Addr, r.Name)
		}
	}
}

// linkedWait returns the closed channel of the live link to name, or nil.
func (m *Mesh) linkedWait(name string) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peers[name]; p != nil && p.link != nil && p.link.alive() {
		return p.link.closed
	}
	return nil
}

func (m *Mesh) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-m.ctx.Done():
		return false
	}
}

// runDialer keeps one address linked for the life of the process: it
// waits while a live link to that hub exists (either direction), and
// redials with backoff when there is none. A refusal ends it.
func (m *Mesh) runDialer(d *dialer) {
	backoff := m.cfg.BackoffMin
	for m.ctx.Err() == nil {
		d.mu.Lock()
		name := d.name
		d.mu.Unlock()
		if name != "" {
			if ch := m.linkedWait(name); ch != nil {
				select {
				case <-ch:
					if !m.sleep(m.cfg.BackoffMin) {
						return
					}
				case <-m.ctx.Done():
					return
				}
				continue
			}
		}
		m.logf("dialing %s", d.addr)
		l, err := m.dial(d.addr, "", false)
		if err == nil && d.expect != "" && l.peer != d.expect {
			l.close(websocket.StatusPolicyViolation, "not the hub expected")
			l, err = nil, fmt.Errorf("hub %s answered at %s, where hub %s was expected", l.peer, d.addr, d.expect)
		}
		d.report(err)
		if err != nil {
			var de *daemon.Error
			if errors.As(err, &de) && m.isSelf(de) {
				m.logf("%s is this hub itself; not dialing it", d.addr)
				return
			}
			if errors.As(err, &de) && isRefusal(de) {
				m.logf("%s refused this hub: %s (%s); not dialing it again", d.addr, de.Message, de.Code)
				d.mu.Lock()
				d.refused = de
				d.mu.Unlock()
				return
			}
			d.mu.Lock()
			d.lastErr = err.Error()
			d.mu.Unlock()
			m.logf("dial %s: %v (again in %v)", d.addr, err, backoff)
			if !m.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, m.cfg.BackoffMax)
			continue
		}
		d.mu.Lock()
		d.name, d.lastErr = l.peer, ""
		d.mu.Unlock()
		ok, refused := m.registerLink(l)
		if refused != nil {
			l.close(websocket.StatusPolicyViolation, refused.Message)
			if !isRefusal(refused) {
				m.logf("not linking %s at %s: %s (%s); again in %v", l.peer, d.addr, refused.Message, refused.Code, backoff)
				d.mu.Lock()
				d.lastErr = refused.Message
				d.mu.Unlock()
				if !m.sleep(backoff) {
					return
				}
				backoff = min(backoff*2, m.cfg.BackoffMax)
				continue
			}
			m.logf("not linking %s at %s: %s (%s); not dialing it again", l.peer, d.addr, refused.Message, refused.Code)
			d.mu.Lock()
			d.refused = refused
			d.mu.Unlock()
			return
		}
		backoff = m.cfg.BackoffMin
		if ok {
			select {
			case <-l.closed:
			case <-m.ctx.Done():
				return
			}
			if !m.sleep(m.cfg.BackoffMin) {
				return
			}
		}
	}
}

// isRefusal reports whether a dial's answer means "never again": a wrong
// peer token or bad hub name (USAGE, from the hub's own error envelope),
// another API version, or the dialing hub's own name. A name a live link
// from another process holds (details.addr) is not: that is usually the
// same hub restarted at a new address while its old link has not yet
// missed its pongs, so the dialer retries with backoff and links once the
// old link closes.
func isRefusal(e *daemon.Error) bool {
	switch e.Code {
	case daemon.CodeHubVersion, daemon.CodeUsage:
		return true
	case daemon.CodeHubNameTaken:
		return !nameHeldByLink(e)
	}
	return false
}

// nameHeldByLink reports whether a HUB_NAME_TAKEN means another process's
// live link holds the name (checkNameLocked names its address), rather
// than that the name is the answering hub's own.
func nameHeldByLink(e *daemon.Error) bool {
	return e.Code == daemon.CodeHubNameTaken && e.Details["addr"] != nil
}

func wsURL(addr string, bulk bool) string {
	scheme, host := "ws://", addr
	if strings.HasPrefix(addr, "https://") {
		scheme, host = "wss://", strings.TrimPrefix(addr, "https://")
	}
	u := scheme + host + "/v1/hub/link"
	if bulk {
		u += "?bulk=1"
	}
	return u
}

func (m *Mesh) httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: m.cfg.PeerCA}
	return &http.Client{Transport: tr}
}

func (m *Mesh) hello() Frame {
	return Frame{T: frameHello, Name: m.cfg.Name, Version: m.cfg.Version, APIVersion: daemon.APIVersion,
		Instance: m.instance, Addr: m.cfg.Advertise, Port: m.port, TLS: m.useTLS, Peers: m.peerRefs()}
}

// peerRefs lists the linked peers, for hello and peers frames. Only an
// address this hub has dialed is passed on: a peer that dialed in from
// behind NAT has one nobody can reach.
func (m *Mesh) peerRefs() []PeerRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PeerRef
	for _, p := range m.peers {
		if p.link != nil && p.link.alive() && p.addr != "" && p.link.dialable.Load() {
			out = append(out, PeerRef{Name: p.name, Addr: p.addr})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// dial opens a link to addr: a control link (with hello) or a bulk one
// to peer, the hub whose control link it serves (named before its read
// loop starts, which logs it).
func (m *Mesh) dial(addr, peer string, bulk bool) (*Link, error) {
	// A bulk link to a dialed-in peer uses the address it gave: on a
	// secure hub that too goes over TLS.
	addr, _ = m.secureAddr(addr)
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.DialTimeout)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+m.cfg.PeerToken)
	h.Set(headerHub, m.cfg.Name)
	h.Set(headerHubInstance, m.instance)
	h.Set("X-Maestro-Api-Version", daemon.APIVersion)
	if m.cfg.Advertise != "" {
		h.Set(headerHubAddr, m.cfg.Advertise)
	}
	h.Set(headerHubPort, strconv.Itoa(m.port))
	if m.useTLS {
		h.Set(headerHubTLS, "1")
	}
	conn, resp, err := websocket.Dial(ctx, wsURL(addr, bulk), &websocket.DialOptions{HTTPHeader: h, HTTPClient: m.httpClient()})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, refusalFrom(resp)
		}
		return nil, err
	}
	l := newLink(m, conn, true, bulk)
	l.addr, l.peer = addr, peer
	if bulk {
		go l.readLoop()
		return l, nil
	}
	if err := l.writeFrame(m.hello()); err != nil {
		l.close(websocket.StatusInternalError, "hello failed")
		return nil, err
	}
	f, err := readHello(ctx, conn)
	if err != nil {
		l.close(websocket.StatusProtocolError, "no hello")
		return nil, err
	}
	if f.APIVersion != daemon.APIVersion {
		l.close(websocket.StatusPolicyViolation, "API version")
		return nil, daemon.Errorf(daemon.CodeHubVersion, "hub %s speaks API %s, this hub %s", f.Name, f.APIVersion, daemon.APIVersion)
	}
	l.peer, l.instance = f.Name, f.Instance
	l.dialable.Store(true)
	go l.readLoop()
	go l.pingLoop(m.cfg.PingEvery, m.cfg.PingTimeout, m.cfg.MissedPongs)
	m.learn(f.Peers)
	return l, nil
}

// refusalFrom decodes a refused upgrade: the hub's error envelope, or a
// plain (retried) error naming the status.
func refusalFrom(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var env daemon.Envelope
	if json.Unmarshal(data, &env) == nil && env.Error != nil && env.Error.Code != "" {
		return &daemon.Error{ErrorBody: *env.Error}
	}
	// No envelope: a proxy or another server answered (a 404, a 429, a
	// 502). That is retried like a dial error, never taken as a refusal.
	return fmt.Errorf("peer link refused: HTTP %d (not a maestro-d hub, or a proxy in the way?)", resp.StatusCode)
}

func readHello(ctx context.Context, conn *websocket.Conn) (Frame, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return Frame{}, fmt.Errorf("waiting for hello: %w", err)
	}
	if typ != websocket.MessageText {
		return Frame{}, errors.New("waiting for hello: got a data frame")
	}
	f, err := decodeFrame(data)
	if err != nil {
		return Frame{}, err
	}
	if f.T != frameHello || f.Name == "" {
		return Frame{}, fmt.Errorf("waiting for hello: got %q", f.T)
	}
	return f, nil
}

// register makes l the link to its peer. When a live link to that peer
// already exists from the same process (both hubs dialed at once), the
// one dialed by the lexically lower name survives; a link from a new
// process of that hub (it restarted) replaces the old one. It reports
// whether l is now the link.
func (m *Mesh) register(l *Link) bool {
	ok, _ := m.registerLink(l)
	return ok
}

// registerLink is register, which for a link this hub dialed first makes
// the check an inbound link gets before it is accepted (checkName): a hub
// with this hub's name, or a second hub with a linked peer's name, is
// refused rather than replacing the live link.
func (m *Mesh) registerLink(l *Link) (bool, *daemon.Error) {
	m.mu.Lock()
	if l.outbound {
		if e := m.checkNameLocked(l.peer, l.instance, l.addr); e != nil {
			m.mu.Unlock()
			return false, e
		}
	}
	p := m.peers[l.peer]
	if p == nil {
		p = &peerState{name: l.peer}
		m.peers[l.peer] = p
	}
	old := p.link
	if old != nil && old.alive() && old.instance == l.instance {
		if dialerName(m.cfg.Name, old) < dialerName(m.cfg.Name, l) {
			m.mu.Unlock()
			m.logf("link %s: duplicate (both hubs dialed); keeping the one %s dialed", l.peer, dialerName(m.cfg.Name, old))
			l.close(websocket.StatusNormalClosure, "duplicate link")
			return false, nil
		}
	}
	p.link, p.instance, p.since, p.stopped, p.reason = l, l.instance, time.Now(), false, ""
	if l.addr != "" {
		p.addr = l.addr
	}
	m.signalLocked()
	m.mu.Unlock()
	if old != nil && old != l {
		old.close(websocket.StatusNormalClosure, "replaced by a newer link")
	}
	dir := "inbound"
	if l.outbound {
		dir = "outbound"
	}
	m.logf("link %s up (%s, %s)", l.peer, dir, l.addr)
	m.cache.drop(l.peer)
	go m.watch(l)
	go m.broadcastPeers()
	if !l.dialable.Load() && l.addr != "" {
		go m.probeBulk(l)
	}
	return true, nil
}

func dialerName(self string, l *Link) string {
	if l.outbound {
		return self
	}
	return l.peer
}

// watch records a link's end.
func (m *Mesh) watch(l *Link) {
	<-l.closed
	m.mu.Lock()
	p := m.peers[l.peer]
	if p != nil && p.link == l {
		p.link = nil
		p.reason = l.Reason()
		l.mu.Lock()
		p.stopped = l.goingAway
		l.mu.Unlock()
		p.since = time.Now()
		m.signalLocked()
	}
	removed := p != nil && p.link == nil
	m.mu.Unlock()
	if removed {
		m.logf("link %s down: %s", l.peer, l.Reason())
	}
	m.cache.drop(l.peer)
}

func (m *Mesh) broadcastPeers() {
	refs := m.peerRefs()
	m.mu.Lock()
	var links []*Link
	for _, p := range m.peers {
		if p.link != nil && p.link.alive() {
			links = append(links, p.link)
		}
	}
	m.mu.Unlock()
	for _, l := range links {
		_ = l.writeFrame(Frame{T: framePeers, Peers: refs})
	}
}

// link returns the live link to hub name, or PEER_UNAVAILABLE (a known
// hub that is down) or DEVICE_NOT_FOUND (no such hub).
func (m *Mesh) link(name string) (*Link, *daemon.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.peers[name]
	if p != nil && p.link != nil && p.link.alive() {
		return p.link, nil
	}
	if p != nil {
		e := daemon.Errorf(daemon.CodePeerUnavailable, "hub %s is not linked to this hub (%s)", name, orUnknown(p.reason))
		return nil, e.WithDetails("hub", name)
	}
	for _, d := range m.dialers {
		d.mu.Lock()
		dn := d.name
		d.mu.Unlock()
		if dn == name {
			return nil, daemon.Errorf(daemon.CodePeerUnavailable, "hub %s is not linked to this hub", name).WithDetails("hub", name)
		}
	}
	return nil, daemon.Errorf(daemon.CodeDeviceNotFound, "no hub named %s in the mesh", name).WithDetails("hub", name)
}

func orUnknown(s string) string {
	if s == "" {
		return "not linked yet"
	}
	return s
}

// linked lists the live links, lowest RTT first.
// downPeers names the known peers whose link is down without their having
// stopped: their devices may still exist, so an id they might own cannot
// be judged absent. A peer that shut down cleanly is not listed.
func (m *Mesh) downPeers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, p := range m.peers {
		if !p.stopped && (p.link == nil || !p.link.alive()) {
			out = append(out, p.name)
		}
	}
	return out
}

func (m *Mesh) linked() []*Link {
	m.mu.Lock()
	var out []*Link
	for _, p := range m.peers {
		if p.link != nil && p.link.alive() {
			out = append(out, p.link)
		}
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RTT() != out[j].RTT() {
			return out[i].RTT() < out[j].RTT()
		}
		return out[i].peer < out[j].peer
	})
	return out
}

// down lists the known hubs that are not linked and did not stop cleanly.
func (m *Mesh) down() []daemon.UnreachablePeer {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []daemon.UnreachablePeer
	for _, p := range m.peers {
		if (p.link == nil || !p.link.alive()) && !p.stopped {
			out = append(out, daemon.UnreachablePeer{Hub: p.name, Error: "link down: " + orUnknown(p.reason)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hub < out[j].Hub })
	return out
}

// ---------------------------------------------------------------------------
// Accepting links

func (m *Mesh) peerAuthorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	want := "Bearer " + m.cfg.PeerToken
	return m.cfg.PeerToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// acceptLink serves GET /v1/hub/link: a peer's control or bulk link.
func (m *Mesh) acceptLink(w http.ResponseWriter, r *http.Request) {
	if !m.peerAuthorized(r) {
		writeError(w, http.StatusUnauthorized, daemon.Errorf(daemon.CodeUsage, "missing or wrong peer token"))
		return
	}
	if v := r.Header.Get("X-Maestro-Api-Version"); v != daemon.APIVersion {
		writeError(w, 0, daemon.Errorf(daemon.CodeHubVersion, "hub %s speaks API %s; this hub %s", r.Header.Get(headerHub), v, daemon.APIVersion))
		return
	}
	name := r.Header.Get(headerHub)
	if !ValidName(name) {
		writeError(w, 0, daemon.Errorf(daemon.CodeUsage, "bad hub name %q in %s", name, headerHub))
		return
	}
	bulk := r.URL.Query().Get("bulk") == "1"
	addr := peerAddr(r)
	if !bulk {
		if e := m.checkName(name, r.Header.Get(headerHubInstance), addr); e != nil {
			if !m.isSelf(e) { // the dialer, this hub, logs a self-link
				m.logf("refused a link from %s (%s): %s", name, r.RemoteAddr, e.Message)
			}
			writeError(w, 0, e)
			return
		}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		m.logf("accept %s: %v", name, err)
		return
	}
	l := newLink(m, conn, false, bulk)
	l.peer, l.addr = name, addr
	if bulk {
		go l.readLoop()
		tieToControl(l, m.linkedWait(name))
		<-l.closed
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.DialTimeout)
	defer cancel()
	if err := l.writeFrame(m.hello()); err != nil {
		l.close(websocket.StatusInternalError, "hello failed")
		return
	}
	f, err := readHello(ctx, conn)
	if err != nil || f.Name != name {
		l.close(websocket.StatusProtocolError, "bad hello")
		return
	}
	l.instance = f.Instance
	if f.Addr != "" {
		if a, err := advertisedAddr(f.Addr, f.Port, f.TLS); err == nil {
			l.addr = a
		}
	}
	go l.readLoop()
	go l.pingLoop(m.cfg.PingEvery, m.cfg.PingTimeout, m.cfg.MissedPongs)
	if m.register(l) {
		m.learn(f.Peers)
	}
	<-l.closed
}

// checkName refuses a link whose name is this hub's own, or a name a live
// link from another process at another address already holds.
func (m *Mesh) checkName(name, instance, addr string) *daemon.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkNameLocked(name, instance, addr)
}

// checkNameLocked is checkName with m.mu held.
func (m *Mesh) checkNameLocked(name, instance, addr string) *daemon.Error {
	if name == m.cfg.Name && instance == m.instance {
		// A --peer (or a peer's list) that reaches this hub itself: the
		// details carry this process's instance, so the dialer knows.
		return daemon.Errorf(daemon.CodeHubNameTaken, "this is hub %q itself", name).
			WithDetails("hub", name, "instance", instance)
	}
	if name == m.cfg.Name {
		return daemon.Errorf(daemon.CodeHubNameTaken, "hub name %q is this hub's own name; give the other hub its own --name", name).
			WithDetails("hub", name)
	}
	if p := m.peers[name]; p != nil && p.link != nil && p.link.alive() && p.instance != instance && p.addr != "" && p.addr != addr {
		return daemon.Errorf(daemon.CodeHubNameTaken, "hub name %q is already linked from %s", name, p.addr).
			WithDetails("hub", name, "addr", p.addr)
	}
	return nil
}

// peerAddr is the address a dialing peer is reached at: its --advertise,
// else its remote host and listener port.
func peerAddr(r *http.Request) string {
	useTLS := r.Header.Get(headerHubTLS) == "1"
	port := r.Header.Get(headerHubPort)
	if a := r.Header.Get(headerHubAddr); a != "" {
		p, _ := strconv.Atoi(port)
		if n, err := advertisedAddr(a, p, useTLS); err == nil {
			return n
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if port == "" || port == "0" {
		return ""
	}
	addr := net.JoinHostPort(host, port)
	if useTLS {
		addr = "https://" + addr
	}
	return addr
}

// ---------------------------------------------------------------------------
// Status

// Status is GET /v1/hub's hub.
func (m *Mesh) Status(ctx context.Context) daemon.HubStatus {
	st := daemon.HubStatus{Name: m.cfg.Name, Version: m.cfg.Version, APIVersion: daemon.APIVersion,
		Addr: m.cfg.Advertise, Listen: m.self.HTTPAddr, TLS: m.useTLS, MDNS: !m.cfg.NoMDNS, Peers: []daemon.HubPeer{}}
	counts := m.deviceCounts(ctx)
	m.mu.Lock()
	for _, p := range m.peers {
		hp := daemon.HubPeer{Name: p.name, Addr: p.addr, State: "down", Error: p.reason}
		if p.stopped {
			hp.State = "stopped"
		}
		if p.link != nil && p.link.alive() {
			hp.State, hp.Error = "linked", ""
			hp.Direction = "inbound"
			if p.link.outbound {
				hp.Direction = "outbound"
			}
			hp.RTTMs = float64(p.link.RTT().Microseconds()) / 1000
			n := counts[p.name]
			hp.Devices = &n
		}
		if !p.since.IsZero() {
			hp.Since = p.since.UTC().Format(time.RFC3339)
		}
		st.Peers = append(st.Peers, hp)
	}
	known := map[string]bool{}
	for _, p := range m.peers {
		known[p.name] = true
	}
	for _, d := range m.dialers {
		d.mu.Lock()
		if (d.name == "" || !known[d.name]) && (d.refused != nil || d.lastErr != "") {
			hp := daemon.HubPeer{Name: d.name, Addr: d.addr, State: "connecting", Error: d.lastErr}
			if d.refused != nil {
				hp.State, hp.Error = "refused", d.refused.Message
			}
			st.Peers = append(st.Peers, hp)
		}
		d.mu.Unlock()
	}
	m.mu.Unlock()
	sort.Slice(st.Peers, func(i, j int) bool {
		if st.Peers[i].Name != st.Peers[j].Name {
			return st.Peers[i].Name < st.Peers[j].Name
		}
		return st.Peers[i].Addr < st.Peers[j].Addr
	})
	return st
}
