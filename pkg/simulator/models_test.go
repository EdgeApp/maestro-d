package simulator

import (
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

const devicetypesJSON = `{"devicetypes":[
 {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-16","modelIdentifier":"iPhone17,3","name":"iPhone 16"},
 {"identifier":"com.apple.CoreSimulator.SimDeviceType.iPad-Pro-13-inch-M4-8GB","modelIdentifier":"iPad16,5","name":"iPad Pro 13-inch (M4)"},
 {"identifier":"com.apple.CoreSimulator.SimDeviceType.No-Model","name":"No model"}
]}`

func resetModels(t *testing.T) {
	t.Helper()
	clear := func() {
		waitModels()
		models.mu.Lock()
		models.byType, models.loadedAt, models.failedAt = nil, time.Time{}, time.Time{}
		models.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// waitModels waits for a read in progress to end.
func waitModels() {
	models.mu.Lock()
	loading := models.loading
	models.mu.Unlock()
	if loading != nil {
		<-loading
	}
}

func withModelsClock(t *testing.T, at *time.Time) {
	t.Helper()
	prev := modelsNow
	modelsNow = func() time.Time { return *at }
	t.Cleanup(func() { modelsNow = prev })
}

func withModelsBounds(t *testing.T, load, listing time.Duration) {
	t.Helper()
	prevLoad, prevWait := modelsLoadTimeout, modelsListingWait
	modelsLoadTimeout, modelsListingWait = load, listing
	t.Cleanup(func() { modelsLoadTimeout, modelsListingWait = prevLoad, prevWait })
}

func countingExec(t *testing.T, calls *atomic.Int32, fn func() *exec.Cmd) {
	t.Helper()
	withFakeExec(t, func(name string, args ...string) *exec.Cmd {
		calls.Add(1)
		return fn()
	})
}

func TestModelIdentifierMapsDeviceTypesOnce(t *testing.T) {
	resetModels(t)
	var calls atomic.Int32
	countingExec(t, &calls, func() *exec.Cmd { return exec.Command("printf", "%s", devicetypesJSON) })
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "iPhone17,3" {
		t.Errorf("iPhone 16 = %q", got)
	}
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPad-Pro-13-inch-M4-8GB"); got != "iPad16,5" {
		t.Errorf("iPad = %q", got)
	}
	if got := ModelIdentifier(""); got != "" {
		t.Errorf("empty type = %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("simctl ran %d times, want 1", n)
	}
}

// A slow read does not hold a listing past modelsListingWait; it runs on,
// and a later listing gets its answer.
func TestModelIdentifierDoesNotWaitOutASlowRead(t *testing.T) {
	resetModels(t)
	withModelsBounds(t, 5*time.Second, 50*time.Millisecond)
	withFakeExec(t, func(string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `sleep 0.4; printf '%s' "$0"`, devicetypesJSON)
	})
	start := time.Now()
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "" {
		t.Errorf("before the read ends = %q", got)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("the listing waited %v", d)
	}
	waitModels()
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "iPhone17,3" {
		t.Errorf("after the read = %q", got)
	}
}

// WarmModels starts the read and returns; a listing that comes later
// waits only for what is left of modelsListingWait.
func TestWarmModelsStartsTheReadEarly(t *testing.T) {
	resetModels(t)
	var calls atomic.Int32
	countingExec(t, &calls, func() *exec.Cmd { return exec.Command("printf", "%s", devicetypesJSON) })
	WarmModels()
	WarmModels()
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "iPhone17,3" || calls.Load() != 1 {
		t.Errorf("after warming: %q, %d reads", got, calls.Load())
	}
	WarmModels()
	if calls.Load() != 1 {
		t.Errorf("warming a read map read again: %d reads", calls.Load())
	}
}

func TestModelIdentifierLoadIsBounded(t *testing.T) {
	resetModels(t)
	withModelsBounds(t, 50*time.Millisecond, time.Second)
	withFakeExec(t, func(string, ...string) *exec.Cmd { return exec.Command("sh", "-c", "sleep 5; true") })
	start := time.Now()
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "" {
		t.Errorf("killed read = %q", got)
	}
	waitModels()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the read took %v; the bound did not kill it", d)
	}
}

func TestModelIdentifierRetriesAFailedLoadAfterTheWindow(t *testing.T) {
	resetModels(t)
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withModelsClock(t, &clock)
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	countingExec(t, &calls, func() *exec.Cmd {
		if fail.Load() {
			return exec.Command("false")
		}
		return exec.Command("printf", "%s", devicetypesJSON)
	})
	const iphone = "com.apple.CoreSimulator.SimDeviceType.iPhone-16"
	if got := ModelIdentifier(iphone); got != "" {
		t.Errorf("failed read = %q", got)
	}
	waitModels()
	fail.Store(false)
	clock = clock.Add(modelsFailFresh - time.Second)
	if got := ModelIdentifier(iphone); got != "" || calls.Load() != 1 {
		t.Errorf("inside the window: %q after %d reads", got, calls.Load())
	}
	clock = clock.Add(2 * time.Second)
	if got := ModelIdentifier(iphone); got != "iPhone17,3" || calls.Load() != 2 {
		t.Errorf("after the window: %q after %d reads", got, calls.Load())
	}
}

func TestModelIdentifierBadJSONAndStartFailure(t *testing.T) {
	resetModels(t)
	withFakeExec(t, fakeExec("nope"))
	if got := ModelIdentifier("x"); got != "" {
		t.Errorf("bad JSON = %q", got)
	}
	resetModels(t)
	withFakeExec(t, func(string, ...string) *exec.Cmd { return exec.Command("/nonexistent/xcrun") })
	if got := ModelIdentifier("x"); got != "" {
		t.Errorf("start failure = %q", got)
	}
}

// A device type added after the first read (an Xcode update under a
// running hub) is found by another read, at most once per window, without
// holding the listing that asked.
func TestModelIdentifierReadsAgainForAnUnknownType(t *testing.T) {
	resetModels(t)
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withModelsClock(t, &clock)
	var calls atomic.Int32
	var answer atomic.Value
	answer.Store(devicetypesJSON)
	countingExec(t, &calls, func() *exec.Cmd { return exec.Command("printf", "%s", answer.Load().(string)) })
	const newType = "com.apple.CoreSimulator.SimDeviceType.iPhone-18"
	if got := ModelIdentifier(newType); got != "" || calls.Load() != 1 {
		t.Fatalf("before the update: %q after %d reads", got, calls.Load())
	}
	answer.Store(`{"devicetypes":[{"identifier":"` + newType + `","modelIdentifier":"iPhone19,1"}]}`)
	clock = clock.Add(modelsFailFresh - time.Second)
	if got := ModelIdentifier(newType); got != "" || calls.Load() != 1 {
		t.Errorf("inside the window: %q after %d reads", got, calls.Load())
	}
	clock = clock.Add(2 * time.Second)
	ModelIdentifier(newType)
	waitModels()
	if got := ModelIdentifier(newType); got != "iPhone19,1" || calls.Load() != 2 {
		t.Errorf("after the window: %q after %d reads", got, calls.Load())
	}
	// A failed re-read keeps the map and waits out the window again.
	answer.Store("nope")
	clock = clock.Add(modelsFailFresh)
	ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.Nope")
	waitModels()
	if got := ModelIdentifier(newType); got != "iPhone19,1" || calls.Load() != 3 {
		t.Errorf("after a failed re-read: %q after %d reads", got, calls.Load())
	}
}

func TestModelsReadRecoversAPanic(t *testing.T) {
	resetModels(t)
	clock := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	withModelsClock(t, &clock)
	withFakeExec(t, func(string, ...string) *exec.Cmd { panic("boom") })
	WarmModels()
	waitModels()
	withFakeExec(t, fakeExec(devicetypesJSON))
	clock = clock.Add(modelsFailFresh)
	if got := ModelIdentifier("com.apple.CoreSimulator.SimDeviceType.iPhone-16"); got != "iPhone17,3" {
		t.Errorf("after a recovered panic: %q", got)
	}
}

func TestListSimulatorsReadsDeviceType(t *testing.T) {
	withFakeLookPath(t, func(string) (string, error) { return "/usr/bin/xcrun", nil })
	withFakeExec(t, fakeExec(`{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-26-5":[
	 {"name":"iPhone 16","udid":"U1","state":"Booted","isAvailable":true,"deviceTypeIdentifier":"com.apple.CoreSimulator.SimDeviceType.iPhone-16"}]}}`))
	sims, err := ListSimulators()
	if err != nil || len(sims) != 1 {
		t.Fatalf("%+v %v", sims, err)
	}
	if sims[0].DeviceType != "com.apple.CoreSimulator.SimDeviceType.iPhone-16" {
		t.Errorf("device type %q", sims[0].DeviceType)
	}
}
