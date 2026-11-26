package hubweb

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestWebPush(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan daemon.Event)
	ticks := make(chan time.Time)
	var holder atomic.Pointer[daemon.DeviceLease]
	var name atomic.Value
	name.Store("Bench")
	projected := make(chan struct{}, 16)
	f := newFeed(source{
		scan: func(context.Context) inventory { return inventory{} },
		project: func(inventory) Snapshot {
			d := daemon.DeviceInfo{Hub: "self", ID: "a@self", Name: name.Load().(string), Lease: holder.Load()}
			s := project("self", "machine", []daemon.DeviceInfo{d}, inventory{})
			projected <- struct{}{}
			return s
		},
	})
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	go f.run(ctx, events, ticks, now)
	client := make(chan []byte, 1)
	f.add <- client
	read := func() Snapshot {
		t.Helper()
		select {
		case data := <-client:
			var s Snapshot
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatal(err)
			}
			if s.GeneratedAt != now().Format(time.RFC3339Nano) {
				t.Fatal(s.GeneratedAt, now())
			}
			return s
		case <-time.After(250 * time.Millisecond):
			t.Fatal("snapshot missed 250 ms event deadline")
			return Snapshot{}
		}
	}
	if s := read(); s.Devices[0].Lease != nil {
		t.Fatal(s)
	}
	<-projected
	elapsed.Add(int64(time.Second))
	holder.Store(&daemon.DeviceLease{Holder: "named", ExpiresAt: "future"})
	events <- daemon.Event{Type: "lease.granted"}
	if s := read(); s.Devices[0].Lease.Holder != "named" {
		t.Fatal(s)
	}
	<-projected
	for i := 0; i < 2; i++ {
		elapsed.Add(int64(2 * time.Second))
		ticks <- now()
		<-projected
		// A no-op event handoff waits until the previous projection and
		// broadcast have both completed, without advancing the clock.
		events <- daemon.Event{Type: "step"}
		select {
		case body := <-client:
			t.Fatalf("unchanged poll sent %s", body)
		default:
		}
	}
	elapsed.Add(int64(time.Second))
	name.Store("New name")
	events <- daemon.Event{Type: "device.shared"}
	if s := read(); s.Devices[0].Name != "New name" {
		t.Fatal(s)
	}
	<-projected
	<-projected // refreshed discovery has the same public result
	events <- daemon.Event{Type: "step"}
	select {
	case body := <-client:
		t.Fatalf("duplicate local update: %s", body)
	default:
	}
	f.remove <- client
	cancel()
	<-f.done
}
