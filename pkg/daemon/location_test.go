package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestMachineName(t *testing.T) {
	t.Setenv(envTestMachine, "")
	if MachineName() == "" {
		t.Fatal("MachineName is empty without the override")
	}
	t.Setenv(envTestMachine, "machine-a")
	if got := MachineName(); got != "machine-a" {
		t.Fatalf("MachineName = %q, want the override", got)
	}
}

func TestSameMachine(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"alex-mac", "alex-mac", true},
		{"Alex-Mac", "alex-mac.local", true},
		{"alex-mac.local.", "ALEX-MAC", true},
		{"alex-mac", "sam-mini", false},
		{"", "", false},
		{"alex-mac", "", false},
	} {
		if got := SameMachine(c.a, c.b); got != c.want {
			t.Errorf("SameMachine(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCallerMachine(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/devices", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	r.Header.Set(HeaderClientMachine, " laptop ")
	if got := CallerMachine(r, false, "hub-host"); got != "laptop" {
		t.Fatalf("header: got %q", got)
	}
	r.Header.Del(HeaderClientMachine)
	if got := CallerMachine(r, false, "hub-host"); got != "" {
		t.Fatalf("remote TCP without header: got %q, want unknown", got)
	}
	if got := CallerMachine(r, true, "hub-host"); got != "hub-host" {
		t.Fatalf("unix socket: got %q", got)
	}
	r.RemoteAddr = "127.0.0.1:4000"
	if got := CallerMachine(r, false, "hub-host"); got != "hub-host" {
		t.Fatalf("loopback: got %q", got)
	}
	r.RemoteAddr = "[::1]:4000"
	if got := CallerMachine(r, false, "hub-host"); got != "hub-host" {
		t.Fatalf("IPv6 loopback: got %q", got)
	}
	r.RemoteAddr = "alice via hub b"
	if got := CallerMachine(r, false, "hub-host"); got != "" {
		t.Fatalf("forwarded: got %q, want unknown", got)
	}
}

func TestSetLocations(t *testing.T) {
	devs := []DeviceInfo{
		{ID: "a", Machine: "Alex-Mac"},
		{ID: "b", Machine: "sam-mini"},
		{ID: "df-1", Machine: "alex-mac", Provider: "devicefarm"},
		{ID: "c"},
	}
	SetLocations(devs, "alex-mac.local")
	want := []string{LocationLocal, LocationMesh, LocationCloud, LocationMesh}
	for i, d := range devs {
		if d.Location != want[i] {
			t.Errorf("%s: location %q, want %q", d.ID, d.Location, want[i])
		}
	}
	// An unknown caller sees every non-cloud device as mesh.
	SetLocations(devs, "")
	for i, w := range []string{LocationMesh, LocationMesh, LocationCloud, LocationMesh} {
		if devs[i].Location != w {
			t.Errorf("unknown caller: %s is %q, want %q", devs[i].ID, devs[i].Location, w)
		}
	}
}

func TestServerCallerMachine(t *testing.T) {
	t.Setenv(envTestMachine, "this-host")
	s := &Server{}
	r := httptest.NewRequest("GET", "/v1/devices", nil)
	// No caller recorded: the unix socket, so this machine.
	if got := s.callerMachine(r); got != "this-host" {
		t.Fatalf("unix: got %q", got)
	}
	r = r.WithContext(withCaller(context.Background(), Caller{Remote: true, Addr: "10.1.1.1:5"}))
	r.RemoteAddr = "10.1.1.1:5"
	if got := s.callerMachine(r); got != "" {
		t.Fatalf("remote TCP: got %q", got)
	}
}
