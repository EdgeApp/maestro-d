package cli

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	wdadriver "github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
)

func stubTVs(t *testing.T, tvs []wdadriver.DevicectlTV, err error) *int {
	t.Helper()
	calls := new(int)
	prev := listDevicectlTVs
	listDevicectlTVs = func() ([]wdadriver.DevicectlTV, error) { *calls++; return tvs, err }
	resetTVCache(t)
	t.Cleanup(func() { listDevicectlTVs = prev; resetTVCache(t) })
	return calls
}

func resetTVCache(t *testing.T) {
	t.Helper()
	tvCache.mu.Lock()
	tvCache.at, tvCache.failedAt, tvCache.tvs, tvCache.inflight, tvCache.now = time.Time{}, time.Time{}, nil, nil, nil

	tvCache.mu.Unlock()
}

func TestPhysicalTVEntries(t *testing.T) {
	stubTVs(t, []wdadriver.DevicectlTV{
		{UDID: "tv-1", Name: "Lab TV", OSVersion: "26.3", Paired: true, Reachable: true, Tunnel: true},
		{UDID: "tv-2", Name: "Hall TV", OSVersion: "27.2.1", Paired: true, Reachable: true},
		{UDID: "tv-4", Name: "Away", OSVersion: "26.6.1", Paired: true},
		{UDID: "tv-3", Name: "Den", OSVersion: "26.1", Reachable: true},
		{UDID: "phone-1", Name: "Seen already", Paired: true, Reachable: true},
	}, nil)
	got := physicalTVEntries([]DeviceEntry{{ID: "phone-1"}}, true)
	want := []DeviceEntry{
		{Platform: "ios", Kind: "tv", ID: "tv-1", Name: "Lab TV", OSVersion: "26.3", State: "connected", Ready: true},
		{Platform: "ios", Kind: "tv", ID: "tv-2", Name: "Hall TV", OSVersion: "27.2.1", State: "paired", Ready: true},
		{Platform: "ios", Kind: "tv", ID: "tv-4", Name: "Away", OSVersion: "26.6.1", State: "unavailable (off or out of reach)"},
		{Platform: "ios", Kind: "tv", ID: "tv-3", Name: "Den", OSVersion: "26.1", State: "not ready (pair it in Xcode and enable developer mode)"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %+v; want %+v", i, got[i], want[i])
		}
	}
	if s := describeState(got[0]); s != "connected (tvOS 26.3)" {
		t.Fatalf("describeState: %s", s)
	}
	// Without --all, only the TVs a session can start on now.
	ready := physicalTVEntries(nil, false)
	if len(ready) != 3 || ready[0].ID != "tv-1" || ready[1].ID != "tv-2" || ready[2].ID != "phone-1" {
		t.Fatalf("ready only: %+v", ready)
	}
	if name, ok := physicalTVInfo("tv-4"); !ok || name != "Away" {
		t.Fatalf("physicalTVInfo: %q %v", name, ok)
	}
	if _, ok := physicalTVInfo("nope"); ok {
		t.Fatal("physicalTVInfo knows an unknown id")
	}
}

func TestPhysicalTVEntries_DevicectlFails(t *testing.T) {
	stubTVs(t, nil, errors.New("no devicectl"))
	if got := physicalTVEntries(nil, true); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestCachedTVs: a fresh answer is reused; a failure is not retried while
// fresh and falls back to the last answer while it is not stale, and to
// nothing after.
func TestCachedTVs(t *testing.T) {
	tvs := []wdadriver.DevicectlTV{{UDID: "tv-1", Paired: true, Reachable: true}}
	var fail error
	calls := 0
	prev := listDevicectlTVs
	listDevicectlTVs = func() ([]wdadriver.DevicectlTV, error) { calls++; return tvs, fail }
	resetTVCache(t)
	t.Cleanup(func() { listDevicectlTVs = prev; resetTVCache(t) })
	clock := time.Unix(1000, 0)
	tvCache.now = func() time.Time { return clock }

	if got := cachedTVs(); len(got) != 1 || calls != 1 {
		t.Fatalf("first: %v, %d calls", got, calls)
	}
	clock = clock.Add(tvListFresh - time.Second)
	if got := cachedTVs(); len(got) != 1 || calls != 1 {
		t.Fatalf("fresh: %v, %d calls", got, calls)
	}
	fail = errors.New("devicectl timed out")
	clock = clock.Add(2 * time.Second)
	if got := cachedTVs(); len(got) != 1 || calls != 2 {
		t.Fatalf("stale fallback: %v, %d calls", got, calls)
	}
	clock = clock.Add(time.Second)
	if got := cachedTVs(); len(got) != 1 || calls != 2 {
		t.Fatalf("a fresh failure was retried: %v, %d calls", got, calls)
	}
	clock = clock.Add(tvListStale)
	if got := cachedTVs(); len(got) != 0 || calls != 3 {
		t.Fatalf("expired: %v, %d calls", got, calls)
	}
	// A TV once seen is still known while devicectl fails.
	if name, ok := physicalTVInfo("tv-1"); !ok || name != "" {
		t.Fatalf("forgot tv-1: %q %v", name, ok)
	}
}

// TestCachedTVs_SingleFlight: while one caller waits on a slow devicectl,
// the others take the last answer at once instead of queueing.
func TestCachedTVs_SingleFlight(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	prev := listDevicectlTVs
	listDevicectlTVs = func() ([]wdadriver.DevicectlTV, error) {
		if calls.Add(1) > 1 {
			<-release
		}
		return []wdadriver.DevicectlTV{{UDID: "tv-1"}}, nil
	}
	resetTVCache(t)
	t.Cleanup(func() { listDevicectlTVs = prev; resetTVCache(t) })
	clock := time.Unix(1000, 0)
	var clockMu sync.Mutex
	tvCache.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	if len(cachedTVs()) != 1 {
		t.Fatal("first answer")
	}
	clockMu.Lock()
	clock = clock.Add(tvListFresh)
	clockMu.Unlock()

	slow := make(chan struct{})
	go func() { cachedTVs(); close(slow) }()
	for calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	for i := 0; i < 5; i++ {
		if got := cachedTVs(); len(got) != 1 {
			t.Fatalf("caller %d got %v", i, got)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond || calls.Load() != 2 {
		t.Fatalf("callers waited %v, %d devicectl calls", d, calls.Load())
	}
	close(release)
	<-slow
}

// TestCachedTVs_StaleWaitsForReading: with no answer that still stands,
// a caller arriving while a reading is in flight waits for it instead of
// seeing no TVs.
func TestCachedTVs_StaleWaitsForReading(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	prev := listDevicectlTVs
	listDevicectlTVs = func() ([]wdadriver.DevicectlTV, error) {
		if calls.Add(1) > 1 {
			<-release
		}
		return []wdadriver.DevicectlTV{{UDID: "tv-stale"}}, nil
	}
	resetTVCache(t)
	t.Cleanup(func() { listDevicectlTVs = prev; resetTVCache(t) })
	clock := time.Unix(1000, 0)
	var clockMu sync.Mutex
	tvCache.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	cachedTVs()
	clockMu.Lock()
	clock = clock.Add(tvListStale + time.Second)
	clockMu.Unlock()

	first := make(chan []wdadriver.DevicectlTV)
	go func() { first <- cachedTVs() }()
	for calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan []wdadriver.DevicectlTV)
	go func() { second <- cachedTVs() }()
	select {
	case got := <-second:
		t.Fatalf("returned %v before the reading finished", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if got := <-second; len(got) != 1 {
		t.Fatalf("second caller got %v", got)
	}
	if got := <-first; len(got) != 1 {
		t.Fatalf("first caller got %v", got)
	}
}

type tvDriverStub struct {
	core.Driver
	tv bool
}

func (d tvDriverStub) IsTV() bool { return d.tv }

func TestIsTVDriver(t *testing.T) {
	if !isTVDriver(tvDriverStub{tv: true}) || isTVDriver(tvDriverStub{}) || isTVDriver(nil) {
		t.Fatal("isTVDriver")
	}
}
