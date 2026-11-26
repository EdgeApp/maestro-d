package hubd

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
// only POST /v1/leases/{lid}/renew does. A reservation (POST
// /v1/reservations, reservations.go) is a lease whose window starts later:
// it is "scheduled" until then, and the sweeper starts it on time. No two
// windows on one device overlap, whether started or scheduled. At expiry, or on DELETE
// /v1/leases/{lid}, the hub ends the lease: a request still running under
// it is cancelled (a step reports INTERRUPTED; one whose driver call
// ignores that is answered LEASE_EXPIRED), and once every request under it
// has unwound the device is detached and one the hub booted for the lease
// is shut down, so the device shows as free in every listing again.
//
// A plain daemon (not a hub) has no leases: there, attach-and-own stays as
// it was, and the lease routes answer USAGE.
//
// The hub keeps its other devices private to its own machine:
// a remote caller does not see them in listings and is refused them with
// DEVICE_NOT_FOUND. The unix socket, which only the hub's machine reaches,
// sees every device.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// leaseSweepEvery is how often a hub ends the leases that expired.
const leaseSweepEvery = time.Second

type leaseEntry struct {
	id, device, holder, description string
	durable                         bool // true once the successful grant and optional replay can be snapshotted
	ttl, max                        time.Duration
	cloud                           bool // cloud sessions cannot outlive their provider allocation
	// starts is the window's start; started is set once it has begun
	// (at once for a lease taken now, by the sweeper for a reservation).
	starts, expires time.Time
	started         atomic.Bool
	// A holder may continue using a device shared when its lease or reservation
	// was granted, even after the operator removes it from config.json.
	sharedAtGrant  atomic.Bool
	booted         bool
	restored       bool // active when the hub restarted
	awaitingAttach atomic.Bool
	suspended      bool // prevents new work after shutdown cancelled existing requests
	// avd is the bootKey of the AVD an emulator lease's device runs
	// ("" for any other device): shut down, the AVD is listed under it.
	avd string
	// ended is closed when the lease ends, cancelling the requests that
	// run under it; torn once its teardown is done (detached, and shut
	// down when it booted the device), which may be later.
	ended chan struct{}
	torn  chan struct{}
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
	// cloud is the cloud device the lease launched (remote.go): the lease's
	// end releases it, and its release ends the lease. cloudDone is closed
	// once it is released, with its minutes in cloudMinutes.
	releaseCloud func() *cloudprov.DeviceMinutes
	cloudDone    chan struct{}
	cloudMinutes *cloudprov.DeviceMinutes
}

// The reasons a lease ends, as the hub logs them; leaseEndEvent names the
// documented event each one publishes.
const (
	endExpired      = "expired"
	endReleased     = "released"
	endAttachFailed = "attach failed"
	endStopped      = "ended (device stopped)"
	endDeleted      = "ended (device deleted)"
	endRemoved      = "ended (device removed)"
	endHubStopped   = "ended (hub stopped)"
	// endCloudReleased: the lease's cloud device was released by its idle
	// timeout or the provider's session cap.
	endCloudReleased = "ended (cloud device released)"
)

// leaseEndEvent is the event an ended lease publishes: lease.expired when
// its window ran out, lease.released for every other end (a release, its
// device stopped or deleted, the attach in its request failing, the hub
// stopping).
func leaseEndEvent(reason string) string {
	if reason == endExpired || reason == endCloudReleased {
		return "lease.expired"
	}
	return "lease.released"
}

// startWork counts a request or an attach under live lease e; false when
// e has expired or ended, and the work must not start.
func (t *leaseTable) startWork(e *leaseEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID[e.id] != e || e.suspended || !e.liveAt(t.now()) {
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
func leaseExpiredErr(e *leaseEntry) *daemon.Error {
	return daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on device %s has expired or was released; take a new one", e.id, e.device).
		WithDetails("lease", e.id)
}

// beingReleasedErr is DEVICE_IN_USE for a device whose ended lease is
// still being torn down.
func beingReleasedErr(device string) *daemon.Error {
	return daemon.Errorf(daemon.CodeDeviceInUse, "device %s is being released (its lease ended); try again shortly", device)
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
func (t *leaseTable) reserved(d daemon.DeviceInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reservedLocked(d) || t.ending[bootKey(d)] || t.byDevice[d.ID] != nil || t.byDevice[avdKey(d)] != nil
}

// tearingDown reports whether device id is reserved by a teardown or a
// boot (reservedLocked), whatever lease holds it.
func (t *leaseTable) tearingDown(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reservedLocked(daemon.DeviceInfo{ID: id})
}

// leaseTable holds a hub's live leases.
type leaseTable struct {
	removed    removedWindows
	bootDuties map[string]bootDuty
	replays    map[string]daemon.AcquisitionReplay
	attempts   map[string]daemon.SelectorAttempt
	mu         sync.Mutex
	now        func() time.Time
	byID       map[string]*leaseEntry
	byDevice   map[string]*leaseEntry
	// scheduled holds each device's reservations that have not started,
	// by start. byDevice holds only started leases; byID holds both.
	scheduled map[string][]*leaseEntry
	// torn holds, by device, the torn channel of an ended lease whose
	// teardown is still running: the lease that starts next on the device
	// is announced once it closes.
	torn map[string]chan struct{}
	// booting reserves the shut-down devices a selector lease is booting,
	// by bootKey, until the lease on the booted device is granted.
	booting map[string]bool
	// ending reserves the devices whose ended lease is still being torn
	// down (detached, and shut down when the lease booted them), so no new
	// lease takes one mid-shutdown. An emulator's AVD is reserved under
	// its bootKey too: it can be listed shut down before the teardown ends.
	// endingN counts the teardowns running under each key: two can overlap
	// on one device (a reservation started while the lease before it was
	// torn down, and released in turn), and the key stays reserved until
	// the last is done.
	ending  map[string]bool
	endingN map[string]int
}

// beginEndingLocked reserves keys ("" skipped) for one more teardown.
// The caller holds t.mu.
func (t *leaseTable) beginEndingLocked(keys ...string) {
	for _, k := range keys {
		if k != "" {
			t.endingN[k]++
			t.ending[k] = true
		}
	}
}

// doneEndingLocked ends one teardown under keys; a key is free once its
// last teardown is done. The caller holds t.mu.
func (t *leaseTable) doneEndingLocked(keys ...string) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if t.endingN[k]--; t.endingN[k] <= 0 {
			delete(t.endingN, k)
			delete(t.ending, k)
		}
	}
}

// bootKey names a shut-down device in the booting reservations so that
// the ready device it becomes matches too: a simulator keeps its UDID,
// and an AVD becomes an emulator serial listed under the AVD's name.
func bootKey(d daemon.DeviceInfo) string {
	if name, ok := strings.CutPrefix(d.ID, daemon.AVDPrefix); ok {
		return "android:" + strings.ToLower(name)
	}
	return d.ID
}

// avdKey is the bootKey of the AVD that Android device d is listed under
// (a running emulator's name is its AVD's, and a shut-down AVD is listed
// as avd:<name>), or "" for any other device. A phone has none: phones are
// named too, and two of one model share a name.
func avdKey(d daemon.DeviceInfo) string {
	if strings.HasPrefix(d.ID, daemon.AVDPrefix) {
		return bootKey(d)
	}
	if d.Platform != "android" || d.Name == "" {
		return ""
	}
	if d.Kind != "emulator" && !strings.HasPrefix(d.ID, "emulator-") {
		return ""
	}
	return "android:" + strings.ToLower(d.Name)
}

// reservedLocked reports whether device d is one a selector lease is
// booting (it may be listed ready before that boot returns) or a teardown
// is shutting down, by its id or, for a running emulator or a shut-down
// AVD, by its AVD's key. The caller holds t.mu.
func (t *leaseTable) reservedLocked(d daemon.DeviceInfo) bool {
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
func (t *leaseTable) releasing(d daemon.DeviceInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reservedLocked(d) {
		return true
	}
	return t.expiredOnLocked(d.ID, avdKey(d), t.now()) != nil
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
	return &leaseTable{removed: make(removedWindows), bootDuties: make(map[string]bootDuty), replays: map[string]daemon.AcquisitionReplay{}, attempts: map[string]daemon.SelectorAttempt{}, now: time.Now, byID: map[string]*leaseEntry{}, byDevice: map[string]*leaseEntry{},
		scheduled: map[string][]*leaseEntry{}, torn: map[string]chan struct{}{}, booting: map[string]bool{}, ending: map[string]bool{}, endingN: map[string]int{}}
}

// liveAt reports whether e has started and its window has not ended.
func (e *leaseEntry) liveAt(now time.Time) bool { return e.started.Load() && now.Before(e.expires) }

// pending reports whether e is a reservation that has not started.
func (e *leaseEntry) pending() bool { return !e.started.Load() }

func (e *leaseEntry) view() daemon.Lease {
	state := daemon.LeaseActive
	if e.pending() {
		state = daemon.LeaseScheduled
	}
	return daemon.Lease{ID: e.id, Device: e.device, Holder: e.holder, Description: e.description, StartsAt: e.starts.UTC().Format(time.RFC3339),
		ExpiresAt: e.expires.UTC().Format(time.RFC3339), State: state,
		TTLSec: int(e.ttl / time.Second), MaxSec: int(e.max / time.Second), Booted: e.booted}
}

// liveFor returns the live lease on device, if any.
func (t *leaseTable) liveFor(device string) (daemon.Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byDevice[device]; e != nil && e.liveAt(t.now()) {
		return e.view(), true
	}
	return daemon.Lease{}, false
}

// get returns a live or scheduled lease by id.
func (t *leaseTable) get(id string) (daemon.Lease, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byID[id]; e != nil && (e.liveAt(t.now()) || e.pending()) {
		return e.view(), true
	}
	return daemon.Lease{}, false
}

// list returns every live and scheduled lease, by device, then start.
func (t *leaseTable) list() []daemon.Lease {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := []daemon.Lease{}
	for _, e := range t.byID {
		if e.liveAt(now) || e.pending() {
			out = append(out, e.view())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].StartsAt < out[j].StartsAt
	})
	return out
}

// renew sets a live lease's window to ttlSec seconds from now (0: its own
// window), refusing one over the lease's maximum, one that would reach
// into the device's next reservation, and a reservation that has not
// started.
func (t *leaseTable) renew(id string, ttlSec int) (daemon.Lease, *daemon.Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.byID[id]
	if e != nil && e.pending() {
		return daemon.Lease{}, daemon.Errorf(daemon.CodeUsage, "lease %s starts at %s; a reservation is not renewed: release it and reserve again to move it",
			id, e.starts.UTC().Format(time.RFC3339)).WithDetails("lease", id, "startsAt", e.starts.UTC().Format(time.RFC3339))
	}
	if e == nil || !e.liveAt(now) {
		return daemon.Lease{}, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s has expired or was released", id).WithDetails("lease", id)
	}
	if !e.durable && !e.cloud {
		return daemon.Lease{}, daemon.Errorf(daemon.CodeBusy, "lease %s is still being acquired", id)
	}
	ttl := e.ttl
	if ttlSec != 0 {
		var we *daemon.Error
		if ttl, we = leaseWindow(e.device, ttlSec, e.max); we != nil {
			return daemon.Lease{}, we
		}
	}
	// Counted from the start of the current second, as a grant is.
	expires := now.Truncate(time.Second).Add(ttl)
	if c := t.overlapLocked(e.tableKey(), now.Truncate(time.Second), expires, e); c != nil {
		return daemon.Lease{}, overlapErr(e.device, c)
	}
	e.ttl = ttl
	e.expires = expires
	return e.view(), nil
}

// overlapLocked returns a lease on device whose window overlaps [from,
// to), other than skip: the started lease (an expired one that has not
// ended yet never overlaps a window from now on) or a reservation.
// Windows that only touch do not overlap. The caller holds t.mu.
func (t *leaseTable) overlapLocked(device string, from, to time.Time, skip *leaseEntry) *leaseEntry {
	overlaps := func(e *leaseEntry) bool {
		return e != nil && e != skip && from.Before(e.expires) && e.starts.Before(to)
	}
	if e := t.byDevice[device]; overlaps(e) {
		return e
	}
	for _, e := range t.scheduled[device] {
		if overlaps(e) {
			return e
		}
	}
	return nil
}

// overlapErr is DEVICE_IN_USE for a window that overlaps lease c on
// device: c's holder and window, and nextStartsAt when c is a reservation.
func overlapErr(device string, c *leaseEntry) *daemon.Error {
	starts, expires := c.starts.UTC().Format(time.RFC3339), c.expires.UTC().Format(time.RFC3339)
	if c.pending() {
		return daemon.Errorf(daemon.CodeDeviceInUse, "device %s is reserved by %s from %s until %s; ask for a window that ends by %s",
			device, daemon.HolderName(c.holder), starts, expires, starts).
			WithDetails("holder", c.holder, "startsAt", starts, "expiresAt", expires, "nextStartsAt", starts)
	}
	return daemon.Errorf(daemon.CodeDeviceInUse, "device %s is leased by %s until %s", device, daemon.HolderName(c.holder), expires).
		WithDetails("holder", c.holder, "startsAt", starts, "expiresAt", expires)
}

// isPending reports whether id is a reservation that has not started.
func (t *leaseTable) isPending(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byID[id]
	return e != nil && e.pending()
}

// notStartedErr is LEASE_NOT_STARTED for a request under reservation e.
func notStartedErr(e *leaseEntry) *daemon.Error {
	starts := e.starts.UTC().Format(time.RFC3339)
	return daemon.Errorf(daemon.CodeLeaseNotStarted, "lease %s on device %s starts at %s; run the command after then", e.id, e.device, starts).
		WithDetails("lease", e.id, "startsAt", starts)
}

// reservations lists device's reservations that have not started, by start.
func (t *leaseTable) reservations(device string) []daemon.DeviceLease {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []daemon.DeviceLease
	for _, e := range t.scheduled[device] {
		out = append(out, daemon.DeviceLease{Holder: e.holder, Description: e.description, StartsAt: e.starts.UTC().Format(time.RFC3339),
			ExpiresAt: e.expires.UTC().Format(time.RFC3339)})
	}
	return out
}

// scheduledOn returns device's reservations that have not started.
func (t *leaseTable) scheduledOn(device string) []*leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*leaseEntry(nil), t.scheduled[device]...)
}

// hasDue reports whether a reservation's start has come.
func (t *leaseTable) hasDue() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, list := range t.scheduled {
		if len(list) > 0 && !list[0].starts.After(now) {
			return true
		}
	}
	return false
}

// startDue starts every reservation whose start has come, on a device
// whose earlier lease has ended (the sweeper ends expired leases first),
// and drops one whose whole window passed before it could start (a hub
// that was down). The caller holds Server.leaseMu.
func (t *leaseTable) startDue(removing map[string]bool) (started, missed []*leaseEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for device, list := range t.scheduled {
		if removing[device] {
			continue
		}
		for len(list) > 0 && !list[0].starts.After(now) {
			e := list[0]
			if !now.Before(e.expires) {
				list = list[1:]
				delete(t.byID, e.id)
				close(e.ended)
				missed = append(missed, e)
				continue
			}
			if t.byDevice[device] != nil {
				break // its predecessor has not ended yet
			}
			list = list[1:]
			e.started.Store(true)
			t.byDevice[device] = e
			started = append(started, e)
		}
		if len(list) == 0 {
			delete(t.scheduled, device)
		} else {
			t.scheduled[device] = list
		}
	}
	return started, missed
}

// successorLocked returns the lease that follows on device once its
// current lease has gone: the started one, else the first reservation.
// The caller holds t.mu.
func (t *leaseTable) successorLocked(device string) *leaseEntry {
	if e := t.byDevice[device]; e != nil {
		return e
	}
	if list := t.scheduled[device]; len(list) > 0 {
		return list[0]
	}
	return nil
}

// handBootGrace is how soon a reservation must start to take over the
// shutdown of a device an ended lease booted: a device is not kept booted
// and idle for a reservation hours away.
const handBootGrace = time.Minute

// overMax is the USAGE error for a window over a device's maximum; device
// "" is a cloud device not launched yet.
func overMax(device string, ttlSec int, maxTTL time.Duration) *daemon.Error {
	whose := "device " + device + "'s"
	if device == "" {
		whose = "a cloud device's"
	}
	return daemon.Errorf(daemon.CodeUsage, "leases: a window of ttlSec %d is over %s maximum of %s (maxSec %d)",
		ttlSec, whose, maxTTL, int(maxTTL/time.Second)).
		WithDetails("maxSec", int(maxTTL/time.Second), "ttlSec", ttlSec)
}

// grant leases device to holder for ttl from starts (zero, or a time not
// after now: at once), unless another lease's window overlaps it. A lease
// that expired but has not ended yet still holds the device for a lease
// taken now: only endLeaseLocked drops a lease, because only it cancels
// the lease's requests, detaches the device and shuts down one the lease
// booted. A later start is a reservation, kept in scheduled until the
// sweeper starts it. The caller holds Server.leaseMu, which serialises
// grants with the sweeper.
func (t *leaseTable) grant(device, avd, holder string, starts time.Time, ttl, maxTTL time.Duration, booted bool) (*leaseEntry, *daemon.Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// Lease times are whole seconds, as RFC 3339 reports them, so a window
	// that starts at another's reported expiresAt only touches it, and
	// expiresAt is always startsAt plus ttl. A lease taken now counts from
	// the start of the current second.
	if !starts.After(now) {
		starts = now.Truncate(time.Second)
	}
	expires := starts.Add(ttl)
	pending := starts.After(now)
	if !pending && t.expiredOnLocked(device, avd, now) != nil {
		return nil, daemon.Errorf(daemon.CodeDeviceInUse, "device %s is being released (its lease expired); try again shortly", device)
	}
	if avd != "" {
		if c := t.overlapLocked(avd, starts, expires, nil); c != nil {
			return nil, overlapErr(device, c)
		}
	}
	if c := t.overlapLocked(device, starts, expires, nil); c != nil {
		return nil, overlapErr(device, c)
	}
	e := &leaseEntry{id: uuid.New().String(), device: device, holder: holder, ttl: ttl, max: maxTTL, starts: starts,
		expires: expires, booted: booted, avd: avd, ended: make(chan struct{})}
	t.byID[e.id] = e
	if pending {
		t.scheduleLocked(e)
	} else {
		e.started.Store(true)
		t.byDevice[device] = e
	}
	return e, nil
}

// scheduleLocked adds reservation e to its device's reservations, which
// stay sorted by start (startDue, successorLocked and handBoot rely on
// it). The caller holds t.mu.
func (t *leaseTable) scheduleLocked(e *leaseEntry) {
	list := append(t.scheduled[e.device], e)
	sort.Slice(list, func(i, j int) bool { return list[i].starts.Before(list[j].starts) })
	t.scheduled[e.device] = list
}

// remove drops e; false when it was already gone.
func (t *leaseTable) remove(e *leaseEntry, removedDevice string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID[e.id] != e {
		return false
	}
	delete(t.byID, e.id)
	if removedDevice != "" {
		t.removed[e.id] = removedLease{Device: removedDevice, Until: e.expires}
	}
	if e.booted && !e.stopped.Load() {
		d := e.duty()
		t.bootDuties[d.key()] = d
	}
	if t.byDevice[e.tableKey()] == e {
		delete(t.byDevice, e.tableKey())
	}
	if list := t.scheduled[e.device]; len(list) > 0 {
		kept := list[:0]
		for _, x := range list {
			if x != e {
				kept = append(kept, x)
			}
		}
		if len(kept) == 0 {
			delete(t.scheduled, e.device)
		} else {
			t.scheduled[e.device] = kept
		}
	}
	if !e.suspended {
		close(e.ended)
	}
	return true
}

// goneLeaseErr is LEASE_EXPIRED for a lease whose device went away with it.
// A cloud device's hint is a new cloud lease, since its id never comes back.
func (s *Server) goneLeaseErr(lease, id string) *daemon.Error {
	hint := "maestro-d device list"
	if local, _, _ := strings.Cut(id, "@"); len(s.deps.Providers) > 0 && cloudIDPattern.MatchString(local) {
		hint = "maestro-d lease --location cloud --platform ios|android"
	}
	return daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on device %s has ended, and the device is gone with it; take a new lease (%s)", lease, id, hint).
		WithDetails("lease", lease, "hint", hint)
}

// cloudIDPattern matches the ids launchCloud gives (newCloudID).
var cloudIDPattern = regexp.MustCompile(`^[a-z0-9]+-[0-9a-f]{8}$`)

// known reports whether this daemon can see device id at all.
func (s *Server) known(id string) bool {
	_, ok := s.Registry().Lookup(id)
	return ok
}

// isLive reports whether lease id is held and live.
func (t *leaseTable) isLive(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byID[id]
	return e != nil && e.liveAt(t.now())
}

// lookup returns the entry for id, live or not.
func (t *leaseTable) lookup(id string) *leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

// expired returns the started entries whose time has run out; a
// reservation that has not started is never among them.
func (t *leaseTable) expired() []*leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.removed.prune(now)
	var out []*leaseEntry
	for _, e := range t.byDevice {
		if !e.liveAt(now) {
			out = append(out, e)
		}
	}
	return out
}

// expiredOn returns the expired, not yet ended lease on device, if any.
func (t *leaseTable) expiredOn(d daemon.DeviceInfo) *leaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.expiredOnLocked(d.ID, avdKey(d), t.now())
}

// Restored emulator grants are indexed by AVD. New grants use the listed device ID.
func (t *leaseTable) expiredOnLocked(device, avd string, now time.Time) *leaseEntry {
	for _, key := range []string{device, avd} {
		if e := t.byDevice[key]; e != nil && !e.liveAt(now) {
			return e
		}
	}
	return nil
}

// use checks a request's lease header against the device's lease. need is
// false for a route that needs a lease only when the device is leased
// (stopping or deleting a device nobody holds). On success it returns the
// entry the request runs under, if any; using it never moves its expiry.
func (t *leaseTable) use(device, header string, need bool) (*leaseEntry, *daemon.Error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cur := t.byDevice[device]
	if cur != nil && !cur.liveAt(now) {
		cur = nil
	}
	if header == "" {
		if cur != nil {
			return nil, daemon.Errorf(daemon.CodeLeaseRequired, "device %s is leased by %s until %s; send its lease in %s", device,
				daemon.HolderName(cur.holder), cur.expires.UTC().Format(time.RFC3339), daemon.HeaderLease).
				WithDetails("holder", cur.holder, "expiresAt", cur.expires.UTC().Format(time.RFC3339))
		}
		if need {
			return nil, daemon.Errorf(daemon.CodeLeaseRequired, "device %s needs a lease on this hub, and clients never take one on their own: "+
				"run `maestro-d lease --device %s` (or md.lease() in JavaScript, POST /v1/leases over REST) and send it in %s",
				device, device, daemon.HeaderLease).WithDetails("hint", "maestro-d lease --device "+device)
		}
		return nil, nil
	}
	if cur != nil && cur.id == header {
		return cur, nil
	}
	if p := t.byID[header]; p != nil && p.pending() && p.device == device {
		return nil, notStartedErr(p)
	}
	if other := t.byID[header]; other != nil && (other.liveAt(now) || other.pending()) {
		return nil, daemon.Errorf(daemon.CodeLeaseRequired, "lease %s is for device %s, not %s", header, other.device, device).
			WithDetails("leaseDevice", other.device)
	}
	return nil, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on device %s has expired or was released; take a new one", header, device).
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
	ctx, cancel := daemon.ContextUntilLeaseEnds(r.Context(), e.ended)
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
	ctx := daemon.WithLeaseWindow(r.Context(), e.ended)
	return r.WithContext(context.WithValue(ctx, leaseCtxKey{}, leaseCtx{e: e, doneWork: doneWork}))
}

// leaseCtxKey carries the lease a request runs under in its context.
type leaseCtxKey struct{}

// leaseCtx is the value under leaseCtxKey: the lease, and the request's
// context before underLease tied it to the lease.
type leaseCtx struct {
	e        *leaseEntry
	doneWork func()
}

// leaseFrom returns the lease a request runs under (set by underLease), if any.
func leaseFrom(ctx context.Context) *leaseEntry {
	v, _ := ctx.Value(leaseCtxKey{}).(leaseCtx)
	return v.e
}

// shareMatches reports whether a lease duration override names a device, by
// id or by name (a running emulator's name is its AVD name).
func shareMatches(entry string, d daemon.DeviceInfo) bool {
	return entry == d.ID || (d.Name != "" && strings.EqualFold(entry, d.Name)) ||
		(strings.HasPrefix(d.ID, daemon.AVDPrefix) && strings.EqualFold(entry, strings.TrimPrefix(d.ID, daemon.AVDPrefix)))
}

// sharedInfo reports whether this hub exposes the device to remote callers.
func (s *Server) sharedInfo(d daemon.DeviceInfo) bool {
	return publiclyShared(s.DeviceConfig(), d)
}

// shareList describes what a hub shares, for its log and refusals: "" when
// it names nothing.
func (s *Server) shareList() string {
	ids := s.DeviceConfig().Shared
	if len(ids) == 0 {
		return ""
	}
	return "config.json " + strings.Join(ids, ", ")
}

// tvInfo reports whether id is an Apple TV on this host, and its name: from
// Deps.TVInfo, or from the registry's record of a TV this daemon attached.
// Neither runs discovery, and neither forgets a TV while devicectl fails,
// so an unshared TV stays private.
func (s *Server) tvInfo(id string) (daemon.DeviceInfo, bool) {
	if s.deps.TVInfo != nil {
		if name, ok := s.deps.TVInfo(id); ok {
			return daemon.DeviceInfo{ID: id, Kind: "tv", Name: name}, true
		}
	}
	if rec, ok := s.Registry().Record(id); ok && strings.EqualFold(rec.Kind, "tv") {
		return rec, true
	}
	return daemon.DeviceInfo{}, false
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
	// Exclude computer sessions before consulting configured shares or cached IDs.
	if id == daemon.ComputerID {
		return false
	}
	if rec, ok := s.Registry().Record(id); ok && daemon.IsComputer(rec) {
		return false
	}
	if tv, ok := s.tvInfo(id); ok {
		return s.sharedInfo(tv)
	}
	s.DeviceConfig() // refresh before consulting either cached share answer
	stable := !daemon.IsEmulatorSerial(id)
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
	info, found := s.Registry().Lookup(id)
	if !found {
		info = daemon.DeviceInfo{ID: id}
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
func (s *Server) OnEvent(ev daemon.Event) {
	switch {
	case ev.Type == "device.booted", ev.Type == "device.stopped", ev.Type == "device.created", ev.Type == "device.deleted",
		strings.HasPrefix(ev.Type, "lease.") && ev.Type != "lease.granted":
		s.shareMu.Lock()
		s.shareGen++
		clear(s.shareSeen)
		s.shareMu.Unlock()
	}
}

// heldBy recognizes only the exact credential for a device shared at grant.
func (s *Server) heldBy(r *http.Request, id string) bool {
	return s.leases.holds(r.Header.Get(daemon.HeaderLease), id)
}

func (t *leaseTable) holds(credential, device string) bool {
	if credential == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byID[credential]
	return e != nil && e.tableKey() == device && e.sharedAtGrant.Load() && (e.liveAt(t.now()) || e.pending())
}

// sharedWith reports whether the request's caller may see device id: a
// remote caller sees only the shared pool.
func (s *Server) sharedWith(r *http.Request, id string) bool {
	return !daemon.CallerFrom(r.Context()).Remote || s.shared(id)
}

// handleEvents serves GET /v1/events. A remote caller sees only the shared
// pool's events: a ?device= naming another device is not found, other
// devices' events are left out, and so is any event that names no device
// but the daemon's shutdown. A device's attach event carries the host's
// report directory, which a remote caller does not get either.
func (s *Server) EventsHandler(w http.ResponseWriter, r *http.Request) {
	if !daemon.CallerFrom(r.Context()).Remote {
		s.Events().ServeSSE(w, r)
		return
	}
	if id := r.URL.Query().Get("device"); id != "" && !s.shared(id) {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", id))
		return
	}
	s.Events().ServeSSEView(w, r, s.remoteEvent)
}

// remoteEvent is a remote caller's view of an event.
func (s *Server) remoteEvent(ev daemon.Event) (daemon.Event, bool) {
	switch {
	case ev.Device == "":
		return ev, ev.Type == "daemon.shutdown"
	case ev.Type == "device.unshared":
		return daemon.Event{Type: ev.Type, Device: ev.Device, Seq: ev.Seq, Time: ev.Time}, ev.PreviouslyShared
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
func (s *Server) Visible(r *http.Request, devs []daemon.DeviceInfo) []daemon.DeviceInfo {
	out := make([]daemon.DeviceInfo, 0, len(devs))
	remote := daemon.CallerFrom(r.Context()).Remote
	for _, d := range devs {
		if remote && !s.sharedInfo(d) && !s.heldBy(r, s.leases.deviceKey(d)) {
			continue
		}
		out = append(out, d)
	}
	s.annotate(out)
	return out
}

// annotate sets lease, maxSec, available, owner, machine and addresses
// on a hub's device entries.
func (s *Server) annotate(devs []daemon.DeviceInfo) {
	owners := s.cfg.OtherOwners()
	machine, addrs := s.HostIdentity()
	for i := range devs {
		d := &devs[i]
		if o := owners[d.ID]; o != "" && !d.Attached {
			d.Owner = o
		}
		d.Lease = nil
		if l, ok := s.leases.liveFor(s.leases.deviceKey(*d)); ok {
			d.Lease = &daemon.DeviceLease{Holder: l.Holder, Description: l.Description, StartsAt: l.StartsAt, ExpiresAt: l.ExpiresAt}
		}
		d.Reservations = s.leases.reservations(d.ID)
		d.MaxSec = int(s.leaseMaxInfo(*d) / time.Second)
		d.Machine, d.Addresses = machine, addrs
		// A cloud device belongs to the lease that launched it: never free.
		d.Available = d.Ready && d.Lease == nil && !d.Attached && d.Owner == "" && !strings.HasPrefix(d.ID, daemon.AVDPrefix) && d.Provider == ""
		if d.Available && s.leases.reserved(*d) {
			// Booting for a selector lease, or still being released.
			d.Available = false
		}
	}
}

// dev guards a /v1/devices/{id} route: an unshared device is not found for
// a remote caller, and on a hub the request must carry the device's lease.
func (s *Server) Gate(mode daemon.LeaseMode, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		target := daemon.DeviceInfo{ID: id}
		if mode == daemon.LeaseIfHeld {
			target = s.stopTarget(id)
			id = target.ID
		}
		s.startDue()
		key := id
		credential := r.Header.Get(daemon.HeaderLease)
		if e := s.leases.lookup(credential); e != nil && e.restoredAVD() {
			if id != s.resolvedDevice(e) {
				daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is not the verified binding of lease %s", id, e.id))
				return
			}
			key = e.tableKey()
		} else if mode == daemon.LeaseIfHeld {
			key = s.leases.deviceKey(target)
		}
		if !s.sharedWith(r, id) && !s.heldBy(r, key) {
			// A device that went away with its lease (a cloud device's
			// idle release, the session cap) answers a request under that
			// lease LEASE_EXPIRED, so clients forget the lease. A device
			// that still exists but is not shared stays not found.
			if header := r.Header.Get(daemon.HeaderLease); header != "" && s.removedLeaseMatches(id, header) {
				daemon.WriteError(w, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on %s %s", header, id, endRemoved).WithDetails("lease", header))
				return
			}
			if header := r.Header.Get(daemon.HeaderLease); mode != daemon.LeaseOpen && header != "" && len(s.deps.Providers) > 0 && cloudIDPattern.MatchString(id) && !s.leases.isLive(header) && !s.known(id) {
				daemon.WriteError(w, s.goneLeaseErr(header, id))
				return
			}
			daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", id))
			return
		}
		if mode == daemon.LeaseOpen {
			h(w, r)
			return
		}
		e, le := s.leases.use(key, r.Header.Get(daemon.HeaderLease), mode == daemon.LeaseNeeded)
		if le != nil {
			// A device that went away (a released cloud device) is not
			// offered for a lease: without one it is not found, and an
			// ended lease on it says the device went too.
			if (le.Code == daemon.CodeLeaseRequired || le.Code == daemon.CodeLeaseExpired) && !s.known(id) {
				if header := r.Header.Get(daemon.HeaderLease); header != "" {
					le = s.goneLeaseErr(header, id)
				} else {
					le = daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", id)
				}
			}
			daemon.WriteError(w, le)
			return
		}
		if e != nil && s.leases.tearingDown(key) {
			// A lease the sweeper started as the one before it ended: that
			// lease's teardown (detach, shutdown) still runs on the device,
			// and nothing under this one may run beside it.
			daemon.WriteError(w, beingReleasedErr(id))
			return
		}
		if e == nil && mode == daemon.LeaseIfHeld && s.leases.releasing(target) {
			// The teardown of the device's ended lease may be shutting
			// it down, or a selector lease booting it (an emulator by its
			// AVD too): a stop or delete beside it would race it.
			daemon.WriteError(w, beingReleasedErr(id))
			return
		}
		if e == nil {
			h(w, r)
			return
		}
		if mode == daemon.LeaseIfHeld {
			// Stopping or deleting the device is work under its lease, so
			// a teardown that ends the lease meanwhile waits for it, and
			// does not shut the device down again once it has
			// (leaseEntry.stopped). It is not cancelled when the lease
			// ends: a stop under way is answered for as it went. The
			// handler ends the lease itself after the stop or delete
			// (endStoppedLease), leaving its own work out first, so the
			// teardown is done before it answers.
			if !s.leases.startWork(e) {
				daemon.WriteError(w, leaseExpiredErr(e))
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
		daemon.WriteError(w, leaseExpiredErr(e))
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
				daemon.WriteError(g, daemon.Errorf(daemon.CodeInternal, "internal error: %v", p))
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
		daemon.WriteError(w, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on device %s ended while the request ran; take a new one", e.id, e.device).
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

// Flush sends what the handler wrote so far (a stream's init segment and
// each fragment): without it no leased route could flush. Like a write, it
// holds mu, and once the gate is closed it does nothing.
func (g *leaseGate) Flush() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.startLocked()
	_ = http.NewResponseController(g.w).Flush()
}

// Unwrap lets http.ResponseController reach the request's writer.
func (g *leaseGate) Unwrap() http.ResponseWriter { return g.w }

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
func (s *Server) stopTarget(id string) daemon.DeviceInfo {
	if info, e := s.Registry().ResolveManaged(id); e == nil {
		return info
	}
	return daemon.DeviceInfo{ID: id}
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	var req daemon.LeaseRequest
	if e := daemon.DecodeBody(r, &req); e != nil {
		daemon.WriteError(w, e)
		return
	}
	if (req.Device == "") == (req.Select == nil) {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "leases: give device or select, not both or neither"))
		return
	}
	if req.TTLSec < 0 {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "leases: ttlSec must be positive"))
		return
	}
	caller := daemon.CallerFrom(r.Context())
	if req.Attach != nil && caller.Remote {
		if e := checkRemoteAttach(req.Attach); e != nil {
			s.LogForbidden(r, e)
			daemon.WriteError(w, e)
			return
		}
	}
	holder := strings.TrimSpace(req.Holder)
	if holder == "" {
		holder = daemon.DescribeCaller(caller)
	}
	var entry *leaseEntry
	var e *daemon.Error
	if req.Device != "" {
		if !s.sharedWith(r, req.Device) {
			daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", req.Device))
			return
		}
		// A shut-down AVD is never driven under its avd: id: booted, it is
		// emulator-<port>, which such a lease would not cover.
		if avd, ok := strings.CutPrefix(req.Device, daemon.AVDPrefix); ok {
			daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "leases: %s is a shut-down AVD, which cannot be leased by id: "+
				"run `maestro-d lease --name %s --boot` to have the hub boot it for the lease, "+
				"or `maestro-d device start %s` and lease the emulator it becomes", req.Device, avd, req.Device).
				WithDetails("hint", "maestro-d lease --name "+avd+" --boot"))
			return
		}
		// A hub leases only a device it lists: an id nothing here can see
		// would get a lease no driver can use.
		d, listed := s.Registry().Lookup(req.Device)
		if !listed {
			daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s is not listed on hub %s", req.Device, s.HubName()))
			return
		}
		if caller.Remote && !s.sharedInfo(d) {
			daemon.WriteError(w, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", req.Device))
			return
		}
		entry, e = s.leaseDevice(daemon.CallerFrom(r.Context()).Remote, d, holder, req.TTLSec, claimNone)
	} else {
		entry, e = s.leaseSelect(r, *req.Select, req.Cloud, holder, req.TTLSec)
	}
	if e != nil {
		daemon.WriteError(w, e)
		return
	}
	s.leases.mu.Lock()
	entry.description = daemon.LeaseDescription(req.Description)
	s.leases.mu.Unlock()
	logger.Info("lease %s: %s leased by %s for %v", entry.id, entry.device, holder, entry.ttl)
	s.Events().Publish(daemon.Event{Type: "lease.granted", Device: entry.device, Message: holder})
	var info *daemon.DeviceInfo
	if req.Attach != nil {
		// The attach runs under the lease, like any request carrying it:
		// the window ending answers LEASE_EXPIRED at once and cancels the
		// attach, and the lease's teardown waits for it to unwind before
		// it detaches the device and frees it (attachLeased).
		ar, stop := underLease(r, entry)
		cfg := *req.Attach
		attachedInfo, ae := s.Attach(ar.Context(), entry.device, &cfg)
		stop()
		if ae != nil {
			s.endLease(entry, endAttachFailed)
			daemon.WriteError(w, ae)
			return
		}
		info = attachedInfo
	} else if d, ok := s.Registry().Lookup(entry.device); ok {
		info = &d
	}
	if info != nil {
		devs := []daemon.DeviceInfo{*info}
		s.annotate(devs)
		s.ApplyDeviceConfig(devs)
		info = &devs[0]
	}
	l, _ := s.leases.get(entry.id)
	if l.ID == "" {
		s.leases.mu.Lock()
		l = entry.view()
		s.leases.mu.Unlock()
	}
	result := daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l, Device: info}
	if s.commitAcquisition(w, r, entry, result) {
		daemon.WriteJSON(w, http.StatusOK, result)
	}
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
func (s *Server) Attach(ctx context.Context, id string, cfg *daemon.AttachConfig) (*daemon.DeviceInfo, *daemon.Error) {
	if daemon.CallerFrom(ctx).Remote {
		if err := checkRemoteAttach(cfg); err != nil {
			return nil, err
		}
	}
	e := leaseFrom(ctx)
	if e == nil {
		return s.AttachDevice(ctx, id, cfg)
	}
	ended := func() *daemon.Error {
		return daemon.Errorf(daemon.CodeLeaseExpired, "lease %s on device %s ended while the device attached; take a new one", e.id, e.device).
			WithDetails("lease", e.id)
	}
	if !s.leases.startWork(e) {
		return nil, ended()
	}
	type result struct {
		a   *daemon.DeviceInfo
		err *daemon.Error
	}
	done := make(chan result, 1)
	go func() {
		a, ae := s.AttachDevice(ctx, id, cfg)
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
		if res.err == nil {
			e.awaitingAttach.Store(false)
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
// bind, when given, runs on the granted entry before leaseMu is released,
// so nothing that ends the lease sees it half set up.
func (s *Server) leaseDevice(remote bool, d daemon.DeviceInfo, holder string, ttlSec int, claim bootClaim, bind ...func(*leaseEntry)) (*leaseEntry, *daemon.Error) {
	device := d.ID
	for range 3 {
		s.leaseMu.Lock()
		old := s.leases.expiredOn(d)
		if old == nil {
			defer s.leaseMu.Unlock()
			e, le := s.leaseDeviceLocked(remote, d, holder, ttlSec, claim)
			if le == nil {
				for _, f := range bind {
					f(e)
				}
			}
			return e, le
		}
		teardown := s.endLeaseLocked(old, endExpired)
		s.leaseMu.Unlock()
		teardown()
		if old.booted {
			// The teardown shut the device down: d is stale, and an
			// emulator's serial is gone. Grant only on what is listed now.
			var listed bool
			if d, listed = s.Registry().Lookup(device); !listed {
				return nil, daemon.Errorf(daemon.CodeDeviceNotFound, "device %s was shut down as its expired lease ended; it is no longer listed on hub %s", device, s.HubName())
			}
		}
	}
	return nil, daemon.Errorf(daemon.CodeDeviceInUse, "device %s: its expired lease is still ending; try again", device)
}

// leaseDeviceLocked grants the lease with Server.leaseMu held: a window
// of ttlSec seconds (0: the default, capped at the device's maximum), refused USAGE
// over the maximum. A device whose expired lease has not ended yet (a
// selector's candidate) is not granted: that lease is ended in the
// background and the device is DEVICE_IN_USE until its teardown is done.
// d is the device as the caller listed it: nothing here runs discovery,
// which would hold leaseMu, and every other lease call, for seconds.
func (s *Server) leaseDeviceLocked(remote bool, d daemon.DeviceInfo, holder string, ttlSec int, claim bootClaim) (*leaseEntry, *daemon.Error) {
	if e := s.admitConfigDevice(remote, d); e != nil {
		return nil, e
	}
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
		return nil, daemon.Errorf(daemon.CodeDeviceInUse, "device %s is booting for another lease", device)
	}
	if ending {
		return nil, beingReleasedErr(device)
	}
	if owner := s.cfg.FindDeviceOwner(device); owner != nil {
		e := daemon.Errorf(daemon.CodeDeviceInUse, "device %s is attached to daemon %q (pid %d)", device, owner.Name, owner.Info.PID)
		return nil, e.WithDetails("owner", owner.Name, "ownerPid", owner.Info.PID)
	}
	e, ge := s.leases.grant(device, k, holder, time.Time{}, window, maxTTL, claim == claimBooted)
	if ge != nil {
		// A lease that expired before grant (it may have been live when
		// leaseDevice looked) is ended in the background, under leaseMu.
		if old := s.leases.expiredOn(d); old != nil {
			go s.endLeaseLocked(old, endExpired)()
		}
		return nil, ge
	}
	e.cloud = d.Provider != ""
	e.sharedAtGrant.Store(s.sharedInfo(d))
	return e, nil
}

// leaseWindow is the window a lease request gets: ttlSec seconds, or with
// none the default capped at the device's maximum; over the maximum is
// USAGE. The seconds are compared before they become a Duration, which a
// huge ttlSec would overflow into an already-expired window.
func leaseWindow(device string, ttlSec int, maxTTL time.Duration) (time.Duration, *daemon.Error) {
	if ttlSec == 0 {
		return min(daemon.DefaultLeaseTTL, maxTTL), nil
	}
	if int64(ttlSec) > int64(maxTTL/time.Second) {
		return 0, overMax(device, ttlSec, maxTTL)
	}
	return time.Duration(ttlSec) * time.Second, nil
}

// leaseMaxInfo is the longest window a lease on listed device d may have:
// the first --lease-max DEVICE=DURATION that names it (by id or name),
// else the hub's --lease-max, else DefaultLeaseMax.
func (s *Server) leaseMaxInfo(d daemon.DeviceInfo) time.Duration {
	max := s.hubLeaseMax()
	for _, m := range s.cfg.LeaseMaxDevices {
		if shareMatches(m.Device, d) {
			max = m.Max
			break
		}
	}
	if d.Provider != "" {
		// A cloud device lives at most as long as its provider's session.
		max = min(max, cloudLeaseMax)
	}
	return max
}

// cloudLeaseMax is the longest lease on a cloud device: Device Farm's
// 150-minute session.
const cloudLeaseMax = 9000 * time.Second

// leaseMaxNote lists the per-device maximums for the hub's start line.
func leaseMaxNote(devs []daemon.LeaseMax) string {
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
	return daemon.DefaultLeaseMax
}

// leaseSelect leases the first free device that matches sel: a ready one,
// neither leased nor attached; with sel.Boot, else a shut-down simulator or
// AVD, which the hub boots for the lease.
func (s *Server) leaseSelect(r *http.Request, sel daemon.LeaseSelector, cloud *cloudprov.CloudSpec, holder string, ttlSec int) (*leaseEntry, *daemon.Error) {
	notFound := daemon.Errorf(daemon.CodeDeviceNotFound, "no device matches the selector %s", sel.String()).WithDetails("select", sel)
	if sel.Hub != "" && !strings.EqualFold(sel.Hub, s.HubName()) {
		return nil, notFound
	}
	if strings.EqualFold(sel.Location, daemon.LocationCloud) {
		return s.leaseCloud(r, sel, cloud, holder, ttlSec)
	}
	if sel.Location != "" {
		return nil, daemon.Errorf(daemon.CodeUsage, "lease: select.location %q: only cloud launches a device; leave it out to lease one of the hub's", sel.Location)
	}
	// What only a cloud launch reads is refused elsewhere, never ignored.
	if sel.Provider != "" {
		return nil, daemon.Errorf(daemon.CodeUsage, "lease: select.provider applies only with select.location cloud").WithDetails("field", "provider")
	}
	if cloud != nil {
		return nil, daemon.Errorf(daemon.CodeUsage, "lease: cloud (idle release, Appium version, …) applies only with select.location cloud").WithDetails("field", "cloud")
	}
	matches, me := s.selectorMatches(r, sel)
	if me != nil {
		return nil, me
	}
	if len(matches) == 0 {
		return nil, notFound
	}
	// A candidate whose maximum window is shorter than ttlSec is skipped; when
	// that leaves none, the answer is USAGE naming the maximum.
	var tooLong, reservedIn *daemon.Error
	s.leaseMu.Lock()
	for _, d := range matches {
		if old := s.leases.expiredOn(d); old != nil {
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
		e, le := s.leaseDeviceLocked(daemon.CallerFrom(r.Context()).Remote, d, holder, ttlSec, claimNone)
		if le == nil {
			s.leaseMu.Unlock()
			return e, nil
		}
		if le.Code == daemon.CodeUsage {
			tooLong = le
		}
		// Free now but reserved inside the window: kept, the earliest
		// reservation first, so the answer can say what window would fit.
		if next, ok := le.Details["nextStartsAt"].(string); ok && le.Code == daemon.CodeDeviceInUse {
			if prev, _ := reservedIn.Detail("nextStartsAt"); reservedIn == nil || next < prev {
				reservedIn = le
			}
		}
	}
	var boot *daemon.DeviceInfo
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
			window, we := leaseWindow(d.ID, ttlSec, s.leaseMaxInfo(*d))
			if we != nil {
				tooLong = we
				continue
			}
			now := s.leases.now()
			if c := s.leases.overlapLocked(d.ID, now, now.Add(window), nil); c != nil {
				// Reserved inside the window: the grant after the boot
				// would refuse it, so it is not booted, and the answer can
				// say what window would fit.
				next := c.starts.UTC().Format(time.RFC3339)
				if prev, _ := reservedIn.Detail("nextStartsAt"); c.pending() && (reservedIn == nil || next < prev) {
					reservedIn = overlapErr(d.ID, c)
				}
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
		if reservedIn != nil {
			next, _ := reservedIn.Detail("nextStartsAt")
			return nil, daemon.Errorf(daemon.CodeDeviceInUse, "every device matching %s is leased, attached, not ready, or reserved within the window; "+
				"ask for a ttlSec that ends by %s", sel.String(), next).
				WithDetails("candidates", ids, "nextStartsAt", next)
		}
		return nil, daemon.Errorf(daemon.CodeDeviceInUse, "every device matching %s is leased, attached or not ready", sel.String()).
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
	releaseIdle := s.HoldIdle()
	info, err := s.Registry().Boot(context.WithoutCancel(r.Context()), daemon.BootRequest{Platform: boot.Platform, Name: boot.ID})
	releaseIdle()
	if err != nil {
		return nil, daemon.WrapErr(daemon.CodeDeviceError, err)
	}
	if r.Context().Err() != nil {
		logger.Info("lease: the client left while %s booted for its lease", info.ID)
		s.disownBoot(info)
		return nil, daemon.Errorf(daemon.CodeInterrupted, "lease: cancelled while booting %s", boot.ID)
	}
	// Boot returns a device someone else started between the selection and
	// this call as it is: the lease shuts down only a device this daemon
	// booted.
	claim := claimReserved
	if info.BootedBy == daemon.BootedByDaemon {
		claim = claimBooted
		s.Events().Publish(daemon.Event{Type: "device.booted", Device: info.ID, Message: boot.Name})
	} else {
		logger.Info("lease: %s was started meanwhile by someone else; its lease will not shut it down", info.ID)
	}
	e, le := s.leaseDevice(daemon.CallerFrom(r.Context()).Remote, info, holder, ttlSec, claim)
	if le != nil {
		s.disownBoot(info)
		return nil, le
	}
	return e, nil
}

// selectorMatches lists the hub's devices the caller sees that sel
// matches, in the order selectors try them, refusing an OS bound that is
// not a version. Cloud devices are left out: one belongs to the lease that
// launched it. Leases and reservations walk the same list.
func (s *Server) selectorMatches(r *http.Request, sel daemon.LeaseSelector) ([]daemon.DeviceInfo, *daemon.Error) {
	for f, v := range map[string]string{"osVersionMin": sel.OSVersionMin, "osVersionMax": sel.OSVersionMax} {
		if _, ok := cloudprov.ParseVersion(v); v != "" && !ok {
			return nil, daemon.Errorf(daemon.CodeUsage, "lease: select.%s %q is not a version", f, v).WithDetails("field", f)
		}
	}
	var matches []daemon.DeviceInfo
	for _, d := range s.Visible(r, s.Registry().List(strings.ToLower(sel.Platform))) {
		if d.Provider != "" || (daemon.CallerFrom(r.Context()).Remote && !s.sharedInfo(d)) {
			continue
		}
		if sel.Matches(d) {
			matches = append(matches, d)
		}
	}
	return matches, nil
}

// disownBoot handles a device a selector lease booted but could not lease
// (a client leased its new serial by id meanwhile, or the client left
// during the boot): the lease that holds
// it shuts it down when it ends, and with no lease on it, it is shut down
// now, unless another daemon has it attached.
func (s *Server) disownBoot(info daemon.DeviceInfo) {
	if info.BootedBy != daemon.BootedByDaemon {
		return
	}
	if s.leases.markBooted(info.ID) {
		s.queueLeases(true)
		logger.Info("lease: %s, booted for a selector lease, went to another lease; that lease shuts it down", info.ID)
		return
	}
	if s.cfg.FindDeviceOwner(info.ID) != nil {
		return
	}
	if err := s.Registry().Stop(info.ID, false); err != nil {
		logger.Warn("lease: shut down %s, booted for a lease that failed: %v", info.ID, err)
	}
}

// bootable reports whether a listed device is a shut-down simulator or AVD.
func bootable(d daemon.DeviceInfo) bool {
	if d.Ready {
		return false
	}
	return strings.EqualFold(d.State, "Shutdown") && (strings.HasPrefix(d.ID, daemon.AVDPrefix) || (d.Platform == "ios" && d.Kind == "simulator"))
}

func (s *Server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	s.startDue()
	leases := s.leases.list()
	visible := leases[:0]
	for _, l := range leases {
		e := s.leases.lookup(l.ID)
		if e == nil {
			continue
		}
		l.Device = s.resolvedDevice(e)
		if !daemon.CallerFrom(r.Context()).Remote || s.shared(l.Device) || s.heldBy(r, e.tableKey()) {
			visible = append(visible, l)
		}
	}
	leases = visible
	daemon.WriteJSON(w, http.StatusOK, daemon.LeasesResult{Envelope: daemon.Envelope{OK: true}, Leases: leases})
}

// visibleLease returns lease id's entry when the caller may see its
// device: a remote caller sees only the shared pool's leases, and any
// other is to it as a lease that does not exist.
func (s *Server) visibleLease(r *http.Request, id string) *leaseEntry {
	e := s.leases.lookup(id)
	if e == nil {
		return nil
	}
	if s.leases.holds(id, e.tableKey()) || s.sharedWith(r, s.resolvedDevice(e)) {
		return e
	}
	return nil
}

func (s *Server) leaseGone(w http.ResponseWriter, id string) {
	// Removal registers its journal revision under leaseMu. Even a
	// concurrent retry must wait for that revision before confirming expiry.
	s.leaseMu.Lock()
	batch := s.queueLeases(false)
	s.leaseMu.Unlock()
	if err := batch.wait(); err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
		return
	}
	daemon.WriteError(w, daemon.Errorf(daemon.CodeLeaseExpired, "lease %s has expired or was released", id).WithDetails("lease", id))
}

func (s *Server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	s.startDue()
	id := r.PathValue("lid")
	l, ok := s.leases.get(id)
	e := s.visibleLease(r, id)
	if !ok || e == nil {
		s.leaseGone(w, id)
		return
	}
	l.Device = s.resolvedDevice(e)
	l.AwaitingAttach = e.awaitingAttach.Load()
	daemon.WriteJSON(w, http.StatusOK, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l})
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	s.startDue()
	id := r.PathValue("lid")
	var req daemon.RenewRequest
	if e := daemon.DecodeBody(r, &req); e != nil {
		daemon.WriteError(w, e)
		return
	}
	if req.TTLSec < 0 {
		daemon.WriteError(w, daemon.Errorf(daemon.CodeUsage, "leases: ttlSec must be positive"))
		return
	}
	le := s.visibleLease(r, id)
	if le == nil {
		s.leaseGone(w, id)
		return
	}
	// A renewal is activity on the lease's cloud device.
	if done, ce := s.CloudBusy(le.device); ce == nil {
		defer done()
	}
	s.leaseMu.Lock()
	l, e := s.leases.renew(id, req.TTLSec)
	s.leaseMu.Unlock()
	if e != nil {
		daemon.WriteError(w, e)
		return
	}
	if !le.cloud {
		if err := s.saveLeases(); err != nil {
			daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
			return
		}
	}
	l.Device = s.resolvedDevice(le)
	l.AwaitingAttach = le.awaitingAttach.Load()
	logger.Info("lease %s: %s renewed until %s", l.ID, l.Device, l.ExpiresAt)
	daemon.WriteJSON(w, http.StatusOK, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l})
}

func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	s.startDue()
	id := r.PathValue("lid")
	e := s.visibleLease(r, id)
	if e == nil {
		s.leaseGone(w, id)
		return
	}
	s.leases.mu.Lock()
	l := e.view()
	s.leases.mu.Unlock()
	l.Device = s.resolvedDevice(e)
	if err := s.endLease(e, endReleased); err != nil {
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
		return
	}
	res := daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: l}
	if e.releaseCloud != nil {
		// The cloud device's release runs in the lease's teardown, at once
		// or once work under the lease unwinds: wait for its minutes a
		// while. A release still pending, or failed, answers without them;
		// device.released carries them either way.
		select {
		case <-e.cloudDone:
			res.DeviceMinutes = e.cloudMinutes
		case <-time.After(cloudLeaseReleaseWait):
		}
	}
	daemon.WriteJSON(w, http.StatusOK, res)
}

// cloudLeaseReleaseWait bounds how long a lease release waits for its cloud
// device's minutes once the lease has ended. A lease with no work in flight
// tears its device down inline first, under the provider release's own
// timeout (cloudTiming.releaseTimeout), so the answer can take longer.
var cloudLeaseReleaseWait = 30 * time.Second

// endLease ends a lease: the device is detached, and shut down when the
// hub booted it for the lease.
func (s *Server) endLease(e *leaseEntry, reason string) error {
	s.leaseMu.Lock()
	teardown := s.endLeaseLocked(e, reason)
	s.leaseMu.Unlock()
	teardown()
	return s.queueLeases(false).wait()
}

// endLeaseLocked removes e with Server.leaseMu held and returns its
// teardown, which the caller runs after unlocking: detaching the device
// (a driver or WDA teardown) and shutting down one the lease booted take
// seconds, and must not hold up every other lease call on the hub. Until
// the teardown is done the device is reserved (leaseTable.ending), so no
// new lease takes it mid-shutdown.
func (s *Server) endLeaseLocked(e *leaseEntry, reason string) (teardown func()) {
	pending := e.pending()
	var removedDevice string
	if reason == endRemoved {
		removedDevice = s.resolvedDevice(e)
	}
	if !s.leases.remove(e, removedDevice) {
		return func() {}
	}
	if e.booted {
		s.Registry().PreserveBoot(e.device, strings.TrimPrefix(e.avd, "android:"))
	}
	if e.durable || e.booted {
		s.queueLeases(true)
	}
	if pending {
		return s.endReservationLocked(e, reason)
	}
	s.leases.mu.Lock()
	key := e.tableKey()
	s.leases.beginEndingLocked(key, e.avd)
	// The teardown of the lease before this one on the device, when it is
	// still running (this one started as that one ended): this teardown
	// waits for it, so the two never detach or shut down side by side.
	before := s.leases.torn[key]
	e.torn = make(chan struct{})
	s.leases.torn[key] = e.torn
	s.leases.mu.Unlock()
	// Counted from here until finish is done, wherever the teardown runs
	// (the caller, the sweeper's goroutine, or after the work under the
	// lease unwound): Shutdown waits for it.
	s.teardowns.add()
	finish := func() {
		defer close(e.torn)
		defer s.teardowns.done()
		defer func() {
			s.leases.mu.Lock()
			s.leases.doneEndingLocked(key, e.avd)
			if s.leases.torn[key] == e.torn {
				delete(s.leases.torn, key)
			}
			s.leases.mu.Unlock()
		}()
		logger.Info("lease %s: %s %s (holder %s)", e.id, e.device, reason, daemon.HolderName(e.holder))
		if device := s.resolvedDevice(e); device != "" {
			s.DetachForStop(device)
		}
		if e.releaseCloud != nil {
			// The lease launched the device: it goes with the lease.
			e.cloudMinutes = e.releaseCloud()
			close(e.cloudDone)
			s.publishLeaseEnd(e, reason)
			return
		}
		if e.booted && !e.stopped.Load() {
			s.dischargeBoot(e.duty(), reason)
		}
		s.publishLeaseEnd(e, reason)
	}
	return func() {
		if before != nil {
			select {
			case <-before:
			default:
				logger.Info("lease %s: %s %s while the lease before it is still torn down; its teardown follows", e.id, e.device, reason)
				go func() {
					<-before
					e.work.Wait()
					finish()
				}()
				return
			}
		}
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

// endReservationLocked ends reservation e, already removed, which has not
// started: nothing runs under it and nothing is attached for it, so its
// teardown neither detaches nor reserves the device, which another lease
// may be using. When e carried the shutdown of a device an earlier lease
// booted, the duty moves to the next lease on the device; with none, and
// no lease started on it, the device is shut down then.
func (s *Server) endReservationLocked(e *leaseEntry, reason string) (teardown func()) {
	logger.Info("lease %s: reservation on %s %s (holder %s)", e.id, e.device, reason, daemon.HolderName(e.holder))
	// The caller publishes the event after releasing Server.leaseMu.
	publish := func() {
		s.publishLeaseEnd(e, reason)
	}
	if !e.booted || reason == endDeleted {
		return publish
	}
	shutdown := s.shutDownBooted(e, reason)
	return func() {
		shutdown()
		publish()
	}
}

// shutDownBooted shuts down the device a reservation e held the shutdown
// of (an earlier lease booted it and handed the duty on) when e ends
// without a lease to pass it to: cancelled, or missed because the hub was
// down or stalled through its window. The device is reserved (ending)
// until the shutdown is done. The returned teardown runs it.
func (s *Server) shutDownBooted(e *leaseEntry, reason string) (teardown func()) {
	s.leases.mu.Lock()
	s.leases.beginEndingLocked(e.device, e.avd)
	s.leases.mu.Unlock()
	s.teardowns.add()
	return func() {
		defer s.teardowns.done()
		defer func() {
			s.leases.mu.Lock()
			s.leases.doneEndingLocked(e.device, e.avd)
			s.leases.mu.Unlock()
		}()
		s.dischargeBoot(e.duty(), reason)
	}
}

// endStoppedLease ends the lease on a device its request (ctx) stopped or
// deleted: the lease the request ran under, and any other on devices. The
// teardown detaches what is left and does not shut the device down again.
// The request's own work on its lease ends first, so unless other work
// runs under the lease, the teardown is done before the request answers.
func (s *Server) endStoppedLease(ctx context.Context, reason string, devices ...string) error {
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
		if reason == endDeleted {
			// A deleted device never comes back under its id: its
			// reservations go with it.
			for _, r := range s.leases.scheduledOn(d) {
				end(r)
			}
		}
	}
	return s.queueLeases(false).wait()
}

// sweepLeases ends every lease whose time ran out. The entries are removed
// under leaseMu; their teardowns run after it, each on its own, and the
// sweep does not wait for them: one slow shutdown never holds up the end
// of a lease that expires meanwhile (the device stays reserved, ending,
// until its teardown is done).
func (s *Server) sweepLeases() {
	s.leaseMu.Lock()
	if s.ShuttingDown() {
		s.leaseMu.Unlock()
		return
	}
	var teardowns []func()
	for _, e := range s.leases.expired() {
		teardowns = append(teardowns, s.endLeaseLocked(e, endExpired))
	}
	// Then the reservations whose start has come, now that the leases
	// before them have ended: their teardowns may still run, and dev()
	// holds the new lease's requests until they are done.
	started, missed := s.leases.startDue(s.removing)
	// A missed reservation that held the shutdown of a device an earlier
	// lease booted passes it on, as a cancel does: to a lease that has
	// started (one may have in this very sweep) or one starting soon, else
	// the device is shut down, reserved (ending) from here.
	for _, e := range missed {
		if !e.booted {
			continue
		}
		s.leases.mu.Lock()
		d := e.duty()
		s.leases.bootDuties[d.key()] = d
		s.leases.mu.Unlock()
		if s.leases.transferDuty(d) {
			continue
		}
		teardowns = append(teardowns, s.shutDownBooted(e, endExpired))
	}
	s.leaseMu.Unlock()
	// Written after unlocking: file I/O never holds up a lease call.
	if err := s.queueLeases(len(started)+len(missed) > 0).wait(); err != nil {
		logger.Warn("persisting lease sweep: %v", err)
	}
	for _, e := range missed {
		logger.Info("lease %s: the window of the reservation on %s passed before it could start", e.id, e.device)
		s.Events().Publish(daemon.Event{Type: "lease.expired", Device: e.device, Message: daemon.HolderName(e.holder)})
	}
	granted := func(e *leaseEntry) {
		logger.Info("lease %s: %s reserved by %s starts until %s", e.id, e.device, daemon.HolderName(e.holder), e.expires.UTC().Format(time.RFC3339))
		s.Events().Publish(daemon.Event{Type: "lease.granted", Device: e.device, Message: daemon.HolderName(e.holder)})
	}
	for _, td := range teardowns {
		go td()
	}
	for _, e := range started {
		// A reservation that starts while the lease before it is still
		// being torn down (it ended in this sweep or an earlier one) is
		// announced after that lease's end, so an event stream sees the
		// device change hands in order.
		s.leases.mu.Lock()
		torn := s.leases.torn[e.device]
		s.leases.mu.Unlock()
		if torn != nil {
			go func(torn chan struct{}, e *leaseEntry) {
				<-torn
				granted(e)
			}(torn, e)
			continue
		}
		granted(e)
	}
}

// startDue runs the sweeper now when a reservation's start has come, so a
// request at the start sees its lease started rather than waiting for the
// next tick.
func (s *Server) startDue() {
	if s.leases.hasDue() {
		s.sweepLeases()
	}
}

// leaseShutdownWait bounds how long a stopping hub waits for its lease
// teardowns: one waiting for work that ignores its cancellation may never
// end.
var leaseShutdownWait = 10 * time.Second

// stopLeases cancels requests and drains teardowns before the daemon
// detaches its devices. Durable grants and reservation boot duties survive
// ordinary shutdown. Cloud sessions are released on either kind of stop.
func (s *Server) stopLeases(endLeases bool) error {
	s.startLeaseTeardowns(endLeases)
	if !s.teardowns.wait(leaseShutdownWait) {
		logger.Warn("lease teardowns still run after %v; stopping beside them", leaseShutdownWait)
	}
	return s.queueLeases(false).wait()
}

func (s *Server) startLeaseTeardowns(endLeases bool) {
	s.leaseMu.Lock()
	s.leases.mu.Lock()
	// Keep pending reservations in the lease journal. On an ordinary stop,
	// live durable grants also survive and their clients reattach after restart.
	all := make([]*leaseEntry, 0, len(s.leases.byDevice))
	for _, e := range s.leases.byID {
		if e.pending() || (!endLeases && e.durable && e.liveAt(s.leases.now())) {
			if e.booted {
				s.Registry().PreserveBoot(e.device, strings.TrimPrefix(e.avd, "android:"))
			}
			if !e.pending() && !e.suspended {
				e.suspended = true
				close(e.ended)
				s.teardowns.add()
				go func() { defer s.teardowns.done(); e.work.Wait() }()
			}
			continue
		}
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
}

// runLeaseSweeper ends expired leases until ctx ends.
func (s *Server) runLeaseSweeper(ctx context.Context) {
	t := time.NewTicker(leaseSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Done():
			return
		case <-t.C:
			go s.retryBootDuties()
			s.DeviceConfig() // also observe edits while only event subscribers are connected
			s.sweepLeases()
		}
	}
}

// checkShareBoot refuses a remote caller's boot or create of a device the
// hub does not share: booting an unshared one is DEVICE_NOT_FOUND, and a
// device may be created only under a name the hub shares.
func (s *Server) CheckBoot(r *http.Request, req daemon.BootRequest) *daemon.Error {
	if !daemon.CallerFrom(r.Context()).Remote {
		return nil
	}
	if req.Create != nil {
		name := req.Create.Name
		if name == "" {
			name = req.Name
		}
		if !s.sharedInfo(daemon.DeviceInfo{ID: name, Name: name, Platform: strings.ToLower(req.Platform)}) {
			shares := s.shareList()
			if shares == "" {
				shares = "nothing"
			}
			return daemon.Forbidden("this hub shares only %s; a remote caller cannot create %q", shares, name).
				WithDetails("field", "name")
		}
		return nil
	}
	// A boot without a platform takes the one Boot infers, so a bare AVD
	// name is android; a simulator's, or one naming both, is not.
	platform := strings.ToLower(req.Platform)
	if platform == "" && len(s.DeviceConfig().Shared) > 0 {
		platform, _ = s.Registry().InferBootPlatform(req.Name)
	}
	if s.sharedInfo(daemon.DeviceInfo{ID: req.Name, Name: strings.TrimPrefix(req.Name, daemon.AVDPrefix), Platform: platform}) || s.shared(req.Name) {
		return nil
	}
	return daemon.Errorf(daemon.CodeDeviceNotFound, "device %s not found", req.Name)
}
