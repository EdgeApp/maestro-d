package daemon

import (
	"context"
	"sync"
	"testing"
	"time"
)

// idleClock replaces the sweep's clock and interval for one test.
type idleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *idleClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *idleClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func fakeIdleClock(t *testing.T) *idleClock {
	c := &idleClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	oldNow, oldSweep := idleNow, idleSweep
	idleNow, idleSweep = c.get, 5*time.Millisecond
	t.Cleanup(func() { idleNow, idleSweep = oldNow, oldSweep })
	return c
}

func attachedNow(t *testing.T, env *shotEnv, id string) bool {
	t.Helper()
	env.server.mu.Lock()
	defer env.server.mu.Unlock()
	return env.server.attached[id] != nil
}

func TestIdleDetach(t *testing.T) {
	clock := fakeIdleClock(t)
	env := newShotEnv(t, time.Minute)
	ctx := context.Background()
	events := make(chan Event, 16)
	evCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = env.c.Events(evCtx, "pix", 0, func(e Event) bool { events <- e; return true })
	}()

	// At the timeout exactly: kept.
	clock.advance(time.Minute)
	time.Sleep(30 * time.Millisecond)
	if !attachedNow(t, env, "pix") {
		t.Fatal("detached at exactly the timeout")
	}
	// A call resets the clock; vars does not hold the device and does not.
	if _, err := env.c.Screenshot(ctx, "pix"); err != nil {
		t.Fatal(err)
	}
	clock.advance(50 * time.Second)
	if _, err := env.c.Vars(ctx, "pix"); err != nil {
		t.Fatal(err)
	}
	clock.advance(20 * time.Second)
	waitFor(t, func() bool { return !attachedNow(t, env, "pix") })
	for {
		select {
		case e := <-events:
			if e.Type == "device.detached" {
				if e.Message != "idle" {
					t.Fatalf("detach event message %q", e.Message)
				}
				goto detached
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no device.detached event")
		}
	}
detached:
	// Devices without an IdleTimer are never idle-detached.
	clock.advance(time.Hour)
	time.Sleep(30 * time.Millisecond)
	if !attachedNow(t, env, "mock-1") || !attachedNow(t, env, "png") {
		t.Fatal("a device without an idle timeout was detached")
	}
	if _, err := env.c.Screenshot(ctx, "pix"); codeOf(t, err) != CodeDeviceNotAttached {
		t.Fatalf("after the idle detach: %v", err)
	}
}

func TestIdleDetach_NotDuringAStep(t *testing.T) {
	clock := fakeIdleClock(t)
	env := newShotEnv(t, time.Minute)
	a := env.server.attached["pix"]
	a.busy.Lock() // a step in flight
	clock.advance(2 * time.Minute)
	time.Sleep(30 * time.Millisecond)
	if !attachedNow(t, env, "pix") {
		t.Fatal("detached during a step")
	}
	a.busy.Unlock()
	waitFor(t, func() bool { return !attachedNow(t, env, "pix") })
}

func TestIdleDetach_ZeroDisables(t *testing.T) {
	clock := fakeIdleClock(t)
	env := newShotEnv(t, 0)
	clock.advance(24 * time.Hour)
	time.Sleep(30 * time.Millisecond)
	if !attachedNow(t, env, "pix") {
		t.Fatal("idle timeout 0 detached")
	}
}

func TestIdleDetach_SkipsAReplacedAttachment(t *testing.T) {
	env := newShotEnv(t, time.Minute)
	a := env.server.attached["pix"]
	env.server.mu.Lock()
	delete(env.server.attached, "pix")
	env.server.mu.Unlock()
	if !env.server.idleDetach(a, time.Minute) { // no longer current: nothing happens
		t.Fatal("idleDetach of a replaced attachment asked to look again")
	}
	env.server.mu.Lock()
	env.server.attached["pix"] = a
	env.server.mu.Unlock()
	if !attachedNow(t, env, "pix") {
		t.Fatal("detached an attachment that was not current")
	}
}

func TestIdleDetach_HoldsTheDeviceUntilClosed(t *testing.T) {
	env := newShotEnv(t, time.Minute)
	a := env.server.attached["pix"]
	inner := a.cleanup
	var heldDuringClose bool
	a.cleanup = func() {
		// A call that arrived during the detach is still waiting for the
		// device, and finds the session closed once it gets it.
		heldDuringClose = !a.busy.TryLock()
		if !heldDuringClose {
			a.busy.Unlock()
		}
		inner()
	}
	if !env.server.idleDetach(a, time.Minute) {
		t.Fatal("idleDetach asked to look again")
	}
	if !heldDuringClose {
		t.Error("the device was free while its connection closed")
	}
	if attachedNow(t, env, "pix") {
		t.Error("still attached")
	}
	if !a.busy.TryLock() {
		t.Error("busy left locked")
	}
}

func TestIdleDetach_LeavesANewAttachment(t *testing.T) {
	env := newShotEnv(t, time.Minute)
	a := env.server.attached["pix"]
	other := &attached{id: "pix"}
	env.server.mu.Lock()
	env.server.attached["pix"] = other
	env.server.mu.Unlock()
	if !env.server.idleDetach(a, time.Minute) {
		t.Fatal("idleDetach of a replaced attachment asked to look again")
	}
	env.server.mu.Lock()
	got := env.server.attached["pix"]
	env.server.attached["pix"] = a
	env.server.mu.Unlock()
	if got != other {
		t.Error("the idle sweep of an old attachment took the new one")
	}
}

// A driver whose connection ends is detached, with event message
// "connection closed"; the next call answers DEVICE_NOT_ATTACHED.
func TestConnectionClosedDetaches(t *testing.T) {
	env := newShotEnv(t, 0)
	ctx := context.Background()
	events := make(chan Event, 16)
	evCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = env.c.Events(evCtx, "pix", 0, func(e Event) bool { events <- e; return true })
	}()
	time.Sleep(30 * time.Millisecond)
	a := env.server.attached["pix"]
	a.busy.Lock() // a step in flight answers first
	close(env.frame.done)
	time.Sleep(30 * time.Millisecond)
	if !attachedNow(t, env, "pix") {
		t.Fatal("detached under a step in flight")
	}
	a.busy.Unlock()
	waitFor(t, func() bool { return !attachedNow(t, env, "pix") })
	for {
		select {
		case e := <-events:
			if e.Type == "device.detached" {
				if e.Message != "connection closed" {
					t.Fatalf("detach event message %q", e.Message)
				}
				if _, err := env.c.Screenshot(ctx, "pix"); codeOf(t, err) != CodeDeviceNotAttached {
					t.Fatalf("after the connection closed: %v", err)
				}
				if !attachedNow(t, env, "png") {
					t.Error("another device was detached")
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no device.detached event")
		}
	}
}

// A connection that ends before the attach has stored the attachment is
// still noticed: the device is detached once attached.
func TestConnectionClosedDuringTheAttach(t *testing.T) {
	env := newShotEnv(t, 0)
	ctx := context.Background()
	if err := env.c.Detach(ctx, "pix"); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	close(closed)
	env.frame.mu.Lock()
	env.frame.done = closed
	env.frame.mu.Unlock()
	if _, err := env.c.Attach(ctx, "pix", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !attachedNow(t, env, "pix") })
}
