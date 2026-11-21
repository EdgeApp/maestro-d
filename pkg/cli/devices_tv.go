package cli

import (
	"runtime"
	"sync"
	"time"

	wdadriver "github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
)

// tvListTimeout bounds the devicectl call behind a device listing. A hub
// answers a peer's listing within FanoutTimeout (3 s) after simctl and adb
// have run, so a slow CoreDevice must cost the listing its TVs, not the
// whole hub.
const tvListTimeout = 1500 * time.Millisecond

// tvListFresh is how long a devicectl answer, or a failure, stands before
// devicectl is asked again; tvListStale is how long the last answer stands
// in while devicectl fails.
const (
	tvListFresh = 5 * time.Second
	tvListStale = time.Minute
)

// listDevicectlTVs is the devicectl reading the TV cache uses; tests
// replace it.
var listDevicectlTVs = func() ([]wdadriver.DevicectlTV, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil
	}
	return wdadriver.ListDevicectlTVs(tvListTimeout)
}

// tvCache keeps the last devicectl answer. One caller at a time asks
// devicectl; the others take the last answer at once rather than queueing
// behind a slow CoreDevice, and only the very first reading is waited for.
var tvCache struct {
	mu       sync.Mutex
	at       time.Time // the last good answer's time
	failedAt time.Time // the last failure's time
	tvs      []wdadriver.DevicectlTV
	inflight chan struct{}
	now      func() time.Time
}

func tvNow() time.Time {
	if tvCache.now != nil {
		return tvCache.now()
	}
	return time.Now()
}

// lastTVsLocked is the last answer while it is not stale, else nothing.
func lastTVsLocked(t time.Time) []wdadriver.DevicectlTV {
	if !tvCache.at.IsZero() && t.Sub(tvCache.at) < tvListStale {
		return tvCache.tvs
	}
	return nil
}

// cachedTVs returns the TVs devicectl lists: from the cache while its
// answer, or its last failure, is fresh; otherwise from one devicectl call,
// which callers arriving meanwhile wait for only when no earlier answer
// still stands.
func cachedTVs() []wdadriver.DevicectlTV {
	tvCache.mu.Lock()
	t := tvNow()
	fresh := func(at time.Time) bool { return !at.IsZero() && t.Sub(at) < tvListFresh }
	if fresh(tvCache.at) || fresh(tvCache.failedAt) {
		defer tvCache.mu.Unlock()
		return lastTVsLocked(t)
	}
	if ch := tvCache.inflight; ch != nil {
		if last := lastTVsLocked(t); last != nil || (!tvCache.at.IsZero() && t.Sub(tvCache.at) < tvListStale) {
			defer tvCache.mu.Unlock()
			return last
		}
		// No answer that still stands: wait for the reading in flight,
		// which tvListTimeout bounds.
		tvCache.mu.Unlock()
		<-ch
		tvCache.mu.Lock()
		defer tvCache.mu.Unlock()
		return lastTVsLocked(tvNow())
	}
	ch := make(chan struct{})
	tvCache.inflight = ch
	tvCache.mu.Unlock()

	tvs, err := listDevicectlTVs()

	tvCache.mu.Lock()
	defer tvCache.mu.Unlock()
	tvCache.inflight = nil
	close(ch)
	t = tvNow()
	if err != nil {
		tvCache.failedAt = t
		return lastTVsLocked(t)
	}
	tvCache.at, tvCache.failedAt, tvCache.tvs = t, time.Time{}, tvs
	wdadriver.RememberTVs(tvs) // also when listDevicectlTVs is not the real reading
	return tvs
}

// physicalTVInfo reports whether id is an Apple TV a devicectl listing has
// returned in this process, and its name. A hub asks it on every remote
// request, so it answers from memory and never runs devicectl; listings, the
// hub's start and every TV attach fill that memory (wdadriver.KnownTV).
func physicalTVInfo(id string) (string, bool) { return wdadriver.KnownTV(id) }

// physicalTVEntries lists the Apple TVs this host has paired, skipping ids
// in seen. They pair over the network, so usbmuxd (physicalIOSEntries)
// never sees them. They are ios devices of kind "tv": the Apple driver runs
// them, and a selector asking for an iOS "device" never picks one. A TV
// that is not paired, not in developer mode, or out of reach is not ready,
// and is listed only with includeUnready, as a shut-down simulator is.
func physicalTVEntries(seen []DeviceEntry, includeUnready bool) []DeviceEntry {
	tvs := cachedTVs()
	known := make(map[string]bool, len(seen))
	for _, e := range seen {
		known[e.ID] = true
	}
	entries := make([]DeviceEntry, 0, len(tvs))
	for _, tv := range tvs {
		if known[tv.UDID] || (!tv.Ready() && !includeUnready) {
			continue
		}
		state := "paired"
		switch {
		case !tv.Paired:
			state = "not ready (pair it in Xcode and enable developer mode)"
		case !tv.Reachable:
			state = "unavailable (off or out of reach)"
		case tv.Tunnel:
			state = "connected"
		}
		entries = append(entries, DeviceEntry{
			Platform:  "ios",
			Kind:      "tv",
			ID:        tv.UDID,
			Name:      tv.Name,
			Model:     tv.Model,
			OSVersion: tv.OSVersion,
			State:     state,
			Ready:     tv.Ready(),
		})
	}
	return entries
}
