package daemon

// leases.go: device leases on a hub daemon.
//
// A hub shares its devices with every client that holds its token, so a
// device is taken for a while with a lease: POST /v1/leases, by device id
// or by selector. Every route that drives or changes a device then needs
// X-Maestro-Lease naming that lease (LEASE_REQUIRED without it,
// LEASE_EXPIRED with a stale one).
//
// A lease is a fixed window: ttlSec seconds from the grant (8 hours by
// default), never longer than the device's maximum (8 hours, or the hub's
// --lease-max, hub-wide or per device). Using it never moves its expiry;
// only POST /v1/leases/{lid}/renew does. At expiry, or on DELETE
// /v1/leases/{lid}, the hub ends the lease: a request still running under
// it is cancelled (a step reports INTERRUPTED; one whose driver call
// ignores that is answered LEASE_EXPIRED), and once every request under it
// has unwound the device is detached and one the hub booted for the lease
// is shut down, so the device shows as free in every listing again.
//
// A plain daemon (not a hub) has no leases: there, attach-and-own stays as
// it was, and the lease routes answer USAGE.
//
// --share-device keeps a hub's other devices private to its own machine:
// a remote caller does not see them in listings and is refused them with
// DEVICE_NOT_FOUND. The unix socket, which only the hub's machine reaches,
// sees every device.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// HeaderLease carries the lease id on every request that drives a leased
// device.
const HeaderLease = "X-Maestro-Lease"

const (
	// DefaultLeaseTTL is a lease's window when the request names none (or
	// the device's maximum, when that is shorter).
	DefaultLeaseTTL = 8 * time.Hour
	// DefaultLeaseMax is the longest window a hub grants unless
	// --lease-max says otherwise.
	DefaultLeaseMax = 8 * time.Hour
	// leaseSweepEvery is how often a hub ends the leases that expired.
	leaseSweepEvery = time.Second
)

// Lease is one lease, as the lease routes return it.
type Lease struct {
	// ID is what X-Maestro-Lease sends.
	ID     string `json:"id"`
	Device string `json:"device"`
	Holder string `json:"holder,omitempty"`
	// ExpiresAt is RFC 3339: the end of the window. Only an explicit
	// renew moves it.
	ExpiresAt string `json:"expiresAt"`
	// TTLSec is the window's length; MaxSec the device's maximum window.
	TTLSec int `json:"ttlSec"`
	MaxSec int `json:"maxSec"`
	// Booted is true when the hub booted the device for this lease; it
	// shuts it down again when the lease ends.
	Booted bool `json:"booted,omitempty"`
}

// DeviceLease is the lease state a hub's device listings carry.
type DeviceLease struct {
	Holder    string `json:"holder,omitempty"`
	ExpiresAt string `json:"expiresAt"`
}

// LeaseSelector picks a device by its properties. Every field set must
// match: platform and kind exactly, name as the device's name or id,
// osVersion as the version or a prefix of it ("26" matches "26.5"), hub as
// the hub's name.
type LeaseSelector struct {
	Platform  string `json:"platform,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	OSVersion string `json:"osVersion,omitempty"`
	Hub       string `json:"hub,omitempty"`
	// Boot makes shut-down simulators and AVDs candidates too: the hub
	// boots the one it picks, and shuts it down when the lease ends.
	Boot bool `json:"boot,omitempty"`
}

// LeaseRequest is the body of POST /v1/leases.
type LeaseRequest struct {
	// Device or Select names the device; exactly one is given.
	Device string         `json:"device,omitempty"`
	Select *LeaseSelector `json:"select,omitempty"`
	// TTLSec is the window in seconds: default 8 hours (or the device's
	// maximum, when that is shorter); over the maximum is USAGE.
	TTLSec int    `json:"ttlSec,omitempty"`
	Holder string `json:"holder,omitempty"`
	// Attach, when given, attaches the device in the same call.
	Attach *AttachConfig `json:"attach,omitempty"`
}

// RenewRequest is the body of POST /v1/leases/{lid}/renew.
type RenewRequest struct {
	// TTLSec is the new window from now (default the lease's own).
	TTLSec int `json:"ttlSec,omitempty"`
}

// LeaseMax is one --lease-max DEVICE=DURATION: the longest window a lease
// on that device (by id or name, as for --share-device) may have.
type LeaseMax struct {
	Device string
	Max    time.Duration
}

// LeaseResult answers POST /v1/leases, GET /v1/leases/{lid}, renew and
// release.
type LeaseResult struct {
	Envelope
	Lease  Lease       `json:"lease"`
	Device *DeviceInfo `json:"device,omitempty"`
}

// LeasesResult answers GET /v1/leases.
type LeasesResult struct {
	Envelope
	Leases []Lease `json:"leases"`
}

type leaseEntry struct {
	id, device, holder string
	ttl, max           time.Duration
	expires            time.Time
	booted             bool
	// avd is the bootKey of the AVD an emulator lease's device runs
	// ("" for any other device): shut down, the AVD is listed under it.
	avd string
	// ended is closed when the lease ends, cancelling the requests that
	// run under it.
	ended chan struct{}
	// work counts the requests (serveLeased) and attaches (attachLeased)
	// running under the lease, and inFlight is its value: the lease's
	// teardown waits for them, so a driver call that ignores cancellation
	// (WebDriverAgent's HTTP calls, its start) can never reach the device
	// after it went to a later holder. work.Add happens only while the
	// lease is in the table (startWork), so never after the teardown's
	// Wait began.
	work     sync.WaitGroup
	inFlight atomic.Int32
	// stopped is set once the holder has stopped or deleted the device
	// under the lease: its teardown does not shut it down again.
	stopped atomic.Bool
}

// The reasons a lease ends, as the hub logs them; leaseEndEvent names the
// documented event each one publishes.
const (
	endExpired      = "expired"
	endReleased     = "released"
	endAttachFailed = "attach failed"
	endStopped      = "ended (device stopped)"
	endDeleted      = "ended (device deleted)"
	endHubStopped   = "ended (hub stopped)"
)

// leaseEndEvent is the event an ended lease publishes: lease.expired when
// its window ran out, lease.released for every other end (a release, its
// device stopped or deleted, the attach in its request failing, the hub
// stopping).
func leaseEndEvent(reason string) string {
	if reason == endExpired {
		return "lease.expired"
	}
	return "lease.released"
}

// startWork counts a request or an attach under live lease e; false when
// e has expired or ended, and the work must not start.
func (t *leaseTable) startWork(e *leaseEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID[e.id] != e || !e.liveAt(t.now()) {
		return false
	}
	e.work.Add(1)
	e.inFlight.Add(1)
	return true
}

// doneWork ends the work startWork counted.
func (e *leaseEntry) doneWork() {
	e.inFlight.Add(-1)
	e.work.Done()
}

// leaseExpiredErr is LEASE_EXPIRED for a request whose lease e ended
// before it could start.
func leaseExpiredErr(e *leaseEntry) *Error {
	return Errorf(CodeLeaseExpired, "lease %s on device %s has expired or was released; take a new one", e.id, e.device).
		WithDetails("lease", e.id)
}

// beingReleasedErr is DEVICE_IN_USE for a device whose ended lease is
// still being torn down.
func beingReleasedErr(device string) *Error {
	return Errorf(CodeDeviceInUse, "device %s is being released (its lease ended); try again shortly", device)
}

// pending counts work that must finish before the hub stops (the lease
// teardowns). Unlike a WaitGroup it may be added to while wait runs, and
// wait gives up after a while.
type pending struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

func (p *pending) add() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == 0 {
		p.idle = make(chan struct{})
	}
	p.n++
}

func (p *pending) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n--
	if p.n == 0 {
		close(p.idle)
	}
}

// wait waits until nothing is pending, for at most d; false when it gave
// up.
func (p *pending) wait(d time.Duration) bool {
	p.mu.Lock()
	if p.n == 0 {
		p.mu.Unlock()
		return true
	}
	idle := p.idle
	p.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-idle:
		return true
	case <-t.C:
		return false
	}
}

// reserved reports whether listed device d is reserved: booting for a
// selector lease, held by a lease that expired but has not ended yet (the
// sweeper ends it within a second), or still being torn down after its
// lease ended (an attach under it may still be unwinding). Such a device
// is not available.
func (t *leaseTable) reserved(d DeviceInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reservedLocked(d) || t.ending[bootKey(d)] || t.byDevice[d.ID] != nil
}

// leaseTable holds a hub's live leases.
type leaseTable struct {
	mu       sync.Mutex
	now      func() time.Time
	byID     map[string]*leaseEntry
	byDevice map[string]*leaseEntry
	// booting reserves the shut-down devices a selector lease is booting,
	// by bootKey, until the lease on the booted device is granted.
	booting map[string]bool
	// ending reserves the devices whose ended lease is still being torn
	// down (detached, and shut down when the lease booted them), so no new
	// lease takes one mid-shutdown. An emulator's AVD is reserved under
	// its bootKey too: it can be listed shut down before the teardown ends.
	ending map[string]bool
}

// bootKey names a shut-down device in the booting reservations so that
// the ready device it becomes matches too: a simulator keeps its UDID,
// and an AVD becomes an emulator serial listed under the AVD's name.
func bootKey(d DeviceInfo) string {
	if name, ok := strings.CutPrefix(d.ID, AVDPrefix); ok {
		return "android:" + strings.ToLower(name)
	}
	return d.ID
}

// avdKey is the bootKey of the AVD that running Android device d is listed
// under (an emulator's name is its AVD's), or "" for any other device.
func avdKey(d DeviceInfo) string {
	if d.Platform != "android" || d.Name == "" {
		return ""
	}
	return "android:" + strings.ToLower(d.Name)
}

// reservedLocked reports whether device d is one a selector lease is
// booting (it may be listed ready before that boot returns) or a teardown
// is shutting down, by its id or, for a running emulator or a shut-down
// AVD, by its AVD's key. The caller holds t.mu.
func (t *leaseTable) reservedLocked(d DeviceInfo) bool {
	if t.booting[d.ID] || t.ending[d.ID] {
		return true
	}
	for _, k := range []string{avdKey(d), bootKey(d)} {
		if k != "" && (t.booting[k] || t.ending[k]) {
			return true
		}
	}
	return false
}

// releasing reports whether device d is being released or reserved: held
// by a lease that expired but has not ended yet, still being torn down
// after its lease ended, or booting for a selector lease (reservedLocked,
// so an emulator and its AVD are one device).
func (t *leaseTable) releasing(d DeviceInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reservedLocked(d) {
		return true
	}
	e := t.byDevice[d.ID]
	return e != nil && !e.liveAt(t.now())
}

// markBooted makes the live lease on device shut it down when it ends;
// false when no live lease holds it.
func (t *leaseTable) markBooted(device string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byDevice[device]; e != nil && e.liveAt(t.now()) {
		e.booted = true
		return true
	}
	return false
}

func newLeaseTable() *leaseTable {
	return &leaseTable{now: time.Now, byID: map[string]*leaseEntry{}, byDevice: map[string]*leaseEntry{}, booting: map[string]bool{}, ending: map[string]bool{}}
}

func (e *leaseEntry) liveAt(now time.Time) bool { return now.Before(e.expires) }

func (e *leaseEntry) view() Lease {
	return Lease{ID: e.id, Device: e.device, Holder: e.holder, ExpiresAt: e.expires.UTC().Format(time.RFC3339),
		TTLSec: int(e.ttl / time.Second), MaxSec: int(e.max / time.Second), Booted: e.booted}
}

// liveFor returns the live lease on device, if any.
func (t *leaseTable) liveFor(device string) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byDevice[device]; e != nil && e.liveAt(t.now()) {
		return e.view(), true
	}
	return Lease{}, false
}

// get returns a live lease by id.
func (t *leaseTable) get(id string) (Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byID[id]; e != nil && e.liveAt(t.now()) {
		return e.view(), true
	}
	return Lease{}, false
}

// list returns every live lease, by device.
func (t *leaseTable) list() []Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := []Lease{}
	for _, e := range t.byID {
		if e.liveAt(now) {
			out = append(out, e.view())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// renew sets a live lease's window to ttlSec seconds from now (0: its own
// window), refusing one over the lease's maximum.
func (t *leaseTable) renew(id string, ttlSec int) (Lease, *Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.byID[id]
	if e == nil || !e.liveAt(now) {
		return Lease{}, Errorf(CodeLeaseExpired, "lease %s has expired or was released", id).WithDetails("lease", id)
	}
	ttl := e.ttl
	if ttlSec != 0 {
		var we *Error
		if ttl, we = leaseWindow(e.device, ttlSec, e.max); we != nil {
			return Lease{}, we
		}
	}
	e.ttl = ttl
	e.expires = now.Add(ttl)
	return e.view(), nil
}

// overMax is the USAGE error for a window over a device's maximum.
func overMax(device string, ttlSec int, maxTTL time.Duration) *Error {
	return Errorf(CodeUsage, "leases: a window of ttlSec %d is over device %s's maximum of %s (maxSec %d)",
		ttlSec, device, maxTTL, int(maxTTL/time.Second)).
		WithDetails("maxSec", int(maxTTL/time.Second), "ttlSec", ttlSec)
}

// grant leases device to holder, unless a lease holds it. A lease that
// expired but has not ended yet still holds it: only endLeaseLocked drops
// a lease, because only it cancels the lease's requests, detaches the
// device and shuts down one the lease booted. The caller holds
// Server.leaseMu, which serialises grants with the sweeper.
func (t *leaseTable) grant(device, avd, holder string, ttl, maxTTL time.Duration, booted bool) (*leaseEntry, *Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if e := t.byDevice[device]; e != nil {
		if !e.liveAt(now) {
			return nil, Errorf(CodeDeviceInUse, "device %s is being released (its lease expired); try again shortly", device)
		}
		return nil, Errorf(CodeDeviceInUse, "device %s is leased by %s until %s", device, HolderName(e.holder), e.expires.UTC().Format(time.RFC3339)).
			WithDetails("holder", e.holder, "expiresAt", e.expires.UTC().Format(time.RFC3339))
	}
	e := &leaseEntry{id: uuid.New().String(), device: device, holder: holder, ttl: ttl, max: maxTTL, expires: now.Add(ttl),
		booted: booted, avd: avd, ended: make(chan struct{})}
	t.byID[e.id] = e
	t.byDevice[device] = e
	return e, nil
}

// remove drops e; false when it was already gone.
func (t *leaseTable) remove(e *leaseEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID[e.id] != e {
		return false
	}
	delete(t.byID, e.id)
	if t.byDevice[e.device] == e {
		delete(t.byDevice, e.device)
	}
	close(e.ended)
	return true
}

// lookup returns the entry for id, live or not.
func (t *leaseTable) lookup(id string) *leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

// expired returns the entries whose time has run out.
func (t *leaseTable) expired() []*leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []*leaseEntry
	for _, e := range t.byID {
		if !e.liveAt(now) {
			out = append(out, e)
		}
	}
	return out
}

// expiredOn returns the expired, not yet ended lease on device, if any.
func (t *leaseTable) expiredOn(device string) *leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byDevice[device]; e != nil && !e.liveAt(t.now()) {
		return e
	}
	return nil
}

// use checks a request's lease header against the device's lease. need is
// false for a route that needs a lease only when the device is leased
// (stopping or deleting a device nobody holds). On success it returns the
// entry the request runs under, if any; using it never moves its expiry.
func (t *leaseTable) use(device, header string, need bool) (*leaseEntry, *Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cur := t.byDevice[device]
	if cur != nil && !cur.liveAt(now) {
		cur = nil
	}
	if header == "" {
		if cur != nil {
			return nil, Errorf(CodeLeaseRequired, "device %s is leased by %s until %s; send its lease in %s", device,
				HolderName(cur.holder), cur.expires.UTC().Format(time.RFC3339), HeaderLease).
				WithDetails("holder", cur.holder, "expiresAt", cur.expires.UTC().Format(time.RFC3339))
		}
		if need {
			return nil, Errorf(CodeLeaseRequired, "device %s needs a lease on this hub, and clients never take one on their own: "+
				"run `maestro-d lease --device %s` (or md.lease() in JavaScript, POST /v1/leases over REST) and send it in %s",
				device, device, HeaderLease).WithDetails("hint", "maestro-d lease --device "+device)
		}
		return nil, nil
	}
	if cur != nil && cur.id == header {
		return cur, nil
	}
	if other := t.byID[header]; other != nil && other.liveAt(now) {
		return nil, Errorf(CodeLeaseRequired, "lease %s is for device %s, not %s", header, other.device, device).
			WithDetails("leaseDevice", other.device)
	}
	return nil, Errorf(CodeLeaseExpired, "lease %s on device %s has expired or was released; take a new one", header, device).
		WithDetails("lease", header)
}

// underLease returns r with a context that is cancelled when lease e
// ends: at the end of its window the request stops (a running step
// reports INTERRUPTED). The context also carries e, so a wrapper that
// detaches the request from its client (idempotent) still ends with the
// lease, and the client's own context, so a wrapper can tell the lease
// ending from the client going (clientContext). stop releases the watch.
func underLease(r *http.Request, e *leaseEntry) (*http.Request, func()) {
	r = withLease(r, e)
	ctx, cancel := untilLeaseEnds(r.Context(), e)
	return r.WithContext(ctx), cancel
}

// withLease returns r with lease e on its context (leaseFrom), without
// tying the context to the lease's end.
func withLease(r *http.Request, e *leaseEntry) *http.Request {
	return withLeaseWork(r, e, nil)
}

// withLeaseWork is withLease for a request counted as work on e:
// doneWork ends that count, once, and a handler that ends e itself
// (endStoppedLease) calls it first, so the teardown does not wait for the
// request that runs it.
func withLeaseWork(r *http.Request, e *leaseEntry, doneWork func()) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), leaseCtxKey{}, leaseCtx{e: e, client: r.Context(), doneWork: doneWork}))
}

// leaseCtxKey carries the lease a request runs under in its context.
type leaseCtxKey struct{}

// leaseCtx is the value under leaseCtxKey: the lease, and the request's
// context before underLease tied it to the lease.
type leaseCtx struct {
	e        *leaseEntry
	client   context.Context
	doneWork func()
}

// leaseFrom returns the lease a request runs under (set by underLease), if any.
func leaseFrom(ctx context.Context) *leaseEntry {
	v, _ := ctx.Value(leaseCtxKey{}).(leaseCtx)
	return v.e
}

// clientContext is ctx without the lease's end: the context of the
// client's request, which ends only when the client goes (or, over TCP
// with a request id, after the grace). ctx itself when no lease is on it.
func clientContext(ctx context.Context) context.Context {
	if v, ok := ctx.Value(leaseCtxKey{}).(leaseCtx); ok {
		return v.client
	}
	return ctx
}

// untilLeaseEnds returns a child of ctx that is cancelled as soon as lease
// e ends. cancel releases the watch.
func untilLeaseEnds(ctx context.Context, e *leaseEntry) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-e.ended:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// HolderName is how a lease's holder reads in messages and listings:
// the holder, or "another client" when it gave none.
func HolderName(h string) string {
	if h == "" {
		return "another client"
	}
	return h
}

// ---------------------------------------------------------------------------
// Sharing

// shareMatches reports whether a --share-device entry names the device, by
// id or by name (a running emulator's name is its AVD name).
func shareMatches(entry string, d DeviceInfo) bool {
	return entry == d.ID || (d.Name != "" && strings.EqualFold(entry, d.Name)) ||
		(strings.HasPrefix(d.ID, AVDPrefix) && strings.EqualFold(entry, strings.TrimPrefix(d.ID, AVDPrefix)))
}

// sharedInfo reports whether a listed device is in the hub's pool.
func (s *Server) sharedInfo(d DeviceInfo) bool {
	if len(s.cfg.ShareDevices) == 0 {
		return true
	}
	for _, entry := range s.cfg.ShareDevices {
		if shareMatches(entry, d) {
			return true
		}
	}
	return false
}

// shareCacheFor is how long shared remembers an answer it may not keep:
// an emulator serial's (emulator-5554 is whichever AVD booted on that port
// last) and a negative one. A device booted, stopped, created or deleted
// here, or a lease ending, forgets them at once.
const shareCacheFor = 30 * time.Second

type shareAnswer struct {
	ok bool
	at time.Time
}

// shared reports whether device id is in the hub's pool. An id that is not
// listed (a device discovery cannot see) is shared only when it is named
// itself. Positive answers are remembered for stable ids, whose device's
// name never changes; an emulator serial's answer, and any negative one,
// for shareCacheFor. So a run of steps, and the event stream that reports
// them to a remote caller, look the device up once, not per request and
// per event: a lookup runs live discovery (adb, emulator, simctl).
func (s *Server) shared(id string) bool {
	if len(s.cfg.ShareDevices) == 0 {
		return true
	}
	stable := !isEmulatorSerial(id)
	now := s.leases.now()
	s.shareMu.Lock()
	if stable && s.sharedIDs[id] {
		s.shareMu.Unlock()
		return true
	}
	if a, ok := s.shareSeen[id]; ok && now.Sub(a.at) < shareCacheFor && !now.Before(a.at) {
		s.shareMu.Unlock()
		return a.ok
	}
	gen := s.shareGen
	s.shareMu.Unlock()
	info, found := s.registry.Lookup(id)
	if !found {
		info = DeviceInfo{ID: id}
	}
	ok := s.sharedInfo(info)
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	switch {
	case gen != s.shareGen:
		// Forgotten while looking: this answer may predate the change.
	case ok && stable:
		s.sharedIDs[id] = true
	default:
		s.shareSeen[id] = shareAnswer{ok: ok, at: now}
	}
	return ok
}

// forgetShared drops the answers shared may not keep, when a device's
// state changes here: a booted, stopped, created or deleted device, or an
// ended lease (its device may be stopped, and another AVD boot on the
// serial).
func (s *Server) forgetShared(ev Event) {
	switch {
	case ev.Type == "device.booted", ev.Type == "device.stopped", ev.Type == "device.created", ev.Type == "device.deleted",
		strings.HasPrefix(ev.Type, "lease.") && ev.Type != "lease.granted":
		s.shareMu.Lock()
		s.shareGen++
		clear(s.shareSeen)
		s.shareMu.Unlock()
	}
}

// isEmulatorSerial reports whether id is an Android emulator's adb serial
// (emulator-<port>), which the next AVD to boot on that port reuses.
func isEmulatorSerial(id string) bool {
	return strings.HasPrefix(id, "emulator-")
}

// sharedWith reports whether the request's caller may see device id: a
// remote caller sees only the shared pool.
func (s *Server) sharedWith(r *http.Request, id string) bool {
	return !CallerFrom(r.Context()).Remote || s.shared(id)
}

// handleEvents serves GET /v1/events. A remote caller sees only the shared
// pool's events: a ?device= naming another device is not found, other
// devices' events are left out, and so is any event that names no device
// but the daemon's shutdown. A device's attach event carries the host's
// report directory, which a remote caller does not get either.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !CallerFrom(r.Context()).Remote {
		s.events.ServeSSE(w, r)
		return
	}
	if id := r.URL.Query().Get("device"); id != "" && !s.shared(id) {
		writeError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
		return
	}
	s.events.ServeSSEView(w, r, s.remoteEvent)
}

// remoteEvent is a remote caller's view of an event.
func (s *Server) remoteEvent(ev Event) (Event, bool) {
	switch {
	case ev.Device == "":
		return ev, ev.Type == "daemon.shutdown"
	case !s.shared(ev.Device):
		return ev, false
	case ev.Type == "device.attached":
		ev.Message = ""
	}
	return ev, true
}

// visible filters and annotates a device list for the request's caller:
// a remote caller sees only the shared pool, and on a hub every entry
// carries its lease state.
func (s *Server) visible(r *http.Request, devs []DeviceInfo) []DeviceInfo {
	out := make([]DeviceInfo, 0, len(devs))
	remote := CallerFrom(r.Context()).Remote
	for _, d := range devs {
		if remote && !s.sharedInfo(d) {
			continue
		}
		out = append(out, d)
	}
	if s.cfg.Hub {
		s.annotate(out)
	}
	return out
}

// annotate sets lease, maxSec, available and owner on a hub's device
// entries.
func (s *Server) annotate(devs []DeviceInfo) {
	owners := otherOwners(s.cfg.Name)
	for i := range devs {
		d := &devs[i]
		if o := owners[d.ID]; o != "" && !d.Attached {
			d.Owner = o
		}
		d.Lease = nil
		if l, ok := s.leases.liveFor(d.ID); ok {
			d.Lease = &DeviceLease{Holder: l.Holder, ExpiresAt: l.ExpiresAt}
		}
		d.MaxSec = int(s.leaseMaxInfo(*d) / time.Second)
		d.Available = d.Ready && d.Lease == nil && !d.Attached && d.Owner == "" && !strings.HasPrefix(d.ID, AVDPrefix)
		if d.Available && s.leases.reserved(*d) {
			// Booting for a selector lease, or still being released.
			d.Available = false
		}
	}
}

// otherOwners maps each device another live daemon has attached to that
// daemon's name.
func otherOwners(self string) map[string]string {
	out := map[string]string{}
	daemons, _ := ListDaemons()
	for _, d := range daemons {
		if d.Name == self || !d.Alive {
			continue
		}
		for _, dev := range d.Devices {
			if dev.Attached {
				out[dev.ID] = d.Name
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Route guard

type leaseMode int

const (
	// leaseOpen: GET /v1/devices/{id} and …/info stay open to every
	// authenticated client.
	leaseOpen leaseMode = iota
	// leaseNeeded: every route that drives or changes the device.
	leaseNeeded
	// leaseIfHeld: stopping or deleting a device needs its lease only when
	// someone holds one.
	leaseIfHeld
)

// dev guards a /v1/devices/{id} route: an unshared device is not found for
// a remote caller, and on a hub the request must carry the device's lease.
func (s *Server) dev(mode leaseMode, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		target := DeviceInfo{ID: id}
		if mode == leaseIfHeld && (s.cfg.Hub || CallerFrom(r.Context()).Remote) {
			target = s.stopTarget(id)
			id = target.ID
		}
		if !s.sharedWith(r, id) {
			writeError(w, Errorf(CodeDeviceNotFound, "device %s not found", id))
			return
		}
		if !s.cfg.Hub || mode == leaseOpen {
			h(w, r)
			return
		}
		e, le := s.leases.use(id, r.Header.Get(HeaderLease), mode == leaseNeeded)
		if le != nil {
			writeError(w, le)
			return
		}
		if e == nil && mode == leaseIfHeld && s.leases.releasing(target) {
			// The teardown of the device's ended lease may be shutting
			// it down, or a selector lease booting it (an emulator by its
			// AVD too): a stop or delete beside it would race it.
			writeError(w, beingReleasedErr(id))
			return
		}
		if e == nil {
			h(w, r)
			return
		}
		if mode == leaseIfHeld {
			// Stopping or deleting the device is work under its lease, so
			// a teardown that ends the lease meanwhile waits for it, and
			// does not shut the device down again once it has
			// (leaseEntry.stopped). It is not cancelled when the lease
			// ends: a stop under way is answered for as it went. The
			// handler ends the lease itself after the stop or delete
			// (endStoppedLease), leaving its own work out first, so the
			// teardown is done before it answers.
			if !s.leases.startWork(e) {
				writeError(w, leaseExpiredErr(e))
				return
			}
			var once sync.Once
			done := func() { once.Do(e.doneWork) }
			defer done()
			h(w, withLeaseWork(r, e, done))
			return
		}
		s.serveLeased(w, r, e, h)
	}
}

// leaseUnwindGrace is how long a request under a lease that ended gets to
// unwind and answer for itself (a step reports INTERRUPTED) before
// serveLeased answers LEASE_EXPIRED for it.
var leaseUnwindGrace = time.Second

// serveLeased runs h under lease e, the way attachLeased runs an attach:
// counted on the lease, so the lease's teardown waits for it before it
// detaches the device and frees it, and beside the request, so the
// request answers promptly when the lease ends. Its context ends with the
// lease (underLease); a driver call that ignores that (a WebDriverAgent
// HTTP call runs to its own timeout) keeps h running, and once the grace
// is up the request answers LEASE_EXPIRED without it. h's writes and body
// reads go through a gate that is shut then, so nothing it does later
// touches the finished request; until h has returned the device is not
// available and a new lease on it is DEVICE_IN_USE ("being released").
func (s *Server) serveLeased(w http.ResponseWriter, r *http.Request, e *leaseEntry, h http.HandlerFunc) {
	if !s.leases.startWork(e) {
		writeError(w, leaseExpiredErr(e))
		return
	}
	lr, stop := underLease(r, e)
	g := &leaseGate{w: w, header: http.Header{}}
	var body *gatedBody
	if lr.Body != nil {
		body = &gatedBody{g: g, rc: lr.Body}
		lr.Body = body
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if p := recover(); p != nil {
				logger.Warn("%s %s under lease %s panicked: %v", r.Method, r.URL.Path, e.id, p)
				writeError(g, Errorf(CodeInternal, "internal error: %v", p))
			}
			stop()
			e.doneWork()
			close(done)
		}()
		h(g, lr)
	}()
	select {
	case <-done:
		return
	case <-e.ended:
	}
	t := time.NewTimer(leaseUnwindGrace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	g.mu.Lock()
	g.closed = true
	started := g.started
	g.mu.Unlock()
	logger.Info("lease %s: %s %s still runs after the lease ended; answered without it, the device stays reserved until it unwinds", e.id, r.Method, r.URL.Path)
	rc := http.NewResponseController(w)
	if body != nil {
		// h may still be reading the request body (a stalled upload): that
		// read must not outlive this handler, so it is ended now.
		defer body.unblock(rc)
	}
	if !started {
		// In full duplex the answer does not wait for that read either.
		_ = rc.EnableFullDuplex()
		writeError(w, Errorf(CodeLeaseExpired, "lease %s on device %s ended while the request ran; take a new one", e.id, e.device).
			WithDetails("lease", e.id))
	}
}

// leaseGate is the ResponseWriter serveLeased hands its handler: it writes
// through to the request's writer until serveLeased answers without the
// handler (closed), and drops every write after that. Each write holds mu,
// so none runs once serveLeased has returned. A write that blocks (a
// client that stopped reading) holds serveLeased up past the grace with
// it, but that request has begun its answer: it could not answer
// LEASE_EXPIRED anyway. Body reads do not hold mu (gatedBody), so a
// stalled upload never keeps serveLeased from answering, and serveLeased
// ends a read still pending before it returns (gatedBody.unblock).
type leaseGate struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	header  http.Header
	started bool
	closed  bool
}

func (g *leaseGate) Header() http.Header { return g.header }

// startLocked sends the handler's headers on its first write.
func (g *leaseGate) startLocked() {
	if !g.started {
		g.started = true
		dst := g.w.Header()
		for k, v := range g.header {
			dst[k] = v
		}
	}
}

func (g *leaseGate) WriteHeader(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.started {
		return
	}
	g.startLocked()
	g.w.WriteHeader(status)
}

func (g *leaseGate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, http.ErrHandlerTimeout
	}
	g.startLocked()
	return g.w.Write(p)
}

// gatedBody is the request body behind a leaseGate: unread once closed.
// reading, while set, is closed when the Read in progress returns.
type gatedBody struct {
	g       *leaseGate
	rc      io.ReadCloser
	reading chan struct{}
}

// Read checks the gate under mu but reads outside it, so the answer never
// waits for a stalled read; unblock ends one still pending once the gate
// is closed.
func (b *gatedBody) Read(p []byte) (int, error) {
	b.g.mu.Lock()
	if b.g.closed {
		b.g.mu.Unlock()
		return 0, http.ErrHandlerTimeout
	}
	done := make(chan struct{})
	b.reading = done
	b.g.mu.Unlock()
	defer close(done)
	return b.rc.Read(p)
}

// bodyUnblockWait bounds how long unblock waits for the pending read to
// return.
var bodyUnblockWait = time.Second

// unblock ends a Read of the request body still pending after the gate
// closed: net/http forbids reading the body once the handler returned,
// and the read would hold the connection until the client sends or goes.
// A read deadline ends it on a connection that has one; a body without
// (a request a peer hub relays) is closed instead.
func (b *gatedBody) unblock(rc *http.ResponseController) {
	b.g.mu.Lock()
	done := b.reading
	b.g.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	if err := rc.SetReadDeadline(time.Now()); err != nil {
		_ = b.rc.Close()
	}
	t := time.NewTimer(bodyUnblockWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		logger.Warn("a request body read under an ended lease did not return within %v", bodyUnblockWait)
	}
}

func (b *gatedBody) Close() error {
	if b.g.isClosed() {
		return nil
	}
	return b.rc.Close()
}

func (g *leaseGate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// stopTarget is the device DELETE /v1/devices/{id} acts on, for its share
// and lease checks: `device delete` takes a simulator or AVD by name
// (Registry.Delete resolves it), so a name is checked as the device it
// names, never as an id nobody leases. An id that names no device, or
// several, is checked as given; the handler reports it. The device comes
// back as listed, so its reservations are checked by its AVD too.
func (s *Server) stopTarget(id string) DeviceInfo {
	if info, e := s.registry.resolveManaged(id); e == nil {
		return info
	}
	return DeviceInfo{ID: id}
}

// ---------------------------------------------------------------------------
// Handlers

func (s *Server) notHub(w http.ResponseWriter) bool {
	if s.cfg.Hub {
		return false
	}
	writeError(w, Errorf(CodeUsage, "daemon %q is not a hub and has no leases (a hub is started with `maestro-d hub start`)", s.cfg.Name))
	return true
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	var req LeaseRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if (req.Device == "") == (req.Select == nil) {
		writeError(w, Errorf(CodeUsage, "leases: give device or select, not both or neither"))
		return
	}
	if req.TTLSec < 0 {
		writeError(w, Errorf(CodeUsage, "leases: ttlSec must be positive"))
		return
	}
	caller := CallerFrom(r.Context())
	if req.Attach != nil && caller.Remote {
		if e := checkRemoteAttach(req.Attach); e != nil {
			s.logForbidden(r, e)
			writeError(w, e)
			return
		}
	}
	holder := strings.TrimSpace(req.Holder)
	if holder == "" {
		holder = describeCaller(caller)
	}
	var entry *leaseEntry
	var e *Error
	if req.Device != "" {
		if !s.sharedWith(r, req.Device) {
			writeError(w, Errorf(CodeDeviceNotFound, "device %s not found", req.Device))
			return
		}
		// A shut-down AVD is never driven under its avd: id: booted, it is
		// emulator-<port>, which such a lease would not cover.
		if avd, ok := strings.CutPrefix(req.Device, AVDPrefix); ok {
			writeError(w, Errorf(CodeUsage, "leases: %s is a shut-down AVD, which cannot be leased by id: "+
				"run `maestro-d lease --name %s --boot` to have the hub boot it for the lease, "+
				"or `maestro-d device start %s` and lease the emulator it becomes", req.Device, avd, req.Device).
				WithDetails("hint", "maestro-d lease --name "+avd+" --boot"))
			return
		}
		// A hub leases only a device it lists: an id nothing here can see
		// would get a lease no driver can use.
		d, listed := s.registry.Lookup(req.Device)
		if !listed {
			writeError(w, Errorf(CodeDeviceNotFound, "device %s is not listed on hub %s", req.Device, s.hubName()))
			return
		}
		entry, e = s.leaseDevice(d, holder, req.TTLSec, claimNone)
	} else {
		entry, e = s.leaseSelect(r, *req.Select, holder, req.TTLSec)
	}
	if e != nil {
		writeError(w, e)
		return
	}
	logger.Info("lease %s: %s leased by %s for %v", entry.id, entry.device, holder, entry.ttl)
	s.events.Publish(Event{Type: "lease.granted", Device: entry.device, Message: holder})
	var info *DeviceInfo
	if req.Attach != nil {
		// The attach runs under the lease, like any request carrying it:
		// the window ending answers LEASE_EXPIRED at once and cancels the
		// attach, and the lease's teardown waits for it to unwind before
		// it detaches the device and frees it (attachLeased).
		ar, stop := underLease(r, entry)
		cfg := *req.Attach
		a, ae := s.attachLeased(ar.Context(), entry.device, &cfg)
		stop()
		if ae != nil {
			s.endLease(entry, endAttachFailed)
			writeError(w, ae)
			return
		}
		d := s.deviceInfo(a)
		info = &d
	} else if d, ok := s.registry.Lookup(entry.device); ok {
		info = &d
	}
	if info != nil {
		devs := []DeviceInfo{*info}
		s.annotate(devs)
		info = &devs[0]
	}
	l, _ := s.leases.get(entry.id)
	if l.ID == "" {
		l = entry.view()
	}
	writeJSON(w, http.StatusOK, LeaseResult{Envelope: Envelope{OK: true}, Lease: l, Device: info})
}

// attachLeased is ensureAttached for a request: under a lease (ctx from
// underLease), the attach is counted on the lease and runs beside the
// request, which answers LEASE_EXPIRED as soon as the lease ends, without
// waiting for it. The attach's context ends with the lease, which
// cancels it as far as the driver allows (attachDevice discards a driver
// that started after it was cancelled); until it has unwound, the lease's
// teardown keeps the device reserved (not available, DEVICE_IN_USE to a
// new lease), and then detaches whatever the attach left attached. So a
// late attach never touches a later holder's session.
func (s *Server) attachLeased(ctx context.Context, id string, cfg *AttachConfig) (*attached, *Error) {
	e := leaseFrom(ctx)
	if e == nil {
		return s.ensureAttached(ctx, id, cfg)
	}
	ended := func() *Error {
		return Errorf(CodeLeaseExpired, "lease %s on device %s ended while the device attached; take a new one", e.id, e.device).
			WithDetails("lease", e.id)
	}
	if !s.leases.startWork(e) {
		return nil, ended()
	}
	type result struct {
		a   *attached
		err *Error
	}
	done := make(chan result, 1)
	go func() {
		a, ae := s.ensureAttached(ctx, id, cfg)
		e.doneWork()
		done <- result{a, ae}
	}()
	select {
	case res := <-done:
		if _, live := s.leases.get(e.id); !live {
			// The window ran out as the attach returned: the lease's
			// teardown detaches the device.
			return nil, ended()
		}
		return res.a, res.err
	case <-e.ended:
		logger.Info("lease %s: %s ended while its attach ran; answered LEASE_EXPIRED, the device stays reserved until the attach unwinds", e.id, e.device)
		return nil, ended()
	}
}

// bootClaim is what a lease request holds on a device a selector lease
// reserved for booting: nothing (the reservation refuses it), the
// reservation only (the device was running by the time Boot ran), or the
// reservation and a boot this hub did, so the lease's end shuts it down.
type bootClaim uint8

const (
	claimNone bootClaim = iota
	claimReserved
	claimBooted
)

// leaseDevice grants a lease on one listed device d: not while another
// lease holds it, nor while another daemon on this machine has it attached.
// An expired lease on the device that the sweeper has not ended yet ends
// first (its teardown runs outside leaseMu), so the new holder gets a
// fresh session. When that lease had booted the device, its teardown shut
// it down, so the device is looked up again: one no longer listed (an
// emulator's serial) is DEVICE_NOT_FOUND.
func (s *Server) leaseDevice(d DeviceInfo, holder string, ttlSec int, claim bootClaim) (*leaseEntry, *Error) {
	device := d.ID
	for range 3 {
		s.leaseMu.Lock()
		old := s.leases.expiredOn(device)
		if old == nil {
			defer s.leaseMu.Unlock()
			return s.leaseDeviceLocked(d, holder, ttlSec, claim)
		}
		teardown := s.endLeaseLocked(old, endExpired)
		s.leaseMu.Unlock()
		teardown()
		if old.booted {
			// The teardown shut the device down: d is stale, and an
			// emulator's serial is gone. Grant only on what is listed now.
			var listed bool
			if d, listed = s.registry.Lookup(device); !listed {
				return nil, Errorf(CodeDeviceNotFound, "device %s was shut down as its expired lease ended; it is no longer listed on hub %s", device, s.hubName())
			}
		}
	}
	return nil, Errorf(CodeDeviceInUse, "device %s: its expired lease is still ending; try again", device)
}

// leaseDeviceLocked grants the lease with Server.leaseMu held: a window
// of ttlSec seconds (0: the default, capped at the device's maximum), refused USAGE
// over the maximum. A device whose expired lease has not ended yet (a
// selector's candidate) is not granted: that lease is ended in the
// background and the device is DEVICE_IN_USE until its teardown is done.
// d is the device as the caller listed it: nothing here runs discovery,
// which would hold leaseMu, and every other lease call, for seconds.
func (s *Server) leaseDeviceLocked(d DeviceInfo, holder string, ttlSec int, claim bootClaim) (*leaseEntry, *Error) {
	device := d.ID
	maxTTL := s.leaseMaxInfo(d)
	window, we := leaseWindow(device, ttlSec, maxTTL)
	if we != nil {
		return nil, we
	}
	// A running emulator is reserved under its AVD too (avdKey): a
	// selector booting that AVD, or a teardown shutting it down.
	k := avdKey(d)
	s.leases.mu.Lock()
	reserved := claim == claimNone && (s.leases.booting[device] || k != "" && s.leases.booting[k])
	ending := s.leases.ending[device] || k != "" && s.leases.ending[k]
	s.leases.mu.Unlock()
	if reserved {
		return nil, Errorf(CodeDeviceInUse, "device %s is booting for another lease", device)
	}
	if ending {
		return nil, beingReleasedErr(device)
	}
	if owner := FindDeviceOwner(device, s.cfg.Name); owner != nil {
		e := Errorf(CodeDeviceInUse, "device %s is attached to daemon %q (pid %d)", device, owner.Name, owner.Info.PID)
		return nil, e.WithDetails("owner", owner.Name, "ownerPid", owner.Info.PID)
	}
	e, ge := s.leases.grant(device, avdKey(d), holder, window, maxTTL, claim == claimBooted)
	if ge != nil {
		// A lease that expired before grant (it may have been live when
		// leaseDevice looked) is ended in the background, under leaseMu.
		if old := s.leases.expiredOn(device); old != nil {
			go s.endLeaseLocked(old, endExpired)()
		}
		return nil, ge
	}
	return e, nil
}

// leaseWindow is the window a lease request gets: ttlSec seconds, or with
// none the default capped at the device's maximum; over the maximum is
// USAGE. The seconds are compared before they become a Duration, which a
// huge ttlSec would overflow into an already-expired window.
func leaseWindow(device string, ttlSec int, maxTTL time.Duration) (time.Duration, *Error) {
	if ttlSec == 0 {
		return min(DefaultLeaseTTL, maxTTL), nil
	}
	if int64(ttlSec) > int64(maxTTL/time.Second) {
		return 0, overMax(device, ttlSec, maxTTL)
	}
	return time.Duration(ttlSec) * time.Second, nil
}

// leaseMaxInfo is the longest window a lease on listed device d may have:
// the first --lease-max DEVICE=DURATION that names it (by id or name),
// else the hub's --lease-max, else DefaultLeaseMax.
func (s *Server) leaseMaxInfo(d DeviceInfo) time.Duration {
	for _, m := range s.cfg.LeaseMaxDevices {
		if shareMatches(m.Device, d) {
			return m.Max
		}
	}
	return s.hubLeaseMax()
}

// leaseMaxNote lists the per-device maximums for the hub's start line.
func leaseMaxNote(devs []LeaseMax) string {
	var b strings.Builder
	for _, m := range devs {
		fmt.Fprintf(&b, ", %s %v", m.Device, m.Max)
	}
	return b.String()
}

func (s *Server) hubLeaseMax() time.Duration {
	if s.cfg.LeaseMax > 0 {
		return s.cfg.LeaseMax
	}
	return DefaultLeaseMax
}

// leaseSelect leases the first free device that matches sel: a ready one,
// neither leased nor attached; with sel.Boot, else a shut-down simulator or
// AVD, which the hub boots for the lease.
func (s *Server) leaseSelect(r *http.Request, sel LeaseSelector, holder string, ttlSec int) (*leaseEntry, *Error) {
	notFound := Errorf(CodeDeviceNotFound, "no device matches the selector %s", sel.String()).WithDetails("select", sel)
	if sel.Hub != "" && !strings.EqualFold(sel.Hub, s.hubName()) {
		return nil, notFound
	}
	var matches []DeviceInfo
	for _, d := range s.visible(r, s.registry.List(strings.ToLower(sel.Platform))) {
		if sel.matches(d) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 0 {
		return nil, notFound
	}
	// A candidate whose maximum window is shorter than ttlSec is skipped; when
	// that leaves none, the answer is USAGE naming the maximum.
	var tooLong *Error
	s.leaseMu.Lock()
	for _, d := range matches {
		if old := s.leases.expiredOn(d.ID); old != nil {
			// Expired but not swept yet: listed unavailable, so it is
			// passed over, and ended in the background now.
			go s.endLeaseLocked(old, endExpired)()
		}
		s.leases.mu.Lock()
		reserved := s.leases.reservedLocked(d)
		s.leases.mu.Unlock()
		if !d.Available || reserved {
			continue
		}
		e, le := s.leaseDeviceLocked(d, holder, ttlSec, claimNone)
		if le == nil {
			s.leaseMu.Unlock()
			return e, nil
		}
		if le.Code == CodeUsage {
			tooLong = le
		}
	}
	var boot *DeviceInfo
	if sel.Boot {
		s.leases.mu.Lock()
		for i := range matches {
			d := &matches[i]
			// A device whose lease is still being torn down may already be
			// listed shut down; it is not free until the teardown is done.
			if !bootable(*d) || d.Lease != nil || s.leases.booting[bootKey(*d)] ||
				s.leases.ending[d.ID] || s.leases.ending[bootKey(*d)] {
				continue
			}
			if _, we := leaseWindow(d.ID, ttlSec, s.leaseMaxInfo(*d)); we != nil {
				tooLong = we
				continue
			}
			s.leases.booting[bootKey(*d)] = true
			boot = d
			break
		}
		s.leases.mu.Unlock()
	}
	s.leaseMu.Unlock()
	if boot == nil && tooLong != nil {
		return nil, tooLong
	}
	if boot == nil {
		ids := make([]string, 0, len(matches))
		for _, d := range matches {
			ids = append(ids, d.ID)
		}
		return nil, Errorf(CodeDeviceInUse, "every device matching %s is leased, attached or not ready", sel.String()).
			WithDetails("candidates", ids)
	}
	// The reservation stands until the booted device's lease is granted
	// (or the boot failed), so no other selector takes the device in the
	// moment it is listed ready.
	defer func() {
		s.leases.mu.Lock()
		delete(s.leases.booting, bootKey(*boot))
		s.leases.mu.Unlock()
	}()
	logger.Info("lease: booting %s (%s) for a selector lease", boot.ID, boot.Name)
	// The boot runs to its end even when the client goes: Registry.Boot
	// cancelled mid-boot returns at once and leaves the device booting,
	// unrecorded, so it would end up running with no lease and as nobody's
	// to shut down. A client gone by the end gets no lease, and the device
	// is shut down again.
	s.idle.Hold()
	info, err := s.registry.Boot(context.WithoutCancel(r.Context()), BootRequest{Platform: boot.Platform, Name: boot.ID})
	s.idle.Release()
	if err != nil {
		return nil, WrapErr(CodeDeviceError, err)
	}
	if r.Context().Err() != nil {
		logger.Info("lease: the client left while %s booted for its lease", info.ID)
		s.disownBoot(info)
		return nil, Errorf(CodeInterrupted, "lease: cancelled while booting %s", boot.ID)
	}
	// Boot returns a device someone else started between the selection and
	// this call as it is: the lease shuts down only a device this daemon
	// booted.
	claim := claimReserved
	if info.BootedBy == BootedByDaemon {
		claim = claimBooted
		s.events.Publish(Event{Type: "device.booted", Device: info.ID, Message: boot.Name})
	} else {
		logger.Info("lease: %s was started meanwhile by someone else; its lease will not shut it down", info.ID)
	}
	e, le := s.leaseDevice(info, holder, ttlSec, claim)
	if le != nil {
		s.disownBoot(info)
		return nil, le
	}
	return e, nil
}

// disownBoot handles a device a selector lease booted but could not lease
// (a client leased its new serial by id meanwhile, or the client left
// during the boot): the lease that holds
// it shuts it down when it ends, and with no lease on it, it is shut down
// now, unless another daemon has it attached.
func (s *Server) disownBoot(info DeviceInfo) {
	if info.BootedBy != BootedByDaemon {
		return
	}
	if s.leases.markBooted(info.ID) {
		logger.Info("lease: %s, booted for a selector lease, went to another lease; that lease shuts it down", info.ID)
		return
	}
	if FindDeviceOwner(info.ID, s.cfg.Name) != nil {
		return
	}
	if err := s.registry.Stop(info.ID, false); err != nil {
		logger.Warn("lease: shut down %s, booted for a lease that failed: %v", info.ID, err)
	}
}

// bootable reports whether a listed device is a shut-down simulator or AVD.
func bootable(d DeviceInfo) bool {
	if d.Ready {
		return false
	}
	return strings.HasPrefix(d.ID, AVDPrefix) || (d.Platform == "ios" && d.Kind == "simulator" && strings.EqualFold(d.State, "Shutdown"))
}

func (sel LeaseSelector) matches(d DeviceInfo) bool {
	if sel.Platform != "" && !strings.EqualFold(sel.Platform, d.Platform) {
		return false
	}
	if sel.Kind != "" && !strings.EqualFold(sel.Kind, d.Kind) {
		return false
	}
	if sel.Name != "" && sel.Name != d.ID && !strings.EqualFold(sel.Name, d.Name) {
		return false
	}
	if sel.OSVersion != "" && d.OSVersion != sel.OSVersion && !strings.HasPrefix(d.OSVersion, sel.OSVersion+".") {
		return false
	}
	return true
}

// String renders the selector for messages: {platform=ios kind=simulator}.
func (sel LeaseSelector) String() string {
	var parts []string
	for _, kv := range [][2]string{{"platform", sel.Platform}, {"kind", sel.Kind}, {"name", sel.Name}, {"osVersion", sel.OSVersion}, {"hub", sel.Hub}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if sel.Boot {
		parts = append(parts, "boot=true")
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func (s *Server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	leases := s.leases.list()
	if CallerFrom(r.Context()).Remote {
		shared := leases[:0]
		for _, l := range leases {
			if s.shared(l.Device) {
				shared = append(shared, l)
			}
		}
		leases = shared
	}
	writeJSON(w, http.StatusOK, LeasesResult{Envelope: Envelope{OK: true}, Leases: leases})
}

// visibleLease returns lease id's entry when the caller may see its
// device: a remote caller sees only the shared pool's leases, and any
// other is to it as a lease that does not exist.
func (s *Server) visibleLease(r *http.Request, id string) *leaseEntry {
	e := s.leases.lookup(id)
	if e == nil || !s.sharedWith(r, e.device) {
		return nil
	}
	return e
}

func (s *Server) leaseGone(w http.ResponseWriter, id string) {
	writeError(w, Errorf(CodeLeaseExpired, "lease %s has expired or was released", id).WithDetails("lease", id))
}

func (s *Server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	id := r.PathValue("lid")
	l, ok := s.leases.get(id)
	if !ok || s.visibleLease(r, id) == nil {
		s.leaseGone(w, id)
		return
	}
	writeJSON(w, http.StatusOK, LeaseResult{Envelope: Envelope{OK: true}, Lease: l})
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	id := r.PathValue("lid")
	var req RenewRequest
	if e := decodeBody(r, &req); e != nil {
		writeError(w, e)
		return
	}
	if req.TTLSec < 0 {
		writeError(w, Errorf(CodeUsage, "leases: ttlSec must be positive"))
		return
	}
	if s.visibleLease(r, id) == nil {
		s.leaseGone(w, id)
		return
	}
	l, e := s.leases.renew(id, req.TTLSec)
	if e != nil {
		writeError(w, e)
		return
	}
	logger.Info("lease %s: %s renewed until %s", l.ID, l.Device, l.ExpiresAt)
	writeJSON(w, http.StatusOK, LeaseResult{Envelope: Envelope{OK: true}, Lease: l})
}

func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	if s.notHub(w) {
		return
	}
	id := r.PathValue("lid")
	e := s.visibleLease(r, id)
	if e == nil {
		s.leaseGone(w, id)
		return
	}
	l := e.view()
	s.endLease(e, endReleased)
	writeJSON(w, http.StatusOK, LeaseResult{Envelope: Envelope{OK: true}, Lease: l})
}

// ---------------------------------------------------------------------------
// Ending leases

// endLease ends a lease: the device is detached, and shut down when the
// hub booted it for the lease.
func (s *Server) endLease(e *leaseEntry, reason string) {
	s.leaseMu.Lock()
	teardown := s.endLeaseLocked(e, reason)
	s.leaseMu.Unlock()
	teardown()
}

// endLeaseLocked removes e with Server.leaseMu held and returns its
// teardown, which the caller runs after unlocking: detaching the device
// (a driver or WDA teardown) and shutting down one the lease booted take
// seconds, and must not hold up every other lease call on the hub. Until
// the teardown is done the device is reserved (leaseTable.ending), so no
// new lease takes it mid-shutdown.
func (s *Server) endLeaseLocked(e *leaseEntry, reason string) (teardown func()) {
	if !s.leases.remove(e) {
		return func() {}
	}
	s.leases.mu.Lock()
	s.leases.ending[e.device] = true
	if e.avd != "" {
		s.leases.ending[e.avd] = true
	}
	s.leases.mu.Unlock()
	// Counted from here until finish is done, wherever the teardown runs
	// (the caller, the sweeper's goroutine, or after the work under the
	// lease unwound): Shutdown waits for it.
	s.teardowns.add()
	finish := func() {
		defer s.teardowns.done()
		defer func() {
			s.leases.mu.Lock()
			delete(s.leases.ending, e.device)
			delete(s.leases.ending, e.avd)
			s.leases.mu.Unlock()
		}()
		logger.Info("lease %s: %s %s (holder %s)", e.id, e.device, reason, HolderName(e.holder))
		s.detachForStop(e.device)
		switch {
		case !e.booted, e.stopped.Load():
			// Not the hub's to shut down, or the holder stopped or
			// deleted it itself.
		case !s.stillLeasedAVD(e):
			logger.Warn("lease %s: %s no longer runs %s; not shutting it down", e.id, e.device, e.avd)
		default:
			if err := s.registry.Stop(e.device, true); err != nil {
				logger.Warn("lease %s: shut down %s: %v", e.id, e.device, err)
			} else {
				s.events.Publish(Event{Type: "device.stopped", Device: e.device, Message: "lease " + reason})
			}
		}
		s.events.Publish(Event{Type: leaseEndEvent(reason), Device: e.device, Message: HolderName(e.holder)})
	}
	return func() {
		if e.inFlight.Load() == 0 {
			finish()
			return
		}
		// A request or an attach under the lease is still running (a
		// driver call that cannot be cancelled): the device stays reserved
		// until it has unwound, then the teardown detaches what it left.
		// The caller is not held up meanwhile; the sweeper ends other
		// leases on time.
		logger.Info("lease %s: %s %s while work under it runs; the device stays reserved until it unwinds", e.id, e.device, reason)
		go func() {
			e.work.Wait()
			logger.Info("lease %s: the work on %s unwound; releasing the device", e.id, e.device)
			finish()
		}()
	}
}

// stillLeasedAVD reports whether e's device still runs the AVD the lease
// recorded. An emulator's serial is its port, which a later emulator takes
// once the leased one dies: the teardown must not shut that one down. Any
// other device, or a serial no longer listed, is left to Registry.Stop.
func (s *Server) stillLeasedAVD(e *leaseEntry) bool {
	if e.avd == "" {
		return true
	}
	d, ok := s.registry.Lookup(e.device)
	return !ok || avdKey(d) == e.avd
}

// endStoppedLease ends the lease on a device its request (ctx) stopped or
// deleted: the lease the request ran under, and any other on devices. The
// teardown detaches what is left and does not shut the device down again.
// The request's own work on its lease ends first, so unless other work
// runs under the lease, the teardown is done before the request answers.
func (s *Server) endStoppedLease(ctx context.Context, reason string, devices ...string) {
	ended := map[*leaseEntry]bool{}
	end := func(e *leaseEntry) {
		if e != nil && !ended[e] {
			ended[e] = true
			e.stopped.Store(true)
			s.endLease(e, reason)
		}
	}
	if v, ok := ctx.Value(leaseCtxKey{}).(leaseCtx); ok && v.e != nil {
		ended[v.e] = true
		v.e.stopped.Store(true)
		if v.doneWork != nil {
			v.doneWork()
		}
		s.endLease(v.e, reason)
	}
	for _, d := range devices {
		s.leases.mu.Lock()
		e := s.leases.byDevice[d]
		s.leases.mu.Unlock()
		end(e)
	}
}

// sweepLeases ends every lease whose time ran out. The entries are removed
// under leaseMu; their teardowns run after it, each on its own, and the
// sweep does not wait for them: one slow shutdown never holds up the end
// of a lease that expires meanwhile (the device stays reserved, ending,
// until its teardown is done).
func (s *Server) sweepLeases() {
	s.leaseMu.Lock()
	var teardowns []func()
	for _, e := range s.leases.expired() {
		teardowns = append(teardowns, s.endLeaseLocked(e, endExpired))
	}
	s.leaseMu.Unlock()
	for _, td := range teardowns {
		go td()
	}
}

// leaseShutdownWait bounds how long a stopping hub waits for its lease
// teardowns: one waiting for work that ignores its cancellation may never
// end.
var leaseShutdownWait = 10 * time.Second

// endAllLeases ends every lease as the hub stops: requests under them are
// cancelled, and each teardown detaches its device and publishes its
// lease event. It waits, for leaseShutdownWait at most, for every teardown
// still running: these, the sweeper's, and those that wait for work under
// their lease to unwind, so none runs beside the hub's own detach and
// shutdown of its devices.
func (s *Server) endAllLeases() {
	s.leaseMu.Lock()
	s.leases.mu.Lock()
	all := make([]*leaseEntry, 0, len(s.leases.byID))
	for _, e := range s.leases.byID {
		all = append(all, e)
	}
	s.leases.mu.Unlock()
	teardowns := make([]func(), 0, len(all))
	for _, e := range all {
		teardowns = append(teardowns, s.endLeaseLocked(e, endHubStopped))
	}
	s.leaseMu.Unlock()
	for _, td := range teardowns {
		go td()
	}
	if !s.teardowns.wait(leaseShutdownWait) {
		logger.Warn("lease teardowns still run after %v; stopping beside them", leaseShutdownWait)
	}
}

// runLeaseSweeper ends expired leases until ctx ends.
func (s *Server) runLeaseSweeper(ctx context.Context) {
	t := time.NewTicker(leaseSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			s.sweepLeases()
		}
	}
}

// ---------------------------------------------------------------------------
// Hub name

// hubName is the hub's name: Config.HubName, else the short host name.
func (s *Server) hubName() string {
	if s.cfg.HubName != "" {
		return s.cfg.HubName
	}
	return DefaultHubName()
}

// DefaultHubName is the short host name, lowercased, as a hub name:
// [a-z0-9-]{1,32}.
func DefaultHubName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	var b strings.Builder
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 32 {
		name = name[:32]
	}
	if name == "" {
		return "hub"
	}
	return name
}

// checkShareBoot refuses a remote caller's boot or create of a device the
// hub does not share: booting an unshared one is DEVICE_NOT_FOUND, and a
// device may be created only under a name --share-device lists.
func (s *Server) checkShareBoot(r *http.Request, req BootRequest) *Error {
	if len(s.cfg.ShareDevices) == 0 || !CallerFrom(r.Context()).Remote {
		return nil
	}
	if req.Create != nil {
		name := req.Create.Name
		if name == "" {
			name = req.Name
		}
		if !s.sharedInfo(DeviceInfo{ID: name, Name: name}) {
			return forbidden("this hub shares only %s; a remote caller cannot create %q", strings.Join(s.cfg.ShareDevices, ", "), name).
				WithDetails("field", "name")
		}
		return nil
	}
	if s.sharedInfo(DeviceInfo{ID: req.Name, Name: strings.TrimPrefix(req.Name, AVDPrefix)}) || s.shared(req.Name) {
		return nil
	}
	return Errorf(CodeDeviceNotFound, "device %s not found", req.Name)
}
