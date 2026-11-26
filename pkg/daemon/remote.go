package daemon

// remote.go: cloud devices. The daemon launches a device through a cloud
// provider (cloudprov.RemoteProvider), holds its Appium driver behind a
// loopback proxy (remoteproxy.go), keeps the provider's session alive while
// it is idle, releases it after its own idle timeout or just before the
// provider's session cap, and records every session in the cloud ledger
// (cloudledger.go) the moment it exists, so a crash never leaves one running
// unseen. A launched device is driven like any other: attach adopts the held
// driver, and steps, flows and the JS client run unchanged.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Cloud device timings. MAESTRO_D_TEST_FAST_CLOUD=1 shortens them for tests
// (cloudTimings).
type cloudTiming struct {
	minIdle, maxIdle, defaultIdle time.Duration
	// keepaliveAfter: idle time before the first ping; keepaliveEvery:
	// between pings.
	keepaliveAfter, keepaliveEvery time.Duration
	// capMargin: how long before the provider's cap the daemon releases;
	// warnWithin: when step results start warning about the cap.
	capMargin, warnWithin time.Duration
	tick                  time.Duration
	releaseTimeout        time.Duration
	// installWait: how long an install waits for its app to appear on the
	// device (the provider answers first; Device Farm's iOS installs land
	// 10-20 s later); installPoll: the check's period.
	installWait, installPoll time.Duration
	// restartRetry: the pause between attempts to reopen the WebDriver
	// session after an install.
	restartRetry time.Duration
	// releaseRetry: the pause between background retries of a release
	// that failed.
	releaseRetry time.Duration
}

// envFastCloud is the test-only seam that lowers the idle-release minimum
// to 1 s and the keepalive interval to 1 s.
const envFastCloud = "MAESTRO_D_TEST_FAST_CLOUD"

func cloudTimings() cloudTiming {
	t := cloudTiming{minIdle: 60 * time.Second, maxIdle: 9000 * time.Second, defaultIdle: 600 * time.Second,
		keepaliveAfter: 60 * time.Second, keepaliveEvery: 60 * time.Second, capMargin: 30 * time.Second,
		warnWithin: 10 * time.Minute, tick: 5 * time.Second, releaseTimeout: 60 * time.Second,
		installWait: 120 * time.Second, installPoll: 2 * time.Second, restartRetry: 2 * time.Second,
		releaseRetry: 30 * time.Second}
	if os.Getenv(envFastCloud) == "1" {
		t.minIdle, t.keepaliveAfter, t.keepaliveEvery, t.tick = time.Second, time.Second, time.Second, 100*time.Millisecond
		t.installWait, t.installPoll, t.restartRetry = 3*time.Second, 100*time.Millisecond, 100*time.Millisecond
		t.releaseRetry = 100 * time.Millisecond
	}
	return t
}

// remoteDevice is a cloud device this daemon holds.
type remoteDevice struct {
	id       string
	provider cloudprov.RemoteProvider
	alloc    *cloudprov.Allocation
	driver   RemoteDriver
	dinfo    DriverInfo
	proxy    *remoteProxy
	appID    string
	idle     time.Duration
	timing   cloudTiming

	// mu guards the clock: requests in flight, the last activity, the
	// last ping and the end state. Idle release checks and marks under it,
	// so a request never starts on a device being released.
	mu           sync.Mutex
	inFlight     int
	lastActivity time.Time
	lastPing     time.Time
	pinging      bool
	ended        string // why the device is gone: released (idle, deleted, …) or ended by the provider

	// driverMu serialises a keepalive ping with a session restart on a
	// device that is not attached.
	driverMu sync.Mutex
	// installMu serialises an install with an attach: an attach waits for
	// an install under way, and an install sees an attach that came first.
	installMu sync.Mutex

	// An independently ended allocation notifies the owner, including when it
	// ends between launch and registration. Both fields are guarded by mu.
	onEnd      func()
	ownerEnded bool

	stop        chan struct{}
	releaseOnce sync.Once
	minutes     *cloudprov.DeviceMinutes
	releaseErr  *Error
	released    chan struct{}
}

// begin counts a request on the device; an error when it is gone.
func (rd *remoteDevice) begin() *Error {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.ended != "" {
		return Errorf(CodeDeviceError, "cloud device %s is gone: %s", rd.id, rd.ended).WithDetails("reason", rd.ended)
	}
	rd.inFlight++
	return nil
}

// end ends a request begin counted; the idle clock restarts when the last
// one ends.
func (rd *remoteDevice) end() {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.inFlight > 0 {
		rd.inFlight--
	}
	if rd.inFlight == 0 {
		rd.lastActivity = time.Now()
	}
}

// due decides, under the clock's lock, whether rd is released for idleness
// now (and marks it gone, so a request that starts after finds it so) or
// is due a keepalive ping. Requests in flight hold both off.
func (rd *remoteDevice) due(now time.Time) (release, ping bool) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.inFlight > 0 || rd.ended != "" {
		return false, false
	}
	since := now.Sub(rd.lastActivity)
	if since >= rd.idle {
		rd.ended = "released: idle"
		return true, false
	}
	return false, since >= rd.timing.keepaliveAfter && now.Sub(rd.lastPing) >= rd.timing.keepaliveEvery
}

// idleAt is when the device is released if nothing happens.
func (rd *remoteDevice) idleAt() time.Time {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.inFlight > 0 {
		return time.Now().Add(rd.idle)
	}
	return rd.lastActivity.Add(rd.idle)
}

// ---------------------------------------------------------------------------
// The server's cloud devices

// remote returns the cloud device id this daemon holds, or nil.
func (s *Server) remote(id string) *remoteDevice {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	return s.remotes[id]
}

// CloudBusy counts a request on id when it is a cloud device: the idle clock
// stops until done is called. It never takes the device's step lock. For
// any other device it does nothing.
func (s *Server) CloudBusy(id string) (done func(), e *Error) {
	rd := s.remote(id)
	if rd == nil {
		return func() {}, nil
	}
	if e := rd.begin(); e != nil {
		return nil, e
	}
	return rd.end, nil
}

// cloudGone is DEVICE_ERROR with the reason for a cloud device whose
// session is over (ended by its provider, or being released); nil for any
// other device.
func (s *Server) cloudGone(id string) *Error {
	rd := s.remote(id)
	if rd == nil {
		return nil
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.ended == "" {
		return nil
	}
	return Errorf(CodeDeviceError, "cloud device %s is gone: %s", rd.id, rd.ended).WithDetails("reason", rd.ended)
}

// cloudWarnings are the notes every step result on id carries: the
// provider's session cap within warnWithin.
func (s *Server) cloudWarnings(id string) []string {
	rd := s.remote(id)
	if rd == nil || rd.alloc.Expires.IsZero() {
		return nil
	}
	if time.Until(rd.alloc.Expires) > rd.timing.warnWithin {
		return nil
	}
	return []string{fmt.Sprintf("cloud session ends at %s", rd.alloc.Expires.UTC().Format(time.RFC3339))}
}

// decorateCloud sets the live fields of the cloud devices in devs.
func (s *Server) decorateCloud(devs []DeviceInfo) {
	for i := range devs {
		if devs[i].Provider == "" {
			continue
		}
		if rd := s.remote(devs[i].LocalIDOrID()); rd != nil {
			devs[i].IdleReleaseAt = rd.idleAt().UTC().Format(time.RFC3339)
		}
	}
}

// LocalIDOrID is the device's id on its own hub's machine.
func (d DeviceInfo) LocalIDOrID() string {
	if d.LocalID != "" {
		return d.LocalID
	}
	return d.ID
}

// validateCloudSpec fills defaults into spec and checks it.
func validateCloudSpec(spec *cloudprov.CloudSpec, t cloudTiming) *Error {
	if spec.Provider == "" {
		spec.Provider = "devicefarm"
	}
	spec.Provider = strings.ToLower(spec.Provider)
	spec.Platform = strings.ToLower(spec.Platform)
	switch spec.Platform {
	case "android", "ios":
	case "":
		return Errorf(CodeUsage, "a cloud device needs a platform: android or ios").WithDetails("field", "platform")
	default:
		return Errorf(CodeUsage, "cloud platform %q is not android or ios", spec.Platform).WithDetails("field", "platform")
	}
	if spec.IdleReleaseSec == 0 {
		spec.IdleReleaseSec = int(t.defaultIdle / time.Second)
	}
	if d := time.Duration(spec.IdleReleaseSec) * time.Second; d < t.minIdle || d > t.maxIdle {
		return Errorf(CodeUsage, "idleReleaseSec %d is outside %d–%d", spec.IdleReleaseSec, int(t.minIdle/time.Second), int(t.maxIdle/time.Second)).
			WithDetails("field", "idleReleaseSec")
	}
	if len(spec.AuxiliaryApps) > 3 {
		return Errorf(CodeUsage, "at most 3 auxiliary apps").WithDetails("field", "auxiliaryApps")
	}
	if spec.AppiumVersion != "" && spec.AppiumVersion != "2" && spec.AppiumVersion != "3" {
		return Errorf(CodeUsage, "appiumVersion %q is not 2 or 3", spec.AppiumVersion).WithDetails("field", "appiumVersion")
	}
	if spec.BootTimeoutSec < 0 {
		return Errorf(CodeUsage, "bootTimeoutSec must not be negative").WithDetails("field", "bootTimeoutSec")
	}
	return nil
}

// newCloudID is a launched device's id: <prefix>-<8 hex>.
func newCloudID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// launchCloud launches a cloud device for spec (already validated) and
// holds it. The provider records every session in the ledger through the
// Created hook before it waits, and stops every session it gives up on.
// ctx is the request's: a client that leaves cancels the launch.
func (s *Server) launchCloud(ctx context.Context, spec cloudprov.CloudSpec, reserve ...func(id string)) (*remoteDevice, *Error) {
	p, e := s.provider(spec.Provider)
	if e != nil {
		return nil, e
	}
	if s.deps.NewRemoteDriver == nil {
		return nil, Errorf(CodeInternal, "cloud devices are not configured")
	}
	t := cloudTimings()
	ctx, unjoin := s.joinLaunches(ctx)
	defer unjoin()
	id := newCloudID(p.IDPrefix())
	pid := os.Getpid()
	hooks := cloudprov.AllocateHooks{
		DeviceID: id,
		Progress: func(state string) {
			s.events.Publish(Event{Type: "device.allocating", Device: id, Message: state})
		},
		Created: func(ref string) error {
			return addLedgerEntry(CloudEntry{Daemon: s.cfg.Name, Hub: s.cfg.HubProcess, PID: pid, ID: id, Provider: spec.Provider, ProviderRef: ref, StartedAt: nowRFC3339()})
		},
		Abandoned: func(ref string) {
			if err := removeLedgerEntry(ref); err != nil {
				logger.Warn("cloud ledger: %v", err)
			}
		},
	}
	logger.Info("Launching a %s %s device %s", spec.Provider, spec.Platform, id)
	alloc, err := p.Allocate(ctx, spec, hooks)
	if err != nil {
		logger.Warn("launch %s: %s", id, p.Redact(err.Error()))
		// A session the provider could not stop as it gave up is still in
		// the ledger under this launch: release it in the background.
		if entries, lerr := ReadLedger(); lerr == nil {
			for _, en := range entries {
				if en.ID == id && en.PID == pid {
					go s.retryReleaseRef(p, id, en.ProviderRef, time.Time{})
				}
			}
		}
		return nil, cloudError(err)
	}
	// From here the session exists: every failure releases it, retrying
	// in the background when the provider refuses.
	fail := func(e *Error) (*remoteDevice, *Error) {
		rctx, cancel := context.WithTimeout(context.Background(), t.releaseTimeout)
		defer cancel()
		if _, rerr := p.Release(rctx, alloc); rerr != nil {
			logger.Warn("launch %s: releasing the session: %s (retrying in the background)", id, p.Redact(rerr.Error()))
			go s.retryReleaseRef(p, id, alloc.Ref, alloc.Expires)
		} else if lerr := removeLedgerEntry(alloc.Ref); lerr != nil {
			logger.Warn("cloud ledger: %v", lerr)
		}
		return nil, e
	}
	proxy, err := newRemoteProxy(alloc.Endpoint)
	if err != nil {
		return fail(WrapErr(CodeDeviceError, err))
	}
	driver, dinfo, err := s.deps.NewRemoteDriver(ctx, proxy.url, alloc.Caps)
	if err != nil {
		proxy.close()
		msg := p.Redact(err.Error())
		logger.Warn("launch %s: WebDriver session: %s", id, msg)
		return fail(Errorf(CodeDeviceError, "cloud device %s: the WebDriver session did not open: %s", id, msg))
	}
	dinfo.Platform, dinfo.Kind, dinfo.Name = spec.Platform, "cloud", alloc.Device.Model
	dinfo.Device.ID, dinfo.Device.Name, dinfo.Device.Platform, dinfo.Device.OSVersion = id, alloc.Device.Model, spec.Platform, alloc.Device.OSVersion
	dinfo.App.ID = spec.AppID
	now := time.Now()
	rd := &remoteDevice{id: id, provider: p, alloc: alloc, driver: driver, dinfo: dinfo, proxy: proxy, appID: spec.AppID,
		idle: time.Duration(spec.IdleReleaseSec) * time.Second, timing: t, lastActivity: now,
		stop: make(chan struct{}), released: make(chan struct{})}
	if ctx.Err() != nil {
		rd.closeDriver()
		return fail(Errorf(CodeInterrupted, "launch %s cancelled", id))
	}
	info := s.cloudInfo(rd, spec)
	for _, f := range reserve {
		f(id)
	}
	s.remoteMu.Lock()
	if s.shuttingDown.Load() {
		// The teardown has run, or is about to (it reads s.remotes under
		// remoteMu after setting shuttingDown): this device would outlive it.
		s.remoteMu.Unlock()
		rd.closeDriver()
		return fail(Errorf(CodeDaemonUnavailable, "daemon is shutting down"))
	}
	if s.remotes == nil {
		s.remotes = map[string]*remoteDevice{}
	}
	s.remotes[id] = rd
	s.remoteMu.Unlock()
	s.registry.PutRecord(info)
	// A held cloud device holds the daemon's idle timer: its own idle
	// release decides when it goes.
	s.idle.Hold()
	go s.watchRemote(rd)
	logger.Info("Launched cloud device %s: %s %s (%s, Appium %s)", id, info.Name, info.OSVersion, spec.Provider, info.AppiumVersion)
	return rd, nil
}

// cloudInfo is a launched device's listing entry.
func (s *Server) cloudInfo(rd *remoteDevice, spec cloudprov.CloudSpec) DeviceInfo {
	a := rd.alloc
	return DeviceInfo{
		ID: rd.id, Platform: spec.Platform, Kind: "cloud", Name: a.Device.Model, Model: a.Device.Model, OSVersion: a.Device.OSVersion,
		State: "allocated", Ready: true, BootedBy: BootedByDaemon, BootedAt: nowRFC3339(), CreatedBy: CreatedByMaestroD,
		AppID: spec.AppID, Provider: spec.Provider, ProviderRef: a.Ref, DashboardURL: a.Device.DashboardURL,
		ExpiresAt: a.Expires.UTC().Format(time.RFC3339), IdleReleaseSec: spec.IdleReleaseSec,
		FormFactor: a.Device.FormFactor, Manufacturer: a.Device.Manufacturer, Machine: MachineName(),
		AppiumVersion: a.Device.AppiumVersion,
	}
}

// watchRemote keeps rd alive while it is idle and releases it after its
// idle timeout or just before the provider's session cap.
func (s *Server) watchRemote(rd *remoteDevice) {
	tick := time.NewTicker(rd.timing.tick)
	defer tick.Stop()
	for {
		select {
		case <-rd.stop:
			return
		case now := <-tick.C:
			if !rd.alloc.Expires.IsZero() && !now.Before(rd.alloc.Expires.Add(-rd.timing.capMargin)) {
				go s.releaseRemote(rd, "session-limit")
				return
			}
			release, ping := rd.due(now)
			if release {
				go s.releaseRemote(rd, "idle")
				return
			}
			if ping {
				s.pingRemote(rd)
			}
		}
	}
}

// pingRemote sends a keepalive. It never overlaps another use of the
// driver: on an attached device it takes the step lock without waiting
// (a step in flight is activity enough), and it always holds driverMu,
// which a session restart takes too. A failed ping ends the device only
// when the provider says the session is no longer running.
func (s *Server) pingRemote(rd *remoteDevice) {
	a := s.getAttached(rd.id)
	if a != nil {
		rd.mu.Lock()
		rd.pinging = true
		rd.mu.Unlock()
		defer func() {
			rd.mu.Lock()
			rd.pinging = false
			rd.mu.Unlock()
		}()
		if !a.busy.TryLock() {
			return
		}
		defer a.busy.Unlock()
	}
	rd.driverMu.Lock()
	err := rd.driver.Keepalive()
	rd.driverMu.Unlock()
	rd.mu.Lock()
	rd.lastPing = time.Now()
	rd.mu.Unlock()
	if err != nil {
		logger.Info("keepalive %s: %s", rd.id, rd.provider.Redact(err.Error()))
		s.checkRemote(rd)
		return
	}
	logger.Debug("keepalive %s", rd.id)
}

// isPinging reports whether a keepalive holds id's step lock right now.
func (s *Server) isPinging(id string) bool {
	rd := s.remote(id)
	if rd == nil {
		return false
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.pinging
}

// checkRemote asks the provider whether rd's session still runs; when it
// does not, the device ends. It reports whether the device ended.
func (s *Server) checkRemote(rd *remoteDevice) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, reason, err := rd.provider.Refresh(ctx, rd.alloc)
	if err != nil {
		// A session the provider no longer knows has ended too.
		if k := cloudprov.KindOf(err); k != cloudprov.Ended && k != cloudprov.NotFound {
			logger.Info("refresh %s: %s", rd.id, rd.provider.Redact(err.Error()))
			return false
		}
		state, reason = "COMPLETED", rd.provider.Redact(err.Error())
	}
	if state == "RUNNING" {
		return false
	}
	if reason == "" {
		reason = "the provider ended the session (" + strings.ToLower(state) + ")"
	}
	s.endRemote(rd, reason)
	return true
}

// endRemote marks rd ended by the provider: its record stays, state ended,
// so later requests answer DEVICE_ERROR with the reason until it is
// deleted.
func (s *Server) endRemote(rd *remoteDevice, reason string) {
	rd.mu.Lock()
	if rd.ended != "" {
		rd.mu.Unlock()
		return
	}
	rd.ended = "session ended: " + reason
	rd.mu.Unlock()
	s.registry.UpdateRecord(rd.id, func(d *DeviceInfo) {
		d.State, d.Ready, d.EndedReason = "ended", false, reason
	})
	s.events.Publish(Event{Type: "device.ended", Device: rd.id, Message: reason})
	logger.Info("cloud device %s ended: %s", rd.id, reason)
	go func() {
		// Read the minutes and drop the ledger entry; the session is over.
		s.finishRemote(rd, "ended", false)
		rd.notifyOwner("ended")
	}()
}

// releaseRemote releases rd (once) and reports the device minutes. reason
// is idle, session-limit, deleted, stopped, daemon-exit or lease-end.
func (s *Server) releaseRemote(rd *remoteDevice, reason string) (*cloudprov.DeviceMinutes, *Error) {
	rd.mu.Lock()
	if rd.ended == "" {
		rd.ended = "released: " + reason
	}
	rd.mu.Unlock()
	m, e := s.finishRemote(rd, reason, true)
	rd.notifyOwner(reason)
	return m, e
}

// notifyOwner reports a provider or idle release. The owner handles an explicit
// stop, delete, or lease release in the request that initiated it.
func (rd *remoteDevice) notifyOwner(reason string) {
	if reason == "lease-end" || reason == "stopped" || reason == "deleted" {
		return
	}
	rd.mu.Lock()
	rd.ownerEnded = true
	notify := rd.onEnd
	rd.onEnd = nil
	rd.mu.Unlock()
	if notify != nil {
		notify()
	}
}

func (rd *remoteDevice) setOwner(onEnd func()) {
	rd.mu.Lock()
	ended := rd.ownerEnded
	if !ended {
		rd.onEnd = onEnd
	}
	rd.mu.Unlock()
	if ended {
		// Registration runs under the lease grant lock.
		go onEnd()
	}
}

// finishRemote ends rd's session and everything the daemon held for it:
// the attachment, the WebDriver session, the proxy, the ledger entry and,
// when drop, its record. It runs once; later calls wait for the first.
func (s *Server) finishRemote(rd *remoteDevice, reason string, drop bool) (*cloudprov.DeviceMinutes, *Error) {
	rd.releaseOnce.Do(func() {
		defer close(rd.released)
		close(rd.stop)
		s.DetachForStop(rd.id)
		rd.closeDriver()
		ctx, cancel := context.WithTimeout(context.Background(), rd.timing.releaseTimeout)
		defer cancel()
		res, err := rd.provider.Release(ctx, rd.alloc)
		if err != nil {
			msg := rd.provider.Redact(err.Error())
			logger.Warn("release %s: %s (retrying in the background; kept in the cloud ledger until it succeeds)", rd.id, msg)
			rd.releaseErr = Errorf(CodeDeviceError, "cloud device %s: releasing its session failed: %s; the daemon keeps retrying", rd.id, msg)
			go s.retryRelease(rd)
		} else {
			if res != nil {
				rd.minutes = res.Minutes
			}
			if lerr := removeLedgerEntry(rd.alloc.Ref); lerr != nil {
				logger.Warn("cloud ledger: %v", lerr)
			}
		}
		s.idle.Release()
		logger.Info("Released cloud device %s (%s)%s", rd.id, reason, minutesNote(rd.minutes))
		s.events.Publish(Event{Type: "device.released", Device: rd.id, Message: reason, DeviceMinutes: rd.minutes})
	})
	<-rd.released
	if drop || rd.ended == "" {
		s.forgetRemote(rd)
	}
	return rd.minutes, rd.releaseErr
}

// retryRelease keeps releasing rd's session after its release failed, every
// releaseRetry, until the provider confirms (a session that already ended
// counts) or the session is long past its cap, when the provider has ended
// it. Its ledger entry stays until then, so a daemon that dies meanwhile
// leaves it to the next start's self-release and to device stop --orphans.
func (s *Server) retryRelease(rd *remoteDevice) {
	s.retryReleaseRef(rd.provider, rd.id, rd.alloc.Ref, rd.alloc.Expires)
}

// retryReleaseRef is retryRelease for session ref of device id, which
// expires at expires (zero when not known).
func (s *Server) retryReleaseRef(p cloudprov.RemoteProvider, id, ref string, expires time.Time) {
	t := cloudTimings()
	deadline := expires.Add(10 * time.Minute)
	if expires.IsZero() {
		deadline = time.Now().Add(cloudprovSessionLimit)
	}
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return
		case <-time.After(t.releaseRetry):
		}
		ctx, cancel := context.WithTimeout(context.Background(), t.releaseTimeout)
		minutes, err := p.ReleaseRef(ctx, ref)
		cancel()
		if err != nil {
			logger.Warn("release %s (retry): %s", id, p.Redact(err.Error()))
			continue
		}
		if lerr := removeLedgerEntry(ref); lerr != nil {
			logger.Warn("cloud ledger: %v", lerr)
		}
		logger.Info("Released cloud device %s on a retry%s", id, minutesNote(minutes))
		s.events.Publish(Event{Type: "device.released", Device: id, Message: "retried", DeviceMinutes: minutes})
		return
	}
	logger.Warn("release %s: gave up retrying past the session cap; kept in the cloud ledger for device stop --orphans", id)
}

// joinLaunches returns ctx, also cancelled when the daemon shuts down.
func (s *Server) joinLaunches(ctx context.Context) (context.Context, func()) {
	if s.launches == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.launches, cancel)
	return ctx, func() { stop(); cancel() }
}

// cloudprovSessionLimit bounds the retries of a session whose cap is not
// known: no provider session outlives it.
const cloudprovSessionLimit = 3 * time.Hour

// forgetRemote drops rd from the server and the registry.
func (s *Server) forgetRemote(rd *remoteDevice) {
	s.remoteMu.Lock()
	if s.remotes[rd.id] == rd {
		delete(s.remotes, rd.id)
	}
	s.remoteMu.Unlock()
	s.registry.DropRecord(rd.id)
}

// closeDriver ends the WebDriver session and the proxy.
func (rd *remoteDevice) closeDriver() {
	rd.driverMu.Lock()
	defer rd.driverMu.Unlock()
	if rd.driver != nil {
		if err := rd.driver.Close(); err != nil {
			logger.Info("close %s: %s", rd.id, rd.provider.Redact(err.Error()))
		}
	}
	rd.proxy.close()
}

func minutesNote(m *cloudprov.DeviceMinutes) string {
	if m == nil {
		return ""
	}
	if m.Estimated {
		return fmt.Sprintf(", about %g device minutes so far (estimated; the provider bills its cleanup on top)", m.Total)
	}
	return fmt.Sprintf(", %g device minutes", m.Total)
}

// releaseAllRemote releases every cloud device, at daemon exit.
func (s *Server) releaseAllRemote(reason string) {
	s.remoteMu.Lock()
	all := make([]*remoteDevice, 0, len(s.remotes))
	for _, rd := range s.remotes {
		all = append(all, rd)
	}
	s.remoteMu.Unlock()
	var wg sync.WaitGroup
	for _, rd := range all {
		wg.Add(1)
		go func(rd *remoteDevice) {
			defer wg.Done()
			_, _ = s.releaseRemote(rd, reason)
		}(rd)
	}
	wg.Wait()
}

// selfRelease releases the sessions the ledger holds for this daemon's name
// from an earlier run (a crash, kill -9, a launchd restart): every entry of
// this name whose pid is not this process's. It runs after the daemon is
// serving, so a spawning client never waits on it.
func (s *Server) selfRelease() {
	entries, err := ReadLedger()
	if err != nil {
		logger.Warn("cloud ledger: %v", err)
		return
	}
	pid := os.Getpid()
	for _, e := range entries {
		if e.Daemon != s.cfg.Name || e.Hub != s.cfg.HubProcess || e.PID == pid {
			continue
		}
		if _, ok := s.deps.Providers[e.Provider]; !ok {
			continue // left for device stop --orphans
		}
		p, pe := s.provider(e.Provider)
		if pe != nil {
			logger.Warn("cloud device %s left by an earlier run: %s", e.ID, pe.Message)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), cloudTimings().releaseTimeout)
		minutes, err := p.ReleaseRef(ctx, e.ProviderRef)
		cancel()
		if err != nil {
			logger.Warn("cloud device %s left by an earlier run: %s", e.ID, p.Redact(err.Error()))
			continue
		}
		if err := removeLedgerEntry(e.ProviderRef); err != nil {
			logger.Warn("cloud ledger: %v", err)
		}
		logger.Info("released orphaned cloud device %s (started %s)%s", e.ID, e.StartedAt, minutesNote(minutes))
		s.events.Publish(Event{Type: "device.released", Device: e.ID, Message: "orphaned", DeviceMinutes: minutes})
	}
}

// ---------------------------------------------------------------------------
// Handlers

// handleCreateCloud launches a cloud device for POST /v1/devices with
// create.cloud: direct mode, which a plain daemon offers its local callers
// only. A hub launches cloud devices for leases alone.
func (s *Server) handleCreateCloud(w http.ResponseWriter, r *http.Request, req BootRequest) {
	if s.cfg.Hub != nil {
		e := Forbidden("launch cloud devices through a lease: maestro-d lease --location cloud --platform ios|android …")
		s.LogForbidden(r, e)
		WriteError(w, e)
		return
	}
	if CallerFrom(r.Context()).Remote {
		e := Forbidden("only a local caller launches cloud devices on a daemon (the daemon's own AWS credentials pay for them)")
		s.LogForbidden(r, e)
		WriteError(w, e)
		return
	}
	spec := *req.Create.Cloud
	c := req.Create
	for f, set := range map[string]bool{"name": c.Name != "", "type": c.Type != "", "runtime": c.Runtime != "", "image": c.Image != "",
		"profile": c.Profile != "", "data-size": c.DataSize != "", "install-image": c.InstallImage, "accept-licenses": c.AcceptLicenses} {
		if set {
			WriteError(w, Errorf(CodeUsage, "device create: --%s does not apply to a cloud device", f).WithDetails("field", f))
			return
		}
	}
	if req.Boot != nil && !*req.Boot {
		WriteError(w, Errorf(CodeUsage, "a cloud device always runs: boot false does not apply").WithDetails("field", "boot"))
		return
	}
	top := strings.ToLower(req.Platform)
	switch {
	case spec.Platform == "":
		spec.Platform = top
	case top != "" && !strings.EqualFold(top, spec.Platform):
		WriteError(w, Errorf(CodeUsage, "platform %s and cloud.platform %s differ", top, spec.Platform).WithDetails("field", "platform"))
		return
	}
	if e := validateCloudSpec(&spec, cloudTimings()); e != nil {
		WriteError(w, e)
		return
	}
	// A blob ref is materialized for the provider's upload.
	tmp, e := s.materializeCloudApps(&spec)
	if e != nil {
		WriteError(w, e)
		return
	}
	if tmp != "" {
		defer os.RemoveAll(tmp)
	}
	s.idle.Hold()
	defer s.idle.Release()
	rd, e := s.launchCloud(r.Context(), spec)
	if e != nil {
		WriteError(w, e)
		return
	}
	info, _ := s.registry.Lookup(rd.id)
	info.Machine = MachineName()
	devs := []DeviceInfo{info}
	SetLocations(devs, s.callerMachine(r))
	s.decorateCloud(devs)
	s.events.Publish(Event{Type: "device.created", Device: rd.id, Message: info.Name})
	s.events.Publish(Event{Type: "device.booted", Device: rd.id, Message: info.Name})
	WriteJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: devs[0]})
}

// materializeCloudApps turns blob refs among spec's apps into files in a
// temporary directory, which the caller removes once the launch is done.
func (s *Server) materializeCloudApps(spec *cloudprov.CloudSpec) (string, *Error) {
	refs := append([]*string{&spec.App}, nil...)
	for i := range spec.AuxiliaryApps {
		refs = append(refs, &spec.AuxiliaryApps[i])
	}
	dir := ""
	for _, ref := range refs {
		if !strings.HasPrefix(*ref, "blob:") {
			continue
		}
		if dir == "" {
			_ = os.MkdirAll(s.cfg.ReportsDir, 0o755)
			d, err := os.MkdirTemp(s.cfg.ReportsDir, ".cloud-apps-")
			if err != nil {
				return "", WrapErr(CodeInternal, err)
			}
			dir = d
		}
		local, err := s.blobs.Materialize(*ref, dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", Errorf(CodeUsage, "app %s: %v", *ref, err).WithDetails("field", "app")
		}
		*ref = local
	}
	return dir, nil
}

// handleStopCloud stops (releases) a cloud device for DELETE
// /v1/devices/{id}, with or without ?delete=true: both end its session.
// The answer is the device, with the minutes the session used.
func (s *Server) handleStopCloud(w http.ResponseWriter, r *http.Request, rd *remoteDevice, del bool) {
	info, _ := s.registry.Lookup(rd.id)
	reason := "stopped"
	if del {
		reason = "deleted"
	}
	minutes, e := s.releaseRemote(rd, reason)
	s.forgetRemote(rd)
	if err := s.deviceStopped(r.Context(), del, rd.id); err != nil {
		WriteError(w, err)
		return
	}
	s.events.Publish(Event{Type: "device.stopped", Device: rd.id})
	if del {
		s.events.Publish(Event{Type: "device.deleted", Device: rd.id, Message: info.Name})
	}
	if e != nil {
		WriteError(w, e)
		return
	}
	info.State, info.Ready, info.Attached, info.DeviceMinutes = "released", false, false, minutes
	WriteJSON(w, http.StatusOK, DeviceResult{Envelope: Envelope{OK: true}, Device: info})
}

// handleProviderSessions serves GET /v1/providers/{provider}/sessions: the
// provider's sessions, newest first.
func (s *Server) handleProviderSessions(w http.ResponseWriter, r *http.Request) {
	p, e := s.provider(r.PathValue("provider"))
	if e != nil {
		WriteError(w, e)
		return
	}
	s.idle.Hold()
	defer s.idle.Release()
	list, err := p.Sessions(r.Context())
	if err != nil {
		WriteError(w, cloudError(err))
		return
	}
	if list == nil {
		list = []cloudprov.SessionSummary{}
	}
	WriteJSON(w, http.StatusOK, ProviderSessionsResult{Envelope: Envelope{OK: true}, Provider: p.Name(), Hub: s.hubLabel(), Sessions: list})
}

// ProviderSessionsResult answers GET /v1/providers/{provider}/sessions.
type ProviderSessionsResult struct {
	Envelope
	Provider string                     `json:"provider"`
	Hub      string                     `json:"hub,omitempty"`
	Sessions []cloudprov.SessionSummary `json:"sessions"`
}

// hubLabel is the hub's name on a hub, "" on a plain daemon.
func (s *Server) hubLabel() string {
	if s.cfg.Hub != nil {
		return s.HubName()
	}
	return ""
}

// PrepareCloudLease validates the requested cloud device and checks that its
// provider is available before a hub reserves launch capacity.
func (s *Server) PrepareCloudLease(sel LeaseSelector, cloud *cloudprov.CloudSpec) (cloudprov.CloudSpec, *Error) {
	spec := cloudprov.CloudSpec{}
	if cloud != nil {
		spec = *cloud
	}
	if spec.App != "" || len(spec.AuxiliaryApps) > 0 {
		return spec, Errorf(CodeUsage, "lease: a cloud lease takes no app; install it once the device is leased (install, POST /v1/devices/{id}/apps)").
			WithDetails("field", "cloud.app")
	}
	spec.Provider = firstNonEmpty(strings.ToLower(sel.Provider), strings.ToLower(spec.Provider), "devicefarm")
	spec.Platform = firstNonEmpty(strings.ToLower(sel.Platform), strings.ToLower(spec.Platform))
	spec.Name = firstNonEmpty(sel.Name, spec.Name)
	spec.OSVersion = firstNonEmpty(sel.OSVersion, spec.OSVersion)
	spec.OSVersionMin = firstNonEmpty(sel.OSVersionMin, spec.OSVersionMin)
	spec.OSVersionMax = firstNonEmpty(sel.OSVersionMax, spec.OSVersionMax)
	if e := validateCloudSpec(&spec, cloudTimings()); e != nil {
		return spec, e
	}
	if _, e := s.provider(spec.Provider); e != nil {
		return spec, e
	}
	return spec, nil
}

// CloudAllocation identifies one specific launch. Its release callbacks remain
// bound to that allocation even after another device reuses its id.
type CloudAllocation struct {
	ID      string
	OnEnd   func(func())
	Release func(forget bool) *cloudprov.DeviceMinutes
}

// LaunchCloudAllocation reserves an id before the device appears in the catalog.
func (s *Server) LaunchCloudAllocation(ctx context.Context, spec cloudprov.CloudSpec, reserve func(string)) (*CloudAllocation, *Error) {
	rd, e := s.launchCloud(ctx, spec, reserve)
	if e != nil {
		return nil, e
	}
	return &CloudAllocation{
		ID:    rd.id,
		OnEnd: rd.setOwner,
		Release: func(forget bool) *cloudprov.DeviceMinutes {
			minutes, _ := s.releaseRemote(rd, "lease-end")
			if forget {
				s.forgetRemote(rd)
			}
			return minutes
		},
	}, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Installing apps

// handleInstallCloud installs the app at path (already materialized) into
// cloud device rd for POST /v1/devices/{id}/apps. The whole install counts
// on the device's idle clock. The upload runs first. Then the install holds
// the device's installMu, which an attach also takes, and on an attached
// device the step lock (honouring ?wait=false), so the install and the
// WebDriver session's restart wait for any step in flight, and no step or
// attach starts while they run.
func (s *Server) handleInstallCloud(w http.ResponseWriter, r *http.Request, rd *remoteDevice, path string) {
	done, e := s.CloudBusy(rd.id)
	if e != nil {
		WriteError(w, e)
		return
	}
	defer done()
	s.idle.Hold()
	defer s.idle.Release()
	ref, err := rd.provider.Upload(r.Context(), path)
	if err != nil {
		WriteError(w, cloudError(err))
		return
	}
	rd.installMu.Lock()
	defer rd.installMu.Unlock()
	if a := s.getAttached(rd.id); a != nil {
		if !s.acquire(w, r, a) {
			return
		}
		defer s.release(a)
	}
	appID := s.appIDOf(path)
	if e := s.installRef(r.Context(), rd, *ref, appID); e != nil {
		WriteError(w, e)
		return
	}
	logger.Info("Installed %s (%s) on cloud device %s for %s", filepath.Base(path), appID, rd.id, DescribeCaller(CallerFrom(r.Context())))
	s.events.Publish(Event{Type: "app.installed", Device: rd.id, Message: appID})
	WriteJSON(w, http.StatusOK, InstallResult{Envelope: Envelope{OK: true}, Device: rd.id, AppID: appID})
}

// installCloud installs path into rd while it is being attached (attach's
// appFile): nothing else drives it yet. It returns the app's id.
func (s *Server) installCloud(ctx context.Context, rd *remoteDevice, path string, _ *attached) (string, *Error) {
	done, e := s.CloudBusy(rd.id)
	if e != nil {
		return "", e
	}
	defer done()
	ref, err := rd.provider.Upload(ctx, path)
	if err != nil {
		return "", cloudError(err)
	}
	appID := s.appIDOf(path)
	if e := s.installRef(ctx, rd, *ref, appID); e != nil {
		return "", e
	}
	s.events.Publish(Event{Type: "app.installed", Device: rd.id, Message: appID})
	return appID, nil
}

// installRef installs an uploaded app into rd's session, waits until the
// device has it (when its id is known), and restarts the WebDriver
// session, so the next steps see the app. The checks and the restart hold
// driverMu, which a keepalive ping takes too.
func (s *Server) installRef(ctx context.Context, rd *remoteDevice, ref cloudprov.AppRef, appID string) *Error {
	if _, err := rd.provider.Install(ctx, rd.alloc, ref); err != nil {
		return cloudError(err)
	}
	if e := s.waitInstalled(ctx, rd, appID); e != nil {
		return e
	}
	// Right after an install the device can refuse a new session for a
	// moment (Android: "UiAutomation not connected"), so the restart is
	// tried a few times.
	t := cloudTimings()
	var err error
	for attempt := 1; attempt <= restartAttempts; attempt++ {
		rd.driverMu.Lock()
		err = rd.driver.RestartSession()
		rd.driverMu.Unlock()
		if err == nil {
			return nil
		}
		if attempt == restartAttempts {
			break
		}
		logger.Info("cloud device %s: restarting the WebDriver session after the install (attempt %d): %s", rd.id, attempt, rd.provider.Redact(err.Error()))
		select {
		case <-ctx.Done():
			return Errorf(CodeDeviceError, "cloud device %s: the install was cancelled while the WebDriver session restarted", rd.id)
		case <-time.After(t.restartRetry):
		}
	}
	return Errorf(CodeDeviceError, "cloud device %s: restarting the WebDriver session after the install: %s", rd.id, rd.provider.Redact(err.Error()))
}

// restartAttempts is how many times an install tries to reopen the
// WebDriver session.
const restartAttempts = 3

// waitInstalled polls the device until appID is installed. Without an app
// id, or when the device cannot answer (a check that fails or does not
// answer a boolean), it returns at once: the WebDriver restart and the next
// step report a missing app then.
func (s *Server) waitInstalled(ctx context.Context, rd *remoteDevice, appID string) *Error {
	if appID == "" {
		return nil
	}
	t := cloudTimings()
	deadline := time.Now().Add(t.installWait)
	for {
		rd.driverMu.Lock()
		ok, err := rd.driver.AppInstalled(appID)
		rd.driverMu.Unlock()
		if err != nil {
			logger.Info("cloud device %s: cannot tell whether %s is installed (%s); not waiting", rd.id, appID, rd.provider.Redact(err.Error()))
			return nil
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return Errorf(CodeDeviceError, "cloud device %s: %s accepted the install, but %s was not on the device after %s",
				rd.id, rd.provider.Name(), appID, t.installWait.Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return Errorf(CodeDeviceError, "cloud device %s: the install of %s was cancelled while waiting for it", rd.id, appID)
		case <-time.After(t.installPoll):
		}
	}
}

// appIDOf reads an app file's id ("" when it cannot be read).
func (s *Server) appIDOf(path string) string {
	if s.deps.AppID == nil {
		return ""
	}
	id, err := s.deps.AppID(path)
	if err != nil {
		logger.Info("install %s: app id not read: %v", filepath.Base(path), err)
		return ""
	}
	return id
}

// ---------------------------------------------------------------------------
// Provider utilities: GET /v1/providers/{provider}/account, GET and POST
// /v1/providers/{provider}/apps. They need credentials, not a device.

// ProviderAccountResult answers GET /v1/providers/{provider}/account.
type ProviderAccountResult struct {
	Envelope
	Provider string         `json:"provider"`
	Hub      string         `json:"hub,omitempty"`
	Account  map[string]any `json:"account"`
}

// ProviderAppsResult answers GET /v1/providers/{provider}/apps.
type ProviderAppsResult struct {
	Envelope
	Provider string             `json:"provider"`
	Hub      string             `json:"hub,omitempty"`
	Apps     []cloudprov.AppRef `json:"apps"`
}

// ProviderUploadRequest is POST /v1/providers/{provider}/apps.
type ProviderUploadRequest struct {
	// App is a path on the daemon's host (local callers only).
	App string `json:"app"`
}

// ProviderUploadResult answers POST /v1/providers/{provider}/apps.
type ProviderUploadResult struct {
	Envelope
	Provider string `json:"provider"`
	Hub      string `json:"hub,omitempty"`
	cloudprov.AppRef
}

func (s *Server) handleProviderAccount(w http.ResponseWriter, r *http.Request) {
	p, e := s.provider(r.PathValue("provider"))
	if e != nil {
		WriteError(w, e)
		return
	}
	acct, err := p.Account(r.Context())
	if err != nil {
		WriteError(w, cloudError(err))
		return
	}
	WriteJSON(w, http.StatusOK, ProviderAccountResult{Envelope: Envelope{OK: true}, Provider: p.Name(), Hub: s.hubLabel(), Account: acct})
}

func (s *Server) handleProviderApps(w http.ResponseWriter, r *http.Request) {
	p, e := s.provider(r.PathValue("provider"))
	if e != nil {
		WriteError(w, e)
		return
	}
	apps, err := p.Apps(r.Context())
	if err != nil {
		WriteError(w, cloudError(err))
		return
	}
	if apps == nil {
		apps = []cloudprov.AppRef{}
	}
	WriteJSON(w, http.StatusOK, ProviderAppsResult{Envelope: Envelope{OK: true}, Provider: p.Name(), Hub: s.hubLabel(), Apps: apps})
}

// handleProviderUpload uploads an app from the daemon's own disk: a local
// caller's only. A remote caller's app reaches the provider through the
// install route on a leased cloud device.
func (s *Server) handleProviderUpload(w http.ResponseWriter, r *http.Request) {
	if CallerFrom(r.Context()).Remote {
		e := Forbidden("upload reads a file on the daemon's own machine; a remote caller installs through install on a leased cloud device (the app is uploaded under the lease)")
		s.LogForbidden(r, e)
		WriteError(w, e)
		return
	}
	var req ProviderUploadRequest
	if e := DecodeBody(r, &req); e != nil {
		WriteError(w, e)
		return
	}
	if req.App == "" {
		WriteError(w, Errorf(CodeUsage, "upload: app is required (a path on this machine)").WithDetails("field", "app"))
		return
	}
	p, e := s.provider(r.PathValue("provider"))
	if e != nil {
		WriteError(w, e)
		return
	}
	s.idle.Hold()
	defer s.idle.Release()
	ref, err := p.Upload(r.Context(), req.App)
	if err != nil {
		WriteError(w, cloudError(err))
		return
	}
	WriteJSON(w, http.StatusOK, ProviderUploadResult{Envelope: Envelope{OK: true}, Provider: p.Name(), Hub: s.hubLabel(), AppRef: *ref})
}
